import jwt from 'jsonwebtoken';
import { SETIJWTClaims } from './types.js';

const JWT_SECRET = process.env.JWT_SECRET;
if (!JWT_SECRET) {
  console.error('[signal-clearance] FATAL: JWT_SECRET environment variable not set');
  process.exit(1);
}

const JWT_TTL_SECONDS = 15 * 60; // 15 minutes

export function issueJWT(params: {
  wrangler_id: string;
  clearance_level: string;
  identity_source: string;
}): { token: string; expires_at: string } {
  const now = Math.floor(Date.now() / 1000);
  const exp = now + JWT_TTL_SECONDS;

  const claims: SETIJWTClaims = {
    wrangler_id: params.wrangler_id,
    clearance_level: params.clearance_level,
    identity_source: params.identity_source,
    iat: now,
    exp,
  };

  const token = jwt.sign(claims, JWT_SECRET!, { algorithm: 'HS256' });
  const expires_at = new Date(exp * 1000).toISOString();

  return { token, expires_at };
}

export function verifyJWT(token: string): SETIJWTClaims | null {
  try {
    return jwt.verify(token, JWT_SECRET!, { algorithms: ['HS256'] }) as SETIJWTClaims;
  } catch {
    return null;
  }
}
