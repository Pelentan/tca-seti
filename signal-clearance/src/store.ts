import {
  WranglerRecord,
  ClearanceLevel,
  FederationConfig,
  GroupMapping,
  SignalScope,
  WranglerType,
} from './types.js';

// ---------------------------------------------------------------------------
// In-memory store — Phase 1
// Production: replace with PostgreSQL via the Signal Clearance database.
// All store operations are isolated behind these functions so the swap
// is a single replacement point per the stub discipline.
// ---------------------------------------------------------------------------

const wranglers = new Map<string, WranglerRecord>();
const wranglersByIdentity = new Map<string, string>(); // ad_identity → wrangler_id
const clearanceLevels = new Map<string, ClearanceLevel>();
const groupMappings = new Map<string, GroupMapping>(); // group_id → mapping
const federationConfigs = new Map<string, FederationConfig>();

// ---------------------------------------------------------------------------
// Wrangler operations
// ---------------------------------------------------------------------------

export function getWrangler(id: string): WranglerRecord | undefined {
  return wranglers.get(id);
}

export function getWranglerByIdentity(adIdentity: string): WranglerRecord | undefined {
  const id = wranglersByIdentity.get(adIdentity);
  if (!id) return undefined;
  return wranglers.get(id);
}

export function upsertWrangler(params: {
  adIdentity: string;
  displayName: string;
  clearanceLevel: string;
  clearanceSource: string;
  wranglerType: WranglerType;
  identitySource: string;
}): WranglerRecord {
  const existing = getWranglerByIdentity(params.adIdentity);
  const now = new Date().toISOString();

  if (existing) {
    const updated: WranglerRecord = {
      ...existing,
      display_name: params.displayName,
      clearance_level: params.clearanceLevel,
      clearance_source: params.clearanceSource,
      wrangler_type: params.wranglerType,
      identity_source: params.identitySource,
      last_login_at: now,
      updated_at: now,
    };
    wranglers.set(existing.wrangler_id, updated);
    return updated;
  }

  const record: WranglerRecord = {
    wrangler_id: `wrnglr-${crypto.randomUUID().split('-')[0]}`,
    display_name: params.displayName,
    ad_identity: params.adIdentity,
    clearance_level: params.clearanceLevel,
    clearance_source: params.clearanceSource,
    wrangler_type: params.wranglerType,
    status: 'active',
    identity_source: params.identitySource,
    first_login_at: now,
    last_login_at: now,
    updated_at: now,
  };

  wranglers.set(record.wrangler_id, record);
  wranglersByIdentity.set(params.adIdentity, record.wrangler_id);
  return record;
}

export function revokeWrangler(id: string): WranglerRecord | undefined {
  const record = wranglers.get(id);
  if (!record) return undefined;
  const revoked = { ...record, status: 'revoked' as const, updated_at: new Date().toISOString() };
  wranglers.set(id, revoked);
  return revoked;
}

export function listWranglers(): WranglerRecord[] {
  return Array.from(wranglers.values());
}

// ---------------------------------------------------------------------------
// Clearance level operations
// ---------------------------------------------------------------------------

export function getClearanceLevel(name: string): ClearanceLevel | undefined {
  return clearanceLevels.get(name);
}

export function listClearanceLevels(): ClearanceLevel[] {
  return Array.from(clearanceLevels.values());
}

export function createClearanceLevel(params: {
  name: string;
  description: string;
  scope: SignalScope;
}): ClearanceLevel {
  const now = new Date().toISOString();
  const level: ClearanceLevel = {
    name: params.name,
    description: params.description,
    scope: params.scope,
    created_at: now,
    updated_at: now,
  };
  clearanceLevels.set(params.name, level);
  return level;
}

export function clearanceGrantsScope(
  clearanceName: string,
  requestedScope: SignalScope
): { granted: boolean; denied_dimensions: string[] } {
  const level = clearanceLevels.get(clearanceName);
  if (!level) {
    return { granted: false, denied_dimensions: ['clearance_level: not found'] };
  }

  const denied: string[] = [];
  const scope = level.scope;

  const check = (
    field: keyof SignalScope,
    requested: string[],
    granted: string[]
  ) => {
    if (granted.includes('*')) return;
    const denied_items = requested.filter(r => !granted.includes(r));
    if (denied_items.length > 0) {
      denied.push(`${field}: ${denied_items.join(', ')} not in clearance`);
    }
  };

  check('application_ids', requestedScope.application_ids, scope.application_ids);
  check('test_tiers', requestedScope.test_tiers, scope.test_tiers);
  check('environments', requestedScope.environments, scope.environments);
  check('feed_types', requestedScope.feed_types, scope.feed_types);

  return { granted: denied.length === 0, denied_dimensions: denied };
}

// ---------------------------------------------------------------------------
// Group mapping operations
// ---------------------------------------------------------------------------

export function addGroupMapping(params: {
  group_id: string;
  group_display_name: string;
  clearance_level: string;
}): GroupMapping {
  const now = new Date().toISOString();
  const mapping: GroupMapping = { ...params, created_at: now, updated_at: now };
  groupMappings.set(params.group_id, mapping);
  return mapping;
}

export function removeGroupMapping(groupId: string): GroupMapping | undefined {
  const mapping = groupMappings.get(groupId);
  if (!mapping) return undefined;
  groupMappings.delete(groupId);
  return mapping;
}

export function listGroupMappings(): GroupMapping[] {
  return Array.from(groupMappings.values());
}

export function resolveGroupsToClearance(groups: string[]): {
  clearance_level: string | null;
  clearance_source: string | null;
} {
  // Highest clearance wins when Wrangler is in multiple mapped groups
  // Order is defined by the order clearance levels were created
  const levelOrder = Array.from(clearanceLevels.keys());

  let bestLevel: string | null = null;
  let bestSource: string | null = null;
  let bestIdx = Infinity;

  for (const groupId of groups) {
    const mapping = groupMappings.get(groupId);
    if (!mapping) continue;
    const idx = levelOrder.indexOf(mapping.clearance_level);
    if (idx !== -1 && idx < bestIdx) {
      bestIdx = idx;
      bestLevel = mapping.clearance_level;
      bestSource = mapping.group_display_name;
    }
  }

  return { clearance_level: bestLevel, clearance_source: bestSource };
}

// ---------------------------------------------------------------------------
// Federation config operations
// ---------------------------------------------------------------------------

export function getFederationConfig(configId: string): FederationConfig | undefined {
  return federationConfigs.get(configId);
}

export function getDefaultFederationConfig(): FederationConfig | undefined {
  const configs = Array.from(federationConfigs.values());
  return configs[0];
}

export function setFederationConfig(config: FederationConfig): void {
  federationConfigs.set(config.config_id, config);
}

export function listFederationConfigs(): FederationConfig[] {
  return Array.from(federationConfigs.values());
}

// ---------------------------------------------------------------------------
// Seed default clearance levels — admin always exists
// ---------------------------------------------------------------------------

export function seedDefaults(): void {
  if (clearanceLevels.size > 0) return;

  const wildcard: SignalScope = {
    application_ids: ['*'],
    test_tiers: ['*'],
    environments: ['*'],
    feed_types: ['*'],
  };

  createClearanceLevel({
    name: 'admin',
    description: 'Full access to all SETI signals and administrative functions.',
    scope: wildcard,
  });

  createClearanceLevel({
    name: 'ops-wr4ngler',
    description: 'Access to all applications, all tiers, all environments. No anomaly or cluster feeds.',
    scope: {
      application_ids: ['*'],
      test_tiers: ['contract', 'plot'],
      environments: ['*'],
      feed_types: ['results', 'failures', 'trends'],
    },
  });

  createClearanceLevel({
    name: 'connie-wr4ngler',
    description: 'Access to contract and plot results for production and staging. No anomaly or cluster signals.',
    scope: {
      application_ids: ['*'],
      test_tiers: ['contract', 'plot'],
      environments: ['production', 'staging'],
      feed_types: ['results', 'failures'],
    },
  });

  createClearanceLevel({
    name: 'sec-wr4ngler',
    description: 'Full signal access including anomaly and cluster-level feeds.',
    scope: wildcard,
  });

  console.log('[signal-clearance] Default clearance levels seeded.');
}
