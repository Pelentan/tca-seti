import https from 'https';
import { buildUpstreamAgent } from './mtls.js';

const OBSERVABILITY_URL = process.env.OBSERVABILITY_URL || 'https://seti-observability:4011';

export function reportEvent(params: {
  callee: string;
  method: string;
  path: string;
  status_code: number;
  latency_ms: number;
  protocol?: string;
}): void {
  // Fire-and-forget — never block on this
  setImmediate(() => {
    const body = JSON.stringify({
      caller: 'signal-clearance',
      callee: params.callee,
      method: params.method,
      path: params.path,
      status_code: params.status_code,
      latency_ms: params.latency_ms,
      protocol: params.protocol ?? 'mtls',
    });

    const url = new URL(`${OBSERVABILITY_URL}/event`);
    const req = https.request(
      {
        hostname: url.hostname,
        port: url.port || 443,
        path: url.pathname,
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(body),
        },
        agent: buildUpstreamAgent(),
      },
      (res) => {
        res.resume(); // Drain response
      }
    );

    req.on('error', (err) => {
      console.error(`[signal-clearance] observability report error: ${err.message}`);
    });

    req.write(body);
    req.end();
  });
}
