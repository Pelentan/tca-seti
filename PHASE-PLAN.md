# tca-seti — Phase Plan

**Status:** In Progress
**Last Updated:** 2026-04-21
**Next Action:** Push gateway and policy images, roll deployment, verify contract tests and plots pass. Then resume Phase 9 — Interactions triage routing, AI-lien Lore integration, Results fast-path write.

---

## Phase 1 — See the Feed: Live observability dashboard accessible to authenticated Wr4nglers

**Status:** Complete
**Deliverable:** A Wr4ngler can log in, provision a signal feed, and watch SETI's own inter-service events appear in the dashboard in real time.
**Rationale:** SETI is the application that watches other applications. If SETI cannot watch itself, the core mechanism is unproven. Observability first — every subsequent Job reports to it from the moment it runs.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| gateway | Go | 4000 | Complete |
| seti-observability | Go | 4011 | Complete |
| signal-clearance | TypeScript | 4001 | Complete |
| ui | TypeScript/React + Go | 4000 (via gateway) | Complete |

### Lessons Learned
- Redis pub/sub as the observability backbone proved its value immediately — the feed was live before any application logic was written.
- AUTH_MODE=dev stub pattern enabled full UI development without a real OIDC provider.

---

## Phase 2 — Core Testing: SETI can run Contract Tests against a registered application and store results

**Status:** Complete
**Deliverable:** Register a TCA application with SETI. SETI reads its contracts, generates test cases, executes them, and stores results. A Wr4ngler can see the results in their feed.
**Rationale:** This is SETI's primary value proposition. Everything else is infrastructure around this capability.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| policy | Go | 4002 | Complete |
| contract-test | Go | 4003 | Complete |
| results | Python | 4008 | Complete |

### Lessons Learned
- Policy owning the application registry and schedule is the correct separation — contract-test is stateless, policy owns state.
- Python distroless runtime requires careful dependency management for SSL/TLS operations.

---

## Phase 3 — Deep Testing: Plot Test execution with call chain verification

**Status:** Complete
**Deliverable:** AI-generated Plots execute against a registered application. Call chains are verified against the Signal Aggregator event stream. Failures route to the Interactions stub.
**Rationale:** Call chain verification requires the Signal Aggregator to be subscribed to application event streams.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| plot-store | Go | 4005 | Complete |
| plot-test | Go | 4004 | Complete |
| signal-aggregator | Go | 4006 | Complete |
| feed-wrangler | Elixir | 4007 | Complete |
| interactions | Go | 4009 (stub) | Complete |

### Lessons Learned
- Elixir OTP supervision for feed-wrangler proved correct — feed process isolation is exactly the failure boundary needed.
- Status model completeness matters: `open` collapsing to `public/private` mid-build caused silent vote rejection. Define status enums completely in contracts before implementation.

---

## Phase 4 — Intelligence and External Access: AI-powered analysis and customer tooling integration

**Status:** Complete
**Deliverable:** Failed tests trigger AI-lien analysis via the Interactions Job. Customer tooling can query SETI results via the Integration Job's versioned API.
**Rationale:** AI-lien requires a running failure path to analyze — Phase 3 must be complete before AI-lien has anything to reason about.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| ai-lien | Python | 4252 | Complete |
| integration | Go | 4013 | Complete |
| augur-canis | Go | 4010 | Complete |

### Lessons Learned
- Double-tap AI query pattern (conservative first-tap, aggressive second-tap) is the correct design for AI diagnostic queries — preserves audit trail while allowing depth.
- Augur Canis Redis pub/sub health check architecture (no open HTTP ports) is the correct pattern for any containerized environment. Worth documenting as a TCA standard.

---

## Phase 5 — cert-forge: Full PKI abstraction layer for the constellation

**Status:** Complete
**Deliverable:** All 15 SETI Jobs obtain instance certs dynamically from cert-forge on startup. Private keys never touch the shared volume. Signing delegated to cert-forge. Enrollment CA gates cert issuance.
**Rationale:** The original shared-volume cert approach was a known architectural debt. cert-forge resolves it completely and establishes a reusable TCA standard component alongside Augur Canis.

### What Was Built

cert-forge is a persistent running service (not an init container) with three-port architecture:
- Port 4016 — plain HTTP, `/ca` only (CA cert is public information)
- Port 4015 — enrollment mTLS, `/instance-cert` only (requires enrollment cert)
- Port 4014 — constellation mTLS, `/sign` only (requires instance cert)

A separate enrollment CA authenticates containers requesting instance certs. The enrollment cert is injected via `.env` (dev) or K8s secret (production). The constellation CA never has its key written to disk. All signing operations are delegated to cert-forge — services are crypto-agnostic.

cert-forge client implemented in all four languages: Go (`certforge.go`), TypeScript (`certforge.ts`), Python (`certforge.py`), Elixir (`cert_forge.ex`).

### Lessons Learned
- TLS client auth is negotiated at the connection level, not the request level. You cannot have unauthenticated and authenticated endpoints on the same port. Three-port design is the correct solution.
- `ListenAndServeTLS` clones `TLSConfig` at server start — mutations to `server.TLSConfig` after start are ignored. `GetCertificate` / `GetConfigForClient` are the correct hooks for dynamic cert behavior.
- The shared volume cert approach is standard practice in Docker Compose tutorials and production systems. cert-forge is genuinely new territory — PKI abstraction at the application layer that works identically across Docker Compose, Swarm, and K8s.
- In K8s production, cert-forge's cert issuance backend points to cert-manager or the company CA. The signing responsibility remains with cert-forge regardless of environment. The constellation is fully decoupled from infrastructure PKI choices.
- Python `global` declaration is illegal for annotated module-level names. Use plain assignment for module-level vars that need `global` access in `__main__` blocks — or restructure to avoid `global` entirely.
- `obtain_certs()` must be the first call in any service startup. All SSL context builds depend on cert material being available.

---

## Phase 6 — Self-Monitoring: SETI monitors itself completely

**Status:** Complete
**Deliverable:** SETI is fully registered in its own Policy Job, running its own Contract Tests on 15-minute schedule, with AI-generated Plots in contracts/plots/ ready for Plot Test execution. The methodology tests itself.
**Rationale:** SETI's self-testing is the proof of concept for the entire methodology. This is not a build phase — it is a configuration and verification phase. No new Jobs.

### Jobs
*No new Jobs — configuration and verification only.*

### Phase 6 Completion Criteria
- [x] tca-seti registered in its own Policy Job with full schedule configuration
- [x] Contract Tests running against all SETI contracts on 15-minute schedule (20/20 passing, 0 skipped)
- [x] AI-generated Plots in contracts/plots/ for all SETI Jobs
- [x] Plot Tests running and passing — 11/11 plots, 100% steps passing (2026-04-11)
- [x] Full self-monitoring loop verified end-to-end

### Lessons Learned
- AI-generated Plots belong in contracts/plots/ alongside the contracts that generated them — same versioning boundary, same source-of-truth principle.
- Plot format requires reading the actual implementation structs, not just the contract schema. The contract specifies intent; the implementation struct is the serialization truth. `token` vs `jwt` — both wrong in different directions.
- `/auth/refresh` requires an httpOnly cookie. Plot-test is a server-side mTLS client with no cookie jar — structurally untestable from this surface. Testable behavior must be externally observable.
- Fire-and-forget endpoints (`/run-contract-test`) return `accepted`, not the run result. Test what the contract promises, not what would be convenient.
- `silentRefresh` on every poll iteration clears localStorage on any transient 400. Rate-limit refresh calls — use the JWT in hand, only refresh on expiry or every N iterations.
- expect_call_chain is where Plot tests differentiate from contract tests: verifying the service called its upstream is behavioral proof that contract tests cannot provide.
- Plot files reload on Plot Store restart without a code rebuild — no `docker compose build` required for plot content changes.

---

## Phase 7 — Application Registry: Sec Wr4ngler-managed external application onboarding

**Status:** Complete
**Deliverable:** A Sec Wr4ngler can onboard external TCA constellations into SETI via the Admin UI without any code changes. Available applications are declared in remote-apps.json (K8s secret in production). The Sec Wr4ngler tests registry connectivity, registers the application (triggering AC federation + Plot ingest), and deregisters when needed. Historical results survive deregistration.
**Rationale:** SETI's value scales with the number of constellations it monitors. Manual registration via API calls is not a viable operational model. The Admin UI with clearance-gated controls is the correct delivery surface.

### Jobs
*No new Jobs — Policy, Signal Aggregator, Plot Store, Gateway, and UI extended.*

### What Was Built

**remote-apps.json** — tag-keyed manifest of available applications. Mounts read-only into Policy at `/etc/seti/remote-apps.json`. K8s: mount as a Secret. Contains: name, description, ac_endpoint, registry_url, registry_type (github/gitlab/generic), registry_token.

**Policy extensions:**
- Reads remote-apps.json on startup, holds available state in memory
- `GET /available-applications` — list all available apps with active/inactive status (all clearances)
- `POST /available-applications/{tag}/test-connection` — tests registry read access, returns contract/plot file counts
- `POST /available-applications/{tag}/register` — registers app in Policy store, triggers Signal Aggregator federation + Plot Store ingest
- `POST /available-applications/{tag}/deregister` — marks inactive, drops federation subscription, stops scheduling. Historical data retained.

**Signal Aggregator extension:** `handleFederationSubscriptions` now accepts POST (runtime federation connect) and DELETE (disconnect). Closes the startup TODO — new applications federate without restart.

**Plot Store extension:** `POST /ingest/{tag}` — fetches contracts/plots/ directory from registry, parses each JSON Plot file, stores new/versions existing. GitHub, GitLab, and generic HTTPS auth supported.

**Gateway:** Two new proxy routes for /available-applications and /available-applications/.

**Admin UI:** Tab structure added. AI CONFIGURATION tab (existing). APPLICATIONS tab: card per available app, connection status badge, Test Connection (all clearances), Register/Deregister (sec-wr4ngler only with green/red button styling).

### Lessons Learned
- remote-apps.json tag-keyed format (not array) scales to hundreds of entries — direct key lookup, no iteration needed.
- registry_type field (github/gitlab/generic) determines auth header format. GitHub: `Authorization: Bearer`. GitLab: `PRIVATE-TOKEN`. Generic: `Authorization: Bearer`. Three lines of switch logic covers the real-world landscape.
- Signal Aggregator federation was startup-only. Adding POST/DELETE to the federation subscriptions handler at runtime closes the architectural gap without a new endpoint.
- Plot ingest needs the GitHub raw content accept header (`application/vnd.github.raw+json`) to get file content directly — without it, GitHub returns a JSON wrapper with base64-encoded content.
- Deregister = inactive, not deleted. This protects historical results and makes re-registration a one-click operation.
- clearanceLevel is already in useAuth state spread — no additional exposure needed.

---

## Build Notes

**Port allocation:**
```
4000  Gateway (external)
4001  Signal Clearance
4002  Policy
4003  Contract Test
4004  Plot Test
4005  Plot Store
4006  Signal Aggregator
4007  Feed Wr4ngler
4008  Results
4009  Interactions
4010  Augur Canis
4011  Observability
4012  (reserved)
4013  Integration
4014  cert-forge (constellation mTLS — /sign)
4015  cert-forge (enrollment mTLS — /instance-cert)
4016  cert-forge (plain HTTP — /ca)
4110  Lore
4252  AI-lien
```

**Language assignments:**
```
Go          — Gateway, Policy, Contract Test, Plot Test, Plot Store,
              Signal Aggregator, Interactions, Augur Canis, Observability,
              Integration, cert-forge, Lore
TypeScript  — Signal Clearance
Elixir      — Feed Wr4ngler
Python      — Results, AI-lien
React/TS    — UI (served by Go static file server)
```

**Key architectural decisions recorded:**
- Federated OIDC via customer AD chosen over passkeys — customer manages user lifecycle
- Signal Clearance uses clearance levels not roles — observability authorization is fundamentally different from feature authorization
- AI-lien is an internal Job not an external service — single caller, bounded demand, contract-enforced structured output
- Integration Job is read-only and versioned — customer tooling must not break on SETI upgrades
- UI is not a special case for feed access — it subscribes via Gateway exactly as any other caller
- Feed Wr4ngler built in Elixir — OTP supervision means crashed feed processes restart in isolation
- Redis channel is seti:events not tca:events — keeps SETI's own observability stream separate from monitored applications
- cert-forge is a persistent running service not an init container — PKI is infrastructure, not initialization
- Three-port cert-forge architecture is load-bearing — TLS client auth per-path is impossible on a single port
- Enrollment CA is separate from constellation CA — compromise of enrollment cert cannot forge constellation certs
- Signing delegation to cert-forge makes services crypto-agnostic — algorithm changes require only cert-forge changes

---

## Phase 8 — Intelligence Layer: Lore, bad-whiff buffer, and AI-lien memory

**Status:** In Progress
**Deliverable:** SETI has persistent institutional memory. Every Plot test run writes step results to the bad-whiff Redis buffer. AI-lien reads Lore baselines and incident history before analysis and writes assessments back. Results writes directly to Lore on self-evident escalations. The AI is no longer amnesiac.
**Rationale:** AI-lien operating without institutional memory is a first responder with no case history. Lore is the architectural gap that makes AI analysis meaningful over time rather than just per-incident.

### Jobs

| Job | Change | Status |
|-----|--------|--------|
| lore | New — Go, PostgreSQL, port 4110 | Complete |
| signal-aggregator | Bad-whiff Redis Stream writes | Pending |
| plot-test | Full contract schema overhaul + whiff writes | Pending |
| ai-lien | Lore read (baselines/incidents) + write (trend points/incidents) | Pending |
| results | Lore direct fast-path write | Pending |

### Architecture Decisions Recorded

- Three-tier storage: run-scoped Redis (capture) → sliding window Redis (bad-whiff) → PostgreSQL (Lore)
- Bad-whiff buffer: Redis Stream per application `seti:whiff:{application_id}`, MAXLEN 10,000 approximate trim. Full buffer is itself a signal.
- Every plot step result writes to bad-whiff regardless of pass/fail — AI-lien needs baseline data, not just anomaly data
- Promotion decision owned by AI-lien — Plot-test executes and records, AI-lien reasons about what to persist
- Results has a direct fast-path to Lore for self-evident escalations that don't require AI reasoning
- Lore baseline `established_by` locked to `ai-lien` only — human input flows through Corrections path, not direct baseline writes
- lib/pq chosen over pgx due to sandbox network constraints — pgx preferred for production (pure Go, better pooling via pgxpool, no golang.org/x/* dependency concerns in a full build environment)
- Port 4110 — intentional

### Lessons Learned
*Populated when phase completes.*

---

## Phase S1 — Supply Chain: All third-party library dependencies eliminated from Go Jobs

**Status:** Complete
**Deliverable:** Zero third-party Go library dependencies across the entire SETI constellation. All Go Jobs use stdlib only. go-redis replaced with a TCA stdlib Redis client in every Job. golang-jwt pending replacement in gateway and policy.
**Rationale:** Every external library is a supply chain vector. go-redis introduced transitive dependencies (bsm, cespare/xxhash, dgryski) with no operational benefit at SETI's scale. The TCA Redis client is stdlib-only, contract-backed, and each Job owns its copy — the three-year lifecycle makes it fully replaceable at the Job boundary.

### What Was Done

- Designed and implemented `TCA redis-client` library (`redis.go`) — stdlib `net` only, RESP2 wire protocol, full pub/sub + streams + pipeline support. Contract at `contracts/lib/redis-client.yaml`.
- Replaced go-redis in: `augur-canis`, `seti-observability`, `gateway`, `contract-test`, `policy`, `plot-test`, `signal-aggregator`, `healthcheck`.
- `signal-aggregator/redis.go` extended with `SubscribeMulti` — multiple-channel subscribe on a single connection, needed for SETI's own event stream subscriptions.
- `watchdog` removed entirely — superseded by augur-canis, which now owns the full health check lifecycle.
- `healthcheck` Dockerfile stage updated across all 14 Job Dockerfiles — `go mod download` and `go.sum` COPY removed from the embedded healthcheck build stage.
- Stale go-redis entries removed from `lore/go.sum`.
- `golang-jwt/jwt/v5` identified as third-party (community fork, not official Go team) — replacement with stdlib HMAC/SHA256 JWT deferred to Phase S2.

### Lessons Learned
- The TCA lib copy-per-Job model is correct. Each Job owns its redis.go, can extend it for its needs (SubscribeMulti), and the contract guarantees behavioral compatibility. The apparent redundancy is intentional isolation.
- go-redis brought three transitive dependencies whose sole function was hashing and test framework support — none of which SETI uses. Removing one library removed four.
- Supply chain work surfaces dead code. watchdog was carried through multiple phases because it built cleanly. The redis replacement exposed it as an orphan.

---

## Phase S2 — Supply Chain: golang-jwt replaced with stdlib JWT

**Status:** Complete
**Deliverable:** gateway and policy use stdlib `crypto/hmac`, `crypto/sha256`, and `encoding/base64` for HS256 JWT signing and verification. Zero third-party Go dependencies across the full constellation.
**Rationale:** golang-jwt/jwt is a community-maintained fork, not an official Go package. The name is misleading. HS256 over stdlib is 30 lines of code.

### Lessons Learned
- stdlib HS256 is straightforward — hmac.New(sha256.New, secret), base64url encode header.claims.sig. No surprises.
- golang-jwt's `RegisteredClaims` embedded struct was the only structural coupling. Replacing it with a plain `map[string]interface{}` for signing and direct field extraction after verify is cleaner.
- gateway only needs `wrangler_id` and `clearance_level` from tokens — it doesn't need to model the full claims shape of every issuer. The flat map approach makes this explicit.

---

## Phase 9 — Closure Loop: Interactions routing, AI-lien memory, Notifier

**Status:** In Progress
**Deliverable:** Failures generate intelligence. Interactions triages escalations across three paths — immediate alert, AI analysis, or silence — based on failure rate and Lore history. AI-lien reads baselines and incident history before analysis and writes assessments back. Critical failures page a human via Notifier while AI-lien assembles a briefing in parallel. The loop is closed.
**Rationale:** SETI with a closed loop is a monitoring system. SETI without it is a dashboard.

### Jobs

| Job | Change | Status |
|-----|--------|--------|
| notifier | New — Go, port 4300, permanent stub | Complete |
| interactions | Triage routing — three paths (immediate/AI/silence) | Pending |
| ai-lien | Lore read (baselines/incidents) + write (trend points/incidents) | Pending |
| results | Lore direct fast-path write for self-evident escalations | Pending |

### Architecture Decisions Recorded

- Interactions is the sole authorized caller of Notifier — AI-lien never calls Notifier directly. Every human notification is traceable to an Interactions decision with a log entry.
- Notifier is a permanent stub in the open source distribution — `stub_active: true` in health response is detectable signal that no humans will be paged. Any operator deploying to production must implement the handler body.
- The notification prompt is deliberately minimal (500 char max) — sensitive incident context travels through secure channels, not through the pager payload. The human follows the alert into SETI for the full picture.
- Three escalation paths in Interactions: (1) immediate — failure rate exceeds threshold, page human + AI briefing in parallel; (2) AI analysis — partial failures, package context and send to AI-lien, write assessment to Lore; (3) silence — single intermittent failure with clean Lore history, write to bad-whiff buffer only.
- AI-lien has two analysis modes: normal ("assess severity, recommend action") and critical briefing ("this is bad, build the fullest possible context package for the human responding now").
- AI-lien returns escalation recommendations to Interactions — Interactions decides whether to call Notifier. AI-lien never calls Notifier directly.
- Show Wr4ngler is the next testing contributor — contract and plot tests cover the inside of the system; Show Wr4ngler covers user perspective, real-world usage patterns, and adversarial scenarios that cannot be derived from contracts.

### Lessons Learned
*Populated when phase completes.*

---

## Phase 10 — Kubernetes Migration: TCA 2.0 deployment architecture

**Status:** Complete
**Deliverable:** The full SETI constellation runs on k3d (local K8s) with the same operational behavior as Docker Compose. A Helm chart is the authoritative constellation definition. cert-forge writes cert material to K8s Secrets. NetworkPolicy defines the communication topology. The dev/prod gap is eliminated.
**Rationale:** Docker Compose is an approximation of the target architecture. It has a 32-network ceiling that forced the ac-net compromise. K8s removes that ceiling and provides the correct primitives — Secrets, NetworkPolicy, init containers, readiness probes — for everything that was being approximated.

**Completed:** 2026-04-14 (approximately 4 hours, engineer-AI partnership)

### What Was Built

**cert-forge K8s Secret integration** (`cert-forge/k8s.go`)
cert-forge detects its environment via the projected ServiceAccount token. In K8s it writes all cert material — CA cert, enrollment CA cert, enrollment cert+key, star-gazer cert+key — to a K8s Secret via the K8s REST API using stdlib `net/http` only. No client-go, no external dependencies, scratch container stays. In Docker Compose it writes to the volume as before. Detection is automatic — same image, same binary, different behavior based on environment.

**Helm chart** (`charts/seti/`)
Single chart covering the full 20-container constellation. Environment-specific behavior via values files — `values/dev.yaml` for k3d local development. One chart deploys to dev, staging, and production.

| Resource type | Count |
|--------------|-------|
| Deployments | 17 |
| StatefulSet | 1 (postgres) |
| Services | 18 |
| ConfigMaps | 2 (contracts, plots) |
| Secrets | 3 (seti-certs, postgres credentials, remote-apps) |
| ServiceAccount + Role + RoleBinding | 1 set (cert-forge) |
| NetworkPolicy | 22 |
| Namespace | 1 |

**Startup ordering** (`charts/seti/templates/_init.tpl`)
Init containers replace `depends_on: condition: service_healthy`. Reusable wait helpers per dependency. K8s holds pods in `Init:` state until all dependencies are ready. Startup is clean, ordered, and observable.

**AC health verification**
Every service uses `exec: ["/healthcheck"]` for readiness and liveness probes — the same healthcheck binary, the same Redis pub/sub mechanism, the same AC Watchdog verification. cert-forge uses `httpGet` on `/ca` as the sole exception.

**NetworkPolicy** (`charts/seti/templates/network-policies/policies.yaml`)
22 policies. Default-deny ingress for all pods. Explicit allow rules per service derived from the actual call matrix. Policies are in the chart regardless of environment; enforced automatically by the CNI in staging and production.

**Scripts** (`scripts/`)
- `k3d-setup.sh` — creates the `seti` cluster, local registry, port mapping
- `build-push.sh` — builds and pushes all 18 service images

### Deployment Tiers (TCA 2.0)

| Tier | Tool | When | Notes |
|------|------|------|-------|
| 0 — Solo Job | Docker Compose (partial stack) | Active Job development | Fast inner loop, no K8s overhead |
| 1 — Dev Constellation | k3d + Helm | Full constellation testing | `helm upgrade`, `k9s` for observability |
| 2 — Staging | k3s or managed K8s + ArgoCD | Pre-production validation | NetworkPolicy enforced, ArgoCD drift detection |
| 3 — Production | k3s (street) or managed K8s + ArgoCD | Live | Same chart, different values |

### Architecture Decisions Recorded

- Docker Compose is Tier 0, not deprecated. Helm chart is the source of truth for the full constellation.
- cert-forge writes to K8s Secret, not PVC. Narrow-scoped ServiceAccount with `get`, `create`, `update` on secrets only.
- All `.env` and volume-based secrets become K8s Secrets. Supplied via `--set` at install time. Never in values files.
- `helm install` requires `-n <namespace>` explicitly. `--create-namespace` alone is insufficient.
- k3d over Docker Desktop built-in K8s. Named, independent, multi-node clusters. Coexists with Docker Compose without interference. Developers don't toggle Docker Desktop modes between projects.
- NodePort 30400 + k3d port mapping. `-p "4000:30400@loadbalancer"` at cluster creation. Same `localhost:4000` URL as Docker Compose.
- ArgoCD for staging and production, not dev. `helm upgrade` is the dev workflow.
- NetworkPolicy in chart, enforcement deferred to staging. Same chart, no changes required.
- Startup contract test failures during fresh install are expected and correct. Degraded-healthy fallback is not a bug.
- ac-net compromise is resolved. NetworkPolicy gives true point-to-point isolation between augur-canis and each service.

### Lessons Learned
- The shared volume cert approach and ac-net compromise were both Docker Compose artifacts, not architectural choices. K8s removes both cleanly.
- `helm install` without `-n <namespace>` deploys to `default` regardless of template namespace declarations. Always specify `-n` explicitly.
- ConfigMaps from contract files are cleaner than volume mounts for read-only config. 440KB fits well under the 1MB limit.
- Init containers are the correct K8s equivalent of `depends_on: condition: service_healthy`. TCP check (`nc`) confirms port availability; AC handles health verification once the service is running.
- `exec: ["/healthcheck"]` is the correct probe for all mTLS services. `httpGet` with `scheme: HTTPS` fails the handshake without a client cert. `tcpSocket` bypasses AC entirely.
- k3d image pull uses `seti-registry:5000` (in-cluster DNS), not `localhost:5000` (host). Push and pull addresses differ. Must be in `values/dev.yaml`.
- A 20-service polyglot constellation migrated from Docker Compose to K8s in approximately 4 hours via engineer-AI partnership. Traditional team estimate: 2-6 weeks.

### Additional Lessons Learned (Post-Delivery — 2026-04-14)

- k3d enforces NetworkPolicy via its bundled controller. The assumption that flannel does not enforce NetworkPolicy is wrong. Policies are active in dev immediately on application.
- Redis and augur-canis ingress policies must use the `app.kubernetes.io/part-of: seti` label selector. Enumerating callers is incomplete — init containers create transient access patterns that runtime caller lists don't capture.
- Services that are universal dependencies (cert-forge, redis, augur-canis, seti-observability) require the label selector approach. Enumerating callers for universal dependencies is always wrong.
- Self-registration has 10 retry attempts. If NetworkPolicy is wrong at startup, services exhaust retries, give up, and run unregistered. AC marks them `skip: job_not_deployed`. Correct NetworkPolicy from the start is the fix, not more retries.
- cert-forge restart without CA persistence invalidates the constellation. CA cert and key are now persisted in the K8s Secret. cert-forge loads the existing CA on restart rather than generating a new one.
- `helm upgrade` does not restart pods on ConfigMap changes. Services that read config at startup require `kubectl rollout restart` to pick up updated ConfigMap content.
- PlotStep `expected_status` is `int` in the Go struct. A JSON array breaks parsing and silently drops the plot.
- Plot tests must clean up persistent state they create. Docker Compose's reset behavior masked this assumption. K8s does not reset in-memory state between runs.

---

## Phase S3 — Supply Chain: Secrets, PostgreSQL X.509, and Graceful Shutdown

**Status:** Complete
**Completed:** 2026-04-21
**Deliverable:** helm install/upgrade requires no --set flags. PostgreSQL authenticates lore via X.509 client certificate — no application password. All 20 Jobs handle SIGTERM gracefully with a documented shutdown sequence. Third-party Go and Python dependencies eliminated from remaining Jobs.

### What Was Built

**Secret elimination:**
- cert-forge generates `postgres-password` and `jwt-secret` on first startup, persists both in the `seti-certs` K8s Secret alongside cert material. Loads existing values on restart — stable across cert-forge restarts.
- gateway, policy, signal-clearance read JWT secret from `/certs/jwt-secret` file path. No `--set jwtSecret=` required.
- postgres superuser uses `POSTGRES_PASSWORD_FILE=/certs/postgres-password`. No `--set postgresCredentials.password=` required.

**PostgreSQL X.509 client certificate authentication:**
- cert-forge issues two new static certs: `postgres-server` (SANs: postgres, localhost) and `lore-db` (CN=lore-db).
- `pg_hba.conf` (ConfigMap): hostssl cert clientcert=verify-full with pg_ident.conf mapping CN=lore-db → lore role.
- `postgresql.conf` (ConfigMap): ssl=on, listen_addresses=*, config files mounted from ConfigMap.
- `init.sql` (ConfigMap): creates `lore` role (LOGIN, no password) and `lore` database, grants schema ownership.
- lore connects via `sslcert=/certs/lore-db.crt&sslkey=/certs/lore-db.key&sslrootcert=/certs/ca.crt`. No password anywhere in the connection string.
- postgres superuser retains cert-forge-generated password for emergency DBA access only — never used by any application.
- Security scanner finding on application password auth: eliminated.

**CA rotation fix:**
- forge.json now lists all 18 SETI deployments in the rotation list.
- cert-forge detects fresh CA generation on startup and immediately rolls all deployments after writing the Secret — no more stale trust pools after manual `kubectl delete secret seti-certs`.

**Graceful shutdown:**
- `x-tca-lifecycle` block added to all 18 contracts — Job-specific shutdown steps.
- Section 8 added to tca-guidelines.md — language patterns for Go, TypeScript, Python, Elixir.
- `shutdown.go` added to all 13 Go Jobs — separate file per the infrastructure-not-domain principle.
- `shutdown.ts` added to signal-clearance, `shutdown.py` to ai-lien and results.
- feed-wrangler: `terminate/2` added to EventSubscriber, `stop/1` added to Application.
- `TCA Redis client Close()` method added to all redis.go copies.
- `terminationGracePeriodSeconds: 20` added to all Deployments, 30 to postgres StatefulSet.
- Pods now terminate with `Completed` (exit 0) instead of `Error`.

**Supply chain cleanup:**
- `results`: dead `redis==5.0.3` dependency removed (was never imported). Dockerfile simplified to single stage.
- `ai-lien`: `requests` replaced with stdlib `urllib`. Dockerfile simplified to single stage.
- `signal-clearance`: `uuid` replaced with `crypto.randomUUID()` (Node 20 builtin). `redis` npm package replaced with TCA stdlib RESP2 client (`redis.ts`) — connection-per-command pattern over `net.Socket`.

### Remaining Supply Chain Items
- `signal-clearance`: `express`, `cookie-parser` (Node.js HTTP routing — significant rewrite)
- `signal-clearance`: `jose` (OIDC JWKS RS256 verification — no stdlib alternative)
- `feed-wrangler`: `redix`, `jason`, `plug_cowboy` (Elixir ecosystem equivalents of stdlib — low priority)
- `lore`: `lib/pq` (PostgreSQL driver — no stdlib alternative exists for database connectivity)

### Lessons Learned
- Security scanners flag application-layer passwords regardless of how they are generated or stored. X.509 client certificate auth eliminates the finding entirely at the architectural level.
- PostgreSQL unix socket trust auth is the clean bootstrap mechanism — init.sql runs via unix socket with no credentials, avoiding the MongoDB localhost exception complexity entirely.
- `listen_addresses = '*'` must be explicit in custom postgresql.conf — PostgreSQL defaults to localhost-only when a custom config file is provided without this setting.
- `pg_ident.conf` maps cert CN to database role — allows cert name (`lore-db`) to differ from the role name (`lore`) without changing either.
- `fsGroup: 70` + `defaultMode: 0640` on the postgres pod is required for PostgreSQL to read the key file — postgres refuses world-readable private keys.
- `defaultMode: 0600` on the lore deployment certs volume is required for the PostgreSQL client to accept the key file — same check, client side.
- cert-forge's CA write to K8s Secret and the kubelet syncing that Secret to pod volumes are not synchronous. The `waitForPostgresPassword` init container polling the file directly is more reliable than polling the HTTP endpoint.
- Graceful shutdown is infrastructure, not domain logic. `shutdown.go` alongside `redis.go` and `jwt.go` — same copy-per-Job pattern, same reasoning.
- Dead dependencies (`redis==5.0.3` in results) survive undetected when requirements.txt is not audited against actual imports. Audit imports, not just declared dependencies.
- Node.js `crypto.randomUUID()` is a global in Node 20 — no import required. ES2022 target with dom lib makes it available to TypeScript without explicit typing.
- Connection-per-command in the TypeScript RESP2 client is correct for low-frequency session operations. The Go mutex-and-persistent-connection model is correct for high-frequency pub/sub. Pattern choice is driven by usage, not by what the library did.

---

## Phase S4 — Supply Chain: TypeScript stdlib complete, TCA lib contracts established

**Status:** Complete
**Completed:** 2026-04-22

### What Was Built

**signal-clearance — zero runtime npm dependencies achieved:**
- `jose` replaced with `jwks.ts` — TCA JWKS client using `crypto.subtle` + stdlib `https`. RS256 JWT verification, JWKS caching with per-pod in-memory default and pluggable shared cache adapter for horizontal scale.
- `uuid` replaced with `crypto.randomUUID()` — Node 20 builtin global, no import needed.
- `redis` npm package replaced with `redis.ts` — TCA RESP2 client using `net.Socket`. Connection-per-command pattern for low-frequency session operations.
- `express` + `cookie-parser` replaced with `router.ts` — TCA HTTP router. App and Router classes with identical (req, res) interface to express. Method routing, URL parameter extraction, JSON body parsing, cookie parsing/setting, sub-router mounting. All 18 route handler bodies unchanged.

**signal-clearance runtime dependencies after Phase S4: zero.**
Build-time only: `@types/node`, `typescript`.

**TCA lib contracts created:**
- `contracts/lib/jwks-client.yaml` — JWKS verification lib. Documents two-tier caching model (per-pod in-memory default, shared Redis adapter for scale), rate limiting, key rotation handling, algorithm constraint (RS256 only), production checklist.
- `contracts/lib/router.yaml` — HTTP routing lib. Documents App/Router classes, TCARequest/TCAResponse interfaces, CookieOptions, all methods with signatures and error behavior.
- `contracts/lib/tca-lib-index.yaml` — updated with both new entries.

**Supply chain fix — router.use() bug:**
- No-prefix sub-router mount (`app.use(router)`) was combining an empty prefix pattern with route patterns incorrectly, breaking all sub-router routes. Fixed to copy routes directly when no prefix is provided.

### Remaining Supply Chain Items

| Job | Dependency | Status |
|-----|-----------|--------|
| lore | lib/pq | No Go stdlib PostgreSQL driver — stays until Go ships one |
| signal-clearance | — | Zero runtime dependencies ✓ |
| feed-wrangler | redix | Replaceable with stdlib RESP2 client in Elixir |
| feed-wrangler | jason | No Elixir stdlib JSON until OTP 27+ — stays |
| feed-wrangler | plug_cowboy | No Elixir stdlib HTTP server — stays |
| ui | react, vite, etc. | Build-time only — never executes in production ✓ |

**Next target: `redix` in feed-wrangler** — same RESP2 pattern, third language implementation.

### Lessons Learned
- TypeScript `body: any` on TCARequest is correct — handlers were written against express's permissive any-typed body. Auditing every destructure for strict typing is a separate pass.
- `new Router()` not `Router()` — class instantiation requires new. Express's factory function pattern masked this for years.
- Empty-prefix sub-router mounting needs special handling — combining an empty pattern with a route pattern adds an extra path separator that breaks route matching.
- Copy-per-Job applies to lib files in every language. `jwks.ts`, `redis.ts`, `router.ts` — each Job that uses them owns its copy.
- `crypto.subtle.importKey()` with `extractable: true` is required for Tier 2 cache adapters that need to re-export keys for Redis storage. Set it even in the default Tier 1 path for symmetry.
- The TCA lib index is the supply chain gate. An AI encountering an unknown import checks the index first. If nothing covers it, it surfaces the gap rather than pulling a package.
