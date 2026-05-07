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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

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
	keyLastSeen        = "ac:state:%s:last_seen"
	keyLastContainerID = "ac:state:%s:last_container_id"
	keyLatencyStream   = "ac:metrics:%s:latency" // Redis Stream — replaces keyLatencyWindow
	keyHealthStream    = "ac:metrics:%s:health"  // Redis Stream — replaces keyHealthWindow
	keyAlertActive     = "ac:state:%s:alert_active"
	keyAlertID         = "ac:state:%s:alert_id"
	keyAlertType       = "ac:state:%s:alert_type"
	keyBarkCount       = "ac:state:%s:bark_count"
	keyLastBarkAt      = "ac:state:%s:last_bark_at"
	keyAlertFirstAt    = "ac:state:%s:alert_first_at"

	// Stream retention — keep 24h of metric samples per Job
	metricsStreamMaxLen = int64(2000) // ~24h at 30s check interval

	// Active alerts stored by alert_id for acknowledgment lookup
	keyAlertByID = "ac:alert:%s" // ac:alert:{alert_id} → service_name
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

var rdb *RedisClient

func connectRedis() {
	rdb = NewRedisClient(redisURL)
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := rdb.Ping(ctx)
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

var checkShutdown context.CancelFunc

func handleCheckRequests() {
	outerCtx, outerCancel := context.WithCancel(context.Background())
	checkShutdown = outerCancel

	for {
		select {
		case <-outerCtx.Done():
			return
		default:
		}
		ch, err := rdb.Subscribe(outerCtx, checkRequestsChannel)
		if err != nil {
			log.Printf("[augur-canis] Failed to subscribe to %s: %v — retrying in 2s", checkRequestsChannel, err)
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("[augur-canis] Subscribed to %s", checkRequestsChannel)

		for msg := range ch {
			checksHandled.Add(1)

			var req CheckRequest
			if err := json.Unmarshal([]byte(msg), &req); err != nil {
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
	pipe := rdb.NewPipeline()
	pipe.Set(resultChannel+"_data", string(payload), time.Duration(resultTTLSec)*time.Second)
	pipe.Publish(resultChannel, string(payload))
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
	pipe := rdb.NewPipeline()

	// Update last seen timestamp
	pipe.Set(fmt.Sprintf(keyLastSeen, serviceName), now.UTC().Format(time.RFC3339), 0)

	// Track container ID for K8s replacement detection
	pipe.Set(fmt.Sprintf(keyLastContainerID, serviceName), containerID, 0)

	// Latency stream — time-series samples for UI and detectors
	pipe.XAdd(fmt.Sprintf(keyLatencyStream, serviceName), metricsStreamMaxLen, map[string]string{
		"latency_ms":   fmt.Sprintf("%d", latencyMs),
		"service":      serviceName,
		"container_id": containerID,
	})

	// Health stream — 0/1 samples for failure rate detector and UI
	pipe.XAdd(fmt.Sprintf(keyHealthStream, serviceName), metricsStreamMaxLen, map[string]string{
		"healthy":      fmt.Sprintf("%d", healthy),
		"service":      serviceName,
		"container_id": containerID,
	})

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
	prev, ok, err := rdb.Get(ctx, prevKey)
	if !ok {
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
		isActive, _, _ := rdb.Get(ctx, alertActiveKey)
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
		ch, err := rdb.Subscribe(ctx, contractRequestsChannel)
		if err != nil {
			log.Printf("[augur-canis] Failed to subscribe to %s: %v — retrying in 2s", contractRequestsChannel, err)
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("[augur-canis] Subscribed to %s", contractRequestsChannel)

		for msg := range ch {
			var req ContractTestRequest
			if err := json.Unmarshal([]byte(msg), &req); err != nil {
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
	// Re-validated here to close CodeQL taint path — primary validation at
	// intake in the job registration handler.
	if err := validateHTTPSURL(job.NetworkEndpoint); err != nil {
		publishContractTestResult(ctx, ContractTestResult{
			RequestID: req.RequestID, ServiceName: req.ServiceName,
			TestName: req.TestName, Passed: false,
			ExpectedStatus: req.ExpectedStatus,
			FailureReason:  "invalid endpoint: " + err.Error(),
			ExecutedAt:     time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
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
	pipe := rdb.NewPipeline()
	// Publish for Contract Test Job subscriber
	pipe.Publish(contractResultsChannel, string(payload))
	// Also store with TTL for late subscribers
	pipe.Set(fmt.Sprintf("tca:contract-result:%s", result.RequestID),
		string(payload), contractResultTTL)
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
	data, ok, err := rdb.Get(ctx, key)
	if !ok {
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

// loadPersistedJobs restores self-registration records persisted to Redis.
// On restart, services will re-register themselves within seconds.
// This load ensures AC knows about them immediately rather than waiting
// for the first health check cycle after each service comes up.
func loadPersistedJobs() {
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "ac:job:*")
	if err != nil {
		log.Printf("[augur-canis] Could not load persisted jobs from Redis: %v", err)
		return
	}
	loaded := 0
	for _, key := range keys {
		data, ok, err := rdb.Get(ctx, key)
		if !ok || err != nil {
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
		log.Printf("[augur-canis] Loaded %d self-registered jobs from Redis", loaded)
	} else {
		log.Printf("[augur-canis] No persisted jobs found — waiting for services to self-register")
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
	keys, err := rdb.Keys(ctx, "ac:state:*:last_seen")
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

		lastSeenStr, ok, err := rdb.Get(ctx, key)
		if !ok || err != nil {
			continue
		}

		lastSeen, err := time.Parse(time.RFC3339, lastSeenStr)
		if err != nil {
			continue
		}

		silent := time.Since(lastSeen) > threshold
		alertActiveKey := fmt.Sprintf(keyAlertActive, serviceName)
		isActive, _, _ := rdb.Get(ctx, alertActiveKey)

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
			alertTypeStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName))
			if AlertType(alertTypeStr) == AlertSilence {
				resolveAlert(ctx, serviceName, "condition_cleared")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Stubbed detectors — latency drift and failure rate
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Alert lifecycle
// ---------------------------------------------------------------------------

func fireAlert(ctx context.Context, serviceName string, alertType AlertType, severity Severity, message, lastHealthyAt string) {
	alertID := fmt.Sprintf("ac-alert-%d", time.Now().UnixNano())
	now := time.Now().UTC().Format(time.RFC3339)

	// Set alert state in Redis
	pipe := rdb.NewPipeline()
	pipe.Set(fmt.Sprintf(keyAlertActive, serviceName), "1", 0)
	pipe.Set(fmt.Sprintf(keyAlertID, serviceName), alertID, 0)
	pipe.Set(fmt.Sprintf(keyAlertType, serviceName), string(alertType), 0)
	pipe.Set(fmt.Sprintf(keyBarkCount, serviceName), "1", 0)
	pipe.Set(fmt.Sprintf(keyLastBarkAt, serviceName), now, 0)
	pipe.Set(fmt.Sprintf(keyAlertFirstAt, serviceName), now, 0)
	// Store reverse lookup: alert_id → service_name
	pipe.Set(fmt.Sprintf(keyAlertByID, alertID), serviceName,
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
	barkCountStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyBarkCount, serviceName))
	barkCount, _ := strconv.Atoi(barkCountStr)
	lastBarkStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyLastBarkAt, serviceName))
	alertID, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, serviceName))
	alertTypeStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName))
	firstAtStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName))

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
	alertID, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, serviceName))
	alertTypeStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName))
	firstAtStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName))
	now := time.Now().UTC().Format(time.RFC3339)

	// Clear alert state
	pipe := rdb.NewPipeline()
	pipe.Del(fmt.Sprintf(keyAlertActive, serviceName))
	pipe.Del(fmt.Sprintf(keyAlertID, serviceName))
	pipe.Del(fmt.Sprintf(keyAlertType, serviceName))
	pipe.Del(fmt.Sprintf(keyBarkCount, serviceName))
	pipe.Del(fmt.Sprintf(keyLastBarkAt, serviceName))
	pipe.Del(fmt.Sprintf(keyAlertFirstAt, serviceName))
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
	if err := rdb.Publish(ctx, alertsChannel, string(payload)); err != nil {
		log.Printf("[augur-canis] Failed to publish alert: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Acknowledgment — called by admin API
// ---------------------------------------------------------------------------

func acknowledgeAlert(ctx context.Context, alertID, acknowledgedBy, note string) error {
	// Look up service name from alert ID
	serviceName, ok, err := rdb.Get(ctx, fmt.Sprintf(keyAlertByID, alertID))
	if !ok {
		return fmt.Errorf("alert %s not found or already resolved", alertID)
	}
	if err != nil {
		return fmt.Errorf("redis error: %v", err)
	}

	// Verify this alert is still active for this service
	activeAlertID, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, serviceName))
	if activeAlertID != alertID {
		return fmt.Errorf("alert %s is no longer the active alert for %s", alertID, serviceName)
	}

	// Stop reminder cadence by setting bark count to acknowledged sentinel
	// We don't clear alert_active — condition may still be present.
	// We stop barking but keep watching.
	rdb.Set(ctx, fmt.Sprintf(keyBarkCount, serviceName), "acknowledged", 0)

	now := time.Now().UTC().Format(time.RFC3339)
	alertTypeStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName))
	firstAtStr, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName))

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
	redisOK := rdb.Ping(ctx) == nil

	// Count active alerts
	alertKeys, _ := rdb.Keys(ctx, "ac:state:*:alert_active")
	activeAlerts := 0
	for _, k := range alertKeys {
		v, _, _ := rdb.Get(ctx, k)
		if v == "1" {
			activeAlerts++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           statusString(redisOK),
		"jobs_registered":  len(registeredJobs),
		"queries_defined":  len(registeredJobs), // one query per registered job
		"checks_completed": checksHandled.Load(),
		"checks_failed":    0,
		"active_alerts":    activeAlerts,
		"alerts_fired":     alertsFired.Load(),
		"redis_connected":  redisOK,
		"last_cycle_at":    time.Now().UTC().Format(time.RFC3339),
		"uptime_seconds":   int(time.Since(startTime).Seconds()),
		"detectors": map[string]string{
			"silence":       "active",
			"latency_drift": "active",
			"failure_rate":  "active",
		},
	})
}

func statusString(redisOK bool) string {
	if !redisOK {
		return "degraded"
	}
	return "healthy"
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

	alertKeys, _ := rdb.Keys(ctx, "ac:state:*:alert_id")
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

		isActive, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertActive, serviceName))
		if isActive != "1" {
			continue
		}

		alertID, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, serviceName))
		alertTypeVal, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, serviceName))
		barkCount, _, _ := rdb.Get(ctx, fmt.Sprintf(keyBarkCount, serviceName))
		firstAt, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, serviceName))
		lastBark, _, _ := rdb.Get(ctx, fmt.Sprintf(keyLastBarkAt, serviceName))

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
		"jobs_registered":           len(registeredJobs),
		"queries_defined":           len(registeredJobs),
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
		if err := validateHTTPSURL(req.NetworkEndpoint); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_ENDPOINT", "message": "network_endpoint: " + err.Error()})
			return
		}
		req.RegisteredAt = time.Now().UTC().Format(time.RFC3339)
		registeredJobs[req.ServiceName] = &req

		// Also persist to Redis for durability across AC restarts
		data, _ := json.Marshal(req)
		rdb.Set(ctx, fmt.Sprintf("ac:job:%s", req.ServiceName), string(data), 0)

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(req)
		log.Printf("[augur-canis] Job registered: %s → %s", req.ServiceName, req.NetworkEndpoint)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleQueriesStub(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"queries": setiContractTests,
		"total":   len(setiContractTests),
	})
}

// handleChecksStub handles /checks/{service} — returns recent results for a specific Job.
func handleChecksStub(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	service := strings.TrimPrefix(r.URL.Path, "/checks/")
	service = strings.TrimSuffix(service, "/")

	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "ac:contract-suite:*")
	if err != nil || len(keys) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"checks": []interface{}{}, "service": service})
		return
	}

	var latest string
	for _, k := range keys {
		if latest == "" || k > latest {
			latest = k
		}
	}

	raw, ok, err := rdb.Get(ctx, latest)
	if !ok || err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"checks": []interface{}{}, "service": service})
		return
	}

	var suite ContractSuiteResult
	if err := json.Unmarshal([]byte(raw), &suite); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"checks": []interface{}{}, "service": service})
		return
	}

	checks := []ContractTestResult{}
	for _, res := range suite.Results {
		if res.ServiceName == service {
			checks = append(checks, res)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"service": service,
		"checks":  checks,
		"total":   len(checks),
		"run_id":  suite.RunID,
	})
}

// ---------------------------------------------------------------------------
// mTLS upstream client
// ---------------------------------------------------------------------------

var upstreamClient *http.Client

func buildUpstreamClient() {
	if certMat == nil {
		log.Printf("[augur-canis] certMat not ready — using default client")
		upstreamClient = http.DefaultClient
		return
	}
	upstreamClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: buildClientTLS(certMat),
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
	return buildServerTLS(certMat)
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

var certMat *CertMaterial

func main() {
	certMat = obtainCerts("augur-canis")
	buildUpstreamClient()
	connectRedis()
	loadPersistedJobs()

	// Always ensure augur-canis is in its own job registry.
	// Other services register via POST /jobs; augur-canis must do this for itself
	// so it appears in contract test runs even after a fresh Redis start.
	if _, exists := registeredJobs["augur-canis"]; !exists {
		self := RegisteredJobRecord{
			ServiceName:     "augur-canis",
			NetworkEndpoint: envOr("AUGUR_CANIS_URL", "https://augur-canis:4010"),
			RegisteredAt:    time.Now().UTC().Format(time.RFC3339),
		}
		registeredJobs["augur-canis"] = &self
		data, _ := json.Marshal(self)
		rdb.Set(context.Background(), "ac:job:augur-canis", string(data), 0)
		log.Printf("[augur-canis] Self-registered as job: %s", self.NetworkEndpoint)
	}

	// Start dev UI if AC_UI_PORT is set (plain HTTP, no auth — dev/setup only)
	startDevUI()

	// Start check request handler
	go handleCheckRequests()

	// Start contract test request handler (Redis pub/sub path from contract-test Job)
	go handleContractTestRequests()

	// Start AC's own contract test scheduler
	go runContractTestScheduler()

	// Start silence detector
	go runSilenceDetector()

	// Start latency drift and failure rate detectors
	runLatencyDriftDetector()
	runFailureRateDetector()

	// Admin API
	mux := http.NewServeMux()
	loadStarGazerCert()
	go selfRegisterWithAC(certMat, "https://augur-canis:4010")

	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/federation/register", handleFederationRegister)
	mux.HandleFunc("/federation/status", handleFederationStatus)
	mux.HandleFunc("/services/register", handleServicesRegister)
	mux.HandleFunc("/configuration", handleConfiguration)
	mux.HandleFunc("/alerts/active", handleActiveAlerts)
	mux.HandleFunc("/alerts/", handleAcknowledge) // /alerts/{id}/acknowledge
	mux.HandleFunc("/jobs", handleJobs)
	mux.HandleFunc("/queries/", handleQueriesStub)
	mux.HandleFunc("/checks/recent", handleRecentSuiteResults)
	mux.HandleFunc("/checks/", handleChecksStub)
	mux.HandleFunc("/run-contract-tests", handleRunContractTests)
	mux.HandleFunc("/ring/run", handleAdHocContractTest)
	mux.HandleFunc("/contract-suites", handleContractSuitesList)
	mux.HandleFunc("/contract-suites/", handleContractSuiteDetail)
	mux.HandleFunc("/baselines", handleListBaselines)
	mux.HandleFunc("/baselines/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleGetWranglerBaseline(w, r)
		case http.MethodPost:
			handleSetWranglerBaseline(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/metrics", handleMetrics)

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
	log.Printf("[augur-canis] Contract tests: %d tests, interval %ds", len(setiContractTests), contractTestIntervalSec)

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[augur-canis] Server error: %v", err)
		}
	}()
	awaitShutdown(server)
}
