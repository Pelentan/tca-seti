package main

// ---------------------------------------------------------------------------
// Graceful shutdown — x-tca-lifecycle
//
// Contract: contracts/openapi/plot-test.yaml x-tca-lifecycle
// Sequence: stop-accepting-connections → drain-in-flight-requests → close-redis → exit-0
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
// shutdown sequence declared in x-tca-lifecycle. Each step is attempted
// independently — errors are logged but do not abort subsequent steps.
func awaitShutdown(server *http.Server) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	log.Printf("[plot-test] Received %s — starting graceful shutdown", sig)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()

	// Step 1+2: Stop accepting connections and drain in-flight requests.
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[plot-test] HTTP server shutdown error: %v", err)
	}

	// Step 3: Close Redis connection — all in-flight publishes have completed.
	if rdb != nil {
		if err := rdb.Close(); err != nil {
			log.Printf("[plot-test] Redis close error: %v", err)
		}
	}

	log.Printf("[plot-test] Shutdown complete")
	os.Exit(0)
}
