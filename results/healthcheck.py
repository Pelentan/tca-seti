#!/usr/bin/env python3
"""
Healthcheck for results (distroless/python3 container).
Raw RESP protocol over socket — zero external dependencies.
Same Watchdog Redis pub/sub mechanism as Go healthcheck binary.
"""

import os
import socket
import time
import secrets
import json
import sys

REDIS_ADDR = os.environ.get('REDIS_URL', 'redis:6379').split(':')
REDIS_HOST = REDIS_ADDR[0]
REDIS_PORT = int(REDIS_ADDR[1]) if len(REDIS_ADDR) > 1 else 6379
SERVICE_NAME = os.environ.get('SERVICE_NAME', 'results')
TIMEOUT_MS = int(os.environ.get('CHECK_TIMEOUT_MS', '8000'))
CONTAINER_ID = os.environ.get('HOSTNAME', 'unknown')

request_id = f'chk-{secrets.token_hex(8)}'
result_channel = f'tca:check-results:{request_id}'

def resp(*args):
    """Build a RESP command."""
    out = f'*{len(args)}\r\n'
    for arg in args:
        s = str(arg)
        out += f'${len(s.encode())}\r\n{s}\r\n'
    return out.encode()

def exit_degraded_healthy(reason):
    print(f'[healthcheck] {reason} — degraded-healthy', file=sys.stderr)
    sys.exit(0)

def exit_unhealthy(reason):
    print(f'[healthcheck] {reason}', file=sys.stderr)
    sys.exit(1)

try:
    # Subscriber connection — subscribe before publishing to avoid race
    sub_sock = socket.create_connection((REDIS_HOST, REDIS_PORT), timeout=3)
    sub_sock.sendall(resp('SUBSCRIBE', result_channel))
    # Read subscribe confirmation
    sub_sock.recv(256)

    # Publisher connection — send check request then close
    pub_sock = socket.create_connection((REDIS_HOST, REDIS_PORT), timeout=3)
    payload = json.dumps({
        'request_id': request_id,
        'container_id': CONTAINER_ID,
        'service_name': SERVICE_NAME,
        'published_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
    })
    pub_sock.sendall(resp('PUBLISH', 'tca:check-requests', payload))
    pub_sock.recv(64)
    pub_sock.close()

    # Wait for Watchdog response
    deadline = time.time() + (TIMEOUT_MS / 1000)
    sub_sock.settimeout(TIMEOUT_MS / 1000)
    buf = b''

    while time.time() < deadline:
        try:
            chunk = sub_sock.recv(4096)
            if not chunk:
                break
            buf += chunk

            # Find JSON object in buffer
            start = buf.find(b'{')
            if start == -1:
                continue

            depth = 0
            end = -1
            for i in range(start, len(buf)):
                if buf[i:i+1] == b'{':
                    depth += 1
                elif buf[i:i+1] == b'}':
                    depth -= 1
                    if depth == 0:
                        end = i
                        break

            if end == -1:
                continue

            result = json.loads(buf[start:end+1])
            sub_sock.close()

            if result.get('healthy') == 1:
                sys.exit(0)
            else:
                exit_unhealthy(f"Unhealthy: {result.get('failure_reason', 'unknown')}")

        except socket.timeout:
            break

    sub_sock.close()
    exit_degraded_healthy('Timeout waiting for Watchdog response')

except OSError as e:
    exit_degraded_healthy(f'Redis connection error: {e}')
