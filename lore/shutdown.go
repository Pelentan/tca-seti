package main

// ---------------------------------------------------------------------------
// Graceful shutdown — x-tca-lifecycle
//
// Contract: contracts/openapi/lore.yaml x-tca-lifecycle
// Sequence: stop-accepting-connections → drain-in-flight-requests →
//           close-database → exit-0
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
// shutdown sequence declared in x-tca-lifecycle. The database pool is closed
// after the HTTP server drains to ensure all in-flight queries complete
// before connections are released.
func awaitShutdown(server *http.Server) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	log.Printf("[lore] Received %s — starting graceful shutdown", sig)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()

	// Step 1+2: Stop accepting connections and drain in-flight requests.
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[lore] HTTP server shutdown error: %v", err)
	}

	// Step 3: Close PostgreSQL connection pool.
	// All in-flight queries have completed — the HTTP server is drained.
	if db != nil {
		if err := db.Close(); err != nil {
			log.Printf("[lore] Database close error: %v", err)
		}
	}

	log.Printf("[lore] Shutdown complete")
	os.Exit(0)
}
