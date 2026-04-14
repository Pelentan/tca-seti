"""
Results Job — SETI historical test results store.

Owns the historical test results database for the SETI constellation.
Stores Contract Test and Plot Test outcomes for all monitored applications.

Phase 2: in-memory storage.
Swap point: replace _store dicts with PostgreSQL in Phase 4.
"""

import base64
import hashlib
import json
import logging
import os
import ssl
from certforge import obtain_certs, build_client_ssl_context, build_server_ssl_context, self_register_with_ac
import threading
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Dict, List, Optional
from urllib.parse import urlparse
from urllib.request import urlopen, Request

logging.basicConfig(
    level=logging.INFO,
    format='[results] %(message)s'
)
log = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

PORT = int(os.environ.get('PORT', '4008'))
REDIS_URL = os.environ.get('REDIS_URL', 'redis:6379')
OBSERVABILITY_URL = os.environ.get('OBSERVABILITY_URL', 'https://seti-observability:4011')

# ---------------------------------------------------------------------------
# In-memory store — Phase 2
# Swap point: replace with PostgreSQL client in Phase 4.
# Structure chosen for easy translation to normalized DB tables.
# ---------------------------------------------------------------------------

_lock = threading.RLock()

# contract_results: run_id → run dict
_contract_results: Dict[str, dict] = {}

# plot_results: run_id → run dict (Phase 3)
_plot_results: Dict[str, dict] = {}

# index: application_id → [run_id, ...] most recent first
_contract_index: Dict[str, List[str]] = {}
_plot_index: Dict[str, List[str]] = {}

_start_time = time.time()

# ---------------------------------------------------------------------------
# mTLS client for observability reporting
# ---------------------------------------------------------------------------

def _build_ssl_context() -> Optional[ssl.SSLContext]:
    try:
        return build_client_ssl_context()
    except Exception as e:
        log.warning(f'Could not build mTLS context: {e} — observability reporting disabled')
        return None

_ssl_ctx = None  # type: Optional[ssl.SSLContext] — initialized after obtain_certs()

def report_event(callee: str, method: str, path: str, status: int, latency_ms: int):
    """Fire-and-forget observability report."""
    def _send():
        try:
            body = json.dumps({
                'caller': 'results',
                'callee': callee,
                'method': method,
                'path': path,
                'status_code': status,
                'latency_ms': latency_ms,
                'protocol': 'mtls',
            }).encode()
            req = Request(
                f'{OBSERVABILITY_URL}/event',
                data=body,
                headers={'Content-Type': 'application/json'},
                method='POST',
            )
            with urlopen(req, context=_ssl_ctx, timeout=5):
                pass
        except Exception as e:
            log.debug(f'Observability report error: {e}')

    threading.Thread(target=_send, daemon=True).start()

# ---------------------------------------------------------------------------
# Request handler
# ---------------------------------------------------------------------------

class ResultsHandler(BaseHTTPRequestHandler):

    def log_message(self, fmt, *args):
        pass  # Suppress default access log — structured logging only

    def send_json(self, status: int, body: dict):
        try:
            payload = json.dumps(body).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', len(payload))
            self.end_headers()
            self.wfile.write(payload)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def read_body(self) -> dict:
        try:
            length = int(self.headers.get('Content-Length', 0))
            if length == 0:
                return {}
            return json.loads(self.rfile.read(length))
        except (json.JSONDecodeError, ValueError):
            return {}

    def do_GET(self):
        path = urlparse(self.path).path
        start = time.time()

        if path == '/health':
            self._handle_health()
        elif path == '/contract-results':
            self._handle_list_contract_results()
        elif path.startswith('/contract-results/'):
            run_id = path[len('/contract-results/'):]
            self._handle_get_contract_result(run_id)
        elif path == '/plot-results':
            self._handle_list_plot_results()
        elif path == '/trends/contract':
            self._handle_contract_trends()
        else:
            self.send_json(404, {'code': 'NOT_FOUND', 'message': f'No route for {path}'})



    def do_POST(self):
        path = urlparse(self.path).path
        start = time.time()
        status = 200

        if path == '/contract-results':
            status = self._handle_store_contract_result()
        elif path == '/plot-results':
            status = self._handle_store_plot_result()
        else:
            self.send_json(404, {'code': 'NOT_FOUND', 'message': f'No route for {path}'})
            status = 404



    # -----------------------------------------------------------------------
    # Contract results
    # -----------------------------------------------------------------------

    def _handle_store_contract_result(self) -> int:
        run = self.read_body()
        run_id = run.get('run_id')
        app_id = run.get('application_id', 'unknown')

        if not run_id:
            self.send_json(400, {'code': 'MISSING_RUN_ID', 'message': 'run_id is required'})
            return 400

        run['stored_at'] = datetime.now(timezone.utc).isoformat()

        with _lock:
            _contract_results[run_id] = run
            if app_id not in _contract_index:
                _contract_index[app_id] = []
            # Insert at front — most recent first
            _contract_index[app_id].insert(0, run_id)
            # Keep last 100 runs per application
            _contract_index[app_id] = _contract_index[app_id][:100]

        passed = run.get('passed_tests', 0)
        failed = run.get('failed_tests', 0)
        status = run.get('status', 'unknown')
        log.info(
            f'Stored contract run {run_id} for {app_id}: '
            f'{passed} passed, {failed} failed, status={status}'
        )

        self.send_json(201, {
            'run_id': run_id,
            'application_id': app_id,
            'stored_at': run['stored_at'],
        })
        return 201

    def _handle_list_contract_results(self):
        with _lock:
            runs = []
            for run_id, run in _contract_results.items():
                runs.append({
                    'run_id': run_id,
                    'application_id': run.get('application_id'),
                    'status': run.get('status'),
                    'total_tests': run.get('total_tests', 0),
                    'passed_tests': run.get('passed_tests', 0),
                    'failed_tests': run.get('failed_tests', 0),
                    'started_at': run.get('started_at'),
                    'completed_at': run.get('completed_at'),
                })
            # Most recent first
            runs.sort(key=lambda r: r.get('started_at', ''), reverse=True)
        self.send_json(200, {'runs': runs, 'total': len(runs)})

    def _handle_get_contract_result(self, run_id: str):
        with _lock:
            run = _contract_results.get(run_id)
        if not run:
            self.send_json(404, {
                'code': 'RUN_NOT_FOUND',
                'message': f'Run {run_id} not found',
            })
            return
        self.send_json(200, run)

    # -----------------------------------------------------------------------
    # Plot results — Phase 3 stub
    # -----------------------------------------------------------------------

    def _handle_store_plot_result(self) -> int:
        run = self.read_body()
        run_id = run.get('run_id', f'plot-{int(time.time()*1000)}')
        app_id = run.get('application_id', 'unknown')
        run['stored_at'] = datetime.now(timezone.utc).isoformat()

        with _lock:
            _plot_results[run_id] = run
            if app_id not in _plot_index:
                _plot_index[app_id] = []
            _plot_index[app_id].insert(0, run_id)
            _plot_index[app_id] = _plot_index[app_id][:100]

        log.info(f'Stored plot run {run_id} for {app_id} (STUB)')
        self.send_json(201, {'run_id': run_id, 'stored_at': run['stored_at']})
        return 201

    def _handle_list_plot_results(self):
        from urllib.parse import parse_qs, urlparse
        qs = parse_qs(urlparse(self.path).query)
        filter_plot_id = qs.get('plot_id', [None])[0]
        filter_app_id  = qs.get('application_id', [None])[0]
        limit          = int(qs.get('limit', ['50'])[0])

        with _lock:
            runs = list(_plot_results.values())

        if filter_plot_id:
            runs = [r for r in runs if r.get('plot_id') == filter_plot_id]
        if filter_app_id:
            runs = [r for r in runs if r.get('application_id') == filter_app_id]

        # Most recent first
        runs.sort(key=lambda r: r.get('started_at', ''), reverse=True)
        runs = runs[:limit]

        self.send_json(200, {'runs': runs, 'total': len(runs)})

    # -----------------------------------------------------------------------
    # Trends — aggregate statistics across runs
    # -----------------------------------------------------------------------

    def _handle_contract_trends(self):
        with _lock:
            runs = list(_contract_results.values())

        if not runs:
            self.send_json(200, {
                'total_runs': 0,
                'pass_rate': None,
                'avg_tests_per_run': None,
                'by_application': {},
            })
            return

        total = len(runs)
        passed_runs = sum(1 for r in runs if r.get('status') == 'passed')
        total_tests = sum(r.get('total_tests', 0) for r in runs)

        by_app = {}
        for run in runs:
            app = run.get('application_id', 'unknown')
            if app not in by_app:
                by_app[app] = {'runs': 0, 'passed': 0, 'failed': 0, 'last_run': None}
            by_app[app]['runs'] += 1
            if run.get('status') == 'passed':
                by_app[app]['passed'] += 1
            else:
                by_app[app]['failed'] += 1
            started = run.get('started_at', '')
            if by_app[app]['last_run'] is None or started > by_app[app]['last_run']:
                by_app[app]['last_run'] = started

        self.send_json(200, {
            'total_runs': total,
            'pass_rate': round(passed_runs / total, 3) if total > 0 else None,
            'passed_runs': passed_runs,
            'failed_runs': total - passed_runs,
            'avg_tests_per_run': round(total_tests / total, 1) if total > 0 else None,
            'by_application': by_app,
        })

    # -----------------------------------------------------------------------
    # Health
    # -----------------------------------------------------------------------

    def _handle_health(self):
        with _lock:
            contract_count = len(_contract_results)
            plot_count = len(_plot_results)
            app_count = len(_contract_index)

        self.send_json(200, {
            'status': 'healthy',
            'storage': 'in-memory (Phase 2)',
            'contract_runs_stored': contract_count,
            'plot_runs_stored': plot_count,
            'applications_tracked': app_count,
            'uptime_seconds': int(time.time() - _start_time),
        })

# ---------------------------------------------------------------------------
# mTLS HTTPS server
# ---------------------------------------------------------------------------

def _build_server_ssl_context() -> ssl.SSLContext:
    return build_server_ssl_context()  # from certforge

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def self_register():
    """Sign and POST a self-registration request to Augur Canis.
    Uses openssl subprocess — available in python:slim-bookworm runtime.
    """
    import base64, datetime, subprocess

    ac_url = os.environ.get('AUGUR_CANIS_URL', 'https://augur-canis:4010')
    service_name = 'results'
    endpoint = 'https://results:4008'
    cert_path = f'/certs/results.crt'
    key_path  = f'/certs/results.key'

    try:
        timestamp = datetime.datetime.utcnow().strftime('%Y-%m-%dT%H:%M:%SZ')

        r = subprocess.run(
            ['openssl', 'x509', '-fingerprint', '-sha256', '-noout', '-in', cert_path],
            capture_output=True, text=True, check=True
        )
        fingerprint = r.stdout.strip().split('=')[-1].replace(':', '').lower()

        payload = (service_name + endpoint + fingerprint + timestamp).encode()
        r = subprocess.run(
            ['openssl', 'dgst', '-sha256', '-sign', key_path, '-binary'],
            input=payload, capture_output=True, check=True
        )
        signature_b64 = base64.b64encode(r.stdout).decode()

        body = json.dumps({
            'service_name':     service_name,
            'network_endpoint': endpoint,
            'cert_fingerprint': fingerprint,
            'timestamp':        timestamp,
            'signature':        signature_b64,
        }).encode()

        req = URLRequest(
            f'{ac_url}/services/register',
            data=body,
            headers={'Content-Type': 'application/json'},
            method='POST',
        )
        with urlopen(req, context=_ssl_ctx, timeout=10) as resp:
            ack = json.loads(resp.read())
            log.info(f'[results] selfRegister: registered with AC (status={ack.get("status")})')
    except Exception as e:
        log.warning(f'[results] selfRegister: failed: {e} — AC may not be ready yet')


class ResilientHTTPServer(HTTPServer):
    """Suppresses broken pipe and SSL errors on individual mTLS connections."""
    def handle_error(self, request, client_address):
        import sys
        exc_type = sys.exc_info()[0]
        if exc_type in (BrokenPipeError, ConnectionResetError, ssl.SSLError):
            return
        super().handle_error(request, client_address)


if __name__ == '__main__':
    # Must obtain certs FIRST — all SSL contexts depend on cert material
    obtain_certs('results')
    _ssl_ctx = _build_ssl_context()
    threading.Thread(target=self_register_with_ac, args=('results', 'https://results:4008'), daemon=True).start()
    server = ResilientHTTPServer(('', PORT), ResultsHandler)
    ssl_ctx = _build_server_ssl_context()
    server.socket = ssl_ctx.wrap_socket(
        server.socket,
        server_side=True,
    )

    log.info(f'Listening on :{PORT} (mTLS, TLS 1.3)')
    log.info('Storage: in-memory (Phase 2 — swap PostgreSQL in Phase 4)')
    log.info('Contract results: /contract-results')
    log.info('Plot results: /plot-results (stub)')
    log.info('Trends: /trends/contract')

    server.serve_forever()
