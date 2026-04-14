# tca-seti — Phase Plan

**Status:** In Progress
**Last Updated:** 2026-04-12
**Next Action:** Phase 9 — Interactions triage routing (three paths), AI-lien Lore integration, Results fast-path write. Notifier is deployed and healthy.

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
