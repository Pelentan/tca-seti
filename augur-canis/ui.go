package main

// ---------------------------------------------------------------------------
// Augur Canis — Standalone Development UI
//
// Serves a plain HTML observability page on AC_UI_PORT (default disabled).
// No authentication. No build step. Embedded into the binary at compile time.
//
// PURPOSE: Initial setup verification and a quick look at what AC does.
//          Not a replacement for SETI or any production observability tooling.
//
// WARNING: This endpoint has NO authentication. It must not be exposed in
//          any non-isolated environment. Set AC_UI_PORT only during setup
//          and local development. Leave it unset in production.
//
// Usage: set AC_UI_PORT=4099 in your environment or docker-compose.yml.
//        Open http://localhost:4099 in a browser.
//        The mTLS admin API remains on PORT (default 4010) — unchanged.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// uiPort reads AC_UI_PORT from the environment.
// Empty string = UI disabled (default).
func uiPort() string {
	return os.Getenv("AC_UI_PORT")
}

// startDevUI launches the plain HTTP UI server if AC_UI_PORT is set.
// Called from main() — no-op if the env var is unset.
func startDevUI() {
	p := uiPort()
	if p == "" {
		return
	}
	if _, err := strconv.Atoi(p); err != nil {
		log.Printf("[augur-canis] AC_UI_PORT=%q is not a valid port — dev UI disabled", p)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveUI)
	mux.HandleFunc("/api/status", serveUIStatus)

	srv := &http.Server{
		Addr:    ":" + p,
		Handler: mux,
	}

	log.Printf("[augur-canis] ⚠ Dev UI: http://localhost:%s — NO AUTH — setup/dev only", p)
	log.Printf("[augur-canis] ⚠ Port 4666: 'For 666' — remove before production")

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[augur-canis] Dev UI server error: %v", err)
		}
	}()
}

// ---------------------------------------------------------------------------
// API endpoint — aggregates all state the UI needs in one call
// ---------------------------------------------------------------------------

func serveUIStatus(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Jobs
	type jobSummary struct {
		ServiceName     string  `json:"service_name"`
		NetworkEndpoint string  `json:"network_endpoint"`
		RegisteredAt    string  `json:"registered_at"`
		LastSeen        string  `json:"last_seen"`
		LatencyMs       float64 `json:"latency_ms"`
		AlertActive     bool    `json:"alert_active"`
		AlertType       string  `json:"alert_type,omitempty"`
	}

	jobs := []jobSummary{}
	for name, j := range registeredJobs {
		lastSeen, _ := rdb.Get(ctx, fmt.Sprintf(keyLastSeen, name)).Result()
		isAlert, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertActive, name)).Result()
		alertType, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, name)).Result()

		// Get latest latency from stream
		latency := 0.0
		msgs, err := rdb.XRevRangeN(ctx, fmt.Sprintf(keyLatencyStream, name), "+", "-", 1).Result()
		if err == nil && len(msgs) > 0 {
			if v, ok := msgs[0].Values["latency_ms"]; ok {
				switch val := v.(type) {
				case string:
					latency, _ = strconv.ParseFloat(val, 64)
				case float64:
					latency = val
				}
			}
		}

		jobs = append(jobs, jobSummary{
			ServiceName:     j.ServiceName,
			NetworkEndpoint: j.NetworkEndpoint,
			RegisteredAt:    j.RegisteredAt,
			LastSeen:        lastSeen,
			LatencyMs:       latency,
			AlertActive:     isAlert == "1",
			AlertType:       alertType,
		})
	}

	// Active alerts
	type alertSummary struct {
		ServiceName string `json:"service_name"`
		AlertID     string `json:"alert_id"`
		AlertType   string `json:"alert_type"`
		Severity    string `json:"severity"`
		FirstAt     string `json:"first_at"`
		BarkCount   string `json:"bark_count"`
	}

	alerts := []alertSummary{}
	alertKeys, _ := rdb.Keys(ctx, "ac:state:*:alert_active").Result()
	for _, key := range alertKeys {
		v, _ := rdb.Get(ctx, key).Result()
		if v != "1" {
			continue
		}
		// Extract service name: ac:state:{service}:alert_active
		svc := strings.TrimPrefix(key, "ac:state:")
		svc = strings.TrimSuffix(svc, ":alert_active")

		alertID, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertID, svc)).Result()
		alertType, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertType, svc)).Result()
		firstAt, _ := rdb.Get(ctx, fmt.Sprintf(keyAlertFirstAt, svc)).Result()
		barkCount, _ := rdb.Get(ctx, fmt.Sprintf(keyBarkCount, svc)).Result()

		severity := "warn"
		if alertType == string(AlertSilence) {
			severity = "critical"
		}

		alerts = append(alerts, alertSummary{
			ServiceName: svc,
			AlertID:     alertID,
			AlertType:   alertType,
			Severity:    severity,
			FirstAt:     firstAt,
			BarkCount:   barkCount,
		})
	}

	// Recent contract suite
	type suiteSummary struct {
		RunID      string `json:"run_id"`
		Trigger    string `json:"trigger"`
		Passed     int    `json:"passed"`
		Failed     int    `json:"failed"`
		Total      int    `json:"total"`
		DurationMs int64  `json:"duration_ms"`
		StartedAt  string `json:"started_at"`
	}

	var recentSuite *suiteSummary
	suiteKeys, _ := rdb.Keys(ctx, "ac:contract-suite:*").Result()
	if len(suiteKeys) > 0 {
		latest := ""
		for _, k := range suiteKeys {
			if latest == "" || k > latest {
				latest = k
			}
		}
		raw, err := rdb.Get(ctx, latest).Result()
		if err == nil {
			var suite ContractSuiteResult
			if jsonErr := json.Unmarshal([]byte(raw), &suite); jsonErr == nil {
				recentSuite = &suiteSummary{
					RunID:      suite.RunID,
					Trigger:    suite.Trigger,
					Passed:     suite.Passed,
					Failed:     suite.Failed,
					Total:      suite.Total,
					DurationMs: suite.DurationMs,
					StartedAt:  suite.StartedAt,
				}
			}
		}
	}

	// Redis health
	redisOK := rdb.Ping(ctx).Err() == nil

	json.NewEncoder(w).Encode(map[string]interface{}{
		"constellation":    constellation,
		"uptime_seconds":   int(time.Since(startTime).Seconds()),
		"redis_connected":  redisOK,
		"jobs_registered":  len(registeredJobs),
		"active_alerts":    len(alerts),
		"checks_completed": checksHandled.Load(),
		"alerts_fired":     alertsFired.Load(),
		"jobs":             jobs,
		"alerts":           alerts,
		"recent_suite":     recentSuite,
		"detectors": map[string]string{
			"silence":       "active",
			"latency_drift": "active",
			"failure_rate":  "active",
		},
		"fetched_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// UI HTML — embedded, no build step, no dependencies
// ---------------------------------------------------------------------------

func serveUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(acUIHTML))
}

const acUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Augur Canis — Dev UI</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  :root {
    --bg: #0d1117; --surface: #161b22; --border: #21262d;
    --text: #e6edf3; --muted: #6e7681; --dim: #484f58;
    --green: #3fb950; --red: #f85149; --amber: #e3b341; --blue: #58a6ff;
    --font-mono: 'Courier New', monospace;
  }
  body { background: var(--bg); color: var(--text); font-family: system-ui, sans-serif;
         font-size: 13px; line-height: 1.5; }

  /* Warning banner */
  .warn-banner {
    background: #2d1b00; border-bottom: 2px solid var(--amber);
    padding: 10px 24px; display: flex; align-items: center; gap: 12px;
  }
  .warn-icon { font-size: 18px; }
  .warn-text { color: var(--amber); font-size: 12px; }
  .warn-text strong { display: block; font-size: 13px; letter-spacing: 0.5px; }

  /* Header */
  header { background: var(--surface); border-bottom: 1px solid var(--border);
           padding: 12px 24px; display: flex; align-items: center; gap: 16px; }
  .logo { font-size: 15px; font-weight: 700; color: var(--blue);
          letter-spacing: 2px; font-family: var(--font-mono); }
  .badge { font-size: 9px; color: var(--amber); border: 1px solid var(--amber);
           border-radius: 3px; padding: 2px 6px; letter-spacing: 1px; }
  .header-right { margin-left: auto; display: flex; align-items: center; gap: 16px; }
  .stat-pill { font-size: 11px; color: var(--muted); font-family: var(--font-mono); }
  .stat-pill span { color: var(--text); font-weight: 600; }
  #status-dot { width: 8px; height: 8px; border-radius: 50%;
                background: var(--green); display: inline-block; }
  #refresh-btn { background: none; border: 1px solid var(--border); border-radius: 4px;
                 color: var(--muted); font-size: 11px; cursor: pointer; padding: 4px 10px;
                 font-family: inherit; }
  #refresh-btn:hover { color: var(--text); border-color: var(--muted); }
  #last-fetch { font-size: 10px; color: var(--dim); font-family: var(--font-mono); }

  /* Layout */
  main { padding: 20px 24px; display: grid;
         grid-template-columns: 1fr 1fr; gap: 16px; }
  @media (max-width: 900px) { main { grid-template-columns: 1fr; } }
  .full-width { grid-column: 1 / -1; }

  /* Cards */
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 8px; overflow: hidden; }
  .card-header { padding: 10px 16px; border-bottom: 1px solid var(--border);
                 display: flex; align-items: center; gap: 10px; }
  .card-title { font-size: 10px; color: var(--dim); letter-spacing: 1.5px; }
  .card-count { font-size: 11px; color: var(--muted); margin-left: auto; }
  .card-body { padding: 12px 16px; }

  /* Jobs table */
  .jobs-table { width: 100%; border-collapse: collapse; }
  .jobs-table th { font-size: 9px; color: var(--dim); letter-spacing: 1px; text-align: left;
                   padding: 4px 8px; border-bottom: 1px solid var(--border); font-weight: normal; }
  .jobs-table td { padding: 7px 8px; border-bottom: 1px solid #0d1117; font-family: var(--font-mono); font-size: 11px; }
  .jobs-table tr:last-child td { border-bottom: none; }
  .jobs-table tr:hover td { background: #0d1117; }
  .status-dot { width: 7px; height: 7px; border-radius: 50%; display: inline-block; margin-right: 6px; }
  .dot-ok  { background: var(--green); }
  .dot-err { background: var(--red); }
  .dot-warn { background: var(--amber); }
  .latency { color: var(--muted); }
  .last-seen { color: var(--dim); font-size: 10px; }

  /* Alerts */
  .alert-row { padding: 10px 0; border-bottom: 1px solid var(--border); }
  .alert-row:last-child { border-bottom: none; }
  .alert-service { font-weight: 600; color: var(--red); font-family: var(--font-mono); }
  .alert-type { font-size: 10px; color: var(--muted); letter-spacing: 1px; margin-top: 2px; }
  .alert-since { font-size: 10px; color: var(--dim); margin-top: 2px; font-family: var(--font-mono); }
  .no-alerts { color: var(--green); font-size: 12px; text-align: center; padding: 20px 0; }

  /* Suite */
  .suite-row { display: flex; align-items: center; gap: 10px; padding: 6px 0; }
  .suite-label { font-size: 10px; color: var(--dim); letter-spacing: 1px; min-width: 80px; }
  .suite-val { font-family: var(--font-mono); font-size: 12px; }
  .pass { color: var(--green); } .fail { color: var(--red); }
  .suite-bar { flex: 1; height: 6px; background: var(--border); border-radius: 3px; overflow: hidden; }
  .suite-bar-fill { height: 100%; border-radius: 3px; background: var(--green); transition: width 0.3s; }

  /* Detectors */
  .detector-row { display: flex; align-items: center; gap: 8px; padding: 6px 0;
                  border-bottom: 1px solid var(--border); }
  .detector-row:last-child { border-bottom: none; }
  .detector-name { font-family: var(--font-mono); font-size: 11px; flex: 1; }
  .detector-state { font-size: 10px; padding: 2px 8px; border-radius: 10px; letter-spacing: 0.5px; }
  .state-active { background: #1a3a1a; color: var(--green); }
  .state-stub   { background: #2d2510; color: var(--amber); }

  /* Summary pills */
  .summary-row { display: flex; gap: 12px; flex-wrap: wrap; }
  .summary-pill { background: var(--bg); border: 1px solid var(--border); border-radius: 6px;
                  padding: 10px 16px; flex: 1; min-width: 100px; }
  .pill-label { font-size: 9px; color: var(--dim); letter-spacing: 1.5px; }
  .pill-value { font-size: 22px; font-weight: 700; font-family: var(--font-mono); margin-top: 2px; }

  .empty { color: var(--dim); font-style: italic; text-align: center; padding: 20px; font-size: 12px; }
  .err-banner { color: var(--red); font-size: 11px; padding: 12px; text-align: center; }
</style>
</head>
<body>

<!-- ⚠ WARNING BANNER -->
<div class="warn-banner">
  <span class="warn-icon">⚠</span>
  <div class="warn-text">
    <strong>DEVELOPMENT VIEW — NOT FOR PRODUCTION</strong>
    This page is served without authentication. Intended for initial setup and local verification only.
    Remove AC_UI_PORT or block this port before deploying to any non-isolated environment.
  </div>
</div>

<!-- Header -->
<header>
  <span id="status-dot"></span>
  <span class="logo">AUGUR CANIS</span>
  <span class="badge">DEV UI</span>
  <div class="header-right">
    <span class="stat-pill">jobs: <span id="h-jobs">—</span></span>
    <span class="stat-pill">alerts: <span id="h-alerts">—</span></span>
    <span class="stat-pill">checks: <span id="h-checks">—</span></span>
    <span class="stat-pill">uptime: <span id="h-uptime">—</span></span>
    <button id="refresh-btn" onclick="load()">↺ Refresh</button>
    <span id="last-fetch"></span>
  </div>
</header>

<main>
  <!-- Summary pills -->
  <div class="full-width">
    <div class="summary-row">
      <div class="summary-pill">
        <div class="pill-label">JOBS REGISTERED</div>
        <div class="pill-value" id="p-jobs" style="color:var(--blue)">—</div>
      </div>
      <div class="summary-pill">
        <div class="pill-label">ACTIVE ALERTS</div>
        <div class="pill-value" id="p-alerts" style="color:var(--red)">—</div>
      </div>
      <div class="summary-pill">
        <div class="pill-label">CHECKS COMPLETED</div>
        <div class="pill-value" id="p-checks" style="color:var(--muted)">—</div>
      </div>
      <div class="summary-pill">
        <div class="pill-label">ALERTS FIRED</div>
        <div class="pill-value" id="p-fired" style="color:var(--amber)">—</div>
      </div>
      <div class="summary-pill">
        <div class="pill-label">CONSTELLATION</div>
        <div class="pill-value" id="p-const" style="color:var(--muted);font-size:14px">—</div>
      </div>
    </div>
  </div>

  <!-- Jobs -->
  <div class="card full-width">
    <div class="card-header">
      <span class="card-title">REGISTERED JOBS</span>
      <span class="card-count" id="jobs-count"></span>
    </div>
    <div class="card-body" style="padding:0">
      <table class="jobs-table">
        <thead>
          <tr>
            <th>SERVICE</th>
            <th>ENDPOINT</th>
            <th>LATENCY</th>
            <th>LAST SEEN</th>
            <th>REGISTERED</th>
          </tr>
        </thead>
        <tbody id="jobs-body">
          <tr><td colspan="5" class="empty">Loading...</td></tr>
        </tbody>
      </table>
    </div>
  </div>

  <!-- Active Alerts -->
  <div class="card">
    <div class="card-header">
      <span class="card-title">ACTIVE ALERTS</span>
    </div>
    <div class="card-body" id="alerts-body">
      <div class="empty">Loading...</div>
    </div>
  </div>

  <!-- Right column: suite + detectors -->
  <div style="display:flex;flex-direction:column;gap:16px">

    <!-- Contract Suite -->
    <div class="card">
      <div class="card-header">
        <span class="card-title">LAST CONTRACT SUITE RUN</span>
      </div>
      <div class="card-body" id="suite-body">
        <div class="empty">No runs yet — fires 30s after startup</div>
      </div>
    </div>

    <!-- Detectors -->
    <div class="card">
      <div class="card-header">
        <span class="card-title">DETECTORS</span>
      </div>
      <div class="card-body" id="detectors-body">
        <div class="empty">Loading...</div>
      </div>
    </div>

  </div>
</main>

<script>
function fmtTime(iso) {
  if (!iso) return '—';
  try {
    return new Date(iso).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit',second:'2-digit'});
  } catch { return iso; }
}

function fmtUptime(s) {
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s/60) + 'm ' + (s%60) + 's';
  return Math.floor(s/3600) + 'h ' + Math.floor((s%3600)/60) + 'm';
}

function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

async function load() {
  try {
    const r = await fetch('/api/status');
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const d = await r.json();
    render(d);
    document.getElementById('last-fetch').textContent = fmtTime(d.fetched_at);
    document.getElementById('status-dot').style.background = d.redis_connected ? 'var(--green)' : 'var(--red)';
  } catch(e) {
    document.getElementById('status-dot').style.background = 'var(--red)';
    document.getElementById('jobs-body').innerHTML = '<tr><td colspan="5" class="err-banner">'+esc(e.message)+'</td></tr>';
  }
}

function render(d) {
  // Header pills
  document.getElementById('h-jobs').textContent = d.jobs_registered;
  document.getElementById('h-alerts').textContent = d.active_alerts;
  document.getElementById('h-checks').textContent = d.checks_completed;
  document.getElementById('h-uptime').textContent = fmtUptime(d.uptime_seconds);
  document.getElementById('p-jobs').textContent = d.jobs_registered;
  document.getElementById('p-alerts').textContent = d.active_alerts;
  document.getElementById('p-checks').textContent = d.checks_completed;
  document.getElementById('p-fired').textContent = d.alerts_fired;
  document.getElementById('p-const').textContent = d.constellation || '—';

  // Jobs table
  const jobs = (d.jobs || []).sort((a,b) => a.service_name.localeCompare(b.service_name));
  document.getElementById('jobs-count').textContent = jobs.length + ' services';
  if (jobs.length === 0) {
    document.getElementById('jobs-body').innerHTML = '<tr><td colspan="5" class="empty">No jobs registered yet — waiting for services to self-register</td></tr>';
  } else {
    document.getElementById('jobs-body').innerHTML = jobs.map(j => {
      const dotClass = j.alert_active ? 'dot-err' : 'dot-ok';
      const lat = j.latency_ms > 0 ? Math.round(j.latency_ms) + 'ms' : '—';
      return '<tr>' +
        '<td><span class="status-dot '+dotClass+'"></span>'+esc(j.service_name)+'</td>' +
        '<td class="latency">'+esc(j.network_endpoint)+'</td>' +
        '<td class="latency">'+lat+'</td>' +
        '<td class="last-seen">'+fmtTime(j.last_seen)+'</td>' +
        '<td class="last-seen">'+fmtTime(j.registered_at)+'</td>' +
        '</tr>';
    }).join('');
  }

  // Alerts
  const alerts = d.alerts || [];
  if (alerts.length === 0) {
    document.getElementById('alerts-body').innerHTML = '<div class="no-alerts">✓ No active alerts</div>';
  } else {
    document.getElementById('alerts-body').innerHTML = alerts.map(a => {
      return '<div class="alert-row">' +
        '<div class="alert-service">'+esc(a.service_name)+'</div>' +
        '<div class="alert-type">'+esc(a.alert_type)+' · bark #'+esc(a.bark_count)+'</div>' +
        '<div class="alert-since">since '+fmtTime(a.first_at)+'</div>' +
        '</div>';
    }).join('');
  }

  // Contract suite
  const suite = d.recent_suite;
  if (!suite) {
    document.getElementById('suite-body').innerHTML = '<div class="empty">No runs yet — fires 30s after startup</div>';
  } else {
    const pct = suite.total > 0 ? (suite.passed / suite.total * 100) : 0;
    const allPass = suite.failed === 0;
    document.getElementById('suite-body').innerHTML =
      '<div class="suite-row">' +
        '<span class="suite-label">RESULT</span>' +
        '<span class="suite-val '+(allPass?'pass':'fail')+'">'+(allPass?'✓ ALL PASSED':'✗ '+suite.failed+' FAILED')+'</span>' +
      '</div>' +
      '<div class="suite-row">' +
        '<span class="suite-label">SCORE</span>' +
        '<div class="suite-bar"><div class="suite-bar-fill" style="width:'+pct+'%;background:'+(allPass?'var(--green)':'var(--red)')+'"></div></div>' +
        '<span class="suite-val" style="min-width:60px;text-align:right">'+suite.passed+'/'+suite.total+'</span>' +
      '</div>' +
      '<div class="suite-row"><span class="suite-label">TRIGGER</span><span class="suite-val">'+esc(suite.trigger)+'</span></div>' +
      '<div class="suite-row"><span class="suite-label">DURATION</span><span class="suite-val">'+suite.duration_ms+'ms</span></div>' +
      '<div class="suite-row"><span class="suite-label">RAN AT</span><span class="suite-val last-seen">'+fmtTime(suite.started_at)+'</span></div>';
  }

  // Detectors
  const det = d.detectors || {};
  document.getElementById('detectors-body').innerHTML = Object.entries(det).map(([name, state]) =>
    '<div class="detector-row">' +
    '<span class="detector-name">'+esc(name).replace(/_/g,' ')+'</span>' +
    '<span class="detector-state '+(state==='active'?'state-active':'state-stub')+'">'+esc(state)+'</span>' +
    '</div>'
  ).join('');
}

// Initial load + 30s auto-refresh
load();
setInterval(load, 30000);
</script>
</body>
</html>`
