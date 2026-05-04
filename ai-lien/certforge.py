"""
cert-forge client for Python TCA services.

Obtains instance cert from cert-forge on startup.
Private key held in memory only — never written to disk or volume.
"""

import os
import ssl
import json
import time
import logging
import urllib.request
from typing import Optional, Tuple

log = logging.getLogger(__name__)

FORGE_URL       = os.environ.get('CERT_FORGE_URL', 'https://cert-forge:4014')
PUBLIC_PORT     = os.environ.get('PUBLIC_PORT', '4016')
ENROLLMENT_PORT = os.environ.get('ENROLLMENT_PORT', '4015')
ENROLLMENT_CERT = os.environ.get('ENROLLMENT_CERT', '/certs/enrollment.crt')
ENROLLMENT_KEY  = os.environ.get('ENROLLMENT_KEY', '/certs/enrollment.key')
AUGUR_CANIS_URL = os.environ.get('AUGUR_CANIS_URL', 'https://augur-canis:4010')


class CertMaterial:
    def __init__(self, ca_cert: bytes, instance_cert: bytes, instance_key: bytes,
                 fingerprint: str, instance_cn: str, instance_id: str, service_name: str):
        self.ca_cert       = ca_cert
        self.instance_cert = instance_cert
        self.instance_key  = instance_key
        self.fingerprint   = fingerprint
        self.instance_cn   = instance_cn
        self.instance_id   = instance_id
        self.service_name  = service_name


_cert_mat: Optional[CertMaterial] = None


def _derive_url(base: str, port: str) -> str:
    """Replace port in URL."""
    import re
    return re.sub(r':\d+$', f':{port}', base)


def _fetch_ca_cert() -> bytes:
    """Fetch CA cert from plain HTTP public port."""
    public_url = _derive_url(FORGE_URL, PUBLIC_PORT).replace('https://', 'http://')
    url = f'{public_url}/ca'
    for attempt in range(1, 31):
        try:
            with urllib.request.urlopen(url, timeout=5) as resp:
                data = json.loads(resp.read())
                if data.get('ca_cert'):
                    log.info('[certforge] CA cert obtained')
                    return data['ca_cert'].encode()
        except Exception as e:
            log.info(f'[certforge] Waiting for /ca (attempt {attempt}/30): {e}')
            time.sleep(2)
    raise RuntimeError('Could not obtain CA cert after 30 attempts')


def _request_instance_cert(service_name: str) -> Tuple[bytes, bytes, str, str]:
    """Request instance cert from enrollment port using enrollment cert."""
    instance_id  = os.environ.get('HOSTNAME', 'local')
    enroll_url   = _derive_url(FORGE_URL, ENROLLMENT_PORT)
    url          = f'{enroll_url}/instance-cert'

    # Enrollment mTLS context
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.load_cert_chain(ENROLLMENT_CERT, ENROLLMENT_KEY)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # enrollment CA verification done server-side
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3

    body = json.dumps({'service_name': service_name, 'instance_id': instance_id}).encode()

    for attempt in range(1, 31):
        try:
            req = urllib.request.Request(
                url, data=body,
                headers={'Content-Type': 'application/json'},
                method='POST'
            )
            with urllib.request.urlopen(req, context=ctx, timeout=10) as resp:
                data = json.loads(resp.read())
                if data.get('cert') and data.get('key'):
                    log.info(f'[certforge] Instance cert obtained (CN={data["instance_cn"]})')
                    return (
                        data['cert'].encode(),
                        data['key'].encode(),
                        data['fingerprint'],
                        data['instance_cn'],
                    )
        except Exception as e:
            log.info(f'[certforge] Waiting for /instance-cert (attempt {attempt}/30): {e}')
            time.sleep(2)
    raise RuntimeError('Could not obtain instance cert after 30 attempts')


def obtain_certs(service_name: str) -> CertMaterial:
    """Obtain all cert material — call once on startup."""
    global _cert_mat
    instance_id = os.environ.get('HOSTNAME', 'local')
    ca_cert     = _fetch_ca_cert()
    cert, key, fingerprint, instance_cn = _request_instance_cert(service_name)
    _cert_mat = CertMaterial(ca_cert, cert, key, fingerprint, instance_cn, instance_id, service_name)
    return _cert_mat


def get_cert_material() -> CertMaterial:
    if _cert_mat is None:
        raise RuntimeError('certMat not initialized — call obtain_certs() first')
    return _cert_mat


def _load_cert_chain(ctx: ssl.SSLContext, cert: bytes, key: bytes) -> None:
    """Load cert+key into SSLContext using temp files — avoids writing to volume."""
    import tempfile
    with tempfile.NamedTemporaryFile(delete=False, suffix='.crt') as cf:
        cf.write(cert)
        cert_path = cf.name
    with tempfile.NamedTemporaryFile(delete=False, suffix='.key') as kf:
        kf.write(key)
        key_path = kf.name
    try:
        ctx.load_cert_chain(cert_path, key_path)
    finally:
        os.unlink(cert_path)
        os.unlink(key_path)


def build_client_ssl_context() -> ssl.SSLContext:
    """mTLS client context for calling upstream services."""
    mat = get_cert_material()
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.load_verify_locations(cadata=mat.ca_cert.decode())
    _load_cert_chain(ctx, mat.instance_cert, mat.instance_key)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    return ctx


def build_server_ssl_context() -> ssl.SSLContext:
    """mTLS server context."""
    mat = get_cert_material()
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_verify_locations(cadata=mat.ca_cert.decode())
    _load_cert_chain(ctx, mat.instance_cert, mat.instance_key)
    ctx.verify_mode = ssl.CERT_REQUIRED
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    return ctx


def _sign_payload(payload: str) -> str:
    """Sign payload via cert-forge /sign, returns base64 signature."""
    import base64
    mat     = get_cert_material()
    ctx     = _build_sign_ctx()
    url     = f'{FORGE_URL}/sign'
    encoded = base64.b64encode(payload.encode()).decode()
    body    = json.dumps({
        'service_name': mat.service_name,
        'instance_id':  mat.instance_id,
        'payload':      encoded,
    }).encode()

    for attempt in range(1, 10**9):
        try:
            req = urllib.request.Request(
                url, data=body,
                headers={'Content-Type': 'application/json'},
                method='POST'
            )
            with urllib.request.urlopen(req, context=ctx, timeout=10) as resp:
                data = json.loads(resp.read())
                if data.get('signature'):
                    return data['signature']
                raise RuntimeError(f'Empty signature: {data}')
        except Exception as e:
            log.info(f'[certforge] /sign attempt {attempt}/10: {e}')
            time.sleep(2)
    raise RuntimeError('Could not get signature after 10 attempts')


def _build_sign_ctx() -> ssl.SSLContext:
    """mTLS context for calling cert-forge /sign."""
    mat = get_cert_material()
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.load_verify_locations(cadata=mat.ca_cert.decode())
    _load_cert_chain(ctx, mat.instance_cert, mat.instance_key)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    return ctx


def self_register_with_ac(service_name: str, endpoint: str) -> None:
    """Register with Augur Canis via cert-forge signed payload."""
    import base64
    from datetime import datetime, timezone
    mat       = get_cert_material()
    timestamp = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    payload   = service_name + endpoint + mat.fingerprint + timestamp

    for attempt in range(1, 10**9):
        try:
            signature = _sign_payload(payload)
            cert_pem  = mat.instance_cert.decode()
            body      = json.dumps({
                'service_name':     service_name,
                'network_endpoint': endpoint,
                'cert_fingerprint': mat.fingerprint,
                'cert_pem':         cert_pem,
                'timestamp':        timestamp,
                'signature':        signature,
            }).encode()

            ctx = _build_sign_ctx()
            req = urllib.request.Request(
                f'{AUGUR_CANIS_URL}/services/register',
                data=body,
                headers={'Content-Type': 'application/json'},
                method='POST'
            )
            with urllib.request.urlopen(req, context=ctx, timeout=10) as resp:
                ack = json.loads(resp.read())
                log.info(f'[{service_name}] selfRegister: registered with AC (status={ack.get("status")})')
                return
        except Exception as e:
            log.info(f'[{service_name}] selfRegister: attempt {attempt}/10: {e}')
            time.sleep(3)
    
