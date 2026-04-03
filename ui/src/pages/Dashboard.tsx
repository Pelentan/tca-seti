import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useAuth } from '../hooks/useAuth';
import { useEventStream, SETIEvent } from '../hooks/useEventStream';

export default function Dashboard() {
  const { jwt, wranglerId, clearanceLevel, logout, getFreshJWT } = useAuth();
  const { events, connected, eventCount, clearEvents } = useEventStream(jwt);
  const [filter, setFilter] = useState('');
  const [paused, setPaused] = useState(false);
  const [hideChecks, setHideChecks] = useState(false);
  const [testing, setTesting] = useState(false);
  const [lastTestStatus, setLastTestStatus] = useState<string | null>(null);
  const [selectedEvent, setSelectedEvent] = useState<SETIEvent | null>(null);

  async function triggerContractTest() {
    setTesting(true);
    setLastTestStatus(null);
    try {
      const freshJwt = await getFreshJWT();
      if (!freshJwt) {
        setLastTestStatus('error');
        return;
      }
      const res = await fetch(`${import.meta.env.VITE_GATEWAY_URL || ''}/run-contract-test`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${freshJwt}`,
        },
        body: JSON.stringify({ application_id: 'seti-self' }),
      });
      setLastTestStatus(res.ok ? 'accepted' : 'error');
    } catch {
      setLastTestStatus('error');
    } finally {
      setTesting(false);
    }
  }

  const isHealthCheck = (e: SETIEvent) => e.path === '/check';

  let displayEvents = events.filter(e => e.caller && e.timestamp && new Date(e.timestamp).getTime() > 0);
  if (hideChecks) displayEvents = displayEvents.filter(e => !isHealthCheck(e));
  if (filter) {
    displayEvents = displayEvents.filter(e =>
      e.caller.includes(filter) || e.callee.includes(filter) || e.path.includes(filter)
    );
  }

  return (
    <div style={styles.root}>
      {/* Header */}
      <div style={styles.header}>
        <div style={styles.headerLeft}>
          <span style={styles.logo}>S.E.T.I.</span>
          <div style={styles.statusDot(connected)} title={connected ? 'Stream connected' : 'Stream disconnected'} />
          <span style={styles.statusLabel}>{connected ? 'LIVE' : 'DISCONNECTED'}</span>
        </div>
        <div style={styles.headerRight}>
          <Link to="/plots" style={styles.navLink('#58a6ff', '#1f6feb')}>Plots</Link>
          {(clearanceLevel === 'sec_wrangler' || clearanceLevel === 'admin') && (
            <Link to="/admin" style={styles.navLink('#e3b341', '#e3b341')}>Admin</Link>
          )}
          <span style={styles.wranglerId}>{wranglerId}</span>
          <button style={styles.logoutBtn} onClick={logout}>Sign out</button>
        </div>
      </div>

      {/* Controls */}
      <div style={styles.controls}>
        <div style={styles.stats}>
          <span style={styles.statItem}>
            <span style={styles.statValue}>{eventCount}</span> events received
          </span>
          <span style={styles.statItem}>
            <span style={styles.statValue}>{displayEvents.length}</span> displayed
          </span>
        </div>
        <div style={styles.controlActions}>
          <input
            style={styles.filterInput}
            placeholder="Filter by service or path..."
            value={filter}
            onChange={e => setFilter(e.target.value)}
          />
          <button style={styles.controlBtn(hideChecks)} onClick={() => setHideChecks(h => !h)}>
            {hideChecks ? 'Show Checks' : 'Hide Checks'}
          </button>
          <button style={styles.controlBtn(paused)} onClick={() => setPaused(p => !p)}>
            {paused ? 'Resume' : 'Pause'}
          </button>
          <button style={styles.controlBtn(false)} onClick={clearEvents}>Clear</button>
          <button
            style={{
              ...styles.controlBtn(false),
              borderColor: lastTestStatus === 'accepted' ? '#3fb950' : lastTestStatus === 'error' ? '#f85149' : '#30363d',
              color: lastTestStatus === 'accepted' ? '#3fb950' : lastTestStatus === 'error' ? '#f85149' : '#8b949e',
            }}
            onClick={triggerContractTest}
            disabled={testing}
          >
            {testing ? 'Testing...' : lastTestStatus === 'accepted' ? 'Test Queued ✓' : 'Run Contract Tests'}
          </button>
        </div>
      </div>

      {/* Event stream */}
      <div style={styles.streamContainer}>
        <div style={styles.streamHeader}>
          <span style={styles.col('caller')}>CALLER</span>
          <span style={styles.col('arrow')}></span>
          <span style={styles.col('callee')}>CALLEE</span>
          <span style={styles.col('method')}>METHOD</span>
          <span style={styles.col('path')}>PATH</span>
          <span style={styles.col('status')}>STATUS</span>
          <span style={styles.col('latency')}>LATENCY</span>
          <span style={styles.col('time')}>TIME</span>
        </div>
        <div style={styles.eventList}>
          {displayEvents.length === 0 && (
            <div style={styles.empty}>
              {connected ? 'Waiting for inter-service events...' : 'Connecting to event stream...'}
            </div>
          )}
          {displayEvents.map((event, i) => (
            <EventRow
              key={`${event.id}-${i}`}
              event={event as SETIEvent}
              onClick={() => setSelectedEvent(event)}
            />
          ))}
        </div>
      </div>

      {/* Event detail modal */}
      {selectedEvent && (
        <EventModal event={selectedEvent} jwt={jwt} onClose={() => setSelectedEvent(null)} />
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Event row
// ---------------------------------------------------------------------------

function EventRow({ event, onClick }: { event: SETIEvent; key?: string; onClick: () => unknown }) {
  const [hovered, setHovered] = useState(false);
  const isCheck = event.path === '/check';

  const statusColor = event.statusCode >= 500 ? '#f85149'
    : event.statusCode >= 400 ? '#e3b341'
    : '#3fb950';

  const time = new Date(event.timestamp).toLocaleTimeString('en-US', {
    hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit',
  });

  return (
    <div
      style={{
        ...styles.eventRow,
        background: hovered ? '#161b22' : 'transparent',
        cursor: 'pointer',
        opacity: isCheck ? 0.6 : 1,
      }}
      onClick={onClick}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
    >
      <span style={{ ...styles.col('caller'), color: '#79c0ff' }}>{event.caller}</span>
      <span style={{ ...styles.col('arrow'), color: '#6e7681' }}>→</span>
      <span style={{ ...styles.col('callee'), color: '#d2a8ff' }}>{event.callee}</span>
      <span style={{ ...styles.col('method'), color: '#e3b341' }}>{event.method}</span>
      <span style={{ ...styles.col('path'), color: '#c9d1d9' }}>{event.path}</span>
      <span style={{ ...styles.col('status'), color: statusColor }}>{event.statusCode}</span>
      <span style={{ ...styles.col('latency'), color: event.latencyMs > 100 ? '#e3b341' : '#56d364' }}>
        {event.latencyMs}ms
      </span>
      <span style={{ ...styles.col('time'), color: '#8b949e' }}>{time}</span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Event detail modal
// ---------------------------------------------------------------------------

interface ContractRun {
  run_id: string;
  application_id: string;
  status: string;
  total_tests: number;
  passed_tests: number;
  failed_tests: number;
  skipped_tests: number;
  started_at: string;
  completed_at: string;
  results: Array<{
    test_name: string;
    service_name: string;
    passed: boolean;
    actual_status: number;
    expected_status: number;
    failure_reason?: string;
    skip_reason?: string;
    latency_ms: number;
    executed_at: string;
  }>;
}

function EventModal({ event, jwt, onClose }: { event: SETIEvent; jwt: string | null; onClose: () => void }) {
  const [contractRuns, setContractRuns] = useState<ContractRun[]>([]);
  const [loadingRuns, setLoadingRuns] = useState(false);
  const [copied, setCopied] = useState(false);

  function copyAll() {
    const lines: string[] = [];
    lines.push(`SETI Event — ${event.caller} → ${event.callee}`);
    lines.push(`${'='.repeat(60)}`);
    lines.push(`Event ID:   ${event.id}`);
    lines.push(`Timestamp:  ${new Date(event.timestamp).toLocaleString()}`);
    lines.push(`Protocol:   ${event.protocol}`);
    lines.push(`Method:     ${event.method}`);
    lines.push(`Path:       ${event.path}`);
    lines.push(`Status:     ${event.statusCode}`);
    lines.push(`Latency:    ${event.latencyMs}ms`);
    if (contractRuns.length > 0) {
      lines.push('');
      lines.push('CONTRACT TEST RESULTS');
      lines.push('-'.repeat(60));
      for (const run of contractRuns) {
        lines.push(`Run: ${run.run_id}  Status: ${run.status.toUpperCase()}  ${run.passed_tests}✓ ${run.failed_tests}✗ ${run.skipped_tests} skipped`);
        for (const r of run.results || []) {
          const icon = r.skip_reason ? '–' : r.passed ? '✓' : '✗';
          const detail = r.skip_reason
            ? `skip: ${r.skip_reason}`
            : `${r.actual_status} / ${r.expected_status}${r.failure_reason ? ' — ' + r.failure_reason : ''}${r.latency_ms ? ' ' + r.latency_ms + 'ms' : ''}`;
          lines.push(`  ${icon} ${r.service_name.padEnd(20)} ${(r.test_name || '').padEnd(30)} ${detail}`);
        }
        lines.push('');
      }
    }
    lines.push('RAW EVENT');
    lines.push('-'.repeat(60));
    lines.push(JSON.stringify(event, null, 2));

    navigator.clipboard.writeText(lines.join('\n')).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  }

  const statusColor = event.statusCode >= 500 ? '#f85149'
    : event.statusCode >= 400 ? '#e3b341'
    : '#3fb950';

  const isCheck = event.path === '/check';
  const isContractTest = event.caller === 'contract-test' || event.callee === 'contract-test'
    || event.path === '/contract-results' || event.path === '/run';

  // Fetch full contract run details when a contract test event is opened
  const fetchedRef = useState(false);
  if (isContractTest && jwt && !fetchedRef[0]) {
    fetchedRef[1](true);
    setLoadingRuns(true);
    const gw = import.meta.env.VITE_GATEWAY_URL || '';
    // Use jwt directly here — modal opens immediately after a test run so token is fresh
    const modalJwt = jwt;
    // First get the list to find the most recent run IDs
    fetch(`${gw}/contract-results`, { headers: { Authorization: `Bearer ${modalJwt}` } })
      .then(r => r.ok ? r.json() : null)
      .then(async (data) => {
        if (!data?.runs?.length) return;
        // Fetch full details for the 3 most recent runs (includes individual test results)
        const details = await Promise.all(
          data.runs.slice(0, 3).map((run: { run_id: string }) =>
            fetch(`${gw}/contract-results/${run.run_id}`, {
              headers: { Authorization: `Bearer ${jwt}` },
            }).then(r => r.ok ? r.json() : null)
          )
        );
        setContractRuns(details.filter(Boolean));
      })
      .catch(() => {})
      .finally(() => setLoadingRuns(false));
  }

  return (
    <div style={styles.modalOverlay} onClick={onClose}>
      <div style={styles.modal} onClick={(e: React.MouseEvent) => e.stopPropagation()}>
        {/* Modal header */}
        <div style={styles.modalHeader}>
          <div style={styles.modalTitle}>
            <span style={{ color: '#79c0ff' }}>{event.caller}</span>
            <span style={{ color: '#484f58', margin: '0 8px' }}>→</span>
            <span style={{ color: '#d2a8ff' }}>{event.callee}</span>
            <span style={{
              marginLeft: 12, fontSize: 10, padding: '2px 6px',
              border: `1px solid ${isCheck ? '#30363d' : isContractTest ? '#1f6feb' : '#484f58'}`,
              borderRadius: 3,
              color: isCheck ? '#484f58' : isContractTest ? '#58a6ff' : '#8b949e',
            }}>
              {isCheck ? 'health check' : isContractTest ? 'contract test' : 'service call'}
            </span>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button
              style={{ ...styles.modalClose, fontSize: 11, padding: '3px 10px',
                border: '1px solid #30363d', borderRadius: 4,
                color: copied ? '#3fb950' : '#8b949e' }}
              onClick={copyAll}
            >
              {copied ? 'Copied ✓' : 'Copy All'}
            </button>
            <button style={styles.modalClose} onClick={onClose}>✕</button>
          </div>
        </div>

        <div style={styles.modalBody}>
          {/* Core fields */}
          <div style={styles.fieldGrid}>
            <Field label="Event ID" value={event.id} />
            <Field label="Timestamp" value={new Date(event.timestamp).toLocaleString()} />
            <Field label="Protocol" value={event.protocol} />
            <Field label="Method" value={event.method} color="#e3b341" />
            <Field label="Path" value={event.path} />
            <Field label="Status" value={String(event.statusCode)} color={statusColor} />
            <Field label="Latency" value={`${event.latencyMs}ms`}
              color={event.latencyMs > 100 ? '#e3b341' : '#56d364'} />
          </div>

          {/* Contract test results — auto-loaded */}
          {isContractTest && (
            <div style={styles.payloadSection}>
              <div style={styles.payloadLabel}>CONTRACT TEST RESULTS</div>
              {loadingRuns ? (
                <div style={styles.payloadNote}>Loading results from Results Job...</div>
              ) : contractRuns.length === 0 ? (
                <div style={styles.payloadNote}>
                  No results found. The run may still be in progress or the Results Job may not
                  yet have received the completed run.
                </div>
              ) : contractRuns.map(run => (
                <div key={run.run_id} style={styles.runCard}>
                  <div style={styles.runHeader}>
                    <span style={{ color: '#8b949e', fontSize: 11 }}>{run.run_id}</span>
                    <span style={{
                      fontSize: 11, fontWeight: 600,
                      color: run.status === 'passed' ? '#3fb950' : run.status === 'failed' ? '#f85149' : '#e3b341',
                    }}>{run.status.toUpperCase()}</span>
                    <span style={{ fontSize: 11, color: '#484f58' }}>
                      {run.passed_tests}✓ {run.failed_tests}✗ {run.skipped_tests} skipped
                    </span>
                  </div>
                  <div style={styles.testList}>
                    {run.results?.map((r, i) => (
                      <div key={i} style={styles.testResult(r.passed, !!r.skip_reason)}>
                        <span style={{ color: r.skip_reason ? '#484f58' : r.passed ? '#3fb950' : '#f85149' }}>
                          {r.skip_reason ? '–' : r.passed ? '✓' : '✗'}
                        </span>
                        <span style={{ color: '#8b949e', minWidth: 100, fontSize: 11 }}>{r.service_name}</span>
                        <span style={{ flex: 1, fontSize: 11, color: '#6e7681' }}>{r.test_name}</span>
                        {!r.skip_reason && (
                          <span style={{ fontSize: 11, color: r.passed ? '#484f58' : '#f85149' }}>
                            {r.actual_status} / {r.expected_status}
                            {r.failure_reason ? ` — ${r.failure_reason}` : ''}
                            {r.latency_ms ? ` ${r.latency_ms}ms` : ''}
                          </span>
                        )}
                        {r.skip_reason && (
                          <span style={{ fontSize: 11, color: '#484f58' }}>{r.skip_reason}</span>
                        )}
                      </div>
                    ))}
                  </div>
                </div>
              ))}
            </div>
          )}

          {/* Health check note */}
          {isCheck && (
            <div style={styles.payloadSection}>
              <div style={styles.payloadLabel}>MECHANISM</div>
              <div style={styles.payloadNote}>
                Health check stubs return synthetic healthy responses via Redis pub/sub on{' '}
                <code>tca:check-requests</code> / <code>tca:check-results:{'{request_id}'}</code>.
                No network call to the Job — the healthcheck binary inside the container
                coordinates with Augur Canis through Redis.
              </div>
            </div>
          )}

          {/* Regular service call note */}
          {!isCheck && !isContractTest && (
            <div style={styles.payloadSection}>
              <div style={styles.payloadLabel}>REQUEST / RESPONSE PAYLOAD</div>
              <div style={styles.payloadNote}>
                The observability stream carries metadata only. Request and response bodies are
                not transmitted — they may contain credentials or PII that must not flow through
                a shared monitoring layer.
              </div>
            </div>
          )}

          {/* Raw event */}
          <div style={styles.payloadSection}>
            <div style={styles.payloadLabel}>RAW EVENT</div>
            <pre style={styles.rawJson}>{JSON.stringify(event, null, 2)}</pre>
          </div>
        </div>
      </div>
    </div>
  );
}

function Field({ label, value, color }: { label: string; value: string; color?: string }) {
  return (
    <div style={styles.field}>
      <div style={styles.fieldLabel}>{label}</div>
      <div style={{ ...styles.fieldValue, color: color || '#e6edf3' }}>{value}</div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

const COL_WIDTHS: Record<string, string> = {
  caller: '130px',
  arrow: '20px',
  callee: '130px',
  method: '60px',
  path: '1fr',
  status: '60px',
  latency: '70px',
  time: '80px',
};

const styles = {
  root: {
    display: 'flex',
    flexDirection: 'column' as const,
    height: '100vh',
    background: '#0d1117',
    overflow: 'hidden',
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    padding: '10px 20px',
    background: '#161b22',
    borderBottom: '1px solid #21262d',
    flexShrink: 0,
  },
  headerLeft: { display: 'flex', alignItems: 'center', gap: 12 },
  logo: { fontSize: 16, fontWeight: 700, color: '#58a6ff', letterSpacing: 3 } as React.CSSProperties,
  statusDot: (connected: boolean): React.CSSProperties => ({
    width: 8, height: 8, borderRadius: '50%',
    background: connected ? '#3fb950' : '#f85149',
    boxShadow: connected ? '0 0 6px #3fb950' : 'none',
  }),
  statusLabel: { fontSize: 11, color: '#8b949e', letterSpacing: 2 } as React.CSSProperties,
  headerRight: { display: 'flex', alignItems: 'center', gap: 16 },
  clearance: {
    fontSize: 10, color: '#8b949e', letterSpacing: 1,
    padding: '2px 8px', border: '1px solid #30363d', borderRadius: 4,
  } as React.CSSProperties,
  wranglerId: { fontSize: 11, color: '#484f58' } as React.CSSProperties,
  navLink: (color: string, border: string): React.CSSProperties => ({
    fontSize: 11, color, textDecoration: 'none',
    border: `1px solid ${border}`, borderRadius: 4, padding: '3px 10px',
  }),
  logoutBtn: {
    background: 'none', border: '1px solid #30363d', borderRadius: 4,
    color: '#8b949e', fontSize: 11, cursor: 'pointer', padding: '4px 10px', fontFamily: 'inherit',
  } as React.CSSProperties,
  controls: {
    display: 'flex', alignItems: 'center', justifyContent: 'space-between',
    padding: '8px 20px', background: '#0d1117', borderBottom: '1px solid #21262d', flexShrink: 0,
  },
  stats: { display: 'flex', gap: 20 },
  statItem: { fontSize: 12, color: '#8b949e' } as React.CSSProperties,
  statValue: { color: '#e6edf3', fontWeight: 600 } as React.CSSProperties,
  controlActions: { display: 'flex', alignItems: 'center', gap: 8 },
  filterInput: {
    background: '#161b22', border: '1px solid #30363d', borderRadius: 4,
    color: '#e6edf3', fontSize: 12, padding: '4px 10px', width: 240, fontFamily: 'inherit',
  } as React.CSSProperties,
  controlBtn: (active: boolean): React.CSSProperties => ({
    background: active ? '#388bfd26' : 'none',
    border: `1px solid ${active ? '#1f6feb' : '#30363d'}`,
    borderRadius: 4, color: active ? '#58a6ff' : '#8b949e',
    fontSize: 11, cursor: 'pointer', padding: '4px 12px', fontFamily: 'inherit',
  }),
  streamContainer: { display: 'flex', flexDirection: 'column' as const, flex: 1, overflow: 'hidden' },
  streamHeader: {
    display: 'grid',
    gridTemplateColumns: Object.values(COL_WIDTHS).join(' '),
    padding: '6px 20px', background: '#161b22', borderBottom: '1px solid #21262d',
    fontSize: 10, color: '#6e7681', letterSpacing: 1, flexShrink: 0,
  },
  col: (_name: string): React.CSSProperties => ({
    overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
  }),
  eventList: { flex: 1, overflowY: 'auto' as const, fontFamily: "'Courier New', monospace", fontSize: 12 },
  eventRow: {
    display: 'grid',
    gridTemplateColumns: Object.values(COL_WIDTHS).join(' '),
    padding: '4px 20px', borderBottom: '1px solid #21262d',
  } as React.CSSProperties,
  empty: { padding: '40px 20px', color: '#484f58', fontSize: 12, textAlign: 'center' as const, letterSpacing: 1 },

  // Modal
  modalOverlay: {
    position: 'fixed' as const, inset: 0,
    background: 'rgba(0,0,0,0.7)', display: 'flex',
    alignItems: 'center', justifyContent: 'center', zIndex: 1000,
  },
  modal: {
    background: '#161b22', border: '1px solid #30363d', borderRadius: 8,
    width: 640, maxWidth: '90vw', maxHeight: '80vh',
    display: 'flex', flexDirection: 'column' as const, overflow: 'hidden',
  },
  modalHeader: {
    display: 'flex', alignItems: 'center', justifyContent: 'space-between',
    padding: '14px 20px', borderBottom: '1px solid #21262d', flexShrink: 0,
  },
  modalTitle: { display: 'flex', alignItems: 'center', fontFamily: "'Courier New', monospace", fontSize: 13 },
  modalClose: {
    background: 'none', border: 'none', color: '#484f58', cursor: 'pointer',
    fontSize: 14, padding: '2px 6px', fontFamily: 'inherit',
  } as React.CSSProperties,
  modalBody: { overflowY: 'auto' as const, padding: '16px 20px', display: 'flex', flexDirection: 'column' as const, gap: 16 },
  fieldGrid: { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '8px 24px' },
  field: { display: 'flex', flexDirection: 'column' as const, gap: 2 },
  fieldLabel: { fontSize: 10, color: '#484f58', letterSpacing: 1, textTransform: 'uppercase' as const },
  fieldValue: { fontSize: 12, fontFamily: "'Courier New', monospace" },
  payloadSection: { display: 'flex', flexDirection: 'column' as const, gap: 8 },
  payloadLabel: { fontSize: 10, color: '#484f58', letterSpacing: 1 },
  payloadNote: {
    fontSize: 12, color: '#8b949e', lineHeight: 1.6,
    padding: '10px 12px', background: '#0d1117', borderRadius: 4,
    border: '1px solid #21262d',
  } as React.CSSProperties,
  rawJson: {
    margin: 0, padding: '10px 12px', background: '#0d1117', borderRadius: 4,
    border: '1px solid #21262d', fontSize: 11, color: '#8b949e',
    fontFamily: "'Courier New', monospace", overflowX: 'auto' as const, lineHeight: 1.5,
  } as React.CSSProperties,
  runCard: {
    background: '#0d1117', border: '1px solid #21262d', borderRadius: 4,
    overflow: 'hidden', marginBottom: 8,
  } as React.CSSProperties,
  runHeader: {
    display: 'flex', alignItems: 'center', gap: 16,
    padding: '8px 12px', borderBottom: '1px solid #21262d',
    background: '#161b22',
  } as React.CSSProperties,
  testList: {
    display: 'flex', flexDirection: 'column' as const,
  },
  testResult: (passed: boolean, skipped: boolean): React.CSSProperties => ({
    display: 'flex', alignItems: 'center', gap: 10,
    padding: '5px 12px', borderBottom: '1px solid #0d1117',
    background: skipped ? 'transparent' : passed ? 'rgba(63,185,80,0.04)' : 'rgba(248,81,73,0.04)',
    fontFamily: "'Courier New', monospace",
  }),
};
