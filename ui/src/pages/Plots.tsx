import { useState, useEffect, useCallback } from 'react';
import { Link } from 'react-router-dom';
import { useAuth } from '../hooks/useAuth';
import { useConstellation } from '../hooks/useConstellation';
import ConstellationNav from '../components/ConstellationNav';

const GATEWAY = import.meta.env.VITE_GATEWAY_URL || '';

interface PlotStep {
  step_number: number;
  description: string;
  method: string;
  path: string;
  expected_status: number;
  expected_statuses?: number[];
  expected_chain?: Array<{ caller: string; callee: string; method?: string; path?: string }>;
  verify_within_seconds?: number;
}

interface Plot {
  plot_id: string;
  application_id: string;
  name: string;
  description: string;
  author: string;
  version: string;
  tags?: string[];
  steps: PlotStep[];
  flagged: boolean;
  created_at: string;
}

interface ChainCall {
  caller: string;
  callee: string;
  method?: string;
  path?: string;
}

interface StepResult {
  step_number: number;
  description: string;
  passed: boolean;
  chain_passed: boolean;
  actual_status?: number;
  expected_status?: number;
  latency_ms?: number;
  failure_reason?: string;
  chain_matched?: ChainCall[];
  chain_unmatched?: ChainCall[];
  request_method?: string;
  request_body?: unknown;
  response_body?: unknown;
}

interface PlotResult {
  run_id: string;
  plot_id: string;
  plot_name: string;
  application_id: string;
  status: string;
  passed_steps: number;
  failed_steps: number;
  total_steps: number;
  started_at: string;
  completed_at?: string;
  steps?: StepResult[];
}

interface RunAllEntry {
  plot: Plot;
  result: PlotResult | null;
  error?: string;
}

const METHOD_COLORS: Record<string, string> = {
  GET: '#3fb950', POST: '#e3b341', PUT: '#58a6ff',
  DELETE: '#f85149', PATCH: '#d2a8ff',
};

export default function Plots() {
  const { jwt, getFreshJWT, getJWTWithRefresh, wranglerId, clearanceLevel, logout } = useAuth();
  const { active, constellations, setConstellation } = useConstellation(jwt);
  const [plots, setPlots] = useState<Plot[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // FIX 2: Set-based expansion — each plot toggles independently, no mutual collapse
  const [expandedPlots, setExpandedPlots] = useState<Set<string>>(new Set());

  const [plotResults, setPlotResults] = useState<Record<string, PlotResult[]>>({});
  const [loadingResults, setLoadingResults] = useState<Record<string, boolean>>({});
  const [expandedStep, setExpandedStep] = useState<string | null>(null);

  // FIX 3: Scoped per-plot running state — individual run never touches Run All state
  const [runningPlot, setRunningPlot] = useState<string | null>(null);
  const [runStatus, setRunStatus] = useState<Record<string, { msg: string; ok: boolean }>>({});

  // FIX 1: Run All — progress counter only during run, full state update only at end
  const [runAllInProgress, setRunAllInProgress] = useState(false);
  const [runAllProgress, setRunAllProgress] = useState<{ current: number; total: number } | null>(null);
  const [runAllResults, setRunAllResults] = useState<RunAllEntry[] | null>(null);

  // FIX 4: Clipboard feedback
  const [copiedKey, setCopiedKey] = useState<string | null>(null);

  async function authFetch(path: string, options: RequestInit = {}) {
    const freshJwt = await getFreshJWT();
    if (!freshJwt) throw new Error('Session expired');
    return fetch(`${GATEWAY}${path}`, {
      ...options,
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${freshJwt}`,
        ...(options.headers || {}),
      },
    });
  }

  async function loadPlots() {
    setLoading(true);
    try {
      const res = await authFetch(`/plots?application_id=${active.id}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      const sorted = (data.plots || []).sort((a: Plot, b: Plot) =>
        (a.name || '').localeCompare(b.name || '')
      );
      setPlots(sorted);
      setError(null);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load plots');
    } finally {
      setLoading(false);
    }
  }

  // FIX 2: Toggle individual plot without affecting others
  function togglePlot(plot: Plot) {
    setExpandedPlots(prev => {
      const next = new Set(prev);
      if (next.has(plot.plot_id)) {
        next.delete(plot.plot_id);
      } else {
        next.add(plot.plot_id);
        if (!plotResults[plot.plot_id]) fetchResults(plot.plot_id);
      }
      return next;
    });
  }

  async function fetchResults(plotId: string): Promise<PlotResult[]> {
    setLoadingResults(prev => ({ ...prev, [plotId]: true }));
    try {
      const res = await authFetch(`/plot-results?plot_id=${plotId}&limit=5`);
      if (res.ok) {
        const data = await res.json();
        const runs = data.runs || [];
        setPlotResults(prev => ({ ...prev, [plotId]: runs }));
        return runs;
      }
    } catch {
      // silent
    } finally {
      setLoadingResults(prev => ({ ...prev, [plotId]: false }));
    }
    return [];
  }

  async function pollResults(plotId: string) {
    for (let i = 0; i < 8; i++) {
      await new Promise(r => setTimeout(r, 2500));
      const runs = await fetchResults(plotId);
      if (runs.length > 0 && runs[0].status !== 'running') break;
    }
  }

  // FIX 3: Individual run — scoped, no interaction with Run All
  async function runPlot(plot: Plot) {
    if (runningPlot) return;
    setRunningPlot(plot.plot_id);
    setRunStatus(prev => ({ ...prev, [plot.plot_id]: { msg: 'Running — results will appear below', ok: true } }));
    setExpandedPlots(prev => new Set(prev).add(plot.plot_id));
    try {
      const freshJwt = await getJWTWithRefresh();
      if (!freshJwt) throw new Error('Session expired');
      const res = await fetch(`${GATEWAY}/run-plot-test`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${freshJwt}` },
        body: JSON.stringify({ plot_id: plot.plot_id, application_id: plot.application_id }),
      });
      if (res.ok) {
        pollResults(plot.plot_id).then(() => {
          setRunStatus(prev => { const next = { ...prev }; delete next[plot.plot_id]; return next; });
        });
      } else {
        const data = await res.json().catch(() => ({}));
        setRunStatus(prev => ({ ...prev, [plot.plot_id]: { msg: `Failed: ${(data as Record<string, string>).message || res.status}`, ok: false } }));
      }
    } catch (e: unknown) {
      setRunStatus(prev => ({ ...prev, [plot.plot_id]: { msg: e instanceof Error ? e.message : 'Unknown error', ok: false } }));
    } finally {
      setRunningPlot(null);
    }
  }

  // FIX 1: Run All — background loop, zero state updates during loop, single batch at end
  const runAllPlots = useCallback(async () => {
    if (plots.length === 0 || runAllInProgress) return;
    setRunAllInProgress(true);
    setRunAllResults(null);
    setRunAllProgress({ current: 0, total: plots.length });

    const collected: RunAllEntry[] = [];

    for (let i = 0; i < plots.length; i++) {
      const plot = plots[i];
      setRunAllProgress({ current: i + 1, total: plots.length });
      try {
        const freshJwt = await getJWTWithRefresh();
        if (!freshJwt) { collected.push({ plot, result: null, error: 'Session expired' }); continue; }

        const res = await fetch(`${GATEWAY}/run-plot-test`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${freshJwt}` },
          body: JSON.stringify({ plot_id: plot.plot_id, application_id: plot.application_id }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          collected.push({ plot, result: null, error: (data as Record<string, string>).message || `HTTP ${res.status}` });
          continue;
        }

        let runResult: PlotResult | null = null;
        let pollJwt = freshJwt;
        for (let p = 0; p < 20; p++) {
          await new Promise(r => setTimeout(r, 3000));
          if (p > 0 && p % 5 === 0) { const r2 = await getJWTWithRefresh(); if (r2) pollJwt = r2; }
          const rRes = await fetch(`${GATEWAY}/plot-results?plot_id=${plot.plot_id}&limit=1`, {
            headers: { Authorization: `Bearer ${pollJwt}` },
          });
          if (rRes.ok) {
            const data = await rRes.json();
            const run = (data.runs || [])[0];
            if (run && run.status !== 'running') { runResult = run; break; }
          }
        }
        collected.push({ plot, result: runResult });
      } catch (e: unknown) {
        collected.push({ plot, result: null, error: e instanceof Error ? e.message : 'Unknown error' });
      }
    }

    // Single batch state update — one re-render total
    const resultMap: Record<string, PlotResult[]> = {};
    for (const { plot, result } of collected) {
      if (result) resultMap[plot.plot_id] = [result];
    }
    setPlotResults(prev => ({ ...prev, ...resultMap }));
    setRunAllResults(collected);
    setRunAllProgress(null);
    setRunAllInProgress(false);
  }, [plots, runAllInProgress, getJWTWithRefresh]);

  // FIX 4: Copy full run detail to clipboard
  function copyRunToClipboard(plot: Plot, run: PlotResult, key: string) {
    const lines: string[] = [];
    const icon = run.status === 'passed' ? '✓' : '✗';
    lines.push(`${icon} ${plot.name}  [${run.status.toUpperCase()}]`);
    lines.push(`  application: ${plot.application_id}  version: v${plot.version || '?'}`);
    lines.push(`  steps: ${run.passed_steps}/${run.total_steps} passed  run: ${run.run_id}`);
    lines.push(`  started: ${run.started_at ? new Date(run.started_at).toISOString() : 'unknown'}`);
    if (run.steps && run.steps.length > 0) {
      lines.push('');
      lines.push('  STEPS:');
      for (const step of run.steps) {
        const si = step.passed ? '  ✓' : '  ✗';
        lines.push(`${si} Step ${step.step_number}: ${step.description}`);
        lines.push(`       status: ${step.actual_status}/${step.expected_status}  latency: ${step.latency_ms ?? 0}ms  chain: ${step.chain_passed ? 'ok' : 'FAIL'}`);
        if (!step.passed) {
          if (step.failure_reason) lines.push(`       reason: ${step.failure_reason}`);
          // FIX 5: include response body
          if (step.response_body !== undefined) lines.push(`       response: ${JSON.stringify(step.response_body)}`);
          if (step.chain_unmatched && step.chain_unmatched.length > 0) {
            lines.push('       missing calls:');
            for (const c of step.chain_unmatched)
              lines.push(`         - ${c.caller} → ${c.callee}${c.method ? ' ' + c.method : ''}${c.path ? ' ' + c.path : ''}`);
          }
        }
      }
    }
    navigator.clipboard.writeText(lines.join('\n')).then(() => {
      setCopiedKey(key);
      setTimeout(() => setCopiedKey(null), 2000);
    });
  }

  // FIX 5: Report includes response_body on failed steps
  function downloadReport(results: RunAllEntry[]) {
    const ts = new Date().toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
    const passed = results.filter(r => r.result?.status === 'passed').length;
    const lines: string[] = [];
    lines.push('='.repeat(70));
    lines.push('  S.E.T.I. — BEHAVIORAL TEST PLOT REPORT');
    lines.push(`  Generated: ${ts}`);
    lines.push(`  Summary: ${passed}/${results.length} plots passed`);
    lines.push('='.repeat(70));
    lines.push('');

    for (const { plot, result, error } of results) {
      const icon = result?.status === 'passed' ? '✓' : '✗';
      lines.push(`${icon} ${plot.name}  [${result?.status === 'passed' ? 'PASSED' : 'FAILED'}]`);
      lines.push(`  application: ${plot.application_id}  version: v${plot.version || '?'}`);
      if (error) {
        lines.push(`  ERROR: ${error}`);
      } else if (result) {
        lines.push(`  steps: ${result.passed_steps}/${result.total_steps} passed  run: ${result.run_id}`);
        lines.push(`  started: ${result.started_at ? new Date(result.started_at).toISOString() : 'unknown'}`);
        if (result.steps && result.steps.length > 0) {
          lines.push('');
          lines.push('  STEPS:');
          for (const step of result.steps) {
            const si = step.passed ? '  ✓' : '  ✗';
            lines.push(`${si} Step ${step.step_number}: ${step.description}`);
            lines.push(`       status: ${step.actual_status}/${step.expected_status}  latency: ${step.latency_ms ?? 0}ms  chain: ${step.chain_passed ? 'ok' : 'FAIL'}`);
            if (!step.passed) {
              if (step.failure_reason) lines.push(`       reason: ${step.failure_reason}`);
              if (step.response_body !== undefined) lines.push(`       response: ${JSON.stringify(step.response_body)}`);
              if (step.chain_unmatched && step.chain_unmatched.length > 0) {
                lines.push('       missing calls:');
                for (const c of step.chain_unmatched)
                  lines.push(`         - ${c.caller} → ${c.callee}${c.method ? ' ' + c.method : ''}${c.path ? ' ' + c.path : ''}`);
              }
            }
          }
        }
      }
      lines.push('');
      lines.push('-'.repeat(70));
      lines.push('');
    }
    lines.push('Report generated by S.E.T.I. — Search for Erroneous Tessellated Interactions');

    const blob = new Blob([lines.join('\n')], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    const now = new Date();
    a.download = `seti-plot-report-${now.toISOString().slice(0, 10)}-${now.toTimeString().slice(0, 8).replace(/:/g, '-')}.txt`;
    document.body.appendChild(a); a.click(); document.body.removeChild(a);
    URL.revokeObjectURL(url);
  }

  useEffect(() => { if (jwt) loadPlots(); }, [jwt, active.id]);

  return (
    <div style={s.root}>
      <div style={s.header}>
        <div style={s.headerLeft}>
          <span style={s.logo}>S.E.T.I.</span>
          <span style={s.pageBadge}>PLOTS</span>
        </div>
        <div style={s.headerRight}>
          <Link to="/" style={s.navBtnGray}>{'<- Dashboard'}</Link>
          {(clearanceLevel === 'sec-wr4ngler' || clearanceLevel === 'admin') && (
            <Link to="/ring" style={s.navBtnGold}>Ring</Link>
          )}
          {(clearanceLevel === 'sec_wrangler' || clearanceLevel === 'admin') && (
            <Link to="/admin" style={s.navBtnGold}>Admin</Link>
          )}
          <span style={s.wranglerId}>{wranglerId}</span>
          <button style={s.logoutBtn} onClick={logout}>Sign out</button>
        </div>
      </div>

      <ConstellationNav constellations={constellations} active={active} onSelect={setConstellation} />

      <div style={s.body}>
        <div style={s.intro}>
          <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 16 }}>
            <div>
              <div style={s.introTitle}>BEHAVIORAL TEST PLOTS</div>
              <div style={s.introText}>
                Plots define real user flows with expected inter-service call chains.
                Each plot is a committed artifact in contracts/plots/, versioned alongside
                the OpenAPI contracts. Click a plot to expand steps and run history.
              </div>
            </div>
            {plots.length > 0 && (
              <button
                style={{ ...s.runAllBtn, opacity: runAllInProgress ? 0.5 : 1, flexShrink: 0 }}
                onClick={runAllPlots}
                disabled={runAllInProgress}
              >
                {runAllInProgress && runAllProgress
                  ? `Running ${runAllProgress.current}/${runAllProgress.total}...`
                  : `>> Run All (${plots.length})`}
              </button>
            )}
          </div>
        </div>

        {/* Run All results — appears only after entire run completes */}
        {runAllResults && (
          <div style={s.runAllPanel}>
            <div style={s.runAllHeader}>
              <span style={s.runAllTitle}>RUN ALL RESULTS</span>
              <span style={{
                ...s.runAllSummary,
                color: runAllResults.every(r => r.result?.status === 'passed') ? '#3fb950' : '#f85149',
              }}>
                {runAllResults.filter(r => r.result?.status === 'passed').length}/{runAllResults.length} passed
              </span>
              <button style={s.reportBtn} onClick={() => downloadReport(runAllResults)}>↓ Report</button>
              <button style={s.bannerClose} onClick={() => setRunAllResults(null)}>x</button>
            </div>
            {runAllResults.map(({ plot, result, error }) => (
              <div key={plot.plot_id} style={{
                ...s.runAllRow,
                borderLeftColor: result?.status === 'passed' ? '#3fb950' : '#f85149',
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{ color: result?.status === 'passed' ? '#3fb950' : '#f85149', fontSize: 12, flexShrink: 0 }}>
                    {result?.status === 'passed' ? '+' : 'x'}
                  </span>
                  <span style={s.runAllPlotName}>{plot.name}</span>
                  {result
                    ? <span style={s.runAllSteps}>{result.passed_steps}/{result.total_steps} steps</span>
                    : <span style={{ fontSize: 11, color: '#f85149', fontStyle: 'italic' }}>{error || 'No result'}</span>}
                </div>
                {/* FIX 5: Show all steps in Run All panel including response bodies */}
                {result?.steps && result.steps.length > 0 && (
                  <div style={s.runAllStepList}>
                    {result.steps.map(step => (
                      <div key={step.step_number} style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                        <div style={s.runAllStepRow}>
                          <span style={{ color: step.passed ? '#3fb950' : '#f85149', fontSize: 11, flexShrink: 0 }}>
                            {step.passed ? '+' : 'x'}
                          </span>
                          <span style={{ fontSize: 11, color: '#8b949e', flex: 1 }}>{step.description}</span>
                          <span style={{ fontSize: 11, color: '#6e7681', fontFamily: "'Courier New', monospace" }}>
                            {step.actual_status} / {step.expected_status}
                          </span>
                          <span style={{ fontSize: 11, color: '#484f58' }}>{step.latency_ms ?? 0}ms</span>
                        </div>
                        {!step.passed && step.failure_reason && (
                          <div style={{ fontSize: 11, color: '#f85149', fontStyle: 'italic', paddingLeft: 16 }}>
                            {step.failure_reason}
                          </div>
                        )}
                        {!step.passed && step.response_body !== undefined && (
                          <pre style={{ ...s.stepDetailPre, marginLeft: 16 }}>
                            {JSON.stringify(step.response_body, null, 2)}
                          </pre>
                        )}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            ))}
          </div>
        )}

        {error && (
          <div style={s.bannerErr}>
            <span>{error}</span>
            <button style={s.bannerClose} onClick={() => setError(null)}>x</button>
          </div>
        )}

        {loading ? (
          <div style={s.empty}>Loading plots...</div>
        ) : plots.length === 0 ? (
          <div style={s.empty}>No plots found for {active.label}.</div>
        ) : (
          <div style={s.plotList}>
            {plots.map(plot => {
              const expanded = expandedPlots.has(plot.plot_id);
              const results = plotResults[plot.plot_id] || [];
              const isLoadingR = loadingResults[plot.plot_id];
              const lastRun = results[0];

              return (
                <div key={plot.plot_id} style={{ ...s.plotCard, borderColor: expanded ? '#1f6feb' : '#21262d' }}>
                  <div style={s.plotCardTop} onClick={() => togglePlot(plot)}>
                    <div style={s.plotCardLeft}>
                      <span style={s.arrow}>{expanded ? 'v' : '>'}</span>
                      <div>
                        <div style={s.plotName}>{plot.name}</div>
                        <div style={s.plotMeta}>
                          <span style={s.metaItem}>{plot.application_id}</span>
                          <span style={s.metaDot}>.</span>
                          <span style={s.metaItem}>{plot.steps.length} step{plot.steps.length !== 1 ? 's' : ''}</span>
                          {plot.version && (<><span style={s.metaDot}>.</span><span style={s.metaItem}>v{plot.version}</span></>)}
                          {lastRun && (
                            <span style={{ ...s.lastRunBadge, color: lastRun.status === 'passed' ? '#3fb950' : '#f85149' }}>
                              last: {lastRun.status}
                            </span>
                          )}
                        </div>
                      </div>
                    </div>
                    <button
                      style={{ ...s.runBtn, opacity: runningPlot === plot.plot_id ? 0.5 : 1 }}
                      onClick={e => { e.stopPropagation(); runPlot(plot); }}
                      disabled={runningPlot === plot.plot_id || runAllInProgress}
                    >
                      {runningPlot === plot.plot_id ? 'Running...' : '> Run'}
                    </button>
                  </div>

                  {expanded && (
                    <div style={s.expandedBody}>
                      <div style={s.plotDesc}>{plot.description}</div>

                      {plot.tags && plot.tags.length > 0 && (
                        <div style={s.tagRow}>
                          {plot.tags.map(tag => <span key={tag} style={s.tag}>{tag}</span>)}
                        </div>
                      )}

                      <div style={s.section}>
                        <div style={s.sectionTitle}>STEPS</div>
                        {plot.steps.map(step => (
                          <div key={step.step_number} style={s.step}>
                            <div style={s.stepRow}>
                              <span style={s.stepNum}>{step.step_number}</span>
                              <span style={s.stepDesc}>{step.description}</span>
                              <span style={{ ...s.stepMethod, color: METHOD_COLORS[step.method] || '#8b949e' }}>{step.method}</span>
                              <span style={s.stepPath}>{step.path}</span>
                              <span style={s.stepExpected}>
                                → {step.expected_statuses ? `[${step.expected_statuses.join('|')}]` : step.expected_status}
                              </span>
                            </div>
                            {step.expected_chain && step.expected_chain.length > 0 && (
                              <div style={s.chainRow}>
                                <span style={s.chainLabel}>Expected calls:</span>
                                {step.expected_chain.map((call, i) => (
                                  <span key={i} style={s.chainItem}>
                                    {call.caller} {'->'} {call.callee}{call.method ? ` ${call.method}` : ''}{call.path ? ` ${call.path}` : ''}
                                  </span>
                                ))}
                              </div>
                            )}
                          </div>
                        ))}
                      </div>

                      {runStatus[plot.plot_id] && (
                        <div style={{
                          fontSize: 11,
                          color: runStatus[plot.plot_id].ok ? '#3fb950' : '#f85149',
                          marginBottom: 10,
                          padding: '6px 10px',
                          border: `1px solid ${runStatus[plot.plot_id].ok ? '#3fb950' : '#f85149'}`,
                          borderRadius: 4,
                        }}>
                          {runStatus[plot.plot_id].ok ? '+ ' : 'x '}{runStatus[plot.plot_id].msg}
                        </div>
                      )}

                      <div style={s.section}>
                        <div style={s.resultsSectionHeader}>
                          <div style={s.sectionTitle}>RECENT RUNS</div>
                          <button style={s.refreshBtn} onClick={e => { e.stopPropagation(); fetchResults(plot.plot_id); }}>
                            {isLoadingR ? '...' : 'Refresh'}
                          </button>
                        </div>

                        {isLoadingR && results.length === 0 ? (
                          <div style={s.noResults}>Loading results...</div>
                        ) : results.length === 0 ? (
                          <div style={s.noResults}>No runs yet — click Run to execute this plot</div>
                        ) : (
                          results.map(run => {
                            const copyKey = `copy-${run.run_id}`;
                            return (
                              <div key={run.run_id} style={{
                                ...s.resultRow,
                                borderLeftColor: run.status === 'passed' ? '#3fb950' : run.status === 'failed' ? '#f85149' : '#e3b341',
                              }}>
                                <div style={s.resultHeader}>
                                  <span style={{ ...s.resultStatus, color: run.status === 'passed' ? '#3fb950' : run.status === 'failed' ? '#f85149' : '#e3b341' }}>
                                    {run.status === 'passed' ? '+' : 'x'} {run.status.toUpperCase()}
                                  </span>
                                  <span style={s.resultSteps}>{run.passed_steps}/{run.total_steps} steps passed</span>
                                  <span style={s.resultTime}>{run.started_at ? new Date(run.started_at).toLocaleString() : ''}</span>
                                  <span style={s.resultId}>{run.run_id.slice(-12)}</span>
                                  {/* FIX 4: Copy to Clipboard button */}
                                  <button
                                    style={s.copyBtn}
                                    onClick={e => { e.stopPropagation(); copyRunToClipboard(plot, run, copyKey); }}
                                    title="Copy run detail to clipboard"
                                  >
                                    {copiedKey === copyKey ? '✓ Copied' : '⎘ Copy'}
                                  </button>
                                </div>
                                {run.steps && run.steps.length > 0 && (
                                  <div style={s.stepResultList}>
                                    {run.steps.map(step => {
                                      const stepKey = `${run.run_id}-${step.step_number}`;
                                      const isExpandedStep = expandedStep === stepKey;
                                      return (
                                        <div key={step.step_number}>
                                          <div
                                            style={{ ...s.stepResultRow, cursor: 'pointer' }}
                                            onClick={() => setExpandedStep(isExpandedStep ? null : stepKey)}
                                          >
                                            <span style={{ color: step.passed ? '#3fb950' : '#f85149', fontSize: 11, flexShrink: 0 }}>
                                              {step.passed ? '+' : 'x'}
                                            </span>
                                            <span style={s.stepResultDesc}>{step.description}</span>
                                            {step.actual_status !== undefined && (
                                              <span style={s.stepResultCode}>{step.actual_status} / {step.expected_status}</span>
                                            )}
                                            {step.latency_ms !== undefined && (
                                              <span style={s.stepResultLatency}>{step.latency_ms}ms</span>
                                            )}
                                            <span style={{ fontSize: 10, color: '#484f58' }}>{isExpandedStep ? 'v' : '>'}</span>
                                          </div>
                                          {isExpandedStep && (
                                            <div style={s.stepDetail}>
                                              <div style={s.stepDetailRow}>
                                                <span style={s.stepDetailLabel}>Request:</span>
                                                <span style={s.stepDetailValue}>{step.request_method} {step.description}</span>
                                              </div>
                                              {step.request_body !== undefined && (
                                                <div style={s.stepDetailRow}>
                                                  <span style={s.stepDetailLabel}>Body sent:</span>
                                                  <pre style={s.stepDetailPre}>{JSON.stringify(step.request_body, null, 2)}</pre>
                                                </div>
                                              )}
                                              {step.response_body !== undefined && (
                                                <div style={s.stepDetailRow}>
                                                  <span style={s.stepDetailLabel}>Response:</span>
                                                  <pre style={{ ...s.stepDetailPre, borderColor: step.passed ? '#3fb950' : '#f85149' }}>
                                                    {JSON.stringify(step.response_body, null, 2)}
                                                  </pre>
                                                </div>
                                              )}
                                              {step.failure_reason && (
                                                <div style={s.stepDetailRow}>
                                                  <span style={s.stepDetailLabel}>Reason:</span>
                                                  <span style={s.stepResultError}>{step.failure_reason}</span>
                                                </div>
                                              )}
                                              {step.chain_unmatched && step.chain_unmatched.length > 0 && (
                                                <div style={s.stepDetailRow}>
                                                  <span style={s.stepDetailLabel}>Missing calls:</span>
                                                  <div style={s.chainDetailList}>
                                                    {step.chain_unmatched.map((c, i) => (
                                                      <span key={i} style={s.chainDetailItemFail}>
                                                        {c.caller} {'→'} {c.callee}{c.method ? ` ${c.method}` : ''}{c.path ? ` ${c.path}` : ''}
                                                      </span>
                                                    ))}
                                                  </div>
                                                </div>
                                              )}
                                              {step.chain_matched && step.chain_matched.length > 0 && (
                                                <div style={s.stepDetailRow}>
                                                  <span style={s.stepDetailLabel}>Observed:</span>
                                                  <div style={s.chainDetailList}>
                                                    {step.chain_matched.map((c, i) => (
                                                      <span key={i} style={s.chainDetailItemPass}>
                                                        {c.caller} {'→'} {c.callee}{c.method ? ` ${c.method}` : ''}{c.path ? ` ${c.path}` : ''}
                                                      </span>
                                                    ))}
                                                  </div>
                                                </div>
                                              )}
                                            </div>
                                          )}
                                        </div>
                                      );
                                    })}
                                  </div>
                                )}
                              </div>
                            );
                          })
                        )}
                      </div>
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}

const s: Record<string, React.CSSProperties> = {
  root: { display: 'flex', flexDirection: 'column', minHeight: '100vh', background: '#0d1117', color: '#e6edf3', fontFamily: "'Segoe UI', system-ui, sans-serif" },
  header: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '10px 20px', background: '#161b22', borderBottom: '1px solid #21262d' },
  headerLeft: { display: 'flex', alignItems: 'center', gap: 16 },
  headerRight: { display: 'flex', alignItems: 'center', gap: 16 },
  logo: { fontSize: 16, fontWeight: 700, color: '#58a6ff', letterSpacing: 3 },
  pageBadge: { fontSize: 10, color: '#58a6ff', border: '1px solid #1f6feb', borderRadius: 4, padding: '2px 6px', letterSpacing: 1 },
  navBtnGray: { fontSize: 11, color: '#8b949e', textDecoration: 'none', border: '1px solid #30363d', borderRadius: 4, padding: '3px 10px' },
  navBtnGold: { fontSize: 11, color: '#e3b341', textDecoration: 'none', border: '1px solid #e3b341', borderRadius: 4, padding: '3px 10px' },
  wranglerId: { fontSize: 11, color: '#484f58' },
  logoutBtn: { background: 'none', border: '1px solid #30363d', borderRadius: 4, color: '#8b949e', fontSize: 11, cursor: 'pointer', padding: '4px 10px', fontFamily: 'inherit' },
  body: { padding: '24px 32px', maxWidth: 960, margin: '0 auto', width: '100%' },
  intro: { marginBottom: 24 },
  introTitle: { fontSize: 11, color: '#484f58', letterSpacing: 2, marginBottom: 6 },
  introText: { fontSize: 12, color: '#6e7681', lineHeight: 1.7 },
  bannerOk: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', border: '1px solid #3fb950', borderRadius: 6, padding: '10px 16px', marginBottom: 16, fontSize: 12, color: '#3fb950' },
  bannerErr: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', border: '1px solid #f85149', borderRadius: 6, padding: '10px 16px', marginBottom: 16, fontSize: 12, color: '#f85149' },
  bannerClose: { background: 'none', border: 'none', cursor: 'pointer', fontFamily: 'inherit', fontSize: 14, color: 'inherit' },
  empty: { fontSize: 12, color: '#484f58', textAlign: 'center', padding: '40px 20px' },
  plotList: { display: 'flex', flexDirection: 'column', gap: 8 },
  plotCard: { background: '#161b22', border: '1px solid', borderRadius: 8, overflow: 'hidden', transition: 'border-color 0.1s' },
  plotCardTop: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '14px 18px', cursor: 'pointer' },
  plotCardLeft: { display: 'flex', alignItems: 'center', gap: 12, flex: 1 },
  arrow: { fontSize: 11, color: '#484f58', flexShrink: 0 },
  plotName: { fontSize: 14, fontWeight: 600, color: '#e6edf3', marginBottom: 4 },
  plotMeta: { display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' },
  metaItem: { fontSize: 11, color: '#484f58' },
  metaDot: { fontSize: 11, color: '#30363d' },
  lastRunBadge: { fontSize: 10, border: '1px solid currentColor', borderRadius: 3, padding: '1px 5px' },
  runBtn: { background: '#388bfd26', border: '1px solid #1f6feb', borderRadius: 4, color: '#58a6ff', fontSize: 11, cursor: 'pointer', padding: '5px 14px', fontFamily: 'inherit', whiteSpace: 'nowrap', flexShrink: 0 },
  expandedBody: { padding: '0 18px 18px 42px' },
  plotDesc: { fontSize: 12, color: '#8b949e', lineHeight: 1.6, marginBottom: 12 },
  tagRow: { display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 16 },
  tag: { fontSize: 10, color: '#6e7681', background: '#21262d', border: '1px solid #30363d', borderRadius: 3, padding: '1px 6px' },
  section: { marginBottom: 16 },
  sectionTitle: { fontSize: 10, color: '#484f58', letterSpacing: 2, marginBottom: 8 },
  resultsSectionHeader: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 8 },
  refreshBtn: { background: 'none', border: '1px solid #30363d', borderRadius: 4, color: '#6e7681', fontSize: 11, cursor: 'pointer', padding: '2px 8px', fontFamily: 'inherit' },
  noResults: { fontSize: 12, color: '#484f58', fontStyle: 'italic', padding: '6px 0' },
  step: { background: '#0d1117', borderRadius: 4, padding: '9px 12px', marginBottom: 5, border: '1px solid #21262d' },
  stepRow: { display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' },
  stepNum: { fontSize: 10, color: '#484f58', border: '1px solid #30363d', borderRadius: 10, padding: '1px 6px', fontFamily: "'Courier New', monospace", flexShrink: 0 },
  stepDesc: { fontSize: 12, color: '#c9d1d9', flex: 1 },
  stepMethod: { fontSize: 11, fontWeight: 600, fontFamily: "'Courier New', monospace" },
  stepPath: { fontSize: 12, color: '#58a6ff', fontFamily: "'Courier New', monospace" },
  stepExpected: { fontSize: 11, color: '#3fb950' },
  chainRow: { marginTop: 6, display: 'flex', flexWrap: 'wrap', gap: 6, alignItems: 'center' },
  chainLabel: { fontSize: 10, color: '#484f58', letterSpacing: 0.5 },
  chainItem: { fontSize: 11, color: '#6e7681', background: '#161b22', border: '1px solid #21262d', borderRadius: 3, padding: '2px 8px', fontFamily: "'Courier New', monospace" },
  resultRow: { background: '#0d1117', borderRadius: 4, padding: '10px 12px', marginBottom: 6, borderLeft: '3px solid', borderTop: '1px solid #21262d', borderRight: '1px solid #21262d', borderBottom: '1px solid #21262d' },
  resultHeader: { display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' },
  resultStatus: { fontSize: 12, fontWeight: 600 },
  resultSteps: { fontSize: 11, color: '#6e7681' },
  resultTime: { fontSize: 11, color: '#484f58', flex: 1, textAlign: 'right' },
  resultId: { fontSize: 10, color: '#30363d', fontFamily: "'Courier New', monospace" },
  copyBtn: { background: 'none', border: '1px solid #30363d', borderRadius: 4, color: '#6e7681', fontSize: 10, cursor: 'pointer', padding: '2px 8px', fontFamily: 'inherit', whiteSpace: 'nowrap', flexShrink: 0 },
  stepResultList: { marginTop: 8, display: 'flex', flexDirection: 'column', gap: 4 },
  stepResultRow: { display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' },
  stepResultDesc: { fontSize: 11, color: '#8b949e', flex: 1 },
  stepResultCode: { fontSize: 11, color: '#6e7681', fontFamily: "'Courier New', monospace" },
  stepResultLatency: { fontSize: 11, color: '#484f58' },
  stepResultError: { fontSize: 11, color: '#f85149', fontStyle: 'italic' },
  reportBtn: { background: 'none', border: '1px solid #30363d', borderRadius: 4, color: '#8b949e', fontSize: 11, cursor: 'pointer', padding: '3px 10px', fontFamily: 'inherit' },
  runAllBtn: { background: '#388bfd26', border: '1px solid #1f6feb', borderRadius: 4, color: '#58a6ff', fontSize: 11, cursor: 'pointer', padding: '7px 16px', fontFamily: 'inherit', whiteSpace: 'nowrap' },
  runAllPanel: { background: '#161b22', border: '1px solid #21262d', borderRadius: 8, padding: '16px 20px', marginBottom: 20 },
  runAllHeader: { display: 'flex', alignItems: 'center', gap: 12, marginBottom: 12 },
  runAllTitle: { fontSize: 10, color: '#484f58', letterSpacing: 2, flex: 1 },
  runAllSummary: { fontSize: 13, fontWeight: 600 },
  runAllRow: { display: 'flex', flexDirection: 'column' as const, gap: 4, padding: '8px 10px', marginBottom: 6, borderLeft: '3px solid', borderTop: '1px solid #21262d', borderRight: '1px solid #21262d', borderBottom: '1px solid #21262d', borderRadius: '0 4px 4px 0', background: '#0d1117' },
  runAllPlotName: { fontSize: 13, fontWeight: 600, color: '#e6edf3', flex: 1 },
  runAllSteps: { fontSize: 11, color: '#6e7681' },
  runAllStepList: { display: 'flex', flexDirection: 'column' as const, gap: 4, marginTop: 4, paddingLeft: 12 },
  runAllStepRow: { display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' as const, fontSize: 11 },
  stepDetail: { background: '#161b22', borderRadius: 4, padding: '8px 12px', marginTop: 4, marginLeft: 16 },
  stepDetailRow: { display: 'flex', gap: 8, marginTop: 6, alignItems: 'flex-start' },
  stepDetailLabel: { fontSize: 10, color: '#484f58', letterSpacing: 1, flexShrink: 0, marginTop: 2 },
  stepDetailValue: { fontSize: 11, color: '#8b949e' },
  stepDetailPre: { fontSize: 10, color: '#8b949e', background: '#161b22', border: '1px solid #21262d', borderRadius: 3, padding: '6px 8px', margin: 0, overflow: 'auto', maxHeight: 200 },
  chainDetailList: { display: 'flex', flexDirection: 'column' as const, gap: 3 },
  chainDetailItemPass: { fontSize: 10, color: '#3fb950', fontFamily: "'Courier New', monospace", background: 'rgba(63,185,80,0.06)', padding: '1px 6px', borderRadius: 2 },
  chainDetailItemFail: { fontSize: 10, color: '#f85149', fontFamily: "'Courier New', monospace", background: 'rgba(248,81,73,0.06)', padding: '1px 6px', borderRadius: 2 },
};
