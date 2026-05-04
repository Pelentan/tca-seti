package main

// ---------------------------------------------------------------------------
// Graceful shutdown — x-tca-lifecycle
//
// Contract: contracts/openapi/augur-canis.yaml x-tca-lifecycle
// Sequence: stop-accepting-connections → drain-in-flight-requests →
//           unsubscribe-check-requests-channel → stop-health-feed-publisher →
//           close-redis → exit-0
//
// terminationGracePeriodSeconds in Helm must exceed drain-timeout-seconds.
// ---------------------------------------------------------------------------

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const shutdownDrainTimeout = 15 * time.Second

// awaitShutdown blocks until SIGTERM or SIGINT, then executes the graceful
// shutdown sequence declared in x-tca-lifecycle. The check-requests
// subscription is cancelled before the Redis connection closes to avoid
// a blocked goroutine on shutdown.
func awaitShutdown(server *http.Server) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	log.Printf("[augur-canis] Received %s — starting graceful shutdown", sig)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()

	// Step 1+2: Stop accepting connections and drain in-flight requests.
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[augur-canis] HTTP server shutdown error: %v", err)
	}

	// Step 3: Cancel the check-requests subscription goroutine.
	if checkShutdown != nil {
		checkShutdown()
	}

	// Step 4+5: Close Redis connection.
	// The health feed publisher and subscription goroutine have exited.
	if rdb != nil {
		if err := rdb.Close(); err != nil {
			log.Printf("[augur-canis] Redis close error: %v", err)
		}
	}

	log.Printf("[augur-canis] Shutdown complete")
	os.Exit(0)
}
