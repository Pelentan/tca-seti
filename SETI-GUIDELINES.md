# S.E.T.I. — Search for Erroneous Tessellated Interactions
## Guidelines and Reference

**Version:** 1.0  
**Last Updated:** 2026-04-13  
**Audience:** Engineers deploying SETI, engineers integrating monitored constellations, Wr4nglers operating SETI  
**Repository:** github.com/Pelentan/tca-seti  
**Related:** [Augur Canis Guidelines](AC-GUIDELINES.md) · [TCA Guidelines](tca-guidelines.md)

---

## 1.  What SETI Is

S.E.T.I. — Search for Erroneous Tessellated Interactions — is a monitoring constellation for TCA applications.  It watches other constellations, tests whether their Jobs behave as their contracts specify, maintains institutional memory of what it has seen, routes failures through an AI triage layer, and alerts humans when something needs attention.

SETI is itself a TCA constellation.  It has 20 Jobs, speaks mTLS throughout, uses the same contract-first discipline it enforces on others, and runs in Docker Compose for development and Kubernetes for production.

SETI requires Augur Canis.  AC is the behavioral verification layer — it executes contract tests, detects anomalies, fires alerts, and maintains the metric streams that feed the LaE visualization.  SETI provides the full operational shell around AC:  the intelligence layer, the Wr4ngler interface, the institutional memory, and the human notification pathway.

---

## 2.  The Constellation Map

20 containers, all healthy.  Three infrastructure services, 17 application Jobs.

### Infrastructure

| Service | Role | Port |
|---------|------|------|
| cert-forge | PKI abstraction — issues instance certs over mTLS | 4014 (enrollment), 4015 (constellation), 4016 (CA/public) |
| redis | Pub/sub backbone, metric streams, alert state | 6379 |
| postgres | Lore institutional memory (persistent) | 5432 |

### SETI Jobs

| Job | Language | Port | Role |
|-----|----------|------|------|
| gateway | Go | 4000 | External face — auth, routing, SSE fan-out |
| signal-clearance | TypeScript | 4001 | Authentication and clearance level management |
| policy | Go | 4002 | Application registry and test schedule |
| contract-test | Go | 4003 | Contract test dispatch and scheduling |
| plot-test | Go | 4004 | Plot execution with call chain verification |
| plot-store | Go | 4005 | Plot definition storage and versioning |
| signal-aggregator | Go | 4006 | Event stream subscription and call chain verification |
| feed-wrangler | Elixir | 4007 | SSE feed lifecycle (OTP-supervised) |
| results | Python | 4008 | Test run result storage and retrieval |
| interactions | Go | 4009 | Failure triage routing |
| augur-canis | Go | 4010 | Behavioral verification, health monitoring, contract test execution |
| seti-observability | Go | 4011 | Inter-service event ingestion and pub/sub |
| integration | Go | 4013 | Versioned external API for monitored application tooling |
| ai-lien | Python | 4252 | AI diagnostic engine (double-tap pattern, Lore-aware) |
| lore | Go | 4110 | Institutional memory (PostgreSQL-backed) |
| notifier | Go | 4300 | Human notification (permanent stub — implement for your environment) |
| ui | TypeScript/React | 4020 (served via gateway) | Wr4ngler interface |

---

## 3.  The Two Networks

SETI uses two isolated Docker networks.

**`seti-internal`** — the primary service network.  All Jobs communicate here.  All traffic is mTLS.  No direct access from outside.

**`ac-net`** — the Augur Canis network.  AC uses this network to reach every registered Job for health checks and contract tests.  Services join this network in addition to `seti-internal`.

The gateway is the only service with an exposed port to the host (`0.0.0.0:4000`).  Everything else is unreachable from outside the Docker network except through the gateway.

---

## 4.  Clearance Levels

SETI uses clearance levels rather than roles.  The distinction matters:  authorization in an observability system is fundamentally different from feature authorization.  A Wr4ngler's clearance level determines what they can see and do, not which features they have access to.

| Level | Name | Capabilities |
|-------|------|--------------|
| 1 | `connie-wr4ngler` | View dashboard, provision feeds, watch event stream |
| 2 | `sec-wr4ngler` | All above + Ring (Trial, Reports, LaE), alert acknowledgment |
| 3 | `admin` | All above + Admin page, application registration, AI provider configuration |

Clearance levels are validated by signal-clearance on every authenticated request.  The gateway injects `X-Wrangler-Clearance` into every upstream request after JWT validation.  Individual Jobs trust this header — they do not re-validate the JWT.

---

## 5.  The Observability Layer

Every Job in the SETI constellation (and every monitored application) wraps its outbound HTTP calls with a thin, non-blocking reporter.  After each call completes, the reporter fires a `POST /event` to the SETI observability Job — who called whom, method, path, status, latency, protocol.  The reporter does not wait for a response.

```go
func reportEvent(callee, method, path string, status int, latencyMs int64) {
    go func() {
        body, _ := json.Marshal(map[string]interface{}{
            "caller":      "service-name",  // hardcoded to this service
            "callee":      callee,
            "method":      method,
            "path":        path,
            "status_code": status,
            "latency_ms":  latencyMs,
            "protocol":    "mtls",
        })
        req, _ := http.NewRequest("POST", observabilityURL+"/event",
            bytes.NewReader(body))
        req.Header.Set("Content-Type", "application/json")
        resp, err := upstreamClient.Do(req)
        if err != nil { return }
        resp.Body.Close()
    }()
}
```

The pattern is identical across all languages:  `threading.Thread` in Python, `Task.start` in Elixir, `fetch()` without `await` in TypeScript.  Launch and forget.

The observability Job validates incoming events against a known-services allowlist, sanitizes sensitive data from paths (JWTs, IPs, UUIDs, query string values), assigns a UUID and timestamp, and publishes to the `tca:events` Redis pub/sub channel.  The gateway subscribes and fans events to connected SSE clients.

**Fire-and-forget is architectural, not optional.**  If the observability Job is on the critical path of any request, it becomes a dependency of the system it monitors.  A monitoring system that can take down the application is a liability.  The 202 response is a courtesy acknowledgment.  Callers do not wait for it.

**Allowlist drops are silent.**  Events from unknown callers or callees are dropped with a 202 response.  Noisy rejection responses give an attacker feedback for probing the allowlist.  Silent drops give them nothing.

**Sanitization is centralized.**  Path sanitization happens once, in the observability Job, in one language.  Individual Jobs do not sanitize before reporting.  A single Job that forgets to strip a token from a path means credentials flow to the dashboard — centralizing sanitization prevents that class of error structurally.

**The observability Job does not report its own calls.**  Doing so would create a reporting loop.  Its contract omits the `x-tca-observability` block deliberately.

---

## 6.  Contract Tests

### What they verify

SETI's contract test suite lives in Augur Canis (`augur-canis/contracttests.go`).  It is a hardcoded, independently authored set of behavioral assertions — one positive test per significant endpoint, one negative test per Job with POST endpoints.

The positive tests verify that endpoints return expected 2xx responses with expected response fields present.

The negative tests verify that POST endpoints with missing or malformed input return 4xx, not 5xx.  A 5xx on bad input means the Job is swallowing errors.  This is a real bug.  Negative tests are the tests most likely to surface it.

### Authorship discipline

The tests are written by hand, separately from the contracts they verify.  This is the "say it three times" discipline:  the contract says what a Job should do, the implementation does it, and the contract test verifies it independently.  Generated tests re-state the contract.  They cannot independently verify it.

If a contract changes, the tests must be updated manually.  That friction is the point — it forces a human to review the behavioral impact of every contract change.

### Scheduling

Tests fire at startup (30-second delay), every 15 minutes on schedule, and on demand via the "Run Contract Tests" button on the Dashboard.  Results are stored in Redis with a 24-hour TTL and visible in Ring → Reports → Contract Tests.

### Adding tests for a monitored application

The current suite tests SETI's own Jobs.  Adding tests for a monitored TCA application means adding entries to `setiContractTests` in `contracttests.go` and rebuilding AC.  No runtime configuration.

---

## 7.  Plot Tests

Plots are multi-step behavioral tests that verify real user flows across service boundaries.  Where contract tests verify a single endpoint in isolation, Plots verify that a sequence of calls produces the expected sequence of behavior — including the internal call chains that a contract test cannot observe.

### Structure

A Plot is a JSON file in `contracts/plots/{application}/` with:
- A series of steps, each specifying an HTTP call and expected response
- Optional capture definitions to extract values from one step's response for use in a later step's path or body
- Optional call chain assertions — which internal calls should have been observed in the signal aggregator event stream after this step

```json
{
  "plot_id": "signal-clearance-clearance-validation-flow",
  "name": "Signal Clearance Validation Flow",
  "application_id": "seti",
  "steps": [
    {
      "step_number": 1,
      "description": "Gateway health check confirms SETI is up before exercising auth",
      "call": { "method": "GET", "path": "/health" },
      "expect_status": 200
    },
    {
      "step_number": 2,
      "description": "Dev login issues a sec-wr4ngler JWT",
      "call": {
        "method": "POST",
        "path": "/auth/dev/login",
        "body": { "display_name": "sec-wr4ngler", "clearance_level": "sec-wr4ngler" }
      },
      "expect_status": 200,
      "capture": [{ "name": "jwt", "path": "jwt" }]
    }
  ]
}
```

### Retry on transient failure

Plot-test retries failed steps up to 3 times with a 10-second interval before marking them failed.  Retry is triggered only on network-level failures and 5xx responses — the signatures of a pod mid-recycle in a containerized environment.  4xx responses, assertion failures, and chain failures are not retried.  Those are real failures.

The retry count is recorded in the step result and written to the bad-whiff buffer.  A step that consistently passes on attempt 2 or 3 across multiple runs is a pattern worth investigating even though the plot passes overall.

### Bad-whiff buffer

Every step result — pass or fail — is written to `seti:whiff:{application_id}` as a Redis Stream entry.  This is tier-two storage between run-scoped Redis (immediate) and PostgreSQL Lore (persistent).  AI-lien reads this stream for baseline context before analysis.  The buffer is capped at 10,000 entries per application.

### Plot authorship

Plots are authored by engineers who understand the application.  AI can scaffold a Plot from a contract and running system, but the Show Wr4ngler role — adversarial testing from the user perspective — cannot be automated.  Generated tests can only verify the happy path.  A Show Wr4ngler finds the unhappy paths.

---

## 8.  The Intelligence Layer

### Interactions — Triage Router

When a test run fails, plot-test or contract-test POSTs to Interactions `/escalate` with the failure counts.  Interactions makes a triage decision across three paths:

**Path 1 — Immediate (≥ 50% failure rate):**  The constellation is in crisis.  Interactions fires the Notifier and an AI-lien critical briefing in parallel.  The Notifier wakes a human.  AI-lien assembles a context package — not a diagnosis, a briefing for the human already on their way.

**Path 2 — AI Analysis (partial failure):**  Interactions packages the full run context — failed test details from Results, recent Lore incidents, known patterns for the application — and sends it to AI-lien for structured assessment.  If AI-lien's assessment recommends human involvement, Interactions calls the Notifier.  The assessment is written to Lore regardless.

**Path 3 — Silence (single intermittent failure, clean 7-day Lore history):**  The failure is logged to the bad-whiff buffer by Signal Aggregator.  No AI call, no notification.  Let the pattern emerge.

The 50% threshold and 7-day silence window are hardcoded constants in `interactions/main.go`.  Changing them requires a code review and rebuild.  This friction is intentional — threshold changes are architectural decisions with operational consequences.

**AI-lien never calls the Notifier directly.**  All Notifier calls go through Interactions.  Every human notification is traceable to an Interactions decision with a log entry.  AI-lien reasons and returns.  Acting is Interactions' job.

### AI-lien — Diagnostic Engine

AI-lien implements the double-tap pattern:
1. **First tap:**  Send the failure context plus a meta-prompt to the AI model.  The model constructs the optimal diagnostic prompt for the situation.
2. **Second tap:**  Execute that prompt.  The model returns a structured assessment in a schema defined by the caller.

The first-tap prompt is returned in every response — the exact question asked is always visible in the audit trail.

AI-lien reads Lore before every analysis.  It fetches the auto-baseline for the failing Job, recent open incidents for the application, and known failure patterns.  An AI-lien with institutional memory is a different instrument than an AI-lien without it.

After analysis, AI-lien writes back to Lore — a trend point for every analysis, and an incident if the assessment severity is high or critical.

AI-lien is model-agnostic.  It asks Policy for the active provider and model on every request — no configuration baked in.  Change the provider in the Admin UI and the next analysis uses it immediately.

Two analysis modes:
- `failure_analysis` — normal path.  Diagnose the failure.  Recommend action.  Assess severity.
- `critical_briefing` — Path 1.  A human is on their way.  Assemble everything they need to understand the situation immediately.  Not a diagnosis — a briefing.

### Lore — Institutional Memory

Lore is a PostgreSQL-backed Go Job that stores four data types:

- **Trend points** — individual observations from AI-lien analyses and the bad-whiff buffer
- **Baselines** — what healthy looks like for each Job, established by AI-lien after sufficient observations
- **Incidents** — identified failure events with AI assessment, recommended action, and open/resolved status
- **Patterns** — recurring failure signatures recognized across multiple incidents

Lore makes the next analysis better than the last.  A fresh SETI installation analyzing its first failure has only the immediate context.  After a month of operation, it has baselines, incident history, and recognized patterns — and AI-lien reads all of it before forming its assessment.

Lore corrections (`POST /corrections`) are stubbed at 501.  The feedback path for a Wr4ngler to push back on an AI assessment is designed but not yet implemented.

### Notifier — Human Alert (Permanent Stub)

The Notifier is a thin, intentionally opaque pager.  It receives a prompt and fires a notification through whatever mechanism the operator has configured for their environment.

The prompt is deliberately minimal (500 character maximum).  Sensitive incident context — service topology, Lore history, full AI briefing — travels through SETI's secure internal channels, not through the notification payload.  The human follows the alert into SETI for the full picture.

**The Notifier is a permanent stub in this repository.**  If you cloned this codebase, you must implement the handler body in `notifier/main.go` before SETI can page humans.  The contract (`contracts/openapi/notifier.yaml`) is what you implement against.  The implementation is yours.

Common implementations:  PagerDuty Events API v2, Twilio SMS, Microsoft Teams webhook, AWS SNS, Slack webhook.  The stub includes commented examples for each.

`stub_active: true` in the health response is detectable signal.  Any production deployment with `stub_active: true` means no humans will be paged under any circumstances.

---

## 9.  The Wr4ngler Interface

### Dashboard

The Dashboard is the primary operational view.  It shows the live inter-service event stream, the contract test result feed, the current constellation (tab navigation for multiple monitored applications), and the Run Contract Tests button.

The event stream is Server-Sent Events from the gateway's `/feeds/{feed_id}/stream` endpoint.  The gateway subscribes to the `tca:events` Redis pub/sub channel and fans events to connected Wr4ngler feeds.  Each Wr4ngler has their own feed — provisioned at login, scoped to their clearance level.

Events are filterable by service name or path.  Hide Checks suppresses the AC health check traffic.  Pause stops the live stream.  Clear clears the visible history.

### Ring

Ring is the Sec Wr4ngler tool.  Three tabs:

**Trial** — Paste an OpenAPI contract YAML, specify a target Job, and AC parses the endpoints and runs them over mTLS.  Results return immediately.  Trial runs are recorded in Reports.  Use this to verify a new contract before it goes into the hardcoded suite, or to investigate a suspected regression without touching code.

**Reports** — Historical contract and plot test results in two columns.  Contract Tests shows all stored AC suite runs with a dropdown selector and full per-test detail on selection.  Plot Tests shows all plot runs for the active constellation.  Both columns have a Download Report button that generates a timestamped plain-text report.

**LaE** (Lag and Errors) — Time-series visualization of latency and error rate per Job.  Select a constellation, select a window (20 min / 1 hr / 6 hr / 24 hr), hit Rope.  AC's metric streams feed the dual-axis charts — latency in milliseconds on the left axis (green), error rate as a percentage on the right axis (red).  The auto-baseline shows as a blue dashed reference line.  Release stops the live update cycle.

### Admin

The Admin page is for admin-clearance Wr4nglers.  It manages:
- Registered constellations (application registry in Policy)
- AI provider configuration (which Ollama instance, which model)
- Wr4ngler account management

---

## 10.  Monitored Application Integration

### What the monitored application must provide

1. A running constellation with Jobs that respond to `POST /check`
2. OpenAPI 3.1 contracts accessible to SETI's contract-test Job
3. A gateway URL registered in SETI's Policy Job

### Registration

Register a monitored application via the SETI Admin UI or directly:

```
POST /applications/register
{
  "display_name": "TCA Vox",
  "gateway_url": "https://tca-vox-gateway:4000",
  "redis_url": "tca-vox-redis:6379",
  "contracts_path": "/contracts/openapi",
  "description": "Voting application built on TCA"
}
```

Once registered, SETI subscribes to the application's event stream via Signal Aggregator, schedules contract tests, and begins monitoring.

### Contracts

SETI reads OpenAPI 3.1 YAML contracts from the monitored application's `contracts/openapi/` directory.  Contracts must be accessible from the container running the contract-test Job — mount them as a volume or fetch them from a Git URL depending on your deployment model.

### What SETI does automatically after registration

- Signal Aggregator subscribes to the application's Redis event channel
- Contract tests are added to the schedule at the configured interval
- Plot tests run daily at 02:00 UTC for any registered Plots
- AC begins health checking all registered endpoints
- Failures escalate through Interactions → AI-lien → Lore → Notifier (Path 1 or 2)

---

## 11.  Constellation Navigation

SETI can monitor multiple constellations simultaneously.  The ConstellationNav component at the top of the Dashboard and Plots pages provides tab navigation across all registered applications, with SETI itself pinned as the first tab.

Up to 7 constellations display as tabs.  Additional constellations appear in an overflow dropdown.  The active constellation is stored in the URL as a `?constellation=` query parameter — shareable links always point to the correct constellation view.

Contract test results, plot results, and event stream feeds are all constellation-scoped.

---

## 12.  Deployment

### Development (Docker Compose)

```bash
git clone https://github.com/Pelentan/tca-seti
cd tca-seti
cp .env.example .env        # set POSTGRES_PASSWORD, JWT_SECRET
docker compose build
docker compose up -d
```

Open `https://localhost:4000`.  Accept the self-signed certificate.  Log in with any display name in `AUTH_MODE=dev`.

The dev login returns a JWT with the clearance level matching the `clearance_level` field in the login request.  Use `sec-wr4ngler` to access Ring.

### Required environment variables

| Variable | Description |
|----------|-------------|
| `POSTGRES_PASSWORD` | PostgreSQL password for Lore |
| `JWT_SECRET` | Signing secret for JWTs issued by signal-clearance |
| `AUTH_MODE` | `dev` for stub auth, `oidc` for production OIDC |

### Production (Kubernetes)

In production, cert-forge's signing backend points to cert-manager or the organizational CA.  `AUTH_MODE=oidc` enables OIDC authentication through the configured provider.  The Notifier must be implemented for your notification environment.

The three services that persist state require proper storage:
- **Lore** — PostgreSQL with a persistent volume
- **Redis** — with AOF persistence enabled
- **cert-forge** — stateless, but must restart cleanly if it crashes

All other Jobs are stateless and restart cleanly.

### Removing the dev UI

`AC_UI_PORT=4666` and its `ports:` mapping are present in `docker-compose.yml` for development convenience.  Remove both before any non-isolated deployment.  The warning banner on port 4666 exists precisely so this is impossible to miss during development.

---

## 13.  Port Reference

| Port | Service | External |
|------|---------|----------|
| 4000 | gateway | Yes — sole external entry point |
| 4001 | signal-clearance | No |
| 4002 | policy | No |
| 4003 | contract-test | No |
| 4004 | plot-test | No |
| 4005 | plot-store | No |
| 4006 | signal-aggregator | No |
| 4007 | feed-wrangler | No |
| 4008 | results | No |
| 4009 | interactions | No |
| 4010 | augur-canis (mTLS admin) | No |
| 4011 | seti-observability | No |
| 4013 | integration | No |
| 4014 | cert-forge (enrollment mTLS) | No |
| 4015 | cert-forge (constellation mTLS) | No |
| 4016 | cert-forge (plain HTTP — CA cert) | No |
| 4020 | ui (static server) | No (served via gateway) |
| 4110 | lore | No |
| 4252 | ai-lien | No |
| 4300 | notifier | No |
| 4666 | augur-canis dev UI (plain HTTP) | Dev only — remove before production |
| 5432 | postgres | No |
| 6379 | redis | No |

---

## 14.  Architectural Decisions Record

**SETI monitors itself.**  SETI's own constellation is registered as the first application in Policy.  All of SETI's Jobs are health-checked by AC, contract-tested by AC's suite, and their event traffic is visible in the Dashboard.  If SETI cannot watch itself, the core mechanism is unproven.

**Observability first.**  The observability Job was built before any business logic.  Every subsequent Job reports to it from the moment it runs.  This was not a development convenience — it was the correct architectural sequencing.  A system you cannot observe while building is harder to debug and harder to trust.

**Three-path triage with hardcoded thresholds.**  The 50% critical threshold and 7-day silence window in Interactions are Go constants.  Changing them requires a code review, a rebuild, and a deployment.  Detection thresholds are not configuration — they are policy decisions.  Configuration values can be changed by anyone with compose file access and changed back.  Code constants require a traceable commit.

**AC inversion.**  Every health monitoring system in the industry runs on the same model:  services expose an HTTP endpoint, an external agent polls it.  SETI's Augur Canis inverts this for the contract testing layer:  AC's healthcheck binary publishes a request, the external agent (AC) verifies behavior over the dedicated ac-net network.  No open ports required.  The service does not need to know it is being tested.

**AI-lien reads before it writes.**  An AI analysis without institutional memory is a first-responder with no case history.  AI-lien reads Lore baselines, recent incidents, and known patterns before every analysis.  It writes assessments back to Lore after.  The loop — Lore feeds AI-lien, AI-lien feeds Lore — means every analysis makes the next one better.

**The Notifier is permanently opaque.**  SETI does not know what "notify a human" means in your environment.  That knowledge belongs to the operator.  The Notifier's contract is the interface.  The implementation is the operator's responsibility.  Making the notification mechanism an explicit stub — rather than a default that mostly works — forces the operator to make a deliberate decision about how humans get woken up.

**Feed Wr4ngler in Elixir.**  OTP supervision means a crashed feed process restarts in isolation.  The supervisor does not take down the Job.  Other Wr4nglers' feeds are unaffected.  This failure boundary was the deciding factor.

**Plots are authored, not generated.**  Plot tests verify real user flows.  AI can scaffold a Plot from a contract, but Show Wr4ngler testing — adversarial scenarios, real-world usage patterns, edge cases derived from operational experience — cannot be automated.  Generated tests re-state the contract.  Show Wr4ngler tests question it.

**cert-forge is infrastructure, not initialization.**  The original shared-volume PKI approach wrote all private keys to a shared volume, meaning every service could read every other service's key material.  cert-forge holds the CA private key in memory only, issues instance certs over an authenticated enrollment channel, and never writes private key material to shared storage.  This is not a SETI-specific pattern — it applies to any TCA constellation with more than one service.

**Redis Streams over Lists for metrics.**  Rolling latency and health windows were initially implemented as Redis Lists.  Lists store raw values with no timestamps.  When the LaE visualization required time-bounded queries, Lists were replaced with Redis Streams.  Streams provide millisecond-precision timestamps as built-in stream IDs, enabling `XRANGE` queries by time window.  The change was additive — the detectors updated their read pattern, everything else was unchanged.

**The Ring is a Sec Wr4ngler tool, not a developer tool.**  Trial, Reports, and LaE are gated to `sec-wr4ngler` clearance.  A Sec Wr4ngler has the authority to interpret test results, sign off on contract behavior, and set alert thresholds.  These capabilities belong together under a single clearance gate.  Developers who need to run ad-hoc tests during development should request `sec-wr4ngler` clearance in their development environment — not bypass the gate.

---

## 15.  Rodeo Clown (Future)

Rodeo Clown is a planned SETI sidecar for security boundary verification.  Named deliberately.  A Rodeo Clown operates outside the normal structure and absorbs hits — exactly the right metaphor for a component that deliberately operates outside the mTLS trust boundary to probe it.

Where the contract test suite verifies that registered endpoints respond correctly from inside the constellation, Rodeo Clown operates without a valid constellation certificate.  It fires requests using plain HTTP, self-signed certs, expired certs, and wrong-CA certs, and reports findings back to SETI via the Integration Job's external API — the only endpoint designed for consumption from outside the constellation.

**What it tests:**
- Does the gateway reject connections without a valid client certificate?
- Does it reject self-signed certificates not issued by the constellation CA?
- Does it reject expired certificates?
- Does it reject connections over plain HTTP where mTLS is required?

These tests cannot be performed from inside the constellation — AC always presents a valid certificate.  Rodeo Clown deliberately doesn't have one.

**What it is not:**  A general-purpose fault injection tool.  Netflix's Chaos Monkey tests failure recovery.  Rodeo Clown tests the transport security boundary.  The scope is deliberately narrow.

Findings route to SETI via the Integration API, not the observability stream — Rodeo Clown is not a trusted constellation member and cannot authenticate to the mTLS observability endpoint.

---

## 16.  Show Wr4ngler Role

The Show Wr4ngler is a defined testing role — not a job title, a function.  Where contract tests verify the inside of the system and plot tests verify user flows, the Show Wr4ngler covers the user perspective, real-world usage patterns, and adversarial scenarios that cannot be derived from contracts.

Things only a Show Wr4ngler can verify:
- What happens when a feed is provisioned and the Wr4ngler immediately logs out?
- What does a Sec Wr4ngler need to see in a report to sign off on a deployment?
- What happens when 50 contract tests run simultaneously?
- What does SETI do when a monitored constellation goes dark mid-test?

The plots built by AI are scaffolding — they prove the happy path works.  The Show Wr4ngler's job is to find every path that isn't happy and decide which ones become permanent tests in the suite.

Show Wr4ngler tests that reveal real defects should be converted to Plot tests or added to the AC contract test suite.  Tests that require adversarial tooling or human judgment remain in the Show Wr4ngler's manual playbook.
