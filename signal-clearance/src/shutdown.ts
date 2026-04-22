// ---------------------------------------------------------------------------
// Graceful shutdown — x-tca-lifecycle
//
// Contract: contracts/openapi/signal-clearance.yaml x-tca-lifecycle
// Sequence: stop-accepting-connections → drain-in-flight-requests →
//           disconnect-redis → exit-0
//
// terminationGracePeriodSeconds in Helm must exceed drain-timeout-seconds.
// ---------------------------------------------------------------------------

import https from 'https';
import { disconnectRedis } from './sessions.js';

const DRAIN_TIMEOUT_MS = 15_000;

// registerShutdown wires SIGTERM and SIGINT handlers onto the running server.
// Call this immediately after server.listen() in main.
export function registerShutdown(server: https.Server): void {
  const handler = async (signal: string) => {
    console.log(`[signal-clearance] Received ${signal} — starting graceful shutdown`);

    // Step 1+2: Stop accepting new connections and drain in-flight requests.
    // Node.js server.close() stops the listener; existing connections finish.
    const closePromise = new Promise<void>((resolve) => {
      server.close(() => resolve());
    });

    // Enforce drain timeout — don't hang forever if a connection is stuck.
    const timeout = setTimeout(() => {
      console.log('[signal-clearance] Drain timeout reached — forcing shutdown');
      process.exit(1);
    }, DRAIN_TIMEOUT_MS);

    try {
      await closePromise;
      clearTimeout(timeout);
    } catch (err) {
      console.error('[signal-clearance] Server close error:', err);
    }

    // Step 3: Disconnect Redis — all session reads have completed.
    try {
      await disconnectRedis();
    } catch (err) {
      console.error('[signal-clearance] Redis disconnect error:', err);
    }

    console.log('[signal-clearance] Shutdown complete');
    process.exit(0);
  };

  process.on('SIGTERM', () => handler('SIGTERM'));
  process.on('SIGINT',  () => handler('SIGINT'));
}
