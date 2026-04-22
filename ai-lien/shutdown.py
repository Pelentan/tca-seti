# ---------------------------------------------------------------------------
# Graceful shutdown — x-tca-lifecycle
#
# Contract: contracts/openapi/ai-lien.yaml x-tca-lifecycle
# Sequence: stop-accepting-connections → drain-in-flight-requests → exit-0
#
# terminationGracePeriodSeconds in Helm must exceed drain-timeout-seconds.
# ---------------------------------------------------------------------------

import logging
import signal
import sys
from http.server import HTTPServer

log = logging.getLogger('ai-lien')

DRAIN_TIMEOUT_SECONDS = 15


def register_shutdown(server: HTTPServer) -> None:
    """Wire SIGTERM and SIGINT handlers onto the running server.
    Call this immediately after server construction, before serve_forever().
    """

    def _handler(signum: int, _frame) -> None:
        sig_name = signal.Signals(signum).name
        log.info(f'Received {sig_name} — starting graceful shutdown')

        # Step 1+2: Stop accepting connections and drain in-flight requests.
        # shutdown() signals serve_forever() to exit after the current request.
        # server_close() releases the socket.
        try:
            server.shutdown()
            server.server_close()
        except Exception as e:
            log.error(f'Server shutdown error: {e}')

        log.info('Shutdown complete')
        sys.exit(0)

    signal.signal(signal.SIGTERM, _handler)
    signal.signal(signal.SIGINT,  _handler)
