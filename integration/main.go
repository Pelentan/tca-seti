package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Integration Job — versioned read-only API for customer tooling.
//
// Exposes a stable /v1 API surface that external tools (Grafana, dashboards,
// CI pipelines) can query for SETI results without coupling to internal
// service APIs. When internal services change, only Integration updates —
// no caller changes required.
//
// All endpoints are read-only. No writes, no mutations.
// Version is pinned in the URL — /v1 is stable. /v2 is a new Job.
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4013")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	policyURL        = envOr("POLICY_URL", "https://policy:4002")
	resultsURL       = envOr("RESULTS_URL", "https://results:4008")
	plotStoreURL     = envOr("PLOT_STORE_URL", "https://plot-store:4005")
	startTime        = time.Now()
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var upstreamClient *http.Client

func buildUpstreamClient() {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Printf("[integration] CA cert not found: %v", err)
		upstreamClient = http.DefaultClient
		return
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	cert, err := tls.LoadX509KeyPair("/certs/integration.crt", "/certs/integration.key")
	if err != nil {
		log.Printf("[integration] Service cert not found: %v", err)
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
		Timeout: 15 * time.Second,
	}
	log.Printf("[integration] mTLS upstream client ready")
}

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller": "integration", "callee": callee,
			"method": method, "path": path,
			"status_code": status, "latency_ms": latencyMs, "protocol": "mtls",
		})
		req, _ := http.NewRequest(http.MethodPost, observabilityURL+"/event",
			strings.NewReader(string(body)))
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

// proxy fetches from an upstream service and writes the response directly
func proxy(w http.ResponseWriter, upstreamURL, callee, method, path string) {
	start := time.Now()
	req, err := http.NewRequest(method, upstreamURL, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"code": "UPSTREAM_ERROR", "message": err.Error()})
		return
	}

	resp, err := upstreamClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		reportEvent(callee, method, path, 0, latency)
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"code": "UPSTREAM_UNAVAILABLE", "message": err.Error()})
		return
	}
	defer resp.Body.Close()
	reportEvent(callee, method, path, resp.StatusCode, latency)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)

	var body interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	json.NewEncoder(w).Encode(body)
}

// ---------------------------------------------------------------------------
// Handlers — /v1 API surface
// ---------------------------------------------------------------------------

func handleV1Applications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	proxy(w, policyURL+"/applications", "policy", "GET", "/applications")
}

func handleV1ApplicationHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	// Extract application_id from path: /v1/applications/{id}/health
	appID := extractPathSegment(r.URL.Path, "/v1/applications/", "/health")
	w.Header().Set("Content-Type", "application/json")

	// Compose health from most recent contract and plot run results
	var contractResults map[string]interface{}
	var plotResults map[string]interface{}

	contractStart := time.Now()
	req, _ := http.NewRequest(http.MethodGet, resultsURL+"/contract-results", nil)
	if req != nil {
		if resp, err := upstreamClient.Do(req); err == nil {
			json.NewDecoder(resp.Body).Decode(&contractResults)
			resp.Body.Close()
		}
	}
	reportEvent("results", "GET", "/contract-results", 200, time.Since(contractStart).Milliseconds())

	plotStart := time.Now()
	req2, _ := http.NewRequest(http.MethodGet, resultsURL+"/plot-results", nil)
	if req2 != nil {
		if resp, err := upstreamClient.Do(req2); err == nil {
			json.NewDecoder(resp.Body).Decode(&plotResults)
			resp.Body.Close()
		}
	}
	reportEvent("results", "GET", "/plot-results", 200, time.Since(plotStart).Milliseconds())

	// Compute simple health summary
	contractStatus := "unknown"
	if contractResults != nil {
		if runs, ok := contractResults["runs"].([]interface{}); ok && len(runs) > 0 {
			if latest, ok := runs[0].(map[string]interface{}); ok {
				if latest["application_id"] == appID {
					if latest["status"] == "passed" {
						contractStatus = "passing"
					} else {
						contractStatus = "failing"
					}
				}
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"application_id":  appID,
		"contract_status": contractStatus,
		"checked_at":      time.Now().UTC().Format(time.RFC3339),
	})
}

func handleV1ContractResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	proxy(w, resultsURL+"/contract-results", "results", "GET", "/contract-results")
}

func handleV1PlotResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	proxy(w, resultsURL+"/plot-results", "results", "GET", "/plot-results")
}

func handleV1ClusterHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// Query policy for all registered applications
	var appData map[string]interface{}
	req, _ := http.NewRequest(http.MethodGet, policyURL+"/applications", nil)
	if req != nil {
		start := time.Now()
		if resp, err := upstreamClient.Do(req); err == nil {
			json.NewDecoder(resp.Body).Decode(&appData)
			resp.Body.Close()
			reportEvent("policy", "GET", "/applications", resp.StatusCode, time.Since(start).Milliseconds())
		}
	}

	total := 0
	if appData != nil {
		if t, ok := appData["total"].(float64); ok {
			total = int(t)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":               "operational",
		"applications_tracked": total,
		"seti_version":         "0.3.0",
		"checked_at":           time.Now().UTC().Format(time.RFC3339),
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "healthy",
		"api_version":    "v1",
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

func extractPathSegment(path, prefix, suffix string) string {
	s := strings.TrimPrefix(path, prefix)
	s = strings.TrimSuffix(s, suffix)
	return s
}

// ---------------------------------------------------------------------------
// mTLS server
// ---------------------------------------------------------------------------

func loadServerTLS() *tls.Config {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Fatalf("[integration] CA cert not found — ensure cert-init completed before integration starts: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	cert, err := tls.LoadX509KeyPair("/certs/integration.crt", "/certs/integration.key")
	if err != nil {
		log.Fatalf("[integration] Service cert not found — ensure cert-init generated integration certs: %v", err)
	}
	return &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

func main() {
	buildUpstreamClient()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/v1/applications", handleV1Applications)
	mux.HandleFunc("/v1/applications/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/health"):
			handleV1ApplicationHealth(w, r)
		case strings.HasSuffix(path, "/results/contract"):
			handleV1ContractResults(w, r)
		case strings.HasSuffix(path, "/results/plot"):
			handleV1PlotResults(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "NOT_FOUND",
				"message": fmt.Sprintf("no route for %s", path),
			})
		}
	})
	mux.HandleFunc("/v1/cluster/health", handleV1ClusterHealth)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadServerTLS(),
	}

	log.Printf("[integration] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[integration] Versioned read-only API — /v1 surface")
	log.Printf("[integration] Policy: %s | Results: %s", policyURL, resultsURL)

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[integration] %v", err)
	}
}
