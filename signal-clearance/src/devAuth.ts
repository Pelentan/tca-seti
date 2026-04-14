// ---------------------------------------------------------------------------
// Dev-mode authentication stub
//
// AUTH_MODE=dev only. Never active in production.
// Provides a direct JWT issuance endpoint for local development without
// requiring a real OIDC IdP or Active Directory.
//
// STUB: logs "STUB: dev auth — real OIDC not enforced"
// Swap point: AUTH_MODE environment variable
// ---------------------------------------------------------------------------

import { Router, Request, Response } from 'express';
import { issueJWT } from './jwt.js';
import { issueRefreshToken } from './sessions.js';
import {
  upsertWrangler,
  getClearanceLevel,
  listClearanceLevels,
  addGroupMapping,
  resolveGroupsToClearance,
} from './store.js';
import { WranglerType } from './types.js';

export const devAuthRouter = Router();

// ---------------------------------------------------------------------------
// Seed dev group mappings so dev logins resolve to clearance levels
// ---------------------------------------------------------------------------

export function seedDevGroupMappings(): void {
  console.log('[signal-clearance] STUB: dev auth mode — seeding dev group mappings');

  const mappings = [
    { group_id: 'dev-admin', group_display_name: 'Dev-Admin', clearance_level: 'admin' },
    { group_id: 'dev-sec', group_display_name: 'Dev-Sec-Wranglers', clearance_level: 'sec-wr4ngler' },
    { group_id: 'dev-ops', group_display_name: 'Dev-Ops-Wranglers', clearance_level: 'ops-wr4ngler' },
    { group_id: 'dev-connie', group_display_name: 'Dev-Connie-Wranglers', clearance_level: 'connie-wr4ngler' },
  ];

  for (const m of mappings) {
    if (getClearanceLevel(m.clearance_level)) {
      addGroupMapping(m);
    }
  }

  console.log('[signal-clearance] STUB: dev group mappings seeded — ' +
    'swap AUTH_MODE=production and configure real IdP for AD login');
}

// ---------------------------------------------------------------------------
// GET /auth/dev/login-form — returns available dev clearance levels
// Used by the UI dev login page to populate the dropdown
// ---------------------------------------------------------------------------

devAuthRouter.get('/auth/dev/login-form', (_req: Request, res: Response) => {
  console.log('[signal-clearance] STUB: dev login form requested — real OIDC not enforced');
  const levels = listClearanceLevels().map(l => ({
    name: l.name,
    description: l.description,
  }));
  res.json({ levels, mode: 'dev' });
});

// ---------------------------------------------------------------------------
// POST /auth/dev/login — issues a real SETI JWT without IdP
// Body: { display_name, clearance_level }
// ---------------------------------------------------------------------------

devAuthRouter.post('/auth/dev/login', async (req: Request, res: Response) => {
  console.log('[signal-clearance] STUB: dev login — real OIDC not enforced');

  const { display_name, clearance_level } = req.body;

  if (!display_name || !clearance_level) {
    res.status(400).json({
      code: 'INVALID_REQUEST',
      message: 'display_name and clearance_level are required',
    });
    return;
  }

  const level = getClearanceLevel(clearance_level);
  if (!level) {
    res.status(400).json({
      code: 'INVALID_CLEARANCE_LEVEL',
      message: `Clearance level '${clearance_level}' not defined`,
    });
    return;
  }

  const adIdentity = `dev-${display_name.toLowerCase().replace(/\s+/g, '.')
    }@dev.local`;

  // Resolve through the same group mapping path as real auth
  // Dev groups are seeded in seedDevGroupMappings()
  const groupId = `dev-${clearance_level.split('-')[0]}`;
  const { clearance_level: resolvedLevel, clearance_source } =
    resolveGroupsToClearance([groupId]);

  const effectiveLevel = resolvedLevel || clearance_level;
  const effectiveSource = clearance_source || 'dev-direct';

  const wranglerType = levelToType(effectiveLevel);

  const wrangler = upsertWrangler({
    adIdentity,
    displayName: display_name,
    clearanceLevel: effectiveLevel,
    clearanceSource: effectiveSource,
    wranglerType,
    identitySource: 'dev-stub',
  });

  const { token: jwt, expires_at: jwtExpiresAt } = issueJWT({
    wrangler_id: wrangler.wrangler_id,
    clearance_level: effectiveLevel,
    identity_source: 'dev-stub',
  });

  const { token: refreshToken, expires_at: refreshExpiresAt } =
    await issueRefreshToken(wrangler.wrangler_id);

  console.log(
    `[signal-clearance] STUB: dev JWT issued for ${wrangler.wrangler_id} ` +
    `(${effectiveLevel}) — not a real AD identity`
  );

  // Set refresh token as httpOnly cookie — same pattern as production OIDC
  res.cookie('seti_refresh', refreshToken, {
    httpOnly: true,
    secure: false,   // dev: HTTP acceptable; production: true
    sameSite: 'lax',
    maxAge: 7 * 24 * 60 * 60 * 1000, // 7 days in ms
    path: '/',
  });

  res.json({
    jwt,
    refresh_token: refreshToken, // also in body for dev tooling convenience
    jwt_expires_at: jwtExpiresAt,
    refresh_expires_at: refreshExpiresAt,
    wrangler_id: wrangler.wrangler_id,
    clearance_level: effectiveLevel,
    identity_source: 'dev-stub',
    clearance_source: effectiveSource,
  });
});

function levelToType(level: string): WranglerType {
  if (level.includes('connie')) return 'connie_wrangler';
  if (level.includes('ops')) return 'ops_wrangler';
  if (level.includes('sec')) return 'sec_wrangler';
  if (level.includes('project')) return 'project_wrangler';
  if (level.includes('show')) return 'show_wrangler';
  if (level === 'admin') return 'admin';
  return 'connie_wrangler';
}
