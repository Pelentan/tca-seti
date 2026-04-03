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
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Known callers allowlist — must match x-tca-observability caller-name values
// ---------------------------------------------------------------------------

var knownServices = map[string]bool{
	"gateway":          true,
	"signal-clearance": true,
	"policy":           true,
	"contract-test":    true,
	"plot-test":        true,
	"plot-store":       true,
	"signal-aggregator": true,
	"feed-wrangler":    true,
	"results":          true,
	"augur-canis":      true,
	"ui":               true,
	"ai-lien":          true,
	"integration":      true,
}

// ---------------------------------------------------------------------------
// Sanitization patterns
// ---------------------------------------------------------------------------

var sanitizers = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), "[token]"},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "[ip]"},
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "[id]"},
	{regexp.MustCompile(`([?&][^=&]+)=([^&]*)`), "$1=[redacted]"},
}

func sanitizePath(path string) string {
	for _, s := range sanitizers {
		path = s.pattern.ReplaceAllString(path, s.replacement)
	}
	return path
}

// ---------------------------------------------------------------------------
// Event structures
// ---------------------------------------------------------------------------

type EventReport struct {
	Caller     string `json:"caller"`
	Callee     string `json:"callee"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	StatusCode int    `json:"status_code"`
	LatencyMs  int64  `json:"latency_ms"`
	Protocol   string `json:"protocol"`
}

type PublishedEvent struct {
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	Caller     string `json:"caller"`
	Callee     string `json:"callee"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	StatusCode int    `json:"statusCode"`
	LatencyMs  int64  `json:"latencyMs"`
	Protocol   string `json:"protocol"`
}

// ---------------------------------------------------------------------------
// Counters
// ---------------------------------------------------------------------------

var (
	eventsReceived  atomic.Int64
	eventsPublished atomic.Int64
	eventsDropped   atomic.Int64
)

// ---------------------------------------------------------------------------
// Redis client
// ---------------------------------------------------------------------------

var rdb *redis.Client

func connectRedis() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis:6379"
	}

	for i := 0; i < 10; i++ {
		rdb = redis.NewClient(&redis.Options{Addr: redisURL})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := rdb.Ping(ctx).Result()
		cancel()
		if err == nil {
			log.Printf("[observability] Connected to Redis at %s", redisURL)
			return
		}
		log.Printf("[observability] Redis not ready (attempt %d/10): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	log.Fatal("[observability] Could not connect to Redis after 10 attempts")
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func handleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	eventsReceived.Add(1)
	w.WriteHeader(http.StatusAccepted)

	var report EventReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		eventsDropped.Add(1)
		log.Printf("[observability] Failed to decode event: %v", err)
		return
	}

	// Validate caller
	if !knownServices[report.Caller] {
		eventsDropped.Add(1)
		log.Printf("[observability] Unknown caller dropped: %q", report.Caller)
		return
	}

	// Sanitize path
	report.Path = sanitizePath(report.Path)

	// Build published event
	event := PublishedEvent{
		ID:         fmt.Sprintf("%d", time.Now().UnixNano()),
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Caller:     report.Caller,
		Callee:     report.Callee,
		Method:     report.Method,
		Path:       report.Path,
		StatusCode: report.StatusCode,
		LatencyMs:  report.LatencyMs,
		Protocol:   report.Protocol,
	}

	payload, err := json.Marshal(event)
	if err != nil {
		eventsDropped.Add(1)
		log.Printf("[observability] Failed to marshal event: %v", err)
		return
	}

	ctx := context.Background()
	if err := rdb.Publish(ctx, "seti:events", payload).Err(); err != nil {
		eventsDropped.Add(1)
		log.Printf("[observability] Failed to publish to Redis: %v", err)
		return
	}

	eventsPublished.Add(1)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	redisStatus := "connected"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		redisStatus = "disconnected"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "healthy",
		"service":         "seti-observability",
		"language":        "Go",
		"redis":           redisStatus,
		"eventsReceived":  eventsReceived.Load(),
		"eventsPublished": eventsPublished.Load(),
		"eventsDropped":   eventsDropped.Load(),
	})
}

func handleRules(w http.ResponseWriter, r *http.Request) {
	callers := make([]string, 0, len(knownServices))
	for k := range knownServices {
		callers = append(callers, k)
	}

	patterns := []map[string]string{}
	for _, s := range sanitizers {
		patterns = append(patterns, map[string]string{
			"pattern":     s.pattern.String(),
			"replacement": s.replacement,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"knownCallers":         callers,
		"sanitizationPatterns": patterns,
	})
}

// ---------------------------------------------------------------------------
// mTLS server setup
// ---------------------------------------------------------------------------

func loadTLSConfig() *tls.Config {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Fatalf("[observability] Failed to read CA cert: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	cert, err := tls.LoadX509KeyPair("/certs/seti-observability.crt", "/certs/seti-observability.key")
	if err != nil {
		log.Fatalf("[observability] Failed to load service cert: %v", err)
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
	port := os.Getenv("PORT")
	if port == "" {
		port = "4011"
	}

	connectRedis()

	mux := http.NewServeMux()
	mux.HandleFunc("/event", handleEvent)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/rules", handleRules)

	// Health check on plain HTTP for Docker healthcheck probe
	// This runs on a separate port (4011+1 = 4012 internal only)
	// so we don't relax mTLS on the main port
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/health", handleHealth)
	go func() {
		log.Printf("[observability] Health probe listening on :4012 (plain HTTP, internal only)")
		if err := http.ListenAndServe(":4012", healthMux); err != nil {
			log.Printf("[observability] Health probe error: %v", err)
		}
	}()

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadTLSConfig(),
	}

	// Strip trailing slash for cleaner logging
	addr := strings.TrimSuffix(server.Addr, "/")
	log.Printf("[observability] Listening on %s (mTLS, TLS 1.3)", addr)
	log.Printf("[observability] Publishing to Redis channel: seti:events")
	log.Printf("[observability] Known callers: %d services", len(knownServices))

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[observability] Server error: %v", err)
	}
}
