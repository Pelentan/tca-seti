/**
 * cert-forge client for signal-clearance.
 *
 * Obtains instance cert from cert-forge on startup.
 * Private key held in memory only — never written to disk.
 */

import https from 'https';
import http from 'http';
import fs from 'fs';

const FORGE_URL      = process.env.CERT_FORGE_URL      ?? 'https://cert-forge:4014';
const PUBLIC_PORT    = process.env.PUBLIC_PORT          ?? '4016';
const ENROLLMENT_PORT = process.env.ENROLLMENT_PORT     ?? '4015';
const ENROLLMENT_CERT = process.env.ENROLLMENT_CERT     ?? '/certs/enrollment.crt';
const ENROLLMENT_KEY  = process.env.ENROLLMENT_KEY      ?? '/certs/enrollment.key';
const SERVICE_NAME   = 'signal-clearance';
const ENDPOINT       = 'https://signal-clearance:4001';

export interface CertMaterial {
  caCert:      Buffer;
  instanceCert: Buffer;
  instanceKey:  Buffer;
  fingerprint:  string;
  instanceCN:   string;
  instanceID:   string;
}

let certMat: CertMaterial | null = null;

export function getCertMaterial(): CertMaterial {
  if (!certMat) throw new Error('certMat not yet initialized — call obtainCerts() first');
  return certMat;
}

/** Derive URL with a different port */
function deriveURL(base: string, port: string): string {
  return base.replace(/:\d+$/, `:${port}`);
}

/** Fetch CA cert from plain HTTP public port */
async function fetchCACert(): Promise<Buffer> {
  const publicBase = deriveURL(FORGE_URL, PUBLIC_PORT).replace('https://', 'http://');
  const url = `${publicBase}/ca`;

  for (let attempt = 1; attempt <= 30; attempt++) {
    try {
      const body = await httpGet(url);
      const parsed = JSON.parse(body);
      if (parsed.ca_cert) {
        console.log('[certforge] CA cert obtained');
        return Buffer.from(parsed.ca_cert);
      }
    } catch (e) {
      console.log(`[certforge] Waiting for /ca (attempt ${attempt}/30): ${e}`);
    }
    await sleep(2000);
  }
  throw new Error('Could not obtain CA cert after 30 attempts');
}

/** Request instance cert from enrollment port using enrollment cert */
async function requestInstanceCert(caCert: Buffer): Promise<{ cert: Buffer; key: Buffer; fingerprint: string; instanceCN: string }> {
  const enrollCert = fs.readFileSync(ENROLLMENT_CERT);
  const enrollKey  = fs.readFileSync(ENROLLMENT_KEY);
  const instanceID = process.env.HOSTNAME ?? 'local';
  const enrollURL  = deriveURL(FORGE_URL, ENROLLMENT_PORT);
  const url        = `${enrollURL}/instance-cert`;

  const agent = new https.Agent({
    ca:                 caCert,
    cert:               enrollCert,
    key:                enrollKey,
    rejectUnauthorized: true,
    minVersion:         'TLSv1.3',
  });

  const body = JSON.stringify({ service_name: SERVICE_NAME, instance_id: instanceID });

  for (let attempt = 1; attempt <= 30; attempt++) {
    try {
      const respBody = await httpsPost(url, body, agent);
      const parsed = JSON.parse(respBody);
      if (parsed.cert && parsed.key) {
        console.log(`[certforge] Instance cert obtained (CN=${parsed.instance_cn})`);
        return {
          cert:        Buffer.from(parsed.cert),
          key:         Buffer.from(parsed.key),
          fingerprint: parsed.fingerprint,
          instanceCN:  parsed.instance_cn,
        };
      }
    } catch (e) {
      console.log(`[certforge] Waiting for /instance-cert (attempt ${attempt}/30): ${e}`);
    }
    await sleep(2000);
  }
  throw new Error('Could not obtain instance cert after 30 attempts');
}

/** Obtain all cert material — call once on startup */
export async function obtainCerts(): Promise<CertMaterial> {
  const instanceID = process.env.HOSTNAME ?? 'local';
  const caCert     = await fetchCACert();
  const { cert, key, fingerprint, instanceCN } = await requestInstanceCert(caCert);

  certMat = { caCert, instanceCert: cert, instanceKey: key, fingerprint, instanceCN, instanceID };
  return certMat;
}

/** Build mTLS server options */
export function buildMTLSOptions(): https.ServerOptions {
  const mat = getCertMaterial();
  return {
    ca:                 mat.caCert,
    cert:               mat.instanceCert,
    key:                mat.instanceKey,
    requestCert:        true,
    rejectUnauthorized: true,
    minVersion:         'TLSv1.3',
  };
}

/** Build mTLS upstream agent */
export function buildUpstreamAgent(): https.Agent {
  const mat = getCertMaterial();
  return new https.Agent({
    ca:         mat.caCert,
    cert:       mat.instanceCert,
    key:        mat.instanceKey,
    minVersion: 'TLSv1.3',
  });
}

/** Sign payload via cert-forge /sign, returns base64 signature */
export async function signPayload(payload: string): Promise<string> {
  const mat    = getCertMaterial();
  const agent  = buildUpstreamAgent();
  const url    = `${FORGE_URL}/sign`;
  const encoded = Buffer.from(payload).toString('base64');
  const body   = JSON.stringify({
    service_name: SERVICE_NAME,
    instance_id:  mat.instanceID,
    payload:      encoded,
  });

  for (let attempt = 1; attempt <= 10; attempt++) {
    try {
      const respBody = await httpsPost(url, body, agent);
      const parsed   = JSON.parse(respBody);
      if (parsed.signature) return parsed.signature;
      throw new Error(`empty signature: ${respBody}`);
    } catch (e) {
      console.log(`[certforge] /sign attempt ${attempt}/10: ${e}`);
      await sleep(2000);
    }
  }
  throw new Error('Could not get signature after 10 attempts');
}

/** Register with Augur Canis */
export async function selfRegisterWithAC(): Promise<void> {
  const mat       = getCertMaterial();
  const acURL     = process.env.AUGUR_CANIS_URL ?? 'https://augur-canis:4010';
  const timestamp = new Date().toISOString().replace(/\.\d+Z$/, 'Z');
  const payload   = SERVICE_NAME + ENDPOINT + mat.fingerprint + timestamp;

  // Extract cert PEM for the registration payload
  const certPEM = mat.instanceCert.toString('utf8');

  for (let attempt = 1; attempt <= 10; attempt++) {
    try {
      const signature = await signPayload(payload);
      const body = JSON.stringify({
        service_name:     SERVICE_NAME,
        network_endpoint: ENDPOINT,
        cert_fingerprint: mat.fingerprint,
        cert_pem:         certPEM,
        timestamp,
        signature,
      });
      const agent = buildUpstreamAgent();
      const respBody = await httpsPost(`${acURL}/services/register`, body, agent);
      const ack = JSON.parse(respBody);
      console.log(`[signal-clearance] selfRegister: registered with AC (status=${ack.status})`);
      return;
    } catch (e) {
      console.log(`[signal-clearance] selfRegister: attempt ${attempt}/10: ${e}`);
      await sleep(3000);
    }
  }
  console.log('[signal-clearance] selfRegister: giving up after 10 attempts');
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

function httpGet(url: string): Promise<string> {
  return new Promise((resolve, reject) => {
    http.get(url, res => {
      let data = '';
      res.on('data', c => { data += c; });
      res.on('end', () => resolve(data));
    }).on('error', reject);
  });
}

function httpsPost(url: string, body: string, agent: https.Agent): Promise<string> {
  return new Promise((resolve, reject) => {
    const u = new URL(url);
    const req = https.request({
      hostname: u.hostname,
      port:     u.port || 443,
      path:     u.pathname,
      method:   'POST',
      headers:  { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
      agent,
    }, res => {
      let data = '';
      res.on('data', c => { data += c; });
      res.on('end', () => {
        if (res.statusCode && res.statusCode >= 400) {
          reject(new Error(`HTTP ${res.statusCode}: ${data}`));
        } else {
          resolve(data);
        }
      });
    });
    req.on('error', reject);
    req.write(body);
    req.end();
  });
}

function sleep(ms: number): Promise<void> {
  return new Promise(r => setTimeout(r, ms));
}
