package main

// ---------------------------------------------------------------------------
// AC Detectors — latency drift and failure rate monitoring.
//
// Two detectors, both per-Job:
//
//   Latency Drift:
//     Reads the 20-sample rolling latency window recorded by recordHealthState.
//     Establishes an auto-baseline after MIN_BASELINE_SAMPLES samples.
//     Fires AlertLatencyDrift if the current rolling mean exceeds the baseline
//     by more than LATENCY_DRIFT_MULTIPLIER.
//     If a Wr4ngler baseline exists for this Job, also checks against that
//     threshold and fires at the configured severity.
//
//   Failure Rate:
//     Reads the 20-sample rolling health window (0/1 per check).
//     Fires AlertFailureRate if failure rate exceeds FAILURE_RATE_THRESHOLD.
//     If a Wr4ngler baseline exists, uses the configured severity.
//
// Two baseline tiers:
//
//   Auto-baseline (AC-established):
//     Built from observed data after MIN_BASELINE_SAMPLES readings.
//     Stored in Redis. Rebuilds on restart. Fires at SeverityWarn.
//     Key: ac:baseline:auto:{service}
//
//   Wr4ngler baseline (human-set):
//     Set explicitly via POST /baselines/{service} admin API.
//     Persists in Redis indefinitely. Fires at operator-configured severity.
//     Key: ac:baseline:wr4ngler:{service}
//     Maps directly to Notifier severity — the operator decides what each
//     severity level means for their notification system.
//
// Thresholds — hardcoded by design. Change requires code review + rebuild.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Detector constants — hardcoded by design
// ---------------------------------------------------------------------------

const (
	minBaselineSamples      = 10   // samples required before auto-baseline is established
	latencyDriftMultiplier  = 2.0  // auto-baseline fires at 2x rolling mean
	failureRateThreshold    = 0.3  // 30% failure rate triggers alert
	detectorIntervalSeconds = 30   // how often detectors run
	consecutiveDriftCycles  = 3    // must exceed threshold for N cycles before firing
)

// ---------------------------------------------------------------------------
// Redis key patterns
// ---------------------------------------------------------------------------

const (
	keyAutoBaseline     = "ac:baseline:auto:%s"      // → JSON AutoBaseline
	keyWranglerBaseline = "ac:baseline:wr4ngler:%s"  // → JSON WranglerBaseline
	keyDriftCycles      = "ac:state:%s:drift_cycles" // consecutive over-threshold cycles
)

// ---------------------------------------------------------------------------
// Baseline types
// ---------------------------------------------------------------------------

// AutoBaseline is established by AC from observed data.
type AutoBaseline struct {
	ServiceName   string  `json:"service_name"`
	MeanLatencyMs float64 `json:"mean_latency_ms"`
	SampleCount   int     `json:"sample_count"`
	EstablishedAt string  `json:"established_at"`
	LastUpdatedAt string  `json:"last_updated_at"`
}

// WranglerBaseline is set explicitly by a Sec Wr4ngler.
type WranglerBaseline struct {
	ServiceName        string  `json:"service_name"`
	LatencyThresholdMs int64   `json:"latency_threshold_ms"`
	FailureRateThresh  float64 `json:"failure_rate_threshold"`
	AlertLevel         string  `json:"alert_level"`
	SetBy              string  `json:"set_by"`
	SetAt              string  `json:"set_at"`
	Notes              string  `json:"notes,omitempty"`
}

// ---------------------------------------------------------------------------
// Auto-baseline management
// ---------------------------------------------------------------------------

func loadAutoBaseline(ctx context.Context, serviceName string) *AutoBaseline {
	raw, ok, err := rdb.Get(ctx, fmt.Sprintf(keyAutoBaseline, serviceName))
	if !ok || err != nil {
		return nil
	}
	var b AutoBaseline
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil
	}
	return &b
}

func saveAutoBaseline(ctx context.Context, b AutoBaseline) {
	payload, _ := json.Marshal(b)
	rdb.Set(ctx, fmt.Sprintf(keyAutoBaseline, b.ServiceName), string(payload), 0)
}

func loadWranglerBaseline(ctx context.Context, serviceName string) *WranglerBaseline {
	raw, ok, err := rdb.Get(ctx, fmt.Sprintf(keyWranglerBaseline, serviceName))
	if !ok || err != nil {
		return nil
	}
	var b WranglerBaseline
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil
	}
	return &b
}

func saveWranglerBaseline(ctx context.Context, b WranglerBaseline) {
	payload, _ := json.Marshal(b)
	rdb.Set(ctx, fmt.Sprintf(keyWranglerBaseline, b.ServiceName), string(payload), 0)
}

// ---------------------------------------------------------------------------
// Stream readers — read from Redis Streams for time-bounded queries
// ---------------------------------------------------------------------------

// readLatencyWindow reads the last N latency samples from the Stream.
func readLatencyWindow(ctx context.Context, serviceName string) ([]float64, float64) {
	msgs, err := rdb.XRevRangeN(ctx,
		fmt.Sprintf(keyLatencyStream, serviceName), "+", "-", 20)
	if err != nil || len(msgs) == 0 {
		return nil, 0
	}
	samples := make([]float64, 0, len(msgs))
	sum := 0.0
	for _, msg := range msgs {
		if v, ok := msg.Fields["latency_ms"]; ok {
			n, _ := strconv.ParseFloat(v, 64)
			if n > 0 {
				samples = append(samples, n)
				sum += n
			}
		}
	}
	if len(samples) == 0 {
		return nil, 0
	}
	return samples, sum / float64(len(samples))
}

// readHealthWindow reads the last N health samples from the Stream.
func readHealthWindow(ctx context.Context, serviceName string) (int, int) {
	msgs, err := rdb.XRevRangeN(ctx,
		fmt.Sprintf(keyHealthStream, serviceName), "+", "-", 20)
	if err != nil || len(msgs) == 0 {
		return 0, 0
	}
	failures := 0
	for _, msg := range msgs {
		if v, ok := msg.Fields["healthy"]; ok {
			h, _ := strconv.Atoi(v)
			if h == 0 {
				failures++
			}
		}
	}
	return failures, len(msgs)
}

// ReadMetricsWindow reads time-bounded latency and health samples for the UI.
func ReadMetricsWindow(ctx context.Context, serviceName string, sinceMs int64) []MetricSample {
	start := "-"
	if sinceMs > 0 {
		start = fmt.Sprintf("%d-0", sinceMs)
	}
	msgs, err := rdb.XRange(ctx,
		fmt.Sprintf(keyLatencyStream, serviceName), start, "+")
	if err != nil {
		return nil
	}

	healthMsgs, _ := rdb.XRange(ctx,
		fmt.Sprintf(keyHealthStream, serviceName), start, "+")
	healthByID := map[string]int{}
	for _, msg := range healthMsgs {
		msID := strings.SplitN(msg.ID, "-", 2)[0]
		if v, ok := msg.Fields["healthy"]; ok {
			h, _ := strconv.Atoi(v)
			healthByID[msID] = h
		}
	}

	samples := make([]MetricSample, 0, len(msgs))
	for _, msg := range msgs {
		msID := strings.SplitN(msg.ID, "-", 2)[0]
		tsMs, _ := strconv.ParseInt(msID, 10, 64)

		var latency float64
		if v, ok := msg.Fields["latency_ms"]; ok {
			latency, _ = strconv.ParseFloat(v, 64)
		}

		healthy := 1
		if h, ok := healthByID[msID]; ok {
			healthy = h
		}

		samples = append(samples, MetricSample{
			TimestampMs: tsMs,
			LatencyMs:   latency,
			Healthy:     healthy == 1,
			ServiceName: serviceName,
		})
	}
	return samples
}

// MetricSample is a single time-series data point for a Job.
type MetricSample struct {
	TimestampMs int64   `json:"ts"`
	LatencyMs   float64 `json:"latency_ms"`
	Healthy     bool    `json:"healthy"`
	ServiceName string  `json:"service"`
}

// ---------------------------------------------------------------------------
// Latency drift detector
// ---------------------------------------------------------------------------

func runLatencyDriftDetector() {
	log.Printf("[augur-canis] Latency drift detector active — interval %ds, threshold %.1fx, min samples %d",
		detectorIntervalSeconds, latencyDriftMultiplier, minBaselineSamples)

	go func() {
		ticker := time.NewTicker(time.Duration(detectorIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			checkLatencyDriftAllServices()
		}
	}()
}

func checkLatencyDriftAllServices() {
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "ac:metrics:*:latency")
	if err != nil {
		return
	}
	for _, key := range keys {
		serviceName := strings.TrimPrefix(key, "ac:metrics:")
		serviceName = strings.TrimSuffix(serviceName, ":latency")
		checkLatencyDrift(ctx, serviceName)
	}
}

func checkLatencyDrift(ctx context.Context, serviceName string) {
	samples, currentMean := readLatencyWindow(ctx, serviceName)
	if len(samples) < minBaselineSamples {
		return
	}

	auto := loadAutoBaseline(ctx, serviceName)
	now := time.Now().UTC().Format(time.RFC3339)

	if auto == nil {
		auto = &AutoBaseline{
			ServiceName:   serviceName,
			MeanLatencyMs: currentMean,
			SampleCount:   len(samples),
			EstablishedAt: now,
			LastUpdatedAt: now,
		}
		saveAutoBaseline(ctx, *auto)
		log.Printf("[augur-canis] Auto-baseline established for %s: %.1fms mean (%d samples)",
			serviceName, currentMean, len(samples))
		return
	}

	alpha := 0.05
	auto.MeanLatencyMs = (1-alpha)*auto.MeanLatencyMs + alpha*currentMean
	auto.SampleCount++
	auto.LastUpdatedAt = now
	saveAutoBaseline(ctx, *auto)

	driftThreshold := auto.MeanLatencyMs * latencyDriftMultiplier
	if currentMean > driftThreshold {
		driftKey := fmt.Sprintf(keyDriftCycles, serviceName)
		rdb.Incr(ctx, driftKey)
		rdb.Expire(ctx, driftKey, 5*time.Minute)
		cyclesStr, _, _ := rdb.Get(ctx, driftKey)
		cycles, _ := strconv.Atoi(cyclesStr)

		if cycles >= consecutiveDriftCycles {
			isActive, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertActive, serviceName))
			if isActive != "1" {
				msg := fmt.Sprintf("Latency drift: %s current mean %.1fms is %.1fx above auto-baseline %.1fms",
					serviceName, currentMean, currentMean/auto.MeanLatencyMs, auto.MeanLatencyMs)
				fireAlert(ctx, serviceName, AlertLatencyDrift, SeverityWarn, msg, now)
				log.Printf("[augur-canis] Latency drift alert: %s", msg)
			}
			rdb.Del(ctx, driftKey)
		}
	} else {
		rdb.Del(ctx, fmt.Sprintf(keyDriftCycles, serviceName))
	}

	wr4ngler := loadWranglerBaseline(ctx, serviceName)
	if wr4ngler != nil && wr4ngler.LatencyThresholdMs > 0 {
		if currentMean > float64(wr4ngler.LatencyThresholdMs) {
			isActive, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertActive, serviceName))
			if isActive != "1" {
				severity := wranglerSeverity(wr4ngler.AlertLevel)
				msg := fmt.Sprintf("Latency SLA breach: %s current mean %.1fms exceeds Wr4ngler threshold %dms (level: %s)",
					serviceName, currentMean, wr4ngler.LatencyThresholdMs, wr4ngler.AlertLevel)
				fireAlert(ctx, serviceName, AlertLatencyDrift, severity, msg, now)
				log.Printf("[augur-canis] Wr4ngler latency alert: %s", msg)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Failure rate detector
// ---------------------------------------------------------------------------

func runFailureRateDetector() {
	log.Printf("[augur-canis] Failure rate detector active — interval %ds, threshold %.0f%%",
		detectorIntervalSeconds, failureRateThreshold*100)

	go func() {
		ticker := time.NewTicker(time.Duration(detectorIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			checkFailureRateAllServices()
		}
	}()
}

func checkFailureRateAllServices() {
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "ac:metrics:*:health")
	if err != nil {
		return
	}
	for _, key := range keys {
		serviceName := strings.TrimPrefix(key, "ac:metrics:")
		serviceName = strings.TrimSuffix(serviceName, ":health")
		checkFailureRate(ctx, serviceName)
	}
}

func checkFailureRate(ctx context.Context, serviceName string) {
	failures, total := readHealthWindow(ctx, serviceName)
	if total < minBaselineSamples {
		return
	}

	rate := float64(failures) / float64(total)
	now := time.Now().UTC().Format(time.RFC3339)

	if rate > failureRateThreshold {
		isActive, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertActive, serviceName))
		if isActive != "1" {
			msg := fmt.Sprintf("Failure rate: %s reporting %.0f%% failures (%d/%d checks)",
				serviceName, rate*100, failures, total)
			fireAlert(ctx, serviceName, AlertFailureRate, SeverityWarn, msg, now)
			log.Printf("[augur-canis] Failure rate alert: %s", msg)
		}
	}

	wr4ngler := loadWranglerBaseline(ctx, serviceName)
	if wr4ngler != nil && wr4ngler.FailureRateThresh > 0 {
		if rate > wr4ngler.FailureRateThresh {
			isActive, _, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertActive, serviceName))
			if isActive != "1" {
				severity := wranglerSeverity(wr4ngler.AlertLevel)
				msg := fmt.Sprintf("Failure rate SLA breach: %s at %.0f%% failures exceeds Wr4ngler threshold %.0f%% (level: %s)",
					serviceName, rate*100, wr4ngler.FailureRateThresh*100, wr4ngler.AlertLevel)
				fireAlert(ctx, serviceName, AlertFailureRate, severity, msg, now)
				log.Printf("[augur-canis] Wr4ngler failure rate alert: %s", msg)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Severity mapping
// ---------------------------------------------------------------------------

func wranglerSeverity(level string) Severity {
	switch strings.ToLower(level) {
	case "critical":
		return SeverityCritical
	case "high", "warn":
		return SeverityWarn
	default:
		return SeverityInfo
	}
}

// ---------------------------------------------------------------------------
// Admin API — Wr4ngler baseline management
// ---------------------------------------------------------------------------

func handleSetWranglerBaseline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	serviceName := strings.TrimPrefix(r.URL.Path, "/baselines/")
	serviceName = strings.TrimSuffix(serviceName, "/")
	if serviceName == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"code": "INVALID_REQUEST", "message": "service name required in path",
		})
		return
	}

	var req struct {
		LatencyThresholdMs int64   `json:"latency_threshold_ms"`
		FailureRateThresh  float64 `json:"failure_rate_threshold"`
		AlertLevel         string  `json:"alert_level"`
		SetBy              string  `json:"set_by"`
		Notes              string  `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": err.Error()})
		return
	}

	validLevels := map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
	if req.AlertLevel == "" || !validLevels[req.AlertLevel] {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "INVALID_REQUEST",
			"message": "alert_level must be critical | high | medium | low",
		})
		return
	}
	if req.LatencyThresholdMs <= 0 && req.FailureRateThresh <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "INVALID_REQUEST",
			"message": "at least one of latency_threshold_ms or failure_rate_threshold must be set",
		})
		return
	}
	if req.SetBy == "" {
		req.SetBy = "sec-wr4ngler"
	}

	baseline := WranglerBaseline{
		ServiceName:        serviceName,
		LatencyThresholdMs: req.LatencyThresholdMs,
		FailureRateThresh:  req.FailureRateThresh,
		AlertLevel:         req.AlertLevel,
		SetBy:              req.SetBy,
		SetAt:              time.Now().UTC().Format(time.RFC3339),
		Notes:              req.Notes,
	}

	ctx := context.Background()
	saveWranglerBaseline(ctx, baseline)

	log.Printf("[augur-canis] Wr4ngler baseline set for %s: latency=%dms failure_rate=%.0f%% level=%s by=%s",
		serviceName, req.LatencyThresholdMs, req.FailureRateThresh*100, req.AlertLevel, req.SetBy)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "set",
		"baseline": baseline,
	})
}

func handleGetWranglerBaseline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	serviceName := strings.TrimPrefix(r.URL.Path, "/baselines/")
	serviceName = strings.TrimSuffix(serviceName, "/")

	ctx := context.Background()
	auto := loadAutoBaseline(ctx, serviceName)
	wr4ngler := loadWranglerBaseline(ctx, serviceName)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"service_name":      serviceName,
		"auto_baseline":     auto,
		"wr4ngler_baseline": wr4ngler,
	})
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := context.Background()

	sinceMs := int64(0)
	if s := r.URL.Query().Get("since_ms"); s != "" {
		sinceMs, _ = strconv.ParseInt(s, 10, 64)
	}

	keys, err := rdb.Keys(ctx, "ac:metrics:*:latency")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"code": "REDIS_ERROR", "message": err.Error()})
		return
	}

	type ServiceMetrics struct {
		ServiceName      string            `json:"service_name"`
		Samples          []MetricSample    `json:"samples"`
		AutoBaseline     *AutoBaseline     `json:"auto_baseline,omitempty"`
		WranglerBaseline *WranglerBaseline `json:"wr4ngler_baseline,omitempty"`
	}

	result := make([]ServiceMetrics, 0, len(keys))
	for _, key := range keys {
		serviceName := strings.TrimPrefix(key, "ac:metrics:")
		serviceName = strings.TrimSuffix(serviceName, ":latency")

		samples := ReadMetricsWindow(ctx, serviceName, sinceMs)
		sm := ServiceMetrics{
			ServiceName:      serviceName,
			Samples:          samples,
			AutoBaseline:     loadAutoBaseline(ctx, serviceName),
			WranglerBaseline: loadWranglerBaseline(ctx, serviceName),
		}
		result = append(result, sm)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"services":   result,
		"total":      len(result),
		"since_ms":   sinceMs,
		"fetched_at": time.Now().UnixMilli(),
	})
}

func handleListBaselines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := context.Background()

	autoKeys, _ := rdb.Keys(ctx, "ac:baseline:auto:*")
	wr4nglerKeys, _ := rdb.Keys(ctx, "ac:baseline:wr4ngler:*")

	seen := map[string]bool{}
	services := []string{}
	for _, k := range autoKeys {
		svc := strings.TrimPrefix(k, "ac:baseline:auto:")
		if !seen[svc] {
			seen[svc] = true
			services = append(services, svc)
		}
	}
	for _, k := range wr4nglerKeys {
		svc := strings.TrimPrefix(k, "ac:baseline:wr4ngler:")
		if !seen[svc] {
			seen[svc] = true
			services = append(services, svc)
		}
	}

	type BaselineSummary struct {
		ServiceName      string            `json:"service_name"`
		AutoBaseline     *AutoBaseline     `json:"auto_baseline"`
		WranglerBaseline *WranglerBaseline `json:"wr4ngler_baseline"`
	}

	summaries := []BaselineSummary{}
	for _, svc := range services {
		summaries = append(summaries, BaselineSummary{
			ServiceName:      svc,
			AutoBaseline:     loadAutoBaseline(ctx, svc),
			WranglerBaseline: loadWranglerBaseline(ctx, svc),
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"baselines": summaries,
		"total":     len(summaries),
	})
}
