import { JWKSClient } from './jwks.js';
import https from 'https';
import { FederationConfig } from './types.js';
import { buildUpstreamAgent } from './mtls.js';

// ---------------------------------------------------------------------------
// OIDC Discovery
// ---------------------------------------------------------------------------

export async function discoverOIDC(discoveryUrl: string): Promise<{
  jwks_uri: string;
  authorization_endpoint: string;
  token_endpoint: string;
  userinfo_endpoint: string;
}> {
  const agent = buildUpstreamAgent();
  return new Promise((resolve, reject) => {
    const req = https.get(discoveryUrl, { agent }, (res) => {
      let body = '';
      res.on('data', chunk => body += chunk);
      res.on('end', () => {
        try {
          resolve(JSON.parse(body));
        } catch (err) {
          reject(new Error(`Failed to parse OIDC discovery document: ${err}`));
        }
      });
    });
    req.on('error', reject);
  });
}

// ---------------------------------------------------------------------------
// JWKS caching — TCA JWKSClient (stdlib, no jose)
// ---------------------------------------------------------------------------

const jwksClients = new Map<string, JWKSClient>();

export function getJWKSClient(jwksUri: string): JWKSClient {
  if (!jwksClients.has(jwksUri)) {
    jwksClients.set(jwksUri, new JWKSClient(jwksUri));
  }
  return jwksClients.get(jwksUri)!;
}

export function refreshJWKS(jwksUri: string): void {
  // Drop the client — next getJWKSClient() call creates a fresh one
  // with an empty cache, forcing a JWKS re-fetch on next verify().
  jwksClients.delete(jwksUri);
  console.log(`[signal-clearance] JWKS client cleared for ${jwksUri}`);
}

// ---------------------------------------------------------------------------
// ID token validation
// ---------------------------------------------------------------------------

export interface ValidatedIDToken {
  identity: string;       // Stable identity claim (e.g. oid)
  display_name: string;   // Display name claim
  groups: string[];       // Group membership claim
  raw: Record<string, unknown>;
}

export async function validateIDToken(
  idToken: string,
  config: FederationConfig
): Promise<ValidatedIDToken> {
  if (!config.jwks_uri) {
    throw new Error('JWKS URI not configured — run OIDC discovery first');
  }

  const client = getJWKSClient(config.jwks_uri);
  const payload = await client.verify(idToken, { audience: config.audience });

  const raw = payload as Record<string, unknown>;

  const identity = raw[config.identity_claim] as string;
  if (!identity) {
    throw new Error(`Identity claim '${config.identity_claim}' not found in ID token`);
  }

  const displayName = (raw[config.name_claim] as string) || identity;
  const groups = (raw[config.groups_claim] as string[]) || [];

  return { identity, display_name: displayName, groups, raw };
}

// ---------------------------------------------------------------------------
// Authorization URL construction (for Gateway redirect)
// ---------------------------------------------------------------------------

export function buildAuthorizationURL(config: FederationConfig, state: string): string {
  if (!config.authorization_endpoint) {
    throw new Error('Authorization endpoint not configured — run OIDC discovery first');
  }

  const params = new URLSearchParams({
    response_type: 'code',
    client_id: config.client_id,
    redirect_uri: process.env.OIDC_REDIRECT_URI || 'https://seti.cluster.internal/auth/callback',
    scope: 'openid profile email groups',
    state,
    // PKCE is handled by the Gateway for browser flows
  });

  return `${config.authorization_endpoint}?${params.toString()}`;
}

// ---------------------------------------------------------------------------
// Authorization code exchange
// ---------------------------------------------------------------------------

export async function exchangeCode(
  code: string,
  config: FederationConfig
): Promise<{ id_token: string; access_token: string }> {
  if (!config.token_endpoint) {
    throw new Error('Token endpoint not configured — run OIDC discovery first');
  }

  const body = new URLSearchParams({
    grant_type: 'authorization_code',
    code,
    client_id: config.client_id,
    client_secret: config.client_secret,
    redirect_uri: process.env.OIDC_REDIRECT_URI || 'https://seti.cluster.internal/auth/callback',
  });

  return new Promise((resolve, reject) => {
    const url = new URL(config.token_endpoint!);
    const postBody = body.toString();

    const req = https.request(
      {
        hostname: url.hostname,
        port: url.port || 443,
        path: url.pathname,
        method: 'POST',
        headers: {
          'Content-Type': 'application/x-www-form-urlencoded',
          'Content-Length': Buffer.byteLength(postBody),
        },
        // Note: IdP is external — no mTLS agent for this call
      },
      (res) => {
        let data = '';
        res.on('data', chunk => data += chunk);
        res.on('end', () => {
          try {
            const parsed = JSON.parse(data);
            if (parsed.error) {
              reject(new Error(`Token exchange failed: ${parsed.error_description || parsed.error}`));
              return;
            }
            resolve({ id_token: parsed.id_token, access_token: parsed.access_token });
          } catch (err) {
            reject(new Error(`Failed to parse token response: ${err}`));
          }
        });
      }
    );
    req.on('error', reject);
    req.write(postBody);
    req.end();
  });
}
