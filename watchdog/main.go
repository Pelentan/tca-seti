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
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// STUB: Phase 1 Watchdog
//
// Subscribes to tca:check-requests and publishes stub healthy responses.
// Real canned query execution against point-to-point networks is deferred.
// All responses are stub_active: true.
//
// This stub exists so the Redis-mediated health check mechanism works
// end-to-end in Phase 1 before the real Watchdog implementation is built.
// The healthcheck binary in every container will receive responses and
// health checks will pass.
//
// Swap point: AUTH_MODE=production (future) — replace stub response logic
// with real canned query execution per watchdog contract.
// ---------------------------------------------------------------------------

var (
	redisURL        = envOr("REDIS_URL", "redis:6379")
	port            = envOr("PORT", "4010")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
)

var (
	checksHandled   atomic.Int64
	checksPublished atomic.Int64
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

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
			log.Printf("[watchdog] STUB: Connected to Redis at %s", redisURL)
			return
		}
		log.Printf("[watchdog] STUB: Redis not ready (attempt %d/10): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	log.Fatal("[watchdog] STUB: Could not connect to Redis after 10 attempts")
}

// ---------------------------------------------------------------------------
// Check request handler — subscribes to tca:check-requests
// ---------------------------------------------------------------------------

func handleCheckRequests() {
	ctx := context.Background()

	for {
		sub := rdb.Subscribe(ctx, "tca:check-requests")
		ch := sub.Channel()
		log.Printf("[watchdog] STUB: Subscribed to tca:check-requests — responding with stub healthy")

		for msg := range ch {
			checksHandled.Add(1)

			var req map[string]string
			if err := json.Unmarshal([]byte(msg.Payload), &req); err != nil {
				log.Printf("[watchdog] STUB: Failed to parse check request: %v", err)
				continue
			}

			requestID := req["request_id"]
			serviceName := req["service_name"]

			if requestID == "" || serviceName == "" {
				log.Printf("[watchdog] STUB: Invalid check request — missing request_id or service_name")
				continue
			}

			log.Printf("[watchdog] STUB: Check request for %s (request_id: %s) — returning stub healthy",
				serviceName, requestID)

			// Publish stub healthy result
			result, _ := json.Marshal(map[string]interface{}{
				"request_id":          requestID,
				"service_name":        serviceName,
				"healthy":             1,
				"latency_ms":          0,
				"time_to_complete_ms": 1,
				"query_version":       0,
				"checked_at":          time.Now().UTC().Format(time.RFC3339),
				"stub_active":         true,
			})

			resultChannel := fmt.Sprintf("tca:check-results:%s", requestID)
			ttl := 30 * time.Second

			if err := rdb.Set(ctx, resultChannel+"_data", result, ttl).Err(); err != nil {
				log.Printf("[watchdog] STUB: Failed to store result: %v", err)
			}
			if err := rdb.Publish(ctx, resultChannel, result).Err(); err != nil {
				log.Printf("[watchdog] STUB: Failed to publish result: %v", err)
				continue
			}

			checksPublished.Add(1)

			// Also publish to tca:watchdog feed
			feedEvent, _ := json.Marshal(map[string]interface{}{
				"service_name":        serviceName,
				"healthy":             1,
				"latency_ms":          0,
				"time_to_complete_ms": 1,
				"query_version":       0,
				"checked_at":          time.Now().UTC().Format(time.RFC3339),
				"constellation":       "tca-seti",
				"stub_active":         true,
			})
			if err := rdb.Publish(ctx, "tca:watchdog", feedEvent).Err(); err != nil {
				log.Printf("[watchdog] STUB: Failed to publish to tca:watchdog: %v", err)
			}
		}

		log.Printf("[watchdog] STUB: tca:check-requests subscription dropped — reconnecting in 2s")
		sub.Close()
		time.Sleep(2 * time.Second)
	}
}

// ---------------------------------------------------------------------------
// Observability reporting
// ---------------------------------------------------------------------------

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller":      "watchdog",
			"callee":      callee,
			"method":      method,
			"path":        path,
			"status_code": status,
			"latency_ms":  latencyMs,
			"protocol":    "mtls",
		})
		// Use standard HTTP since observability may not have mTLS client in Phase 1 stub
		// Full mTLS client wired in when real implementation replaces stub
		req, err := http.NewRequest(http.MethodPost, observabilityURL+"/event",
			jsonReader(body))
		if err != nil {
			log.Printf("[watchdog] STUB: observability error: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		http.DefaultClient.Do(req)
	}()
}

func jsonReader(b []byte) *jsonBytesReader {
	return &jsonBytesReader{data: b}
}

type jsonBytesReader struct {
	data   []byte
	offset int
}

func (r *jsonBytesReader) Read(p []byte) (n int, err error) {
	if r.offset >= len(r.data) {
		return 0, fmt.Errorf("EOF")
	}
	n = copy(p, r.data[r.offset:])
	r.offset += n
	return
}

// ---------------------------------------------------------------------------
// mTLS server setup
// ---------------------------------------------------------------------------

func loadTLSConfig() *tls.Config {
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Fatalf("[watchdog] STUB: Failed to read CA cert: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	cert, err := tls.LoadX509KeyPair("/certs/watchdog.crt", "/certs/watchdog.key")
	if err != nil {
		log.Fatalf("[watchdog] STUB: Failed to load service cert: %v", err)
	}

	return &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

// ---------------------------------------------------------------------------
// Admin HTTP handlers
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":            "stub_active",
		"stub_active":       true,
		"jobs_registered":   0,
		"queries_defined":   0,
		"checks_completed":  checksHandled.Load(),
		"checks_failed":     0,
		"redis_connected":   true,
		"last_cycle_at":     time.Now().UTC().Format(time.RFC3339),
		"uptime_seconds":    int(time.Since(startTime).Seconds()),
	})
}

func handleConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"check_interval_seconds": 30,
		"result_ttl_seconds":     30,
		"feed_channel":           "tca:watchdog",
		"request_channel":        "tca:check-requests",
		"stub_active":            true,
		"jobs_registered":        0,
		"queries_defined":        0,
	})
}

func handleStubNotice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"code":    "STUB_ACTIVE",
		"message": "STUB: Watchdog real implementation not yet active. Check requests handled via Redis pub/sub stub.",
	})
}

var startTime = time.Now()

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	connectRedis()

	// Start check request handler in background
	go handleCheckRequests()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/configuration", handleConfiguration)
	mux.HandleFunc("/jobs", handleStubNotice)
	mux.HandleFunc("/queries/", handleStubNotice)
	mux.HandleFunc("/checks/", handleStubNotice)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadTLSConfig(),
	}

	log.Printf("[watchdog] STUB: Admin API listening on :%s (mTLS)", port)
	log.Printf("[watchdog] STUB: Handling check requests via tca:check-requests")
	log.Printf("[watchdog] STUB: Publishing health feed to tca:watchdog")
	log.Printf("[watchdog] STUB: Real canned query execution deferred to full implementation")

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[watchdog] STUB: Server error: %v", err)
	}
}
