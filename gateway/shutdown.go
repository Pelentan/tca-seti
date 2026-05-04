package main

// ---------------------------------------------------------------------------
// Graceful shutdown — x-tca-lifecycle
//
// Contract: contracts/openapi/gateway.yaml x-tca-lifecycle
// Sequence: stop-accepting-connections → drain-in-flight-requests →
//           close-sse-client-connections → unsubscribe-seti-events-channel →
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

// awaitShutdown blocks until SIGTERM or SIGINT is received, then executes
// the graceful shutdown sequence declared in x-tca-lifecycle. Each step is
// attempted independently — errors are logged but do not abort subsequent
// steps. The drain timeout is the outer bound; SIGKILL from Kubernetes
// enforces the hard deadline via terminationGracePeriodSeconds.
func awaitShutdown(server *http.Server) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	log.Printf("[gateway] Received %s — starting graceful shutdown", sig)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()

	// Step 1+2: Stop accepting connections and drain in-flight requests.
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[gateway] HTTP server shutdown error: %v", err)
	}

	// Step 3: Close SSE client connections — clients reading from closed
	// channels cause goroutine leaks. Must happen after HTTP server stops
	// accepting but before Redis subscription is cancelled.
	sseMu.Lock()
	for c := range sseClients {
		close(c.ch)
	}
	sseClients = map[*sseClient]struct{}{}
	sseMu.Unlock()

	// Step 4+5: Cancel Redis subscription goroutine and let it exit cleanly.
	if redisShutdown != nil {
		redisShutdown()
	}

	log.Printf("[gateway] Shutdown complete")
	os.Exit(0)
}
