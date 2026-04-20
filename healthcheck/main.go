package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"time"
)

// Healthcheck binary for TCA Jobs.
// Uses Augur Canis Redis pub/sub mechanism — no open HTTP ports required.
//
// Flow:
//  1. Publish check request to tca:check-requests
//  2. Subscribe to tca:check-results:{request_id}
//  3. Wait for AC to execute canned query and publish result
//  4. Exit 0 (healthy) or 1 (unhealthy or timeout)
//
// Environment variables:
//
//	REDIS_URL        — Redis host:port (required)
//	SERVICE_NAME     — this Job's service name (required)
//	CHECK_TIMEOUT_MS — how long to wait for AC response (default 8000)
func main() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		fmt.Fprintln(os.Stderr, "[healthcheck] REDIS_URL not set")
		os.Exit(1)
	}

	serviceName := os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		fmt.Fprintln(os.Stderr, "[healthcheck] SERVICE_NAME not set")
		os.Exit(1)
	}

	timeoutMs := 8000
	if v := os.Getenv("CHECK_TIMEOUT_MS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			timeoutMs = t
		}
	}

	requestID := fmt.Sprintf("chk-%x", rand.Int63())
	containerID := os.Getenv("HOSTNAME") // Docker sets HOSTNAME to container ID

	rdb := NewRedisClient(redisURL)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeoutMs)*time.Millisecond,
	)
	defer cancel()

	// Subscribe to result channel BEFORE publishing request to avoid race
	// where AC publishes before we subscribe
	resultChannel := fmt.Sprintf("tca:check-results:%s", requestID)
	ch, err := rdb.Subscribe(ctx, resultChannel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[healthcheck] Failed to subscribe to result channel: %v\n", err)
		// Fall back to Redis connectivity check
		if pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second); rdb.Ping(pingCtx) == nil {
			pingCancel()
			fmt.Fprintln(os.Stderr, "[healthcheck] AC unavailable — Redis reachable, degraded-healthy")
			os.Exit(0)
		} else {
			pingCancel()
			os.Exit(1)
		}
	}

	// Publish check request
	request, _ := json.Marshal(map[string]string{
		"request_id":   requestID,
		"container_id": containerID,
		"service_name": serviceName,
		"published_at": time.Now().UTC().Format(time.RFC3339),
	})

	if err := rdb.Publish(ctx, "tca:check-requests", string(request)); err != nil {
		fmt.Fprintf(os.Stderr, "[healthcheck] Failed to publish check request: %v\n", err)
		// AC unavailable — fall back to Redis connectivity check
		// Don't kill the Job because AC isn't up yet (startup ordering)
		if pingCtx, pingCancel := context.WithTimeout(
			context.Background(), 2*time.Second,
		); rdb.Ping(pingCtx) == nil {
			pingCancel()
			fmt.Fprintln(os.Stderr, "[healthcheck] AC unavailable — Redis reachable, degraded-healthy")
			os.Exit(0)
		} else {
			pingCancel()
			os.Exit(1)
		}
	}

	// Wait for AC response
	select {
	case msg, ok := <-ch:
		if !ok {
			fmt.Fprintln(os.Stderr, "[healthcheck] Result channel closed unexpectedly")
			os.Exit(1)
		}
		var result map[string]interface{}
		if err := json.Unmarshal([]byte(msg), &result); err != nil {
			fmt.Fprintf(os.Stderr, "[healthcheck] Failed to parse result: %v\n", err)
			os.Exit(1)
		}

		switch v := result["healthy"].(type) {
		case float64:
			if v == 1 {
				os.Exit(0)
			}
		case int:
			if v == 1 {
				os.Exit(0)
			}
		}

		reason, _ := result["failure_reason"].(string)
		fmt.Fprintf(os.Stderr, "[healthcheck] Unhealthy: %s\n", reason)
		os.Exit(1)

	case <-ctx.Done():
		// AC timed out — fall back to Redis connectivity
		fmt.Fprintln(os.Stderr, "[healthcheck] Timeout waiting for AC response")
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer pingCancel()
		if rdb.Ping(pingCtx) == nil {
			fmt.Fprintln(os.Stderr, "[healthcheck] Redis reachable — degraded-healthy")
			os.Exit(0)
		}
		os.Exit(1)
	}
}
