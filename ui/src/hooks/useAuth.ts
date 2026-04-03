import { useState, useEffect, useRef } from 'react';

const GATEWAY = import.meta.env.VITE_GATEWAY_URL || '';
const JWT_KEY = 'seti_jwt';

interface AuthState {
  jwt: string | null;
  wranglerId: string | null;
  clearanceLevel: string | null;
  isLoading: boolean;
}

export function useAuth() {
  const [state, setState] = useState<AuthState>({
    jwt: null,
    wranglerId: null,
    clearanceLevel: null,
    isLoading: true,
  });
  const refreshedRef = useRef(false);

  useEffect(() => {
    if (refreshedRef.current) return;
    refreshedRef.current = true;

    // Check URL fragment for JWT from OIDC callback redirect
    const fragment = window.location.hash;
    if (fragment.startsWith('#jwt=')) {
      const jwt = fragment.slice(5);
      window.history.replaceState(null, '', window.location.pathname);
      storeAndSetJWT(jwt);
      return;
    }

    // Try stored JWT
    const stored = localStorage.getItem(JWT_KEY);
    if (stored && !isExpired(stored)) {
      setJWTState(stored);
      return;
    }

    // Try silent refresh via httpOnly cookie
    silentRefresh().finally(() => {
      setState(prev => ({ ...prev, isLoading: false }));
    });
  }, []);

  function storeAndSetJWT(jwt: string) {
    localStorage.setItem(JWT_KEY, jwt);
    setJWTState(jwt);
  }

  function setJWTState(jwt: string) {
    const claims = parseJWT(jwt);
    setState({
      jwt,
      wranglerId: claims?.wrangler_id ?? null,
      clearanceLevel: claims?.clearance_level ?? null,
      isLoading: false,
    });
  }

  async function silentRefresh() {
    try {
      // credentials: 'include' sends the httpOnly seti_refresh cookie.
      // No need to read or send the token from JS — the browser handles it.
      const res = await fetch(`${GATEWAY}/auth/refresh`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      if (res.ok) {
        const data = await res.json();
        storeAndSetJWT(data.jwt);
        // Cookie is rotated server-side; no localStorage handling needed
      }
    } catch {
      // Silent refresh failed — user needs to log in
    }
  }

  function logout() {
    const jwt = localStorage.getItem(JWT_KEY);
    localStorage.removeItem(JWT_KEY);
    // httpOnly cookie cleared by the logout endpoint
    setState({ jwt: null, wranglerId: null, clearanceLevel: null, isLoading: false });
    if (jwt) {
      fetch(`${GATEWAY}/auth/logout`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${jwt}` },
      }).catch(() => {});
    }
    // In dev mode route to dev login, in production to OIDC login
    const isDevMode = import.meta.env.VITE_AUTH_MODE !== 'production';
    window.location.href = isDevMode ? '/dev-login' : '/login';
  }

  async function getFreshJWT(): Promise<string | null> {
    // Return current JWT if still valid (more than 60s remaining)
    const current = localStorage.getItem(JWT_KEY);
    if (current) {
      const claims = parseJWT(current);
      const exp = Number(claims?.exp ?? 0);
      if (Date.now() / 1000 < exp - 60) return current;
    }
    // Expired or nearly expired — silent refresh
    await silentRefresh();
    const refreshed = localStorage.getItem(JWT_KEY);
    return refreshed && !isExpired(refreshed) ? refreshed : null;
  }

  return { ...state, logout, getFreshJWT };
}

function parseJWT(token: string): Record<string, string> | null {
  try {
    const payload = token.split('.')[1];
    return JSON.parse(atob(payload.replace(/-/g, '+').replace(/_/g, '/')));
  } catch {
    return null;
  }
}

function isExpired(token: string): boolean {
  const claims = parseJWT(token);
  if (!claims?.exp) return true;
  return Date.now() / 1000 > Number(claims.exp);
}
