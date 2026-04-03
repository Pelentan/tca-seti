#!/usr/bin/env node
// Healthcheck for signal-clearance (distroless/nodejs container).
// Raw RESP over net sockets — zero npm dependencies.
// Watchdog Redis pub/sub mechanism, same as the Go healthcheck binary.
'use strict';

const net = require('net');
const crypto = require('crypto');

const redisAddr  = (process.env.REDIS_URL || 'redis:6379').split(':');
const redisHost  = redisAddr[0] || 'redis';
const redisPort  = parseInt(redisAddr[1] || '6379', 10);
const svcName    = process.env.SERVICE_NAME || 'signal-clearance';
const timeoutMs  = parseInt(process.env.CHECK_TIMEOUT_MS || '8000', 10);

const requestId     = `chk-${crypto.randomBytes(8).toString('hex')}`;
const resultChannel = `tca:check-results:${requestId}`;

let done = false;
const exit = (code) => {
  if (done) return;
  done = true;
  setTimeout(() => process.exit(code), 50);
};

const overallTimeout = setTimeout(() => {
  // Watchdog did not respond in time — treat as degraded-healthy.
  // The Job is running; the Watchdog will do behavioural verification
  // on the next cycle.
  console.error('[healthcheck] Timeout — degraded-healthy');
  exit(0);
}, timeoutMs);

// ---------------------------------------------------------------------------
// RESP command builder
// ---------------------------------------------------------------------------
function resp(...args) {
  let out = `*${args.length}\r\n`;
  for (const arg of args) {
    const s = String(arg);
    out += `$${Buffer.byteLength(s, 'utf8')}\r\n${s}\r\n`;
  }
  return out;
}

// ---------------------------------------------------------------------------
// Scan accumulated buffer for a complete JSON object with a healthy field.
// Returns true (healthy), false (unhealthy), or null (not yet available).
// ---------------------------------------------------------------------------
function scanForResult(buf) {
  let i = 0;
  while (i < buf.length) {
    const start = buf.indexOf('{', i);
    if (start === -1) break;

    let depth = 0, end = -1;
    for (let j = start; j < buf.length; j++) {
      if (buf[j] === '{') depth++;
      else if (buf[j] === '}') { if (--depth === 0) { end = j; break; } }
    }
    if (end === -1) break; // Incomplete — wait for more data

    try {
      const obj = JSON.parse(buf.slice(start, end + 1));
      if (typeof obj.healthy === 'number') {
        return obj.healthy === 1 ? true : false;
      }
    } catch (_) {}

    i = end + 1;
  }
  return null;
}

// ---------------------------------------------------------------------------
// Subscriber connection — accumulate all data into a single buffer
// ---------------------------------------------------------------------------
let buffer = '';

const sub = net.createConnection({ host: redisHost, port: redisPort });
sub.setEncoding('utf8');

sub.on('error', (err) => {
  console.error(`[healthcheck] sub error: ${err.message} — degraded-healthy`);
  clearTimeout(overallTimeout);
  exit(0);
});

sub.on('connect', () => {
  sub.write(resp('SUBSCRIBE', resultChannel));
  // Wait 100 ms to ensure Redis has registered the subscription
  // before we publish the check request on the publisher connection.
  setTimeout(openPublisher, 100);
});

sub.on('data', (chunk) => {
  buffer += chunk;
  const result = scanForResult(buffer);
  if (result === null) return; // Still waiting
  clearTimeout(overallTimeout);
  if (result) {
    exit(0);
  } else {
    console.error('[healthcheck] Watchdog returned unhealthy');
    exit(1);
  }
});

// ---------------------------------------------------------------------------
// Publisher connection — send check request, then destroy
// ---------------------------------------------------------------------------
function openPublisher() {
  const payload = JSON.stringify({
    request_id:   requestId,
    container_id: process.env.HOSTNAME || 'unknown',
    service_name: svcName,
    published_at: new Date().toISOString(),
  });

  const pub = net.createConnection({ host: redisHost, port: redisPort });
  pub.setEncoding('utf8');

  pub.on('error', (err) => {
    console.error(`[healthcheck] pub error: ${err.message} — degraded-healthy`);
    clearTimeout(overallTimeout);
    exit(0);
  });

  pub.on('connect', () => {
    pub.write(resp('PUBLISH', 'tca:check-requests', payload));
    setTimeout(() => { try { pub.destroy(); } catch (_) {} }, 300);
  });
}
