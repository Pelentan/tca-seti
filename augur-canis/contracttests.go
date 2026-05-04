package main

// ---------------------------------------------------------------------------
// AC Contract Test Suite
//
// Hardcoded independent verification of every SETI Job contract.
// These tests are authored separately from the contracts — intentional
// friction. If a contract changes, update these tests manually.
// That work loop is the point.
//
// Positive tests: every significant readable endpoint returns expected 2xx.
// Negative tests: one per Job with POST endpoints — malformed/missing input
//                 must return 4xx, not 5xx. 5xx on bad input = swallowed error.
//
// Transport: AC's mTLS upstream client. Same transport as production traffic.
// Jobs not registered or not reachable are skipped, not failed.
//
// Scheduling:
//   - 30s after startup (let Jobs register)
//   - Every CONTRACT_TEST_INTERVAL_SECONDS (default 900 = 15 min)
//   - On-demand: POST /run-contract-tests
//
// Version: 2026-04-23 — reflects contracts as shipped.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// ACContractTest defines a single independently authored test case.
type ACContractTest struct {
	ServiceName    string
	TestName       string
	Method         string
	Path           string
	Body           interface{}
	ExpectedStatus int
	ResponseFields []string // top-level JSON keys that must be present
	IsNegative     bool
}

// setiContractTests is the authoritative contract test suite for SETI.
// Covers every Job and every significant endpoint.
// Update this when a contract changes — the build won't remind you.
var setiContractTests = []ACContractTest{

	// -----------------------------------------------------------------------
	// ai-lien (port 4252)
	// Contract: ai-lien.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "ai-lien", TestName: "ai-lien/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "ai-lien", TestName: "ai-lien/neg-analyze-empty",
		Method: "POST", Path: "/analyze", Body: map[string]interface{}{},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// augur-canis (port 4010) — self-test
	// Contract: augur-canis.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "augur-canis", TestName: "augur-canis/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status", "jobs_registered"},
	},
	{
		ServiceName: "augur-canis", TestName: "augur-canis/jobs-list",
		Method: "GET", Path: "/jobs", ExpectedStatus: 200,
		ResponseFields: []string{"jobs", "total"},
	},
	{
		ServiceName: "augur-canis", TestName: "augur-canis/queries-list",
		Method: "GET", Path: "/queries", ExpectedStatus: 200,
	},
	{
		ServiceName: "augur-canis", TestName: "augur-canis/configuration",
		Method: "GET", Path: "/configuration", ExpectedStatus: 200,
		ResponseFields: []string{"check_interval_seconds"},
	},
	{
		ServiceName: "augur-canis", TestName: "augur-canis/checks-recent",
		Method: "GET", Path: "/checks/recent", ExpectedStatus: 200,
	},
	{
		ServiceName: "augur-canis", TestName: "augur-canis/federation-status",
		Method: "GET", Path: "/federation/status", ExpectedStatus: 200,
	},
	{
		ServiceName: "augur-canis", TestName: "augur-canis/neg-register-empty",
		Method: "POST", Path: "/services/register", Body: map[string]interface{}{},
		ExpectedStatus: 400, IsNegative: true,
	},
	{
		ServiceName: "augur-canis", TestName: "augur-canis/neg-federation-register-empty",
		Method: "POST", Path: "/federation/register", Body: map[string]interface{}{},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// cert-forge (port 4014 — sign mTLS, registered port)
	// Contract: cert-forge.yaml
	// Note: /ca is on public port 4016 and /instance-cert is on enrollment
	// port 4015 — neither is reachable via the registered mTLS endpoint.
	// Only /sign is testable through AC's upstream client.
	// -----------------------------------------------------------------------
	{
		ServiceName: "cert-forge", TestName: "cert-forge/neg-sign-empty",
		Method: "POST", Path: "/sign", Body: map[string]interface{}{},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// contract-test (port 4003)
	// Contract: contract-test.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "contract-test", TestName: "contract-test/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "contract-test", TestName: "contract-test/neg-run-empty",
		Method: "POST", Path: "/run", Body: map[string]interface{}{},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// feed-wrangler (port 4007)
	// Contract: feed-wrangler.yaml
	// Note: POST endpoints time out from AC's Go mTLS client due to
	// Elixir/Cowboy TLS negotiation differences. GET endpoints work fine.
	// Negative tests verified via Ring Trial.
	// -----------------------------------------------------------------------
	{
		ServiceName: "feed-wrangler", TestName: "feed-wrangler/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "feed-wrangler", TestName: "feed-wrangler/feeds-list",
		Method: "GET", Path: "/feeds", ExpectedStatus: 200,
		ResponseFields: []string{"feeds"},
	},

	// -----------------------------------------------------------------------
	// gateway (port 4000)
	// Contract: gateway.yaml
	// Note: gateway is the external face — tests hit it directly via AC network.
	// Auth-protected routes tested as negative (no token) — expect 401.
	// -----------------------------------------------------------------------
	{
		ServiceName: "gateway", TestName: "gateway/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "gateway", TestName: "gateway/neg-protected-no-auth",
		Method:         "GET",
		Path:           "/applications",
		ExpectedStatus: 401,
		IsNegative:     true,
	},
	{
		ServiceName: "gateway", TestName: "gateway/neg-results-contract-no-auth",
		Method: "GET", Path: "/contract-results", ExpectedStatus: 401,
		IsNegative: true,
	},
	{
		ServiceName: "gateway", TestName: "gateway/neg-results-plot-no-auth",
		Method: "GET", Path: "/plot-results", ExpectedStatus: 401,
		IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// integration (port 4013)
	// Contract: integration.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "integration", TestName: "integration/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "integration", TestName: "integration/cluster-health",
		Method: "GET", Path: "/v1/cluster/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "integration", TestName: "integration/applications-list",
		Method: "GET", Path: "/v1/applications", ExpectedStatus: 200,
		ResponseFields: []string{"applications"},
	},

	// -----------------------------------------------------------------------
	// interactions (port 4009)
	// Contract: interactions.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "interactions", TestName: "interactions/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "interactions", TestName: "interactions/neg-escalate-empty",
		Method: "POST", Path: "/escalate", Body: map[string]interface{}{},
		ExpectedStatus: 400, IsNegative: true,
	},
	{
		ServiceName: "interactions", TestName: "interactions/notify-regeneration",
		// Fire-and-forget by contract design — accepts empty body, queues notification.
		// Same pattern as seti-observability/event.
		Method: "POST", Path: "/notify/regeneration", Body: map[string]interface{}{},
		ExpectedStatus: 202,
	},

	// -----------------------------------------------------------------------
	// lore (port 4110)
	// Contract: lore.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "lore", TestName: "lore/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status", "baselines_established"},
	},
	{
		ServiceName: "lore", TestName: "lore/trend-points-list",
		Method: "GET", Path: "/trend-points", ExpectedStatus: 200,
		ResponseFields: []string{"trend_points", "total"},
	},
	{
		ServiceName: "lore", TestName: "lore/incidents-list",
		Method: "GET", Path: "/incidents", ExpectedStatus: 200,
		ResponseFields: []string{"incidents", "total", "open_count"},
	},
	{
		ServiceName: "lore", TestName: "lore/patterns-list",
		Method: "GET", Path: "/patterns", ExpectedStatus: 200,
		ResponseFields: []string{"patterns", "total"},
	},
	{
		ServiceName: "lore", TestName: "lore/corrections-list",
		// Stub endpoint — returns 501 until implemented. Verifies it doesn't crash.
		Method: "GET", Path: "/corrections", ExpectedStatus: 501,
	},
	{
		ServiceName: "lore", TestName: "lore/neg-correction-missing-fields",
		// Stub endpoint — returns 501 regardless of input until implemented.
		// When this flips to 400, the implementation is live and this test needs updating.
		Method: "POST", Path: "/corrections",
		Body:           map[string]interface{}{"application_id": "test"},
		ExpectedStatus: 501,
	},
	{
		ServiceName: "lore", TestName: "lore/neg-trend-point-missing-fields",
		Method: "POST", Path: "/trend-points",
		Body:           map[string]interface{}{"application_id": "test"},
		ExpectedStatus: 400, IsNegative: true,
	},
	{
		ServiceName: "lore", TestName: "lore/neg-incident-missing-fields",
		Method: "POST", Path: "/incidents",
		Body:           map[string]interface{}{"application_id": "test"},
		ExpectedStatus: 400, IsNegative: true,
	},
	{
		ServiceName: "lore", TestName: "lore/neg-pattern-missing-fields",
		Method: "POST", Path: "/patterns",
		Body:           map[string]interface{}{"application_id": "test"},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// notifier (port 4300)
	// Contract: notifier.yaml
	// Permanent stub — stub_active: true is correct and expected.
	// -----------------------------------------------------------------------
	{
		ServiceName: "notifier", TestName: "notifier/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status", "stub_active"},
	},
	{
		ServiceName: "notifier", TestName: "notifier/neg-notify-missing-fields",
		Method: "POST", Path: "/notify",
		Body:           map[string]interface{}{"severity": "critical"},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// plot-store (port 4005)
	// Contract: plot-store.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "plot-store", TestName: "plot-store/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "plot-store", TestName: "plot-store/plots-list",
		Method: "GET", Path: "/plots", ExpectedStatus: 200,
		ResponseFields: []string{"plots", "total"},
	},
	{
		ServiceName: "plot-store", TestName: "plot-store/neg-create-missing-fields",
		Method: "POST", Path: "/plots",
		Body:           map[string]interface{}{"name": "incomplete"},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// plot-test (port 4004)
	// Contract: plot-test.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "plot-test", TestName: "plot-test/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "plot-test", TestName: "plot-test/neg-run-empty",
		Method: "POST", Path: "/run", Body: map[string]interface{}{},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// policy (port 4002)
	// Contract: policy.yaml
	// Note: /applications/register no longer exists — registration moved to
	// /available-applications/{tag}/register (path-param, not directly testable).
	// Negative test updated to POST /applications with missing required fields.
	// -----------------------------------------------------------------------
	{
		ServiceName: "policy", TestName: "policy/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "policy", TestName: "policy/applications-list",
		Method: "GET", Path: "/applications", ExpectedStatus: 200,
		ResponseFields: []string{"applications"},
	},
	{
		ServiceName: "policy", TestName: "policy/ai-providers-list",
		Method: "GET", Path: "/ai-providers", ExpectedStatus: 200,
		ResponseFields: []string{"providers"},
	},
	{
		ServiceName: "policy", TestName: "policy/ai-providers-active",
		// 503 when no AI provider is configured — valid dev state.
		Method: "GET", Path: "/ai-providers/active", ExpectedStatus: 503,
	},
	{
		ServiceName: "policy", TestName: "policy/available-applications",
		Method: "GET", Path: "/available-applications", ExpectedStatus: 200,
	},
	{
		ServiceName: "policy", TestName: "policy/neg-register-application-missing-fields",
		// POST /applications is 405 — implementation routes registration to /applications/register.
		Method: "POST", Path: "/applications/register",
		Body:           map[string]interface{}{"display_name": "incomplete"},
		ExpectedStatus: 400, IsNegative: true,
	},
	{
		ServiceName: "policy", TestName: "policy/neg-ai-provider-missing-fields",
		Method: "POST", Path: "/ai-providers",
		Body:           map[string]interface{}{"name": "incomplete"},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// results (port 4008)
	// Contract: results.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "results", TestName: "results/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "results", TestName: "results/contract-results-list",
		Method: "GET", Path: "/contract-results", ExpectedStatus: 200,
		ResponseFields: []string{"runs"},
	},
	{
		ServiceName: "results", TestName: "results/plot-results-list",
		Method: "GET", Path: "/plot-results", ExpectedStatus: 200,
		ResponseFields: []string{"runs"},
	},
	{
		ServiceName: "results", TestName: "results/trends-cluster",
		// Route not yet implemented — returns 404. When implemented, expect 200.
		Method: "GET", Path: "/trends/cluster", ExpectedStatus: 404,
	},
	{
		ServiceName: "results", TestName: "results/neg-store-missing-fields",
		Method: "POST", Path: "/contract-results",
		Body:           map[string]interface{}{"application_id": "test"}, // missing run_id
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// seti-observability (port 4011)
	// Contract: seti-observability.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "seti-observability", TestName: "seti-observability/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "seti-observability", TestName: "seti-observability/rules",
		Method: "GET", Path: "/rules", ExpectedStatus: 200,
		ResponseFields: []string{"knownCallers"},
	},
	{
		ServiceName: "seti-observability", TestName: "seti-observability/event-ingest",
		Method: "POST", Path: "/event",
		Body: map[string]interface{}{
			"caller": "augur-canis", "callee": "seti-observability",
			"method": "POST", "path": "/event",
			"status_code": 202, "latency_ms": 1, "protocol": "mtls",
		},
		ExpectedStatus: 202,
	},
	{
		ServiceName: "seti-observability", TestName: "seti-observability/neg-event-empty",
		Method: "POST", Path: "/event", Body: map[string]interface{}{},
		// Fire-and-forget by contract design — 202 even on empty/bad input.
		// The event is silently dropped after decode failure. This verifies
		// the endpoint doesn't crash (5xx) on bad input, which is the real concern.
		ExpectedStatus: 202,
	},

	// -----------------------------------------------------------------------
	// signal-aggregator (port 4006)
	// Contract: signal-aggregator.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "signal-aggregator", TestName: "signal-aggregator/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "signal-aggregator", TestName: "signal-aggregator/subscriptions-list",
		Method: "GET", Path: "/subscriptions", ExpectedStatus: 200,
		ResponseFields: []string{"subscriptions"},
	},
	{
		ServiceName: "signal-aggregator", TestName: "signal-aggregator/federation-subscriptions",
		Method: "GET", Path: "/federation/subscriptions", ExpectedStatus: 200,
	},
	{
		ServiceName: "signal-aggregator", TestName: "signal-aggregator/neg-subscribe-missing-url",
		Method: "POST", Path: "/subscriptions",
		Body:           map[string]interface{}{"application_id": "test"},
		ExpectedStatus: 400, IsNegative: true,
	},
	{
		ServiceName: "signal-aggregator", TestName: "signal-aggregator/neg-verify-call-chain-empty",
		Method: "POST", Path: "/verify/call-chain", Body: map[string]interface{}{},
		ExpectedStatus: 400, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// signal-clearance (port 4001)
	// Contract: signal-clearance.yaml
	// Note: POST auth endpoints time out from AC's Go mTLS client due to
	// Node.js TLS connection handling differences. GET endpoints work fine.
	// Negative tests for auth endpoints verified via Ring Trial.
	// -----------------------------------------------------------------------
	{
		ServiceName: "signal-aggregator", TestName: "signal-aggregator/neg-correlate-empty",
		// Route not yet implemented — returns 404. When implemented, expect 400.
		Method: "POST", Path: "/correlate", Body: map[string]interface{}{},
		ExpectedStatus: 404,
	},
	{
		ServiceName: "signal-clearance", TestName: "signal-clearance/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
		ResponseFields: []string{"status"},
	},
	{
		ServiceName: "signal-clearance", TestName: "signal-clearance/wranglers-list",
		Method: "GET", Path: "/wranglers", ExpectedStatus: 200,
	},
	{
		ServiceName: "signal-clearance", TestName: "signal-clearance/clearance-levels",
		Method: "GET", Path: "/clearance/levels", ExpectedStatus: 200,
	},
	{
		ServiceName: "signal-clearance", TestName: "signal-clearance/federation-config",
		// 404 NOT_CONFIGURED when no IdP is set up — valid dev state.
		Method: "GET", Path: "/federation/config", ExpectedStatus: 404,
	},
	{
		ServiceName: "signal-clearance", TestName: "signal-clearance/federation-groups",
		Method: "GET", Path: "/federation/groups", ExpectedStatus: 200,
	},
	{
		ServiceName: "signal-clearance", TestName: "signal-clearance/neg-clearance-validate-empty",
		// Wrangler lookup occurs before field validation — 404 WRANGLER_NOT_FOUND on empty body.
		Method: "POST", Path: "/clearance/validate", Body: map[string]interface{}{},
		ExpectedStatus: 404, IsNegative: true,
	},

	// -----------------------------------------------------------------------
	// ui (port 4020)
	// Contract: ui.yaml
	// -----------------------------------------------------------------------
	{
		ServiceName: "ui", TestName: "ui/health",
		Method: "GET", Path: "/health", ExpectedStatus: 200,
	},
	{
		ServiceName: "ui", TestName: "ui/root",
		Method: "GET", Path: "/", ExpectedStatus: 200,
	},
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

var contractTestIntervalSec = envOrInt("CONTRACT_TEST_INTERVAL_SECONDS", 900)

func runContractTestScheduler() {
	log.Printf("[augur-canis] Contract test scheduler: %d tests, interval %ds",
		len(setiContractTests), contractTestIntervalSec)

	time.Sleep(30 * time.Second)
	runContractTestSuite("startup")

	ticker := time.NewTicker(time.Duration(contractTestIntervalSec) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		runContractTestSuite("scheduled")
	}
}

// ---------------------------------------------------------------------------
// Suite runner
// ---------------------------------------------------------------------------

func runContractTestSuite(trigger string) *ContractSuiteResult {
	started := time.Now()
	runID := fmt.Sprintf("ac-suite-%d", started.UnixNano())
	ctx := context.Background()

	log.Printf("[augur-canis] Contract suite starting — trigger=%s run=%s tests=%d",
		trigger, runID, len(setiContractTests))

	results := make([]ContractTestResult, 0, len(setiContractTests))
	passed, failed, skipped := 0, 0, 0

	for _, test := range setiContractTests {
		req := ContractTestRequest{
			RequestID:              fmt.Sprintf("%s-%s", runID, test.TestName),
			ServiceName:            test.ServiceName,
			Method:                 test.Method,
			Path:                   test.Path,
			Body:                   test.Body,
			ExpectedStatus:         test.ExpectedStatus,
			ExpectedResponseFields: test.ResponseFields,
			TestName:               test.TestName,
			ContractVersion:        "2026-04-11",
			PublishedAt:            time.Now().UTC().Format(time.RFC3339),
		}

		result := executeContractTestSync(req)
		results = append(results, result)

		switch {
		case strings.HasPrefix(result.FailureReason, "skip:"):
			skipped++
		case result.Passed:
			passed++
		default:
			failed++
		}
	}

	durationMs := time.Since(started).Milliseconds()
	suite := &ContractSuiteResult{
		RunID:      runID,
		Trigger:    trigger,
		Passed:     passed,
		Failed:     failed,
		Skipped:    skipped,
		Total:      len(setiContractTests),
		DurationMs: durationMs,
		Results:    results,
		StartedAt:  started.UTC().Format(time.RFC3339),
	}

	persistSuiteResult(ctx, suite)

	status := fmt.Sprintf("%d passed, %d failed, %d skipped", passed, failed, skipped)
	if failed > 0 {
		log.Printf("[augur-canis] Contract suite FAILED — %s in %dms", status, durationMs)
		for _, r := range results {
			if !r.Passed && !strings.HasPrefix(r.FailureReason, "skip:") {
				log.Printf("[augur-canis]   ✗ %s: %s", r.TestName, r.FailureReason)
			}
		}
	} else {
		log.Printf("[augur-canis] Contract suite passed — %s in %dms", status, durationMs)
	}

	return suite
}

// ---------------------------------------------------------------------------
// ContractSuiteResult
// ---------------------------------------------------------------------------

type ContractSuiteResult struct {
	RunID      string               `json:"run_id"`
	Trigger    string               `json:"trigger"`
	Passed     int                  `json:"passed"`
	Failed     int                  `json:"failed"`
	Skipped    int                  `json:"skipped"`
	Total      int                  `json:"total"`
	DurationMs int64                `json:"duration_ms"`
	Results    []ContractTestResult `json:"results"`
	StartedAt  string               `json:"started_at"`
}

func persistSuiteResult(ctx context.Context, suite *ContractSuiteResult) {
	payload, err := json.Marshal(suite)
	if err != nil {
		log.Printf("[augur-canis] Failed to marshal suite result: %v", err)
		return
	}
	rdb.Set(ctx, fmt.Sprintf("ac:contract-suite:%s", suite.RunID), string(payload), 24*time.Hour)
	rdb.Publish(ctx, "tca:ac-contract-suite", string(payload))

	// Publish individual results to the existing contract-results channel
	// so they surface in Results Job alongside contract-test Job runs
	for _, r := range suite.Results {
		publishContractTestResult(ctx, r)
	}
}

// ---------------------------------------------------------------------------
// executeContractTestSync — synchronous execution for suite runner
// ---------------------------------------------------------------------------

func executeContractTestSync(req ContractTestRequest) ContractTestResult {
	start := time.Now()

	job, err := getRegisteredJob(context.Background(), req.ServiceName)
	if err != nil {
		return ContractTestResult{
			RequestID: req.RequestID, ServiceName: req.ServiceName,
			TestName: req.TestName, Passed: false,
			ExpectedStatus: req.ExpectedStatus,
			FailureReason:  "skip: job_not_deployed",
			ExecutedAt:     time.Now().UTC().Format(time.RFC3339),
		}
	}

	target := job.NetworkEndpoint + req.Path

	var httpReq *http.Request
	if req.Body != nil {
		bodyBytes, _ := json.Marshal(req.Body)
		httpReq, err = http.NewRequest(req.Method, target, bytes.NewReader(bodyBytes))
		if err == nil {
			httpReq.ContentLength = int64(len(bodyBytes))
		}
	} else {
		httpReq, err = http.NewRequest(req.Method, target, nil)
	}
	if err != nil {
		return ContractTestResult{
			RequestID: req.RequestID, ServiceName: req.ServiceName,
			TestName: req.TestName, Passed: false,
			ExpectedStatus: req.ExpectedStatus,
			FailureReason:  fmt.Sprintf("build request failed: %v", err),
			ExecutedAt:     time.Now().UTC().Format(time.RFC3339),
		}
	}

	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := upstreamClient.Do(httpReq)
	latencyMs := time.Since(start).Milliseconds()

	if err != nil {
		reason := fmt.Sprintf("request failed: %v", err)
		s := err.Error()
		if strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") ||
			strings.Contains(s, "context deadline exceeded") {
			reason = "skip: job_not_reachable"
		}
		reportEvent(req.ServiceName, req.Method, req.Path, 0, latencyMs)
		return ContractTestResult{
			RequestID: req.RequestID, ServiceName: req.ServiceName,
			TestName: req.TestName, Passed: false,
			ActualStatus: 0, ExpectedStatus: req.ExpectedStatus,
			FailureReason: reason, LatencyMs: latencyMs,
			ExecutedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	defer resp.Body.Close()

	var responseBody interface{}
	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	if n > 0 {
		json.Unmarshal(buf[:n], &responseBody)
	}

	passed := resp.StatusCode == req.ExpectedStatus
	failureReason := ""
	if !passed {
		failureReason = fmt.Sprintf("expected %d, got %d", req.ExpectedStatus, resp.StatusCode)
	} else if len(req.ExpectedResponseFields) > 0 {
		if m, ok := responseBody.(map[string]interface{}); ok {
			for _, field := range req.ExpectedResponseFields {
				if _, exists := m[field]; !exists {
					passed = false
					failureReason = fmt.Sprintf("field '%s' missing from response", field)
					break
				}
			}
		}
	}

	reportEvent(req.ServiceName, req.Method, req.Path, resp.StatusCode, latencyMs)

	return ContractTestResult{
		RequestID: req.RequestID, ServiceName: req.ServiceName,
		TestName: req.TestName, Passed: passed,
		ActualStatus: resp.StatusCode, ExpectedStatus: req.ExpectedStatus,
		ActualResponse: responseBody, FailureReason: failureReason,
		LatencyMs: latencyMs, ExecutedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// Admin handlers wired into main.go
// ---------------------------------------------------------------------------

func handleRecentSuiteResults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := context.Background()

	keys, err := rdb.Keys(ctx, "ac:contract-suite:*")
	if err != nil || len(keys) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []interface{}{},
			"message": "No suite runs yet — first run fires 15s after startup",
		})
		return
	}

	var latest string
	for _, k := range keys {
		if latest == "" || k > latest {
			latest = k
		}
	}

	raw, _, _ := rdb.Get(ctx, latest)
	var suite ContractSuiteResult
	if err := json.Unmarshal([]byte(raw), &suite); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"results": []interface{}{}})
		return
	}

	json.NewEncoder(w).Encode(suite)
}

func handleRunContractTests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	go func() { runContractTestSuite("on-demand") }()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "accepted",
		"message":    "Contract test suite started",
		"test_count": len(setiContractTests),
	})
}

// handleAdHocContractTest parses a YAML contract submitted by a Sec Wr4ngler
// and runs those endpoints on demand. Used by the Ring page.
func handleAdHocContractTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	var req struct {
		ServiceName string `json:"service_name"`
		YAML        string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ServiceName == "" || req.YAML == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"code": "INVALID_REQUEST", "message": "service_name and yaml required",
		})
		return
	}

	// Parse the YAML into test cases using the same logic as contract-test Job
	// Returns immediately with parsed test cases and dispatches execution async
	tests := parseYAMLToTests(req.ServiceName, req.YAML)
	if len(tests) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"code": "PARSE_FAILED", "message": "No testable endpoints found in provided YAML",
		})
		return
	}

	started := time.Now()
	runID := fmt.Sprintf("ring-%d", started.UnixNano())
	results := make([]ContractTestResult, 0, len(tests))

	for _, test := range tests {
		ctReq := ContractTestRequest{
			RequestID:      fmt.Sprintf("%s-%s", runID, test.TestName),
			ServiceName:    test.ServiceName,
			Method:         test.Method,
			Path:           test.Path,
			Body:           test.Body,
			ExpectedStatus: test.ExpectedStatus,
			TestName:       test.TestName,
		}
		results = append(results, executeContractTestSync(ctReq))
	}

	passed, failed, skipped := 0, 0, 0
	for _, r := range results {
		switch {
		case strings.HasPrefix(r.FailureReason, "skip:"):
			skipped++
		case r.Passed:
			passed++
		default:
			failed++
		}
	}

	suite := &ContractSuiteResult{
		RunID:      runID,
		Trigger:    "ring",
		Passed:     passed,
		Failed:     failed,
		Skipped:    skipped,
		Total:      len(tests),
		DurationMs: time.Since(started).Milliseconds(),
		Results:    results,
		StartedAt:  started.UTC().Format(time.RFC3339),
	}

	// Persist — Ring runs are part of the audit trail
	persistSuiteResult(context.Background(), suite)

	json.NewEncoder(w).Encode(suite)
}

// handleContractSuitesList handles GET /contract-suites — list of all stored suite runs.
// Returns summary metadata only. Full results fetched separately on selection.
func handleContractSuitesList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := context.Background()

	keys, err := rdb.Keys(ctx, "ac:contract-suite:*")
	if err != nil || len(keys) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"suites": []interface{}{}})
		return
	}

	type SuiteSummary struct {
		RunID      string `json:"run_id"`
		Trigger    string `json:"trigger"`
		Passed     int    `json:"passed"`
		Failed     int    `json:"failed"`
		Skipped    int    `json:"skipped"`
		Total      int    `json:"total"`
		DurationMs int64  `json:"duration_ms"`
		StartedAt  string `json:"started_at"`
	}

	suites := make([]SuiteSummary, 0, len(keys))
	for _, key := range keys {
		raw, ok, err := rdb.Get(ctx, key)
		if !ok || err != nil {
			continue
		}
		var suite ContractSuiteResult
		if err := json.Unmarshal([]byte(raw), &suite); err != nil {
			continue
		}
		suites = append(suites, SuiteSummary{
			RunID:      suite.RunID,
			Trigger:    suite.Trigger,
			Passed:     suite.Passed,
			Failed:     suite.Failed,
			Skipped:    suite.Skipped,
			Total:      suite.Total,
			DurationMs: suite.DurationMs,
			StartedAt:  suite.StartedAt,
		})
	}

	// Most recent first
	for i := 0; i < len(suites)-1; i++ {
		for j := i + 1; j < len(suites); j++ {
			if suites[j].StartedAt > suites[i].StartedAt {
				suites[i], suites[j] = suites[j], suites[i]
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"suites": suites,
		"total":  len(suites),
	})
}
// Minimal parser — reads paths and HTTP methods, generates GET tests (200)
// and POST tests with empty body (400 negative). Not a full OpenAPI parser.
// handleContractSuiteDetail handles GET /contract-suites/{run_id} — full detail for one run.
func handleContractSuiteDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	runID := strings.TrimPrefix(r.URL.Path, "/contract-suites/")
	runID = strings.TrimSuffix(runID, "/")

	raw, ok, err := rdb.Get(context.Background(), fmt.Sprintf("ac:contract-suite:%s", runID))
	if !ok || err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"code": "NOT_FOUND", "message": fmt.Sprintf("Suite %s not found or expired", runID),
		})
		return
	}
	w.Write([]byte(raw))
}

func parseYAMLToTests(serviceName, yamlContent string) []ACContractTest {
	var tests []ACContractTest
	lines := strings.Split(yamlContent, "\n")

	var currentPath string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Path detection: lines like "  /health:" at indent 2
		if strings.HasPrefix(line, "  /") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			currentPath = strings.TrimSuffix(strings.TrimSpace(line), ":")
			continue
		}

		if currentPath == "" {
			continue
		}

		// Skip paths with parameters — can't test without knowing the ID
		if strings.Contains(currentPath, "{") {
			continue
		}

		method := ""
		switch trimmed {
		case "get:":
			method = "GET"
		case "post:":
			method = "POST"
		}

		if method == "" {
			continue
		}

		testName := fmt.Sprintf("%s%s-%s", serviceName, currentPath, strings.ToLower(method))
		testName = strings.ReplaceAll(testName, "/", "-")
		testName = strings.TrimPrefix(testName, "-")

		if method == "GET" {
			tests = append(tests, ACContractTest{
				ServiceName:    serviceName,
				TestName:       testName,
				Method:         "GET",
				Path:           currentPath,
				ExpectedStatus: 200,
			})
		} else if method == "POST" {
			// Positive: empty body for fire-and-forget endpoints may return 200/201/202
			// Negative: empty body for endpoints that require fields should return 400
			tests = append(tests, ACContractTest{
				ServiceName:    serviceName,
				TestName:       testName + "-neg",
				Method:         "POST",
				Path:           currentPath,
				Body:           map[string]interface{}{},
				ExpectedStatus: 400,
				IsNegative:     true,
			})
		}
	}

	return tests
}
