import fs from 'fs';
import crypto from 'crypto';
import { SETIJWTClaims } from './types.js';

// JWT secret is written by cert-forge to /certs/jwt-secret.
// JWT_SECRET_FILE points to that path. Never passed as a plain env var.
const jwtSecretFile = process.env.JWT_SECRET_FILE;
if (!jwtSecretFile) {
  console.error('[signal-clearance] FATAL: JWT_SECRET_FILE environment variable not set');
  process.exit(1);
}
let JWT_SECRET: string;
try {
  JWT_SECRET = fs.readFileSync(jwtSecretFile, 'utf8').trim();
} catch (e) {
  console.error(`[signal-clearance] FATAL: Could not read JWT secret from ${jwtSecretFile}: ${e}`);
  process.exit(1);
}
if (!JWT_SECRET) {
  console.error('[signal-clearance] FATAL: JWT secret file is empty');
  process.exit(1);
}

const JWT_TTL_SECONDS = 15 * 60; // 15 minutes

// ---------------------------------------------------------------------------
// Stdlib HS256 JWT — no external dependencies.
// ---------------------------------------------------------------------------

function base64url(data: Buffer | string): string {
  const buf = typeof data === 'string' ? Buffer.from(data) : data;
  return buf.toString('base64url');
}

function base64urlDecode(s: string): Buffer {
  return Buffer.from(s, 'base64url');
}

const HEADER = base64url(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));

function hmacSHA256(data: string): Buffer {
  return crypto.createHmac('sha256', JWT_SECRET).update(data).digest();
}

export function issueJWT(params: {
  wrangler_id: string;
  clearance_level: string;
  identity_source: string;
}): { token: string; expires_at: string } {
  const now = Math.floor(Date.now() / 1000);
  const exp = now + JWT_TTL_SECONDS;

  const payload = base64url(JSON.stringify({
    wrangler_id: params.wrangler_id,
    clearance_level: params.clearance_level,
    identity_source: params.identity_source,
    iat: now,
    exp,
  }));

  const body = `${HEADER}.${payload}`;
  const sig = base64url(hmacSHA256(body));
  const token = `${body}.${sig}`;
  const expires_at = new Date(exp * 1000).toISOString();

  return { token, expires_at };
}

export function verifyJWT(token: string): SETIJWTClaims | null {
  try {
    const parts = token.split('.');
    if (parts.length !== 3) return null;

    // Verify header alg
    const header = JSON.parse(base64urlDecode(parts[0]).toString());
    if (header.alg !== 'HS256') return null;

    // Verify signature
    const body = `${parts[0]}.${parts[1]}`;
    const expected = hmacSHA256(body);
    const provided = base64urlDecode(parts[2]);
    if (!crypto.timingSafeEqual(expected, provided)) return null;

    // Decode and validate claims
    const claims = JSON.parse(base64urlDecode(parts[1]).toString()) as SETIJWTClaims;
    if (Math.floor(Date.now() / 1000) > claims.exp) return null;

    return claims;
  } catch {
    return null;
  }
}
