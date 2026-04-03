package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Plot Test Job — executes AI-generated Plots against registered applications.
//
// Flow per run:
//  1. Load Plot from Plot Store
//  2. For each step:
//     a. Execute the step's HTTP action via Augur Canis (Redis pub/sub, like Contract Test)
//     b. Verify expected call chain against Signal Aggregator
//     c. Verify response status and shape
//     d. Record step result
//  3. If any step fails: POST to Interactions for escalation
//  4. Store run results in Results Job
// ---------------------------------------------------------------------------

var (
	port               = envOr("PORT", "4004")
	observabilityURL   = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	plotStoreURL       = envOr("PLOT_STORE_URL", "https://plot-store:4005")
	signalAggURL       = envOr("SIGNAL_AGGREGATOR_URL", "https://signal-aggregator:4006")
	resultsURL         = envOr("RESULTS_URL", "https://results:4008")
	interactionsURL    = envOr("INTERACTIONS_URL", "https://interactions:4009")
	policyURL          = envOr("POLICY_URL", "https://policy:4002")
	redisURL           = envOr("REDIS_URL", "redis:6379")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Plot model (mirrors Plot Store)
// ---------------------------------------------------------------------------

type ExpectedCall struct {
	Caller         string `json:"caller"`
	Callee         string `json:"callee"`
	Method         string `json:"method,omitempty"`
	Path           string `json:"path,omitempty"`
	MinOccurrences int    `json:"min_occurrences,omitempty"`
}

type PlotStep struct {
	StepNumber     int               `json:"step_number"`
	Description    string            `json:"description"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           interface{}       `json:"body,omitempty"`
	ExpectedStatus int               `json:"expected_status"`
	ExpectedChain  []ExpectedCall    `json:"expected_chain,omitempty"`
	VerifyWithin   int               `json:"verify_within_seconds,omitempty"`
}

type Plot struct {
	PlotID        string     `json:"plot_id"`
	ApplicationID string     `json:"application_id"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	Steps         []PlotStep `json:"steps"`
}

// ---------------------------------------------------------------------------
// Run model
// ---------------------------------------------------------------------------

type StepResult struct {
	StepNumber     int            `json:"step_number"`
	Description    string         `json:"description"`
	Passed         bool           `json:"passed"`
	ActualStatus   int            `json:"actual_status"`
	ExpectedStatus int            `json:"expected_status"`
	ChainPassed    bool           `json:"chain_passed"`
	ChainMatched   []ExpectedCall `json:"chain_matched,omitempty"`
	ChainUnmatched []ExpectedCall `json:"chain_unmatched,omitempty"`
	FailureReason  string         `json:"failure_reason,omitempty"`
	LatencyMs      int64          `json:"latency_ms"`
	ExecutedAt     string         `json:"executed_at"`
	RequestURL     string         `json:"request_url,omitempty"`
	RequestMethod  string         `json:"request_method,omitempty"`
	RequestBody    interface{}    `json:"request_body,omitempty"`
	ResponseBody   interface{}    `json:"response_body,omitempty"`
}

type PlotRun struct {
	RunID         string       `json:"run_id"`
	PlotID        string       `json:"plot_id"`
	PlotName      string       `json:"plot_name"`
	ApplicationID string       `json:"application_id"`
	Status        string       `json:"status"` // running | passed | failed | error
	TotalSteps    int          `json:"total_steps"`
	PassedSteps   int          `json:"passed_steps"`
	FailedSteps   int          `json:"failed_steps"`
	Steps         []StepResult `json:"steps"`
	StartedAt     string       `json:"started_at"`
	CompletedAt   string       `json:"completed_at,omitempty"`
}

var (
	runsMu   sync.RWMutex
	runs     = map[string]*PlotRun{}
	totalRuns atomic.Int64
	startTime = time.Now()
)

// ---------------------------------------------------------------------------
// mTLS client
// ---------------------------------------------------------------------------

var upstreamClient *http.Client

func buildUpstreamClient() {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Printf("[plot-test] CA cert not found: %v", err)
		upstreamClient = http.DefaultClient
		return
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	cert, err := tls.LoadX509KeyPair("/certs/plot-test.crt", "/certs/plot-test.key")
	if err != nil {
		log.Printf("[plot-test] Service cert not found: %v", err)
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
		Timeout: 30 * time.Second,
	}
}

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller": "plot-test", "callee": callee,
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
// Helpers — upstream calls
// ---------------------------------------------------------------------------

func getJSON(url string, out interface{}) (int, error) {
	start := time.Now()
	resp, err := upstreamClient.Get(url)
	latency := time.Since(start).Milliseconds()

	parts := strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)
	callee := parts[0]
	path := "/"
	if len(parts) > 1 {
		path = "/" + parts[1]
	}

	if err != nil {
		reportEvent(callee, "GET", path, 0, latency)
		return 0, err
	}
	defer resp.Body.Close()
	reportEvent(callee, "GET", path, resp.StatusCode, latency)
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(url string, body interface{}, out interface{}) (int, error) {
	start := time.Now()
	payload, _ := json.Marshal(body)
	resp, err := upstreamClient.Post(url, "application/json", bytes.NewReader(payload))
	latency := time.Since(start).Milliseconds()

	parts := strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)
	callee := parts[0]
	path := "/"
	if len(parts) > 1 {
		path = "/" + parts[1]
	}

	if err != nil {
		reportEvent(callee, "POST", path, 0, latency)
		return 0, err
	}
	defer resp.Body.Close()
	reportEvent(callee, "POST", path, resp.StatusCode, latency)
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// Execute a single plot step via Augur Canis (same Redis mechanism as Contract Test)
// ---------------------------------------------------------------------------

func executeStep(applicationID string, step PlotStep, jwt string) (int, interface{}, int64, error) {
	// Get the application's gateway URL from Policy
	var appData map[string]interface{}
	status, err := getJSON(fmt.Sprintf("%s/applications/%s", policyURL, applicationID), &appData)
	if err != nil || status != 200 {
		return 0, nil, 0, fmt.Errorf("could not get application %s from Policy: status %d", applicationID, status)
	}

	gatewayURL, _ := appData["gateway_url"].(string)
	if gatewayURL == "" {
		return 0, nil, 0, fmt.Errorf("no gateway_url for application %s", applicationID)
	}

	// Execute the step directly against the application gateway
	// (Plot Test hits the gateway, which routes internally — this tests real user flows)
	targetURL := gatewayURL + step.Path

	var bodyReader io.Reader
	if step.Body != nil {
		bodyBytes, _ := json.Marshal(step.Body)
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequest(step.Method, targetURL, bodyReader)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("failed to build request: %v", err)
	}
	for k, v := range step.Headers {
		req.Header.Set(k, v)
	}
	if step.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}

	start := time.Now()
	resp, err := upstreamClient.Do(req)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		reportEvent(gatewayURL, step.Method, step.Path, 0, latency)
		return 0, nil, latency, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()
	reportEvent(gatewayURL, step.Method, step.Path, resp.StatusCode, latency)

	var responseBody interface{}
	json.NewDecoder(resp.Body).Decode(&responseBody)
	return resp.StatusCode, responseBody, latency, nil
}

// ---------------------------------------------------------------------------
// Verify call chain against Signal Aggregator
// ---------------------------------------------------------------------------

func verifyChain(applicationID string, after time.Time, step PlotStep) (bool, []ExpectedCall, []ExpectedCall) {
	if len(step.ExpectedChain) == 0 {
		return true, nil, nil
	}

	within := step.VerifyWithin
	if within == 0 {
		within = 30
	}

	var result struct {
		Passed    bool           `json:"passed"`
		Matched   []ExpectedCall `json:"matched"`
		Unmatched []ExpectedCall `json:"unmatched"`
	}

	_, err := postJSON(signalAggURL+"/verify/call-chain", map[string]interface{}{
		"application_id": applicationID,
		"after":          after.UTC().Format(time.RFC3339Nano),
		"within_seconds": within,
		"expected_calls": step.ExpectedChain,
	}, &result)

	if err != nil {
		log.Printf("[plot-test] Signal Aggregator verify failed: %v", err)
		return false, nil, step.ExpectedChain
	}

	return result.Passed, result.Matched, result.Unmatched
}

// ---------------------------------------------------------------------------
// Execute a full plot run
// ---------------------------------------------------------------------------

func executeRun(plotID, applicationID string) *PlotRun {
	runID := fmt.Sprintf("plot-run-%d", time.Now().UnixNano())

	run := &PlotRun{
		RunID:         runID,
		PlotID:        plotID,
		ApplicationID: applicationID,
		Status:        "running",
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	runsMu.Lock()
	runs[runID] = run
	runsMu.Unlock()
	totalRuns.Add(1)

	// Load plot from Plot Store
	var plot Plot
	status, err := getJSON(fmt.Sprintf("%s/plots/%s", plotStoreURL, plotID), &plot)
	if err != nil || status != 200 {
		run.Status = "error"
		run.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		log.Printf("[plot-test] Could not load plot %s: status %d err %v", plotID, status, err)
		return run
	}

	run.PlotName = plot.Name
	run.TotalSteps = len(plot.Steps)

	// Get service account JWT for authenticated requests
	var tokenData map[string]interface{}
	jwt := ""
	accountID := fmt.Sprintf("svc-%s", applicationID)
	if _, err := postJSON(fmt.Sprintf("%s/accounts/%s/token", policyURL, accountID), nil, &tokenData); err == nil {
		if t, ok := tokenData["jwt"].(string); ok {
			jwt = t
		}
	}

	// Execute each step
	for _, step := range plot.Steps {
		stepStart := time.Now()
		actualStatus, responseBody, latency, execErr := executeStep(applicationID, step, jwt)

		stepResult := StepResult{
			StepNumber:     step.StepNumber,
			Description:    step.Description,
			ExpectedStatus: step.ExpectedStatus,
			ActualStatus:   actualStatus,
			LatencyMs:      latency,
			ExecutedAt:     stepStart.UTC().Format(time.RFC3339),
			RequestMethod:  step.Method,
			RequestBody:    step.Body,
			ResponseBody:   responseBody,
		}

		if execErr != nil {
			stepResult.Passed = false
			stepResult.ChainPassed = false
			stepResult.FailureReason = execErr.Error()
		} else {
			// Check response status
			statusPassed := actualStatus == step.ExpectedStatus

			// Brief delay to allow observability events to propagate through
			// the pipeline: gateway → seti-observability → Redis → Signal Aggregator
			time.Sleep(500 * time.Millisecond)

			// Verify call chain
			chainPassed, matched, unmatched := verifyChain(applicationID, stepStart, step)
			stepResult.ChainPassed = chainPassed
			stepResult.ChainMatched = matched
			stepResult.ChainUnmatched = unmatched

			stepResult.Passed = statusPassed && chainPassed
			if !statusPassed {
				stepResult.FailureReason = fmt.Sprintf("Expected status %d, got %d", step.ExpectedStatus, actualStatus)
			} else if !chainPassed {
				stepResult.FailureReason = fmt.Sprintf("%d expected calls not observed in event stream", len(unmatched))
			}
		}

		if stepResult.Passed {
			run.PassedSteps++
		} else {
			run.FailedSteps++
		}
		run.Steps = append(run.Steps, stepResult)

		log.Printf("[plot-test] Step %d/%d (%s): passed=%v status=%d chain=%v latency=%dms",
			step.StepNumber, run.TotalSteps, step.Description,
			stepResult.Passed, actualStatus, stepResult.ChainPassed, latency)
	}

	if run.FailedSteps > 0 {
		run.Status = "failed"
	} else {
		run.Status = "passed"
	}
	run.CompletedAt = time.Now().UTC().Format(time.RFC3339)

	log.Printf("[plot-test] Run %s complete: %d passed, %d failed", runID, run.PassedSteps, run.FailedSteps)

	// Store in Results Job
	go func() {
		postJSON(resultsURL+"/plot-results", run, nil)
	}()

	// Escalate failures to Interactions
	if run.FailedSteps > 0 {
		go func() {
			postJSON(interactionsURL+"/escalate", map[string]interface{}{
				"run_id":         runID,
				"plot_id":        plotID,
				"application_id": applicationID,
				"failed_steps":   run.FailedSteps,
				"total_steps":    run.TotalSteps,
			}, nil)
		}()
	}

	return run
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	var req struct {
		PlotID        string `json:"plot_id"`
		ApplicationID string `json:"application_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PlotID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "plot_id required"})
		return
	}
	if req.ApplicationID == "" {
		req.ApplicationID = "seti-self"
	}

	// Run async, return immediately
	go executeRun(req.PlotID, req.ApplicationID)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "accepted",
		"plot_id":        req.PlotID,
		"application_id": req.ApplicationID,
		"message":        "Plot test run initiated",
	})
}

func handleRunStatus(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimPrefix(r.URL.Path, "/run/")
	w.Header().Set("Content-Type", "application/json")
	runsMu.RLock()
	run, exists := runs[runID]
	runsMu.RUnlock()
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("run %s not found", runID)})
		return
	}
	json.NewEncoder(w).Encode(run)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "healthy",
		"total_runs":     totalRuns.Load(),
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

func loadServerTLS() *tls.Config {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Fatalf("[plot-test] CA cert not found: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	cert, err := tls.LoadX509KeyPair("/certs/plot-test.crt", "/certs/plot-test.key")
	if err != nil {
		log.Fatalf("[plot-test] cert: %v — ensure cert-init completed before plot-test starts", err)
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
	mux.HandleFunc("/run", handleRun)
	mux.HandleFunc("/run/", handleRunStatus)

	server := &http.Server{Addr: ":" + port, Handler: mux, TLSConfig: loadServerTLS()}

	log.Printf("[plot-test] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[plot-test] Plot Store: %s | Signal Aggregator: %s | Interactions: %s",
		plotStoreURL, signalAggURL, interactionsURL)

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[plot-test] %v", err)
	}
}
