# SETI — Search for Erroneous Tessellated Interactions
## System Reference Document

**Status:** Living Document  
**Last Updated:** 2026-04-09  
**Audience:** AI partners and engineers operating, extending, or reasoning about SETI  
**Scope:** Architecture, operations, observability, AI integration, and known limitations

---

## 1. What SETI Is

SETI is a TCA constellation-level monitoring application. It watches other TCA constellations — running Contract Tests against their contracts, executing AI-generated Plot Tests against their behavior, correlating their observability feeds, and routing failures through an AI diagnostic pipeline.

SETI is also the proof of concept for TCA's self-monitoring capability. It monitors itself. Every Job in the SETI constellation is registered in SETI's own Policy Job and subject to the same testing regime SETI applies to any other application. If SETI cannot monitor itself correctly, it cannot monitor anything else correctly.

**SETI is not a passive log aggregator.** It is an active testing system with opinions about what constitutes correct behavior, a clearance model that controls who sees what, and an AI analysis layer that reasons about failures rather than just recording them.

---

## 2. The Constellation

### Jobs

| Job | Language | Port | Role |
|-----|----------|------|------|
| gateway | Go | 4000 | Single external entry point. TLS termination, JWT validation, routing, response header forwarding. |
| signal-clearance | TypeScript | 4001 | Authentication and clearance level assignment. Issues JWTs and refresh tokens. Sets httpOnly seti_refresh cookie. |
| policy | Go | 4002 | Application registry, test schedules, failure response policies. Source of truth for what SETI monitors and when. Auto-registers seti-self on startup. |
| contract-test | Go | 4003 | Reads contracts, generates test cases, routes through AC via Redis, stores results. Scheduler polls policy every 60s. |
| plot-test | Go | 4004 | Loads AI-generated Plots, executes step sequences, verifies call chains via Signal Aggregator. Daily scheduler at 02:00 UTC. |
| plot-store | Go | 4005 | Stores and serves Plot definitions. Flags Plots for regeneration when contracts change. |
| signal-aggregator | Go | 4006 | Subscribes to application observability streams. Provides call chain verification for Plot Tests. |
| feed-wrangler | Elixir | 4007 | Per-Wr4ngler scoped signal delivery. OTP supervision — crashed feeds restart in isolation. |
| results | Python | 4008 | Stores and serves contract test and plot test results. |
| interactions | Go | 4009 | Failure routing hub. Receives failure events and routes to AI-lien, notification channels, or human escalation. |
| augur-canis | Go | 4010 | Behavioral health verification. Executes canned queries against all Jobs. Redis pub/sub health feed. |
| seti-observability | Go | 4011 | Receives inter-service event reports from all Jobs. Sanitizes and publishes to Redis seti:events. |
| integration | Go | 4013 | Versioned read-only API for customer tooling. |
| cert-forge | Go | 4014/4015/4016 | PKI abstraction layer. Three-port design — see Section 5. |
| ai-lien | Python | 4252 | AI diagnostic engine. Double-tap Ollama query pattern. |
| ui | React/TS + Go | 4020 | Browser interface. Live event feed, contract results, plot management, admin panel. |

### Infrastructure

| Component | Role |
|-----------|------|
| Redis | Session store, pub/sub backbone (seti:events, tca:augur-canis, tca:check-requests, tca:check-results, tca:contract-requests, tca:contract-results) |
| cert-forge | PKI — generates CA, issues instance certs via enrollment mTLS, holds private keys in memory only |

### Observability Channel

SETI uses `seti:events` — not `tca:events`. SETI's own inter-service traffic stays separate from monitored application traffic. The dashboard shows `seti:events`.

---

## 3. Clearance Levels

| Level | Who | What they see |
|-------|-----|---------------|
| sec-wr4ngler | Security engineers | Full feed, all events, AC alerts, federation status |
| ops-wr4ngler | Operations | Application health, contract results, plot results |
| dev-wr4ngler | Developers | Their application's events only, their test results |

Clearance source is federated AD group membership. In dev mode (`AUTH_MODE=dev`) clearance is self-selected at the dev login screen.

---

## 4. Session and Authentication

### Token Model

**JWT (access token)** — 15-minute TTL. Self-contained, signed, stateless. Contains wrangler_id, clearance_level, identity_source, issued-at, expiry. Cannot be revoked before expiry.

**Refresh token** — 7-day TTL. Opaque random string stored in Redis. Delivered as an httpOnly `seti_refresh` cookie through the gateway. Rotates on every use.

### Session Behavior — Inactivity Timeout

The 15-minute JWT TTL is an **inactivity timeout**, not a fixed wall clock limit from login. Every authenticated API call invokes `getFreshJWT()` which unconditionally calls the refresh endpoint. If the user does something at minute 10 and returns at minute 17, the session is still valid — the minute-10 action refreshed the token, resetting the clock to minute 25.

Inactivity for more than 15 minutes causes the JWT to expire. The next interaction attempts a refresh — if the refresh token cookie is still valid (within 7 days), the session recovers transparently. If not, the user is redirected to login.

**Critical implementation note:** `getFreshJWT()` must refresh unconditionally on every call. Any "only refresh if near expiry" check defeats the inactivity timeout model entirely. A user active at minute 10 with a remaining TTL of 5 minutes would not be refreshed, and would be logged out at minute 15 despite being active.

### Cookie Scope

The `seti_refresh` httpOnly cookie is issued through the gateway (port 4000) because all client traffic routes through it. The gateway must forward `Set-Cookie` response headers from signal-clearance to the browser — a proxy that reads only the response body discards the cookie silently. The cookie is scoped to localhost:4000 and is sent by the browser on every request to that origin.

### What Observability Sees and Doesn't See

**Visible:**
- `gateway → signal-clearance POST /auth/refresh 200` — successful session extension
- `gateway → signal-clearance POST /auth/refresh 400` — failed refresh (expired or invalid token)
- `gateway → signal-clearance POST /auth/dev/login 200` — re-authentication after timeout

**Not visible:**
- JWT expiry detection (browser JavaScript)
- Redirect to login page (browser navigation)
- The gap between a failed refresh and re-login

**Operational implication:** `POST /auth/refresh 400` followed by silence then a fresh login is probably a normal timeout. The same pattern with a short gap, unexpected IP, or no subsequent login has different significance. The feed contains signals but not interpretation. This is an AI reasoning problem — see Section 8.

---

## 5. PKI — cert-forge

### Architecture

cert-forge is a persistent running service. It generates the constellation CA, issues instance certs on demand, and holds all private key material in memory only. Services never perform cryptographic operations themselves and never see other services' key material.

**Three-port design:**
- Port 4016 — plain HTTP, `/ca` only. CA cert is public.
- Port 4015 — enrollment mTLS, `/instance-cert` only. Requires enrollment cert.
- Port 4014 — constellation mTLS, `/sign` only. Requires instance cert.

**Enrollment CA** — separate from the constellation CA. cert-forge generates one enrollment cert on startup, written to the certs volume. Injected into every container via environment variables. Its only capability: call `/instance-cert`.

**Key material on shared volume:** only `ca.crt` and the enrollment cert/key. No service private keys ever touch the volume.

**Certforge client implementations:** Go (`augur-canis/certforge.go` — canonical reference), TypeScript (`signal-clearance/src/certforge.ts`), Python (`results/certforge.py`, `ai-lien/certforge.py`), Elixir (`feed-wrangler/lib/feed_wrangler/cert_forge.ex`).

**In K8s production:** cert-forge's issuance backend points to cert-manager or the company CA. Signing responsibility stays with cert-forge. No other service changes.

---

## 6. Contract Testing

### Mechanics

1. Policy auto-registers `seti-self` on startup (contracts at `/contracts/openapi`, gateway at `https://gateway:4000`, 15-minute interval)
2. Contract Test scheduler polls Policy `/applications` every 60 seconds
3. At the configured interval, Contract Test reads all YAML files from the contracts path
4. `serviceNameFromTitle()` derives the service name from the contract title
5. Test cases generated for all 2xx GET endpoints (Phase 2 scope)
6. Published to Redis `tca:contract-requests`
7. AC executes HTTP calls via point-to-point mTLS networks, publishes results to `tca:contract-results`
8. Contract Test assembles run, POSTs 201 to Results Job

### serviceNameFromTitle() Transform

Strips `"SETI - "` prefix → strips parenthetical suffixes → strips trailing `" Job"` → lowercase → spaces to hyphens.

**The contract title is authoritative.** The derived name must exactly match the service's `service_name` in its self-registration payload. Display name choices (capitalisation, numeronyms like Wr4ngler vs Wrangler) must not diverge from the technical identifier.

### Skip Reasons

| Reason | Meaning | Common Cause |
|--------|---------|--------------|
| `job_not_deployed` | No AC registration for this service name | Contract title produces a different string than service's self-registration name |
| `job_not_reachable` | Registration found, HTTP call failed | Wrong port in service's self-registration `network_endpoint` — must match actual listening port |

### Current Results (Phase 6 — 2026-04-09)

20/20 passing, 0 failed, 0 skipped on all SETI Jobs. Scheduler running on 15-minute interval. PlotTestEnabled: true for seti-self.

---

## 7. Plot Testing

### What Plots Are

AI-generated test scenarios — sequences of steps exercising realistic interaction patterns. Where contract tests verify status codes, Plot tests verify system behavior: correct inter-service call sequences, correct data flow, correct failure handling.

### Plot Generation (Current State)

Plots are generated by an AI partner reading contracts and reasoning about realistic scenarios. A well-formed Plot targets a specific Job, exercises a realistic scenario (not just health endpoints), specifies expected call chain verification, and has a clear pass/fail criterion requiring no human judgment.

**Current state:** Plot generation and Plot Test execution are not yet fully verified end-to-end. This is the next active workstream.

### Scheduler

Plot tests run daily at 02:00 UTC for applications with `plot_test_enabled: true`. Scheduler watches Policy every 60 seconds.

---

## 8. AI Integration — Current and Required

### Current: AI-lien

Diagnostic engine for test failures. When Interactions routes a failure with policy action `escalate`, AI-lien applies the double-tap pattern:
- First tap: conservative — describe failure, identify likely causes, suggest investigation steps
- Second tap: deeper — given first assessment, what specific remediation is recommended

AI-lien fetches active provider and model from Policy before every analysis. No local config.

### Required: AI Reasoning on the Feed

Several event classes require AI reasoning that pure algorithms cannot provide:

**Session anomaly detection** — `POST /auth/refresh 400` followed by silence then fresh login is probably a normal timeout. With a short gap or unexpected source it is not. A pure algorithm cannot distinguish these reliably.

**Pattern correlation across time** — a service failing after three weeks of clean passes is more significant than one that has been intermittently failing. Significance requires memory of prior state.

**Deployment vs incident** — `job_not_reachable` during a deployment window is normal. Outside it, not. Requires context the feed alone doesn't provide.

### Required: AI Memory Store

**This is a need-to-pursue, not a nice-to-have.**

AI-lien operates from cold start on every invocation. It has no memory of prior failures, baseline profiles, resolved incidents, or learned patterns. Without persistent memory, the AI is always a first responder with no case history.

**What the memory store holds** (not raw events — those are in Redis and Results):
- **Baseline profiles** — what does normal behavior look like per Job? Latency distributions, call frequencies, typical failure rates.
- **Resolved incidents** — what happened, what AI assessed, what was correct, what was wrong.
- **Learned patterns** — recurring event sequences that have been assigned meaning through prior analysis.
- **Wr4ngler corrections** — when a Wr4ngler overrides an AI assessment, that correction feeds back into future reasoning.

Schema must be designed around what the AI needs to recall, not what happened. Vector embeddings of raw events are insufficient — the AI needs structured facts it can reason about.

This sits in the grey area between pure algorithms and human judgment — too subtle for rules-based detection, too high-volume for human review, well-suited for AI reasoning with institutional memory.

---

## 9. Federation

SETI federates with external TCA constellations by subscribing to their AC health feeds and verifying events against session certificates.

**star-gazer** is SETI's federation identity — static cert/key on the shared volume with a longer lifecycle than instance certs.

Registration flow: Sec Wr4ngler registers application with `ac_endpoint` → SETI sends signed registration request → external AC verifies star-gazer cert → AC returns session certificate → Signal Aggregator subscribes to external `tca:augur-canis` channel.

Session cert rotation is automatic — Signal Aggregator detects signature verification failures and re-registers.

---

## 10. Operational Patterns

### Starting SETI

```bash
docker compose down -v   # destroy volume — cert-forge regenerates CA
docker compose up --build
```

cert-forge starts first (`depends_on: service_healthy` for all other services). All services call cert-forge's enrollment server to obtain instance certs before starting.

### Interpreting the Event Feed

**Scheduler traffic:** `contract-test → policy GET /applications` every 60 seconds is the sync loop — not a test run. Normal background traffic.

**Test run signature:** Burst of AC calls followed by `contract-test → results POST /contract-results 201`. Click the results event to expand individual test outcomes.

**Auth traffic:** `gateway → signal-clearance POST /auth/refresh 200` appearing regularly is session extension working correctly. 400 on this endpoint indicates a failed refresh — check whether the seti_refresh cookie is present in browser dev tools.

**Check traffic:** `augur-canis → {service} GET /health` and related calls are behavioral health checks. Use "Hide Checks" to filter when watching application-level traffic.

### What the Feed Cannot Tell You

- Why a browser session ended (client-side, no service call)
- Whether a pattern is anomalous relative to historical behavior
- The significance of a silence
- Whether a sequence represents a security incident or normal operation

These are explicitly the domain of AI reasoning with persistent memory — not bugs in the observability model.

---

## 11. Known Gaps and Next Workstream

### Immediate Next: Plot Generation and Verification

Phase 6 remaining work:
- Generate AI Plots for SETI's Jobs and submit to Plot Store
- Verify Plot Test end-to-end execution
- Trigger intentional contract test failure and verify AI-lien routing
- Update tca-guidelines.md with Phase 6 learnings (done — 2026-04-09)
- Mark Phase 6 complete in PHASE-PLAN.md

### Active Gaps

**AI memory store** — not built. Most significant architectural gap. See Section 8.

**Plot generation standard** — not yet formalized. Plots need to be generated and verified before a repeatable standard can be documented.

**Negative contract testing** — Phase 3 work. Phase 2 covers 2xx GETs only. Phase 3 adds schema-derived POST/PUT bodies and crafted 4xx inputs.

**AC point-to-point networks** — Docker Compose network limit prevents full isolation. Shared AC network in current deployment. Full isolation requires Swarm or K8s.

**Automated remediation** — alert-only for now. Automated response requires human-in-the-loop design review.

### TCA 2.0 Candidates

- cert-forge as standard TCA component (PKI abstraction for any constellation)
- AI memory store architecture (generalizable beyond SETI)
- Swarm as first-class deployment tier (between Compose and K8s)
- K8s Helm chart generation from contracts
