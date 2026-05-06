package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
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
	jwtSecret        = mustReadSecretFile("JWT_SECRET_FILE")
	signalAggURL     = envOr("SIGNAL_AGGREGATOR_URL", "https://signal-aggregator:4006")
)

func validateHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL must include a host")
	}
	return nil
}

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

// mustReadSecretFile reads a secret value from the file path given by the
// named environment variable. Used for secrets written by cert-forge to
// the /certs volume rather than passed as plain env vars.
func mustReadSecretFile(envKey string) []byte {
	path := os.Getenv(envKey)
	if path == "" {
		log.Fatalf("[policy] Required env var %s is not set", envKey)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("[policy] Failed to read secret file %s: %v", path, err)
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		log.Fatalf("[policy] Secret file %s is empty", path)
	}
	return data
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

func issueServiceAccountJWT(account *ServiceAccount) (string, error) {
	now := time.Now()
	claims := map[string]interface{}{
		"account_id":      account.AccountID,
		"application_id":  account.ApplicationID,
		"role":            account.Role,
		"clearance_level": "sec-wr4ngler",
		"wrangler_id":     account.AccountID,
		"iat":             now.Unix(),
		"exp":             now.Add(15 * time.Minute).Unix(),
	}
	return SignJWT(claims, jwtSecret)
}

// ---------------------------------------------------------------------------
// Redis client
// ---------------------------------------------------------------------------

var rdb *RedisClient

func connectRedis() {
	for i := 0; i < 10; i++ {
		rdb = NewRedisClient(redisURL)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := rdb.Ping(ctx)
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
// Called after go selfRegisterWithAC(certMat, "https://policy:4002") during startup.
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

func registerSelfApplication() {
	mu.Lock()
	defer mu.Unlock()

	if _, exists := applications["seti"]; exists {
		return
	}

	contractsPath := envOr("CONTRACTS_PATH", "/contracts/openapi")
	gatewayURL := envOr("SELF_GATEWAY_URL", "https://gateway:4000")
	selfRedisURL := envOr("REDIS_URL", "redis:6379")

	app := &Application{
		ApplicationID:  "seti",
		DisplayName:    "SETI (Self)",
		Description:    "SETI monitoring its own constellation. True mastery starts with oneself.",
		Environment:    "development",
		GatewayURL:     gatewayURL,
		RedisURL:       selfRedisURL,
		ContractsPath:  contractsPath,
		ServiceAccount: "svc-seti",
		TestSchedule: &TestSchedule{
			ContractTestIntervalMinutes: 15,
			PlotTestEnabled:             true,
			Enabled:                     true,
		},
		Status:       "active",
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}
	applications["seti"] = app

	// Create service account for SETI's own constellation
	account := &ServiceAccount{
		AccountID:     "svc-seti",
		ApplicationID: "seti",
		Role:          "contract-test",
		Description:   "SETI self-monitoring service account",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	accounts["svc-seti"] = account

	log.Printf("[policy] Self-registered SETI constellation (seti)")
	log.Printf("[policy] Contracts path: %s", contractsPath)
	log.Printf("[policy] Service account: svc-seti")
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
	upstreamClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: buildClientTLS(certMat),
		},
	}
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
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "METHOD_NOT_ALLOWED",
			"message": "use POST /applications/register to register an application",
		})
		return
	}
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

	if req.DisplayName == "" || req.GatewayURL == "" || req.RedisURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "INVALID_REQUEST",
			"message": "display_name, gateway_url, and redis_url are required",
		})
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

	if r.Method == http.MethodDelete {
		mu.Lock()
		delete(applications, appID)
		// Remove associated service account
		delete(accounts, fmt.Sprintf("svc-%s", appID))
		mu.Unlock()
		log.Printf("[policy] Application deleted: %s", appID)
		json.NewEncoder(w).Encode(map[string]string{
			"status":         "ok",
			"message":        fmt.Sprintf("Application %s deleted", appID),
			"application_id": appID,
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
		"clearance_level": "sec-wr4ngler",
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
	return buildServerTLS(certMat)
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
// Available Applications — remote-apps.json manifest
// ---------------------------------------------------------------------------

var (
	plotStoreURL    = envOr("PLOT_STORE_URL", "https://plot-store:4005")
	remoteAppsPath  = envOr("REMOTE_APPS_PATH", "/etc/seti/remote-apps.json")
)

type RemoteApp struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	ACEndpoint     string `json:"ac_endpoint"`
	RegistryURL    string `json:"registry_url"`
	RegistryType   string `json:"registry_type"` // github | gitlab | generic
	RegistryToken  string `json:"registry_token"`
}

type AvailableAppState struct {
	RemoteApp
	Tag                    string `json:"tag"`
	MonitoringStatus       string `json:"monitoring_status"` // active | inactive
	ApplicationID          string `json:"application_id,omitempty"`
	ActivatedAt            string `json:"activated_at,omitempty"`
	ConnectionLastTestedAt string `json:"connection_last_tested_at,omitempty"`
	ConnectionLastStatus   string `json:"connection_last_status"` // ok | failed | untested
}

var (
	remoteAppsMu    sync.RWMutex
	remoteApps      = map[string]*RemoteApp{}       // tag → definition from JSON
	availableStates = map[string]*AvailableAppState{} // tag → live state
)

func loadRemoteApps() {
	data, err := os.ReadFile(remoteAppsPath)
	if err != nil {
		log.Printf("[policy] remote-apps.json not found at %s — no available applications loaded", remoteAppsPath)
		return
	}
	raw := map[string]RemoteApp{}
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("[policy] Failed to parse remote-apps.json: %v", err)
		return
	}
	remoteAppsMu.Lock()
	for tag, app := range raw {
		a := app
		remoteApps[tag] = &a
		if _, exists := availableStates[tag]; !exists {
			availableStates[tag] = &AvailableAppState{
				RemoteApp:            a,
				Tag:                  tag,
				MonitoringStatus:     "inactive",
				ConnectionLastStatus: "untested",
			}
		}
	}
	remoteAppsMu.Unlock()
	log.Printf("[policy] Loaded %d available applications from remote-apps.json", len(raw))
}

func registryFetch(app *RemoteApp, path string) ([]byte, error) {
	url := strings.TrimRight(app.RegistryURL, "/") + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	switch app.RegistryType {
	case "gitlab":
		req.Header.Set("PRIVATE-TOKEN", app.RegistryToken)
	default: // github, generic
		req.Header.Set("Authorization", "Bearer "+app.RegistryToken)
	}
	req.Header.Set("Accept", "application/json")

	// Registry calls use plain HTTPS — not mTLS (external service).
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("registry returned %d", resp.StatusCode)
	}
	var buf []byte
	buf = make([]byte, 0)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

func countYAMLFiles(data []byte) int {
	// GitHub/GitLab return a JSON array of file objects.
	var files []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &files); err != nil {
		return 0
	}
	count := 0
	for _, f := range files {
		if (f.Type == "file" || f.Type == "blob") &&
			(strings.HasSuffix(f.Name, ".yaml") || strings.HasSuffix(f.Name, ".yml")) {
			count++
		}
	}
	return count
}

func handleAvailableApplications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	remoteAppsMu.RLock()
	list := make([]*AvailableAppState, 0, len(availableStates))
	active, inactive := 0, 0
	for _, s := range availableStates {
		list = append(list, s)
		if s.MonitoringStatus == "active" {
			active++
		} else {
			inactive++
		}
	}
	remoteAppsMu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"applications": list,
		"total":        len(list),
		"active_count": active,
		"inactive_count": inactive,
	})
}

func handleAvailableApplication(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Path: /available-applications/{tag}/{action}
	trimmed := strings.TrimPrefix(r.URL.Path, "/available-applications/")
	parts := strings.SplitN(trimmed, "/", 2)
	tag := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	remoteAppsMu.RLock()
	state, exists := availableStates[tag]
	app := remoteApps[tag]
	remoteAppsMu.RUnlock()

	if !exists {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "NOT_FOUND",
			"message": fmt.Sprintf("tag %q not found in remote-apps.json", tag),
		})
		return
	}

	switch {
	case r.Method == http.MethodPost && action == "test-connection":
		now := time.Now().UTC().Format(time.RFC3339)
		contractsData, err := registryFetch(app, "contracts/openapi")
		if err != nil {
			remoteAppsMu.Lock()
			state.ConnectionLastTestedAt = now
			state.ConnectionLastStatus = "failed"
			remoteAppsMu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"tag":       tag,
				"success":   false,
				"error":     err.Error(),
				"tested_at": now,
			})
			return
		}
		contractCount := countYAMLFiles(contractsData)

		plotCount := 0
		if plotsData, err := registryFetch(app, "contracts/plots"); err == nil {
			plotCount = countYAMLFiles(plotsData)
		}

		remoteAppsMu.Lock()
		state.ConnectionLastTestedAt = now
		state.ConnectionLastStatus = "ok"
		remoteAppsMu.Unlock()

		reportEvent("registry:"+tag, "GET", "contracts/openapi", 200, 0)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"tag":             tag,
			"success":         true,
			"contracts_found": contractCount,
			"plots_found":     plotCount,
			"tested_at":       now,
		})

	case r.Method == http.MethodPost && action == "register":
		remoteAppsMu.RLock()
		alreadyActive := exists && state.MonitoringStatus == "active"
		remoteAppsMu.RUnlock()

		if alreadyActive {
			json.NewEncoder(w).Encode(state)
			return
		}

		// Accept app definition from connie-agent body if not in remoteApps
		if !exists {
			var incoming struct {
				Name               string `json:"name"`
				Description        string `json:"description"`
				Namespace          string `json:"namespace"`
				ACEndpoint         string `json:"ac_endpoint"`
				FederationEndpoint string `json:"federation_endpoint"`
				CaURL              string `json:"ca_url"`
				RegistryURL        string `json:"registry_url"`
				RegistryType       string `json:"registry_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil || incoming.Name == "" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{
					"code":    "INVALID_REQUEST",
					"message": "tag not in remote-apps.json and no valid app definition in request body",
				})
				return
			}
			if incoming.ACEndpoint != "" {
				if err := validateHTTPSURL(incoming.ACEndpoint); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_ENDPOINT", "message": "ac_endpoint: " + err.Error()})
					return
				}
			}
			if incoming.RegistryURL != "" {
				if err := validateHTTPSURL(incoming.RegistryURL); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_ENDPOINT", "message": "registry_url: " + err.Error()})
					return
				}
			}
			remoteAppsMu.Lock()
			app = &RemoteApp{
				Name:         incoming.Name,
				Description:  incoming.Description,
				ACEndpoint:   incoming.ACEndpoint,
				RegistryURL:  incoming.RegistryURL,
				RegistryType: incoming.RegistryType,
			}
			remoteApps[tag] = app
			if availableStates[tag] == nil {
				availableStates[tag] = &AvailableAppState{
					Tag:              tag,
					MonitoringStatus: "inactive",
				}
			}
			state = availableStates[tag]
			remoteAppsMu.Unlock()
			log.Printf("[policy] Remote app %q registered dynamically by connie-agent", tag)
		}

		// 1. Register in Policy application store
		now := time.Now().UTC().Format(time.RFC3339)
		appID := "app-" + tag
		mu.Lock()
		if _, exists := applications[appID]; !exists {
			applications[appID] = &Application{
				ApplicationID: appID,
				DisplayName:   app.Name,
				Description:   app.Description,
				Environment:   "production",
				GatewayURL:    app.ACEndpoint, // AC endpoint as proxy for gateway location
				ContractsPath: app.RegistryURL + "/contracts/openapi",
				TestSchedule: &TestSchedule{
					ContractTestIntervalMinutes: 15,
					PlotTestEnabled:             true,
					Enabled:                     true,
				},
				Status:       "active",
				RegisteredAt: now,
			}
			acctID := "svc-" + tag
			accounts[acctID] = &ServiceAccount{
				AccountID:     acctID,
				ApplicationID: appID,
				Role:          "contract-test",
				Description:   fmt.Sprintf("Service account for %s monitoring", app.Name),
				CreatedAt:     now,
			}
		}
		mu.Unlock()

		// 2. Initiate AC federation via Signal Aggregator
		fedBody, _ := json.Marshal(map[string]string{
			"application_id": appID,
			"ac_endpoint":    app.ACEndpoint,
		})
		fedReq, _ := http.NewRequest(http.MethodPost, signalAggURL+"/federation/subscriptions",
			strings.NewReader(string(fedBody)))
		if fedReq != nil {
			fedReq.Header.Set("Content-Type", "application/json")
			start := time.Now()
			fedResp, fedErr := upstreamClient.Do(fedReq)
			latency := time.Since(start).Milliseconds()
			if fedErr != nil {
				log.Printf("[policy] Federation request failed for %s: %v", tag, fedErr)
				reportEvent("signal-aggregator", "POST", "/federation/subscriptions", 0, latency)
			} else {
				reportEvent("signal-aggregator", "POST", "/federation/subscriptions", fedResp.StatusCode, latency)
				fedResp.Body.Close()
			}
		}

		// 3. Trigger Plot Store ingest
		ingestBody, _ := json.Marshal(map[string]string{
			"registry_url":   app.RegistryURL,
			"registry_type":  app.RegistryType,
			"registry_token": app.RegistryToken,
		})
		ingestReq, _ := http.NewRequest(http.MethodPost,
			plotStoreURL+"/ingest/"+tag,
			strings.NewReader(string(ingestBody)))
		if ingestReq != nil {
			ingestReq.Header.Set("Content-Type", "application/json")
			start := time.Now()
			ingestResp, ingestErr := upstreamClient.Do(ingestReq)
			latency := time.Since(start).Milliseconds()
			if ingestErr != nil {
				log.Printf("[policy] Plot ingest failed for %s: %v", tag, ingestErr)
				reportEvent("plot-store", "POST", "/ingest/"+tag, 0, latency)
			} else {
				reportEvent("plot-store", "POST", "/ingest/"+tag, ingestResp.StatusCode, latency)
				ingestResp.Body.Close()
			}
		}

		remoteAppsMu.Lock()
		state.MonitoringStatus = "active"
		state.ApplicationID = appID
		state.ActivatedAt = now
		remoteAppsMu.Unlock()

		log.Printf("[policy] Available application registered: %s (%s)", tag, app.Name)
		json.NewEncoder(w).Encode(state)

	case r.Method == http.MethodPost && action == "deregister":
		remoteAppsMu.RLock()
		alreadyInactive := state.MonitoringStatus == "inactive"
		remoteAppsMu.RUnlock()

		if alreadyInactive {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "NOT_ACTIVE",
				"message": fmt.Sprintf("application %q is not currently active", tag),
			})
			return
		}

		// Stop AC federation
		appID := "app-" + tag
		delReq, _ := http.NewRequest(http.MethodDelete,
			signalAggURL+"/federation/subscriptions?application_id="+appID, nil)
		if delReq != nil {
			start := time.Now()
			delResp, delErr := upstreamClient.Do(delReq)
			latency := time.Since(start).Milliseconds()
			if delErr != nil {
				log.Printf("[policy] Federation disconnect failed for %s: %v", tag, delErr)
				reportEvent("signal-aggregator", "DELETE", "/federation/subscriptions", 0, latency)
			} else {
				reportEvent("signal-aggregator", "DELETE", "/federation/subscriptions", delResp.StatusCode, latency)
				delResp.Body.Close()
			}
		}

		// Mark inactive in Policy store (keep record, stop scheduling)
		mu.Lock()
		if reg, exists := applications[appID]; exists {
			reg.Status = "suspended"
		}
		mu.Unlock()

		remoteAppsMu.Lock()
		state.MonitoringStatus = "inactive"
		state.ApplicationID = ""
		state.ActivatedAt = ""
		remoteAppsMu.Unlock()

		log.Printf("[policy] Available application deregistered: %s (%s)", tag, app.Name)
		json.NewEncoder(w).Encode(state)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

var certMat *CertMaterial

func main() {
	certMat = obtainCerts("policy")
	buildUpstreamClient()
	connectRedis()
	loadRemoteApps()
	registerSelfApplication()
	selfRegisterWithAC(certMat, "https://policy:4002")
	go registerJobsWithAC()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/applications", handleApplications)
	mux.HandleFunc("/applications/register", handleRegisterApplication)
	mux.HandleFunc("/applications/", handleGetApplication)
	mux.HandleFunc("/accounts/", handleIssueToken)
	mux.HandleFunc("/ai-providers", handleAIProviders)
	mux.HandleFunc("/ai-providers/", handleAIProvider)
	mux.HandleFunc("/available-applications", handleAvailableApplications)
	mux.HandleFunc("/available-applications/", handleAvailableApplication)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadTLSConfig(),
	}

	log.Printf("[policy] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[policy] Self-registration: seti active")
	log.Printf("[policy] Storage: in-memory (Phase 2 — swap PostgreSQL in Phase 4)")

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[policy] Server error: %v", err)
		}
	}()
	awaitShutdown(server)
}
