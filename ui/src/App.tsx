import { Routes, Route, Navigate } from 'react-router-dom';
import { useAuth } from './hooks/useAuth';
import Admin from "./pages/Admin";
import Plots from "./pages/Plots";
import Ring from "./pages/Ring";
import Dashboard from './pages/Dashboard';
import Login from './pages/Login';
import DevLogin from './pages/DevLogin';

export default function App() {
  const { jwt, isLoading } = useAuth();

  if (isLoading) {
    return (
      <div style={styles.loading}>
        <span style={styles.loadingText}>SETI initializing...</span>
      </div>
    );
  }

  // AUTH_MODE is baked into the build. In dev mode (default) we skip the
  // OIDC login page entirely and go straight to the dev login form.
  // In production, VITE_AUTH_MODE=production routes through the AD flow.
  const isDevMode = import.meta.env.VITE_AUTH_MODE !== 'production';
  const loginPath = isDevMode ? '/dev-login' : '/login';

  return (
    <Routes>
      <Route path="/login" element={jwt ? <Navigate to="/" replace /> : <Login />} />
      <Route path="/dev-login" element={jwt ? <Navigate to="/" replace /> : <DevLogin />} />
      <Route
        path="/admin"
        element={jwt ? <Admin /> : <Navigate to={loginPath} replace />}
      />
      <Route
        path="/ring"
        element={jwt ? <Ring /> : <Navigate to={loginPath} replace />}
      />
      <Route
        path="/plots"
        element={jwt ? <Plots /> : <Navigate to={loginPath} replace />}
      />
      <Route
        path="/*"
        element={jwt ? <Dashboard /> : <Navigate to={loginPath} replace />}
      />
    </Routes>
  );
}

const styles = {
  loading: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100vh',
    background: '#0d1117',
  } as React.CSSProperties,
  loadingText: {
    color: '#58a6ff',
    fontSize: '14px',
    letterSpacing: '2px',
    textTransform: 'uppercase' as const,
  },
};
