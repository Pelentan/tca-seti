package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Interactions Job — failure notification receiver.
//
// Phase 3 STUB: logs all received failure notifications visibly and returns
// a structured stub response. No actual AI analysis yet.
//
// Swap point: Phase 4 replaces the stub handler with calls to AI-lien for
// automated failure diagnosis and plot regeneration suggestions.
//
// Contract obligations:
//   POST /escalate        — receive a failed test run for escalation
//   POST /notify/regeneration — notify that a plot should be regenerated
//   GET  /health
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4009")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	ailienURL        = envOr("AI_LIEN_URL", "https://ai-lien:4252")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	escalationsReceived    atomic.Int64
	regenerationsReceived  atomic.Int64
	upstreamClient         *http.Client
	startTime              = time.Now()
)

func buildUpstreamClient() {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		upstreamClient = http.DefaultClient
		return
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	cert, err := tls.LoadX509KeyPair("/certs/interactions.crt", "/certs/interactions.key")
	if err != nil {
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
			"caller": "interactions", "callee": callee,
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

func handleEscalate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}

	var payload map[string]interface{}
	json.NewDecoder(r.Body).Decode(&payload)
	escalationsReceived.Add(1)

	runID, _ := payload["run_id"].(string)
	appID, _ := payload["application_id"].(string)
	failCount, _ := payload["failed_tests"].(float64)

	// STUB: log visibly that this is a stub — swap point for Phase 4 AI-lien
	log.Printf("[interactions] STUB: escalation received for run %s (%s) — %d failures — real AI analysis not yet active (Phase 4)", runID, appID, int(failCount))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "accepted",
		"stub_active": true,
		"message":     "STUB: escalation queued — AI-lien analysis active in Phase 4",
		"run_id":      runID,
		"escalation_id": "stub-" + runID,
	})
}

func handleRegeneration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}

	var payload map[string]interface{}
	json.NewDecoder(r.Body).Decode(&payload)
	regenerationsReceived.Add(1)

	plotID, _ := payload["plot_id"].(string)
	reason, _ := payload["reason"].(string)

	// STUB: log visibly — swap point for Phase 4
	log.Printf("[interactions] STUB: regeneration request for plot %s — reason: %s — real regeneration not yet active (Phase 4)", plotID, reason)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "accepted",
		"stub_active": true,
		"message":     "STUB: regeneration queued — AI-lien plot generation active in Phase 4",
		"plot_id":     plotID,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                  "healthy",
		"stub_active":             true,
		"escalations_received":    escalationsReceived.Load(),
		"regenerations_received":  regenerationsReceived.Load(),
		"phase_4_swap_point":      "Replace stub handlers with AI-lien calls when Phase 4 builds",
		"uptime_seconds":          int(time.Since(startTime).Seconds()),
	})
}

func loadServerTLS() *tls.Config {
	caCert, _ := os.ReadFile("/certs/ca.crt")
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	cert, err := tls.LoadX509KeyPair("/certs/interactions.crt", "/certs/interactions.key")
	if err != nil {
		log.Fatalf("[interactions] cert: %v", err)
	}
	return &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: caPool,
		Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13,
	}
}

func main() {
	buildUpstreamClient()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/escalate", handleEscalate)
	mux.HandleFunc("/notify/regeneration", handleRegeneration)

	server := &http.Server{Addr: ":" + port, Handler: mux, TLSConfig: loadServerTLS()}

	log.Printf("[interactions] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[interactions] STUB ACTIVE — Phase 4 swap point: replace with AI-lien calls")

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[interactions] %v", err)
	}
}
