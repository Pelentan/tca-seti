// ---------------------------------------------------------------------------
// TCA JWKS Client — stdlib only, zero external dependencies.
//
// Contract: contracts/lib/jwks-client.yaml
//
// Implements JWKS endpoint fetch, key caching, and RS256 JWT verification
// using Node.js crypto.subtle (Web Crypto API) and stdlib https.
//
// Copy-per-Job discipline applies. Copy this file into any TCA Job that
// needs to verify OIDC tokens. Do not import from another Job.
//
// Algorithm: RS256 (RSASSA-PKCS1-v1_5 + SHA-256) only.
// Algorithm agility is a security risk, not a feature. If a different
// algorithm is required, write a new lib with a new contract.
// ---------------------------------------------------------------------------

import https from 'https';
import type { RequestOptions } from 'https';

// ---------------------------------------------------------------------------
// JWKSCache interface — pluggable cache backend.
// Default (no adapter): in-memory Map (Tier 1).
// With adapter: shared external cache such as Redis (Tier 2).
// See contract for reference Redis adapter implementation.
// ---------------------------------------------------------------------------

export interface JWKSCache {
  get(kid: string): Promise<CryptoKey | null>;
  set(kid: string, key: CryptoKey, ttlSeconds: number): Promise<void>;
}

// ---------------------------------------------------------------------------
// Internal types
// ---------------------------------------------------------------------------

interface JWKKey {
  kty: string;
  kid?: string;
  use?: string;
  alg?: string;
  n: string;
  e: string;
}

interface JWKSet {
  keys: JWKKey[];
}

export interface JWTPayload {
  [claim: string]: unknown;
  exp?: number;
  aud?: string | string[];
  iss?: string;
  sub?: string;
}

// ---------------------------------------------------------------------------
// JWKSClient
// ---------------------------------------------------------------------------

const RS256_PARAMS: RsaHashedImportParams = {
  name: 'RSASSA-PKCS1-v1_5',
  hash: 'SHA-256',
};

const DEFAULT_MIN_REFRESH_SECONDS = 300;
const DEFAULT_KEY_TTL_SECONDS     = 86400;
const MIN_ALLOWED_REFRESH_SECONDS = 60;

export class JWKSClient {
  private readonly jwksUri:    string;
  private readonly cache:      JWKSCache;
  private readonly minRefresh: number;
  private readonly keyTtl:     number;
  private lastFetch:           number = 0;
  // Tier 1 in-memory fallback when no external cache provided
  private readonly memCache:   Map<string, CryptoKey> = new Map();
  private readonly useMemCache: boolean;
  private readonly agentOptions?: RequestOptions;

  constructor(
    jwksUri: string,
    options?: {
      cache?:                      JWKSCache;
      minRefreshIntervalSeconds?:  number;
      keyTtlSeconds?:              number;
      agentOptions?:               RequestOptions;
    }
  ) {
    if (!jwksUri.startsWith('https://')) {
      throw new Error(`JWKS URI must be HTTPS — got: ${jwksUri}`);
    }

    this.jwksUri       = jwksUri;
    this.useMemCache   = !options?.cache;
    this.cache         = options?.cache ?? this.makeTier1Adapter();
    this.minRefresh    = Math.max(
      options?.minRefreshIntervalSeconds ?? DEFAULT_MIN_REFRESH_SECONDS,
      MIN_ALLOWED_REFRESH_SECONDS
    );
    this.keyTtl        = options?.keyTtlSeconds ?? DEFAULT_KEY_TTL_SECONDS;
    this.agentOptions  = options?.agentOptions;
  }

  // Tier 1 adapter wraps the in-memory Map as a JWKSCache
  private makeTier1Adapter(): JWKSCache {
    return {
      get: async (kid: string) => this.memCache.get(kid) ?? null,
      set: async (kid: string, key: CryptoKey) => { this.memCache.set(kid, key); },
    };
  }

  // -------------------------------------------------------------------------
  // Public API
  // -------------------------------------------------------------------------

  // prefetch warms the cache at startup. Not required — verify() handles
  // cold starts — but eliminates first-request latency and fails fast if
  // the JWKS endpoint is unreachable.
  async prefetch(): Promise<void> {
    await this.fetchAndCache();
  }

  // verify validates an RS256 JWT. Returns the decoded payload on success.
  // Throws on any verification failure. See contract for error taxonomy.
  async verify(
    token: string,
    options?: { audience?: string }
  ): Promise<JWTPayload> {
    const parts = token.split('.');
    if (parts.length !== 3) {
      throw new Error('malformed token');
    }

    // Step 1: Decode header
    const header = this.decodeBase64urlJSON(parts[0]) as {
      alg?: string;
      kid?: string;
    };

    if (header.alg !== 'RS256') {
      throw new Error(`unsupported algorithm: ${header.alg}`);
    }

    const kid = header.kid ?? '';

    // Step 2: Look up key — cache first, then fetch if allowed
    let key = await this.lookupKey(kid);

    if (!key) {
      const now = Date.now() / 1000;
      if (now - this.lastFetch < this.minRefresh) {
        throw new Error(
          `unknown key: ${kid} — retry after rate limit window (${this.minRefresh}s)`
        );
      }
      await this.fetchAndCache();
      key = await this.lookupKey(kid);
      if (!key) {
        throw new Error(`unknown key: ${kid}`);
      }
    }

    // Step 3: Verify RS256 signature
    // CRITICAL: work with raw bytes — do not convert signature to string
    const signingInput  = Buffer.from(`${parts[0]}.${parts[1]}`);
    const signatureBytes = Buffer.from(parts[2], 'base64url');

    const valid = await crypto.subtle.verify(
      'RSASSA-PKCS1-v1_5',
      key,
      signatureBytes,
      signingInput
    );

    if (!valid) {
      throw new Error('invalid signature');
    }

    // Step 4: Decode and validate claims
    const payload = this.decodeBase64urlJSON(parts[1]) as JWTPayload;

    const now = Math.floor(Date.now() / 1000);
    if (payload.exp !== undefined && now > payload.exp) {
      throw new Error('token expired');
    }

    if (options?.audience) {
      const aud = payload.aud;
      const audList = Array.isArray(aud) ? aud : [aud];
      if (!audList.includes(options.audience)) {
        throw new Error(`audience mismatch: expected ${options.audience}`);
      }
    }

    return payload;
  }

  // -------------------------------------------------------------------------
  // Internal helpers
  // -------------------------------------------------------------------------

  private async lookupKey(kid: string): Promise<CryptoKey | null> {
    try {
      return await this.cache.get(kid);
    } catch {
      // Cache infrastructure failure — fall through to JWKS fetch
      return null;
    }
  }

  private async fetchAndCache(): Promise<void> {
    const jwks = await this.fetchJWKS();
    this.lastFetch = Date.now() / 1000;

    for (const jwk of jwks.keys) {
      if (jwk.kty !== 'RSA') continue;
      if (jwk.use && jwk.use !== 'sig') continue;
      if (!jwk.kid) continue;

      try {
        const key = await crypto.subtle.importKey(
          'jwk',
          jwk as JsonWebKey,
          RS256_PARAMS,
          // exportable=true so Tier 2 adapters can re-export for storage
          true,
          ['verify']
        );

        try {
          await this.cache.set(jwk.kid, key, this.keyTtl);
        } catch (err) {
          // Cache write failure is non-fatal — key will be re-fetched next time
          console.error(`[jwks] Cache set failed for kid=${jwk.kid}: ${err}`);
        }
      } catch (err) {
        console.error(`[jwks] Failed to import key kid=${jwk.kid}: ${err}`);
      }
    }
  }

  private fetchJWKS(): Promise<JWKSet> {
    return new Promise((resolve, reject) => {
      const url = new URL(this.jwksUri);
      const reqOptions: RequestOptions = {
        hostname: url.hostname,
        port:     url.port || 443,
        path:     url.pathname + url.search,
        method:   'GET',
        // External IdP — no mTLS agent. Callers needing mTLS pass agentOptions.
        ...this.agentOptions,
      };

      const req = https.request(reqOptions, (res) => {
        let body = '';
        res.on('data', (chunk: Buffer) => { body += chunk.toString(); });
        res.on('end', () => {
          try {
            const parsed = JSON.parse(body) as JWKSet;
            if (!Array.isArray(parsed.keys)) {
              reject(new Error('invalid JWKS response: missing keys array'));
              return;
            }
            resolve(parsed);
          } catch {
            reject(new Error(`invalid JWKS response: ${body.slice(0, 100)}`));
          }
        });
      });

      req.setTimeout(10_000, () => {
        req.destroy();
        reject(new Error('JWKS fetch timed out'));
      });

      req.on('error', (err: Error) => {
        reject(new Error(`JWKS fetch failed: ${err.message}`));
      });

      req.end();
    });
  }

  private decodeBase64urlJSON(encoded: string): unknown {
    try {
      return JSON.parse(Buffer.from(encoded, 'base64url').toString('utf8'));
    } catch {
      throw new Error('malformed token: invalid base64url JSON');
    }
  }
}
