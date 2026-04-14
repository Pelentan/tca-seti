package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Signal Aggregator — subscribes to constellation event streams and makes
// them queryable for call chain verification.
//
// On startup: subscribes to SETI's own tca:events (self-monitoring).
// On POST /subscriptions: connects to an external constellation's Redis.
//
// Maintains a rolling event window per application for Plot Test to query.
// Re-publishes all events to seti:events for dashboard delivery.
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4006")
	redisURL         = envOr("REDIS_URL", "redis:6379")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	policyURL        = envOr("POLICY_URL", "https://policy:4002")
	windowSize       = 5000 // events per application
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Event model
// ---------------------------------------------------------------------------

type ConstellationEvent struct {
	ApplicationID string `json:"application_id"`
	Caller        string `json:"caller"`
	Callee        string `json:"callee"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	StatusCode    int    `json:"status_code"`
	LatencyMs     int64  `json:"latency_ms"`
	Protocol      string `json:"protocol"`
	ReceivedAt    string `json:"received_at"`
}

// ---------------------------------------------------------------------------
// Subscription registry
// ---------------------------------------------------------------------------

type Subscription struct {
	ApplicationID  string   `json:"application_id"`
	RedisURL       string   `json:"redis_url"`
	Channels       []string `json:"channels"`
	Status         string   `json:"status"` // active | reconnecting | failed
	EventsReceived int64    `json:"events_received"`
	SubscribedAt   string   `json:"subscribed_at"`
	cancel         context.CancelFunc
}

var (
	subMu         sync.RWMutex
	subscriptions = map[string]*Subscription{}

	// Rolling event windows per application
	windowMu = sync.RWMutex{}
	windows  = map[string][]ConstellationEvent{}
)

func totalBuffered() int {
	windowMu.RLock()
	defer windowMu.RUnlock()
	total := 0
	for _, w := range windows {
		total += len(w)
	}
	return total
}

func appendEvent(appID string, ev ConstellationEvent) {
	windowMu.Lock()
	defer windowMu.Unlock()
	w := windows[appID]
	w = append(w, ev)
	if len(w) > windowSize {
		w = w[len(w)-windowSize:]
	}
	windows[appID] = w
}

func queryEvents(appID, caller, callee, since string, limit int) []ConstellationEvent {
	windowMu.RLock()
	defer windowMu.RUnlock()

	var src []ConstellationEvent
	if appID != "" {
		src = windows[appID]
	} else {
		for _, w := range windows {
			src = append(src, w...)
		}
	}

	var out []ConstellationEvent
	for _, e := range src {
		if caller != "" && e.Caller != caller {
			continue
		}
		if callee != "" && e.Callee != callee {
			continue
		}
		if since != "" && e.ReceivedAt <= since {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Redis clients
// ---------------------------------------------------------------------------

var (
	localRDB       *redis.Client // SETI's own Redis
	upstreamClient *http.Client
)

func connectRedis() {
	for i := 0; i < 10; i++ {
		localRDB = redis.NewClient(&redis.Options{Addr: redisURL})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := localRDB.Ping(ctx).Result()
		cancel()
		if err == nil {
			log.Printf("[signal-aggregator] Connected to Redis at %s", redisURL)
			return
		}
		log.Printf("[signal-aggregator] Redis not ready (%d/10): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	log.Fatal("[signal-aggregator] Could not connect to Redis")
}

// ---------------------------------------------------------------------------
// Subscription goroutine — one per application
// ---------------------------------------------------------------------------

func startSubscription(sub *Subscription) {
	ctx, cancel := context.WithCancel(context.Background())
	sub.cancel = cancel

	go func() {
		rdb := redis.NewClient(&redis.Options{Addr: sub.RedisURL})
		defer rdb.Close()

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			pubsub := rdb.Subscribe(ctx, sub.Channels...)
			ch := pubsub.Channel()
			sub.Status = "active"
			log.Printf("[signal-aggregator] Subscribed to %v for %s", sub.Channels, sub.ApplicationID)

			for {
				select {
				case <-ctx.Done():
					pubsub.Close()
					return
				case msg, ok := <-ch:
					if !ok {
						goto reconnect
					}
					processEvent(sub, msg.Channel, msg.Payload)
				}
			}

		reconnect:
			pubsub.Close()
			sub.Status = "reconnecting"
			log.Printf("[signal-aggregator] %s subscription dropped — reconnecting in 2s", sub.ApplicationID)
			time.Sleep(2 * time.Second)
		}
	}()
}

func processEvent(sub *Subscription, channel, payload string) {
	sub.EventsReceived++

	// Parse the raw event
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return
	}

	ev := ConstellationEvent{
		ApplicationID: sub.ApplicationID,
		ReceivedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}

	if v, ok := raw["caller"].(string); ok {
		ev.Caller = v
	}
	if v, ok := raw["callee"].(string); ok {
		ev.Callee = v
	}
	if v, ok := raw["method"].(string); ok {
		ev.Method = v
	}
	if v, ok := raw["path"].(string); ok {
		ev.Path = v
	}
	if v, ok := raw["status_code"].(float64); ok {
		ev.StatusCode = int(v)
	}
	if v, ok := raw["latency_ms"].(float64); ok {
		ev.LatencyMs = int64(v)
	}
	if v, ok := raw["protocol"].(string); ok {
		ev.Protocol = v
	}

	appendEvent(sub.ApplicationID, ev)

	// Re-publish to seti:events for dashboard and other subscribers
	enriched, _ := json.Marshal(ev)
	localRDB.Publish(context.Background(), "seti:aggregated", enriched)

	// Write to bad-whiff buffer — tier 2 of the three-tier storage model.
	// Every event writes regardless of pass/fail. AI-lien needs baseline data,
	// not just anomaly data. The buffer is a 24-hour sliding window of everything
	// that crossed the constellation boundary for this application.
	// Key: seti:whiff:{application_id}  Stream: MAXLEN ~10000 approximate trim.
	// A full buffer (10k entries) is itself a signal worth investigating.
	writeWhiffBuffer(sub.ApplicationID, ev)
}

// writeWhiffBuffer writes an event to the application's bad-whiff Redis Stream.
// Fire-and-forget — errors are logged but never affect the caller.
// Key pattern: seti:whiff:{application_id}
// MAXLEN ~10000 approximate trim — keeps recent history without unbounded growth.
func writeWhiffBuffer(applicationID string, ev ConstellationEvent) {
	go func() {
		key := "seti:whiff:" + applicationID
		ctx := context.Background()

		args := &redis.XAddArgs{
			Stream: key,
			MaxLen: 10000,
			Approx: true,
			ID:     "*",
			Values: map[string]interface{}{
				"application_id": applicationID,
				"caller":         ev.Caller,
				"callee":         ev.Callee,
				"method":         ev.Method,
				"path":           ev.Path,
				"status_code":    fmt.Sprintf("%d", ev.StatusCode),
				"latency_ms":     fmt.Sprintf("%d", ev.LatencyMs),
				"protocol":       ev.Protocol,
				"received_at":    ev.ReceivedAt,
			},
		}
		if err := localRDB.XAdd(ctx, args).Err(); err != nil {
			log.Printf("[signal-aggregator] whiff buffer write failed for %s: %v", applicationID, err)
		}
	}()
}

// ---------------------------------------------------------------------------
// Self-subscription — SETI monitors its own tca:events on startup
// ---------------------------------------------------------------------------

func selfSubscribe() {
	sub := &Subscription{
		ApplicationID: "seti",
		RedisURL:      redisURL,
		Channels:      []string{"seti:events", "tca:augur-canis"},
		Status:        "active",
		SubscribedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	subMu.Lock()
	subscriptions["seti"] = sub
	subMu.Unlock()
	windowMu.Lock()
	windows["seti"] = []ConstellationEvent{}
	windowMu.Unlock()
	startSubscription(sub)
	log.Printf("[signal-aggregator] Self-subscribed to seti (tca:events, tca:augur-canis)")
}

// ---------------------------------------------------------------------------
// Call chain verification
// ---------------------------------------------------------------------------

type ExpectedCall struct {
	Caller         string `json:"caller"`
	Callee         string `json:"callee"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	MinOccurrences int    `json:"min_occurrences"`
}

type VerifyRequest struct {
	ApplicationID string         `json:"application_id"`
	After         string         `json:"after"`
	WithinSeconds int            `json:"within_seconds"`
	ExpectedCalls []ExpectedCall `json:"expected_calls"`
}

type VerifyResult struct {
	Passed         bool           `json:"passed"`
	Matched        []ExpectedCall `json:"matched"`
	Unmatched      []ExpectedCall `json:"unmatched"`
	EventsSearched int            `json:"events_searched"`
	VerifiedAt     string         `json:"verified_at"`
}

func verifyCallChain(req VerifyRequest) VerifyResult {
	within := req.WithinSeconds
	if within == 0 {
		within = 30
	}

	deadline := req.After
	if deadline != "" {
		if t, err := time.Parse(time.RFC3339, req.After); err == nil {
			deadline2 := t.Add(time.Duration(within) * time.Second).Format(time.RFC3339Nano)
			// Use deadline to filter
			_ = deadline2
		}
	}

	events := queryEvents(req.ApplicationID, "", "", req.After, 0)

	result := VerifyResult{
		Passed:         true,
		EventsSearched: len(events),
		VerifiedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	for _, expected := range req.ExpectedCalls {
		minOcc := expected.MinOccurrences
		if minOcc == 0 {
			minOcc = 1
		}

		count := 0
		for _, ev := range events {
			if expected.Caller != "" && ev.Caller != expected.Caller {
				continue
			}
			if expected.Callee != "" && ev.Callee != expected.Callee {
				continue
			}
			if expected.Method != "" && ev.Method != expected.Method {
				continue
			}
			if expected.Path != "" && ev.Path != expected.Path {
				continue
			}
			count++
		}

		if count >= minOcc {
			result.Matched = append(result.Matched, expected)
		} else {
			result.Unmatched = append(result.Unmatched, expected)
			result.Passed = false
		}
	}

	if result.Matched == nil {
		result.Matched = []ExpectedCall{}
	}
	if result.Unmatched == nil {
		result.Unmatched = []ExpectedCall{}
	}

	return result
}

// ---------------------------------------------------------------------------
// mTLS
// ---------------------------------------------------------------------------

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
			"caller": "signal-aggregator", "callee": callee,
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
// HTTP handlers
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	subMu.RLock()
	count := len(subscriptions)
	subMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "healthy", "subscriptions": count,
		"events_buffered": totalBuffered(),
		"uptime_seconds":  int(time.Since(startTime).Seconds()),
	})
}

func handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		subMu.RLock()
		subs := make([]*Subscription, 0, len(subscriptions))
		for _, s := range subscriptions {
			subs = append(subs, s)
		}
		subMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"subscriptions": subs, "total": len(subs)})

	case http.MethodPost:
		var req struct {
			ApplicationID string   `json:"application_id"`
			RedisURL      string   `json:"redis_url"`
			Channels      []string `json:"channels"`
			WindowSize    int      `json:"window_size"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ApplicationID == "" || req.RedisURL == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "application_id and redis_url required"})
			return
		}

		subMu.Lock()
		if _, exists := subscriptions[req.ApplicationID]; exists {
			subMu.Unlock()
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"code": "ALREADY_SUBSCRIBED", "message": fmt.Sprintf("already subscribed to %s", req.ApplicationID)})
			return
		}

		channels := req.Channels
		if len(channels) == 0 {
			channels = []string{"tca:events", "tca:augur-canis"}
		}
		sub := &Subscription{
			ApplicationID: req.ApplicationID,
			RedisURL:      req.RedisURL,
			Channels:      channels,
			Status:        "active",
			SubscribedAt:  time.Now().UTC().Format(time.RFC3339),
		}
		subscriptions[req.ApplicationID] = sub
		subMu.Unlock()

		windowMu.Lock()
		ws := windowSize
		if req.WindowSize > 0 {
			ws = req.WindowSize
		}
		windows[req.ApplicationID] = make([]ConstellationEvent, 0, ws)
		windowMu.Unlock()

		startSubscription(sub)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(sub)
		log.Printf("[signal-aggregator] Subscribed to %s at %s", req.ApplicationID, req.RedisURL)
	}
}

func handleSubscriptionDelete(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
	appID = strings.TrimSuffix(appID, "/")

	subMu.Lock()
	sub, exists := subscriptions[appID]
	if exists {
		if sub.cancel != nil {
			sub.cancel()
		}
		delete(subscriptions, appID)
	}
	subMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("no subscription for %s", appID)})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": fmt.Sprintf("unsubscribed from %s", appID)})
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	appID := q.Get("application_id")
	caller := q.Get("caller")
	callee := q.Get("callee")
	since := q.Get("since")
	limit := 100
	if l := q.Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	events := queryEvents(appID, caller, callee, since, limit)
	if events == nil {
		events = []ConstellationEvent{}
	}

	oldest, newest := "", ""
	if len(events) > 0 {
		oldest = events[0].ReceivedAt
		newest = events[len(events)-1].ReceivedAt
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"events": events, "total": len(events),
		"oldest_event_at": oldest, "newest_event_at": newest,
	})
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	var req VerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ApplicationID == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "application_id and expected_calls required"})
		return
	}
	result := verifyCallChain(req)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ---------------------------------------------------------------------------
// mTLS server
// ---------------------------------------------------------------------------

func loadServerTLS() *tls.Config {
	return buildServerTLS(certMat)
}

var startTime = time.Now()

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

var certMat *CertMaterial

func main() {
	certMat = obtainCerts("signal-aggregator")
	buildUpstreamClient()
	connectRedis()
	loadStarGazer()
	go selfRegisterWithAC(certMat, "https://signal-aggregator:4006")
	selfSubscribe()
	go initFederationOnStartup()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/subscriptions", handleSubscriptions)
	mux.HandleFunc("/subscriptions/", handleSubscriptionDelete)
	mux.HandleFunc("/events", handleEvents)
	mux.HandleFunc("/verify/call-chain", handleVerify)
	mux.HandleFunc("/federation/subscriptions", handleFederationSubscriptions)
	mux.HandleFunc("/federation/subscriptions/", handleFederationReconnect)

	server := &http.Server{
		Addr: ":" + port, Handler: mux, TLSConfig: loadServerTLS(),
	}

	log.Printf("[signal-aggregator] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[signal-aggregator] Self-subscribed to seti — watching tca:events and tca:augur-canis")

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[signal-aggregator] %v", err)
	}
}
