package main

// ---------------------------------------------------------------------------
// Graceful shutdown — x-tca-lifecycle
//
// Contract: contracts/openapi/signal-aggregator.yaml x-tca-lifecycle
// Sequence: stop-accepting-connections → drain-in-flight-requests →
//           unsubscribe-all-federation-channels →
//           unsubscribe-seti-events-channel → close-redis → exit-0
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
// shutdown sequence declared in x-tca-lifecycle. Federation subscriptions
// are cancelled in order before the local Redis connection closes.
func awaitShutdown(server *http.Server) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	log.Printf("[signal-aggregator] Received %s — starting graceful shutdown", sig)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()

	// Step 1+2: Stop accepting connections and drain in-flight requests.
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[signal-aggregator] HTTP server shutdown error: %v", err)
	}

	// Step 3: Cancel all federation subscription goroutines.
	// Each Subscription has a cancel func set when startSubscription was called.
	subMu.Lock()
	for appID, sub := range subscriptions {
		if sub.cancel != nil {
			sub.cancel()
			log.Printf("[signal-aggregator] Cancelled subscription for %s", appID)
		}
	}
	subMu.Unlock()

	// Step 4: Close local Redis connection.
	// All subscription goroutines have been cancelled.
	if localRDB != nil {
		if err := localRDB.Close(); err != nil {
			log.Printf("[signal-aggregator] Redis close error: %v", err)
		}
	}

	log.Printf("[signal-aggregator] Shutdown complete")
	os.Exit(0)
}
