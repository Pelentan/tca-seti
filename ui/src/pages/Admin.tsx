import { CSSProperties } from "react";
import { Link } from "react-router-dom";
import { useState, useEffect } from 'react';
import { useAuth } from '../hooks/useAuth';

const GATEWAY = import.meta.env.VITE_GATEWAY_URL || '';

interface AIProvider {
  provider_id: string;
  name: string;
  ollama_url: string;
  description?: string;
  status: 'reachable' | 'unreachable' | 'unknown';
  available_models: string[];
  active_model?: string;
  last_refreshed_at?: string;
  registered_at: string;
}

export default function Admin() {
  const { jwt, getFreshJWT, wranglerId, logout } = useAuth();
  const [providers, setProviders] = useState<AIProvider[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // New provider form
  const [newName, setNewName] = useState('');
  const [newURL, setNewURL] = useState('');
  const [newDesc, setNewDesc] = useState('');
  const [adding, setAdding] = useState(false);

  async function authFetch(path: string, options: RequestInit = {}) {
    const freshJwt = await getFreshJWT();
    if (!freshJwt) throw new Error('Session expired — please sign in again');
    return fetch(`${GATEWAY}${path}`, {
      ...options,
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${freshJwt}`,
        ...(options.headers || {}),
      },
    });
  }

  async function loadProviders() {
    try {
      const res = await authFetch('/ai-providers');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      setProviders(data.providers || []);
      setError(null);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load providers');
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => { if (jwt) loadProviders(); }, [jwt]);

  async function addProvider() {
    if (!newName || !newURL) return;
    setAdding(true);
    try {
      const res = await authFetch('/ai-providers', {
        method: 'POST',
        body: JSON.stringify({ name: newName, ollama_url: newURL, description: newDesc }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setNewName(''); setNewURL(''); setNewDesc('');
      await loadProviders();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to add provider');
    } finally {
      setAdding(false);
    }
  }

  async function refreshModels(providerId: string) {
    try {
      const res = await authFetch(`/ai-providers/${providerId}/refresh`, { method: 'POST' });
      if (!res.ok) {
        const data = await res.json();
        setError(data.message || `HTTP ${res.status}`);
        return;
      }
      await loadProviders();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to refresh');
    }
  }

  async function setActiveModel(providerId: string, modelName: string) {
    try {
      const res = await authFetch(`/ai-providers/${providerId}/active-model`, {
        method: 'PUT',
        body: JSON.stringify({ model_name: modelName }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      await loadProviders();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to set model');
    }
  }

  async function removeProvider(providerId: string) {
    try {
      const res = await authFetch(`/ai-providers/${providerId}`, { method: 'DELETE' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      await loadProviders();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to remove provider');
    }
  }

  return (
    <div style={s.root}>
      {/* Header */}
      <div style={s.header}>
        <div style={s.headerLeft}>
          <span style={s.logo}>S.E.T.I.</span>
          <span style={s.adminBadge}>ADMIN</span>
        </div>
        <div style={s.headerRight}>
          <Link to="/" style={s.navBtn('#8b949e', '#30363d')}>← Dashboard</Link>
          <Link to="/plots" style={s.navBtn('#58a6ff', '#1f6feb')}>Plots</Link>
          <span style={s.wranglerId}>{wranglerId}</span>
          <button style={s.logoutBtn} onClick={logout}>Sign out</button>
        </div>
      </div>

      <div style={s.body}>
        <div style={s.section}>
          <div style={s.sectionTitle}>AI PROVIDER CONFIGURATION</div>
          <div style={s.sectionSubtitle}>
            Configure Ollama endpoints and select the active model for AI-lien diagnostics.
            AI-lien fetches this configuration on every invocation — changes take effect immediately.
          </div>
        </div>

        {error && (
          <div style={s.errorBar}>
            <span>{error}</span>
            <button style={s.errorClose} onClick={() => setError(null)}>✕</button>
          </div>
        )}

        {/* Add provider form */}
        <div style={s.card}>
          <div style={s.cardTitle}>ADD PROVIDER</div>
          <div style={s.formRow}>
            <div style={s.formField}>
              <label style={s.label}>Name</label>
              <input
                style={s.input}
                id="aiName"
                name="aiName"
                autoComplete="off"
                data-lpignore="true"
                data-form-type="other"
                placeholder="e.g. Local RTX 5090"
                value={newName}
                onChange={e => setNewName(e.target.value)}
              />
            </div>
            <div style={{ ...s.formField, flex: 2 }}>
              <label style={s.label}>Ollama URL</label>
              <input
                style={s.input}
                id="aiOllamaUrl"
                name="aiOllamaUrl"
                autoComplete="off"
                data-lpignore="true"
                data-form-type="other"
                placeholder="e.g. http://host.docker.internal:11434"
                value={newURL}
                onChange={e => setNewURL(e.target.value)}
              />
            </div>
            <div style={s.formField}>
              <label style={s.label}>Description (optional)</label>
              <input
                style={s.input}
                id="aiDescription"
                name="aiDescription"
                autoComplete="off"
                data-lpignore="true"
                data-form-type="other"
                placeholder="Notes about this provider"
                value={newDesc}
                onChange={e => setNewDesc(e.target.value)}
              />
            </div>
            <div style={s.formFieldBtn}>
              <label style={s.label}>&nbsp;</label>
              <button
                style={{
                  ...s.btn,
                  opacity: (!newName || !newURL || adding) ? 0.5 : 1,
                }}
                onClick={addProvider}
                disabled={!newName || !newURL || adding}
              >
                {adding ? 'Adding...' : 'Add Provider'}
              </button>
            </div>
          </div>
        </div>

        {/* Provider list */}
        {loading ? (
          <div style={s.empty}>Loading providers...</div>
        ) : providers.length === 0 ? (
          <div style={s.empty}>No AI providers configured. Add one above to enable AI-lien diagnostics.</div>
        ) : (
          providers.map(p => (
            <div key={p.provider_id} style={s.card}>
              <div style={s.providerHeader}>
                <div style={s.providerName}>
                  <span style={s.name}>{p.name}</span>
                  <span style={{
                    ...s.statusBadge,
                    borderColor: p.status === 'reachable' ? '#3fb950' : p.status === 'unreachable' ? '#f85149' : '#484f58',
                    color: p.status === 'reachable' ? '#3fb950' : p.status === 'unreachable' ? '#f85149' : '#484f58',
                  }}>
                    {p.status}
                  </span>
                  {p.active_model && (
                    <span style={s.activeBadge}>active: {p.active_model}</span>
                  )}
                </div>
                <div style={s.providerActions}>
                  <button style={s.btnSmall} onClick={() => refreshModels(p.provider_id)}>
                    Refresh Models
                  </button>
                  <button style={{ ...s.btnSmall, ...s.btnDanger }} onClick={() => removeProvider(p.provider_id)}>
                    Remove
                  </button>
                </div>
              </div>

              <div style={s.providerURL}>{p.ollama_url}</div>
              {p.description && <div style={s.providerDesc}>{p.description}</div>}

              {p.last_refreshed_at && (
                <div style={s.lastRefreshed}>
                  Last refreshed: {new Date(p.last_refreshed_at).toLocaleString()}
                </div>
              )}

              {/* Model selector */}
              {p.available_models && p.available_models.length > 0 ? (
                <div style={s.modelSection}>
                  <div style={s.modelLabel}>AVAILABLE MODELS</div>
                  <div style={s.modelGrid}>
                    {p.available_models.map(model => (
                      <button
                        key={model}
                        style={{
                          ...s.modelBtn,
                          borderColor: p.active_model === model ? '#3fb950' : '#30363d',
                          color: p.active_model === model ? '#3fb950' : '#8b949e',
                          background: p.active_model === model ? 'rgba(63,185,80,0.08)' : 'none',
                        }}
                        onClick={() => setActiveModel(p.provider_id, model)}
                      >
                        {p.active_model === model && '✓ '}{model}
                      </button>
                    ))}
                  </div>
                </div>
              ) : (
                <div style={s.noModels}>
                  No models loaded. Click "Refresh Models" to query the Ollama endpoint.
                </div>
              )}
            </div>
          ))
        )}
      </div>
    </div>
  );
}

const s = {
  root: {
    display: 'flex', flexDirection: 'column' as const,
    minHeight: '100vh', background: '#0d1117', color: '#e6edf3',
    fontFamily: "'Segoe UI', system-ui, sans-serif",
  },
  header: {
    display: 'flex', alignItems: 'center', justifyContent: 'space-between',
    padding: '10px 20px', background: '#161b22', borderBottom: '1px solid #21262d',
  },
  headerLeft: { display: 'flex', alignItems: 'center', gap: 16 },
  headerRight: { display: 'flex', alignItems: 'center', gap: 16 },
  navBtn: (color: string, border: string): CSSProperties => ({
    fontSize: 11, color, textDecoration: 'none',
    border: `1px solid ${border}`, borderRadius: 4, padding: '3px 10px',
  }),
  logo: { fontSize: 16, fontWeight: 700, color: '#58a6ff', letterSpacing: 3 } as CSSProperties,
  adminBadge: {
    fontSize: 10, color: '#e3b341', border: '1px solid #e3b341',
    borderRadius: 4, padding: '2px 6px', letterSpacing: 1,
  } as CSSProperties,

  wranglerId: { fontSize: 11, color: '#484f58' } as CSSProperties,
  logoutBtn: {
    background: 'none', border: '1px solid #30363d', borderRadius: 4,
    color: '#8b949e', fontSize: 11, cursor: 'pointer', padding: '4px 10px',
    fontFamily: 'inherit',
  } as CSSProperties,
  body: { padding: '24px 32px', maxWidth: 900, margin: '0 auto', width: '100%' },
  section: { marginBottom: 24 },
  sectionTitle: {
    fontSize: 11, color: '#484f58', letterSpacing: 2, marginBottom: 6,
  },
  sectionSubtitle: { fontSize: 12, color: '#6e7681', lineHeight: 1.6 },
  errorBar: {
    display: 'flex', alignItems: 'center', justifyContent: 'space-between',
    background: 'rgba(248,81,73,0.1)', border: '1px solid #f85149',
    borderRadius: 6, padding: '10px 16px', marginBottom: 16,
    fontSize: 12, color: '#f85149',
  },
  errorClose: {
    background: 'none', border: 'none', color: '#f85149',
    cursor: 'pointer', fontFamily: 'inherit', fontSize: 14,
  } as CSSProperties,
  card: {
    background: '#161b22', border: '1px solid #21262d',
    borderRadius: 8, padding: '16px 20px', marginBottom: 12,
  },
  cardTitle: { fontSize: 10, color: '#484f58', letterSpacing: 2, marginBottom: 14 },
  formRow: { display: 'flex', gap: 12, alignItems: 'flex-end', flexWrap: 'wrap' as const },
  formField: { display: 'flex', flexDirection: 'column' as const, gap: 6, flex: 1, minWidth: 140 },
  formFieldBtn: { display: 'flex', flexDirection: 'column' as const, gap: 6 },
  label: { fontSize: 10, color: '#6e7681', letterSpacing: 1 } as CSSProperties,
  input: {
    background: '#0d1117', border: '1px solid #30363d', borderRadius: 4,
    color: '#e6edf3', fontSize: 12, padding: '7px 10px', fontFamily: 'inherit',
    outline: 'none',
  } as CSSProperties,
  btn: {
    background: '#388bfd26', border: '1px solid #1f6feb', borderRadius: 4,
    color: '#58a6ff', fontSize: 12, cursor: 'pointer', padding: '7px 16px',
    fontFamily: 'inherit', whiteSpace: 'nowrap' as const,
  },
  btnSmall: {
    background: 'none', border: '1px solid #30363d', borderRadius: 4,
    color: '#8b949e', fontSize: 11, cursor: 'pointer', padding: '4px 10px',
    fontFamily: 'inherit',
  } as CSSProperties,
  btnDanger: { borderColor: '#da3633', color: '#f85149' } as CSSProperties,
  providerHeader: {
    display: 'flex', alignItems: 'center', justifyContent: 'space-between',
    marginBottom: 8,
  },
  providerName: { display: 'flex', alignItems: 'center', gap: 10 },
  name: { fontSize: 14, fontWeight: 600, color: '#e6edf3' } as CSSProperties,
  statusBadge: {
    fontSize: 10, padding: '1px 6px', border: '1px solid', borderRadius: 3, letterSpacing: 0.5,
  } as CSSProperties,
  activeBadge: {
    fontSize: 10, color: '#3fb950', background: 'rgba(63,185,80,0.1)',
    border: '1px solid #3fb950', borderRadius: 3, padding: '1px 6px',
    fontFamily: "'Courier New', monospace",
  } as CSSProperties,
  providerActions: { display: 'flex', gap: 8 },
  providerURL: {
    fontSize: 12, color: '#58a6ff', fontFamily: "'Courier New', monospace",
    marginBottom: 4,
  },
  providerDesc: { fontSize: 12, color: '#6e7681', marginBottom: 4 },
  lastRefreshed: { fontSize: 11, color: '#484f58', marginBottom: 12 },
  modelSection: { marginTop: 12 },
  modelLabel: { fontSize: 10, color: '#484f58', letterSpacing: 1, marginBottom: 8 },
  modelGrid: { display: 'flex', flexWrap: 'wrap' as const, gap: 8 },
  modelBtn: {
    background: 'none', border: '1px solid', borderRadius: 4,
    fontSize: 12, cursor: 'pointer', padding: '5px 12px',
    fontFamily: "'Courier New', monospace", transition: 'all 0.1s',
  } as CSSProperties,
  noModels: { fontSize: 12, color: '#484f58', marginTop: 8, fontStyle: 'italic' as const },
  empty: { fontSize: 12, color: '#484f58', textAlign: 'center' as const, padding: '40px 20px' },
};
