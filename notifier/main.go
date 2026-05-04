package main

// ---------------------------------------------------------------------------
// Notifier — wakes a human. That's it.
//
// This is a PERMANENT STUB in the open source distribution.
// If you cloned this repository, you must replace the handler body in
// handleNotify() with a call to your notification system before deploying
// to production.
//
// The contract is what you implement against. See contracts/openapi/notifier.yaml.
//
// What goes here:
//   - PagerDuty Events API v2
//   - Twilio SMS
//   - Microsoft Teams incoming webhook
//   - AWS SNS publish
//   - Slack webhook
//   - Whatever wakes your people
//
// What does NOT go here:
//   - Routing logic (that belongs in Interactions)
//   - Severity thresholds (that belongs in Interactions)
//   - AI analysis (that belongs in AI-lien)
//   - Incident context (travels through secure channels, not this payload)
//
// The Notifier is dumb by design. Keep it that way.
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

var (
	port             = envOr("PORT", "4300")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	notificationsAccepted  atomic.Int64
	notificationsDelivered atomic.Int64 // always 0 in stub
	startTime              = time.Now()
)

// ---------------------------------------------------------------------------
// mTLS
// ---------------------------------------------------------------------------

var certMat *CertMaterial
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
			"caller": "notifier", "callee": callee,
			"method": method, "path": path,
			"status_code": status, "latency_ms": latencyMs, "protocol": "mtls",
		})
		req, err := http.NewRequest(http.MethodPost, observabilityURL+"/event",
			strings.NewReader(string(body)))
		if err != nil {
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
// Handlers
// ---------------------------------------------------------------------------

func handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()

	var req struct {
		Prompt        string `json:"prompt"`
		Severity      string `json:"severity"`
		ApplicationID string `json:"application_id"`
		EscalationID  string `json:"escalation_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	if req.Prompt == "" || req.Severity == "" || req.ApplicationID == "" || req.EscalationID == "" {
		errJSON(w, http.StatusBadRequest, "INVALID_REQUEST",
			"prompt, severity, application_id, and escalation_id are required")
		return
	}
	if len(req.Prompt) > 500 {
		errJSON(w, http.StatusBadRequest, "INVALID_REQUEST",
			"prompt must not exceed 500 characters")
		return
	}

	notificationsAccepted.Add(1)
	notificationID := fmt.Sprintf("notif-%d", time.Now().UnixNano())

	// ---------------------------------------------------------------------------
	// STUB — replace this block with your notification mechanism.
	//
	// The req struct contains everything you need:
	//   req.Prompt        — the notification text to send
	//   req.Severity      — critical | high | medium | low
	//   req.ApplicationID — which constellation application triggered this
	//   req.EscalationID  — for correlation back to Lore
	//
	// Example (PagerDuty):
	//   payload := map[string]interface{}{
	//     "routing_key":  os.Getenv("PAGERDUTY_ROUTING_KEY"),
	//     "event_action": "trigger",
	//     "payload": map[string]interface{}{
	//       "summary":  req.Prompt,
	//       "severity": req.Severity,
	//       "source":   req.ApplicationID,
	//       "custom_details": map[string]string{
	//         "escalation_id": req.EscalationID,
	//       },
	//     },
	//   }
	//   // POST payload to https://events.pagerduty.com/v2/enqueue
	//
	// When your implementation delivers successfully, increment:
	//   notificationsDelivered.Add(1)
	//
	// ---------------------------------------------------------------------------
	log.Printf("[notifier] STUB: notification accepted but NOT delivered — implement handleNotify() for your environment")
	log.Printf("[notifier] STUB: severity=%s app=%s escalation=%s prompt=%q",
		req.Severity, req.ApplicationID, req.EscalationID, req.Prompt)

	reportEvent("notification-system", "POST", "/notify", 202, time.Since(start).Milliseconds())

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"notification_id": notificationID,
		"stub_active":     true,
		"accepted_at":     time.Now().UTC().Format(time.RFC3339),
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":                   "healthy",
		"stub_active":              true,
		"notifications_accepted":   notificationsAccepted.Load(),
		"notifications_delivered":  notificationsDelivered.Load(),
		"uptime_seconds":           int(time.Since(startTime).Seconds()),
	})
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	certMat = obtainCerts("notifier")
	buildUpstreamClient()
	go selfRegisterWithAC(certMat, "https://notifier:4300")

	mux := http.NewServeMux()
	mux.HandleFunc("/notify", handleNotify)
	mux.HandleFunc("/health", handleHealth)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: buildServerTLS(certMat),
	}

	log.Printf("[notifier] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[notifier] STUB ACTIVE — implement handleNotify() for your environment")
	log.Printf("[notifier] See contracts/openapi/notifier.yaml for the contract")
	log.Printf("[notifier] See comments in handleNotify() for implementation guidance")

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[notifier] Server error: %v", err)
		}
	}()
	awaitShutdown(server)
}
