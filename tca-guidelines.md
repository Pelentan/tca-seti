# Tessellated Constellation Architecture — Project Guidelines

**Status:** Living Document  
**Last Updated:** 2026-03-30  
**Audience:** AI partners and engineers working on TCA projects  
**Scope:** Architectural rules, sequencing, and operational standards for any TCA implementation

---

## 1. What TCA Is (Operational Summary)

Tessellated Constellation Architecture is a polyglot microservices methodology built around three non-negotiable structural commitments:

1. **Contracts before code.** Every service interface is defined in a machine-verifiable OpenAPI 3.1 contract before a single line of implementation is written.
2. **Jobs as isolated units.** Each service (a "Job") does one thing, knows nothing about the outside world except what its contract tells it, and runs in its own container.
3. **Three-year lifecycle.** Every Job is designed to be completely rewritten within three years. This is not a failure state. It is the architecture working as intended.

AI is not incidental to TCA. It is structural. The reason contracts-first is enforceable now when it wasn't before is that an AI partner will honor a contract without drifting. The reason polyglot is practical now when it wasn't before is that no human needs to be an expert in every language in the stack. The reason three-year rewrites are feasible now is that a Job rewrite is an afternoon of work with an AI partner.

---

## 2. Project Initialization — Commit Zero

Before a single line of implementation code is written, the following must exist in the repository. These are checkboxes, not suggestions.

- [ ] **`.gitignore`** — generated for the full agreed polyglot stack. Covers compiled artifacts, dependency directories, IDE cruft, environment files, generated certificates, and any secret material paths defined by the project.
- [ ] **`.github/workflows/security.yml`** — CodeQL scanning configured for every language in the stack. If a language is added to the project later, the workflow is updated at that moment, not later.
- [ ] **GitHub secret scanning enabled.**
- [ ] **`README.md`** — minimum viable: what this is, architecture overview, stack with one-line rationale per component, how to run from a clean clone.

Security scanning at commit zero means every subsequent commit is scanned. Adding it later means every prior commit wasn't. There is no "we'll add it when things stabilize."

Certificates, API keys, and any generated credential material are excluded from the repository before those files are created. `.gitignore` first, secrets second.

---

## 3. Architecture-First Sequencing

Work proceeds in this order. Do not skip steps or compress them together.

**Step 1: Define the Jobs**  
Identify what each Job is responsible for — and only responsible for. A Job that is doing two distinct things is not a well-bounded Job. If a Job's description requires the word "and" to describe its primary function, that is a signal to split it.

**Step 2: Define stub behavior**  
Before writing contracts, decide which Jobs will be stubbed and what those stubs return. A stub is not an absence of design — it is a fully designed interface with deferred implementation. Stubs must return correct types and shapes, log visibly that they are stubs ("would send email to {address} via SMTP"), and have a single, clearly marked swap point where the real implementation replaces stub behavior. No caller should change when a stub is replaced.

**Step 3: Write the contracts**  
AI writes the contracts based on the agreed Job definitions. This is the first thing AI produces on a project, before any implementation code. See Section 4.

**Step 4: Implement**  
AI implements each Job against its contract. See Section 5.

The sequence is not flexible. A contract written after implementation is not a contract — it is documentation. Documentation drifts. Contracts don't.

---

## 4. Contracts

### The Rule
The contract is immutable after initial development is complete. If implementation friction suggests the contract should change, the answer is not to change the contract. Surface the conflict, re-examine the Job definition, and either adjust during the initial development window or create a new Job. Once callers exist, the contract is frozen.

This rule exists because the contract is what allows callers to be written without knowledge of the implementation. The moment a contract can be silently adjusted to resolve implementation friction, every caller becomes a liability. Do not propose contract changes as a resolution to implementation problems.

### Format
All TCA contracts are OpenAPI 3.1 YAML. Every contract must define:

- Every endpoint with its full path
- Every request body schema with required fields and types
- Every response schema for every status code, including error shapes
- The `servers` block referencing the service name (not a hardcoded IP or port)
- The `x-tca-observability` block (see Section 6)
- The `x-tca-security` block (see Section 7)

No other paths or response codes are implemented. If the contract doesn't define it, the Job doesn't return it.

### What the Contract Does Not Specify
The contract does not specify implementation language, internal data structures, internal logic, database technology, or anything else inside the Job boundary. The contract is the surface. The interior is entirely opaque to everyone outside the container.

### Contract Location
`contracts/openapi/{service-name}.yaml` in the project root.

### Status Model Discipline
If a Job manages a resource with a status field (poll status, order status, payment status), define the complete status enum in the contract before implementation begins. Collapsing or renaming status values after callers exist requires updating every Job that checks that field. TCA Vox learned this when `open` was collapsed into `public/private` mid-project — the vote handler still checked for `"open"` and silently rejected all votes until the check was updated. Define the status model once, completely, in the contract.

---

## 5. Jobs

### Boundaries
A Job knows nothing about the outside world except what arrives through its contract-defined interface. It does not know the names of other services. It does not know how many instances of itself are running. It does not share a database with any other Job. It does not share code libraries with any other Job. Shared contracts and shared interfaces are fine. Shared state is not.

### Size
A Job should be at most a few thousand lines of implementation code. If it is substantially larger, it is likely doing more than one job. The practical test: a developer should be able to read the Job's code and understand what it does in a single sitting. More importantly, the entire Job must fit within an AI's context window. If it doesn't fit, it cannot be maintained, debugged, or rewritten with AI assistance — which defeats a core structural assumption of TCA.

### Language Selection
Language is chosen based on what is optimal for the Job's primary function. Not what the team knows. Not what the rest of the stack uses. The right language for the Job. AI removes the constraint that the team needs prior expertise — any modern language is viable if the AI can implement it competently and the engineer can read and verify the output.

If a language choice is causing persistent friction during implementation (library conflicts, version churn, poor fit for the problem domain), bring it back to the Job definition stage and reconsider. The Bank Job precedent: the contract required zero changes when the implementation language was replaced entirely. That is the standard.

### AI as Source of Truth for Code
During active development, the AI holds the authoritative state of the codebase. The engineer makes architectural decisions, validates the output, watches the build, and tests the running system. The AI tracks what was built, how, and what changed. Any local modifications made by the engineer must be fed back to the AI before the next build cycle.

This division is not about authority — the engineer has final authority on all decisions. It is about the AI maintaining coherent context across the full implementation so that changes are consistent and nothing gets lost between sessions.

### State Synchronization Between Jobs
When one Job maintains a derived or cached copy of data owned by another Job, define an explicit sync strategy in the contract before implementation begins. Identify which fields need to be mirrored, define the sync trigger (on write, on publish, on schedule), and implement it from the start. TCA Vox had two Jobs holding poll state — poll-builder (source of truth) and poll-wr4ngler (voting mechanics). Status sync was added reactively rather than contractually, resulting in stale status values that broke voting. Define sync at design time, not after the bug surfaces.

---

## 6. Observability — `x-tca-observability`

Every Job contract must include the `x-tca-observability` extension block. Every Job implementation must be built with observability reporting baked in from the start — it is not added later.

Each Job reports all outbound calls to the Observability Job. The report is fire-and-forget: the Job does not wait for confirmation and the application is not affected if the Observability Job is unavailable.

**What gets reported:** caller service name, callee service name, HTTP method, path (sanitized), response status code, round-trip latency in milliseconds, protocol.

**What never gets reported:** user IDs in unsanitized form, JWT tokens, financial data, personally identifiable information.

The Observability Job owns all sanitization. Individual Jobs report raw data to Observability; Observability sanitizes before publishing to Redis pub/sub. No Job other than Observability publishes to the observability channel.

The `x-tca-observability` contract block signals to the AI implementing the Job that outbound call reporting is required. An AI encountering this block should implement the reporting wrapper for all outbound calls without being asked.

---

## 7. Security — `x-tca-security`

Every Job contract must include the `x-tca-security` extension block. Security posture is defined at the contract layer, not added during or after implementation.

### Zero Trust Posture
TCA targets zero-trust internal networking by default. Services trust the handshake, not the network.

**Service-to-service:** Mutual TLS on all internal service boundaries. Each service has its own certificate. Certificates are rotatable without contract changes. The internal network is not a trust boundary — mTLS is.

**User-to-service:** All user requests are authenticated and policy-checked at the gateway. JWT (short-lived, 15 minutes) plus Redis-backed refresh token. High-sensitivity operations (financial, account modification) re-validate the session independently at the service level — they do not rely solely on a valid JWT.

**Auth/OPA:** Policy decisions are centralized. Services ask "is this allowed?" rather than implementing their own authorization logic.

### The `x-tca-security` Block
The block in the contract defines:
- Minimum TLS version for this service (`1.3` preferred; `1.2` where library constraints force it — document the constraint)
- Whether the service is externally exposed or internal-only
- Authentication requirements for each endpoint
- Any service-specific policy requirements

An AI encountering the `x-tca-security` block implements the specified security posture without being asked. TLS version minimums, certificate configuration, and authentication requirements in the block are not suggestions — they are requirements.

### Stub Behavior for Security Services
During development, security services (Auth, OPA, Bank) may be stubbed to return "authorized" on all requests. The stub must:
- Accept the correct request shape
- Log visibly that it is returning a stub authorization ("STUB: auth check passed for {service} — real policy not enforced")
- Have a single swap point where real policy replaces the stub

Zero-trust structure is built in from commit zero even when the policy engine is stubbed. The structure is not added when the stub is replaced.

---

## 8. The Three-Year Lifecycle

Every Job is designed to be completely rewritten within three years. Design decisions should reflect this.

**What this means at implementation time:**
- No hard dependencies between Jobs beyond the contract interface
- No shared databases between services
- No shared code libraries between services
- All configuration external to the container (environment variables, not baked-in constants)
- Container images that build from source, not from a manually maintained state

**What this does not mean:**
- It does not mean building for throwaway quality. Jobs are built to production standards.
- It does not mean rewrites are scheduled. They happen when the landscape demands it: language versions, library churn, security currency, or a better implementation approach.
- It does not mean the contract changes. The contract outlives the implementation.

The practical test for whether a Job is well-bounded: can it be completely rewritten in an afternoon? If yes, the boundary is real. If no, something has leaked across it.

---

## 9. Kubernetes Deployment Patterns

### The Core Rule
Pods are the atomic unit in K8s — you cannot scale containers within a Pod independently. Never co-locate services in a single Pod to solve a latency problem. Use Pod Affinity to keep latency-sensitive services on the same node while keeping them in separate Pods.

### Service Grouping by Latency Budget

**Hot-path domain — hard affinity, same node**  
Services in the critical request path. Affinity rules keep them on the same node; communication approaches loopback speeds without losing independent scaling.

**Security/Financial Domain — co-located with each other, isolated from hot path**  
Called less frequently, higher latency tolerance. Co-locate these with each other but don't pin them to hot-path nodes.

**Communication and async domains — float freely**  
Nearly decoupled from request latency. Let the scheduler place these on available capacity.

### The Legitimate Sidecar Exception
A true sidecar process — one that is always called by exactly one service and has no independent scaling requirement — may share a Pod. OPA as a policy engine co-located with Auth is the canonical example. This is the use case Pod co-location is designed for.

### Topology Spread Constraints
Affinity rules keep services close. Spread constraints keep them distributed across availability zones. Both are required. Affinity without spread is a single point of failure.

---

## 10. Dependency and Build Discipline

### Lockfiles Are Non-Negotiable
Every package manager produces a lockfile. Every lockfile is committed. Every Dockerfile uses the lockfile-respecting install command.

| Ecosystem | Lockfile | Dockerfile command |
|-----------|----------|--------------------|
| Node/npm | `package-lock.json` | `npm ci` |
| Go | `go.sum` | `go mod download` |
| Python | `requirements.txt` (pinned) | `pip install -r requirements.txt` |
| Java/Maven | `pom.xml` (explicit versions) | standard Maven with no version ranges |
| Rust | `Cargo.lock` | standard Cargo |

`npm install` in a Dockerfile without a committed lockfile produces version drift between local and container builds. The failure mode is builds that work locally and break in Docker, or worse, silently behave differently. Use `npm ci`.

### Dockerfile Layer Order
Dependencies before source. Cache invalidation on source changes should not re-run dependency installation.

```dockerfile
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
```

### No "Latest" Versions
"Latest" is not a version. All dependencies specify explicit versions. Unpinned dependencies are a supply chain risk and a reproducibility failure.

### Check Library API Versions Before Implementing
Training data trails reality. Before writing implementation code against any library with a major version history, verify the current API. The cost of one check is much lower than the cost of debugging version mismatch errors in a running container. This is especially critical for security-adjacent libraries (authentication, cryptography, TLS) where API changes often reflect security decisions.

### Container Base Images — No Alpine in Runtime Stages
Alpine is not permitted in runtime container stages. The goal is minimal attack surface: no shell, no package manager, nothing beyond what the Job needs to run.

**Build stages** may use whatever base provides the necessary toolchain. The build stage is discarded — it does not ship.

**Runtime stages** follow the principle of minimum viable base image — the smallest image that can run the binary with its required shared libraries. Alpine is never the answer (musl libc incompatibilities, false sense of minimalism). scratch is the target for statically linked binaries; when shared library dependencies make scratch impossible, use the slimmest image that satisfies those dependencies explicitly.

Current established choices by language:

| Language | Runtime base |
|----------|-------------|
| Go | `scratch` — static binary, zero dependencies |
| TypeScript / Node.js | `gcr.io/distroless/nodejs20-debian12` |
| Python | `gcr.io/distroless/python3-debian12` |
| Elixir | `debian:bookworm-slim` + explicit runtime deps (`libssl3 libncurses6 libstdc++6`) — BEAM shared library requirements make distroless/base insufficient |
| GnuCOBOL + Go | `debian:bookworm-slim` + `libcob4 libgmp10 libgc1` — COBOL binary links against libcob; scratch is not viable |

The single exception is `cert-init`, which uses Alpine. It runs once at startup, generates certificates, and exits. It never serves traffic, never holds persistent state, and is not a TCA Job. The exception is narrow and does not extend to any other service.

**Build stage base images by language:**

| Language | Build base |
|----------|-----------|
| Go | `golang:1.22-bookworm` |
| TypeScript / Node.js | `node:20-bookworm` |
| Python | `python:3.12-slim-bookworm` |
| Elixir | `elixir:1.16-slim` |

**The npm version notice.** Node.js base images ship with an older npm. Suppress the upgrade notice by pinning the current npm version at the top of the build stage, before `npm ci`:

```dockerfile
FROM node:20-bookworm AS builder
RUN npm install -g npm@<current>
```

Check the current npm version before writing any Node.js Dockerfile. Zero warnings on a build is not a stretch goal. It is the standard.

---

## 11. Logging Standards

### Stdout Only
Services write to stdout and stderr. No file output inside containers. Where that output goes is an infrastructure concern, not an application concern — the application is fully decoupled from the logging destination.

### Structured JSON in Production
Development: plaintext to stdout is acceptable.  
Any environment feeding a log aggregator: structured JSON, one event per line.

Minimum fields:
```json
{
  "timestamp": "ISO 8601 UTC",
  "level": "info|warn|error",
  "service": "service-name",
  "message": "human-readable description",
  "request_id": "uuid"
}
```

Domain fields are added as top-level keys. No free-form string concatenation for fields that will be filtered or aggregated.

### Logs as Diagnostic Signal
Container logs are the first diagnostic tool when something is wrong. If a Job is misbehaving and the logs offer nothing useful, the logging is insufficient — not the problem. Every error path must log enough context to diagnose the failure without attaching a debugger: the operation attempted, the inputs involved, and the specific error received. TCA Vox COBOL vote persistence failures were only diagnosable because the drain loop logged the full COBOL error output including the file status code.

---

## 12. Service Exposure

Nothing is externally exposed that does not need to be.

In Docker Compose: only the gateway gets a published port. All other services communicate on the internal network only.

In Kubernetes: this maps to network policies. The mapping is 1:1 from Compose to K8s network policies by design.

The gateway is the single external entry point. TLS terminates there. JWT validation happens there. Everything downstream is internal.

---

## 13. Data Pipeline Design

### Async Pipelines Require Complete Field Contracts
When data moves through an async pipeline — stream, queue, worker, storage — every field must be defined and populated at the point of entry. A missing field is not a runtime error at the point of omission; it is a silent contract violation that fails at the far end of the pipeline with no obvious connection to the origin. TCA Vox: `ForwardedVote` was missing `submittedAt` when sent to redis-handler. Redis-handler's validation rejected every vote silently in a background goroutine. The votes appeared to succeed at the API layer. Nothing ever persisted. Define all pipeline fields contractually and validate them at entry.

### COBOL File I/O
GnuCOBOL's `OPEN EXTEND` requires the file to already exist — it will not create a new file. For new files, use `OPEN OUTPUT`. The correct append-or-create pattern: probe with `OPEN INPUT`, check file status 35 (file not found), close, then branch to `OPEN OUTPUT` if new or `OPEN EXTEND` if existing. File status 35 on `OPEN EXTEND` is the diagnostic signal when COBOL-written persistence fails on first write.

### Redis Stream Consumer Groups
Consumer groups created with position `"0"` replay all existing messages from stream creation. Groups created with `"$"` only receive new messages. Use `"0"` at group creation so restarts replay unacknowledged messages. Use `">"` in XREADGROUP to receive only undelivered messages in normal operation. Do not ACK a failed message — it remains pending for retry on the next cycle.

---

## 14. UI Routing in Single-Page Applications

### Route Guards Must Be Symmetric
When multiple effects or hooks both trigger page-loading logic, they must reference the same skip list. TCA Vox had two `useEffect` hooks that both called `loadPage` with separate skip lists that diverged over time — routes added to one were missed in the other, causing 400 errors on navigation. One canonical skip list, referenced by all effects that need it.

### Page-Builder Routes vs. Application Routes
When a TCA application uses a page-builder Job to serve CMS-managed content alongside application-managed routes, the boundary must be explicit and enforced on the frontend. Application routes must be intercepted before any call to the page-builder. Sending application routes to the page-builder causes 400 errors that surface as broken navigation. Maintain an explicit list of application-owned route prefixes and skip the page-builder call for all of them.

### Session Persistence Across Refresh
If an application uses short-lived JWTs plus refresh tokens, the UI must attempt a silent refresh before rendering any authenticated content. The pattern: on app mount, fire a `/refresh` request using the httpOnly cookie before gating any render on authentication state. Use a ref guard rather than state to prevent React StrictMode's double-invocation from firing two concurrent refresh requests.

---

## 15. SQL and Database Discipline

### PostgreSQL Column Alias Rules
PostgreSQL does not allow referencing a column alias in `GROUP BY` or `WHERE` in the same query. `SELECT x AS value ... GROUP BY value` fails with "column 'value' does not exist." Use the full expression: `GROUP BY answers::json->>0`. This is a known divergence from MySQL and SQLite. All AI-generated PostgreSQL queries must use full expressions in GROUP BY, never aliases.

### Schema Changes Require Volume Resets in Development
`CREATE TABLE IF NOT EXISTS` does not add new columns to existing tables. Adding a column to a running development database requires `docker compose down -v` to destroy volumes, then a full rebuild. The silent failure mode: queries succeed but new columns return null. Document which schema changes require a volume reset in the session notes.

### Financial Values Are Always Strings
Never use `type: number` in an OpenAPI contract for a financial value. Floating point introduces rounding errors that compound. Financial values are `type: string` with a decimal format description in the contract. Implementation uses fixed-point arithmetic (COBOL, decimal libraries) or integer arithmetic in the smallest denomination. This is a contract error — it cannot be corrected by implementation choices alone.

---

## 16. Third-Party Rendering Libraries (Vega-Lite)

### vconcat Spec Rules
When composing multiple Vega-Lite charts into a single `vconcat` spec:

- Strip `$schema` from every sub-spec. A `$schema` field on a sub-spec triggers Vega's internal signal name generation (`concat_N_width`) which the runtime cannot resolve.
- Add explicit `width` to every sub-spec. Without it, sub-specs that use `layer` internally generate dynamic width signals that also fail. A fixed `width: 500` prevents this.
- Add `resolve: { scale: { color: 'independent' }, legend: { color: 'independent' } }` to the outer `vconcat`. Without it, color scales and legends merge across charts, showing all options from all charts in every chart's legend.

### SVG Background
Vega's `toSVG()` outputs `style="background-color: white"` on the root SVG element regardless of the Vega-Lite `config.background` setting. When the report has a non-white background, this overwrites it. Strip it after generation: replace the white background attribute with `transparent`.

### Ordinal vs. Quantitative Axis in Layered Specs
A `layer` spec with one mark on an ordinal x-axis and another on a quantitative x-axis will conflict — Vega cannot resolve two scale types on the same axis. Use `resolve: { axis: { x: 'independent' } }` on the layer spec. Ordinal bars get proper band width; quantitative rule marks position correctly on their own scale.

---

## 17. Configuration Object Design

### Nest Before Callers Exist
When a config object accumulates more than 4-5 fields, restructure from flat to nested before callers exist. Flat configs become unmaintainable as they grow. TCA Vox report config started flat and had to be restructured into `style.page`, `style.title`, and `layout` sections mid-development. The restructure required updating every caller. Nested structure is cheaper to add before callers than to retrofit after.

### Defaults Are Part of the Contract
Every field added to a config type must have a corresponding default value in the `DEFAULT_*` constant. A field without a default forces every caller to handle `undefined` or set the field explicitly. Add the field and its default together, atomically.

### Backwards Compatibility on Contract-Breaking Changes
When a request payload shape changes in a breaking way, keep the old shape working alongside the new one. Accept both, with the old shape as a fallback, for at least one release cycle. Never hard-remove old required fields in the same release that adds new ones. TCA Vox report generation changed from `{ pollGuid, selections, reportTitle }` to `{ pollGuid, config }` — both shapes were accepted during transition.

---

---

## 19. Phase Planning — When in Doubt, Phase It Out

For projects larger than a handful of Jobs, build order is an architectural decision that deserves the same deliberate treatment as Job decomposition. The wrong build sequence produces a constellation that cannot be meaningfully tested until it is mostly complete. The right sequence produces a working system at every phase boundary — each phase delivers something observable, verifiable, and useful on its own.

### When Phase Planning Is Required

Phase planning is required when any of the following are true:

- The constellation has more than six Jobs
- Any Job cannot be meaningfully tested without two or more other Jobs running
- The build is expected to span multiple sessions or multiple engineers
- A new AI session may need to resume work in progress

For smaller constellations where all Jobs can be built and verified in a single session, phase planning is optional but still recommended as a session-continuity tool.

### The Phase Plan Document

Every project requiring phase planning must include a `PHASE-PLAN.md` in the repository root. This document is created before implementation begins and maintained throughout the build. It is the first thing a new AI session reads when resuming a project.

**PHASE-PLAN.md contains:**

1. **Phase list** — each phase named, described in one sentence, and marked with its status: pending, in-progress, or complete.
2. **Per-phase Job list** — which Jobs are built in each phase, with individual completion status.
3. **Per-phase deliverable** — what the constellation can do at the end of this phase that it could not do before. Must be observable and verifiable, not theoretical.
4. **Rationale** — why this phase ordering was chosen. What dependency or visibility requirement drives the sequence.
5. **Lessons learned** — populated as each phase completes. What the phase revealed that affects subsequent phases.
6. **Next action** — the single most specific next thing to do. Updated whenever work pauses. A new AI session reads this line and knows where to start.

**The Next Action field is mandatory and must be kept current.** Vague entries like "continue implementation" are not acceptable. The correct format is: "Implement [specific Job], starting with [specific function/file], because [specific reason].". A new session reading this should be able to start working within one exchange.

### Phase Ordering Principles

**Observability first.** If the constellation has an Observability Job, it is built in Phase 1. Every other Job reports to it from the moment it runs. Building observability last means the entire build proceeded blind.

**The feed before the data.** Build the path that lets you watch the system work before building the system itself. In a constellation with a monitoring dashboard, that means the Gateway, Observability, authentication, and UI land in Phase 1 even if they are not the most complex Jobs. You want to see events flowing before you build what generates them.

**Stubs carry full weight.** A phase is complete when its Jobs are running and their contracts are honored — including stubs. A stub that returns correctly shaped responses and logs visibly is a complete Job for phase purposes. The phase deliverable does not require every Job to have full implementation.

**Each phase must be independently deployable.** `docker compose up` at any phase boundary must produce a running system. Not a complete system — a running one. If a phase produces code that cannot be started without Jobs from the next phase, the phase boundary is in the wrong place.

**Dependencies flow forward, never backward.** A Job built in Phase N must not require a Job from Phase N+1 to function at a stub level. If it does, either the phase boundary moves or the dependency is a stub.

### The PHASE-PLAN.md Template

```markdown
# [Project Name] — Phase Plan

**Status:** [In Progress / Complete]
**Last Updated:** YYYY-MM-DD
**Next Action:** [Specific next task — updated whenever work pauses]

---

## Phase 1 — [Name]: [One-sentence deliverable]

**Status:** [Pending / In Progress / Complete]
**Deliverable:** What you can observe/verify when this phase is done.
**Rationale:** Why this phase comes first.

### Jobs

| Job | Language | Port | Status |
|-----|----------|------|--------|
| job-name | Go | 4000 | Complete / In Progress / Pending |

### Lessons Learned
*Populated when phase completes.*

---

## Phase 2 — [Name]: [One-sentence deliverable]
...
```

### Relationship to tca-guidelines

The Phase Plan does not replace any existing TCA discipline — contracts still come before implementation, stubs are still real contracts, observability is still mandatory from the start. The Phase Plan governs the *order* in which that discipline is applied across a larger build. It is a sequencing document, not an architectural one.

The AI partner maintains PHASE-PLAN.md as source of truth alongside the code. When a phase completes, the AI updates the status, populates lessons learned, and sets the Next Action field before any other work proceeds. The engineer confirms the update before the session ends.

---

---

## 20. The Augur Canis (AC) — Standard TCA Component

Every TCA constellation includes an Augur Canis (AC) agent alongside the Observability Job. AC is not optional instrumentation — it is the health verification layer that enables Docker and Kubernetes to know whether a Job is genuinely functioning, not merely running.

The name carries two meanings that describe the thing precisely. *Augur*: one who reads signs and renders a verdict through examination, not by asking the subject. *Canis*: the dog that alerts when the pattern breaks. Together: behavioral verification plus anomaly signaling.

### What Augur Canis Does

AC executes minimal canned queries against each Job in the constellation to verify behavioral health, not just process presence. A Job can be running and broken. AC detects the difference.

It publishes health state to a Redis pub/sub channel (`tca:augur-canis`) that SETI subscribes to alongside `tca:events`. Health state, latency, and time-to-complete for every Job flow into the same signal layer as observability events.

When anomalies are detected, AC barks on a dedicated alert channel (`tca:augur-canis:alerts`) separate from the health feed. The alert channel carries only barks — a subscriber that only wants to know when something is wrong does not need to filter a high-volume health feed.

### Network Topology — Point-to-Point Isolation

AC connects to each Job via its own dedicated network containing exactly two members: AC and that Job. If the constellation has N Jobs, there are N dedicated AC networks, named `ac-{service}-net`.

This is not a shared AC network. A shared network would mean a compromised Job could reach every other Job through AC's network. Point-to-point networks mean the blast radius of any compromise stops at that Job's own networks plus its dedicated AC link — and that link cannot be used by the compromised Job to pivot, because AC is the sole initiator on all point-to-point networks.

In Docker Compose:
```yaml
networks:
  ac-gateway-net:
    driver: bridge
  ac-clearance-net:
    driver: bridge
  # ... one per Job
```

Each Job is attached to its normal operational networks plus its single dedicated AC network. AC is attached to all AC networks and nothing else — it has no access to the operational networks between Jobs.

### Health Check Mechanism — Redis-Mediated, No Open Ports

No Job exposes a plain HTTP health port. No Job exposes any additional surface for health checking. The mechanism is entirely Redis-mediated, with no open ports on any container:

1. The healthcheck binary inside a Job container publishes a check request to Redis `tca:check-requests` with a unique `request_id`, `container_id`, and `service_name`.
2. AC subscribes to `tca:check-requests`, receives the request, looks up the canned query for `service_name` from its DB.
3. AC executes the canned query over the dedicated point-to-point mTLS network to that Job.
4. AC publishes the result to `tca:check-results:{request_id}` with a configurable TTL (default 30 seconds). The TTL prevents accumulation if the healthcheck binary crashes or times out.
5. The healthcheck binary subscribes to `tca:check-results:{request_id}`, reads the result, exits 0 (healthy) or 1 (unhealthy).

Docker Compose healthcheck:
```yaml
healthcheck:
  test: ["CMD", "/healthcheck"]
  interval: 30s
  timeout: 10s
  retries: 3
  start_period: 30s
environment:
  SERVICE_NAME: "job-name"
  REDIS_URL: "redis:6379"
```

Kubernetes exec probe (identical binary, no network call from K8s):
```yaml
livenessProbe:
  exec:
    command: ["/healthcheck"]
  initialDelaySeconds: 30
  periodSeconds: 30
```

The healthcheck binary falls back to a Redis connectivity check if AC is unavailable (startup ordering, AC restart). A Job that can reach Redis is treated as degraded-healthy rather than killed. AC will do the full behavioral verification on the next cycle.

### Canned Queries

AC stores one query definition per Job in its own isolated database. A canned query is the minimum request that confirms a Job is alive and responding correctly per its contract — not a Contract Test, not a Plot. Just enough to distinguish "running and healthy" from "running and broken."

Query definitions are versioned and updateable by the Sec Wr4ngler without code changes or redeployment. The query version used is recorded in every check result for audit purposes. As a constellation evolves, query definitions evolve with it — AC does not need to be redeployed to update a check.

Example query definitions:
- **Observability Job**: POST /event with a known test payload, expect 202
- **Gateway**: GET /health, expect 200 with `{"status":"healthy"}`
- **Signal Clearance**: GET /health, expect 200
- **A data-writing Job**: POST with a minimal valid payload, expect 201 or 202

### Anomaly Detection and Alert Lifecycle

AC watches the health feed for three classes of anomaly. Silence detection is active. Latency drift and failure rate detection are implemented as data-collection stubs — the Redis windows are populated on every check cycle, the alert logic is deferred.

**Silence detection (active):** If a Job stops appearing in the check feed for longer than `SILENCE_THRESHOLD_SECONDS` (default 3x check interval), AC fires an alert.

**Latency drift (stubbed):** Rolling window of the last 20 latency readings per Job stored in Redis. Alert fires when current reading exceeds `baseline * multiplier` for K consecutive cycles. Stub: window populated, alert logic not yet active.

**Failure rate (stubbed):** Rolling window of the last 20 health results (0/1) per Job stored in Redis. Alert fires when failure rate exceeds configurable threshold. Stub: window populated, alert logic not yet active.

**Alert lifecycle:**
1. Condition detected → bark once, `bark_count: 1`
2. Next check cycle → bark again, `bark_count: 2`
3. Next check cycle → bark again, `bark_count: 3`
4. After 3 barks → reminder bark every `REMINDER_INTERVAL_SECONDS` (default 30s) until acknowledged
5. `POST /alerts/{alert_id}/acknowledge` → stops reminders, sends acknowledgment bark, condition still watched
6. Condition clears → auto-resolve regardless of acknowledgment state, sends `resolved` bark with `resolution_type: condition_cleared`
7. If condition never acknowledged when it clears → resolved bark notes `unacknowledged_clear` for post-mortem

**Alert storm prevention:** The `alert_active` flag prevents duplicate initial alerts. Once in reminder mode, reminders fire on the configured interval regardless of check cycle timing.

### Kubernetes Container Replacement Detection

When Kubernetes replaces a container (OOMKill, crashloop, manual restart), AC infers the replacement from the data it already receives. No Kubernetes API access required. No RBAC. No sidecar.

The healthcheck binary includes the container ID (`$HOSTNAME`, which Docker sets to the container ID) in every check request. AC tracks the last known container ID per Job:

- Check request arrives with a new container ID for a service that had an active alert → AC auto-resolves with `resolution_type: container_replaced`
- Check request arrives with a new container ID for a service with no active alert → AC resets the latency and health windows for that Job, logs the replacement

**Optional enhancement — PreStop lifecycle hook:**
```yaml
lifecycle:
  preStop:
    exec:
      command: ["/notify-ac"]
```
A tiny binary that publishes a `terminating` event to Redis before the container is killed. AC receives it and can mark the alert as `container_replaced` with advance notice rather than inferring after the fact. This is optional — the container ID change detection catches replacements even when the PreStop hook cannot execute (frozen container).

### Redis Channels

| Channel | Publisher | Subscriber | Purpose |
|---------|-----------|------------|---------|
| `tca:check-requests` | healthcheck binary | AC | Trigger a check for a specific Job |
| `tca:check-results:{request_id}` | AC | healthcheck binary | Return check result to caller |
| `tca:augur-canis` | AC | SETI Signal Aggregator | Constellation health state feed |
| `tca:augur-canis:alerts` | AC | SETI, operators, AI monitors | Alert-only feed — barks only |

### Redis State Keys

AC stores all anomaly detection state in Redis. State resets on Redis flush — this is a feature, not a bug. A Redis flush typically follows an architecture change, which invalidates the existing baseline. AC re-establishes its baseline naturally on the next N check cycles.

```
ac:state:{service}:last_seen           → ISO8601 timestamp of last check request
ac:state:{service}:last_container_id   → current container ID
ac:state:{service}:last_container_id:prev → previous container ID (for replacement detection)
ac:state:{service}:latency_window      → list of last 20 latency_ms values (capped)
ac:state:{service}:health_window       → list of last 20 healthy values — 0 or 1 (capped)
ac:state:{service}:alert_active        → "1" when alert is firing
ac:state:{service}:alert_id            → current alert UUID
ac:state:{service}:alert_type          → silence | latency_drift | failure_rate
ac:state:{service}:bark_count          → 1-3 initial, "acknowledged" after ack
ac:state:{service}:last_bark_at        → ISO8601 timestamp of last bark
ac:state:{service}:alert_first_at      → ISO8601 timestamp when alert first fired
ac:alert:{alert_id}                    → service_name (reverse lookup, 24h TTL)
```

### The Healthcheck Binary

A small static Go binary built into every container. It handles the full Redis pub/sub coordination:

```
/healthcheck
  → publishes to tca:check-requests
  → waits on tca:check-results:{request_id} (with TTL-based timeout)
  → exits 0 (healthy=1) or 1 (healthy=0)
  → falls back to Redis PING if AC unavailable — degraded-healthy
```

Built during the Docker build stage from `healthcheck/` in the project root. The same binary works in every container regardless of language. In scratch containers it is the only binary besides the service binary. In distroless containers it is copied in alongside the service.

For distroless/nodejs containers, `healthcheck.js` provides identical behavior using Node's built-in `net` module and raw RESP protocol — no npm dependencies.

```dockerfile
# In every Go/static Job's Dockerfile (project-root build context):
WORKDIR /hc
COPY healthcheck/go.mod healthcheck/go.sum ./
RUN go mod download
COPY healthcheck/main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /healthcheck .
```

The binary requires two environment variables:
- `REDIS_URL` — Redis connection string (already present in every Job)
- `SERVICE_NAME` — this Job's service name, for the check request payload

### SETI Integration

SETI knows that every registered TCA application has an Augur Canis agent. The Signal Aggregator subscribes to each application's `tca:augur-canis` channel alongside `tca:events` when the application is registered. Cross-application Job health flows into the same signal layer as observability events.

The dedicated `tca:augur-canis:alerts` channel is also subscribed by SETI for alert correlation. An AI monitoring agent subscribed to the alerts channel needs no special logic — three barks followed by reminders is a sufficient signal for automated escalation. A human operator benefits from the reminder cadence to ensure the alert is seen.

### Adding AC to a New TCA Project

1. Add the Augur Canis Job to the constellation with its own DB.
2. Add one `ac-{service}-net` network per Job in docker-compose.yml.
3. Attach each Job to its dedicated AC network in addition to its operational networks.
4. Attach AC to all AC networks and to the main internal network only.
5. Register each Job with AC via `POST /jobs`.
6. Define a canned query for each Job via `PUT /queries/{service_name}`.
7. Add the healthcheck binary build to each Job's Dockerfile.
8. Set `SERVICE_NAME` and `REDIS_URL` environment variables in each Job's service definition.
9. Add Docker Compose healthcheck `test: ["CMD", "/healthcheck"]` to each service.

AC's own health is verified by SETI's Signal Aggregator watching the `tca:augur-canis` feed — if the feed goes silent, AC itself has failed. AC does not check itself.

---

## 18. What We've Learned (Running Log)

Most recent first. Add entries as lessons are established — not on every commit, but when a principle is proven or a painful mistake earns its place here.

**2026-03-31 — Augur Canis: The Correct Health Check Architecture**
The Redis pub/sub coordination pattern for health checks — where the healthcheck binary inside a container triggers a check via Redis rather than exposing an HTTP port — eliminates every class of health check vulnerability simultaneously: no open ports, no unauthenticated surfaces, health check traffic itself observable through the monitoring layer. This pattern is not TCA-specific; it is the correct architecture for any containerized environment. TCA's polyglot constraint forced the general solution by making language-specific workarounds unacceptable across six languages simultaneously.

**2026-03-31 — Augur Canis: Point-to-Point Networks Are Non-Negotiable**
A shared health check network introduces the attack vector the agent is designed to detect. If a Job is compromised and the Augur Canis agent shares a network with all Jobs, the compromised Job can reach every other Job through that network. N Jobs require N dedicated two-member networks. The naming convention is `ac-{service}-net`.

**2026-03-31 — K8s Container Replacement Inferred from Container ID Change**
Kubernetes container replacement does not require Kubernetes API access or RBAC permissions to detect. The healthcheck binary already sends the container ID (`$HOSTNAME`) on every check request. Augur Canis tracks the previous container ID per Job and infers replacement when it changes. PreStop lifecycle hooks are an optional enhancement for advance notice but are not required.

**2026-03-27 — Config Objects: Nest Before Callers Exist**
TCA Vox report config required a mid-development restructure from flat to nested sections because fields outgrew the flat shape before structure was established. Restructuring after callers exist requires updating every caller. Nest early.

**2026-03-27 — Vega SVG Has a Hardcoded White Background**
`vega.toSVG()` outputs `style="background-color: white"` regardless of config. Strip it. In `vconcat`, strip `$schema` from sub-specs, add explicit `width`, and use `resolve: { scale/legend: { color: 'independent' } }`.

**2026-03-27 — PostgreSQL Rejects Column Aliases in GROUP BY**
`GROUP BY value` where `value` is an alias fails. Use the full expression. This diverges from MySQL/SQLite behavior. AI-generated PostgreSQL must use full expressions.

**2026-03-27 — Define Status Models Completely Before Implementation**
Collapsing status values mid-project requires updating every Job that checks that field. TCA Vox: `open` → `public/private` missed the vote handler, silently rejecting all votes. Define the complete status enum in the contract first.

**2026-03-27 — Async Pipeline Fields Must Be Complete at Entry**
Missing fields fail silently at the far end of the pipeline, not at the point of omission. TCA Vox: missing `submittedAt` in `ForwardedVote` caused silent vote rejection in a background goroutine. Define all pipeline fields contractually and validate at entry.

**2026-03-27 — GnuCOBOL OPEN EXTEND Requires Existing File**
`OPEN EXTEND` fails with status 35 on a non-existent file. Probe with `OPEN INPUT` first, then branch to `OPEN OUTPUT` for new files.

**2026-03-27 — UI Route Guards Must Be Symmetric**
Multiple effects with separate skip lists will diverge. One canonical skip list, referenced by all effects.

**2026-03-27 — Schema Changes Require Volume Resets**
`CREATE TABLE IF NOT EXISTS` does not add new columns. Volume reset (`docker compose down -v`) required when adding columns to existing tables.

**2026-03-07 — No Alpine in Runtime Stages**
Alpine is not permitted in runtime container stages. Go Jobs run in scratch. TypeScript/Node use distroless. The only exception is `cert-init`. Zero warnings on a build is the standard.

**2026-03-04 — x-tca-security Belongs in the Contract, Not the Implementation**
Security posture defined only in implementation code is invisible to subsequent Jobs and contract reviewers. The `x-tca-security` block makes security requirements a first-class part of the interface definition.

**2026-02-26 — npm ci + Lockfiles Are Non-Negotiable in Docker**
`npm install` without a committed lockfile produces version drift. Use `npm ci`.

**2026-02-26 — Check Library API Versions Before Writing Code**
SimpleWebAuthn v10 had breaking changes from v9 not reflected in training data. Verify current API before implementation for any library with major version history.

**2026-02-26 — Stdout Is the Only Correct Logging Target in Containers**
Writing logs to files inside containers creates operational complexity with zero benefit. Stdout decouples the application from logging infrastructure entirely.

**2026-02-25 — Decomposition Is a Principle, Granularity Is a Variable**
The right question is not "microservices or monolith?" It is: what is the latency budget, and where does it come from?

**2026-02-25 — .gitignore and Security Actions Are Commit Zero**
Security scanning added mid-project means every prior commit was unscanned.

**2026-02-25 — Contracts Before Implementation**
Writing OpenAPI specs first forces clarity. Ambiguities that would cause mid-implementation pivots get resolved at design time instead.

**2026-02-25 — Stub Contracts Are Real Contracts**
When the real implementation replaces a stub, no caller changes. That is the test.
