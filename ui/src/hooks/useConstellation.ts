import { useState, useEffect, useCallback } from 'react';
import { useSearchParams } from 'react-router-dom';

const GATEWAY = import.meta.env.VITE_GATEWAY_URL || '';

export interface Constellation {
  id: string;       // application_id used in API calls — 'seti' or 'app-tca-vox'
  label: string;    // display name shown in the tab
  tag: string;      // short tag from remote-apps.json — 'seti' or 'tca-vox'
  isSelf: boolean;  // true only for the SETI self-monitoring entry
}

// ---------------------------------------------------------------------------
// useConstellation
//
// Provides the active constellation selection (from ?constellation= param)
// and the list of available constellations from Policy.
//
// Usage:
//   const { active, constellations, setConstellation } = useConstellation(jwt);
//   // active.id is the application_id to pass to API calls
// ---------------------------------------------------------------------------

export function useConstellation(jwt: string | null) {
  const [searchParams, setSearchParams] = useSearchParams();
  const [constellations, setConstellations] = useState<Constellation[]>([seti]);
  const [loading, setLoading] = useState(false);

  // Load available constellations from Policy
  useEffect(() => {
    if (!jwt) return;

    setLoading(true);
    fetch(`${GATEWAY}/available-applications`, {
      headers: { Authorization: `Bearer ${jwt}` },
    })
      .then(r => r.ok ? r.json() : { applications: [] })
      .then(data => {
        const active = (data.applications || []).filter(
          (a: { monitoring_status: string }) => a.monitoring_status === 'active'
        );

        const external: Constellation[] = active.map((a: {
          tag: string;
          name: string;
          application_id?: string;
        }) => ({
          id: a.application_id || `app-${a.tag}`,
          label: a.name,
          tag: a.tag,
          isSelf: false,
        }));

        // SETI self is always first, pinned
        setConstellations([seti, ...external]);
      })
      .catch(() => {
        // No available-applications endpoint or error — show SETI only
        setConstellations([seti]);
      })
      .finally(() => setLoading(false));
  }, [jwt]);

  // Active constellation — read from URL param, default to 'seti'
  const activeId = searchParams.get('constellation') || 'seti';
  const active = constellations.find(c => c.id === activeId) ?? seti;

  const setConstellation = useCallback((id: string) => {
    setSearchParams(prev => {
      const next = new URLSearchParams(prev);
      if (id === 'seti') {
        next.delete('constellation'); // clean URL for the default
      } else {
        next.set('constellation', id);
      }
      return next;
    });
  }, [setSearchParams]);

  return { active, constellations, setConstellation, loading };
}

// The SETI self entry — always present, not from remote-apps.json
const seti: Constellation = {
  id: 'seti',
  label: 'SETI',
  tag: 'seti',
  isSelf: true,
};
