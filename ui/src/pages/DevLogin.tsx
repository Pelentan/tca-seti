import { useState, useEffect } from 'react';

const GATEWAY = import.meta.env.VITE_GATEWAY_URL || '';

interface ClearanceLevel {
  name: string;
  description: string;
}

export default function DevLogin() {
  const [levels, setLevels] = useState<ClearanceLevel[]>([]);
  const [displayName, setDisplayName] = useState('Dev Wr4ngler');
  const [selectedLevel, setSelectedLevel] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    // Fetch available clearance levels from Signal Clearance via Gateway
    fetch(`${GATEWAY}/auth/dev/login-form`)
      .then(r => r.json())
      .then(data => {
        setLevels(data.levels || []);
        if (data.levels?.length > 0) {
          setSelectedLevel(data.levels[0].name);
        }
      })
      .catch(() => setError('Could not reach Signal Clearance'));
  }, []);

  async function handleLogin() {
    if (!displayName.trim() || !selectedLevel) return;
    setLoading(true);
    setError('');

    try {
      const res = await fetch(`${GATEWAY}/auth/dev/login`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({
          display_name: displayName.trim(),
          clearance_level: selectedLevel,
        }),
      });

      if (!res.ok) {
        const data = await res.json();
        setError(data.message || 'Login failed');
        return;
      }

      const data = await res.json();
      // Store JWT — refresh token is set as httpOnly cookie by the server
      localStorage.setItem('seti_jwt', data.jwt);
      window.location.href = '/';
    } catch (err) {
      setError('Login request failed');
    } finally {
      setLoading(false);
    }
  }

  return (
    <div style={styles.container}>
      <div style={styles.card}>
        <div style={styles.header}>
          <div style={styles.acronym}>S.E.T.I.</div>
          <div style={styles.fullName}>Search for Erroneous Tessellated Interactions</div>
        </div>

        <div style={styles.devBadge}>
          DEV MODE — No AD Required
        </div>

        {error && <div style={styles.error}>{error}</div>}

        <div style={styles.field}>
          <label style={styles.label}>Display Name</label>
          <input
            style={styles.input}
            value={displayName}
            onChange={e => setDisplayName(e.target.value)}
            placeholder="Your name"
            onKeyDown={e => e.key === 'Enter' && handleLogin()}
          />
        </div>

        <div style={styles.field}>
          <label style={styles.label}>Clearance Level</label>
          <select
            style={styles.select}
            value={selectedLevel}
            onChange={e => setSelectedLevel(e.target.value)}
          >
            {levels.map(l => (
              <option key={l.name} value={l.name}>
                {l.name} — {l.description.slice(0, 50)}
                {l.description.length > 50 ? '...' : ''}
              </option>
            ))}
          </select>
        </div>

        <button
          style={styles.loginButton}
          onClick={handleLogin}
          disabled={loading || !selectedLevel}
        >
          {loading ? 'Signing in...' : 'Sign in (Dev)'}
        </button>

        <div style={styles.footer}>
          STUB: real OIDC not enforced · set AUTH_MODE=production for AD login
        </div>
      </div>
    </div>
  );
}

const styles: Record<string, React.CSSProperties> = {
  container: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100vh',
    background: '#0d1117',
  },
  card: {
    width: 420,
    padding: '40px 36px',
    background: '#161b22',
    border: '1px solid #21262d',
    borderRadius: 8,
    display: 'flex',
    flexDirection: 'column',
    gap: 18,
  },
  header: {
    textAlign: 'center',
  },
  acronym: {
    fontSize: 28,
    fontWeight: 700,
    color: '#58a6ff',
    letterSpacing: 6,
    marginBottom: 8,
  },
  fullName: {
    fontSize: 11,
    color: '#8b949e',
    letterSpacing: 1,
    textTransform: 'uppercase',
  },
  devBadge: {
    textAlign: 'center',
    fontSize: 11,
    color: '#e3b341',
    background: '#2d2a1a',
    border: '1px solid #4a3d00',
    borderRadius: 4,
    padding: '6px 12px',
    letterSpacing: 1,
  },
  error: {
    padding: '10px 14px',
    background: '#3d1a1a',
    border: '1px solid #f85149',
    borderRadius: 6,
    color: '#f85149',
    fontSize: 12,
  },
  field: {
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
  },
  label: {
    fontSize: 11,
    color: '#8b949e',
    letterSpacing: 1,
    textTransform: 'uppercase',
  },
  input: {
    background: '#0d1117',
    border: '1px solid #30363d',
    borderRadius: 6,
    color: '#e6edf3',
    fontSize: 13,
    padding: '8px 12px',
    fontFamily: 'inherit',
    outline: 'none',
  },
  select: {
    background: '#0d1117',
    border: '1px solid #30363d',
    borderRadius: 6,
    color: '#e6edf3',
    fontSize: 13,
    padding: '8px 12px',
    fontFamily: 'inherit',
    cursor: 'pointer',
  },
  loginButton: {
    width: '100%',
    padding: '12px 0',
    background: '#388bfd26',
    border: '1px solid #1f6feb',
    borderRadius: 6,
    color: '#58a6ff',
    fontSize: 13,
    fontWeight: 600,
    cursor: 'pointer',
    letterSpacing: 0.5,
    fontFamily: 'inherit',
  },
  footer: {
    textAlign: 'center',
    fontSize: 10,
    color: '#484f58',
    letterSpacing: 0.5,
  },
};
