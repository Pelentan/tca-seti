// ---------------------------------------------------------------------------
// Wrangler identity and clearance types
// ---------------------------------------------------------------------------

export interface WranglerRecord {
  wrangler_id: string;
  display_name: string;
  ad_identity: string;
  clearance_level: string;
  clearance_source: string;
  wrangler_type: WranglerType;
  status: 'active' | 'revoked';
  identity_source: string;
  first_login_at: string;
  last_login_at: string;
  updated_at: string;
}

export type WranglerType =
  | 'connie_wrangler'
  | 'project_wrangler'
  | 'sec_wrangler'
  | 'ops_wrangler'
  | 'show_wrangler'
  | 'admin';

// ---------------------------------------------------------------------------
// Signal scope and clearance level types
// ---------------------------------------------------------------------------

export interface SignalScope {
  application_ids: string[];
  test_tiers: string[];
  environments: string[];
  feed_types: string[];
}

export interface ClearanceLevel {
  name: string;
  description: string;
  scope: SignalScope;
  created_at: string;
  updated_at: string;
}

// ---------------------------------------------------------------------------
// Federation configuration types
// ---------------------------------------------------------------------------

export interface FederationConfig {
  config_id: string;
  display_name: string;
  oidc_discovery_url: string;
  client_id: string;
  client_secret: string; // Never returned in API responses
  audience: string;
  groups_claim: string;
  name_claim: string;
  identity_claim: string;
  introspection_enabled: boolean;
  jwks_uri?: string;
  authorization_endpoint?: string;
  token_endpoint?: string;
  userinfo_endpoint?: string;
  jwks_last_refreshed_at: string;
  jwks_refresh_interval_minutes: number;
}

export interface GroupMapping {
  group_id: string;
  group_display_name: string;
  clearance_level: string;
  created_at: string;
  updated_at: string;
}

// ---------------------------------------------------------------------------
// Auth result types
// ---------------------------------------------------------------------------

export interface AuthResult {
  jwt: string;
  refresh_token: string;
  jwt_expires_at: string;
  refresh_expires_at: string;
  wrangler_id: string;
  clearance_level: string;
  identity_source: string;
  clearance_source: string;
}

// ---------------------------------------------------------------------------
// SETI JWT claims
// ---------------------------------------------------------------------------

export interface SETIJWTClaims {
  wrangler_id: string;
  clearance_level: string;
  identity_source: string;
  iat: number;
  exp: number;
}

// ---------------------------------------------------------------------------
// Error response
// ---------------------------------------------------------------------------

export interface ErrorResponse {
  code: string;
  message: string;
}
