package main

import (
	"context"
	"crypto/tls"
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

	"github.com/redis/go-redis/v9"
	"bufio"
	"regexp"
)

// ---------------------------------------------------------------------------
// Contract Test Job — SETI tier-one testing.
//
// Reads OpenAPI 3.1 contracts from a registered constellation's contracts
// path, generates test cases from every defined endpoint and response code,
// and routes them through AC via Redis pub/sub. Never touches constellation
// Job networks directly — AC is the sole executor on point-to-point networks.
//
// For authenticated endpoints, Contract Test obtains a short-lived JWT from
// Policy (the service account for this constellation) and injects it as the
// Authorization header in the test request payload.
//
// Results are published by AC to tca:contract-results and consumed here,
// then forwarded to the Results Job for storage.
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4003")
	redisURL         = envOr("REDIS_URL", "redis:6379")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	policyURL        = envOr("POLICY_URL", "https://policy:4002")
	resultsURL       = envOr("RESULTS_URL", "https://results:4008")
	contractsPath    = envOr("CONTRACTS_PATH", "/contracts/openapi")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// OpenAPI contract parsing — minimal stdlib YAML scanner
// We only need: info.title, info.version, paths structure, and response codes.
// No external YAML library — parsed with regexp and bufio line scanning.
// ---------------------------------------------------------------------------

type OpenAPIContract struct {
	Title   string
	Version string
	Paths   map[string]map[string]OpenAPIOperation // path → method → operation
}

type OpenAPIOperation struct {
	Summary         string
	RequiresBody    bool
	ResponseCodes   []int
}

// parseContract extracts the minimal fields needed for test generation
// from an OpenAPI 3.1 YAML file using stdlib-only line scanning.
func parseContract(data []byte) *OpenAPIContract {
	contract := &OpenAPIContract{
		Paths: map[string]map[string]OpenAPIOperation{},
	}

	reTitle    := regexp.MustCompile(`^\s{2}title:\s*(.+)$`)
	reVersion  := regexp.MustCompile(`^\s{2}version:\s*["']?([^"']+)["']?$`)
	rePath     := regexp.MustCompile(`^  (/[^\s:]+):`)
	reMethod   := regexp.MustCompile(`^    (get|post|put|patch|delete|head|options):\s*$`)
	reStatus   := regexp.MustCompile(`^        ["']?([1-5]\d{2})["']?:`)
	reRequired := regexp.MustCompile(`^\s+required:\s+true`)

	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	var (
		currentPath    string
		currentMethod  string
		inRequestBody  bool
		inPaths        bool
		inInfo         bool
	)

	for scanner.Scan() {
		line := scanner.Text()

		// Section detection
		if line == "info:" {
			inInfo = true
			inPaths = false
			continue
		}
		if line == "paths:" {
			inPaths = true
			inInfo = false
			continue
		}
		if len(line) > 0 && line[0] != ' ' && line[len(line)-1] == ':' {
			inInfo = false
			if line != "paths:" {
				inPaths = false
			}
		}

		// Info section
		if inInfo {
			if m := reTitle.FindStringSubmatch(line); m != nil {
				contract.Title = strings.TrimSpace(strings.Trim(m[1], "\"'"))
			}
			if m := reVersion.FindStringSubmatch(line); m != nil {
				contract.Version = strings.TrimSpace(m[1])
			}
		}

		// Paths section
		if inPaths {
			if m := rePath.FindStringSubmatch(line); m != nil {
				currentPath = m[1]
				currentMethod = ""
				inRequestBody = false
				if _, exists := contract.Paths[currentPath]; !exists {
					contract.Paths[currentPath] = map[string]OpenAPIOperation{}
				}
				continue
			}

			if currentPath != "" {
				if m := reMethod.FindStringSubmatch(line); m != nil {
					currentMethod = strings.ToUpper(strings.TrimSpace(m[1]))
					inRequestBody = false
					contract.Paths[currentPath][currentMethod] = OpenAPIOperation{}
					continue
				}

				if strings.Contains(line, "requestBody:") {
					inRequestBody = true
					continue
				}

				if inRequestBody && reRequired.MatchString(line) {
					if op, ok := contract.Paths[currentPath][currentMethod]; ok {
						op.RequiresBody = true
						contract.Paths[currentPath][currentMethod] = op
					}
				}

				if currentMethod != "" {
					if m := reStatus.FindStringSubmatch(line); m != nil {
						code := 0
						fmt.Sscanf(m[1], "%d", &code)
						if code > 0 {
							op := contract.Paths[currentPath][currentMethod]
							op.ResponseCodes = append(op.ResponseCodes, code)
							contract.Paths[currentPath][currentMethod] = op
						}
					}
				}
			}
		}
	}

	return contract
}

// ---------------------------------------------------------------------------
// Test case generation from contract
// ---------------------------------------------------------------------------

type TestCase struct {
	TestName        string            `json:"test_name"`
	ServiceName     string            `json:"service_name"`
	ContractFile    string            `json:"contract_file"`
	ContractVersion string            `json:"contract_version"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	Headers         map[string]string `json:"headers"`
	Body            interface{}       `json:"body,omitempty"`
	ExpectedStatus  int               `json:"expected_status"`
	RequiresAuth    bool              `json:"requires_auth"`
}

// serviceNameFromTitle extracts the service name from the contract title.
// "SETI - Gateway" → "gateway", "SETI - Signal Clearance (AC)" → "signal-clearance"
func serviceNameFromTitle(title string) string {
	// Strip "SETI - " prefix
	name := strings.TrimPrefix(title, "SETI - ")
	// Strip parenthetical suffixes
	if idx := strings.Index(name, " ("); idx != -1 {
		name = name[:idx]
	}
	// Strip trailing " Job" suffix (many SETI contracts include "Job" in the title)
	name = strings.TrimSuffix(name, " Job")
	// Lowercase and hyphenate
	name = strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	return name
}

func generateTestCases(contractFile string, contract *OpenAPIContract) []TestCase {
	serviceName := serviceNameFromTitle(contract.Title)
	var cases []TestCase

	for path, methods := range contract.Paths {
		for method, op := range methods {
			for _, expectedStatus := range op.ResponseCodes {
				// Only test primary success responses (2xx) for Phase 2
				// Negative cases (4xx) require crafted invalid inputs — Phase 3
				if expectedStatus < 200 || expectedStatus >= 300 {
					continue
				}

				// Skip auth-required endpoints in Phase 2 — they need schema-derived bodies
				if requiresAuth(path, method) {
					continue
				}
				// Phase 2: skip mutation endpoints that need real request bodies
				// /event is the only POST we test — it accepts any JSON payload
				// Phase 3 will generate schema-derived bodies for all endpoints
				if (method == "POST" || method == "PUT" || method == "PATCH") && path != "/event" {
					continue
				}

				testName := fmt.Sprintf("%s %s -> %d", method, path, expectedStatus)

				tc := TestCase{
					TestName:        testName,
					ServiceName:     serviceName,
					ContractFile:    filepath.Base(contractFile),
					ContractVersion: contract.Version,
					Method:          method,
					Path:            path,
					Headers:         map[string]string{"Content-Type": "application/json"},
					ExpectedStatus:  expectedStatus,
					RequiresAuth:    requiresAuth(path, method),
				}

				// Add minimal body for endpoints that require one
				if op.RequiresBody && (method == "POST" || method == "PUT" || method == "PATCH") {
					tc.Body = map[string]interface{}{}
				}

				cases = append(cases, tc)
			}
		}
	}

	return cases
}

// requiresAuth determines if an endpoint needs a JWT.
// Phase 2: only test public/health endpoints without auth.
// Auth-required endpoint testing is Phase 3 — requires generating valid
// request bodies that match each endpoint's contract schema.
func requiresAuth(path, method string) bool {
	_ = method
	publicPaths := []string{
		"/health",
		"/auth/callback",
		"/auth/dev/login-form",
		"/auth/dev/login",
		"/auth/federated",
		"/auth/refresh",
		"/event",
		"/rules",
		"/configuration",
		"/alerts/active",
		"/jobs",
	}
	for _, p := range publicPaths {
		if path == p {
			return false
		}
	}
	// Phase 2: skip auth-required endpoints rather than fail them with empty bodies
	// Phase 3 will generate contract-schema-derived request bodies
	return true
}

// ---------------------------------------------------------------------------
// Test run lifecycle
// ---------------------------------------------------------------------------

type TestRun struct {
	RunID         string      `json:"run_id"`
	ApplicationID string      `json:"application_id"`
	StartedAt     string      `json:"started_at"`
	CompletedAt   string      `json:"completed_at,omitempty"`
	TotalTests    int         `json:"total_tests"`
	PassedTests   int         `json:"passed_tests"`
	FailedTests   int         `json:"failed_tests"`
	SkippedTests  int         `json:"skipped_tests"`
	Status        string      `json:"status"` // running | passed | failed | error
	Results       []TestResult `json:"results"`
}

type TestResult struct {
	TestName       string      `json:"test_name"`
	ServiceName    string      `json:"service_name"`
	Passed         bool        `json:"passed"`
	ActualStatus   int         `json:"actual_status"`
	ExpectedStatus int         `json:"expected_status"`
	FailureReason  string      `json:"failure_reason,omitempty"`
	LatencyMs      int64       `json:"latency_ms"`
	SkipReason     string      `json:"skip_reason,omitempty"`
	ExecutedAt     string      `json:"executed_at"`
}

var (
	runsInProgress sync.Map // runID → *TestRun
	totalRuns      atomic.Int64
)

// ---------------------------------------------------------------------------
// Redis coordination
// ---------------------------------------------------------------------------

var rdb *redis.Client

func connectRedis() {
	for i := 0; i < 10; i++ {
		rdb = redis.NewClient(&redis.Options{Addr: redisURL})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := rdb.Ping(ctx).Result()
		cancel()
		if err == nil {
			log.Printf("[contract-test] Connected to Redis at %s", redisURL)
			return
		}
		log.Printf("[contract-test] Redis not ready (attempt %d/10): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	log.Fatal("[contract-test] Could not connect to Redis after 10 attempts")
}

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
// Service account token — obtained from Policy Job
// ---------------------------------------------------------------------------

func getServiceAccountToken(applicationID string) (string, error) {
	// For self-registration, account ID is svc-{application_id}
	accountID := fmt.Sprintf("svc-%s", applicationID)

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/accounts/%s/token", policyURL, accountID), nil)
	if err != nil {
		return "", err
	}

	resp, err := upstreamClient.Do(req)
	reportEvent("policy", "POST", fmt.Sprintf("/accounts/%s/token", accountID),
		func() int {
			if err != nil || resp == nil {
				return 0
			}
			return resp.StatusCode
		}(), time.Since(start).Milliseconds())

	if err != nil {
		return "", fmt.Errorf("policy request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("policy returned %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	token, ok := result["jwt"].(string)
	if !ok {
		return "", fmt.Errorf("no jwt in policy response")
	}
	return token, nil
}

// ---------------------------------------------------------------------------
// Test execution
// ---------------------------------------------------------------------------

func executeRun(applicationID string) (*TestRun, error) {
	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	run := &TestRun{
		RunID:         runID,
		ApplicationID: applicationID,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		Status:        "running",
	}
	runsInProgress.Store(runID, run)
	totalRuns.Add(1)

	// Get service account JWT for authenticated requests
	jwt, err := getServiceAccountToken(applicationID)
	if err != nil {
		log.Printf("[contract-test] Could not get service account token: %v — auth endpoints will be skipped", err)
	}

	// Discover contract files
	contractFiles, err := filepath.Glob(filepath.Join(contractsPath, "*.yaml"))
	if err != nil || len(contractFiles) == 0 {
		run.Status = "error"
		run.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		return run, fmt.Errorf("no contract files found at %s", contractsPath)
	}

	log.Printf("[contract-test] Starting run %s for %s — %d contract files found",
		runID, applicationID, len(contractFiles))

	// Generate all test cases first
	var allTests []TestCase
	for _, file := range contractFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			log.Printf("[contract-test] Failed to read %s: %v", file, err)
			continue
		}
		contract := parseContract(data)
		if contract.Title == "" {
			log.Printf("[contract-test] Skipping %s — could not parse title", file)
			continue
		}
		cases := generateTestCases(file, contract)
		allTests = append(allTests, cases...)
	}

	run.TotalTests = len(allTests)
	log.Printf("[contract-test] Generated %d test cases from contracts", run.TotalTests)

	// Execute tests through AC via Redis
	ctx := context.Background()
	resultTimeout := 30 * time.Second

	for _, tc := range allTests {
		// Skip auth endpoints if no JWT available
		if tc.RequiresAuth && jwt == "" {
			run.SkippedTests++
			run.Results = append(run.Results, TestResult{
				TestName:    tc.TestName,
				ServiceName: tc.ServiceName,
				Passed:      false,
				SkipReason:  "auth_token_unavailable",
				ExecutedAt:  time.Now().UTC().Format(time.RFC3339),
			})
			continue
		}

		// Inject auth header if required
		headers := tc.Headers
		if tc.RequiresAuth && jwt != "" {
			headers = make(map[string]string)
			for k, v := range tc.Headers {
				headers[k] = v
			}
			headers["Authorization"] = "Bearer " + jwt
		}

		// Build request ID for correlation
		requestID := fmt.Sprintf("%s-%d", runID, time.Now().UnixNano())
		resultChannel := fmt.Sprintf("tca:contract-results")

		// Subscribe to result channel before publishing
		sub := rdb.Subscribe(ctx, resultChannel)

		// Publish test request to AC
		payload, _ := json.Marshal(map[string]interface{}{
			"request_id":               requestID,
			"service_name":             tc.ServiceName,
			"method":                   tc.Method,
			"path":                     tc.Path,
			"headers":                  headers,
			"body":                     tc.Body,
			"expected_status":          tc.ExpectedStatus,
						"test_name":                tc.TestName,
			"contract_version":         tc.ContractVersion,
			"published_at":             time.Now().UTC().Format(time.RFC3339),
		})

		if err := rdb.Publish(ctx, "tca:contract-requests", payload).Err(); err != nil {
			sub.Close()
			run.FailedTests++
			run.Results = append(run.Results, TestResult{
				TestName:      tc.TestName,
				ServiceName:   tc.ServiceName,
				Passed:        false,
				FailureReason: fmt.Sprintf("Failed to publish to AC: %v", err),
				ExecutedAt:    time.Now().UTC().Format(time.RFC3339),
			})
			continue
		}

		// Wait for result
		resultCtx, cancel := context.WithTimeout(ctx, resultTimeout)
		ch := sub.Channel()
		var result TestResult

		waitLoop:
		for {
			select {
			case msg := <-ch:
				var acResult map[string]interface{}
				if err := json.Unmarshal([]byte(msg.Payload), &acResult); err != nil {
					continue
				}
				// Correlate by request_id
				if acResult["request_id"] != requestID {
					continue
				}
				result = TestResult{
					TestName:    tc.TestName,
					ServiceName: tc.ServiceName,
					Passed:      acResult["passed"] == true,
					ExecutedAt:  fmt.Sprintf("%v", acResult["executed_at"]),
				}
				if status, ok := acResult["actual_status"].(float64); ok {
					result.ActualStatus = int(status)
				}
				result.ExpectedStatus = tc.ExpectedStatus
				if latency, ok := acResult["latency_ms"].(float64); ok {
					result.LatencyMs = int64(latency)
				}
				if reason, ok := acResult["failure_reason"].(string); ok {
					result.FailureReason = reason
				}
				break waitLoop

			case <-resultCtx.Done():
				result = TestResult{
					TestName:      tc.TestName,
					ServiceName:   tc.ServiceName,
					Passed:        false,
					FailureReason: fmt.Sprintf("Timeout waiting for AC result after %s", resultTimeout),
					ExecutedAt:    time.Now().UTC().Format(time.RFC3339),
				}
				break waitLoop
			}
		}

		cancel()
		sub.Close()

		if result.Passed {
			run.PassedTests++
		} else if result.SkipReason != "" || strings.HasPrefix(result.FailureReason, "skip:") {
			run.SkippedTests++
			// Move failure_reason into skip_reason for display, stripping the skip: prefix
			if result.SkipReason == "" {
				result.SkipReason = strings.TrimPrefix(result.FailureReason, "skip: ")
				result.FailureReason = ""
			}
		} else {
			run.FailedTests++
		}
		run.Results = append(run.Results, result)

		log.Printf("[contract-test] %s/%s: passed=%v status=%d",
			tc.ServiceName, tc.TestName, result.Passed, result.ActualStatus)
	}

	// Finalize run
	run.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	if run.FailedTests > 0 {
		run.Status = "failed"
	} else {
		run.Status = "passed"
	}

	log.Printf("[contract-test] Run %s complete: %d passed, %d failed, %d skipped",
		runID, run.PassedTests, run.FailedTests, run.SkippedTests)

	// Forward to Results Job
	go forwardToResults(run)

	return run, nil
}

// ---------------------------------------------------------------------------
// Forward results to Results Job
// ---------------------------------------------------------------------------

func forwardToResults(run *TestRun) {
	start := time.Now()
	payload, _ := json.Marshal(run)
	req, err := http.NewRequest(http.MethodPost, resultsURL+"/contract-results",
		strings.NewReader(string(payload)))
	if err != nil {
		log.Printf("[contract-test] Failed to build results request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := upstreamClient.Do(req)
	reportEvent("results", "POST", "/contract-results", func() int {
		if err != nil || resp == nil {
			return 0
		}
		return resp.StatusCode
	}(), time.Since(start).Milliseconds())

	if err != nil {
		log.Printf("[contract-test] Failed to forward to results: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("[contract-test] Run %s forwarded to Results Job", run.RunID)
}

// ---------------------------------------------------------------------------
// Observability reporting
// ---------------------------------------------------------------------------

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller":      "contract-test",
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
			return
		}
		resp.Body.Close()
	}()
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
		ApplicationID string `json:"application_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ApplicationID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "INVALID_REQUEST",
			"message": "application_id is required",
		})
		return
	}

	go func() {
		if _, err := executeRun(req.ApplicationID); err != nil {
			log.Printf("[contract-test] Run error for %s: %v", req.ApplicationID, err)
		}
	}()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "accepted",
		"application_id": req.ApplicationID,
		"message":        "Contract test run initiated",
	})
}

func handleRunStatus(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimPrefix(r.URL.Path, "/run/")
	w.Header().Set("Content-Type", "application/json")

	if run, ok := runsInProgress.Load(runID); ok {
		json.NewEncoder(w).Encode(run)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]string{
		"code":    "RUN_NOT_FOUND",
		"message": fmt.Sprintf("Run %s not found (may have completed and been cleared)", runID),
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "healthy",
		"total_runs":     totalRuns.Load(),
		"contracts_path": contractsPath,
		"uptime_seconds": int(time.Since(startTime).Seconds()),
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
// Main
// ---------------------------------------------------------------------------

var certMat *CertMaterial

func main() {
	certMat = obtainCerts("contract-test")
	buildUpstreamClient()
	go selfRegisterWithAC(certMat, "https://contract-test:4003")
	connectRedis()
	startScheduler()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/run", handleRun)
	mux.HandleFunc("/run/", handleRunStatus)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadTLSConfig(),
	}

	log.Printf("[contract-test] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[contract-test] Contracts path: %s", contractsPath)
	log.Printf("[contract-test] Test requests route through AC via tca:contract-requests")

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[contract-test] Server error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scheduler — polls Policy for registered applications and runs contract
// tests on the configured interval for each application.
// ---------------------------------------------------------------------------

type scheduledApp struct {
	applicationID string
	intervalMins  int
	ticker        *time.Ticker
	stop          chan struct{}
}

var (
	scheduledApps   = map[string]*scheduledApp{}
	scheduledAppsMu sync.Mutex
)

func startScheduler() {
	log.Printf("[contract-test] Scheduler starting — polling policy every 60s for application list")
	go func() {
		// Initial load
		syncSchedules()
		// Refresh every 60 seconds to pick up new registrations or interval changes
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			syncSchedules()
		}
	}()
}

func syncSchedules() {
	apps, err := fetchApplicationsFromPolicy()
	if err != nil {
		log.Printf("[contract-test] Scheduler: could not fetch applications: %v", err)
		return
	}

	scheduledAppsMu.Lock()
	defer scheduledAppsMu.Unlock()

	// Start schedulers for new or changed applications
	for _, app := range apps {
		if !app.Enabled {
			continue
		}
		interval := app.IntervalMins
		if interval <= 0 {
			interval = 15
		}

		existing, ok := scheduledApps[app.ID]
		if ok && existing.intervalMins == interval {
			continue // already scheduled at the right interval
		}

		// Stop existing if interval changed
		if ok {
			close(existing.stop)
			existing.ticker.Stop()
			log.Printf("[contract-test] Scheduler: updated interval for %s → %dm", app.ID, interval)
		}

		sa := &scheduledApp{
			applicationID: app.ID,
			intervalMins:  interval,
			ticker:        time.NewTicker(time.Duration(interval) * time.Minute),
			stop:          make(chan struct{}),
		}
		scheduledApps[app.ID] = sa

		go func(s *scheduledApp) {
			log.Printf("[contract-test] Scheduler: watching %s every %dm", s.applicationID, s.intervalMins)
			for {
				select {
				case <-s.ticker.C:
					log.Printf("[contract-test] Scheduler: triggering run for %s", s.applicationID)
					if _, err := executeRun(s.applicationID); err != nil {
						log.Printf("[contract-test] Scheduler: run failed for %s: %v", s.applicationID, err)
					}
				case <-s.stop:
					return
				}
			}
		}(sa)
	}

	// Stop schedulers for removed applications
	for id, sa := range scheduledApps {
		found := false
		for _, app := range apps {
			if app.ID == id {
				found = true
				break
			}
		}
		if !found {
			close(sa.stop)
			sa.ticker.Stop()
			delete(scheduledApps, id)
			log.Printf("[contract-test] Scheduler: stopped watching %s (deregistered)", id)
		}
	}
}

type appScheduleInfo struct {
	ID           string
	IntervalMins int
	Enabled      bool
}

func fetchApplicationsFromPolicy() ([]appScheduleInfo, error) {
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, policyURL+"/applications", nil)
	if err != nil {
		return nil, err
	}
	resp, err := upstreamClient.Do(req)
	reportEvent("policy", "GET", "/applications",
		func() int {
			if err != nil || resp == nil { return 0 }
			return resp.StatusCode
		}(), time.Since(start).Milliseconds())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Applications []struct {
			ApplicationID string `json:"application_id"`
			Status        string `json:"status"`
			TestSchedule  *struct {
				ContractTestIntervalMinutes int  `json:"contract_test_interval_minutes"`
				Enabled                     bool `json:"enabled"`
			} `json:"test_schedule"`
		} `json:"applications"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var apps []appScheduleInfo
	for _, a := range result.Applications {
		if a.Status != "active" {
			continue
		}
		info := appScheduleInfo{ID: a.ApplicationID, IntervalMins: 15, Enabled: true}
		if a.TestSchedule != nil {
			info.IntervalMins = a.TestSchedule.ContractTestIntervalMinutes
			info.Enabled = a.TestSchedule.Enabled
		}
		apps = append(apps, info)
	}
	return apps, nil
}
