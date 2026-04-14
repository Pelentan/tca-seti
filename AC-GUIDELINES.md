# Augur Canis — Guidelines and Reference

**Version:** 1.0  
**Last Updated:** 2026-04-13  
**Audience:** Engineers deploying AC for the first time, and engineers integrating AC into a TCA constellation  
**Repository:** github.com/Pelentan/augur-canis  

---

## 1.  What Augur Canis Is

Augur Canis (AC) is a health and behavioral monitoring service for containerized microservice constellations.  It watches your services, tests whether they behave as their contracts specify, detects anomalies in latency and failure rate, fires alerts when things go wrong, and remembers what it has seen.

It has one dependency:  Redis.  It speaks mTLS.  It self-registers with no external coordination.  You add a small registration snippet to each service you want monitored, point those services at AC, and AC handles the rest.

AC is a standalone product.  It does not require SETI, TCA, or any other framework to do its job.  For operators who want to go further, AC is also the behavioral verification layer in a TCA constellation — but that is a later concern.

---

## 2.  What AC Does

**Health monitoring.**  AC sends a `POST /check` request to every registered service on a configurable interval (default 30 seconds).  Services respond with their health status and container ID.  AC records latency, detects silence, and tracks container replacements in Kubernetes environments.

**Contract test execution.**  AC maintains a hardcoded suite of contract tests — one set per registered service.  These tests are authored independently from the contracts they verify.  They run on a schedule (default every 15 minutes) and on demand.  Results are stored in Redis for 24 hours.

**Behavioral pattern detection.**  AC maintains rolling time-series streams of latency and health data per service.  Three detectors run continuously:

- **Silence detector:** fires when a service stops responding entirely.
- **Latency drift detector:** fires when current latency exceeds the established baseline by a configurable multiplier.
- **Failure rate detector:** fires when more than a configurable percentage of recent health checks are unhealthy.

**Alert lifecycle.**  AC fires alerts into a Redis pub/sub channel.  It barks a configurable number of times at configurable intervals before going quiet on a persistent problem.  Alerts are acknowledged by name or by alert ID.

**Institutional memory.**  AC stores baseline latency data per service, auto-establishing baselines from observed behavior after a minimum number of samples.  Sec Wr4nglers can set explicit SLA thresholds with configurable alert severity levels.

**Dev UI.**  When `AC_UI_PORT` is set, AC serves a plain HTML observability page on that port — no auth, no build step, one page.  Intended for initial setup verification and a quick visual check.  Remove before production.

---

## 3.  The Only Dependency:  Redis

AC requires Redis.  Every piece of state AC maintains — health windows, latency streams, alert state, contract suite results, baselines — lives in Redis.  This is intentional:  AC's state survives restarts, is queryable by other services, and scales to multiple AC instances without coordination.

**Minimum docker-compose.yml:**

```yaml
services:
  redis:
    image: redis:7-alpine
    container_name: ac-redis
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 3

  augur-canis:
    build: .
    container_name: augur-canis
    environment:
      PORT: "4010"
      REDIS_URL: "redis:6379"
      CONSTELLATION: "my-constellation"
      AC_UI_PORT: "4666"   # ⚠ remove before production
    ports:
      - "4666:4666"        # ⚠ dev UI only — remove before production
    depends_on:
      redis:
        condition: service_healthy
```

For a TCA constellation with mTLS, additional environment variables are required.  See Section 8.

---

## 4.  Self-Registration Pattern

Any service that wants AC to monitor it calls a self-registration endpoint at startup.  This is the entire integration cost on the service side.

The registration call includes:
- `service_name` — the canonical name of the service (must match the contract title exactly if contract tests are used)
- `network_endpoint` — the full URL AC should use to reach this service for health checks and contract tests
- `cert_fingerprint` — SHA-256 fingerprint of the service's instance certificate (used for identity verification in mTLS environments)
- `timestamp` — ISO 8601 UTC
- `signature` — HMAC or cert-based signature over the above fields, preventing spoofed registrations

AC verifies the signature, stores the registration, and begins monitoring immediately on the next check cycle.

**Go implementation** (canonical — see `certforge.go` in the repository):

```go
func selfRegisterWithAC(certMat *CertMaterial, endpoint string) {
    // Build registration payload
    // Sign with instance private key
    // POST to https://augur-canis:4010/services/register
    // Retry with backoff until successful
}
```

The self-registration call retries indefinitely with exponential backoff.  AC may not be ready when a service starts.  This is expected.  Services do not fail to start because AC is not yet available.

**What AC does when a service registers:**
1. Stores the registration in memory and Redis (persists across AC restarts)
2. Adds the service to the health check rotation
3. Adds the service to the contract test suite if tests are defined for it
4. Begins recording latency and health samples to its metric streams

---

## 5.  Health Check Protocol

AC sends `POST /check` to each registered service's `network_endpoint`.  The request body:

```json
{
  "request_id": "check-1776004577255257373",
  "service_name": "my-service",
  "constellation": "my-constellation",
  "timestamp": "2026-04-13T12:00:00Z"
}
```

The service responds:

```json
{
  "service_name": "my-service",
  "status": "healthy",
  "container_id": "abc123def456",
  "uptime_seconds": 3600
}
```

`status` must be `"healthy"` for AC to record a clean check.  Any other value, or any non-200 response, is recorded as a failure.

`container_id` is used for Kubernetes container replacement detection.  When AC sees a different container ID than the previous check, it infers a pod replacement and auto-resolves any active silence alert for that service.

The check interval is configurable via `CHECK_INTERVAL_SECONDS` (default 30).  The silence threshold is configurable via `SILENCE_THRESHOLD_SECONDS` (default 90 — three missed checks).

### Using AC's check endpoint for Docker and Kubernetes health probes

Every service that self-registers with AC already implements `POST /check`.  That same endpoint doubles as the target for Docker Compose healthchecks and Kubernetes readiness and liveness probes — no additional endpoint is needed.

**Docker Compose:**

```yaml
healthcheck:
  test: ["CMD", "wget", "-qO-", "--post-data='{}'",
         "http://localhost:4007/check"]
  interval: 30s
  timeout: 10s
  retries: 3
  start_period: 20s
```

For mTLS services, use the `healthcheck` binary pattern (a small Go binary included in the container that makes the mTLS call with the service's own cert) rather than `wget` or `curl`, which cannot present a client certificate:

```yaml
healthcheck:
  test: ["CMD", "/healthcheck"]
  interval: 30s
  timeout: 10s
  retries: 3
  start_period: 20s
```

**Kubernetes readiness and liveness probes:**

The readiness probe determines whether the pod is ready to receive traffic.  The liveness probe determines whether the pod should be restarted.  Use the same `/check` endpoint for both, with different thresholds reflecting their different purposes.

```yaml
readinessProbe:
  httpPost:
    path: /check
    port: 4007
  initialDelaySeconds: 10
  periodSeconds: 10
  failureThreshold: 3

livenessProbe:
  httpPost:
    path: /check
    port: 4007
  initialDelaySeconds: 30
  periodSeconds: 30
  failureThreshold: 5
```

For mTLS pods, use `exec` probes with the healthcheck binary:

```yaml
readinessProbe:
  exec:
    command: ["/healthcheck"]
  initialDelaySeconds: 10
  periodSeconds: 10
  failureThreshold: 3

livenessProbe:
  exec:
    command: ["/healthcheck"]
  initialDelaySeconds: 30
  periodSeconds: 30
  failureThreshold: 5
```

The result is a single `/check` implementation that serves three purposes simultaneously:  AC's health monitoring, Docker's container health tracking, and Kubernetes' readiness and liveness gating.  A service that is unhealthy fails all three — AC alerts, Docker marks the container unhealthy, and Kubernetes stops routing traffic to the pod and restarts it if the condition persists.  No additional endpoints, no duplicated logic.

---

## 6.  Contract Test Suite

The contract test suite is the independent behavioral verification layer.  It is distinct from health checks in a critical way:  **health checks verify that a service is alive; contract tests verify that it behaves correctly.**

### What the tests verify

For each registered service, the suite defines:
- **Positive tests** — key endpoints return expected 2xx status codes with expected response fields present
- **Negative tests** — POST endpoints with missing or malformed input return 4xx, not 5xx

A 5xx on bad input means the service is swallowing errors.  That is a real bug.  The negative tests are the tests most likely to catch it.

### Authorship discipline

The tests are authored by hand, independently from the contracts they verify.  This is deliberate.  Generated tests can only re-state what the contract already says — they cannot independently verify it.  If a contract changes, the tests must be updated manually.  That friction is the point.  The work loop is:  change the contract, update the tests, review the delta.  A test suite that updates itself automatically when the contract changes provides no independent verification.

### Scheduling

The suite fires:
- Once at startup (after a 30-second delay for services to register)
- Every `CONTRACT_TEST_INTERVAL_SECONDS` (default 900 — 15 minutes)
- On demand via `POST /run-contract-tests`

Results are stored in Redis with a 24-hour TTL under `ac:contract-suite:{run_id}`.

### Transport

All contract tests execute over the same mTLS transport AC uses for health checks.  Tests that cannot reach a service (not registered, connection refused, DNS failure) are skipped, not failed.  A skip is a deployment gap, not a test failure.

### Customization

The test suite is defined in `contracttests.go`.  Adding tests for a new service means adding entries to the `setiContractTests` slice and rebuilding.  No runtime configuration.  No test files.  The tests are code.

---

## 7.  Detectors

All three detectors run on a shared ticker (default 30-second interval).  Each detector scans all services that have accumulated enough data before evaluating.

### Silence Detector

Fires `AlertSilence` at `SeverityCritical` when a service has not responded to a health check within `SILENCE_THRESHOLD_SECONDS`.

Default:  90 seconds (3 missed checks at 30-second interval).

Auto-resolves when the service responds again.  Also auto-resolves when a Kubernetes container replacement is detected — a pod replacement is not a silence, it is a restart.

### Latency Drift Detector

Maintains a rolling time-series stream of latency samples per service (`ac:metrics:{service}:latency` in Redis).  Establishes an auto-baseline after `MIN_BASELINE_SAMPLES` (default 10) samples using exponential moving average with a slow decay factor — the baseline tracks legitimate improvement over time without losing history.

Fires `AlertLatencyDrift` at `SeverityWarn` when:
- Current rolling mean exceeds auto-baseline × `LATENCY_DRIFT_MULTIPLIER` (default 2.0)
- For `CONSECUTIVE_DRIFT_CYCLES` (default 3) consecutive detection cycles

The consecutive-cycle requirement prevents a single slow health check response from triggering an alert.

If a Wr4ngler baseline has been set for the service (via `POST /baselines/{service}`), AC also checks against that threshold and fires at the configured severity level — independently of the auto-baseline evaluation.

### Failure Rate Detector

Reads the rolling health stream (`ac:metrics:{service}:health`) — a time-series of 0/1 values per check.

Fires `AlertFailureRate` at `SeverityWarn` when:
- Failure rate over the most recent `MIN_BASELINE_SAMPLES` checks exceeds `FAILURE_RATE_THRESHOLD` (default 0.3 — 30%)

If a Wr4ngler failure rate threshold is set for the service, AC checks against that threshold at the configured severity.

### Threshold Hardcoding

All detector thresholds are hardcoded constants in the Go source, not environment variables.  This is deliberate.  Changing a detection threshold is an architectural decision — it requires a code review, a rebuild, and a deployment.  An environment variable threshold can be set to zero by anyone with access to the compose file and then reset to hide the change.  The friction is the safeguard.

---

## 8.  Alert Lifecycle

When a detector fires:

1. AC creates an alert record in Redis keyed by service name
2. AC publishes an `Alert` object to the Redis `tca:augur-canis:alerts` pub/sub channel
3. AC logs the alert at INFO level

**The bark pattern.**  AC does not repeat alerts indefinitely.  It sends a configurable number of initial barks (`MAX_INITIAL_BARKS`, default 3) at `REMINDER_INTERVAL_SECONDS` intervals.  After the initial barks are exhausted, AC goes quiet.  Silence on a persistent problem is intentional — the problem has already been communicated.

**Acknowledgment.**  Alerts are acknowledged via `POST /alerts/{alert_id}/acknowledge`.  Acknowledgment records who acknowledged it and an optional note.  It does not resolve the alert — resolution happens when the condition clears.

**Auto-resolution.**  The silence detector auto-resolves when the service responds.  The latency drift and failure rate detectors auto-resolve when the condition clears for a sustained period.

**Alert severity.**  Three levels:  `critical`, `warn`, `info`.  The silence detector always fires at `critical`.  Drift and failure rate detectors fire at `warn` for auto-baseline violations.  Wr4ngler baselines can be set to fire at any level including `critical` — which maps to Notifier's highest-urgency notification path when integrated with SETI.

---

## 9.  Baselines and Wr4ngler Thresholds

### Auto-baselines

AC establishes baselines automatically.  After `MIN_BASELINE_SAMPLES` latency readings for a service, AC computes the mean and stores it as the auto-baseline.  The auto-baseline updates on every subsequent reading using EMA — it does not reset.

Auto-baselines are stored in Redis under `ac:baseline:auto:{service}` and rebuild automatically after an AC restart.

### Wr4ngler baselines

A Sec Wr4ngler can set an explicit threshold per service:

```
POST /baselines/{service_name}
{
  "latency_threshold_ms": 50,
  "failure_rate_threshold": 0.1,
  "alert_level": "critical",
  "set_by": "wr4ngler-id",
  "notes": "SLA requires 50ms p99 and 90% availability"
}
```

`alert_level` maps directly to Notifier severity in a SETI integration — `critical` wakes humans, `low` sends an email digest.  In standalone AC, `alert_level` is recorded in the alert and available to any downstream consumer of the alerts channel.

Both thresholds are optional.  Setting only `latency_threshold_ms` without `failure_rate_threshold` is valid.  At least one must be set.

Wr4ngler baselines persist in Redis indefinitely.  They do not rebuild from observed data — they are explicit operator decisions and must be explicitly removed or replaced.

### Viewing baselines

```
GET /baselines                    → all services with any baseline
GET /baselines/{service_name}     → both tiers for one service
```

---

## 10.  Metric Streams

AC writes two Redis Streams per registered service:

- `ac:metrics:{service}:latency` — one entry per health check, containing `latency_ms` and `container_id`
- `ac:metrics:{service}:health` — one entry per health check, containing `healthy` (1 or 0) and `container_id`

Stream entries use Redis auto-generated millisecond-precision timestamps as IDs.  This enables time-bounded queries with `XRANGE`.

Streams are capped at `metricsStreamMaxLen` entries (default 2000, approximately 24 hours at a 30-second check interval).

The metric streams are queryable via `GET /metrics?since_ms={unix_ms}`.  The response includes all registered services with their samples filtered to the requested time window, plus auto-baseline and Wr4ngler baseline data for each service.

These streams are the data source for the LaE (Lag and Errors) visualization in SETI.  In standalone AC, they are available to any tool that can make an authenticated HTTP request.

---

## 11.  Dev UI — Port 4666

When `AC_UI_PORT=4666` is set, AC starts a plain HTTP server on that port.  No authentication.  No build step.  The page is embedded in the AC binary.

**What it shows:**
- Summary pills — jobs registered, active alerts, checks completed, alerts fired, constellation name
- Registered jobs table — service name, endpoint, latest latency, last seen, registered at, alert status
- Active alerts — service name, alert type, bark count, time since first alert
- Last contract suite run — result bar, score, trigger, duration, timestamp
- Detector states — silence, latency drift, failure rate

The page polls `GET /api/status` every 30 seconds.  The refresh button triggers an immediate poll.

**Production warning.**  The warning banner on this page is not a suggestion.  This endpoint has no authentication.  Anyone who can reach the port can see your service topology, endpoint URLs, and health data.  Remove `AC_UI_PORT` and the corresponding `ports:` mapping before deploying to any non-isolated environment.

Port 4666.  For 666.  The name is the reminder.

---

## 12.  Admin API Reference

All admin endpoints are on the mTLS port (`PORT`, default 4010).  They require a valid constellation certificate.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | AC health, job count, alert count, uptime |
| `/jobs` | GET | All registered jobs |
| `/jobs` | POST | Manually register a job |
| `/alerts/active` | GET | All active alerts |
| `/alerts/{id}/acknowledge` | POST | Acknowledge an alert |
| `/checks/recent` | GET | Most recent contract suite run |
| `/checks/{service}` | GET | Recent results for a specific service |
| `/contract-suites` | GET | List of all stored suite runs (summary) |
| `/contract-suites/{run_id}` | GET | Full detail for a specific suite run |
| `/run-contract-tests` | POST | Trigger on-demand suite run |
| `/ring/run` | POST | Ad-hoc contract test from YAML input |
| `/baselines` | GET | All services with baselines |
| `/baselines/{service}` | GET | Both baseline tiers for a service |
| `/baselines/{service}` | POST | Set Wr4ngler baseline for a service |
| `/metrics` | GET | Time-series latency and health data |
| `/configuration` | GET | AC configuration values |
| `/federation/register` | POST | Register a federated AC instance |
| `/federation/status` | GET | Federation status |
| `/queries` | GET | Current contract test definitions |

---

## 13.  Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `4010` | mTLS admin API port |
| `REDIS_URL` | `redis:6379` | Redis connection string |
| `CONSTELLATION` | `tca-seti` | Constellation identifier |
| `CHECK_INTERVAL_SECONDS` | `30` | How often AC checks each service |
| `SILENCE_THRESHOLD_SECONDS` | `90` | Seconds before a non-responding service is considered silent |
| `REMINDER_INTERVAL_SECONDS` | `30` | Seconds between alert barks |
| `MAX_INITIAL_BARKS` | `3` | Number of times AC barks before going quiet |
| `CONTRACT_TEST_INTERVAL_SECONDS` | `900` | Seconds between scheduled contract suite runs |
| `RESULT_TTL_SECONDS` | `30` | TTL for individual contract test results |
| `AC_UI_PORT` | _(unset)_ | Plain HTTP dev UI port — unset in production |
| `CERT_FORGE_URL` | `https://cert-forge:4014` | cert-forge enrollment endpoint (mTLS deployments) |
| `ENROLLMENT_CERT` | `/certs/enrollment.crt` | Path to enrollment certificate |
| `ENROLLMENT_KEY` | `/certs/enrollment.key` | Path to enrollment private key |
| `ENROLLMENT_CA` | `/certs/enrollment-ca.crt` | Path to enrollment CA certificate |
| `AUGUR_CANIS_URL` | `https://augur-canis:4010` | AC's own address (used by services self-registering) |
| `OBSERVABILITY_URL` | _(none)_ | SETI observability endpoint for event reporting |
| `STAR_GAZER_CERT` | `/certs/star-gazer.crt` | Federation identity certificate |

Detection thresholds (`MIN_BASELINE_SAMPLES`, `LATENCY_DRIFT_MULTIPLIER`, `FAILURE_RATE_THRESHOLD`, `CONSECUTIVE_DRIFT_CYCLES`, `metricsStreamMaxLen`) are hardcoded constants.  They are not environment variables by design.

---

## 14.  Redis Key Reference

| Key Pattern | Type | Contents |
|-------------|------|----------|
| `ac:job:{service}` | String | Registered job record (JSON) |
| `ac:state:{service}:last_seen` | String | ISO 8601 timestamp of last health check |
| `ac:state:{service}:last_container_id` | String | Most recent container ID |
| `ac:state:{service}:alert_active` | String | `"1"` if alert is active |
| `ac:state:{service}:alert_id` | String | Current alert ID |
| `ac:state:{service}:alert_type` | String | `silence`, `latency_drift`, `failure_rate` |
| `ac:state:{service}:bark_count` | String | Number of barks sent for current alert |
| `ac:state:{service}:last_bark_at` | String | Timestamp of most recent bark |
| `ac:state:{service}:alert_first_at` | String | Timestamp when alert first fired |
| `ac:state:{service}:drift_cycles` | String | Consecutive over-threshold cycles (TTL: 5 min) |
| `ac:metrics:{service}:latency` | Stream | Latency samples with timestamps |
| `ac:metrics:{service}:health` | Stream | Health samples (0/1) with timestamps |
| `ac:baseline:auto:{service}` | String | Auto-established latency baseline (JSON) |
| `ac:baseline:wr4ngler:{service}` | String | Wr4ngler-set threshold (JSON) |
| `ac:contract-suite:{run_id}` | String | Full contract suite result (JSON, 24h TTL) |
| `ac:alert:{alert_id}` | String | Alert ID → service name reverse lookup (24h TTL) |
| `tca:augur-canis` | Pub/Sub | Health check result events |
| `tca:augur-canis:alerts` | Pub/Sub | Alert events |
| `tca:contract-requests` | Pub/Sub | Contract test dispatch channel |
| `tca:contract-results` | Pub/Sub | Contract test result channel |
| `tca:ac-contract-suite` | Pub/Sub | Suite completion events |

---

## 15.  Standalone vs. SETI Integration

AC is fully functional as a standalone service.  Nothing in the first 14 sections requires SETI, TCA, or any other tooling.

When AC is integrated into a SETI constellation, additional capabilities activate:

- **Interactions routing.**  Alert events from AC's pub/sub channel are consumed by the Interactions Job, which routes failures through a three-path triage system (immediate escalation, AI analysis, silence).
- **AI-lien analysis.**  The Interactions Job sends failure context to AI-lien, which reads Lore institutional memory before analysis and writes assessments back.
- **Lore persistence.**  AI-lien writes trend points and incidents to Lore (PostgreSQL-backed).  AC-generated baselines are supplemented by AI-assessed baselines established over time.
- **Notifier integration.**  Critical failures page on-call humans through the operator-configured Notifier Job.
- **SETI observability.**  AC reports all outbound calls to the SETI observability stream for the Dashboard event log.
- **Ring / LaE UI.**  The SETI UI provides Ring (contract test management, report history) and LaE (lag and errors visualization) built on AC's existing API endpoints.

The integration surface is entirely in AC's existing admin API.  No new endpoints are required.  No changes to self-registering services.  The jump from standalone AC to full SETI integration is an Interactions configuration change, not an AC change.

---

## 16.  Architectural Decisions Record

**Self-registration over discovery.**  AC does not scan networks, read Docker APIs, or use service mesh metadata.  Services opt in explicitly.  This means AC only monitors services that have decided to be monitored — no accidental monitoring of infrastructure services, no false positives from transient containers.

**Hardcoded test suite over generated tests.**  Contract tests are authored by a human who read the contract and wrote assertions independently.  Generated tests re-state the contract; they do not verify it.  The hardcoded suite is the "say it three times" discipline:  the contract says what it should do, the implementation does it, the test verifies it independently.

**Redis Streams over Lists for metrics.**  Rolling latency and health windows were initially implemented as Redis Lists (LPUSH/LTRIM).  Lists store raw values with no timestamps, making time-bounded queries impossible.  Streams were adopted to support time-slice filtering in the LaE visualization and to enable `XRANGE` queries for the metrics API.

**Detection thresholds as constants.**  Detector thresholds are Go constants, not environment variables.  Changing a detection threshold is an architectural decision with operational consequences.  It requires a code review, a rebuild, and a tracked deployment.  An environment variable threshold can be silently changed by anyone with compose file access and silently changed back.  The hardcoding is the audit trail.

**Alert barking pattern over continuous alerting.**  AC barks a configurable number of times and goes quiet.  Continuous alerting on persistent problems trains operators to ignore alert channels.  A finite bark count communicates urgency without noise fatigue.  The problem is either resolved (alert auto-clears) or acknowledged (human has seen it).  Either way, continued barking serves no purpose.

**Enrollment CA separation.**  AC's mTLS deployment uses cert-forge with a separate enrollment CA.  The enrollment CA's only issued credential is the enrollment certificate — the only private key material that touches shared storage.  The constellation CA private key never leaves cert-forge's memory.  Compromising the enrollment cert grants the ability to request instance certs; it does not grant the ability to forge constellation CA signatures.
