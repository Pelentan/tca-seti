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

    // Expired or missing — try silent refresh via httpOnly cookie
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
      const res = await fetch(`${GATEWAY}/auth/refresh`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      if (res.ok) {
        const data = await res.json();
        storeAndSetJWT(data.jwt);
      } else {
        localStorage.removeItem(JWT_KEY);
        setState({ jwt: null, wranglerId: null, clearanceLevel: null, isLoading: false });
      }
    } catch {
      // Network error — don't clear state
    }
  }

  function logout() {
    const jwt = localStorage.getItem(JWT_KEY);
    localStorage.removeItem(JWT_KEY);
    setState({ jwt: null, wranglerId: null, clearanceLevel: null, isLoading: false });
    if (jwt) {
      fetch(`${GATEWAY}/auth/logout`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${jwt}` },
      }).catch(() => {});
    }
    const isDevMode = import.meta.env.VITE_AUTH_MODE !== 'production';
    window.location.href = isDevMode ? '/dev-login' : '/login';
  }

  // ---------------------------------------------------------------------------
  // getFreshJWT — called before every authenticated API call.
  // Always refreshes — every interaction with the backend extends the session.
  // ---------------------------------------------------------------------------
  async function getFreshJWT(): Promise<string | null> {
    await silentRefresh();
    const refreshed = localStorage.getItem(JWT_KEY);
    return refreshed && !isExpired(refreshed) ? refreshed : null;
  }

  // Alias — kept for call sites that explicitly want refresh-on-action semantics
  const getJWTWithRefresh = getFreshJWT;

  return { ...state, logout, getFreshJWT, getJWTWithRefresh };
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
