package main

// ---------------------------------------------------------------------------
// federation.go — Signal Aggregator Federation
//
// Signal-aggregator is a pure stream consumer in the TCA Federation Protocol.
// It does NOT perform the three-message handshake — that is connie-agent's
// responsibility.
//
// connie-agent performs the handshake with Target AC and then calls
// POST /federation/subscriptions on this service with the resulting
// session material:
//   - ac_endpoint        — where to connect the SSE stream
//   - session_cert       — PEM of AC's session cert (trusted root for TLS)
//   - monitor_cert_pem   — PEM of connie-agent's instance cert (our client cert)
//   - monitor_key_pem    — PEM of connie-agent's instance private key (our client key)
//   - session_cert_id    — fingerprint for event signature verification
//
// Signal-aggregator builds an mTLS client from that material and connects
// to GET /stream on the Target AC.  It maintains that connection, forwarding
// events into the rolling window, and notifies connie-agent if the stream
// drops so connie-agent can re-run the handshake.
// ---------------------------------------------------------------------------

import (
	"bufio"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// validateHTTPSURL returns an error if raw is not a valid https URL.
// All inter-service endpoints in TCA are mTLS/HTTPS — any other scheme
// is rejected at intake so it never reaches an outgoing HTTP call.
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

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type FederatedApp struct {
	AppID          string
	ACEndpoint     string
	CACert         string            // Vox CA PEM — trusted root for AC's mTLS cert
	SessionCert    *x509.Certificate // trusted root for event signature verification
	SessionCertID  string
	MonitorCert    tls.Certificate   // our mTLS client cert+key
	RegisteredAt   time.Time
	LastEventAt    time.Time
	ReconnectCount int
	Status         string
	EventsVerified int64
	cancelMonitor  context.CancelFunc
	done           chan struct{} // closed when all goroutines have exited
}

// ---------------------------------------------------------------------------
// Global federation state
// ---------------------------------------------------------------------------

var (
	fedMu   sync.RWMutex
	fedApps = map[string]*FederatedApp{}

	setiInstanceID      = envOr("SETI_INSTANCE_ID", "tca-seti-primary")
	silenceThresholdSec = envOrInt("SILENCE_THRESHOLD_SECONDS", 90)
)

func envOrInt(key string, def int) int {
	v := envOr(key, "")
	if v == "" {
		return def
	}
	var n int
	fmt.Sscanf(v, "%d", &n)
	if n > 0 {
		return n
	}
	return def
}

// ---------------------------------------------------------------------------
// Startup
// ---------------------------------------------------------------------------

func initFederationOnStartup() {
	// Federation is driven entirely by connie-agent via POST /federation/subscriptions.
	// Signal-aggregator waits to be told which constellations to connect to
	// and receives fully authenticated session material for each one.
	log.Printf("[federation] Ready — waiting for constellation federation requests from connie-agent")
}

// ---------------------------------------------------------------------------
// Connect — called after connie-agent completes the three-message handshake
// ---------------------------------------------------------------------------

// connectFederatedApp registers a constellation and starts the SSE stream.
// All session material is provided by connie-agent — no handshake happens here.
func connectFederatedApp(appID, acEndpoint, caCert string, sessionCert *x509.Certificate,
	sessionCertID string, monitorCert tls.Certificate) error {

	fedMu.Lock()
	existing, exists := fedApps[appID]
	if exists && existing.cancelMonitor != nil {
		existing.cancelMonitor()
		oldDone := existing.done
		fedMu.Unlock()
		// Wait for old goroutines to fully exit before starting new ones
		if oldDone != nil {
			select {
			case <-oldDone:
			case <-time.After(5 * time.Second):
				log.Printf("[federation] Timed out waiting for old goroutines to exit for %s", appID)
			}
		}
		fedMu.Lock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	app := &FederatedApp{
		AppID:         appID,
		ACEndpoint:    acEndpoint,
		CACert:        caCert,
		SessionCert:   sessionCert,
		SessionCertID: sessionCertID,
		MonitorCert:   monitorCert,
		RegisteredAt:  time.Now(),
		LastEventAt:   time.Now(),
		Status:        "active",
		cancelMonitor: cancel,
		done:          done,
	}
	if exists {
		app.ReconnectCount = existing.ReconnectCount + 1
	}
	fedApps[appID] = app
	fedMu.Unlock()

	log.Printf("[federation] Constellation connected app=%s endpoint=%s session=%s reconnects=%d monitorCertLeaves=%d",
		appID, acEndpoint, sessionCertID, app.ReconnectCount, len(app.MonitorCert.Certificate))

	go func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); runFederatedFeed(ctx, app) }()
		go func() { defer wg.Done(); runSilenceMonitor(ctx, appID) }()
		wg.Wait()
		close(done)
		log.Printf("[federation] All goroutines exited for %s", appID)
	}()

	// Announce new constellation to gateway so it subscribes to the federation channel.
	announceFederation(appID, acEndpoint)

	return nil
}

// announceFederation publishes a constellation announcement to seti:federation:announce.
// The gateway subscribes to this channel and dynamically adds a subscription
// to federation:{appID}:events when it receives an announcement.
// handleFederationProxy proxies requests to a remote constellation's AC
// using the established monitor cert mTLS connection.
// Routes:
//   POST /federation/proxy/{tag}/run-contract-tests
//   GET  /federation/proxy/{tag}/contract-suites
//   GET  /federation/proxy/{tag}/contract-suites/{runId}
//   POST /federation/proxy/{tag}/plots/run
//
// Adding a route here requires a Contract change and full revalidation —
// this allowlist is an intentional security checkpoint, not a maintenance burden.
func handleFederationProxy(w http.ResponseWriter, r *http.Request) {
	// Parse /federation/proxy/{tag}/{action...}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/federation/proxy/"), "/", 2)
	if len(parts) < 2 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing action"})
		return
	}
	tag, action := parts[0], "/"+parts[1]

	// Allowlist of permitted remote AC actions.  Extending this list requires
	// a Contract update and revalidation — do not add entries without that process.
	allowedActions := map[string]bool{
		"/run-contract-tests": true,
		"/contract-suites":    true,
		"/plots/run":          true,
	}
	// contract-suites/{runId} — allow any path rooted at /contract-suites/
	if !allowedActions[action] && !strings.HasPrefix(action, "/contract-suites/") {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "action not permitted"})
		return
	}

	fedMu.RLock()
	app, ok := fedApps[tag]
	fedMu.RUnlock()

	if !ok || app.Status != "active" {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "constellation not active"})
		return
	}

	// Build TLS client using monitor cert (trusted by remote AC) and Vox CA
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM([]byte(app.CACert))

	log.Printf("[federation] proxy: using monitor cert leaves=%d caPool=%v for %s",
		len(app.MonitorCert.Certificate), caPool != nil, tag)

	client := buildConstellationClient(app)

	target := app.ACEndpoint + action
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "could not build request"})
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[federation] proxy error %s %s: %v", r.Method, target, err)
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func announceFederation(appID, acEndpoint string) {
	if localRDB == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"application_id": appID,
		"channel":        "federation:" + appID + ":events",
		"ac_endpoint":    acEndpoint,
	})
	if err := localRDB.Publish(context.Background(), "seti:federation:announce", string(payload)); err != nil {
		log.Printf("[federation] Failed to announce constellation %s: %v", appID, err)
	} else {
		log.Printf("[federation] Announced constellation %s to gateway", appID)
	}
}

// buildConstellationClient builds an mTLS client that:
//   - presents MonitorCert as the client certificate (SETI instance cert)
//   - trusts SessionCert's CA chain for server cert verification
func buildConstellationClient(app *FederatedApp) *http.Client {
	pool := x509.NewCertPool()
	if app.CACert != "" {
		pool.AppendCertsFromPEM([]byte(app.CACert))
	}
	if app.SessionCert != nil {
		pool.AddCert(app.SessionCert)
	}

	monitorCert := app.MonitorCert
	tlsConfig := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS13,
		// GetClientCertificate forces the cert to be presented regardless of
		// what CAs the server advertises as acceptable — necessary because AC's
		// ClientCAs pool doesn't contain SETI's CA, so Go would otherwise
		// silently skip presenting the client cert.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &monitorCert, nil
		},
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}
}

// ---------------------------------------------------------------------------
// SSE stream
// ---------------------------------------------------------------------------

func runFederatedFeed(ctx context.Context, app *FederatedApp) {
	streamURL := strings.TrimRight(app.ACEndpoint, "/") + "/stream"

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := connectSSEStream(ctx, app, streamURL); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[federation] SSE stream lost for %s: %v — waiting for connie-agent re-handshake",
				app.AppID, err)

			// Mark degraded and wait — connie-agent's silence monitor will
			// detect the drop and trigger a re-handshake
			fedMu.Lock()
			if a, ok := fedApps[app.AppID]; ok {
				a.Status = "degraded"
			}
			fedMu.Unlock()

			// Back off before retrying — connie-agent will push new session material
			time.Sleep(10 * time.Second)
		}
	}
}

func connectSSEStream(ctx context.Context, app *FederatedApp, url string) error {
	client := buildConstellationClient(app)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("AC /stream returned %d", resp.StatusCode)
	}

	log.Printf("[federation] SSE stream connected to %s for %s", url, app.AppID)

	// Update status to active on successful connect
	fedMu.Lock()
	if a, ok := fedApps[app.AppID]; ok {
		a.Status = "active"
	}
	fedMu.Unlock()

	scanner := newSSEScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line := scanner.Text()
		if line != "" {
			log.Printf("[federation] SSE line from %s: %s", app.AppID, line[:min(len(line), 80)])
		}
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload != "" {
				processFederatedEvent(app.AppID, payload)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %v", err)
	}
	return fmt.Errorf("stream closed by server")
}

// ---------------------------------------------------------------------------
// Event processing
// ---------------------------------------------------------------------------

func processFederatedEvent(appID, payload string) {
	var envelope map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		log.Printf("[federation] Could not parse feed event for %s: %v", appID, err)
		return
	}

	certID, _ := envelope["session_cert_id"].(string)
	sig, _ := envelope["signature"].(string)

	if certID != "" && sig != "" {
		if err := verifyFeedEvent(appID, certID, sig, payload); err != nil {
			log.Printf("[federation] Signature verification FAILED for %s (cert=%s): %v — skipping event", appID, certID, err)
			return
		}
		fedMu.Lock()
		if app, ok := fedApps[appID]; ok {
			app.LastEventAt = time.Now()
			app.EventsVerified++
			if app.EventsVerified%10 == 1 {
				log.Printf("[federation] Verified event #%d from %s (cert=%s)",
					app.EventsVerified, appID, certID)
			}
		}
		fedMu.Unlock()
	} else {
		fedMu.Lock()
		if app, ok := fedApps[appID]; ok {
			app.LastEventAt = time.Now()
		}
		fedMu.Unlock()
	}

	// Unwrap the AC stream envelope — event data is in the "payload" field
	eventData := envelope
	if inner, ok := envelope["payload"]; ok {
		if innerMap, ok := inner.(map[string]interface{}); ok {
			eventData = innerMap
		}
	}

	// Observability event — has caller/callee fields
	caller, _ := eventData["caller"].(string)
	callee, _ := eventData["callee"].(string)
	if caller != "" && callee != "" {
		method, _ := eventData["method"].(string)
		path, _ := eventData["path"].(string)
		statusCode := 200
		if v, ok := eventData["status_code"].(float64); ok {
			statusCode = int(v)
		}
		latencyMs := int64(0)
		if v, ok := eventData["latency_ms"].(float64); ok {
			latencyMs = int64(v)
		}
		protocol, _ := eventData["protocol"].(string)
		if protocol == "" {
			protocol = "mtls"
		}
		publishFederatedEvent(appID, map[string]interface{}{
			"application_id": appID,
			"caller":         caller,
			"callee":         callee,
			"method":         method,
			"path":           path,
			"status_code":    statusCode,
			"latency_ms":     latencyMs,
			"protocol":       protocol,
			"timestamp":      time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	// Contract test result event
	eventType, _ := envelope["type"].(string)
	if eventType == "contract_result" {
		publishFederatedEvent(appID, map[string]interface{}{
			"application_id": appID,
			"type":           "contract_result",
			"payload":        eventData,
			"timestamp":      time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	// Health check event — has service_name field
	serviceName, _ := eventData["service_name"].(string)
	if serviceName == "" {
		log.Printf("[federation] Skipping event from %s — no caller/callee or service_name in payload", appID)
		return
	}
	healthyBool, _ := eventData["healthy"].(bool)
	healthyNum, _ := eventData["healthy"].(float64)
	isHealthy := healthyBool || healthyNum == 1
	status := 200
	if !isHealthy {
		status = 503
	}
	publishFederatedEvent(appID, map[string]interface{}{
		"application_id": appID,
		"caller":         "augur-canis",
		"callee":         serviceName,
		"method":         "POST",
		"path":           "/check",
		"status_code":    status,
		"latency_ms":     0,
		"protocol":       "sse",
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
	})
}

var (
	fedEventCount   int
	fedEventCountMu sync.Mutex
	fedEventLastLog time.Time
)

func publishFederatedEvent(appID string, enriched map[string]interface{}) {
	fedEventCountMu.Lock()
	fedEventCount++
	count := fedEventCount
	if time.Since(fedEventLastLog) > 5*time.Second {
		log.Printf("[federation] publishFederatedEvent rate: %d events in last 5s for %s", count, appID)
		fedEventCount = 0
		fedEventLastLog = time.Now()
	}
	fedEventCountMu.Unlock()
	ev := ConstellationEvent{
		ApplicationID: appID,
		ReceivedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	if v, ok := enriched["caller"].(string); ok {
		ev.Caller = v
	}
	if v, ok := enriched["callee"].(string); ok {
		ev.Callee = v
	}
	if v, ok := enriched["method"].(string); ok {
		ev.Method = v
	}
	if v, ok := enriched["path"].(string); ok {
		ev.Path = v
	}
	if v, ok := enriched["status_code"].(int); ok {
		ev.StatusCode = v
	}
	if v, ok := enriched["latency_ms"].(int64); ok {
		ev.LatencyMs = v
	}
	if v, ok := enriched["protocol"].(string); ok {
		ev.Protocol = v
	}

	appendEvent(appID, ev)

	eventJSON, err := json.Marshal(ev)
	if err != nil {
		log.Printf("[federation] Could not marshal event for %s: %v", appID, err)
		return
	}
	channel := "federation:" + appID + ":events"
	if localRDB != nil {
		if err := localRDB.Publish(context.Background(), channel, string(eventJSON)); err != nil {
			log.Printf("[federation] Failed to publish to %s: %v", channel, err)
		}
	}
}

func verifyFeedEvent(appID, certID, sig, fullPayload string) error {
	fedMu.RLock()
	app, ok := fedApps[appID]
	fedMu.RUnlock()

	if !ok {
		return fmt.Errorf("app %s not registered", appID)
	}

	if app.SessionCertID != certID {
		return fmt.Errorf("cert ID mismatch: have %s, event has %s — AC may have restarted",
			app.SessionCertID, certID)
	}

	// broadcastToStream signs json.Marshal(inner payload) — not the outer envelope.
	// Extract the raw payload bytes and hash those.
	var envelope struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(fullPayload), &envelope); err != nil {
		return fmt.Errorf("parse envelope: %v", err)
	}
	if len(envelope.Payload) == 0 {
		return fmt.Errorf("missing payload field in envelope")
	}

	hash := sha256.Sum256(envelope.Payload)
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("decode signature: %v", err)
	}

	pubKey, ok := app.SessionCert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("session cert has non-RSA key")
	}

	return rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], sigBytes)
}

// ---------------------------------------------------------------------------
// Silence monitor
// ---------------------------------------------------------------------------

func runSilenceMonitor(ctx context.Context, appID string) {
	threshold := time.Duration(silenceThresholdSec) * time.Second
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fedMu.RLock()
			app, ok := fedApps[appID]
			fedMu.RUnlock()
			if !ok {
				return
			}

			silence := time.Since(app.LastEventAt)
			if silence > threshold {
				log.Printf("[federation] Feed silence detected for %s (%.0fs > %ds) — connie-agent will re-handshake",
					appID, silence.Seconds(), silenceThresholdSec)

				fedMu.Lock()
				if a, ok := fedApps[appID]; ok {
					a.Status = "degraded"
				}
				fedMu.Unlock()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func handleFederationSubscriptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		fedMu.RLock()
		subs := make([]map[string]interface{}, 0, len(fedApps))
		for _, app := range fedApps {
			subs = append(subs, map[string]interface{}{
				"application_id":  app.AppID,
				"ac_endpoint":     app.ACEndpoint,
				"status":          app.Status,
				"session_cert_id": app.SessionCertID,
				"registered_at":   app.RegisteredAt.UTC().Format(time.RFC3339),
				"last_event_at":   app.LastEventAt.UTC().Format(time.RFC3339),
				"reconnect_count": app.ReconnectCount,
				"events_verified": app.EventsVerified,
				"ca_cert":         app.CACert,
			})
		}
		fedMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"subscriptions": subs})

	case http.MethodPost:
		// Receives fully authenticated session material from connie-agent.
		// connie-agent has already completed the three-message handshake.
		var req struct {
			ApplicationID  string `json:"application_id"`
			ACEndpoint     string `json:"ac_endpoint"`
			SessionCert    string `json:"session_cert"`
			SessionCertID  string `json:"session_cert_id"`
			MonitorCertPEM string `json:"monitor_cert_pem"`
			MonitorKeyPEM  string `json:"monitor_key_pem"`
			CACert         string `json:"ca_cert"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			req.ApplicationID == "" || req.ACEndpoint == "" ||
			req.SessionCert == "" || req.MonitorCertPEM == "" || req.MonitorKeyPEM == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "INVALID_REQUEST",
				"message": "application_id, ac_endpoint, session_cert, monitor_cert_pem, and monitor_key_pem required",
			})
			return
		}

		// Parse session cert (trusted root for TLS verification)
		block, _ := pem.Decode([]byte(req.SessionCert))
		if block == nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_CERT", "message": "could not parse session_cert PEM"})
			return
		}
		sessionCert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_CERT", "message": "could not parse session cert: " + err.Error()})
			return
		}

		// Parse monitor cert+key (our mTLS client credential)
		monitorCert, err := tls.X509KeyPair([]byte(req.MonitorCertPEM), []byte(req.MonitorKeyPEM))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_CERT", "message": "could not parse monitor cert/key: " + err.Error()})
			return
		}

		if err := validateHTTPSURL(req.ACEndpoint); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_ENDPOINT", "message": "ac_endpoint: " + err.Error()})
			return
		}

		// Idempotent — if already active with same session, return current status
		fedMu.RLock()
		existing, exists := fedApps[req.ApplicationID]
		fedMu.RUnlock()
		if exists && existing.Status == "active" && existing.SessionCertID == req.SessionCertID {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"application_id":  existing.AppID,
				"status":          existing.Status,
				"session_cert_id": existing.SessionCertID,
				"reconnect_count": existing.ReconnectCount,
				"message":         "already federated with this session",
			})
			return
		}

		if err := connectFederatedApp(req.ApplicationID, req.ACEndpoint, req.CACert,
			sessionCert, req.SessionCertID, monitorCert); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "CONNECT_FAILED",
				"message": err.Error(),
			})
			return
		}

		fedMu.RLock()
		app := fedApps[req.ApplicationID]
		fedMu.RUnlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"application_id":  app.AppID,
			"status":          app.Status,
			"session_cert_id": app.SessionCertID,
			"stream_url":      strings.TrimRight(app.ACEndpoint, "/") + "/stream",
		})

	case http.MethodDelete:
		appID := r.URL.Query().Get("application_id")
		if appID == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "INVALID_REQUEST",
				"message": "application_id query parameter required",
			})
			return
		}
		fedMu.Lock()
		app, exists := fedApps[appID]
		if exists {
			if app.cancelMonitor != nil {
				app.cancelMonitor()
			}
			delete(fedApps, appID)
		}
		fedMu.Unlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "NOT_FOUND",
				"message": fmt.Sprintf("no federation subscription for %s", appID),
			})
			return
		}
		log.Printf("[federation] Disconnected federation for %s", appID)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"message": fmt.Sprintf("federation disconnected for %s", appID),
		})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleFederationReconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ApplicationID string `json:"application_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ApplicationID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Reconnect is triggered by connie-agent pushing new session material
	// via POST /federation/subscriptions. This endpoint just marks degraded.
	fedMu.Lock()
	if app, ok := fedApps[req.ApplicationID]; ok {
		app.Status = "reconnecting"
	}
	fedMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"message": "reconnect scheduled — awaiting new session material from connie-agent",
	})
}

// ---------------------------------------------------------------------------
// SSE scanner
// ---------------------------------------------------------------------------

func newSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	return scanner
}

// ---------------------------------------------------------------------------
// Federation Redis subscription
// ---------------------------------------------------------------------------

// addFederationSubscription creates a local Redis subscription for the
// constellation's federation channel (federation:{appID}:events).
// Events published by processFederatedEvent flow through the existing
// processEvent pipeline — tagged, buffered, and forwarded to the UI SSE stream.
func addFederationSubscription(appID string) {
	channel := "federation:" + appID + ":events"

	sub := &Subscription{
		ApplicationID: appID,
		RedisURL:      redisURL,
		Channels:      []string{channel},
		Status:        "active",
		SubscribedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	subMu.Lock()
	// If a subscription already exists for this appID, cancel it first
	if existing, ok := subscriptions[appID]; ok && existing.cancel != nil {
		existing.cancel()
	}
	subscriptions[appID] = sub
	subMu.Unlock()

	windowMu.Lock()
	if _, exists := windows[appID]; !exists {
		windows[appID] = []ConstellationEvent{}
	}
	windowMu.Unlock()

	startSubscription(sub)
	log.Printf("[federation] Subscribed to %s for constellation %s", channel, appID)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
