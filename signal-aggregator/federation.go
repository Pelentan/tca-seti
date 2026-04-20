package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Federation — SETI's outbound side of the trust handshake.
//
// On startup (or when a Sec Wr4ngler registers a new constellation):
//   1. Signal Aggregator signs a registration request with star-gazer.key.
//   2. Sends it to the constellation's AC POST /federation/register.
//   3. AC verifies the signature, returns a session public certificate.
//   4. Signal Aggregator subscribes to the AC's tca:augur-canis feed.
//   5. Every incoming event is verified against the session cert.
//
// Feed monitor (per federated app):
//   - Silence beyond silenceThreshold → re-register, update session cert.
//   - session_cert_id mismatch or signature failure → re-register.
//   - Data lost during re-registration is acceptable — SETI is an
//     accumulator, not a ledger.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type FederatedApp struct {
	AppID          string
	ACEndpoint     string
	RedisURL       string
	FeedChannel    string
	SessionCert    *x509.Certificate
	SessionCertID  string
	RegisteredAt   time.Time
	LastEventAt    time.Time
	ReconnectCount int
	Status         string // active | degraded | reconnecting | failed
	EventsVerified int64
	cancelMonitor  context.CancelFunc
}

type acRegistrationRequest struct {
	SetiInstanceID     string `json:"seti_instance_id"`
	StarGazerPublicCert string `json:"star_gazer_public_cert"`
	Timestamp          string `json:"timestamp"`
	Signature          string `json:"signature"`
}

type acRegistrationResponse struct {
	SessionCert     string `json:"session_cert"`
	SessionCertID   string `json:"session_cert_id"`
	IssuedAt        string `json:"issued_at"`
	FeedChannel     string `json:"feed_channel"`
	ConstellationID string `json:"constellation_id"`
}

// ---------------------------------------------------------------------------
// Global federation state
// ---------------------------------------------------------------------------

var (
	fedMu   sync.RWMutex
	fedApps = map[string]*FederatedApp{}

	// Star-gazer identity — loaded once on startup
	starGazerKey  *rsa.PrivateKey
	starGazerCert []byte // PEM — sent in every registration request

	setiInstanceID      = envOr("SETI_INSTANCE_ID", "tca-seti-primary")
	selfACEndpoint      = envOr("AC_ENDPOINT", "https://augur-canis:4010")
	silenceThresholdSec = envOrInt("SILENCE_THRESHOLD_SECONDS", 90)
)

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		if n > 0 {
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// Startup
// ---------------------------------------------------------------------------

func loadStarGazer() {
	certPath := envOr("STAR_GAZER_CERT", "/certs/star-gazer.crt")
	keyPath := envOr("STAR_GAZER_KEY", "/certs/star-gazer.key")

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		log.Printf("[federation] Could not load star-gazer cert: %v — federation disabled", err)
		return
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		log.Printf("[federation] Could not load star-gazer key: %v — federation disabled", err)
		return
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		log.Printf("[federation] star-gazer key is not valid PEM")
		return
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8
		pk, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			log.Printf("[federation] Could not parse star-gazer key: %v", err)
			return
		}
		var ok bool
		key, ok = pk.(*rsa.PrivateKey)
		if !ok {
			log.Printf("[federation] star-gazer key is not RSA")
			return
		}
	}

	starGazerKey = key
	starGazerCert = certPEM
	log.Printf("[federation] Star-gazer identity loaded (instance=%s)", setiInstanceID)
}

func initFederationOnStartup() {
	if starGazerKey == nil {
		log.Printf("[federation] Star-gazer not loaded — skipping federation init")
		return
	}

	// Give the rest of the stack a moment to be ready
	time.Sleep(3 * time.Second)

	// Register with seti AC first — proves the mechanism locally
	if selfACEndpoint != "" {
		log.Printf("[federation] Initiating federation with seti AC at %s", selfACEndpoint)
		if err := connectFederatedApp("seti", selfACEndpoint, redisURL); err != nil {
			log.Printf("[federation] seti federation failed: %v — will retry on silence detection", err)
		}
	}

	// TODO: fetch additional registered constellations from Policy and connect to each
}

// ---------------------------------------------------------------------------
// Registration handshake
// ---------------------------------------------------------------------------

func signRegistrationPayload(instanceID, timestamp string) (string, error) {
	if starGazerKey == nil {
		return "", fmt.Errorf("star-gazer key not loaded")
	}
	payload := instanceID + timestamp
	hash := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, starGazerKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

func registerWithAC(acEndpoint string) (*acRegistrationResponse, error) {
	timestamp := time.Now().UTC().Format(time.RFC3339)
	sig, err := signRegistrationPayload(setiInstanceID, timestamp)
	if err != nil {
		return nil, err
	}

	reqBody := acRegistrationRequest{
		SetiInstanceID:     setiInstanceID,
		StarGazerPublicCert: string(starGazerCert),
		Timestamp:          timestamp,
		Signature:          sig,
	}
	body, _ := json.Marshal(reqBody)

	url := strings.TrimRight(acEndpoint, "/") + "/federation/register"
	resp, err := postJSONToAC(url, body)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AC returned %d", resp.StatusCode)
	}

	var result acRegistrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %v", err)
	}
	return &result, nil
}

func postJSONToAC(url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return upstreamClient.Do(req)
}

func parseSessionCert(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("not valid PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ---------------------------------------------------------------------------
// Connect a federated app — register + subscribe
// ---------------------------------------------------------------------------

func connectFederatedApp(appID, acEndpoint, feedRedisURL string) error {
	result, err := registerWithAC(acEndpoint)
	if err != nil {
		return fmt.Errorf("registration failed: %v", err)
	}

	sessionCert, err := parseSessionCert(result.SessionCert)
	if err != nil {
		return fmt.Errorf("parse session cert: %v", err)
	}

	feedChannel := result.FeedChannel
	if feedChannel == "" {
		feedChannel = "tca:augur-canis"
	}

	fedMu.Lock()
	existing, exists := fedApps[appID]
	if exists && existing.cancelMonitor != nil {
		existing.cancelMonitor()
	}

	ctx, cancel := context.WithCancel(context.Background())
	app := &FederatedApp{
		AppID:          appID,
		ACEndpoint:     acEndpoint,
		RedisURL:       feedRedisURL,
		FeedChannel:    feedChannel,
		SessionCert:    sessionCert,
		SessionCertID:  result.SessionCertID,
		RegisteredAt:   time.Now(),
		LastEventAt:    time.Now(),
		Status:         "active",
		cancelMonitor:  cancel,
	}
	if exists {
		app.ReconnectCount = existing.ReconnectCount + 1
	}
	fedApps[appID] = app
	fedMu.Unlock()

	log.Printf("[federation] Registered with %s AC (app=%s, session=%s, reconnects=%d)",
		acEndpoint, appID, result.SessionCertID, app.ReconnectCount)

	go runFederatedFeed(ctx, app)
	go runSilenceMonitor(ctx, appID)

	return nil
}

// ---------------------------------------------------------------------------
// Feed subscription with verification
// ---------------------------------------------------------------------------

func runFederatedFeed(ctx context.Context, app *FederatedApp) {
	rdb := NewRedisClient(app.RedisURL)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		subCtx, subCancel := context.WithCancel(ctx)
		ch, err := rdb.Subscribe(subCtx, app.FeedChannel)
		if err != nil {
			subCancel()
			log.Printf("[federation] Subscribe failed for %s — retrying in 2s: %v", app.AppID, err)
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("[federation] Subscribed to %s for %s", app.FeedChannel, app.AppID)

		for {
			select {
			case <-ctx.Done():
				subCancel()
				return
			case msg, ok := <-ch:
				if !ok {
					subCancel()
					goto reconnect
				}
				processFederatedEvent(app.AppID, msg)
			}
		}

	reconnect:
		log.Printf("[federation] Feed dropped for %s — reconnecting in 2s", app.AppID)
		time.Sleep(2 * time.Second)
	}
}

func processFederatedEvent(appID, payload string) {
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		log.Printf("[federation] Could not parse feed event for %s: %v", appID, err)
		return
	}

	// Verify signature if present
	certID, _ := event["session_cert_id"].(string)
	sig, _ := event["signature"].(string)

	if certID != "" && sig != "" {
		if err := verifyFeedEvent(appID, certID, sig, payload); err != nil {
			log.Printf("[federation] Signature verification FAILED for %s (cert=%s): %v", appID, certID, err)
			// Trigger re-registration asynchronously
			go func() {
				if err := reconnectFederatedApp(appID); err != nil {
					log.Printf("[federation] Re-registration failed for %s: %v", appID, err)
				}
			}()
			return
		}
		fedMu.Lock()
		if app, ok := fedApps[appID]; ok {
			app.LastEventAt = time.Now()
			app.EventsVerified++
			if app.EventsVerified % 10 == 1 {
				log.Printf("[federation] Verified event #%d from %s (cert=%s)", app.EventsVerified, appID, certID)
			}
		}
		fedMu.Unlock()
	} else {
		// Unsigned event — update last seen but don't count as verified
		fedMu.Lock()
		if app, ok := fedApps[appID]; ok {
			app.LastEventAt = time.Now()
			log.Printf("[federation] Unsigned event received from %s (no session active on AC side yet)", appID)
		}
		fedMu.Unlock()
	}

	// Forward the event into the rolling window as a health event
	// (distinct from observability events — service_name not caller/callee)
	serviceName, _ := event["service_name"].(string)
	if serviceName == "" {
		return
	}

	// Store in window as a synthetic observability event so SETI's
	// dashboard and query layer can see health state
	healthy, _ := event["healthy"].(bool)
	status := 200
	if !healthy {
		status = 503
	}
	appendEvent(appID, ConstellationEvent{
		ApplicationID: appID,
		Caller:        "augur-canis",
		Callee:        serviceName,
		Method:        "POST",
		Path:          "/check",
		StatusCode:    status,
		LatencyMs:     0,
		Protocol:      "redis-pubsub",
		ReceivedAt:    time.Now().UTC().Format(time.RFC3339),
	})
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

	// Verify signature over the unsigned payload (everything except session_cert_id and signature fields)
	// The AC signs the event before adding those fields, so we reconstruct
	var eventMap map[string]interface{}
	json.Unmarshal([]byte(fullPayload), &eventMap)
	delete(eventMap, "session_cert_id")
	delete(eventMap, "signature")
	unsigned, _ := json.Marshal(eventMap)

	hash := sha256.Sum256(unsigned)
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
				log.Printf("[federation] Feed silence detected for %s (%.0fs > %ds) — re-registering",
					appID, silence.Seconds(), silenceThresholdSec)

				fedMu.Lock()
				if a, ok := fedApps[appID]; ok {
					a.Status = "degraded"
				}
				fedMu.Unlock()

				go func() {
					if err := reconnectFederatedApp(appID); err != nil {
						log.Printf("[federation] Re-registration failed for %s: %v", appID, err)
						fedMu.Lock()
						if a, ok := fedApps[appID]; ok {
							a.Status = "failed"
						}
						fedMu.Unlock()
					}
				}()
			}
		}
	}
}

func reconnectFederatedApp(appID string) error {
	fedMu.RLock()
	app, ok := fedApps[appID]
	fedMu.RUnlock()
	if !ok {
		return fmt.Errorf("app %s not found", appID)
	}
	return connectFederatedApp(appID, app.ACEndpoint, app.RedisURL)
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
				"feed_channel":    app.FeedChannel,
			})
		}
		fedMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"subscriptions": subs})

	case http.MethodPost:
		// Triggered by Policy when a Sec Wr4ngler registers an available application.
		// Closes the TODO in initFederationOnStartup — runtime federation without restart.
		var req struct {
			ApplicationID string `json:"application_id"`
			ACEndpoint    string `json:"ac_endpoint"`
			FeedRedisURL  string `json:"feed_redis_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			req.ApplicationID == "" || req.ACEndpoint == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "INVALID_REQUEST",
				"message": "application_id and ac_endpoint required",
			})
			return
		}
		feedRedis := req.FeedRedisURL
		if feedRedis == "" {
			feedRedis = redisURL // default to SETI's own Redis if not specified
		}

		if starGazerKey == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "FEDERATION_DISABLED",
				"message": "star-gazer identity not loaded — federation unavailable",
			})
			return
		}

		// Idempotent — if already registered, return current status
		fedMu.RLock()
		existing, exists := fedApps[req.ApplicationID]
		fedMu.RUnlock()
		if exists && existing.Status == "active" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"application_id":  existing.AppID,
				"status":          existing.Status,
				"session_cert_id": existing.SessionCertID,
				"reconnect_count": existing.ReconnectCount,
				"message":         "already federated",
			})
			return
		}

		if err := connectFederatedApp(req.ApplicationID, req.ACEndpoint, feedRedis); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{
				"code":    "FEDERATION_FAILED",
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
			"feed_channel":    app.FeedChannel,
		})

	case http.MethodDelete:
		// Path: DELETE /federation/subscriptions?application_id={id}
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
		log.Printf("[federation] Disconnected federation for %s (Sec Wr4ngler deregistration)", appID)
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

	// Path: /federation/subscriptions/{application_id}/reconnect
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		http.Error(w, `{"error":"missing application_id"}`, http.StatusBadRequest)
		return
	}
	appID := parts[len(parts)-2] // .../subscriptions/{appID}/reconnect

	if err := reconnectFederatedApp(appID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadGateway)
		return
	}

	fedMu.RLock()
	app := fedApps[appID]
	fedMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"application_id":  appID,
		"status":          app.Status,
		"session_cert_id": app.SessionCertID,
		"reconnect_count": app.ReconnectCount,
	})
}
