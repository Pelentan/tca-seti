"""
AI-lien Job — AI diagnostic engine for SETI.

Implements the double-tap pattern:
  First tap:  send failure context + meta-prompt to Ollama.
              The model constructs an optimal analysis prompt.
  Second tap: send the first-tap output as the actual query.
              The model returns a structured diagnostic assessment.

AI-lien is model-agnostic. It asks Policy for the active provider
and model on every request — no configuration baked in anywhere.
Change the model in the UI, next analysis uses it immediately.

The first-tap prompt is returned in every response and stored in
the Results Job audit trail. The exact question asked is always
visible — not just the raw input and output.
"""

import json
import logging
import os
import ssl
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Optional
from urllib.parse import urlparse
from urllib.request import urlopen, Request as URLRequest
import requests

logging.basicConfig(level=logging.INFO, format='[ai-lien] %(message)s')
log = logging.getLogger(__name__)

PORT = int(os.environ.get('PORT', '4252'))
POLICY_URL = os.environ.get('POLICY_URL', 'https://policy:4002')
RESULTS_URL = os.environ.get('RESULTS_URL', 'https://results:4008')
OBSERVABILITY_URL = os.environ.get('OBSERVABILITY_URL', 'https://seti-observability:4011')

_start_time = time.time()
_analyses_completed = 0
_analyses_failed = 0

# ---------------------------------------------------------------------------
# Meta-prompt templates — first tap constructs the analysis prompt.
# Add new analysis types here without contract changes.
# ---------------------------------------------------------------------------

META_PROMPTS = {
    'failure_analysis': """You are constructing a diagnostic prompt for analyzing a software test failure.

Given this failure context, construct a precise, technical prompt that will produce
a structured root cause analysis. The prompt should ask for:
1. The most likely root cause of the failure
2. Whether this appears to be a contract violation, network issue, or implementation bug
3. Specific steps to investigate and resolve
4. Confidence level (high/medium/low)

Failure context:
{context}

Respond with ONLY the diagnostic prompt text. No preamble, no explanation.""",

    'trend_analysis': """You are constructing an analysis prompt for software test trend data.

Given this trend context, construct a prompt that will identify:
1. Whether the trend indicates degradation or improvement
2. Which services or endpoints are most affected
3. Recommended actions based on the pattern
4. Risk assessment if current trend continues

Trend context:
{context}

Respond with ONLY the analysis prompt text. No preamble, no explanation.""",

    'plot_failure': """You are constructing a diagnostic prompt for a failed behavioral test (Plot test).

Given this plot failure context, construct a prompt that will identify:
1. Which step failed and why the call chain was not observed
2. Whether this is a timing issue, missing service call, or contract violation
3. Whether the Plot definition needs updating or the application behavior is wrong
4. Suggested plot revision if needed

Plot failure context:
{context}

Respond with ONLY the diagnostic prompt text. No preamble, no explanation.""",
}

DEFAULT_RESPONSE_SCHEMA = {
    "root_cause": "string — concise description of the most likely cause",
    "category": "contract_violation | network_error | implementation_bug | timing_issue | unknown",
    "confidence": "high | medium | low",
    "investigation_steps": ["list of specific steps to investigate"],
    "recommended_action": "string — what to do next",
    "requires_plot_revision": "boolean — only present for plot_failure analysis type"
}

# ---------------------------------------------------------------------------
# mTLS client
# ---------------------------------------------------------------------------

def _build_ssl_context() -> Optional[ssl.SSLContext]:
    try:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.load_verify_locations('/certs/ca.crt')
        ctx.load_cert_chain('/certs/ai-lien.crt', '/certs/ai-lien.key')
        ctx.minimum_version = ssl.TLSVersion.TLSv1_3
        return ctx
    except Exception as e:
        log.warning(f'Could not load mTLS certs: {e}')
        return None

_ssl_ctx = _build_ssl_context()

def _mtls_get(url: str) -> dict:
    req = URLRequest(url)
    with urlopen(req, context=_ssl_ctx, timeout=10) as resp:
        return json.loads(resp.read())

def _mtls_post(url: str, body: dict) -> tuple[int, dict]:
    payload = json.dumps(body).encode()
    req = URLRequest(url, data=payload, headers={'Content-Type': 'application/json'}, method='POST')
    with urlopen(req, context=_ssl_ctx, timeout=10) as resp:
        return resp.status, json.loads(resp.read())

def report_event(callee: str, method: str, path: str, status: int, latency_ms: int):
    def _send():
        try:
            _mtls_post(f'{OBSERVABILITY_URL}/event', {
                'caller': 'ai-lien', 'callee': callee,
                'method': method, 'path': path,
                'status_code': status, 'latency_ms': latency_ms, 'protocol': 'mtls',
            })
        except Exception:
            pass
    threading.Thread(target=_send, daemon=True).start()

# ---------------------------------------------------------------------------
# Active provider — ask Policy every time, never cache
# ---------------------------------------------------------------------------

def get_active_provider() -> dict:
    """Ask Policy for the current active AI provider. No caching."""
    start = time.time()
    try:
        result = _mtls_get(f'{POLICY_URL}/ai-providers/active')
        report_event('policy', 'GET', '/ai-providers/active', 200,
                     int((time.time() - start) * 1000))
        return result
    except Exception as e:
        report_event('policy', 'GET', '/ai-providers/active', 503,
                     int((time.time() - start) * 1000))
        raise RuntimeError(f'No active AI provider configured: {e}')

# ---------------------------------------------------------------------------
# Ollama API calls — plain HTTP to the provider URL
# ---------------------------------------------------------------------------

def ollama_generate(ollama_url: str, model: str, prompt: str, timeout: int = 120) -> str:
    """Call Ollama /api/generate and return the full response text."""
    start = time.time()
    try:
        resp = requests.post(
            f'{ollama_url}/api/generate',
            json={'model': model, 'prompt': prompt, 'stream': False},
            timeout=timeout,
        )
        resp.raise_for_status()
        latency = int((time.time() - start) * 1000)
        log.info(f'Ollama response: {latency}ms, model={model}')
        return resp.json().get('response', '')
    except Exception as e:
        latency = int((time.time() - start) * 1000)
        raise RuntimeError(f'Ollama call failed after {latency}ms: {e}')

# ---------------------------------------------------------------------------
# Double-tap execution
# ---------------------------------------------------------------------------

def double_tap(analysis_type: str, context: dict, response_schema: dict,
               ollama_url: str, model: str) -> dict:
    """
    Execute the double-tap pattern:
    First tap: construct the optimal analysis prompt from the context.
    Second tap: execute the analysis using that prompt.
    """
    # Select meta-prompt template
    meta_template = META_PROMPTS.get(analysis_type, META_PROMPTS['failure_analysis'])
    context_str = json.dumps(context, indent=2)
    meta_prompt = meta_template.format(context=context_str)

    # First tap — get the optimized analysis prompt
    log.info(f'First tap: constructing analysis prompt (model={model})')
    tap_one_start = time.time()
    analysis_prompt = ollama_generate(ollama_url, model, meta_prompt)
    tap_one_ms = int((time.time() - tap_one_start) * 1000)
    log.info(f'First tap complete: {tap_one_ms}ms, prompt length={len(analysis_prompt)}')

    # Second tap — execute the analysis
    schema_str = json.dumps(response_schema, indent=2)
    second_prompt = f"""{analysis_prompt}

Respond in valid JSON matching this schema exactly:
{schema_str}

Respond with ONLY the JSON object. No preamble, no explanation, no markdown."""

    log.info(f'Second tap: executing analysis (model={model})')
    tap_two_start = time.time()
    raw_response = ollama_generate(ollama_url, model, second_prompt)
    tap_two_ms = int((time.time() - tap_two_start) * 1000)
    log.info(f'Second tap complete: {tap_two_ms}ms')

    # Parse the structured response
    assessment = None
    parse_error = None
    try:
        # Strip markdown code blocks if present
        clean = raw_response.strip()
        if clean.startswith('```'):
            lines = clean.split('\n')
            clean = '\n'.join(lines[1:-1] if lines[-1] == '```' else lines[1:])
        assessment = json.loads(clean)
    except json.JSONDecodeError as e:
        parse_error = str(e)
        log.warning(f'Could not parse assessment as JSON: {e}')
        assessment = {'raw_response': raw_response, 'parse_error': str(e)}

    return {
        'first_tap_prompt': analysis_prompt,
        'first_tap_ms': tap_one_ms,
        'second_tap_ms': tap_two_ms,
        'total_ms': tap_one_ms + tap_two_ms,
        'model': model,
        'provider': ollama_url,
        'assessment': assessment,
        'parse_error': parse_error,
    }

# ---------------------------------------------------------------------------
# HTTP handler
# ---------------------------------------------------------------------------

class AILienHandler(BaseHTTPRequestHandler):

    def log_message(self, fmt, *args):
        pass

    def send_json(self, status: int, body: dict):
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', len(payload))
        self.end_headers()
        self.wfile.write(payload)

    def read_body(self) -> dict:
        length = int(self.headers.get('Content-Length', 0))
        if length == 0:
            return {}
        return json.loads(self.rfile.read(length))

    def do_GET(self):
        path = urlparse(self.path).path
        if path == '/health':
            self.send_json(200, {
                'status': 'healthy',
                'stub_active': False,
                'analyses_completed': _analyses_completed,
                'analyses_failed': _analyses_failed,
                'uptime_seconds': int(time.time() - _start_time),
            })
        else:
            self.send_json(404, {'code': 'NOT_FOUND'})

    def do_POST(self):
        global _analyses_completed, _analyses_failed
        path = urlparse(self.path).path

        if path != '/analyze':
            self.send_json(404, {'code': 'NOT_FOUND'})
            return

        body = self.read_body()
        analysis_type = body.get('analysis_type', 'failure_analysis')
        context = body.get('package', {})
        response_schema = body.get('response_schema', DEFAULT_RESPONSE_SCHEMA)
        run_id = body.get('run_id', '')

        if not context:
            self.send_json(400, {'code': 'INVALID_REQUEST', 'message': 'package is required'})
            return

        start = time.time()

        try:
            # Get active provider — fresh every time
            provider = get_active_provider()
            ollama_url = provider['ollama_url']
            model = provider['active_model']

            log.info(f'Analysis request: type={analysis_type} run_id={run_id} model={model}')

            result = double_tap(analysis_type, context, response_schema, ollama_url, model)
            result['run_id'] = run_id
            result['analysis_type'] = analysis_type
            result['analyzed_at'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())

            _analyses_completed += 1
            latency = int((time.time() - start) * 1000)
            log.info(f'Analysis complete: {latency}ms total (first={result["first_tap_ms"]}ms second={result["second_tap_ms"]}ms)')

            # Store first-tap prompt in Results Job audit trail
            threading.Thread(
                target=self._store_audit,
                args=(run_id, analysis_type, result['first_tap_prompt'], result['assessment']),
                daemon=True,
            ).start()

            self.send_json(200, result)

        except RuntimeError as e:
            _analyses_failed += 1
            log.error(f'Analysis failed: {e}')
            self.send_json(503, {
                'code': 'ANALYSIS_FAILED',
                'message': str(e),
                'run_id': run_id,
            })

    def _store_audit(self, run_id: str, analysis_type: str,
                     first_tap_prompt: str, assessment: dict):
        """Store the first-tap prompt and assessment in Results for audit trail."""
        try:
            start = time.time()
            status, _ = _mtls_post(f'{RESULTS_URL}/ai-audit', {
                'run_id': run_id,
                'analysis_type': analysis_type,
                'first_tap_prompt': first_tap_prompt,
                'assessment': assessment,
                'stored_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
            })
            report_event('results', 'POST', '/ai-audit', status,
                         int((time.time() - start) * 1000))
        except Exception as e:
            log.warning(f'Could not store audit trail: {e}')

# ---------------------------------------------------------------------------
# mTLS server
# ---------------------------------------------------------------------------

def build_server_ssl_context() -> ssl.SSLContext:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_verify_locations('/certs/ca.crt')
    ctx.load_cert_chain('/certs/ai-lien.crt', '/certs/ai-lien.key')
    ctx.verify_mode = ssl.CERT_REQUIRED
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    return ctx

if __name__ == '__main__':
    server = HTTPServer(('', PORT), AILienHandler)
    try:
        ssl_ctx = build_server_ssl_context()
        server.socket = ssl_ctx.wrap_socket(server.socket, server_side=True)
        log.info(f'Listening on :{PORT} (mTLS, TLS 1.3)')
    except Exception as e:
        log.error(f'Could not load TLS certs: {e}')
        log.error('Ensure cert-init completed before ai-lien starts')
        raise SystemExit(1)
    log.info('Double-tap Ollama diagnostic engine')
    log.info(f'Policy: {POLICY_URL} | Results: {RESULTS_URL}')
    log.info('Active provider: fetched from Policy on every request — no local config')
    server.serve_forever()
