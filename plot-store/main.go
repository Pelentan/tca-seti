package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Plot Store — owns the Plot database.
//
// A Plot is an AI-generated test scenario: a sequence of steps each defining
// an HTTP action to execute, expected call chains to verify via Signal
// Aggregator, and expected response shape. Plot Test loads Plots from here.
//
// Phase 3: in-memory storage.
// Swap point: replace with PostgreSQL in Phase 4.
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4005")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	plotsPath        = envOr("PLOTS_PATH", "/contracts/plots")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Plot model
// ---------------------------------------------------------------------------

type PlotStep struct {
	StepNumber    int                      `json:"step_number"`
	Description   string                   `json:"description"`
	Method        string                   `json:"method"`
	Path          string                   `json:"path"`
	Headers       map[string]string        `json:"headers,omitempty"`
	Body          interface{}              `json:"body,omitempty"`
	ExpectedStatus int                     `json:"expected_status"`
	ExpectedChain []ExpectedCall           `json:"expected_chain,omitempty"`
	VerifyWithin  int                      `json:"verify_within_seconds,omitempty"`
}

type ExpectedCall struct {
	Caller         string `json:"caller"`
	Callee         string `json:"callee"`
	Method         string `json:"method,omitempty"`
	Path           string `json:"path,omitempty"`
	MinOccurrences int    `json:"min_occurrences,omitempty"`
}

type Plot struct {
	PlotID        string     `json:"plot_id"`
	ApplicationID string     `json:"application_id"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	Version       string     `json:"version"`
	Author        string     `json:"author"` // "ai-lien" or wrangler ID
	Steps         []PlotStep `json:"steps"`
	Tags          []string   `json:"tags,omitempty"`
	Flagged       bool       `json:"flagged"`
	FlagReason    string     `json:"flag_reason,omitempty"`
	CreatedAt     string     `json:"created_at"`
	UpdatedAt     string     `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// In-memory store — Phase 3
// Swap point: replace with PostgreSQL in Phase 4
// ---------------------------------------------------------------------------

var (
	mu     sync.RWMutex
	plots  = map[string]*Plot{}
	nextID atomic.Int64
)

func generateID() string {
	return fmt.Sprintf("plot-%d", time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// mTLS
// ---------------------------------------------------------------------------

var upstreamClient *http.Client

func buildUpstreamClient() {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Printf("[plot-store] CA cert not found: %v", err)
		upstreamClient = http.DefaultClient
		return
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	cert, err := tls.LoadX509KeyPair("/certs/plot-store.crt", "/certs/plot-store.key")
	if err != nil {
		log.Printf("[plot-store] Service cert not found: %v", err)
		upstreamClient = http.DefaultClient
		return
	}
	upstreamClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: caPool, Certificates: []tls.Certificate{cert},
				MinVersion: tls.VersionTLS13,
			},
		},
		Timeout: 10 * time.Second,
	}
}

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller": "plot-store", "callee": callee,
			"method": method, "path": path,
			"status_code": status, "latency_ms": latencyMs, "protocol": "mtls",
		})
		req, _ := http.NewRequest(http.MethodPost, observabilityURL+"/event", strings.NewReader(string(body)))
		if req == nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := upstreamClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func handlePlots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	switch r.Method {
	case http.MethodGet:
		appFilter := r.URL.Query().Get("application_id")
		mu.RLock()
		result := make([]*Plot, 0, len(plots))
		for _, p := range plots {
			if appFilter != "" && p.ApplicationID != appFilter {
				continue
			}
			result = append(result, p)
		}
		mu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"plots": result, "total": len(result)})
		reportEvent("self", "GET", "/plots", 200, time.Since(start).Milliseconds())

	case http.MethodPost:
		var req Plot
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": err.Error()})
			return
		}
		if req.ApplicationID == "" || req.Name == "" || len(req.Steps) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "application_id, name, and at least one step required"})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		req.PlotID = generateID()
		req.CreatedAt = now
		req.UpdatedAt = now

		mu.Lock()
		plots[req.PlotID] = &req
		mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(req)
		log.Printf("[plot-store] Plot created: %s (%s) for %s — %d steps", req.PlotID, req.Name, req.ApplicationID, len(req.Steps))
		reportEvent("self", "POST", "/plots", 201, time.Since(start).Milliseconds())

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handlePlot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/plots/"), "/")
	plotID := parts[0]

	if len(parts) > 1 && parts[1] == "flag" && r.Method == http.MethodPost {
		var req struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		p, exists := plots[plotID]
		if exists {
			p.Flagged = true
			p.FlagReason = req.Reason
			p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		mu.Unlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("plot %s not found", plotID)})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "plot flagged"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		p, exists := plots[plotID]
		mu.RUnlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("plot %s not found", plotID)})
			return
		}
		json.NewEncoder(w).Encode(p)
		reportEvent("self", "GET", "/plots/"+plotID, 200, time.Since(start).Milliseconds())

	case http.MethodPut:
		var req Plot
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": err.Error()})
			return
		}
		mu.Lock()
		existing, exists := plots[plotID]
		if !exists {
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("plot %s not found", plotID)})
			return
		}
		req.PlotID = plotID
		req.CreatedAt = existing.CreatedAt
		req.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		plots[plotID] = &req
		mu.Unlock()
		json.NewEncoder(w).Encode(req)

	case http.MethodDelete:
		mu.Lock()
		_, exists := plots[plotID]
		if exists {
			delete(plots, plotID)
		}
		mu.Unlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("plot %s not found", plotID)})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": fmt.Sprintf("plot %s deleted", plotID)})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleAppPlots(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimPrefix(r.URL.Path, "/applications/")
	appID = strings.TrimSuffix(appID, "/plots")

	mu.RLock()
	result := make([]*Plot, 0)
	for _, p := range plots {
		if p.ApplicationID == appID {
			result = append(result, p)
		}
	}
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"plots": result, "total": len(result), "application_id": appID})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	count := len(plots)
	mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "healthy", "plots_stored": count,
		"storage": "in-memory (Phase 3 — swap PostgreSQL in Phase 4)",
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

var startTime = time.Now()

func loadServerTLS() *tls.Config {
	caCert, _ := os.ReadFile("/certs/ca.crt")
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	cert, err := tls.LoadX509KeyPair("/certs/plot-store.crt", "/certs/plot-store.key")
	if err != nil {
		log.Fatalf("[plot-store] cert: %v", err)
	}
	return &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: caPool,
		Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13,
	}
}

func loadPlotsFromDisk() {
	appDir := filepath.Join(plotsPath)
	entries, err := os.ReadDir(appDir)
	if err != nil {
		log.Printf("[plot-store] No plots directory found at %s — starting empty", plotsPath)
		return
	}

	loaded := 0
	for _, appEntry := range entries {
		if !appEntry.IsDir() {
			continue
		}
		appID := appEntry.Name()
		plotFiles, err := os.ReadDir(filepath.Join(appDir, appID))
		if err != nil {
			continue
		}

		for _, pf := range plotFiles {
			if !strings.HasSuffix(pf.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(appDir, appID, pf.Name()))
			if err != nil {
				log.Printf("[plot-store] Failed to read %s: %v", pf.Name(), err)
				continue
			}
			var p Plot
			if err := json.Unmarshal(data, &p); err != nil {
				log.Printf("[plot-store] Failed to parse %s: %v", pf.Name(), err)
				continue
			}
			if p.ApplicationID == "" {
				p.ApplicationID = appID
			}
			now := time.Now().UTC().Format(time.RFC3339)
			p.PlotID = generateID()
			p.CreatedAt = now
			p.UpdatedAt = now
			mu.Lock()
			plots[p.PlotID] = &p
			mu.Unlock()
			loaded++
			log.Printf("[plot-store] Loaded plot: %s (%s)", p.Name, pf.Name())
		}
	}
	log.Printf("[plot-store] Loaded %d plots from %s", loaded, plotsPath)
}

func main() {
	buildUpstreamClient()
	loadPlotsFromDisk()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/plots", handlePlots)
	mux.HandleFunc("/plots/", handlePlot)
	mux.HandleFunc("/applications/", handleAppPlots)

	server := &http.Server{Addr: ":" + port, Handler: mux, TLSConfig: loadServerTLS()}
	log.Printf("[plot-store] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[plot-store] Storage: in-memory (Phase 3 — swap PostgreSQL in Phase 4)")

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[plot-store] %v", err)
	}
}
