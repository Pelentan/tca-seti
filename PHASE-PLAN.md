# tca-seti — Phase Plan

**Status:** In Progress
**Last Updated:** 2026-04-01
**Next Action:** docker compose down -v && docker compose up --build. Navigate to /admin (sec-wrangler clearance), add Ollama provider URL, refresh models, select active model. Then trigger a failing plot test to exercise the full AI-lien pipeline. into tca-seti/ root. Then: docker compose up --build. No other steps — .env is included with dev credentials. Navigate to https://localhost:4000/dev-login to sign in, pick a clearance level, and watch the observability event stream. The Augur Canis (AC) stub responds to all health checks via Redis pub/sub — no plain HTTP ports open on any container. Silence detection is active — AC will bark on tca:augur-canis:alerts if a Job stops reporting.

---

## Phase 1 — See the Feed: Live observability dashboard accessible to authenticated Wr4nglers

**Status:** Pending
**Deliverable:** A Wr4ngler can log in via federated AD, provision a signal feed, and watch SETI's own inter-service events appear in the dashboard in real time. The constellation is observable from the moment it runs.
**Rationale:** SETI is the application that watches other applications. If SETI cannot watch itself, the core mechanism is unproven. Observability first — every subsequent Job reports to it from the moment it runs. The feed before the data — see the system work before building what generates the data.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| gateway | Go | 4000 | Pending |
| seti-observability | Go | 4011 | Pending |
| signal-clearance | TypeScript | 4001 | Pending |
| ui | TypeScript/React + Go | 4000 (via gateway) | Pending |

### Phase 1 Completion Criteria
- [ ] `docker compose up` produces a running system with no errors
- [ ] Wr4ngler can reach the login page in a browser
- [ ] Federated AD login completes and issues a SETI JWT
- [ ] Wr4ngler can provision a signal feed via the Gateway
- [ ] Wr4ngler can connect to the SSE stream and see events
- [ ] SETI's own inter-service calls appear in the observability dashboard
- [ ] cert-init generates all Phase 1 certificates cleanly on startup

### Lessons Learned
*Populated when phase completes.*

---

## Phase 2 — Core Testing: SETI can run Contract Tests against a registered application and store results

**Status:** COMPLETE
**Deliverable:** Register a TCA application with SETI. SETI reads its contracts, generates test cases, executes them, and stores results. A Wr4ngler can see the results in their feed.
**Rationale:** This is SETI's primary value proposition. Everything else is infrastructure around this capability. Policy owns the application registry and schedule. Contract Test reads contracts and runs tests. Results stores outcomes. These three Jobs together deliver the first complete testing loop.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| policy | Go | 4002 | Complete |
| contract-test | Go | 4003 | Complete |
| results | Python | 4008 | Complete |

### Phase 2 Completion Criteria
- [ ] Register tca-seti itself as the first monitored application
- [ ] Contract Test Job reads tca-seti's own contracts from the repository
- [ ] Contract Test run executes and produces pass/fail results
- [ ] Results stored in Results Job database
- [ ] Results appear in the Wr4ngler's feed via the Phase 1 delivery layer
- [ ] SETI is running Contract Tests against itself

### Lessons Learned
*Populated when phase completes.*

---

## Phase 3 — Deep Testing: Plot Test execution with call chain verification

**Status:** COMPLETE
**Deliverable:** AI-generated Plots execute against a registered application. Call chains are verified against the Signal Aggregator event stream. Failures route to the Interactions stub.
**Rationale:** Call chain verification requires the Signal Aggregator to be subscribed to application event streams. Feed Wr4ngler enables scoped signal delivery beyond what Phase 1's basic feed provides. Interactions closes the failure response loop even in stub form.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| plot-store | Go | 4005 | Complete |
| plot-test | Go | 4004 | Complete |
| signal-aggregator | Go | 4006 | Complete |
| feed-wrangler | Elixir | 4007 | Complete |
| interactions | Go | 4009 (stub) | Complete |

### Phase 3 Completion Criteria
- [ ] Plot Store accepts and stores AI-generated Plots
- [ ] Plot Test Job loads Plots and executes steps against target application
- [ ] Signal Aggregator subscribes to tca-seti's own seti:events stream
- [ ] Call chain verification confirms inter-service calls occurred
- [ ] Plot test failures route to Interactions stub with visible stub logging
- [ ] Feed Wr4ngler provisions scoped channels correctly
- [ ] First full Plot test run against tca-seti completes with results in feed

### Lessons Learned
*Populated when phase completes.*

---

## Phase 4 — Intelligence and External Access: AI-powered analysis and customer tooling integration

**Status:** COMPLETE
**Deliverable:** Failed tests trigger AI-lien analysis via the Interactions Job. Customer tooling can query SETI results via the Integration Job's versioned API.
**Rationale:** AI-lien requires a running failure path to analyze — Phase 3 must be complete before AI-lien has anything to reason about. Integration requires a populated Results database. Both depend on the core testing loop from Phases 2 and 3.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| ai-lien | Python | 4252 | Complete |
| integration | Go | 4013 | Complete |
| watchdog | Go | 4010 (n/a — implemented as Augur Canis) | Complete |

### Phase 4 Completion Criteria
- [ ] AI-lien container running with Ollama instance loaded
- [ ] Test failure triggers Interactions Job AI diagnostic path
- [ ] AI-lien double-tap pattern executes and returns structured assessment
- [ ] First-tap prompt preserved in Results Job audit trail
- [ ] Integration Job `/v1/applications` returns registered applications
- [ ] Customer tooling (Grafana test query) successfully pulls SETI data
- [ ] Watchdog stub running with visible stub logging on triggered analysis

### Lessons Learned
*Populated when phase completes.*

---

## Phase 5 — Self-Registration: SETI monitors itself completely

**Status:** Pending
**Deliverable:** SETI is fully registered in its own Policy Job, running its own Contract Tests and Plot Tests on schedule, routing its own failures through its own Interactions Job, and visible in its own feed. The methodology tests itself.
**Rationale:** SETI's self-testing is the proof of concept for the entire methodology. It cannot be completed until all Jobs are running. Phase 5 is not a build phase — it is a configuration and verification phase. No new Jobs. Just SETI turned fully on itself.

### Jobs
*No new Jobs — configuration and verification only.*

### Phase 5 Completion Criteria
- [ ] tca-seti registered in its own Policy Job with full schedule configuration
- [ ] Contract Tests running against all 15 tca-seti contracts on 15-minute schedule
- [ ] AI-generated Plots submitted to Plot Store for all tca-seti Jobs
- [ ] Plot Tests running daily against tca-seti
- [ ] At least one intentional Contract Test failure triggered and routed correctly
- [ ] At least one Plot Test failure analyzed by AI-lien
- [ ] Full self-monitoring loop verified end-to-end
- [ ] tca-guidelines.md updated with Plot generation standard derived from this build
- [ ] PHASE-PLAN.md marked complete

### Lessons Learned
*Populated when phase completes.*

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
4010  Watchdog
4011  Observability
4012  (reserved)
4013  Integration
4252  AI-lien
```

**Language assignments:**
```
Go          — Gateway, Policy, Contract Test, Plot Test, Plot Store,
              Signal Aggregator, Interactions, Watchdog, Observability,
              Integration
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
