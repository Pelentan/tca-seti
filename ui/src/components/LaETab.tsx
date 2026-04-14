import { useState, useEffect, useRef, useCallback } from 'react';

const GATEWAY = import.meta.env.VITE_GATEWAY_URL || '';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface MetricSample {
  ts: number;       // Unix ms
  latency_ms: number;
  healthy: boolean;
  service: string;
}

interface AutoBaseline {
  mean_latency_ms: number;
  sample_count: number;
  established_at: string;
}

interface WranglerBaseline {
  latency_threshold_ms: number;
  failure_rate_threshold: number;
  alert_level: string;
  set_by: string;
  set_at: string;
}

interface ServiceMetrics {
  service_name: string;
  samples: MetricSample[];
  auto_baseline: AutoBaseline | null;
  wr4ngler_baseline: WranglerBaseline | null;
}

interface MetricsResponse {
  services: ServiceMetrics[];
  fetched_at: number;
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const TIME_SLICES = [
  { label: '20 min', ms: 20 * 60 * 1000 },
  { label: '1 hr',   ms: 60 * 60 * 1000 },
  { label: '6 hr',   ms: 6 * 60 * 60 * 1000 },
  { label: '24 hr',  ms: 24 * 60 * 60 * 1000 },
];

type FilterMode = 'ALL' | 'LATENCY' | 'ERRORS';

// ---------------------------------------------------------------------------
// Abbreviation — ≤4 chars: use as-is. Hyphenated: initials. Single long word: first 4 uppercase.
// ---------------------------------------------------------------------------

function abbrev(name: string): string {
  if (name.length <= 4) return name.toUpperCase();
  const parts = name.split('-');
  if (parts.length > 1) return parts.map(p => p[0].toUpperCase()).join('');
  return name.slice(0, 4).toUpperCase();
}

// ---------------------------------------------------------------------------
// SVG dual-axis chart — latency (left, green) + error rate (right, red)
// ---------------------------------------------------------------------------

interface ChartProps {
  samples: MetricSample[];
  width: number;
  height: number;
  filter: FilterMode;
  autoBaseline: AutoBaseline | null;
  wranglerThresholdMs: number | null;
}

function DualAxisChart({ samples, width, height, filter, autoBaseline, wranglerThresholdMs }: ChartProps) {
  if (!samples || samples.length === 0) {
    return (
      <svg width={width} height={height}>
        <text x={width / 2} y={height / 2} textAnchor="middle"
          fill="#484f58" fontSize={11} fontFamily="'Courier New', monospace">
          no data
        </text>
      </svg>
    );
  }

  const PAD = { top: 8, right: 48, bottom: 24, left: 52 };
  const W = width - PAD.left - PAD.right;
  const H = height - PAD.top - PAD.bottom;

  // Time domain
  const tMin = samples[0].ts;
  const tMax = samples[samples.length - 1].ts;
  const tRange = Math.max(tMax - tMin, 1);
  const xOf = (ts: number) => PAD.left + ((ts - tMin) / tRange) * W;

  // Latency domain — left axis
  const latencies = samples.map(s => s.latency_ms).filter(v => v > 0);
  const latMax = Math.max(...latencies, autoBaseline?.mean_latency_ms ?? 0, wranglerThresholdMs ?? 0, 10) * 1.2;
  const yLatOf = (v: number) => PAD.top + H - (v / latMax) * H;

  // Error rate domain — right axis (0–100%)
  // Compute rolling error rate per sample point (5-sample window)
  const errorRates = samples.map((_, i) => {
    const window = samples.slice(Math.max(0, i - 4), i + 1);
    const errs = window.filter(s => !s.healthy).length;
    return (errs / window.length) * 100;
  });
  const errMax = Math.max(...errorRates, 10);
  const yErrOf = (v: number) => PAD.top + H - (v / errMax) * H;

  // Build SVG paths
  const latPath = samples.reduce((acc, s, i) => {
    if (s.latency_ms <= 0) return acc;
    const x = xOf(s.ts), y = yLatOf(s.latency_ms);
    return acc + (i === 0 || !samples[i-1] || samples[i-1].latency_ms <= 0 ? `M${x},${y}` : `L${x},${y}`);
  }, '');

  const errPath = errorRates.reduce((acc, v, i) => {
    const x = xOf(samples[i].ts), y = yErrOf(v);
    return acc + (i === 0 ? `M${x},${y}` : `L${x},${y}`);
  }, '');

  // X-axis tick labels (up to 5)
  const xTicks = Array.from({ length: 5 }, (_, i) => {
    const ts = tMin + (tRange * i) / 4;
    const d = new Date(ts);
    const label = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    return { x: PAD.left + (W * i) / 4, label };
  });

  // Y-axis ticks (left — latency)
  const yTicksLeft = [0, 0.25, 0.5, 0.75, 1].map(f => ({
    y: PAD.top + H * (1 - f),
    label: Math.round(latMax * f) + 'ms',
  }));

  // Y-axis ticks (right — error %)
  const yTicksRight = [0, 0.5, 1].map(f => ({
    y: PAD.top + H * (1 - f),
    label: Math.round(errMax * f) + '%',
  }));

  return (
    <svg width={width} height={height} style={{ overflow: 'visible' }}>
      {/* Grid lines */}
      {yTicksLeft.map((t, i) => (
        <line key={i} x1={PAD.left} x2={PAD.left + W} y1={t.y} y2={t.y}
          stroke="#21262d" strokeWidth={1} />
      ))}

      {/* Auto-baseline reference line */}
      {autoBaseline && (filter === 'ALL' || filter === 'LATENCY') && (
        <>
          <line
            x1={PAD.left} x2={PAD.left + W}
            y1={yLatOf(autoBaseline.mean_latency_ms)}
            y2={yLatOf(autoBaseline.mean_latency_ms)}
            stroke="#388bfd" strokeWidth={1} strokeDasharray="4,3" opacity={0.6} />
          <text x={PAD.left + W + 2} y={yLatOf(autoBaseline.mean_latency_ms) + 4}
            fill="#388bfd" fontSize={8} fontFamily="'Courier New', monospace" opacity={0.7}>
            base
          </text>
        </>
      )}

      {/* Wr4ngler threshold line */}
      {wranglerThresholdMs && (filter === 'ALL' || filter === 'LATENCY') && (
        <>
          <line
            x1={PAD.left} x2={PAD.left + W}
            y1={yLatOf(wranglerThresholdMs)} y2={yLatOf(wranglerThresholdMs)}
            stroke="#e3b341" strokeWidth={1} strokeDasharray="6,2" opacity={0.8} />
          <text x={PAD.left + 2} y={yLatOf(wranglerThresholdMs) - 2}
            fill="#e3b341" fontSize={8} fontFamily="'Courier New', monospace">
            SLA
          </text>
        </>
      )}

      {/* Latency line */}
      {(filter === 'ALL' || filter === 'LATENCY') && latPath && (
        <path d={latPath} fill="none" stroke="#3fb950" strokeWidth={1.5}
          strokeLinejoin="round" strokeLinecap="round" />
      )}

      {/* Error rate line */}
      {(filter === 'ALL' || filter === 'ERRORS') && errPath && (
        <path d={errPath} fill="none" stroke="#f85149" strokeWidth={1.5}
          strokeLinejoin="round" strokeLinecap="round" opacity={0.85} />
      )}

      {/* Left Y-axis */}
      <line x1={PAD.left} x2={PAD.left} y1={PAD.top} y2={PAD.top + H}
        stroke="#30363d" strokeWidth={1} />
      {(filter === 'ALL' || filter === 'LATENCY') && yTicksLeft.map((t, i) => (
        <text key={i} x={PAD.left - 4} y={t.y + 4} textAnchor="end"
          fill="#484f58" fontSize={9} fontFamily="'Courier New', monospace">
          {t.label}
        </text>
      ))}

      {/* Right Y-axis */}
      <line x1={PAD.left + W} x2={PAD.left + W} y1={PAD.top} y2={PAD.top + H}
        stroke="#30363d" strokeWidth={1} />
      {(filter === 'ALL' || filter === 'ERRORS') && yTicksRight.map((t, i) => (
        <text key={i} x={PAD.left + W + 4} y={t.y + 4} textAnchor="start"
          fill="#484f58" fontSize={9} fontFamily="'Courier New', monospace">
          {t.label}
        </text>
      ))}

      {/* X-axis */}
      <line x1={PAD.left} x2={PAD.left + W} y1={PAD.top + H} y2={PAD.top + H}
        stroke="#30363d" strokeWidth={1} />
      {xTicks.map((t, i) => (
        <text key={i} x={t.x} y={PAD.top + H + 14} textAnchor="middle"
          fill="#484f58" fontSize={9} fontFamily="'Courier New', monospace">
          {t.label}
        </text>
      ))}
    </svg>
  );
}

// ---------------------------------------------------------------------------
// Job row — alert line input (stubbed) + chart
// ---------------------------------------------------------------------------

interface JobRowProps {
  svc: ServiceMetrics;
  filter: FilterMode;
  constellationAbbrev: string;
}

function JobRow({ svc, filter, constellationAbbrev }: JobRowProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [chartWidth, setChartWidth] = useState(600);

  useEffect(() => {
    if (!containerRef.current) return;
    const obs = new ResizeObserver(entries => {
      const w = entries[0]?.contentRect.width;
      if (w) setChartWidth(Math.max(200, w - 80)); // 80 for alert line column
    });
    obs.observe(containerRef.current);
    return () => obs.disconnect();
  }, []);

  const wranglerMs = svc.wr4ngler_baseline?.latency_threshold_ms
    ? Number(svc.wr4ngler_baseline.latency_threshold_ms)
    : null;

  const latest = svc.samples[svc.samples.length - 1];
  const latestLatency = latest?.latency_ms ?? null;
  const latestHealthy = latest?.healthy ?? true;

  return (
    <div style={s.jobRow}>
      {/* Left label + alert line stub */}
      <div style={s.jobLabel}>
        <div style={s.jobName}>
          <span style={s.jobAbbrev}>{constellationAbbrev}:{abbrev(svc.service_name)}</span>
          <span style={s.jobFullName}>{svc.service_name}</span>
        </div>
        <div style={s.jobStats}>
          {latestLatency !== null && (
            <span style={{ color: '#3fb950', fontSize: 10 }}>{Math.round(latestLatency)}ms</span>
          )}
          {!latestHealthy && (
            <span style={{ color: '#f85149', fontSize: 10 }}>ERR</span>
          )}
        </div>
        <div style={s.alertLineStub} title="Wr4ngler alert threshold — coming soon">
          <span style={s.alertLineLabel}>⚠ alert</span>
          <input
            type="number"
            disabled
            placeholder="—"
            style={s.alertLineInput}
            title="Set alert threshold (stub)"
          />
        </div>
      </div>

      {/* Chart */}
      <div ref={containerRef} style={s.chartContainer}>
        <DualAxisChart
          samples={svc.samples}
          width={chartWidth}
          height={90}
          filter={filter}
          autoBaseline={svc.auto_baseline}
          wranglerThresholdMs={wranglerMs}
        />
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// LaE Tab
// ---------------------------------------------------------------------------

interface LaETabProps {
  jwt: string | null;
  getFreshJWT: () => Promise<string | null>;
}

export default function LaETab({ jwt, getFreshJWT }: LaETabProps) {
  const [metrics, setMetrics] = useState<ServiceMetrics[]>([]);
  const [availableJobs, setAvailableJobs] = useState<string[]>([]);
  const [initialLoad, setInitialLoad] = useState(true);
  const [roping, setRoping] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<FilterMode>('ALL');
  const [timeSliceIdx, setTimeSliceIdx] = useState(0);
  const [selectedConstellation, setSelectedConstellation] = useState('');
  const [selectedJob, setSelectedJob] = useState('ALL');
  const [lastFetch, setLastFetch] = useState<Date | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
  // Stable ref for getFreshJWT — avoids infinite useEffect loops when the
  // function reference changes on every render from the parent hook
  const getFreshJWTRef = useRef(getFreshJWT);
  useEffect(() => { getFreshJWTRef.current = getFreshJWT; }, [getFreshJWT]);

  const constellations = ['SETI'];

  // Fetch available jobs when constellation is selected — uses metrics endpoint
  // (same source as the charts, confirmed working) with a minimal time window
  useEffect(() => {
    if (!selectedConstellation || selectedConstellation === '') {
      setAvailableJobs([]);
      return;
    }
    let cancelled = false;
    async function loadJobs() {
      try {
        const freshJwt = await getFreshJWTRef.current();
        if (!freshJwt || cancelled) return;
        // Fetch a small window just to get service names — no samples needed
        const sinceMs = Date.now() - 60 * 60 * 1000; // 1hr window
        const res = await fetch(`${GATEWAY}/augur-canis/metrics?since_ms=${sinceMs}`, {
          headers: { Authorization: `Bearer ${freshJwt}` },
        });
        if (!res.ok || cancelled) return;
        const data: MetricsResponse = await res.json();
        if (!cancelled) {
          const jobs = (data.services || [])
            .map(s => s.service_name)
            .filter(Boolean)
            .sort();
          setAvailableJobs(jobs);
        }
      } catch { /* ignore */ }
    }
    loadJobs();
    return () => { cancelled = true; };
  }, [selectedConstellation]); // getFreshJWT intentionally excluded — stable via ref

  // Metrics fetch — silent on background refresh, spinner only on first pull
  const fetchMetrics = useCallback(async (isBackground = false) => {
    if (!jwt) return;
    try {
      const freshJwt = await getFreshJWTRef.current();
      if (!freshJwt) return;
      const sinceMs = Date.now() - TIME_SLICES[timeSliceIdx].ms;
      const res = await fetch(`${GATEWAY}/augur-canis/metrics?since_ms=${sinceMs}`, {
        headers: { Authorization: `Bearer ${freshJwt}` },
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: MetricsResponse = await res.json();
      setMetrics(data.services || []);
      setLastFetch(new Date());
      setError(null);
    } catch (e: unknown) {
      if (!isBackground) setError(e instanceof Error ? e.message : 'Failed to load metrics');
    } finally {
      if (!isBackground) setInitialLoad(false);
    }
  }, [jwt, timeSliceIdx]); // getFreshJWT intentionally excluded — stable via ref

  // When roping and time slice changes, do a fresh initial load
  useEffect(() => {
    if (!roping) return;
    setInitialLoad(true);
    fetchMetrics(false);
  }, [fetchMetrics, roping]);

  function handleRope() {
    if (roping) {
      setRoping(false);
      if (intervalRef.current) clearInterval(intervalRef.current);
      intervalRef.current = null;
    } else {
      setRoping(true);
      setInitialLoad(true);
      fetchMetrics(false);
      // Background refreshes — silent, no spinner, charts stay visible
      intervalRef.current = setInterval(() => fetchMetrics(true), 30_000);
    }
  }

  useEffect(() => () => {
    if (intervalRef.current) clearInterval(intervalRef.current);
  }, []);

  // Jobs for dropdown: prefer availableJobs (loaded on constellation select),
  // fall back to whatever metrics returned if Rope is active
  const jobList = availableJobs.length > 0
    ? availableJobs
    : metrics.map(s => s.service_name).sort();

  const visibleServices = metrics
    .filter(svc => selectedJob === 'ALL' || svc.service_name === selectedJob)
    .sort((a, b) => a.service_name.localeCompare(b.service_name));

  const constellationLabel = selectedConstellation && selectedConstellation !== 'ALL'
    ? selectedConstellation
    : 'SETI';

  return (
    <div style={s.root}>
      {/* Controls */}
      <div style={s.controls}>
        <div style={s.controlsRow}>
          {/* Constellation */}
          <div style={s.controlGroup}>
            <label style={s.controlLabel}>CONSTELLATION</label>
            <select style={s.select} value={selectedConstellation}
              onChange={e => { setSelectedConstellation(e.target.value); setSelectedJob('ALL'); }}>
              <option value="">— Select —</option>
              <option value="ALL">All</option>
              {constellations.sort().map(c => (
                <option key={c} value={c}>{c}</option>
              ))}
            </select>
          </div>

          {/* Job — populated on constellation select */}
          <div style={s.controlGroup}>
            <label style={s.controlLabel}>JOB</label>
            <select style={s.select} value={selectedJob}
              onChange={e => setSelectedJob(e.target.value)}
              disabled={!selectedConstellation}>
              <option value="ALL">All</option>
              {jobList.map(name => (
                <option key={name} value={name}>
                  {constellationLabel} — {name}
                </option>
              ))}
            </select>
          </div>

          {/* Time slice */}
          <div style={s.controlGroup}>
            <label style={s.controlLabel}>WINDOW</label>
            <select style={s.select} value={timeSliceIdx}
              onChange={e => setTimeSliceIdx(Number(e.target.value))}>
              {TIME_SLICES.map((ts, i) => (
                <option key={i} value={i}>{ts.label}</option>
              ))}
            </select>
          </div>

          {/* Filter buttons */}
          <div style={s.controlGroup}>
            <label style={s.controlLabel}>SHOW</label>
            <div style={s.filterBtns}>
              {(['ALL', 'LATENCY', 'ERRORS'] as FilterMode[]).map(f => (
                <button key={f}
                  style={{ ...s.filterBtn, ...(filter === f ? s.filterBtnActive : {}) }}
                  onClick={() => setFilter(f)}>
                  {f}
                </button>
              ))}
              <select
                style={{ ...s.select, opacity: filter === 'ERRORS' ? 1 : 0.3, cursor: filter === 'ERRORS' ? 'pointer' : 'not-allowed', minWidth: 120 }}
                disabled={filter !== 'ERRORS'}
                title={filter !== 'ERRORS' ? 'Select Errors to enable' : 'Filter by error type'}>
                <option>Error type ▾</option>
              </select>
            </div>
          </div>

          {/* Rope / Release */}
          <div style={{ ...s.controlGroup, marginLeft: 'auto', alignItems: 'flex-end' }}>
            <label style={s.controlLabel}>&nbsp;</label>
            <button
              style={{ ...s.ropeBtn, ...(roping ? s.ropeBtnActive : {}) }}
              onClick={handleRope}
              disabled={!selectedConstellation}>
              {roping ? '⬡ Release' : '⬡ Rope'}
            </button>
            {lastFetch && (
              <span style={s.lastFetch}>
                {lastFetch.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })}
              </span>
            )}
          </div>
        </div>
      </div>

      {/* Legend */}
      <div style={s.legend}>
        {(filter === 'ALL' || filter === 'LATENCY') && (
          <>
            <span style={legendDot('#3fb950')} />
            <span style={s.legendLabel}>Latency (ms, left axis)</span>
            <span style={legendDot('#388bfd')} />
            <span style={{ ...s.legendLabel, opacity: 0.7 }}>Auto-baseline</span>
            <span style={legendDot('#e3b341')} />
            <span style={{ ...s.legendLabel, opacity: 0.7 }}>SLA threshold</span>
          </>
        )}
        {(filter === 'ALL' || filter === 'ERRORS') && (
          <>
            <span style={legendDot('#f85149')} />
            <span style={s.legendLabel}>Error rate (%, right axis)</span>
          </>
        )}
        <span style={{ marginLeft: 'auto', fontSize: 10, color: '#30363d' }}>
          {roping
            ? `${visibleServices.length} job${visibleServices.length !== 1 ? 's' : ''} · ${TIME_SLICES[timeSliceIdx].label} window`
            : ''}
        </span>
      </div>

      {/* Charts — idle, spinner on first load, then live (silent background refresh) */}
      {!roping && metrics.length === 0 ? (
        <div style={s.idle}>
          <div style={s.idleTitle}>SELECT A CONSTELLATION AND HIT ROPE</div>
          <div style={s.idleText}>
            Rope pulls live metric data and starts the 30-second refresh cycle.
            Release stops it.
          </div>
        </div>
      ) : roping && initialLoad ? (
        <div style={s.empty}>Pulling metrics...</div>
      ) : error && metrics.length === 0 ? (
        <div style={s.errorBanner}>{error}</div>
      ) : visibleServices.length === 0 ? (
        <div style={s.empty}>
          No metric data yet — AC starts recording after the first health check cycle (~30s).
        </div>
      ) : (
        <div style={s.chartList}>
          {visibleServices.map(svc => (
            <JobRow
              key={svc.service_name}
              svc={svc}
              filter={filter}
              constellationAbbrev={constellationLabel}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

const s: Record<string, React.CSSProperties> = {
  root: { display: 'flex', flexDirection: 'column', minHeight: 0, flex: 1 },
  controls: { background: '#0d1117', borderBottom: '1px solid #21262d', padding: '12px 32px' },
  controlsRow: { display: 'flex', alignItems: 'flex-end', gap: 20, flexWrap: 'wrap' as const },
  controlGroup: { display: 'flex', flexDirection: 'column', gap: 4 },
  controlLabel: { fontSize: 9, color: '#484f58', letterSpacing: 1.5 },
  select: { background: '#161b22', border: '1px solid #30363d', borderRadius: 4, color: '#e6edf3', fontSize: 11, padding: '5px 8px', fontFamily: 'inherit', outline: 'none', cursor: 'pointer' },
  filterBtns: { display: 'flex', gap: 4, alignItems: 'center' },
  filterBtn: { background: 'none', border: '1px solid #30363d', borderRadius: 3, color: '#6e7681', fontSize: 10, cursor: 'pointer', padding: '4px 10px', fontFamily: "'Courier New', monospace", letterSpacing: 1 },
  filterBtnActive: { background: '#388bfd26', border: '1px solid #388bfd', color: '#58a6ff' },
  refreshBtn: { background: 'none', border: '1px solid #30363d', borderRadius: 3, color: '#6e7681', fontSize: 10, cursor: 'pointer', padding: '5px 10px', fontFamily: 'inherit' },
  ropeBtn: { background: 'none', border: '1px solid #30363d', borderRadius: 3, color: '#6e7681', fontSize: 11, cursor: 'pointer', padding: '5px 14px', fontFamily: "'Courier New', monospace", letterSpacing: 1, transition: 'all 0.15s' },
  ropeBtnActive: { background: '#3fb95026', border: '1px solid #3fb950', color: '#3fb950' },
  lastFetch: { fontSize: 9, color: '#30363d', fontFamily: "'Courier New', monospace" },
  idle: { display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', flex: 1, gap: 12, padding: 48 },
  idleTitle: { fontSize: 11, color: '#484f58', letterSpacing: 2 },
  idleText: { fontSize: 12, color: '#30363d', textAlign: 'center' as const, maxWidth: 360, lineHeight: 1.6 },
  legend: { display: 'flex', alignItems: 'center', gap: 8, padding: '8px 32px', borderBottom: '1px solid #21262d', fontSize: 10, color: '#6e7681' },
  legendLabel: { fontSize: 10, color: '#6e7681' },
  empty: { padding: '32px', fontSize: 12, color: '#484f58', fontStyle: 'italic', textAlign: 'center' as const },
  errorBanner: { margin: '16px 32px', fontSize: 12, color: '#f85149', border: '1px solid #f85149', borderRadius: 4, padding: '10px 14px' },
  chartList: { display: 'flex', flexDirection: 'column', gap: 1, overflow: 'auto' as const, flex: 1 },
  jobRow: { display: 'flex', alignItems: 'stretch', borderBottom: '1px solid #161b22', background: '#0d1117', minHeight: 118 },
  jobLabel: { display: 'flex', flexDirection: 'column', justifyContent: 'space-between', padding: '10px 10px 10px 32px', minWidth: 120, maxWidth: 120, borderRight: '1px solid #21262d', gap: 4 },
  jobName: { display: 'flex', flexDirection: 'column', gap: 2 },
  jobAbbrev: { fontSize: 11, color: '#58a6ff', fontFamily: "'Courier New', monospace", fontWeight: 700, letterSpacing: 0.5 },
  jobFullName: { fontSize: 9, color: '#484f58', fontFamily: "'Courier New', monospace" },
  jobStats: { display: 'flex', gap: 6 },
  alertLineStub: { display: 'flex', flexDirection: 'column', gap: 3 },
  alertLineLabel: { fontSize: 8, color: '#30363d', letterSpacing: 1 },
  alertLineInput: { background: '#161b22', border: '1px solid #21262d', borderRadius: 3, color: '#484f58', fontSize: 10, padding: '3px 6px', width: '100%', fontFamily: "'Courier New', monospace", cursor: 'not-allowed', outline: 'none' },
  chartContainer: { flex: 1, padding: '4px 8px', display: 'flex', alignItems: 'center', minWidth: 0 },
};

// Helper for legend dots
function legendDot(color: string): React.CSSProperties {
  return { width: 8, height: 8, borderRadius: '50%', background: color, display: 'inline-block', flexShrink: 0 };
}
