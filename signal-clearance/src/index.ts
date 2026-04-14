import express, { Request, Response, NextFunction } from 'express';
import cookieParser from 'cookie-parser';
import https from 'https';
import { v4 as uuidv4 } from 'uuid';
import { buildMTLSOptions, obtainCerts } from './certforge.js';
import { selfRegister } from './selfregister.js';
import { reportEvent } from './observability.js';
import { issueJWT } from './jwt.js';
import {
  connectRedis,
  issueRefreshToken,
  consumeRefreshToken,
  revokeAllSessions,
  getActiveSessionCount,
  isRedisConnected,
} from './sessions.js';
import {
  getWrangler,
  upsertWrangler,
  revokeWrangler,
  listWranglers,
  getClearanceLevel,
  listClearanceLevels,
  createClearanceLevel,
  clearanceGrantsScope,
  addGroupMapping,
  removeGroupMapping,
  listGroupMappings,
  resolveGroupsToClearance,
  getFederationConfig,
  getDefaultFederationConfig,
  setFederationConfig,
  listFederationConfigs,
  seedDefaults,
} from './store.js';
import {
  discoverOIDC,
  validateIDToken,
  buildAuthorizationURL,
  exchangeCode,
  refreshJWKS,
} from './oidc.js';
import { SignalScope, WranglerType } from './types.js';
import { devAuthRouter, seedDevGroupMappings } from './devAuth.js';

const app = express();
app.use(express.json());
app.use(cookieParser());

const PORT = parseInt(process.env.PORT || '4001', 10);

// ---------------------------------------------------------------------------
// Pending OIDC states — in-memory, Phase 1
// ---------------------------------------------------------------------------

const pendingStates = new Map<string, { created_at: number }>();

// Clean up expired states every 5 minutes
setInterval(() => {
  const cutoff = Date.now() - 10 * 60 * 1000; // 10-minute TTL
  for (const [state, data] of pendingStates) {
    if (data.created_at < cutoff) pendingStates.delete(state);
  }
}, 5 * 60 * 1000);

// ---------------------------------------------------------------------------
// Helper: wrap route with observability timing
// ---------------------------------------------------------------------------

function timed(callee: string, method: string, path: string, fn: () => Promise<Response | void>) {
  const start = Date.now();
  return fn().finally(() => {
    // Status code not available here — reported by individual handlers
    reportEvent({ callee, method, path, status_code: 0, latency_ms: Date.now() - start });
  });
}

// ---------------------------------------------------------------------------
// Auth — OIDC authorize URL (called by Gateway for redirect)
// ---------------------------------------------------------------------------

app.get('/auth/oidc/authorize', async (req: Request, res: Response) => {
  const start = Date.now();
  try {
    const config = getDefaultFederationConfig();
    if (!config) {
      res.status(503).json({ code: 'IDP_NOT_CONFIGURED', message: 'No IdP configuration found' });
      return;
    }

    const state = uuidv4();
    pendingStates.set(state, { created_at: Date.now() });

    const redirectUrl = buildAuthorizationURL(config, state);
    reportEvent({ callee: 'idp', method: 'GET', path: '/authorize', status_code: 302, latency_ms: Date.now() - start });
    res.json({ redirect_url: redirectUrl, state });
  } catch (err) {
    console.error('[signal-clearance] authorize error:', err);
    res.status(500).json({ code: 'INTERNAL_ERROR', message: String(err) });
  }
});

// ---------------------------------------------------------------------------
// Auth — OIDC callback (Gateway calls this after code exchange)
// ---------------------------------------------------------------------------

app.post('/auth/oidc/callback', async (req: Request, res: Response) => {
  const start = Date.now();
  try {
    const { code, state } = req.body;

    if (!pendingStates.has(state)) {
      res.status(401).json({ code: 'INVALID_STATE', message: 'State parameter invalid or expired' });
      return;
    }
    pendingStates.delete(state);

    const config = getDefaultFederationConfig();
    if (!config) {
      res.status(503).json({ code: 'IDP_NOT_CONFIGURED', message: 'No IdP configuration' });
      return;
    }

    // Exchange authorization code for tokens at the IdP
    const exchangeStart = Date.now();
    let tokens: { id_token: string; access_token: string };
    try {
      tokens = await exchangeCode(code, config);
      reportEvent({ callee: 'idp', method: 'POST', path: '/token', status_code: 200, latency_ms: Date.now() - exchangeStart });
    } catch (err) {
      reportEvent({ callee: 'idp', method: 'POST', path: '/token', status_code: 400, latency_ms: Date.now() - exchangeStart });
      res.status(401).json({ code: 'TOKEN_EXCHANGE_FAILED', message: String(err) });
      return;
    }

    await completeAuth(tokens.id_token, config.config_id, res);
  } catch (err) {
    console.error('[signal-clearance] callback error:', err);
    res.status(500).json({ code: 'INTERNAL_ERROR', message: String(err) });
  }
});

// ---------------------------------------------------------------------------
// Auth — federated login (direct ID token submission)
// ---------------------------------------------------------------------------

app.post('/auth/federated', async (req: Request, res: Response) => {
  const start = Date.now();
  try {
    const { id_token, idp_config_id } = req.body;
    if (!id_token) {
      res.status(400).json({ code: 'MISSING_ID_TOKEN', message: 'id_token is required' });
      return;
    }

    const configId = idp_config_id || getDefaultFederationConfig()?.config_id;
    if (!configId) {
      res.status(503).json({ code: 'IDP_NOT_CONFIGURED', message: 'No IdP configuration' });
      return;
    }

    await completeAuth(id_token, configId, res);
  } catch (err) {
    console.error('[signal-clearance] federated auth error:', err);
    res.status(500).json({ code: 'INTERNAL_ERROR', message: String(err) });
  }
});

// ---------------------------------------------------------------------------
// Shared auth completion logic
// ---------------------------------------------------------------------------

async function completeAuth(idToken: string, configId: string, res: Response): Promise<void> {
  const config = getFederationConfig(configId);
  if (!config) {
    res.status(503).json({ code: 'IDP_NOT_CONFIGURED', message: `Config ${configId} not found` });
    return;
  }

  // Validate ID token
  let validated;
  try {
    validated = await validateIDToken(idToken, config);
  } catch (err) {
    res.status(401).json({ code: 'INVALID_ID_TOKEN', message: String(err) });
    return;
  }

  // Map groups to clearance level
  const { clearance_level, clearance_source } = resolveGroupsToClearance(validated.groups);
  if (!clearance_level || !clearance_source) {
    res.status(403).json({
      code: 'NO_CLEARANCE_MAPPING',
      message: 'Authenticated identity maps to no configured SETI clearance level',
    });
    return;
  }

  // Determine wrangler type from clearance level name
  const wranglerType = clearanceLevelToType(clearance_level);

  // Upsert wrangler record
  const wrangler = upsertWrangler({
    adIdentity: validated.identity,
    displayName: validated.display_name,
    clearanceLevel: clearance_level,
    clearanceSource: clearance_source,
    wranglerType,
    identitySource: configId,
  });

  if (wrangler.status === 'revoked') {
    res.status(403).json({
      code: 'ACCESS_REVOKED',
      message: 'SETI access has been manually revoked for this identity',
    });
    return;
  }

  // Issue SETI JWT and refresh token
  const { token: jwt, expires_at: jwtExpiresAt } = issueJWT({
    wrangler_id: wrangler.wrangler_id,
    clearance_level,
    identity_source: configId,
  });

  const { token: refreshToken, expires_at: refreshExpiresAt } = await issueRefreshToken(wrangler.wrangler_id);

  console.log(`[signal-clearance] Wrangler authenticated: ${wrangler.wrangler_id} (${clearance_level})`);

  // Set httpOnly cookie so silent refresh works without storing token in JS
  res.cookie('seti_refresh', refreshToken, {
    httpOnly: true,
    secure: process.env.AUTH_MODE === 'production',
    sameSite: 'lax',
    maxAge: 7 * 24 * 60 * 60 * 1000,
    path: '/',
  });

  res.json({
    jwt,
    refresh_token: refreshToken,
    jwt_expires_at: jwtExpiresAt,
    refresh_expires_at: refreshExpiresAt,
    wrangler_id: wrangler.wrangler_id,
    clearance_level,
    identity_source: configId,
    clearance_source,
  });
}

function clearanceLevelToType(level: string): WranglerType {
  if (level.includes('connie')) return 'connie_wrangler';
  if (level.includes('ops')) return 'ops_wrangler';
  if (level.includes('sec')) return 'sec_wrangler';
  if (level.includes('project')) return 'project_wrangler';
  if (level.includes('show')) return 'show_wrangler';
  if (level === 'admin') return 'admin';
  return 'connie_wrangler';
}

// ---------------------------------------------------------------------------
// Auth — refresh
// ---------------------------------------------------------------------------

app.post('/auth/refresh', async (req: Request, res: Response) => {
  try {
    // Accept refresh token from body OR from httpOnly cookie (silent refresh from UI)
    const refresh_token = req.body?.refresh_token || req.cookies?.seti_refresh;
    if (!refresh_token) {
      res.status(400).json({ code: 'MISSING_REFRESH_TOKEN', message: 'refresh_token required in body or cookie' });
      return;
    }

    const wranglerId = await consumeRefreshToken(refresh_token);
    if (!wranglerId) {
      res.status(401).json({ code: 'INVALID_REFRESH_TOKEN', message: 'Refresh token invalid or expired' });
      return;
    }

    const wrangler = getWrangler(wranglerId);
    if (!wrangler || wrangler.status === 'revoked') {
      res.status(403).json({ code: 'ACCESS_REVOKED', message: 'Wrangler access has been revoked' });
      return;
    }

    const { token: jwt, expires_at: jwtExpiresAt } = issueJWT({
      wrangler_id: wrangler.wrangler_id,
      clearance_level: wrangler.clearance_level,
      identity_source: wrangler.identity_source,
    });

    const { token: newRefresh, expires_at: refreshExpiresAt } = await issueRefreshToken(wranglerId);

    // Rotate the httpOnly cookie with the new refresh token
    res.cookie('seti_refresh', newRefresh, {
      httpOnly: true,
      secure: process.env.AUTH_MODE === 'production',
      sameSite: 'lax',
      maxAge: 7 * 24 * 60 * 60 * 1000,
      path: '/',
    });

    res.json({
      jwt,
      refresh_token: newRefresh,
      jwt_expires_at: jwtExpiresAt,
      refresh_expires_at: refreshExpiresAt,
      wrangler_id: wrangler.wrangler_id,
      clearance_level: wrangler.clearance_level,
      identity_source: wrangler.identity_source,
      clearance_source: wrangler.clearance_source,
    });
  } catch (err) {
    console.error('[signal-clearance] refresh error:', err);
    res.status(500).json({ code: 'INTERNAL_ERROR', message: String(err) });
  }
});

// ---------------------------------------------------------------------------
// Auth — logout
// ---------------------------------------------------------------------------

app.post('/auth/logout', async (req: Request, res: Response) => {
  try {
    const wranglerId = req.headers['x-wrangler-id'] as string;
    if (!wranglerId) {
      res.status(401).json({ code: 'UNAUTHORIZED', message: 'No active session' });
      return;
    }

    const count = await revokeAllSessions(wranglerId);

    // Clear the httpOnly refresh cookie
    res.clearCookie('seti_refresh', { path: '/' });

    res.json({
      wrangler_id: wranglerId,
      sessions_invalidated: count,
      logged_out_at: new Date().toISOString(),
    });
  } catch (err) {
    console.error('[signal-clearance] logout error:', err);
    res.status(500).json({ code: 'INTERNAL_ERROR', message: String(err) });
  }
});

// ---------------------------------------------------------------------------
// Wranglers
// ---------------------------------------------------------------------------

app.get('/wranglers', (req: Request, res: Response) => {
  const wranglerList = listWranglers();
  res.json({ wranglers: wranglerList, total: wranglerList.length });
});

app.get('/wranglers/:id', (req: Request, res: Response) => {
  const wrangler = getWrangler(req.params.id);
  if (!wrangler) {
    res.status(404).json({ code: 'WRANGLER_NOT_FOUND', message: `Wrangler ${req.params.id} not found` });
    return;
  }
  res.json(wrangler);
});

app.delete('/wranglers/:id', async (req: Request, res: Response) => {
  try {
    const wrangler = revokeWrangler(req.params.id);
    if (!wrangler) {
      res.status(404).json({ code: 'WRANGLER_NOT_FOUND', message: `Wrangler ${req.params.id} not found` });
      return;
    }

    const sessionCount = await revokeAllSessions(req.params.id);
    res.json({
      wrangler_id: req.params.id,
      sessions_invalidated: sessionCount,
      feed_subscriptions_closed: 0, // Feed Wr4ngler handles this in Phase 3
      revoked_at: new Date().toISOString(),
      note: 'SETI access revoked. Remove from mapped AD groups to prevent re-access.',
    });
  } catch (err) {
    console.error('[signal-clearance] revoke error:', err);
    res.status(500).json({ code: 'INTERNAL_ERROR', message: String(err) });
  }
});

// ---------------------------------------------------------------------------
// Clearance levels
// ---------------------------------------------------------------------------

app.get('/clearance/levels', (_req: Request, res: Response) => {
  const levels = listClearanceLevels();
  res.json({ levels, total: levels.length });
});

app.post('/clearance/levels', (req: Request, res: Response) => {
  const { name, description, scope } = req.body;
  if (!name || !description || !scope) {
    res.status(400).json({ code: 'INVALID_REQUEST', message: 'name, description, and scope are required' });
    return;
  }
  if (getClearanceLevel(name)) {
    res.status(409).json({ code: 'CLEARANCE_LEVEL_EXISTS', message: `Clearance level '${name}' already defined` });
    return;
  }
  const level = createClearanceLevel({ name, description, scope });
  res.status(201).json(level);
});

// ---------------------------------------------------------------------------
// Clearance validation — hot path for Feed Wr4ngler
// ---------------------------------------------------------------------------

app.post('/clearance/validate', (req: Request, res: Response) => {
  const { wrangler_id, requested_scope } = req.body;

  const wrangler = getWrangler(wrangler_id);
  if (!wrangler || wrangler.status === 'revoked') {
    res.status(404).json({ code: 'WRANGLER_NOT_FOUND', message: 'Wrangler not found or revoked' });
    return;
  }

  const { granted, denied_dimensions } = clearanceGrantsScope(
    wrangler.clearance_level,
    requested_scope as SignalScope
  );

  if (!granted) {
    res.status(403).json({
      wrangler_id,
      wrangler_clearance_level: wrangler.clearance_level,
      requested_scope,
      denied_dimensions,
    });
    return;
  }

  const expiresAt = new Date(Date.now() + 5 * 60 * 1000).toISOString();
  const clearanceToken = `ct-${uuidv4()}`;

  res.json({
    wrangler_id,
    clearance_token: clearanceToken,
    granted_scope: requested_scope,
    token_expires_at: expiresAt,
  });
});

// ---------------------------------------------------------------------------
// Federation config
// ---------------------------------------------------------------------------

app.get('/federation/config', (_req: Request, res: Response) => {
  const configs = listFederationConfigs();
  if (configs.length === 0) {
    res.status(404).json({ code: 'NOT_CONFIGURED', message: 'No IdP configuration found' });
    return;
  }
  // Redact client_secret
  const safe = configs.map(c => ({ ...c, client_secret: '[REDACTED]' }));
  res.json(safe[0]);
});

app.put('/federation/config', async (req: Request, res: Response) => {
  try {
    const existing = getDefaultFederationConfig();
    const config = { ...existing, ...req.body };

    // Run OIDC discovery if URL changed or endpoints not yet populated
    if (!config.jwks_uri || req.body.oidc_discovery_url) {
      try {
        const discovery = await discoverOIDC(config.oidc_discovery_url);
        config.jwks_uri = discovery.jwks_uri;
        config.authorization_endpoint = discovery.authorization_endpoint;
        config.token_endpoint = discovery.token_endpoint;
        config.userinfo_endpoint = discovery.userinfo_endpoint;
        config.jwks_last_refreshed_at = new Date().toISOString();
        console.log('[signal-clearance] OIDC discovery completed');
      } catch (err) {
        console.error('[signal-clearance] OIDC discovery failed:', err);
        // Continue with existing endpoints if discovery fails
      }
    }

    setFederationConfig(config);
    res.json({ ...config, client_secret: '[REDACTED]' });
  } catch (err) {
    console.error('[signal-clearance] federation config update error:', err);
    res.status(500).json({ code: 'INTERNAL_ERROR', message: String(err) });
  }
});

// ---------------------------------------------------------------------------
// Group mappings
// ---------------------------------------------------------------------------

app.get('/federation/groups', (_req: Request, res: Response) => {
  const mappings = listGroupMappings();
  res.json({ mappings, total: mappings.length });
});

app.post('/federation/groups', (req: Request, res: Response) => {
  const { group_id, group_display_name, clearance_level } = req.body;
  if (!group_id || !group_display_name || !clearance_level) {
    res.status(400).json({ code: 'INVALID_REQUEST', message: 'group_id, group_display_name, and clearance_level are required' });
    return;
  }
  if (!getClearanceLevel(clearance_level)) {
    res.status(400).json({ code: 'INVALID_CLEARANCE_LEVEL', message: `Clearance level '${clearance_level}' not defined` });
    return;
  }
  const mapping = addGroupMapping({ group_id, group_display_name, clearance_level });
  res.status(201).json(mapping);
});

app.delete('/federation/groups/:groupId', (req: Request, res: Response) => {
  const mapping = removeGroupMapping(req.params.groupId);
  if (!mapping) {
    res.status(404).json({ code: 'MAPPING_NOT_FOUND', message: `Group mapping ${req.params.groupId} not found` });
    return;
  }
  res.json({
    group_id: req.params.groupId,
    clearance_level_was: mapping.clearance_level,
    affected_wranglers: 0, // Phase 1: not tracking this count
    removed_at: new Date().toISOString(),
  });
});

// ---------------------------------------------------------------------------
// JWKS refresh
// ---------------------------------------------------------------------------

app.post('/federation/jwks/refresh', async (req: Request, res: Response) => {
  try {
    const config = getDefaultFederationConfig();
    if (!config?.jwks_uri) {
      res.status(503).json({ code: 'NOT_CONFIGURED', message: 'JWKS URI not configured' });
      return;
    }
    refreshJWKS(config.jwks_uri);
    config.jwks_last_refreshed_at = new Date().toISOString();
    setFederationConfig(config);
    res.json({
      config_id: config.config_id,
      keys_loaded: 0, // Actual count not available without fetching
      refreshed_at: config.jwks_last_refreshed_at,
    });
  } catch (err) {
    res.status(500).json({ code: 'INTERNAL_ERROR', message: String(err) });
  }
});

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

app.get('/health', async (_req: Request, res: Response) => {
  const redisConnected = isRedisConnected();
  const activeSessionCount = redisConnected ? await getActiveSessionCount().catch(() => -1) : -1;
  const configs = listFederationConfigs();
  const levels = listClearanceLevels();
  const mappings = listGroupMappings();
  const wranglerList = listWranglers();

  res.json({
    status: redisConnected ? 'healthy' : 'degraded',
    idp_configs: configs.map(c => ({
      config_id: c.config_id,
      display_name: c.display_name,
      jwks_reachable: !!c.jwks_uri,
      last_successful_auth: null,
    })),
    wranglers_active: wranglerList.filter(w => w.status === 'active').length,
    wranglers_revoked: wranglerList.filter(w => w.status === 'revoked').length,
    active_sessions: activeSessionCount,
    clearance_levels_defined: levels.length,
    group_mappings: mappings.length,
    uptime_seconds: Math.floor(process.uptime()),
  });
});

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

async function main() {
  await connectRedis();
  seedDefaults();



  const authMode = process.env.AUTH_MODE || 'dev';

  if (authMode === 'dev') {
    console.log('[signal-clearance] AUTH_MODE=dev — stub auth active, real OIDC disabled');
    seedDevGroupMappings();
    app.use(devAuthRouter);
  } else {
    console.log('[signal-clearance] AUTH_MODE=production — OIDC federation active');
  }

  await obtainCerts();
  const tlsOptions = buildMTLSOptions();
  const server = https.createServer(tlsOptions, app);

  server.listen(PORT, () => {
    console.log(`[signal-clearance] Listening on :${PORT} (mTLS, TLS 1.3)`);
    console.log(`[signal-clearance] Default clearance levels: ${listClearanceLevels().length}`);
    console.log(`[signal-clearance] Auth mode: ${authMode}`);
    setTimeout(() => selfRegister(), 3000);
  });
}

main().catch(err => {
  console.error('[signal-clearance] Fatal error:', err);
  process.exit(1);
});
