package main

import (
	"bytes"
	"context"
	"crypto/tls"
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

// PlotCall mirrors the contract PlotCall schema.
type PlotCall struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    interface{}       `json:"body,omitempty"`
}

// PlotAssertion defines a semantic assertion on a step's response body.
type PlotAssertion struct {
	Field    string      `json:"field"`
	Operator string      `json:"operator"`
	Value    interface{} `json:"value,omitempty"`
}

// CaptureDefinition extracts a value from the response for later steps.
type CaptureDefinition struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ExpectedCall struct {
	Caller         string `json:"caller"`
	Callee         string `json:"callee"`
	Method         string `json:"method,omitempty"`
	Path           string `json:"path,omitempty"`
	MinOccurrences int    `json:"min_occurrences,omitempty"`
}

// PlotStep supports both the contract schema (Call wrapper) and the legacy flat format.
type PlotStep struct {
	StepNumber  int    `json:"step_number"`
	Description string `json:"description"`

	// Contract schema — preferred
	Call            *PlotCall           `json:"call,omitempty"`
	ExpectStatus    int                 `json:"expect_status,omitempty"`
	Assertions      []PlotAssertion     `json:"assertions,omitempty"`
	Capture         []CaptureDefinition `json:"capture,omitempty"`
	ExpectCallChain []ExpectedCall      `json:"expect_call_chain,omitempty"`

	// Legacy flat format — backward compatible
	Method           string            `json:"method,omitempty"`
	Path             string            `json:"path,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	Body             interface{}       `json:"body,omitempty"`
	ExpectedStatus   int               `json:"expected_status,omitempty"`
	ExpectedStatuses []int             `json:"expected_statuses,omitempty"` // any one match passes; overrides expected_status
	ExpectedChain    []ExpectedCall    `json:"expected_chain,omitempty"`
	VerifyWithin     int               `json:"verify_within_seconds,omitempty"`
}

// Normalize resolves dual-format fields into canonical flat fields.
// Must be called before execution. Applies capture substitutions to path and body.
func (s *PlotStep) Normalize(captures map[string]string) {
	if s.Call != nil {
		s.Method = s.Call.Method
		s.Path = s.Call.Path
		if s.Call.Headers != nil {
			s.Headers = s.Call.Headers
		}
		if s.Call.Body != nil {
			s.Body = s.Call.Body
		}
	}
	if s.ExpectStatus != 0 && s.ExpectedStatus == 0 {
		s.ExpectedStatus = s.ExpectStatus
	}
	if len(s.ExpectCallChain) > 0 && len(s.ExpectedChain) == 0 {
		s.ExpectedChain = s.ExpectCallChain
	}
	if len(captures) > 0 {
		s.Path = applyCaptures(s.Path, captures)
		if bodyStr, ok := s.Body.(string); ok {
			s.Body = applyCaptures(bodyStr, captures)
		}
		for i := range s.ExpectedChain {
			s.ExpectedChain[i].Path = applyCaptures(s.ExpectedChain[i].Path, captures)
		}
	}
}

func applyCaptures(s string, captures map[string]string) string {
	for k, v := range captures {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// resolvePath navigates a dot-notation path into a decoded JSON value.
// Returns the value and true if found, nil and false if not.
func resolvePath(data interface{}, path string) (interface{}, bool) {
	if path == "" {
		return data, true
	}
	parts := strings.SplitN(path, ".", 2)
	m, ok := data.(map[string]interface{})
	if !ok {
		return nil, false
	}
	val, exists := m[parts[0]]
	if !exists {
		return nil, false
	}
	if len(parts) == 1 {
		return val, true
	}
	return resolvePath(val, parts[1])
}

// evaluateAssertion checks a single PlotAssertion against the response body.
type AssertionResult struct {
	Field         string      `json:"field"`
	Operator      string      `json:"operator"`
	ExpectedValue interface{} `json:"expected_value,omitempty"`
	ActualValue   interface{} `json:"actual_value,omitempty"`
	Passed        bool        `json:"passed"`
}

func evaluateAssertion(a PlotAssertion, body interface{}) AssertionResult {
	result := AssertionResult{Field: a.Field, Operator: a.Operator, ExpectedValue: a.Value}
	actual, found := resolvePath(body, a.Field)
	result.ActualValue = actual

	switch a.Operator {
	case "exists":
		result.Passed = found
	case "not_exists":
		result.Passed = !found
	case "equals", "eq":
		result.Passed = found && fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", a.Value)
	case "not_equals":
		result.Passed = found && fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", a.Value)
	case "contains":
		if s, ok := actual.(string); ok {
			result.Passed = found && strings.Contains(s, fmt.Sprintf("%v", a.Value))
		}
	case "not_contains":
		if s, ok := actual.(string); ok {
			result.Passed = found && !strings.Contains(s, fmt.Sprintf("%v", a.Value))
		}
	case "present":
		result.Passed = found && actual != nil
	case "gte", "greater_than_or_equal":
		if n, ok := actual.(float64); ok {
			if v, ok := a.Value.(float64); ok {
				result.Passed = n >= v
			}
		}
	case "greater_than":
		if n, ok := actual.(float64); ok {
			if v, ok := a.Value.(float64); ok {
				result.Passed = n > v
			}
		}
	case "less_than":
		if n, ok := actual.(float64); ok {
			if v, ok := a.Value.(float64); ok {
				result.Passed = n < v
			}
		}
	default:
		result.Passed = false
	}
	return result
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
	StepNumber        int               `json:"step_number"`
	Description       string            `json:"description"`
	Passed            bool              `json:"passed"`
	ActualStatus      int               `json:"actual_status"`
	ExpectedStatus    int               `json:"expected_status"`
	ChainPassed       bool              `json:"chain_passed"`
	ChainMatched      []ExpectedCall    `json:"chain_matched,omitempty"`
	ChainUnmatched    []ExpectedCall    `json:"chain_unmatched,omitempty"`
	AssertionResults  []AssertionResult `json:"assertion_results,omitempty"`
	AssertionsPassed  int               `json:"assertions_passed"`
	AssertionsFailed  int               `json:"assertions_failed"`
	FailureReason     string            `json:"failure_reason,omitempty"`
	LatencyMs         int64             `json:"latency_ms"`
	AttemptsCount     int               `json:"attempts_count"` // >1 means retries were needed
	ExecutedAt        string            `json:"executed_at"`
	RequestMethod     string            `json:"request_method,omitempty"`
	RequestBody       interface{}       `json:"request_body,omitempty"`
	ResponseBody      interface{}       `json:"response_body,omitempty"`
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

// Retry constants for transient failures (pod recycle, connection refused, 5xx).
// Hardcoded by design — change requires code review and rebuild.
const (
	stepMaxAttempts   = 3
	stepRetryInterval = 10 * time.Second
)

var (
	runsMu    sync.RWMutex
	runs      = map[string]*PlotRun{}
	totalRuns atomic.Int64
	startTime = time.Now()
	rdb       *RedisClient
)

func connectRedis() {
	rdb = NewRedisClient(redisURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx); err != nil {
		log.Printf("[plot-test] Redis not available: %v — whiff buffer disabled", err)
		rdb = nil
	} else {
		log.Printf("[plot-test] Connected to Redis at %s", redisURL)
	}
}

// writeStepWhiff writes a step execution result to the bad-whiff Redis Stream.
// Fire-and-forget. Every step writes regardless of pass/fail.
func writeStepWhiff(applicationID, runID string, step PlotStep, result StepResult) {
	if rdb == nil {
		return
	}
	go func() {
		key := "seti:whiff:" + applicationID
		fields := map[string]string{
			"application_id":    applicationID,
			"run_id":            runID,
			"step_number":       fmt.Sprintf("%d", result.StepNumber),
			"description":       result.Description,
			"method":            result.RequestMethod,
			"path":              step.Path,
			"expected_status":   fmt.Sprintf("%d", result.ExpectedStatus),
			"actual_status":     fmt.Sprintf("%d", result.ActualStatus),
			"passed":            fmt.Sprintf("%v", result.Passed),
			"chain_passed":      fmt.Sprintf("%v", result.ChainPassed),
			"assertions_passed": fmt.Sprintf("%d", result.AssertionsPassed),
			"assertions_failed": fmt.Sprintf("%d", result.AssertionsFailed),
			"latency_ms":        fmt.Sprintf("%d", result.LatencyMs),
			"attempts_count":    fmt.Sprintf("%d", result.AttemptsCount),
			"recorded_at":       result.ExecutedAt,
		}
		if _, err := rdb.XAdd(context.Background(), key, 10000, fields); err != nil {
			log.Printf("[plot-test] whiff buffer write failed for %s step %d: %v",
				applicationID, result.StepNumber, err)
		}
	}()
}

// ---------------------------------------------------------------------------
// mTLS client
// ---------------------------------------------------------------------------

var upstreamClient *http.Client

func buildUpstreamClient() {
	upstreamClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: buildClientTLS(certMat),
		},
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

// isRetryable returns true for failures that are likely transient —
// network errors and 5xx responses that indicate a pod mid-recycle.
// 4xx responses, assertion failures, and chain failures are NOT retried:
// those are real failures that retrying won't fix.
func isRetryable(status int, err error) bool {
	if err != nil {
		return true // network-level failure: connection refused, timeout, DNS
	}
	if status == http.StatusNotImplemented {
		return false // 501 Not Implemented is permanent, not transient
	}
	return status >= 500 // 5xx: pod degraded or mid-recycle
}

// executeStepWithRetry wraps executeStep with retry logic for transient failures.
// Attempts up to stepMaxAttempts times with stepRetryInterval between each.
// Returns the result, attempt count, and whether any retry was needed.
func executeStepWithRetry(applicationID string, step PlotStep, jwt string) (int, interface{}, int64, error, int) {
	var (
		status       int
		responseBody interface{}
		latency      int64
		execErr      error
	)

	for attempt := 1; attempt <= stepMaxAttempts; attempt++ {
		status, responseBody, latency, execErr = executeStep(applicationID, step, jwt)

		if !isRetryable(status, execErr) {
			return status, responseBody, latency, execErr, attempt
		}

		if attempt < stepMaxAttempts {
			reason := fmt.Sprintf("status=%d", status)
			if execErr != nil {
				reason = execErr.Error()
			}
			log.Printf("[plot-test] Step %d transient failure (attempt %d/%d): %s — retrying in %s",
				step.StepNumber, attempt, stepMaxAttempts, reason, stepRetryInterval)
			time.Sleep(stepRetryInterval)
		}
	}

	// All attempts exhausted — return last result with attempt count
	if execErr != nil {
		execErr = fmt.Errorf("retry_exhausted after %d attempts: %w", stepMaxAttempts, execErr)
	} else {
		execErr = fmt.Errorf("retry_exhausted after %d attempts: last status %d", stepMaxAttempts, status)
	}
	return status, responseBody, latency, execErr, stepMaxAttempts
}

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
	captures := map[string]string{} // capture store — scoped to this run
	for _, step := range plot.Steps {
		stepStart := time.Now()

		// Normalize resolves call wrapper / expect_status / expect_call_chain
		// and applies any captures from prior steps to path and body
		step.Normalize(captures)

		actualStatus, responseBody, latency, execErr, attempts := executeStepWithRetry(applicationID, step, jwt)

		stepResult := StepResult{
			StepNumber:     step.StepNumber,
			Description:    step.Description,
			ExpectedStatus: step.ExpectedStatus,
			ActualStatus:   actualStatus,
			LatencyMs:      latency,
			AttemptsCount:  attempts,
			ExecutedAt:     stepStart.UTC().Format(time.RFC3339),
			RequestMethod:  step.Method,
			RequestBody:    step.Body,
			ResponseBody:   responseBody,
		}
		// When only expected_statuses is used, populate ExpectedStatus with
		// the actual status if it matched (so the UI shows a clean pass),
		// or the first listed value if it didn't (so the UI shows a real mismatch).
		if step.ExpectedStatus == 0 && len(step.ExpectedStatuses) > 0 {
			matched := false
			for _, s := range step.ExpectedStatuses {
				if actualStatus == s {
					stepResult.ExpectedStatus = actualStatus
					matched = true
					break
				}
			}
			if !matched {
				stepResult.ExpectedStatus = step.ExpectedStatuses[0]
			}
		}

		if attempts > 1 {
			log.Printf("[plot-test] Step %d required %d attempts", step.StepNumber, attempts)
		}

		if execErr != nil {
			stepResult.Passed = false
			stepResult.ChainPassed = false
			stepResult.FailureReason = execErr.Error()
		} else {
			statusPassed := actualStatus == step.ExpectedStatus
		if !statusPassed && len(step.ExpectedStatuses) > 0 {
			for _, s := range step.ExpectedStatuses {
				if actualStatus == s {
					statusPassed = true
					break
				}
			}
		}

			// Extract capture values from response body for use in subsequent steps
			for _, cap := range step.Capture {
				if val, found := resolvePath(responseBody, cap.Path); found {
					captures[cap.Name] = fmt.Sprintf("%v", val)
					log.Printf("[plot-test] Step %d captured %s = %v", step.StepNumber, cap.Name, val)
				}
			}

			// Evaluate semantic assertions on response body
			var assertionResults []AssertionResult
			assertionsPassed, assertionsFailed := 0, 0
			for _, assertion := range step.Assertions {
				ar := evaluateAssertion(assertion, responseBody)
				assertionResults = append(assertionResults, ar)
				if ar.Passed {
					assertionsPassed++
				} else {
					assertionsFailed++
				}
			}
			stepResult.AssertionResults = assertionResults
			stepResult.AssertionsPassed = assertionsPassed
			stepResult.AssertionsFailed = assertionsFailed

			// Brief delay to allow observability events to propagate
			time.Sleep(500 * time.Millisecond)

			// Verify call chain
			chainPassed, matched, unmatched := verifyChain(applicationID, stepStart, step)
			stepResult.ChainPassed = chainPassed
			stepResult.ChainMatched = matched
			stepResult.ChainUnmatched = unmatched

			assertionsPassed2 := assertionsFailed == 0
			stepResult.Passed = statusPassed && chainPassed && assertionsPassed2
			if !statusPassed {
				if len(step.ExpectedStatuses) > 0 {
					stepResult.FailureReason = fmt.Sprintf("Expected one of %v, got %d",
						step.ExpectedStatuses, actualStatus)
				} else {
					stepResult.FailureReason = fmt.Sprintf("Expected status %d, got %d",
						step.ExpectedStatus, actualStatus)
				}
			} else if !assertionsPassed2 {
				stepResult.FailureReason = fmt.Sprintf("%d assertion(s) failed", assertionsFailed)
			} else if !chainPassed {
				stepResult.FailureReason = fmt.Sprintf("%d expected call(s) not observed in event stream",
					len(unmatched))
			}
		}

		if stepResult.Passed {
			run.PassedSteps++
		} else {
			run.FailedSteps++
		}
		run.Steps = append(run.Steps, stepResult)

		// Write every step result to bad-whiff buffer — tier 2 storage.
		// AI-lien needs baseline data, not just failure data.
		writeStepWhiff(applicationID, runID, step, stepResult)

		log.Printf("[plot-test] Step %d/%d (%s): passed=%v status=%d assertions=%d/%d chain=%v latency=%dms",
			step.StepNumber, run.TotalSteps, step.Description,
			stepResult.Passed, actualStatus,
			stepResult.AssertionsPassed, stepResult.AssertionsPassed+stepResult.AssertionsFailed,
			stepResult.ChainPassed, latency)
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

	// Escalate to Interactions — always, on any failure.
	// Interactions decides the path (immediate/AI/silence) based on failure rate and Lore history.
	if run.FailedSteps > 0 {
		go func() {
			postJSON(interactionsURL+"/escalate", map[string]interface{}{
				"run_id":         runID,
				"application_id": applicationID,
				"test_tier":      "plot",
				"total_tests":    run.TotalSteps,
				"failed_tests":   run.FailedSteps,
				"passed_tests":   run.PassedSteps,
				"skipped_tests":  0,
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
		req.ApplicationID = "seti"
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
	return buildServerTLS(certMat)
}

var certMat *CertMaterial

func main() {
	certMat = obtainCerts("plot-test")
	go selfRegisterWithAC(certMat, "https://plot-test:4004")
	buildUpstreamClient()
	connectRedis()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/run", handleRun)
	mux.HandleFunc("/run/", handleRunStatus)

	server := &http.Server{Addr: ":" + port, Handler: mux, TLSConfig: loadServerTLS()}

	log.Printf("[plot-test] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[plot-test] Plot Store: %s | Signal Aggregator: %s | Interactions: %s",
		plotStoreURL, signalAggURL, interactionsURL)

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[plot-test] Server error: %v", err)
		}
	}()
	awaitShutdown(server)
}

// ---------------------------------------------------------------------------
// Scheduler — polls Policy for applications with plot tests enabled and
// runs them on the configured cron schedule (simplified: daily at 02:00 UTC).
// ---------------------------------------------------------------------------

var (
	plotScheduledApps   = map[string]*plotScheduledApp{}
	plotScheduledAppsMu sync.Mutex
)

type plotScheduledApp struct {
	applicationID string
	stop          chan struct{}
}

func startPlotScheduler() {
	log.Printf("[plot-test] Scheduler starting — polling policy every 60s for applications with plot tests enabled")
	go func() {
		syncPlotSchedules()
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			syncPlotSchedules()
		}
	}()
}

func syncPlotSchedules() {
	apps, err := fetchPlotEnabledApps()
	if err != nil {
		log.Printf("[plot-test] Scheduler: could not fetch applications: %v", err)
		return
	}

	plotScheduledAppsMu.Lock()
	defer plotScheduledAppsMu.Unlock()

	for _, appID := range apps {
		if _, ok := plotScheduledApps[appID]; ok {
			continue // already scheduled
		}

		sa := &plotScheduledApp{
			applicationID: appID,
			stop:          make(chan struct{}),
		}
		plotScheduledApps[appID] = sa

		go func(s *plotScheduledApp) {
			log.Printf("[plot-test] Scheduler: watching %s — runs daily at 02:00 UTC", s.applicationID)
			for {
				now := time.Now().UTC()
				// Next 02:00 UTC
				next := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, time.UTC)
				if now.After(next) {
					next = next.Add(24 * time.Hour)
				}
				timer := time.NewTimer(next.Sub(now))
				select {
				case <-timer.C:
					log.Printf("[plot-test] Scheduler: triggering daily run for %s", s.applicationID)
					triggerPlotRunForApp(s.applicationID)
				case <-s.stop:
					timer.Stop()
					return
				}
			}
		}(sa)
	}

	// Stop removed applications
	for id, sa := range plotScheduledApps {
		found := false
		for _, appID := range apps {
			if appID == id {
				found = true
				break
			}
		}
		if !found {
			close(sa.stop)
			delete(plotScheduledApps, id)
			log.Printf("[plot-test] Scheduler: stopped watching %s", id)
		}
	}
}

func fetchPlotEnabledApps() ([]string, error) {
	var result struct {
		Applications []struct {
			ApplicationID string `json:"application_id"`
			Status        string `json:"status"`
			TestSchedule  *struct {
				PlotTestEnabled bool `json:"plot_test_enabled"`
				Enabled         bool `json:"enabled"`
			} `json:"test_schedule"`
		} `json:"applications"`
	}
	if _, err := getJSON(policyURL+"/applications", &result); err != nil {
		return nil, err
	}
	var apps []string
	for _, a := range result.Applications {
		if a.Status != "active" {
			continue
		}
		if a.TestSchedule != nil && a.TestSchedule.PlotTestEnabled && a.TestSchedule.Enabled {
			apps = append(apps, a.ApplicationID)
		}
	}
	return apps, nil
}

func triggerPlotRunForApp(applicationID string) {
	// Fetch all plots for this application from plot-store and run each
	plotStoreURL := envOr("PLOT_STORE_URL", "https://plot-store:4005")
	var result struct {
		Plots []struct {
			PlotID string `json:"plot_id"`
		} `json:"plots"`
	}
	if _, err := getJSON(fmt.Sprintf("%s/plots?application_id=%s", plotStoreURL, applicationID), &result); err != nil {
		log.Printf("[plot-test] Scheduler: could not fetch plots for %s: %v", applicationID, err)
		return
	}
	if len(result.Plots) == 0 {
		log.Printf("[plot-test] Scheduler: no plots found for %s — skipping run", applicationID)
		return
	}
	log.Printf("[plot-test] Scheduler: running %d plots for %s", len(result.Plots), applicationID)
	for _, p := range result.Plots {
		go executeRun(p.PlotID, applicationID)
	}
}
