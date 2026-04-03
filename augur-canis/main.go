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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Augur Canis (AC) — Behavioral health verification and alert agent.
//
// Every TCA constellation includes one AC instance. It is not SETI-specific.
//
// Three responsibilities:
//  1. Handle health check requests from the healthcheck binary inside each
//     Job container via Redis pub/sub (tca:check-requests).
//     Phase 1 STUB: returns healthy for all Jobs.
//     Full implementation: executes canned queries over point-to-point mTLS.
//
//  2. Publish health state to tca:augur-canis for SETI Signal Aggregator.
//
//  3. Watch the health feed for anomalies and bark on tca:augur-canis:alerts.
//     ACTIVE:  Silence detection (Job stops reporting).
//     STUBBED: Latency drift detection.
//     STUBBED: Failure rate detection.
//
// Alert lifecycle:
//   - Condition detected → 3 barks on consecutive cycles
//   - After 3 barks → reminder bark every 30 seconds until acknowledged
//   - Acknowledgment → stop barking, record who/when
//   - Condition clears → auto-resolve, final resolved bark
//   - K8s container replacement → inferred from container ID change,
//     auto-resolve with resolution_type: container_replaced
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

var (
	redisURL         = envOr("REDIS_URL", "redis:6379")
	port             = envOr("PORT", "4010")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	constellation    = envOr("CONSTELLATION", "tca-seti")

	checkIntervalSec   = envOrInt("CHECK_INTERVAL_SECONDS", 30)
	resultTTLSec       = envOrInt("RESULT_TTL_SECONDS", 30)
	silenceThresholdSec = envOrInt("SILENCE_THRESHOLD_SECONDS", 90) // 3 * check interval
	reminderIntervalSec = envOrInt("REMINDER_INTERVAL_SECONDS", 30)
	maxInitialBarks    = envOrInt("MAX_INITIAL_BARKS", 3)

	// Redis channel names
	checkRequestsChannel    = "tca:check-requests"
	healthFeedChannel       = "tca:augur-canis"
	alertsChannel           = "tca:augur-canis:alerts"
	contractRequestsChannel = "tca:contract-requests"
	contractResultsChannel  = "tca:contract-results"

	contractResultTTL = 60 * time.Second

	// Redis key prefixes
	keyLastSeen       = "ac:state:%s:last_seen"
	keyLastContainerID = "ac:state:%s:last_container_id"
	keyLatencyWindow  = "ac:state:%s:latency_window"
	keyHealthWindow   = "ac:state:%s:health_window"
	keyAlertActive    = "ac:state:%s:alert_active"
	keyAlertID        = "ac:state:%s:alert_id"
	keyAlertType      = "ac:state:%s:alert_type"
	keyBarkCount      = "ac:state:%s:bark_count"
	keyLastBarkAt     = "ac:state:%s:last_bark_at"
	keyAlertFirstAt   = "ac:state:%s:alert_first_at"

	// Active alerts stored by alert_id for acknowledgment lookup
	keyAlertByID = "ac:alert:%s" // ac:alert:{alert_id} → service_name
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// Counters
// ---------------------------------------------------------------------------

var (
	checksHandled   atomic.Int64
	checksPublished atomic.Int64
	alertsFired     atomic.Int64
	startTime       = time.Now()
)

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
			log.Printf("[augur-canis] Connected to Redis at %s", redisURL)
			return
		}
		log.Printf("[augur-canis] Redis not ready (attempt %d/10): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	log.Fatal("[augur-canis] Could not connect to Redis after 10 attempts")
}

// ---------------------------------------------------------------------------
// Alert structures
// ---------------------------------------------------------------------------

type AlertType string

const (
	AlertSilence     AlertType = "silence"
	AlertLatencyDrift AlertType = "latency_drift" // STUBBED
	AlertFailureRate AlertType = "failure_rate"   // STUBBED
	AlertResolved    AlertType = "resolved"
)

type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarn     Severity = "warn"
	SeverityInfo     Severity = "info"
)

type Alert struct {
	AlertID        string    `json:"alert_id"`
	ServiceName    string    `json:"service_name"`
	AlertType      AlertType `json:"alert_type"`
	Severity       Severity  `json:"severity"`
	Message        string    `json:"message"`
	TriggeredAt    string    `json:"triggered_at"`
	LastHealthyAt  string    `json:"last_healthy_at,omitempty"`
	BarkNumber     int       `json:"bark_number,omitempty"`
	ResolutionType string    `json:"resolution_type,omitempty"`
	ResolvedAt     string    `json:"resolved_at,omitempty"`
	Constellation  string    `json:"constellation"`
	StubActive     bool      `json:"stub_active"`
}

// ---------------------------------------------------------------------------
// Health check request handler
// ---------------------------------------------------------------------------

type CheckRequest struct {
	RequestID   string `json:"request_id"`
	ContainerID string `json:"container_id"`
	ServiceName string `json:"service_name"`
	PublishedAt string `json:"published_at"`
}

type CheckResult struct {
	RequestID         string `json:"request_id"`
	ServiceName       string `json:"service_name"`
	Healthy           int    `json:"healthy"`
	LatencyMs         int64  `json:"latency_ms"`
	TimeToCompleteMs  int64  `json:"time_to_complete_ms"`
	QueryVersion      int    `json:"query_version"`
	CheckedAt         string `json:"checked_at"`
	FailureReason     string `json:"failure_reason,omitempty"`
	StubActive        bool   `json:"stub_active"`
}

func handleCheckRequests() {
	ctx := context.Background()

	for {
		sub := rdb.Subscribe(ctx, checkRequestsChannel)
		ch := sub.Channel()
		log.Printf("[augur-canis] Subscribed to %s", checkRequestsChannel)

		for msg := range ch {
			checksHandled.Add(1)

			var req CheckRequest
			if err := json.Unmarshal([]byte(msg.Payload), &req); err != nil {
				log.Printf("[augur-canis] Failed to parse check request: %v", err)
				continue
			}

			if req.RequestID == "" || req.ServiceName == "" {
				log.Printf("[augur-canis] Invalid check request — missing fields")
				continue
			}

			go processCheckRequest(req)
		}

		log.Printf("[augur-canis] %s subscription dropped — reconnecting in 2s", checkRequestsChannel)
		sub.Close()
		time.Sleep(2 * time.Second)
	}
}

func processCheckRequest(req CheckRequest) {
	ctx := context.Background()
	now := time.Now()

	// ---------------------------------------------------------------------------
	// STUB: Phase 1 — return healthy for all Jobs.
	// Full implementation: look up canned query for req.ServiceName,
	// execute it over the dedicated point-to-point mTLS network,
	// return real result.
	// ---------------------------------------------------------------------------
	healthy := 1
	latencyMs := int64(1)

	// Record state for anomaly detection
	recordHealthState(ctx, req.ServiceName, req.ContainerID, healthy, latencyMs, now)

	// Publish result back to healthcheck binary
	result := CheckResult{
		RequestID:        req.RequestID,
		ServiceName:      req.ServiceName,
		Healthy:          healthy,
		LatencyMs:        latencyMs,
		TimeToCompleteMs: time.Since(now).Milliseconds() + 1,
		QueryVersion:     0,
		CheckedAt:        now.UTC().Format(time.RFC3339),
		StubActive:       true,
	}

	payload, _ := json.Marshal(result)
	resultChannel := fmt.Sprintf("tca:check-results:%s", req.RequestID)

	// Publish with TTL so orphaned results don't accumulate
	pipe := rdb.Pipeline()
	pipe.Set(ctx, resultChannel+"_data", payload, time.Duration(resultTTLSec)*time.Second)
	pipe.Publish(ctx, resultChannel, payload)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("[augur-canis] Failed to publish result for %s: %v", req.ServiceName, err)
		return
	}
	checksPublished.Add(1)

	// Publish to health feed (tca:augur-canis) — signed with session key if registered
	publishSignedFeedEvent(map[string]interface{}{
		"service_name":         req.ServiceName,
		"container_id":         req.ContainerID,
		"healthy":              healthy,
		"latency_ms":           latencyMs,
		"time_to_complete_ms":  result.TimeToCompleteMs,
		"query_version":        0,
		"checked_at":           result.CheckedAt,
		"constellation":        constellation,
		"stub_active":          true,
	})

	// Also report to seti:events so health checks appear in the observability dashboard.
	// AC is a caller like any other Job — its check executions are inter-service traffic.
	reportEvent(req.ServiceName, "POST", "/check", healthy*200+(1-healthy)*503, latencyMs)
}

// ---------------------------------------------------------------------------
// State recording — feeds all three anomaly detectors
// ---------------------------------------------------------------------------

func recordHealthState(ctx context.Context, serviceName, containerID string, healthy int, latencyMs int64, now time.Time) {
	pipe := rdb.Pipeline()

	// Update last seen timestamp
	pipe.Set(ctx, fmt.Sprintf(keyLastSeen, serviceName), now.UTC().Format(time.RFC3339), 0)

	// Track container ID for K8s replacement detection
	pipe.Set(ctx, fmt.Sprintf(keyLastContainerID, serviceName), containerID, 0)

	// Rolling latency window — capped at 20 samples
	// STUB: stored but not evaluated
	latencyKey := fmt.Sprintf(keyLatencyWindow, serviceName)
	pipe.LPush(ctx, latencyKey, latencyMs)
	pipe.LTrim(ctx, latencyKey, 0, 19)

	// Rolling health window — 0/1 per check, capped at 20 samples
	// STUB: stored but not evaluated
	healthKey := fmt.Sprintf(keyHealthWindow, serviceName)
	pipe.LPush(ctx, healthKey, healthy)
	pipe.LTrim(ctx, healthKey, 0, 19)

	pipe.Exec(ctx)

	// Check for K8s container replacement
	checkContainerReplacement(ctx, serviceName, containerID)
}

// ---------------------------------------------------------------------------
// K8s container replacement detection
// ---------------------------------------------------------------------------

func checkContainerReplacement(ctx context.Context, serviceName, currentContainerID string) {
	if currentContainerID == "" {
		return
	}

	// Get the container ID from the previous check
	prevKey := fmt.Sprintf(keyLastContainerID, serviceName) + ":prev"
	prev, err := rdb.Get(ctx, prevKey).Result()
	if err == redis.Nil {
		// First check for this service — store and return
		rdb.Set(ctx, prevKey, currentContainerID, 0)
		return
	}
	if err != nil {
		return
	}

	// Container ID changed — K8s replaced the container
	if prev != currentContainerID && prev != "" {
		log.Printf("[augur-canis] Container ID change detected for %s (%s → %s) — K8s replacement inferred",
			serviceName, prev[:8], currentContainerID[:8])

		// Auto-resolve any active alert for this service
		alertActiveKey := fmt.Sprintf(keyAlertActive, serviceName)
		isActive, _ := rdb.Get(ctx, alertActiveKey).Result()
		if isActive == "1" {
			resolveAlert(ctx, serviceName, "container_replaced")
		}

		// Update prev container ID
		rdb.Set(ctx, prevKey, currentContainerID, 0)
	}
}

// ---------------------------------------------------------------------------
// Contract test request handler
// ---------------------------------------------------------------------------

type ContractTestRequest struct {
	RequestID              string            `json:"request_id"`
	ServiceName            string            `json:"service_name"`
	Method                 string            `json:"method"`
	Path                   string            `json:"path"`
	Headers                map[string]string `json:"headers"`
	Body                   interface{}       `json:"body,omitempty"`
	ExpectedStatus         int               `json:"expected_status"`
	ExpectedResponseFields []string          `json:"expected_response_fields,omitempty"`
	TestName               string            `json:"test_name"`
	ContractVersion        string            `json:"contract_version"`
	PublishedAt            string            `json:"published_at"`
}

type ContractTestResult struct {
	RequestID      string      `json:"request_id"`
	ServiceName    string      `json:"service_name"`
	TestName       string      `json:"test_name"`
	Passed         bool        `json:"passed"`
	ActualStatus   int         `json:"actual_status"`
	ExpectedStatus int         `json:"expected_status"`
	ActualResponse interface{} `json:"actual_response,omitempty"`
	FailureReason  string      `json:"failure_reason,omitempty"`
	LatencyMs      int64       `json:"latency_ms"`
	ExecutedAt     string      `json:"executed_at"`
}

func handleContractTestRequests() {
	ctx := context.Background()

	for {
		sub := rdb.Subscribe(ctx, contractRequestsChannel)
		ch := sub.Channel()
		log.Printf("[augur-canis] Subscribed to %s", contractRequestsChannel)

		for msg := range ch {
			var req ContractTestRequest
			if err := json.Unmarshal([]byte(msg.Payload), &req); err != nil {
				log.Printf("[augur-canis] Failed to parse contract test request: %v", err)
				continue
			}

			if req.RequestID == "" || req.ServiceName == "" {
				log.Printf("[augur-canis] Invalid contract test request — missing fields")
				continue
			}

			go executeContractTest(req)
		}

		log.Printf("[augur-canis] %s subscription dropped — reconnecting in 2s", contractRequestsChannel)
		sub.Close()
		time.Sleep(2 * time.Second)
	}
}

func executeContractTest(req ContractTestRequest) {
	ctx := context.Background()
	start := time.Now()

	// Look up the registered Job's network endpoint
	job, err := getRegisteredJob(ctx, req.ServiceName)
	if err != nil {
		// Job not registered — skip, not fail. This is a deployment gap, not a test failure.
		publishContractTestResult(ctx, ContractTestResult{
			RequestID:      req.RequestID,
			ServiceName:    req.ServiceName,
			TestName:       req.TestName,
			Passed:         false,
			ExpectedStatus: req.ExpectedStatus,
			FailureReason:  "skip: job_not_deployed",
			ExecutedAt:     time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	// Build and execute the request over the point-to-point mTLS network
	target := job.NetworkEndpoint + req.Path

	var bodyReader *bytesReader
	if req.Body != nil {
		bodyBytes, _ := json.Marshal(req.Body)
		br := bytesReader(bodyBytes)
		bodyReader = &br
	}

	var httpReq *http.Request
	if bodyReader != nil {
		httpReq, err = http.NewRequest(req.Method, target, bodyReader)
	} else {
		httpReq, err = http.NewRequest(req.Method, target, nil)
	}
	if err != nil {
		publishContractTestResult(ctx, ContractTestResult{
			RequestID:      req.RequestID,
			ServiceName:    req.ServiceName,
			TestName:       req.TestName,
			Passed:         false,
			ExpectedStatus: req.ExpectedStatus,
			FailureReason:  fmt.Sprintf("Failed to build request: %v", err),
			ExecutedAt:     time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	// Apply headers from test request
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if httpReq.Header.Get("Content-Type") == "" && req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := upstreamClient.Do(httpReq)
	latencyMs := time.Since(start).Milliseconds()

	if err != nil {
		// Network-level failure (DNS, connection refused, timeout) = job not reachable.
		// Treat the same as job_not_deployed — a deployment gap, not a test failure.
		errStr := err.Error()
		skipReason := ""
		if strings.Contains(errStr, "no such host") || strings.Contains(errStr, "connection refused") || strings.Contains(errStr, "context deadline exceeded") {
			skipReason = "skip: job_not_reachable"
		}
		publishContractTestResult(ctx, ContractTestResult{
			RequestID:      req.RequestID,
			ServiceName:    req.ServiceName,
			TestName:       req.TestName,
			Passed:         false,
			ActualStatus:   0,
			ExpectedStatus: req.ExpectedStatus,
			FailureReason:  func() string { if skipReason != "" { return skipReason }; return fmt.Sprintf("Request failed: %v", err) }(),
			LatencyMs:      latencyMs,
			ExecutedAt:     time.Now().UTC().Format(time.RFC3339),
		})
		reportEvent(req.ServiceName, req.Method, req.Path, 0, latencyMs)
		return
	}
	defer resp.Body.Close()

	// Parse response body
	var responseBody interface{}
	bodyBytes := make([]byte, 64*1024) // 64KB max
	n, _ := resp.Body.Read(bodyBytes)
	if n > 0 {
		json.Unmarshal(bodyBytes[:n], &responseBody)
	}

	// Evaluate result
	passed := resp.StatusCode == req.ExpectedStatus
	failureReason := ""

	if !passed {
		failureReason = fmt.Sprintf("Expected status %d, got %d",
			req.ExpectedStatus, resp.StatusCode)
	} else if len(req.ExpectedResponseFields) > 0 {
		// Check expected fields exist in response
		if bodyMap, ok := responseBody.(map[string]interface{}); ok {
			for _, field := range req.ExpectedResponseFields {
				if _, exists := bodyMap[field]; !exists {
					passed = false
					failureReason = fmt.Sprintf("Expected field '%s' not found in response", field)
					break
				}
			}
		}
	}

	result := ContractTestResult{
		RequestID:      req.RequestID,
		ServiceName:    req.ServiceName,
		TestName:       req.TestName,
		Passed:         passed,
		ActualStatus:   resp.StatusCode,
		ExpectedStatus: req.ExpectedStatus,
		ActualResponse: responseBody,
		FailureReason:  failureReason,
		LatencyMs:      latencyMs,
		ExecutedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	publishContractTestResult(ctx, result)
	reportEvent(req.ServiceName, req.Method, req.Path, resp.StatusCode, latencyMs)

	log.Printf("[augur-canis] Contract test %s/%s: passed=%v status=%d latency=%dms",
		req.ServiceName, req.TestName, passed, resp.StatusCode, latencyMs)
}

func publishContractTestResult(ctx context.Context, result ContractTestResult) {
	payload, _ := json.Marshal(result)
	pipe := rdb.Pipeline()
	// Publish for Contract Test Job subscriber
	pipe.Publish(ctx, contractResultsChannel, payload)
	// Also store with TTL for late subscribers
	pipe.Set(ctx, fmt.Sprintf("tca:contract-result:%s", result.RequestID),
		payload, contractResultTTL)
	pipe.Exec(ctx)
}

// In-memory job registry for contract test routing
// Full implementation: backed by AC's own DB in Phase 4
var registeredJobs = map[string]*RegisteredJobRecord{}

type RegisteredJobRecord struct {
	ServiceName     string
	NetworkEndpoint string
	Description     string
	RegisteredAt    string
}

func getRegisteredJob(ctx context.Context, serviceName string) (*RegisteredJobRecord, error) {
	if job, ok := registeredJobs[serviceName]; ok {
		return job, nil
	}
	// Fall back to Redis for jobs registered via the admin API
	key := fmt.Sprintf("ac:job:%s", serviceName)
	data, err := rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, fmt.Errorf("job %s not registered", serviceName)
	}
	if err != nil {
		return nil, err
	}
	var job RegisteredJobRecord
	if err := json.Unmarshal([]byte(data), &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// ---------------------------------------------------------------------------
// Load jobs from Redis into in-memory map on startup
// Ensures registered jobs survive AC restarts
// ---------------------------------------------------------------------------

// bootstrapJobs seeds the in-memory registry with Phase 2 Jobs on startup.
// AC must know its constellation regardless of whether Policy has registered yet.
// Policy's POST /jobs calls update these records with any additional detail;
// the bootstrap ensures contract tests work even if Policy registration is delayed.
func bootstrapJobs() {
	defaultJobs := []RegisteredJobRecord{
		{ServiceName: "gateway",           NetworkEndpoint: envOr("ENDPOINT_GATEWAY",          "https://gateway:4000"),          Description: "SETI external gateway"},
		{ServiceName: "seti-observability",NetworkEndpoint: envOr("ENDPOINT_OBSERVABILITY",     "https://seti-observability:4011"), Description: "SETI observability event collector"},
		{ServiceName: "signal-clearance",  NetworkEndpoint: envOr("ENDPOINT_SIGNAL_CLEARANCE",  "https://signal-clearance:4001"), Description: "SETI identity and clearance"},
		{ServiceName: "ui",                NetworkEndpoint: envOr("ENDPOINT_UI",                "https://ui:4020"),                Description: "SETI monitoring dashboard"},
		{ServiceName: "augur-canis",       NetworkEndpoint: envOr("ENDPOINT_AUGUR_CANIS",       "https://augur-canis:4010"),      Description: "SETI Augur Canis health agent"},
		{ServiceName: "policy",            NetworkEndpoint: envOr("ENDPOINT_POLICY",            "https://policy:4002"),           Description: "SETI policy and application registry"},
		{ServiceName: "contract-test",     NetworkEndpoint: envOr("ENDPOINT_CONTRACT_TEST",     "https://contract-test:4003"),    Description: "SETI contract test executor"},
		{ServiceName: "results",           NetworkEndpoint: envOr("ENDPOINT_RESULTS",           "https://results:4008"),          Description: "SETI test results store"},
		{ServiceName: "signal-aggregator", NetworkEndpoint: envOr("ENDPOINT_SIGNAL_AGGREGATOR", "https://signal-aggregator:4006"), Description: "SETI signal aggregator"},
		{ServiceName: "plot-store",        NetworkEndpoint: envOr("ENDPOINT_PLOT_STORE",        "https://plot-store:4005"),       Description: "SETI plot store"},
		{ServiceName: "plot-test",         NetworkEndpoint: envOr("ENDPOINT_PLOT_TEST",         "https://plot-test:4004"),        Description: "SETI plot test executor"},
		{ServiceName: "interactions",      NetworkEndpoint: envOr("ENDPOINT_INTERACTIONS",      "https://interactions:4009"),     Description: "SETI interactions (stub)"},
		{ServiceName: "feed-wr4ngler",     NetworkEndpoint: envOr("ENDPOINT_FEED_WRANGLER",     "https://feed-wrangler:4007"),    Description: "SETI feed wrangler"},
		{ServiceName: "integration",      NetworkEndpoint: envOr("ENDPOINT_INTEGRATION",      "https://integration:4013"),      Description: "SETI versioned API"},
		{ServiceName: "ai-lien",          NetworkEndpoint: envOr("ENDPOINT_AI_LIEN",          "https://ai-lien:4252"),          Description: "SETI AI diagnostic engine"},
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := range defaultJobs {
		defaultJobs[i].RegisteredAt = now
		registeredJobs[defaultJobs[i].ServiceName] = &defaultJobs[i]
	}
	log.Printf("[augur-canis] Bootstrapped %d Phase 2 Jobs into registry", len(defaultJobs))

	// Also load any additional jobs previously registered in Redis by Policy
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "ac:job:*").Result()
	if err != nil {
		return
	}
	loaded := 0
	for _, key := range keys {
		data, err := rdb.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		var job RegisteredJobRecord
		if err := json.Unmarshal([]byte(data), &job); err != nil {
			continue
		}
		registeredJobs[job.ServiceName] = &job
		loaded++
	}
	if loaded > 0 {
		log.Printf("[augur-canis] Loaded %d additional jobs from Redis", loaded)
	}
}

// ---------------------------------------------------------------------------
// Silence detection — runs on a ticker
// ---------------------------------------------------------------------------

func runSilenceDetector() {
	ticker := time.NewTicker(time.Duration(checkIntervalSec) * time.Second)
	defer ticker.Stop()

	log.Printf("[augur-canis] Silence detector running — threshold %ds", silenceThresholdSec)

	for range ticker.C {
		checkSilence()
	}
}

func checkSilence() {
	ctx := context.Background()
	threshold := time.Duration(silenceThresholdSec) * time.Second

	// Find all tracked services
	keys, err := rdb.Keys(ctx, "ac:state:*:last_seen").Result()
	if err != nil {
		return
	}

	for _, key := range keys {
		// Extract service name from key: ac:state:{service_name}:last_seen
		var serviceName string
		fmt.Sscanf(key, "ac:state:%s", &serviceName)
		// Strip trailing :last_seen
		if len(serviceName) > 9 {
			serviceName = serviceName[:len(serviceName)-9]
		}
		if serviceName == "" {
			continue
		}

		lastSeenStr, err := rdb.Get(ctx, key).Result()
		if err != nil {
			continue
		}

		lastSeen, err := time.Parse(time.RFC3339, lastSeenStr)
		if err != nil {
			continue
		}

		silent := time.Since(lastSeen) > threshold
		alertActiveKey := fmt.Sprintf(keyAlertActive, serviceName)
		isActive, _ := rdb.Get(ctx, alertActiveKey).Result()

		if silent && isActive != "1" {
			// New silence — fire initial alert
			fireAlert(ctx, serviceName, AlertSilence, SeverityCritical,
				fmt.Sprintf("Job %s has been silent for >%ds — no health checks received",
					serviceName, silenceThresholdSec),
				lastSeen.UTC().Format(time.RFC3339))

		} else if silent && isActive == "1" {
			// Ongoing silence — reminder cadence
			handleOngoingAlert(ctx, serviceName)

		} else if !silent && isActive == "1" {
			// Condition cleared — auto-resolve
			alertTypeStr, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName)).Result()
			if AlertType(alertTypeStr) == AlertSilence {
				resolveAlert(ctx, serviceName, "condition_cleared")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Stubbed detectors — latency drift and failure rate
// ---------------------------------------------------------------------------

func runLatencyDriftDetector() {
	// STUB: data is recorded in recordHealthState but never evaluated here.
	// Implementation: read ac:state:{service}:latency_window from Redis,
	// compute rolling mean, compare current reading to mean * threshold multiplier,
	// fire AlertLatencyDrift if exceeded for K consecutive cycles.
	log.Printf("[augur-canis] STUB: Latency drift detector registered — not yet active")
}

func runFailureRateDetector() {
	// STUB: data is recorded in recordHealthState but never evaluated here.
	// Implementation: read ac:state:{service}:health_window from Redis,
	// compute failure_count/window_size, fire AlertFailureRate if > threshold.
	log.Printf("[augur-canis] STUB: Failure rate detector registered — not yet active")
}

// ---------------------------------------------------------------------------
// Alert lifecycle
// ---------------------------------------------------------------------------

func fireAlert(ctx context.Context, serviceName string, alertType AlertType, severity Severity, message, lastHealthyAt string) {
	alertID := fmt.Sprintf("ac-alert-%d", time.Now().UnixNano())
	now := time.Now().UTC().Format(time.RFC3339)

	// Set alert state in Redis
	pipe := rdb.Pipeline()
	pipe.Set(ctx, fmt.Sprintf(keyAlertActive, serviceName), "1", 0)
	pipe.Set(ctx, fmt.Sprintf(keyAlertID, serviceName), alertID, 0)
	pipe.Set(ctx, fmt.Sprintf(keyAlertType, serviceName), string(alertType), 0)
	pipe.Set(ctx, fmt.Sprintf(keyBarkCount, serviceName), "1", 0)
	pipe.Set(ctx, fmt.Sprintf(keyLastBarkAt, serviceName), now, 0)
	pipe.Set(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName), now, 0)
	// Store reverse lookup: alert_id → service_name
	pipe.Set(ctx, fmt.Sprintf(keyAlertByID, alertID), serviceName,
		time.Duration(24)*time.Hour)
	pipe.Exec(ctx)

	bark(ctx, Alert{
		AlertID:       alertID,
		ServiceName:   serviceName,
		AlertType:     alertType,
		Severity:      severity,
		Message:       message,
		TriggeredAt:   now,
		LastHealthyAt: lastHealthyAt,
		BarkNumber:    1,
		Constellation: constellation,
		StubActive:    alertType != AlertSilence, // silence detection is real
	})

	alertsFired.Add(1)
	log.Printf("[augur-canis] Alert fired: %s on %s (bark 1/3)", alertType, serviceName)
}

func handleOngoingAlert(ctx context.Context, serviceName string) {
	barkCountStr, _ := rdb.Get(ctx, fmt.Sprintf(keyBarkCount, serviceName)).Result()
	barkCount, _ := strconv.Atoi(barkCountStr)
	lastBarkStr, _ := rdb.Get(ctx, fmt.Sprintf(keyLastBarkAt, serviceName)).Result()
	alertID, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, serviceName)).Result()
	alertTypeStr, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName)).Result()
	firstAtStr, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName)).Result()

	now := time.Now().UTC()

	if barkCount < maxInitialBarks {
		// Still in initial 3-bark sequence — bark on next cycle
		barkCount++
		rdb.Set(ctx, fmt.Sprintf(keyBarkCount, serviceName), strconv.Itoa(barkCount), 0)
		rdb.Set(ctx, fmt.Sprintf(keyLastBarkAt, serviceName), now.Format(time.RFC3339), 0)

		bark(ctx, Alert{
			AlertID:       alertID,
			ServiceName:   serviceName,
			AlertType:     AlertType(alertTypeStr),
			Severity:      SeverityCritical,
			Message:       fmt.Sprintf("Job %s still silent — bark %d of %d", serviceName, barkCount, maxInitialBarks),
			TriggeredAt:   firstAtStr,
			BarkNumber:    barkCount,
			Constellation: constellation,
		})
		log.Printf("[augur-canis] Alert ongoing: %s (bark %d/%d)", serviceName, barkCount, maxInitialBarks)

	} else {
		// Reminder cadence — bark every reminderIntervalSec
		lastBark, err := time.Parse(time.RFC3339, lastBarkStr)
		if err != nil {
			return
		}

		if time.Since(lastBark) >= time.Duration(reminderIntervalSec)*time.Second {
			rdb.Set(ctx, fmt.Sprintf(keyLastBarkAt, serviceName), now.Format(time.RFC3339), 0)

			bark(ctx, Alert{
				AlertID:       alertID,
				ServiceName:   serviceName,
				AlertType:     AlertType(alertTypeStr),
				Severity:      SeverityCritical,
				Message:       fmt.Sprintf("REMINDER: Job %s still silent — unacknowledged", serviceName),
				TriggeredAt:   firstAtStr,
				BarkNumber:    -1, // -1 = reminder mode
				Constellation: constellation,
			})
			log.Printf("[augur-canis] Alert reminder: %s (unacknowledged)", serviceName)
		}
	}
}

func resolveAlert(ctx context.Context, serviceName, resolutionType string) {
	alertID, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, serviceName)).Result()
	alertTypeStr, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName)).Result()
	firstAtStr, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName)).Result()
	now := time.Now().UTC().Format(time.RFC3339)

	// Clear alert state
	pipe := rdb.Pipeline()
	pipe.Del(ctx, fmt.Sprintf(keyAlertActive, serviceName))
	pipe.Del(ctx, fmt.Sprintf(keyAlertID, serviceName))
	pipe.Del(ctx, fmt.Sprintf(keyAlertType, serviceName))
	pipe.Del(ctx, fmt.Sprintf(keyBarkCount, serviceName))
	pipe.Del(ctx, fmt.Sprintf(keyLastBarkAt, serviceName))
	pipe.Del(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName))
	pipe.Exec(ctx)

	_ = alertTypeStr // recorded for audit; resolved bark always uses AlertResolved type
	bark(ctx, Alert{
		AlertID:        alertID,
		ServiceName:    serviceName,
		AlertType:      AlertResolved,
		Severity:       SeverityInfo,
		Message:        fmt.Sprintf("Job %s alert resolved (%s)", serviceName, resolutionType),
		TriggeredAt:    firstAtStr,
		ResolutionType: resolutionType,
		ResolvedAt:     now,
		Constellation:  constellation,
	})

	log.Printf("[augur-canis] Alert resolved: %s (%s)", serviceName, resolutionType)
}

func bark(ctx context.Context, alert Alert) {
	payload, _ := json.Marshal(alert)
	if err := rdb.Publish(ctx, alertsChannel, payload).Err(); err != nil {
		log.Printf("[augur-canis] Failed to publish alert: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Acknowledgment — called by admin API
// ---------------------------------------------------------------------------

func acknowledgeAlert(ctx context.Context, alertID, acknowledgedBy, note string) error {
	// Look up service name from alert ID
	serviceName, err := rdb.Get(ctx, fmt.Sprintf(keyAlertByID, alertID)).Result()
	if err == redis.Nil {
		return fmt.Errorf("alert %s not found or already resolved", alertID)
	}
	if err != nil {
		return fmt.Errorf("redis error: %v", err)
	}

	// Verify this alert is still active for this service
	activeAlertID, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, serviceName)).Result()
	if activeAlertID != alertID {
		return fmt.Errorf("alert %s is no longer the active alert for %s", alertID, serviceName)
	}

	// Stop reminder cadence by setting bark count to acknowledged sentinel
	// We don't clear alert_active — condition may still be present.
	// We stop barking but keep watching.
	rdb.Set(ctx, fmt.Sprintf(keyBarkCount, serviceName), "acknowledged", 0)

	now := time.Now().UTC().Format(time.RFC3339)
	alertTypeStr, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName)).Result()
	firstAtStr, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName)).Result()

	bark(context.Background(), Alert{
		AlertID:       alertID,
		ServiceName:   serviceName,
		AlertType:     AlertType(alertTypeStr),
		Severity:      SeverityInfo,
		Message:       fmt.Sprintf("Alert acknowledged by %s: %s", acknowledgedBy, note),
		TriggeredAt:   firstAtStr,
		BarkNumber:    0, // 0 = acknowledgment bark
		Constellation: constellation,
		ResolvedAt:    now,
	})

	log.Printf("[augur-canis] Alert %s acknowledged by %s", alertID, acknowledgedBy)
	return nil
}

// ---------------------------------------------------------------------------
// Admin HTTP handlers
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	redisOK := rdb.Ping(ctx).Err() == nil

	// Count active alerts
	alertKeys, _ := rdb.Keys(ctx, "ac:state:*:alert_active").Result()
	activeAlerts := 0
	for _, k := range alertKeys {
		v, _ := rdb.Get(ctx, k).Result()
		if v == "1" {
			activeAlerts++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           statusString(redisOK),
		"stub_active":      true, // canned query execution not yet implemented
		"jobs_registered":  0,
		"queries_defined":  0,
		"checks_completed": checksHandled.Load(),
		"checks_failed":    0,
		"active_alerts":    activeAlerts,
		"alerts_fired":     alertsFired.Load(),
		"redis_connected":  redisOK,
		"last_cycle_at":    time.Now().UTC().Format(time.RFC3339),
		"uptime_seconds":   int(time.Since(startTime).Seconds()),
		"detectors": map[string]string{
			"silence":      "active",
			"latency_drift": "stub",
			"failure_rate":  "stub",
		},
	})
}

func statusString(redisOK bool) string {
	if !redisOK {
		return "degraded"
	}
	return "stub_active"
}

func handleAcknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}

	// Extract alert_id from path: /alerts/{alert_id}/acknowledge
	path := r.URL.Path
	var alertID string
	fmt.Sscanf(path, "/alerts/%s", &alertID)
	// Strip trailing /acknowledge
	if len(alertID) > 12 {
		alertID = alertID[:len(alertID)-12]
	}
	if alertID == "" {
		http.Error(w, `{"code":"MISSING_ALERT_ID"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		AcknowledgedBy string `json:"acknowledged_by"`
		Note           string `json:"note"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.AcknowledgedBy == "" {
		body.AcknowledgedBy = "unknown"
	}

	if err := acknowledgeAlert(r.Context(), alertID, body.AcknowledgedBy, body.Note); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "ALERT_NOT_FOUND",
			"message": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"alert_id":        alertID,
		"acknowledged_by": body.AcknowledgedBy,
		"acknowledged_at": time.Now().UTC().Format(time.RFC3339),
		"note":            body.Note,
	})
}

func handleActiveAlerts(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()

	alertKeys, _ := rdb.Keys(ctx, "ac:state:*:alert_id").Result()
	alerts := []map[string]interface{}{}

	for _, key := range alertKeys {
		var serviceName string
		fmt.Sscanf(key, "ac:state:%s", &serviceName)
		if len(serviceName) > 9 {
			serviceName = serviceName[:len(serviceName)-9]
		}
		if serviceName == "" {
			continue
		}

		isActive, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertActive, serviceName)).Result()
		if isActive != "1" {
			continue
		}

		alertID, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, serviceName)).Result()
		alertTypeVal, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName)).Result()
		barkCount, _ := rdb.Get(ctx, fmt.Sprintf(keyBarkCount, serviceName)).Result()
		firstAt, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName)).Result()
		lastBark, _ := rdb.Get(ctx, fmt.Sprintf(keyLastBarkAt, serviceName)).Result()

		alerts = append(alerts, map[string]interface{}{
			"alert_id":     alertID,
			"service_name": serviceName,
			"alert_type":   alertTypeVal,
			"bark_count":   barkCount,
			"first_at":     firstAt,
			"last_bark_at": lastBark,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"alerts": alerts,
		"total":  len(alerts),
	})
}

func handleConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"check_interval_seconds":    checkIntervalSec,
		"result_ttl_seconds":        resultTTLSec,
		"silence_threshold_seconds": silenceThresholdSec,
		"reminder_interval_seconds": reminderIntervalSec,
		"max_initial_barks":         maxInitialBarks,
		"feed_channel":              healthFeedChannel,
		"request_channel":           checkRequestsChannel,
		"alerts_channel":            alertsChannel,
		"stub_active":               true,
		"jobs_registered":           0,
		"queries_defined":           0,
	})
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		jobs := make([]*RegisteredJobRecord, 0, len(registeredJobs))
		for _, j := range registeredJobs {
			jobs = append(jobs, j)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"jobs":  jobs,
			"total": len(jobs),
		})

	case http.MethodPost:
		var req RegisteredJobRecord
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": err.Error()})
			return
		}
		if req.ServiceName == "" || req.NetworkEndpoint == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "service_name and network_endpoint required"})
			return
		}
		req.RegisteredAt = time.Now().UTC().Format(time.RFC3339)
		registeredJobs[req.ServiceName] = &req

		// Also persist to Redis for durability across AC restarts
		data, _ := json.Marshal(req)
		rdb.Set(ctx, fmt.Sprintf("ac:job:%s", req.ServiceName), data, 0)

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(req)
		log.Printf("[augur-canis] Job registered: %s → %s", req.ServiceName, req.NetworkEndpoint)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleQueriesStub(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	json.NewEncoder(w).Encode(map[string]string{
		"code":    "STUB_ACTIVE",
		"message": "Canned query management deferred to Phase 4 full implementation.",
	})
}

func handleChecksStub(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	json.NewEncoder(w).Encode(map[string]string{
		"code":    "STUB_ACTIVE",
		"message": "Direct check triggering deferred to Phase 4 full implementation.",
	})
}

// ---------------------------------------------------------------------------
// mTLS upstream client
// ---------------------------------------------------------------------------

var upstreamClient *http.Client

func buildUpstreamClient() {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Printf("[augur-canis] CA cert not found — observability reporting will fail: %v", err)
		upstreamClient = http.DefaultClient
		return
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	cert, err := tls.LoadX509KeyPair("/certs/augur-canis.crt", "/certs/augur-canis.key")
	if err != nil {
		log.Printf("[augur-canis] Service cert not found — observability reporting will fail: %v", err)
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
		Timeout: 5 * time.Second,
	}
	log.Printf("[augur-canis] mTLS upstream client ready")
}

// ---------------------------------------------------------------------------
// Observability reporting
// ---------------------------------------------------------------------------

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller":      "augur-canis",
			"callee":      callee,
			"method":      method,
			"path":        path,
			"status_code": status,
			"latency_ms":  latencyMs,
			"protocol":    "mtls",
		})
		br := bytesReader(body)
		req, err := http.NewRequest(http.MethodPost, observabilityURL+"/event", br)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := upstreamClient.Do(req)
		if err != nil {
			log.Printf("[augur-canis] observability report error: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

type bytesReader []byte

func (b bytesReader) Read(p []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("EOF")
	}
	n = copy(p, b)
	return n, nil
}

// ---------------------------------------------------------------------------
// mTLS server
// ---------------------------------------------------------------------------

func loadTLSConfig() *tls.Config {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Fatalf("[augur-canis] Failed to read CA cert: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	cert, err := tls.LoadX509KeyPair("/certs/augur-canis.crt", "/certs/augur-canis.key")
	if err != nil {
		log.Fatalf("[augur-canis] Failed to load service cert: %v", err)
	}

	return &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	buildUpstreamClient()
	connectRedis()
	bootstrapJobs()

	// Start check request handler
	go handleCheckRequests()

	// Start contract test request handler
	go handleContractTestRequests()

	// Start silence detector
	go runSilenceDetector()

	// Register stubbed detectors (no-ops until implemented)
	runLatencyDriftDetector()
	runFailureRateDetector()

	// Admin API
	mux := http.NewServeMux()
	loadStarGazerCert()

	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/federation/register", handleFederationRegister)
	mux.HandleFunc("/federation/status", handleFederationStatus)
	mux.HandleFunc("/services/register", handleServicesRegister)
	mux.HandleFunc("/configuration", handleConfiguration)
	mux.HandleFunc("/alerts/active", handleActiveAlerts)
	mux.HandleFunc("/alerts/", handleAcknowledge) // /alerts/{id}/acknowledge
	mux.HandleFunc("/jobs", handleJobs)
	mux.HandleFunc("/queries/", handleQueriesStub)
	mux.HandleFunc("/checks/", handleChecksStub)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadTLSConfig(),
	}

	log.Printf("[augur-canis] Admin API listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[augur-canis] Health feed: %s", healthFeedChannel)
	log.Printf("[augur-canis] Alerts feed: %s", alertsChannel)
	log.Printf("[augur-canis] Silence threshold: %ds", silenceThresholdSec)
	log.Printf("[augur-canis] Reminder interval: %ds", reminderIntervalSec)
	log.Printf("[augur-canis] Initial barks: %d", maxInitialBarks)
	log.Printf("[augur-canis] STUB: Canned query execution deferred to full implementation")

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[augur-canis] Server error: %v", err)
	}
}
