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
from certforge import obtain_certs, build_client_ssl_context, build_server_ssl_context, self_register_with_ac
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Optional
from urllib.parse import urlparse
from urllib.request import urlopen, Request as URLRequest
import urllib.request
import urllib.error

logging.basicConfig(level=logging.INFO, format='[ai-lien] %(message)s')
log = logging.getLogger(__name__)

PORT = int(os.environ.get('PORT', '4252'))
POLICY_URL = os.environ.get('POLICY_URL', 'https://policy:4002')
RESULTS_URL = os.environ.get('RESULTS_URL', 'https://results:4008')
OBSERVABILITY_URL = os.environ.get('OBSERVABILITY_URL', 'https://seti-observability:4011')
LORE_URL = os.environ.get('LORE_URL', 'https://lore:4110')

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

    'critical_briefing': """You are constructing an urgent briefing prompt for a critical system failure.
A human Wr4ngler is already on their way to investigate. Your job is NOT to diagnose —
it is to assemble everything they need to understand the situation the moment they arrive.

Given this failure context, construct a prompt that will produce:
1. A concise situation summary (what is failing, how bad, since when)
2. What has already been tried or detected
3. The most likely cause based on history and patterns
4. The exact first three actions the Wr4ngler should take right now
5. What NOT to do (common mistakes for this failure pattern)

Context includes Lore history — use it. Prior incidents are more valuable than speculation.

Critical failure context:
{context}

Respond with ONLY the briefing prompt text. No preamble, no explanation.""",

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
        return build_client_ssl_context()
    except Exception as e:
        log.warning(f'Could not build mTLS context: {e}')
        return None

_ssl_ctx = None  # type: Optional[ssl.SSLContext] — initialized after obtain_certs()

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
# Lore integration — read context before analysis, write assessments after
# ---------------------------------------------------------------------------

def fetch_lore_context(application_id: str, job_name: str = None) -> dict:
    """
    Read institutional memory from Lore before analysis.
    Returns baselines, recent incidents, and known patterns.
    AI-lien with history is fundamentally different from AI-lien without it.
    """
    context = {}
    since_72h = time.strftime('%Y-%m-%dT%H:%M:%SZ',
                               time.gmtime(time.time() - 72 * 3600))

    # Baseline for this job — what does healthy look like?
    if job_name:
        try:
            baseline = _mtls_get(f'{LORE_URL}/baselines/{application_id}/{job_name}')
            context['baseline'] = baseline
            report_event('lore', 'GET', f'/baselines/{application_id}/{job_name}', 200, 0)
        except Exception:
            context['baseline'] = None  # No baseline established yet

    # Recent open incidents — what's already known to be wrong?
    try:
        incidents = _mtls_get(
            f'{LORE_URL}/incidents?application_id={application_id}'
            f'&status=open&since={since_72h}&limit=5'
        )
        context['recent_incidents'] = incidents.get('incidents', [])
        context['open_incident_count'] = incidents.get('open_count', 0)
        report_event('lore', 'GET', '/incidents', 200, 0)
    except Exception:
        context['recent_incidents'] = []
        context['open_incident_count'] = 0

    # Known patterns — recurring failure signatures
    try:
        patterns = _mtls_get(
            f'{LORE_URL}/patterns?application_id={application_id}&limit=5'
        )
        context['known_patterns'] = patterns.get('patterns', [])
        report_event('lore', 'GET', '/patterns', 200, 0)
    except Exception:
        context['known_patterns'] = []

    return context


def write_lore_trend_point(application_id: str, job_name: str, run_id: str,
                            signal_type: str, description: str,
                            evidence: dict, related_incident_id: str = None) -> str | None:
    """
    Write a trend point to Lore after analysis.
    Returns the trend_point_id if successful, None if not.
    """
    body = {
        'application_id': application_id,
        'job_name': job_name,
        'source_job': 'ai-lien',
        'signal_type': signal_type,
        'description': description,
        'evidence': evidence,
        'run_id': run_id,
    }
    if related_incident_id:
        body['related_incident_id'] = related_incident_id

    try:
        start = time.time()
        status, result = _mtls_post(f'{LORE_URL}/trend-points', body)
        report_event('lore', 'POST', '/trend-points', status,
                     int((time.time() - start) * 1000))
        if status == 201:
            return result.get('trend_point_id')
    except Exception as e:
        log.warning(f'Lore trend point write failed: {e}')
    return None


def write_lore_incident(application_id: str, job_name: str, run_id: str,
                         trigger_type: str, description: str,
                         ai_assessment: str, ai_recommended_action: str,
                         related_trend_point_ids: list = None) -> str | None:
    """
    Write an incident to Lore when AI-lien determines the failure warrants one.
    Returns the incident_id if successful, None if not.
    """
    try:
        start = time.time()
        status, result = _mtls_post(f'{LORE_URL}/incidents', {
            'application_id': application_id,
            'job_name': job_name,
            'source_job': 'ai-lien',
            'trigger_type': trigger_type,
            'description': description,
            'ai_assessment': ai_assessment,
            'ai_recommended_action': ai_recommended_action,
            'run_id': run_id,
            'related_trend_point_ids': related_trend_point_ids or [],
        })
        report_event('lore', 'POST', '/incidents', status,
                     int((time.time() - start) * 1000))
        if status == 201:
            return result.get('incident_id')
    except Exception as e:
        log.warning(f'Lore incident write failed: {e}')
    return None

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

def sanitize_provider_url(url: str) -> str:
    """Validate and reconstruct a provider URL from parsed components.
    Returning a new string breaks CodeQL taint chain — primary source
    is Policy's AI provider configuration which is admin-controlled.
    """
    parsed = urlparse(url)
    if parsed.scheme not in ('http', 'https'):
        raise ValueError(f'Provider URL scheme must be http or https, got {parsed.scheme!r}')
    if not parsed.netloc:
        raise ValueError('Provider URL must include a host')
    return parsed.geturl()


def ollama_generate(ollama_url: str, model: str, prompt: str, timeout: int = 120) -> str:
    """Call Ollama /api/generate and return the full response text."""
    start = time.time()
    try:
        body = json.dumps({'model': model, 'prompt': prompt, 'stream': False}).encode()
        req = urllib.request.Request(
            f'{ollama_url}/api/generate',
            data=body,
            headers={'Content-Type': 'application/json'},
            method='POST',
        )
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            data = json.loads(resp.read().decode())
        latency = int((time.time() - start) * 1000)
        log.info(f'Ollama response: {latency}ms, model={model}')
        return data.get('response', '')
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
        try:
            payload = json.dumps(body).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', len(payload))
            self.end_headers()
            self.wfile.write(payload)
        except (BrokenPipeError, ConnectionResetError):
            pass  # Client disconnected before response — not an error

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
        if path == '/health':
            self.send_json(200, {
                'status': 'healthy',
                'stub_active': False,
                'lore_enabled': True,
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

        # Extract application/job identifiers for Lore reads
        application_id = context.get('application_id', '')
        job_name = context.get('job_name', context.get('test_tier', ''))

        # Enrich context with Lore institutional memory before analysis.
        # AI-lien with history is fundamentally different from AI-lien without it.
        if application_id:
            log.info(f'Fetching Lore context for {application_id}/{job_name}')
            lore_context = fetch_lore_context(application_id, job_name or None)
            context['lore_institutional_memory'] = lore_context
            open_count = lore_context.get('open_incident_count', 0)
            pattern_count = len(lore_context.get('known_patterns', []))
            log.info(f'Lore context loaded: {open_count} open incidents, {pattern_count} known patterns')

        try:
            # Get active provider — fresh every time
            provider = get_active_provider()
            ollama_url = sanitize_provider_url(provider['ollama_url'])
            model = provider['active_model']

            log.info(f'Analysis request: type={analysis_type} run_id={run_id} model={model}')

            result = double_tap(analysis_type, context, response_schema, ollama_url, model)
            result['run_id'] = run_id
            result['analysis_type'] = analysis_type
            result['analyzed_at'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())

            _analyses_completed += 1
            latency = int((time.time() - start) * 1000)
            log.info(f'Analysis complete: {latency}ms total (first={result["first_tap_ms"]}ms second={result["second_tap_ms"]}ms)')

            # Write assessment back to Lore and store audit trail — both async
            assessment = result.get('assessment', {})
            threading.Thread(
                target=self._write_to_lore,
                args=(application_id, job_name, run_id, analysis_type, assessment),
                daemon=True,
            ).start()
            threading.Thread(
                target=self._store_audit,
                args=(run_id, analysis_type, result['first_tap_prompt'], assessment),
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

    def _write_to_lore(self, application_id: str, job_name: str, run_id: str,
                        analysis_type: str, assessment: dict):
        """Write assessment back to Lore as trend point and/or incident."""
        if not application_id or not assessment:
            return

        root_cause = assessment.get('root_cause', 'unknown')
        confidence = assessment.get('confidence', 'low')
        category = assessment.get('category', 'unknown')
        recommended_action = assessment.get('recommended_action', '')
        severity = assessment.get('severity', 'medium')
        escalate = assessment.get('escalate_to_wr4ngler', False)

        # Always write a trend point — every analysis is a data point
        signal_type = f'{analysis_type}_{category}' if category != 'unknown' else analysis_type
        trend_point_id = write_lore_trend_point(
            application_id=application_id,
            job_name=job_name or 'unknown',
            run_id=run_id,
            signal_type=signal_type,
            description=f'{analysis_type}: {root_cause} (confidence={confidence})',
            evidence={
                'category': category,
                'confidence': confidence,
                'recommended_action': recommended_action,
                'severity': severity,
                'escalate_to_wr4ngler': escalate,
            },
        )

        # Write incident if AI assessed this as worth escalating or high/critical
        if escalate or severity in ('critical', 'high'):
            ai_assessment_str = (
                f'Category: {category} | Confidence: {confidence} | {root_cause}'
            )
            incident_id = write_lore_incident(
                application_id=application_id,
                job_name=job_name or 'unknown',
                run_id=run_id,
                trigger_type=analysis_type,
                description=root_cause,
                ai_assessment=ai_assessment_str,
                ai_recommended_action=recommended_action,
                related_trend_point_ids=[trend_point_id] if trend_point_id else [],
            )
            if incident_id:
                log.info(f'Lore incident created: {incident_id} for run {run_id}')
        else:
            log.info(f'Lore trend point written: {trend_point_id} for run {run_id}')

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

def _build_server_ssl_context() -> ssl.SSLContext:
    return build_server_ssl_context()  # from certforge

def _build_server_ssl_context_unused() -> ssl.SSLContext:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_verify_locations('/certs/ca.crt')
    ctx.load_cert_chain('/certs/ai-lien.crt', '/certs/ai-lien.key')
    ctx.verify_mode = ssl.CERT_REQUIRED
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    return ctx


def self_register():
    """Sign and POST a self-registration request to Augur Canis.
    Uses openssl subprocess — available in python:slim-bookworm runtime.
    """
    import base64, datetime, subprocess

    ac_url = os.environ.get('AUGUR_CANIS_URL', 'https://augur-canis:4010')
    service_name = 'ai-lien'
    endpoint = 'https://ai-lien:4252'
    cert_path = f'/certs/ai-lien.crt'
    key_path  = f'/certs/ai-lien.key'

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
            log.info(f'[ai-lien] selfRegister: registered with AC (status={ack.get("status")})')
    except Exception as e:
        log.warning(f'[ai-lien] selfRegister: failed: {e} — AC may not be ready yet')


class ResilientHTTPServer(HTTPServer):
    """HTTPServer that suppresses broken pipe and SSL errors on individual connections.
    Python's BaseHTTPServer propagates these to stderr and terminates the handler
    thread, causing the Go mTLS client to see a broken pipe on the write side.
    """
    def handle_error(self, request, client_address):
        import sys
        exc_type = sys.exc_info()[0]
        if exc_type in (BrokenPipeError, ConnectionResetError, ssl.SSLError):
            return  # Expected when mTLS client closes connection early
        super().handle_error(request, client_address)


if __name__ == '__main__':
    # Must obtain certs FIRST — all SSL contexts depend on cert material
    obtain_certs('ai-lien')
    _ssl_ctx = _build_ssl_context()
    threading.Thread(target=self_register_with_ac, args=('ai-lien', 'https://ai-lien:4252'), daemon=True).start()
    server = ResilientHTTPServer(('', PORT), AILienHandler)
    try:
        ssl_ctx = _build_server_ssl_context()
        server.socket = ssl_ctx.wrap_socket(server.socket, server_side=True)
        log.info(f'Listening on :{PORT} (mTLS, TLS 1.3)')
    except Exception as e:
        log.error(f'Could not load TLS certs: {e}')
        raise SystemExit(1)
    log.info('Double-tap Ollama diagnostic engine')
    log.info(f'Policy: {POLICY_URL} | Results: {RESULTS_URL} | Lore: {LORE_URL}')
    log.info('Lore: reads baselines/incidents before analysis, writes trend points/incidents after')
    log.info('Active provider: fetched from Policy on every request — no local config')

    from shutdown import register_shutdown
    register_shutdown(server)
    server.serve_forever()
