package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Policy Job — application registry and administrative core of SETI.
//
// Owns the primary SETI database (in-memory Phase 2, PostgreSQL Phase 4).
// Responsibilities:
//   - Application registry: track registered constellations
//   - Service account issuance: JWT tokens for SETI internal Jobs
//   - Test scheduling: when/how often Contract Tests run per application
//   - On application registration: notify Signal Aggregator to subscribe
//     to that constellation's tca:events and tca:augur-canis channels
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4002")
	redisURL         = envOr("REDIS_URL", "redis:6379")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	jwtSecret        = []byte(mustEnv("JWT_SECRET"))
	signalAggURL     = envOr("SIGNAL_AGGREGATOR_URL", "https://signal-aggregator:4006")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("[policy] Required env var %s not set", key)
	}
	return v
}

// ---------------------------------------------------------------------------
// In-memory store — Phase 2
// Swap point: replace with PostgreSQL client in Phase 4
// ---------------------------------------------------------------------------

type Application struct {
	ApplicationID   string          `json:"application_id"`
	DisplayName     string          `json:"display_name"`
	Description     string          `json:"description"`
	Environment     string          `json:"environment"`
	GatewayURL      string          `json:"gateway_url"`
	RedisURL        string          `json:"redis_url"`
	ContractsPath   string          `json:"contracts_path"`
	ServiceAccount  string          `json:"service_account"`
	TestSchedule    *TestSchedule   `json:"test_schedule"`
	Status          string          `json:"status"` // active | suspended | deregistered
	RegisteredAt    string          `json:"registered_at"`
	LastTestedAt    string          `json:"last_tested_at,omitempty"`
}

type TestSchedule struct {
	ContractTestIntervalMinutes int  `json:"contract_test_interval_minutes"`
	PlotTestEnabled             bool `json:"plot_test_enabled"`
	Enabled                     bool `json:"enabled"`
}

type ServiceAccount struct {
	AccountID     string `json:"account_id"`
	ApplicationID string `json:"application_id"`
	Role          string `json:"role"`
	Description   string `json:"description"`
	CreatedAt     string `json:"created_at"`
}

var (
	mu           sync.RWMutex
	applications = map[string]*Application{}
	accounts     = map[string]*ServiceAccount{}
)

// ---------------------------------------------------------------------------
// Service account JWT issuance
// These tokens are how SETI internal Jobs authenticate to constellation services.
// ---------------------------------------------------------------------------

type SETIServiceClaims struct {
	AccountID     string `json:"account_id"`
	ApplicationID string `json:"application_id"`
	Role          string `json:"role"`
	ClearanceLevel string `json:"clearance_level"`
	WranglerID    string `json:"wrangler_id"`
	jwt.RegisteredClaims
}

func issueServiceAccountJWT(account *ServiceAccount) (string, error) {
	now := time.Now()
	claims := SETIServiceClaims{
		AccountID:      account.AccountID,
		ApplicationID:  account.ApplicationID,
		Role:           account.Role,
		ClearanceLevel: "sec-wrangler", // Service accounts get sec-wrangler clearance
		WranglerID:     account.AccountID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

// ---------------------------------------------------------------------------
// Redis client
// ---------------------------------------------------------------------------

var rdb *redis.Client

func connectRedis() {
	for i := 0; i < 10; i++ {
		rdb = redis.NewClient(&redis.Options{Addr: redisURL})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := rdb.Ping(ctx).Result()
		cancel()
		if err == nil {
			log.Printf("[policy] Connected to Redis at %s", redisURL)
			return
		}
		log.Printf("[policy] Redis not ready (attempt %d/10): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	log.Fatal("[policy] Could not connect to Redis after 10 attempts")
}

// ---------------------------------------------------------------------------
// Self-registration — SETI registers itself on startup
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Register constellation Jobs with Augur Canis
// AC needs to know each Job's network endpoint to route contract test requests.
// Called after selfRegister() during startup.
// ---------------------------------------------------------------------------

var acURL = envOr("AUGUR_CANIS_URL", "https://augur-canis:4010")

type acJobRegistration struct {
	ServiceName     string `json:"service_name"`
	NetworkEndpoint string `json:"network_endpoint"`
	Description     string `json:"description"`
}

func registerJobsWithAC() {
	// Phase 2 known Jobs and their internal network endpoints
	jobs := []acJobRegistration{
		{"gateway",          "https://gateway:4000",          "SETI external gateway"},
		{"seti-observability","https://seti-observability:4011","SETI observability event collector"},
		{"signal-clearance", "https://signal-clearance:4001", "SETI identity and clearance"},
		{"ui",               "http://ui:3000",                "SETI monitoring dashboard"},
		{"augur-canis",      "https://augur-canis:4010",      "SETI Augur Canis health agent"},
		{"policy",           "https://policy:4002",           "SETI policy and application registry"},
		{"contract-test",    "https://contract-test:4003",    "SETI contract test executor"},
		{"results",          "https://results:4008",          "SETI test results store"},
	}

	// Give AC a moment to be ready — it starts concurrently
	time.Sleep(3 * time.Second)

	registered := 0
	for _, job := range jobs {
		body, _ := json.Marshal(job)
		req, err := http.NewRequest(http.MethodPost, acURL+"/jobs",
			strings.NewReader(string(body)))
		if err != nil {
			log.Printf("[policy] Failed to build AC registration for %s: %v", job.ServiceName, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, err := upstreamClient.Do(req)
		latencyMs := time.Since(start).Milliseconds()

		if err != nil {
			log.Printf("[policy] Failed to register %s with AC: %v", job.ServiceName, err)
			reportEvent("augur-canis", "POST", "/jobs", 0, latencyMs)
			continue
		}
		resp.Body.Close()
		reportEvent("augur-canis", "POST", "/jobs", resp.StatusCode, latencyMs)

		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK ||
			resp.StatusCode == http.StatusConflict {
			registered++
			log.Printf("[policy] Registered %s with AC (status %d)", job.ServiceName, resp.StatusCode)
		} else {
			log.Printf("[policy] AC registration for %s returned %d", job.ServiceName, resp.StatusCode)
		}
	}

	log.Printf("[policy] AC job registration complete: %d/%d jobs registered", registered, len(jobs))
}

func selfRegister() {
	mu.Lock()
	defer mu.Unlock()

	if _, exists := applications["seti-self"]; exists {
		return
	}

	contractsPath := envOr("CONTRACTS_PATH", "/contracts/openapi")
	gatewayURL := envOr("SELF_GATEWAY_URL", "https://gateway:4000")
	selfRedisURL := envOr("REDIS_URL", "redis:6379")

	app := &Application{
		ApplicationID:  "seti-self",
		DisplayName:    "SETI (Self)",
		Description:    "SETI monitoring its own constellation. True mastery starts with oneself.",
		Environment:    "development",
		GatewayURL:     gatewayURL,
		RedisURL:       selfRedisURL,
		ContractsPath:  contractsPath,
		ServiceAccount: "svc-seti-self",
		TestSchedule: &TestSchedule{
			ContractTestIntervalMinutes: 15,
			PlotTestEnabled:             false,
			Enabled:                     true,
		},
		Status:       "active",
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}
	applications["seti-self"] = app

	// Create service account for SETI's own constellation
	account := &ServiceAccount{
		AccountID:     "svc-seti-self",
		ApplicationID: "seti-self",
		Role:          "contract-test",
		Description:   "SETI self-monitoring service account",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	accounts["svc-seti-self"] = account

	log.Printf("[policy] Self-registered SETI constellation (seti-self)")
	log.Printf("[policy] Contracts path: %s", contractsPath)
	log.Printf("[policy] Service account: svc-seti-self")
}

// ---------------------------------------------------------------------------
// AI Provider registry — runtime configuration, no env vars
// ---------------------------------------------------------------------------

type AIProvider struct {
	ProviderID      string   `json:"provider_id"`
	Name            string   `json:"name"`
	OllamaURL       string   `json:"ollama_url"`
	Description     string   `json:"description,omitempty"`
	Status          string   `json:"status"` // reachable | unreachable | unknown
	AvailableModels []string `json:"available_models"`
	ActiveModel     string   `json:"active_model,omitempty"`
	LastRefreshedAt string   `json:"last_refreshed_at,omitempty"`
	RegisteredAt    string   `json:"registered_at"`
}

var (
	providerMu sync.RWMutex
	providers  = map[string]*AIProvider{}
)

// ---------------------------------------------------------------------------
// mTLS upstream client
// ---------------------------------------------------------------------------

var upstreamClient *http.Client

func buildUpstreamClient() {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Printf("[policy] CA cert not found — upstream calls will fail: %v", err)
		upstreamClient = http.DefaultClient
		return
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	cert, err := tls.LoadX509KeyPair("/certs/policy.crt", "/certs/policy.key")
	if err != nil {
		log.Printf("[policy] Service cert not found: %v", err)
		upstreamClient = http.DefaultClient
		return
	}

	upstreamClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      caPool,
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS13,
			},
		},
		Timeout: 30 * time.Second,
	}
	log.Printf("[policy] mTLS upstream client ready")
}

// ---------------------------------------------------------------------------
// Observability reporting
// ---------------------------------------------------------------------------

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller":      "policy",
			"callee":      callee,
			"method":      method,
			"path":        path,
			"status_code": status,
			"latency_ms":  latencyMs,
			"protocol":    "mtls",
		})
		req, _ := http.NewRequest(http.MethodPost, observabilityURL+"/event",
			strings.NewReader(string(body)))
		if req == nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := upstreamClient.Do(req)
		if err != nil {
			log.Printf("[policy] observability error: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

// ---------------------------------------------------------------------------
// HTTP handlers — Application registry
// ---------------------------------------------------------------------------

func handleApplications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mu.RLock()
	apps := make([]*Application, 0, len(applications))
	for _, a := range applications {
		apps = append(apps, a)
	}
	mu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"applications": apps,
		"total":        len(apps),
	})
}

func handleRegisterApplication(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	var req struct {
		DisplayName  string        `json:"display_name"`
		Description  string        `json:"description"`
		Environment  string        `json:"environment"`
		GatewayURL   string        `json:"gateway_url"`
		RedisURL     string        `json:"redis_url"`
		ContractsPath string       `json:"contracts_path"`
		TestSchedule *TestSchedule `json:"test_schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": err.Error()})
		return
	}

	// Generate application ID from display name
	appID := strings.ToLower(strings.ReplaceAll(req.DisplayName, " ", "-"))
	accountID := fmt.Sprintf("svc-%s", appID)

	mu.Lock()
	if _, exists := applications[appID]; exists {
		mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "APPLICATION_EXISTS",
			"message": fmt.Sprintf("Application %s already registered", appID),
		})
		return
	}

	schedule := req.TestSchedule
	if schedule == nil {
		schedule = &TestSchedule{
			ContractTestIntervalMinutes: 15,
			PlotTestEnabled:             false,
			Enabled:                     true,
		}
	}

	app := &Application{
		ApplicationID:  appID,
		DisplayName:    req.DisplayName,
		Description:    req.Description,
		Environment:    req.Environment,
		GatewayURL:     req.GatewayURL,
		RedisURL:       req.RedisURL,
		ContractsPath:  req.ContractsPath,
		ServiceAccount: accountID,
		TestSchedule:   schedule,
		Status:         "active",
		RegisteredAt:   time.Now().UTC().Format(time.RFC3339),
	}
	applications[appID] = app

	account := &ServiceAccount{
		AccountID:     accountID,
		ApplicationID: appID,
		Role:          "contract-test",
		Description:   fmt.Sprintf("Service account for %s monitoring", req.DisplayName),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	accounts[accountID] = account
	mu.Unlock()

	log.Printf("[policy] Application registered: %s (%s)", appID, req.DisplayName)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(app)
}

func handleGetApplication(w http.ResponseWriter, r *http.Request) {
	// Path: /applications/{id}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/applications/"), "/")
	appID := parts[0]

	mu.RLock()
	app, exists := applications[appID]
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "APPLICATION_NOT_FOUND",
			"message": fmt.Sprintf("Application %s not found", appID),
		})
		return
	}
	json.NewEncoder(w).Encode(app)
}

// ---------------------------------------------------------------------------
// HTTP handlers — Service accounts
// ---------------------------------------------------------------------------

func handleIssueToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}

	// Path: /accounts/{account_id}/token
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/accounts/"), "/")
	accountID := parts[0]

	mu.RLock()
	account, exists := accounts[accountID]
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "ACCOUNT_NOT_FOUND",
			"message": fmt.Sprintf("Service account %s not found", accountID),
		})
		return
	}

	token, err := issueServiceAccountJWT(account)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"code": "TOKEN_ERROR", "message": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"account_id":     account.AccountID,
		"application_id": account.ApplicationID,
		"jwt":            token,
		"expires_at":     time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
		"clearance_level": "sec-wrangler",
	})
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	appCount := len(applications)
	acctCount := len(accounts)
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":               "healthy",
		"applications_registered": appCount,
		"service_accounts":     acctCount,
		"storage":              "in-memory (Phase 2)",
		"uptime_seconds":       int(time.Since(startTime).Seconds()),
	})
}

var startTime = time.Now()

// ---------------------------------------------------------------------------
// mTLS server
// ---------------------------------------------------------------------------

func loadTLSConfig() *tls.Config {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Fatalf("[policy] Failed to read CA cert: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	cert, err := tls.LoadX509KeyPair("/certs/policy.crt", "/certs/policy.key")
	if err != nil {
		log.Fatalf("[policy] Failed to load service cert: %v", err)
	}

	return &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

// ---------------------------------------------------------------------------
// AI Provider handlers
// ---------------------------------------------------------------------------

func ollamaModels(ollamaURL string) ([]string, error) {
	resp, err := http.Get(ollamaURL + "/api/tags")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

func handleAIProviders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		providerMu.RLock()
		list := make([]*AIProvider, 0, len(providers))
		for _, p := range providers {
			list = append(list, p)
		}
		providerMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"providers": list, "total": len(list)})

	case http.MethodPost:
		var req struct {
			Name        string `json:"name"`
			OllamaURL   string `json:"ollama_url"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.OllamaURL == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "name and ollama_url required"})
			return
		}
		providerID := strings.ToLower(strings.ReplaceAll(req.Name, " ", "-"))
		now := time.Now().UTC().Format(time.RFC3339)

		providerMu.Lock()
		existing, exists := providers[providerID]
		if exists {
			existing.OllamaURL = req.OllamaURL
			existing.Description = req.Description
			providerMu.Unlock()
			json.NewEncoder(w).Encode(existing)
			return
		}
		p := &AIProvider{
			ProviderID:      providerID,
			Name:            req.Name,
			OllamaURL:       req.OllamaURL,
			Description:     req.Description,
			Status:          "unknown",
			AvailableModels: []string{},
			RegisteredAt:    now,
		}
		providers[providerID] = p
		providerMu.Unlock()
		log.Printf("[policy] AI provider registered: %s → %s", providerID, req.OllamaURL)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(p)
	}
}

func handleAIProvider(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/ai-providers/"), "/")
	providerID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	// GET /ai-providers/active — special case
	if providerID == "active" {
		providerMu.RLock()
		var active *AIProvider
		for _, p := range providers {
			if p.ActiveModel != "" && p.Status == "reachable" {
				active = p
				break
			}
		}
		// Fallback: any provider with active model even if not recently verified
		if active == nil {
			for _, p := range providers {
				if p.ActiveModel != "" {
					active = p
					break
				}
			}
		}
		providerMu.RUnlock()
		if active == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"code": "NO_ACTIVE_PROVIDER", "message": "no AI provider with active model configured"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"provider_id":  active.ProviderID,
			"name":         active.Name,
			"ollama_url":   active.OllamaURL,
			"active_model": active.ActiveModel,
		})
		return
	}

	providerMu.RLock()
	p, exists := providers[providerID]
	providerMu.RUnlock()

	if !exists {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("provider %s not found", providerID)})
		return
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		json.NewEncoder(w).Encode(p)

	case r.Method == http.MethodDelete && action == "":
		providerMu.Lock()
		delete(providers, providerID)
		providerMu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": fmt.Sprintf("provider %s removed", providerID)})

	case r.Method == http.MethodPost && action == "refresh":
		models, err := ollamaModels(p.OllamaURL)
		now := time.Now().UTC().Format(time.RFC3339)
		providerMu.Lock()
		if err != nil {
			p.Status = "unreachable"
			p.LastRefreshedAt = now
			providerMu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"code": "PROVIDER_UNREACHABLE", "message": err.Error()})
			return
		}
		p.AvailableModels = models
		p.Status = "reachable"
		p.LastRefreshedAt = now
		providerMu.Unlock()
		log.Printf("[policy] AI provider %s refreshed: %d models available", providerID, len(models))
		json.NewEncoder(w).Encode(p)

	case r.Method == http.MethodPut && action == "active-model":
		var req struct {
			ModelName string `json:"model_name"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.ModelName == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "model_name required"})
			return
		}
		providerMu.Lock()
		p.ActiveModel = req.ModelName
		providerMu.Unlock()
		log.Printf("[policy] AI provider %s active model set to: %s", providerID, req.ModelName)
		json.NewEncoder(w).Encode(p)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	buildUpstreamClient()
	connectRedis()
	selfRegister()
	go registerJobsWithAC()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/applications", handleApplications)
	mux.HandleFunc("/applications/register", handleRegisterApplication)
	mux.HandleFunc("/applications/", handleGetApplication)
	mux.HandleFunc("/accounts/", handleIssueToken)
	mux.HandleFunc("/ai-providers", handleAIProviders)
	mux.HandleFunc("/ai-providers/", handleAIProvider)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadTLSConfig(),
	}

	log.Printf("[policy] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[policy] Self-registration: seti-self active")
	log.Printf("[policy] Storage: in-memory (Phase 2 — swap PostgreSQL in Phase 4)")

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[policy] Server error: %v", err)
	}
}
