package main

// ---------------------------------------------------------------------------
// Interactions Job — triage router for SETI failure escalations.
//
// Three escalation paths:
//
//   Path 1 — Immediate (critical failure rate exceeded):
//     Fire Notifier and AI-lien critical briefing IN PARALLEL.
//     AI-lien mode: "this is bad, assemble the fullest possible context
//     package for the human responding now." Not a diagnosis — a briefing.
//     Write incident to Lore immediately. Log notification in Lore.
//
//   Path 2 — AI Analysis (partial failure, no clear pattern):
//     Send context package to AI-lien for structured assessment.
//     AI-lien returns recommended_action. If action is "escalate_to_wr4ngler",
//     Interactions calls Notifier and logs the decision. Write trend point
//     and/or incident to Lore based on assessment.
//
//   Path 3 — Silence (single intermittent, clean history):
//     Log to bad-whiff buffer only (already done by Signal Aggregator).
//     No AI call, no notification. Let the pattern emerge.
//
// Triage thresholds:
//   criticalFailureThreshold — fraction of tests failing that triggers Path 1.
//   Default 0.5 (50%). Hardcoded — change requires code review and rebuild.
//   This friction is intentional: threshold changes are architectural decisions.
//
//   silenceHistoryHours — hours of clean Lore history required to silence a
//   single failure. Default 168 (7 days). Same deliberate friction.
//
// Authorized callers: Contract Test Job, Plot Test Job.
// Authorized callees: AI-lien, Notifier, Lore, Results, Observability.
// AI-lien never calls Notifier directly — all Notifier calls go through here.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4009")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	ailienURL        = envOr("AI_LIEN_URL", "https://ai-lien:4252")
	notifierURL      = envOr("NOTIFIER_URL", "https://notifier:4300")
	loreURL          = envOr("LORE_URL", "https://lore:4110")
	resultsURL       = envOr("RESULTS_URL", "https://results:4008")
)

// Triage thresholds — hardcoded by design. Change requires code review + rebuild.
const (
	criticalFailureThreshold = 0.5 // >= 50% failing → Path 1 immediate escalation
	silenceHistoryHours      = 168 // 7 days clean Lore history → silence single failures
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	escalationsReceived   atomic.Int64
	path1Immediate        atomic.Int64
	path2AIAnalysis       atomic.Int64
	path3Silence          atomic.Int64
	notificationsFired    atomic.Int64
	regenerationsReceived atomic.Int64
	startTime             = time.Now()
)

// ---------------------------------------------------------------------------
// mTLS
// ---------------------------------------------------------------------------

var (
	certMat        *CertMaterial
	upstreamClient *http.Client
)

func buildUpstreamClient() {
	upstreamClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: buildClientTLS(certMat),
		},
	}
}

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller": "interactions", "callee": callee,
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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// ---------------------------------------------------------------------------
// Upstream helpers
// ---------------------------------------------------------------------------

func postJSON(url string, body interface{}) (int, map[string]interface{}, error) {
	start := time.Now()
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(payload))

	resp, err := upstreamClient.Do(req)
	latencyMs := time.Since(start).Milliseconds()

	parts := strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)
	callee := parts[0]
	path := "/"
	if len(parts) > 1 {
		path = "/" + parts[1]
	}
	if err != nil {
		reportEvent(callee, "POST", path, 0, latencyMs)
		return 0, nil, err
	}
	defer resp.Body.Close()
	reportEvent(callee, "POST", path, resp.StatusCode, latencyMs)

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return resp.StatusCode, result, nil
}

func getJSON(url string) (int, map[string]interface{}, error) {
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := upstreamClient.Do(req)
	latencyMs := time.Since(start).Milliseconds()

	parts := strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)
	callee := parts[0]
	path := "/"
	if len(parts) > 1 {
		path = "/" + parts[1]
	}
	if err != nil {
		reportEvent(callee, "GET", path, 0, latencyMs)
		return 0, nil, err
	}
	defer resp.Body.Close()
	reportEvent(callee, "GET", path, resp.StatusCode, latencyMs)

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return resp.StatusCode, result, nil
}

// ---------------------------------------------------------------------------
// Triage
// ---------------------------------------------------------------------------

// EscalationPayload is the structured incoming escalation request.
type EscalationPayload struct {
	RunID         string `json:"run_id"`
	ApplicationID string `json:"application_id"`
	TestTier      string `json:"test_tier"`  // "contract" | "plot"
	TotalTests    int    `json:"total_tests"`
	FailedTests   int    `json:"failed_tests"`
	PassedTests   int    `json:"passed_tests"`
	SkippedTests  int    `json:"skipped_tests"`
}

func triageEscalation(p EscalationPayload) {
	escalationID := fmt.Sprintf("esc-%d", time.Now().UnixNano())
	log.Printf("[interactions] Triage: run=%s app=%s tier=%s failed=%d/%d id=%s",
		p.RunID, p.ApplicationID, p.TestTier, p.FailedTests, p.TotalTests, escalationID)

	failureRate := 0.0
	if p.TotalTests > 0 {
		failureRate = float64(p.FailedTests) / float64(p.TotalTests)
	}

	// -------------------------------------------------------------------
	// Path 1 — Immediate
	// -------------------------------------------------------------------
	if failureRate >= criticalFailureThreshold {
		path1Immediate.Add(1)
		log.Printf("[interactions] Path 1 (immediate): %.0f%% failing — Notifier + AI briefing in parallel",
			failureRate*100)

		done := make(chan struct{}, 2)

		go func() {
			defer func() { done <- struct{}{} }()
			prompt := fmt.Sprintf("SETI: Critical failure in %s. %d/%d %s tests failing. AI briefing in progress. Run: %s",
				p.ApplicationID, p.FailedTests, p.TotalTests, p.TestTier, p.RunID)
			status, _, err := postJSON(notifierURL+"/notify", map[string]interface{}{
				"prompt":         prompt,
				"severity":       "critical",
				"application_id": p.ApplicationID,
				"escalation_id":  escalationID,
			})
			if err != nil {
				log.Printf("[interactions] Notifier call failed: %v", err)
			} else {
				notificationsFired.Add(1)
				log.Printf("[interactions] Notifier fired: status=%d", status)
			}
		}()

		go func() {
			defer func() { done <- struct{}{} }()
			callAILien(p, escalationID, true)
		}()

		<-done
		<-done
		return
	}

	// -------------------------------------------------------------------
	// Path 3 — Silence (check before Path 2 to avoid unnecessary AI calls)
	// -------------------------------------------------------------------
	if p.FailedTests == 1 && hasCleanLoreHistory(p.ApplicationID, silenceHistoryHours) {
		path3Silence.Add(1)
		log.Printf("[interactions] Path 3 (silence): single failure with %dh clean history — no escalation",
			silenceHistoryHours)
		return
	}

	// -------------------------------------------------------------------
	// Path 2 — AI Analysis
	// -------------------------------------------------------------------
	path2AIAnalysis.Add(1)
	log.Printf("[interactions] Path 2 (AI analysis): %d/%d failing — sending to AI-lien",
		p.FailedTests, p.TotalTests)
	callAILien(p, escalationID, false)
}

// hasCleanLoreHistory returns true if no open incidents exist for this
// application in the past N hours.
func hasCleanLoreHistory(applicationID string, hours int) bool {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)
	url := fmt.Sprintf("%s/incidents?application_id=%s&status=open&since=%s&limit=1",
		loreURL, applicationID, since)
	status, result, err := getJSON(url)
	if err != nil || status != 200 {
		log.Printf("[interactions] Lore history check failed (status=%d err=%v) — not silencing", status, err)
		return false
	}
	total, _ := result["total"].(float64)
	return total == 0
}

// callAILien sends the failure context to AI-lien for analysis.
// If critical=true, uses the critical_briefing mode — assemble context
// for the human already on their way, not a diagnosis.
func callAILien(p EscalationPayload, escalationID string, critical bool) {
	analysisType := "failure_analysis"
	if critical {
		analysisType = "critical_briefing"
	}

	runDetail := fetchRunDetail(p.RunID, p.TestTier)
	loreContext := fetchLoreContext(p.ApplicationID)

	contextPackage := map[string]interface{}{
		"escalation_id":    escalationID,
		"run_id":           p.RunID,
		"application_id":   p.ApplicationID,
		"test_tier":        p.TestTier,
		"failed_tests":     p.FailedTests,
		"total_tests":      p.TotalTests,
		"failure_rate":     fmt.Sprintf("%.0f%%", float64(p.FailedTests)/float64(p.TotalTests)*100),
		"run_detail":       runDetail,
		"lore_context":     loreContext,
		"critical_briefing": critical,
	}

	responseSchema := map[string]interface{}{
		"root_cause":           "string — most likely cause",
		"category":             "contract_violation | network_error | implementation_bug | timing_issue | unknown",
		"confidence":           "high | medium | low",
		"investigation_steps": []string{"ordered steps to investigate"},
		"recommended_action":  "string — specific next action",
		"escalate_to_wr4ngler": "boolean — true if human involvement needed",
		"severity":             "critical | high | medium | low",
	}
	if critical {
		responseSchema["briefing_summary"] = "string — situation summary for the responding Wr4ngler"
		responseSchema["immediate_steps"]  = []string{"ordered actions for the responding human right now"}
	}

	status, result, err := postJSON(ailienURL+"/analyze", map[string]interface{}{
		"analysis_type":   analysisType,
		"package":         contextPackage,
		"response_schema": responseSchema,
		"run_id":          p.RunID,
	})

	if err != nil || status != 200 {
		log.Printf("[interactions] AI-lien call failed: status=%d err=%v — writing fallback trend point", status, err)
		postJSON(loreURL+"/trend-points", map[string]interface{}{
			"application_id": p.ApplicationID,
			"job_name":       p.TestTier + "-tests",
			"source_job":     "interactions",
			"signal_type":    "ai_lien_unavailable",
			"description": fmt.Sprintf("%d/%d %s tests failing — AI-lien unavailable, manual review required",
				p.FailedTests, p.TotalTests, p.TestTier),
			"evidence": map[string]interface{}{
				"escalation_id": escalationID,
				"run_id":        p.RunID,
				"failed_tests":  p.FailedTests,
				"total_tests":   p.TotalTests,
			},
			"run_id": p.RunID,
		})
		return
	}

	assessment, _ := result["assessment"].(map[string]interface{})
	if assessment == nil {
		log.Printf("[interactions] AI-lien returned no assessment for run %s", p.RunID)
		return
	}

	rootCause, _ := assessment["root_cause"].(string)
	recommendedAction, _ := assessment["recommended_action"].(string)
	confidence, _ := assessment["confidence"].(string)
	category, _ := assessment["category"].(string)
	severity, _ := assessment["severity"].(string)
	escalateToWrangler, _ := assessment["escalate_to_wr4ngler"].(bool)

	if severity == "" {
		severity = "medium"
	}
	if rootCause == "" {
		rootCause = "unknown — assessment incomplete"
	}
	if recommendedAction == "" {
		recommendedAction = "manual investigation required"
	}

	log.Printf("[interactions] AI-lien assessment: category=%s confidence=%s escalate=%v",
		category, confidence, escalateToWrangler)

	// If AI-lien recommends escalation and we haven't already paged (Path 2 only)
	if escalateToWrangler && !critical {
		prompt := fmt.Sprintf("SETI: %s degradation in %s. AI assessment: %s. Wr4ngler review requested. Run: %s",
			strings.ToUpper(severity), p.ApplicationID, rootCause, p.RunID)
		notifyStatus, _, notifyErr := postJSON(notifierURL+"/notify", map[string]interface{}{
			"prompt":         prompt,
			"severity":       severity,
			"application_id": p.ApplicationID,
			"escalation_id":  escalationID,
		})
		if notifyErr != nil {
			log.Printf("[interactions] Notifier call failed: %v", notifyErr)
		} else {
			notificationsFired.Add(1)
			log.Printf("[interactions] Notifier fired on AI recommendation: status=%d", notifyStatus)
		}
	}

	// Write incident to Lore
	aiAssessment := fmt.Sprintf("Category: %s | Confidence: %s | %s", category, confidence, rootCause)
	_, _, loreErr := postJSON(loreURL+"/incidents", map[string]interface{}{
		"application_id":          p.ApplicationID,
		"job_name":                p.TestTier + "-tests",
		"source_job":              "interactions",
		"trigger_type":            "test_failure",
		"description":             fmt.Sprintf("%d/%d %s tests failing", p.FailedTests, p.TotalTests, p.TestTier),
		"ai_assessment":           aiAssessment,
		"ai_recommended_action":   recommendedAction,
		"run_id":                  p.RunID,
		"related_trend_point_ids": []string{},
	})
	if loreErr != nil {
		log.Printf("[interactions] Lore incident write failed: %v", loreErr)
	} else {
		log.Printf("[interactions] Lore incident written: run=%s app=%s", p.RunID, p.ApplicationID)
	}
}

// fetchRunDetail retrieves the full run result from Results for AI context.
func fetchRunDetail(runID, testTier string) map[string]interface{} {
	var url string
	if testTier == "plot" {
		url = fmt.Sprintf("%s/plot-results/%s", resultsURL, runID)
	} else {
		url = fmt.Sprintf("%s/contract-results/%s", resultsURL, runID)
	}
	status, result, err := getJSON(url)
	if err != nil || status != 200 {
		return map[string]interface{}{"error": "could not fetch run detail", "run_id": runID}
	}
	return result
}

// fetchLoreContext retrieves recent incidents and patterns for AI context.
func fetchLoreContext(applicationID string) map[string]interface{} {
	since := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	ctx := map[string]interface{}{}
	if _, incidents, err := getJSON(
		fmt.Sprintf("%s/incidents?application_id=%s&since=%s&limit=5", loreURL, applicationID, since)); err == nil {
		ctx["recent_incidents"] = incidents
	}
	if _, patterns, err := getJSON(
		fmt.Sprintf("%s/patterns?application_id=%s&limit=5", loreURL, applicationID)); err == nil {
		ctx["known_patterns"] = patterns
	}
	return ctx
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func handleEscalate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}

	var payload EscalationPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	if payload.RunID == "" || payload.ApplicationID == "" {
		errJSON(w, http.StatusBadRequest, "INVALID_REQUEST",
			"run_id and application_id are required")
		return
	}

	escalationsReceived.Add(1)
	escalationID := fmt.Sprintf("esc-%d", time.Now().UnixNano())

	// Accept immediately — triage runs in background
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":        "accepted",
		"escalation_id": escalationID,
		"run_id":        payload.RunID,
		"accepted_at":   time.Now().UTC().Format(time.RFC3339),
	})

	go triageEscalation(payload)
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
	log.Printf("[interactions] Regeneration advisory: plot=%s reason=%s", plotID, reason)

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":          "accepted",
		"notification_id": fmt.Sprintf("notif-%d", time.Now().UnixNano()),
		"plot_id":         plotID,
		"accepted_at":     time.Now().UTC().Format(time.RFC3339),
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":                 "healthy",
		"escalations_received":   escalationsReceived.Load(),
		"path1_immediate":        path1Immediate.Load(),
		"path2_ai_analysis":      path2AIAnalysis.Load(),
		"path3_silence":          path3Silence.Load(),
		"notifications_fired":    notificationsFired.Load(),
		"regenerations_received": regenerationsReceived.Load(),
		"thresholds": map[string]interface{}{
			"critical_failure_rate": criticalFailureThreshold,
			"silence_history_hours": silenceHistoryHours,
		},
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func loadServerTLS() *tls.Config {
	return buildServerTLS(certMat)
}

func main() {
	certMat = obtainCerts("interactions")
	go selfRegisterWithAC(certMat, "https://interactions:4009")
	buildUpstreamClient()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/escalate", handleEscalate)
	mux.HandleFunc("/notify/regeneration", handleRegeneration)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadServerTLS(),
	}

	log.Printf("[interactions] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[interactions] Critical threshold: %.0f%% | Silence history: %dh",
		criticalFailureThreshold*100, silenceHistoryHours)
	log.Printf("[interactions] AI-lien: %s | Notifier: %s | Lore: %s",
		ailienURL, notifierURL, loreURL)

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[interactions] %v", err)
	}
}
