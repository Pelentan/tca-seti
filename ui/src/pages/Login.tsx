const GATEWAY = import.meta.env.VITE_GATEWAY_URL || '';

export default function Login() {
  const error = new URLSearchParams(window.location.search).get('error');

  function handleLogin() {
    // Redirect to Gateway which redirects to customer IdP
    window.location.href = `${GATEWAY}/login`;
  }

  return (
    <div style={styles.container}>
      <div style={styles.card}>
        <div style={styles.header}>
          <div style={styles.acronym}>S.E.T.I.</div>
          <div style={styles.fullName}>Search for Erroneous Tessellated Interactions</div>
        </div>

        {error && (
          <div style={styles.error}>
            Authentication failed: {error.replace(/_/g, ' ')}
          </div>
        )}

        <div style={styles.description}>
          Sign in with your organization credentials to access the SETI monitoring constellation.
        </div>

        <button style={styles.loginButton} onClick={handleLogin}>
          Sign in with Active Directory
        </button>

        <div style={styles.footer}>
          TCA · Zero Trust · Federated Identity
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
    width: 380,
    padding: '40px 36px',
    background: '#161b22',
    border: '1px solid #21262d',
    borderRadius: 8,
    display: 'flex',
    flexDirection: 'column',
    gap: 20,
  },
  header: {
    textAlign: 'center',
    marginBottom: 8,
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
  error: {
    padding: '10px 14px',
    background: '#3d1a1a',
    border: '1px solid #f85149',
    borderRadius: 6,
    color: '#f85149',
    fontSize: 12,
  },
  description: {
    color: '#8b949e',
    fontSize: 13,
    lineHeight: 1.6,
    textAlign: 'center',
  },
  loginButton: {
    width: '100%',
    padding: '12px 0',
    background: '#1f6feb',
    border: 'none',
    borderRadius: 6,
    color: '#fff',
    fontSize: 13,
    fontWeight: 600,
    cursor: 'pointer',
    letterSpacing: 0.5,
    fontFamily: 'inherit',
  },
  footer: {
    textAlign: 'center',
    fontSize: 11,
    color: '#484f58',
    letterSpacing: 1,
  },
};
