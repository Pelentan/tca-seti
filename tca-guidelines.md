# Tessellated Constellation Architecture — Project Guidelines

**Status:** Living Document  
**Last Updated:** 2026-04-13  
**Audience:** AI partners and engineers working on TCA projects  
**Scope:** Architectural rules, sequencing, and operational standards for any TCA implementation  
**Related:** [Augur Canis Guidelines](AC-GUIDELINES.md) · [SETI Guidelines](SETI-GUIDELINES.md)

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

### Build Context — Project Root Required

Every Docker image in a TCA constellation is built from the **project root**, not from the service subdirectory.  This is non-negotiable and must be enforced in `scripts/build-push.sh`.

```bash
# Correct — build context is project root
docker build -t registry/service:tag -f service/Dockerfile .

# Wrong — build context is the service directory
docker build -t registry/service:tag service/
```

**Why:** TCA Dockerfiles use service-prefixed COPY paths (`COPY service/go.mod ./`) so all files in the constellation are accessible during the build.  A service-directory context will silently appear to succeed on cached layers while failing on fresh builds.  The diagnostic indicator is `transferring context: 2B` in the Docker build output — an empty context.

**build-push.sh format:** The third field is always `.`:

```bash
services=(
  "augur-canis:augur-canis/Dockerfile:."
  "gateway:gateway/Dockerfile:."
)
```

### Dockerfile Layer Order
Dependencies before source. Cache invalidation on source changes should not re-run dependency installation.

```dockerfile
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
```


### Container Image Naming in Shared Registries

When multiple constellations share a single container registry — as is standard in the TCA k3d development environment — image names must be prefixed with the constellation name to prevent collision.

**The rule:** `<constellation>-<service>:<tag>`

Examples:
- `seti-gateway:dev` — SETI constellation gateway
- `vox-gateway:dev` — Vox constellation gateway
- `seti-cert-forge:dev` — SETI cert-forge instance
- `vox-cert-forge:dev` — Vox cert-forge instance

This applies to every image in the constellation, including infrastructure images like `cert-forge` and `augur-canis`. Two constellations may each have a `cert-forge` — they are different binaries with different configurations and must not share an image.

In production deployments where each constellation has a dedicated registry, the prefix is unnecessary and should be omitted. The `imagePrefix` Helm value (empty in `values.yaml`, set to `<constellation>-` in `values/dev.yaml`) controls this without requiring template changes.

**Why this matters:** A `build-push.sh` that pushes `gateway:dev` overwrites any other constellation's `gateway:dev` in the same registry. The last push wins silently. Services then run the wrong binary with no error until runtime behavior diverges. This was discovered during the SETI and Vox K8s migration when both constellations pushed `gateway:dev` to the shared `tca-registry` and SETI's gateway started serving Vox's binary.

**Establish the prefix at project initialization.** Retrofitting it after images are already in the registry requires a full rebuild and redeploy. Add the prefix to `build-push.sh` and the Helm values before the first `docker push`.

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

## 20. Augur Canis — Standard TCA Component

Augur Canis (AC) is a health and behavioral monitoring service for TCA constellations.  It is also a standalone product with its own repository.  Every TCA constellation includes an Augur Canis instance.

The name carries two meanings that describe the thing precisely.  *Augur*: one who reads signs and renders a verdict through examination, not by asking the subject.  *Canis*: the dog that alerts when the pattern breaks.  Together: behavioral verification plus anomaly signaling.

### What AC Does

AC executes contract tests against every registered Job, maintains rolling time-series metric streams, runs three anomaly detectors continuously, fires alerts through a Redis pub/sub channel, and remembers baselines.  Full reference: [AC-GUIDELINES.md](AC-GUIDELINES.md).

The six capabilities in brief:

**Health monitoring** — AC sends `POST /check` to every registered service on a configurable interval (default 30 seconds).  Services respond with status and container ID.  AC records latency, detects silence, and tracks Kubernetes container replacements.

**Contract test execution** — AC maintains a hardcoded suite of behavioral assertions authored independently from the contracts they verify.  Tests run on a schedule and on demand.  The suite is the "say it three times" verification layer:  contract says what, implementation does it, test verifies independently.

**Latency drift detection** — Rolling time-series streams per Job in Redis Streams.  Auto-baseline established from observed data.  Alert fires when current mean exceeds baseline × multiplier for N consecutive cycles.  Wr4ngler SLA thresholds can be set per Job with configurable alert severity.

**Failure rate detection** — Rolling health stream per Job.  Alert fires when failure rate exceeds configurable threshold.

**Silence detection** — Alert fires when a Job stops responding.  Auto-resolves on Kubernetes container replacement via container ID tracking.

**Institutional memory** — Auto-baselines per Job in Redis.  Wr4ngler baselines in Redis (persistent, explicit operator decisions).  Metric streams queryable via time-bounded API for the SETI LaE visualization.

### Self-Registration

Any service that wants AC to monitor it calls `POST /services/register` at startup.  The registration includes the service name, network endpoint, certificate fingerprint, and a signature for identity verification.  Self-registration retries indefinitely — services do not fail to start because AC is not yet available.

The service name in the registration payload must match the contract title exactly as processed by the contract-test Job's `serviceNameFromTitle()` function.  A mismatch causes all contract tests for that service to be silently skipped with `job_not_deployed`.

### Network Topology

AC joins a dedicated two-member Docker network with each service it monitors (`ac-net` in the SETI implementation).  This is not a shared network.  A shared AC network means a compromised Job can reach every other Job through AC's network.  Point-to-point isolation confines the blast radius.

In Docker Compose:
```yaml
networks:
  ac-net:
    driver: bridge
```

Each Job attaches to its operational networks plus `ac-net`.  AC attaches to `ac-net` only — it has no access to the operational networks between Jobs.

### Detection Thresholds Are Code, Not Configuration

All detector thresholds (`MIN_BASELINE_SAMPLES`, `LATENCY_DRIFT_MULTIPLIER`, `FAILURE_RATE_THRESHOLD`, `CONSECUTIVE_DRIFT_CYCLES`) are Go constants in the source, not environment variables.  Changing a detection threshold is an architectural decision — it requires a code review, a rebuild, and a tracked deployment.  Environment variable thresholds can be silently changed and silently reverted.  Code constants require a commit.

### Dev UI — Port 4666

When `AC_UI_PORT=4666` is set, AC serves a plain HTML observability page on that port — no authentication, no build step, embedded in the binary.  Intended for initial setup verification.  The port name is the warning: 4666, "For 666."  Remove before production.

### Standalone Repository

AC has its own repository: github.com/Pelentan/augur-canis.  It can be adopted independently of SETI and TCA.  The integration surface with SETI is entirely through AC's existing admin API — no new endpoints are required when AC is integrated into a SETI constellation.

### Adding AC to a New TCA Project

1. Add AC and Redis to the constellation's docker-compose.yml.
2. Add `ac-net` to docker-compose networks.
3. Add each Job to `ac-net` in its service definition.
4. Each service calls `selfRegisterWithAC()` at startup.
5. Add contract test entries to `contracttests.go` for each new service.
6. Add Docker Compose healthcheck `test: ["CMD", "/healthcheck"]` to each service.

The healthcheck binary (`healthcheck/` in the project root) is built into every container during the Docker build stage.  It handles the Redis-mediated health coordination and falls back to a Redis PING if AC is unavailable.

Full setup reference: [AC-GUIDELINES.md](AC-GUIDELINES.md).

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

**2026-04-09 — cert-forge: PKI Abstraction Is a Standard TCA Component**
Distributing certificates via a shared Docker volume exposes all private keys to all services — any compromised service can read every other service's key material. cert-forge solves this by acting as a PKI abstraction layer: it generates the CA in memory, issues instance certificates on demand over an enrollment mTLS connection, and holds all private key material in memory only. Services receive their own cert/key over an encrypted channel and never see any other service's material. cert-forge is a candidate standard TCA component applicable to any constellation, not a SETI-specific pattern.

**2026-04-09 — cert-forge: Three-Port Architecture Is Load-Bearing**
TLS client authentication cannot be enforced per-path on a single port — it is a connection-level property. cert-forge requires three distinct servers: port for plain HTTP (CA cert distribution — public), port for enrollment mTLS (instance cert issuance — requires enrollment cert), port for constellation mTLS (signing operations — requires instance cert). Attempting to collapse these onto fewer ports will break the security model.

**2026-04-09 — cert-forge: Enrollment CA Pattern**
cert-forge generates a separate enrollment CA whose only issued credential is a single enrollment cert written to the shared volume. This cert's only capability is calling the instance-cert endpoint. The constellation CA private key never touches the shared volume. In K8s production, the cert issuance backend points to cert-manager or the organizational CA — the signing responsibility stays with cert-forge, the constellation is decoupled from infrastructure PKI choices.

**2026-04-09 — Gateway Must Forward Upstream Response Headers**
A reverse proxy that reads the upstream response body and status code but does not copy upstream response headers silently discards Set-Cookie, Cache-Control, and other headers the client depends on. In SETI, the gateway was discarding Set-Cookie from signal-clearance, so the httpOnly refresh token cookie was never stored in the browser — the client sent every refresh request with no cookie and received 400. Always copy all upstream response headers to the client response before writing the body.

**2026-04-09 — httpOnly Cookie Scope Is the Issuing Domain and Port**
A cookie set by a service on port N is scoped to port N. If a reverse proxy on port M forwards the response but does not preserve the Set-Cookie header, the browser never receives the cookie. If the proxy does forward it, the cookie is scoped to the proxy's port (M), and the browser will send it back to port M — which is correct when all client traffic routes through the proxy. The cookie must be issued through the gateway, not directly from the upstream service, for cookie-based auth to work in a proxied architecture.

**2026-04-09 — Session Timeout Is Inactivity Timeout, Not Wall Clock**
A JWT with a 15-minute TTL is not a 15-minute session limit — it is a 15-minute inactivity timeout, implemented by refreshing the token on every authenticated API call. getFreshJWT() must call the refresh endpoint unconditionally on every invocation, not only when the token is near expiry. Any expiry-check before refresh defeats the inactivity timeout model: a user active at minute 10 who returns at minute 17 is still locked out because the token was never refreshed during the active period.

**2026-04-09 — Contract Title Must Produce the Same String as Service Self-Registration Name**
Contract-test derives the service name from the contract title using serviceNameFromTitle(). Augur Canis looks up registered jobs by the name the service used when it called self-register. If these two strings don't match exactly, every test for that service is silently skipped with job_not_deployed — not failed, skipped. The contract title is authoritative. Display name choices (capitalisation, numeronym substitution like Wr4ngler vs Wrangler) must not diverge from the technical identifier. Verify: serviceNameFromTitle(contract.title) == service.service_name in self-registration payload.

**2026-04-13 — Contract Tests Are Not Generated Tests**
A contract test suite generated from OpenAPI contracts cannot independently verify those contracts — it re-states them.  Independent verification requires a human to read the contract, understand the intent, and write assertions separately.  If a contract changes, the tests must be updated manually.  That friction is the point.  Hardcode the test suite; update it by hand.

**2026-04-13 — Negative Tests Are the Tests Most Likely to Find Real Bugs**
Positive tests verify that correct input produces correct output.  Negative tests verify that incorrect input produces a 4xx response, not a 5xx.  A 5xx on bad input means the Job is swallowing errors.  This is a real bug that positive tests cannot surface.  Every Job with POST endpoints needs at least one negative test sending malformed or missing required fields.

**2026-04-13 — Detection Thresholds Belong in Code, Not Configuration**
Monitoring and alerting thresholds are architectural decisions with operational consequences.  An environment variable threshold can be set to zero by anyone with access to the compose file, then reset.  A hardcoded constant requires a code review, a rebuild, and a deployment.  The friction is the safeguard.  Interactions' critical failure threshold (50%) and silence window (7 days) are Go constants, not env vars.

**2026-04-13 — Python BaseHTTPServer + Go mTLS Client: Use ResilientHTTPServer**
Python's `BaseHTTPServer.handle_error` propagates `BrokenPipeError` and `ssl.SSLError` to stderr and terminates the handler thread.  This causes the Go mTLS client to see a broken pipe on the write side.  Subclass `HTTPServer` with a `handle_error` override that silently absorbs `BrokenPipeError`, `ConnectionResetError`, and `ssl.SSLError`.  These are expected when an mTLS client closes the connection before reading the full response.

**2026-04-13 — Go HTTP Client + Python BaseHTTPServer: Use bytes.NewReader with ContentLength**
A custom `io.Reader` type that only implements `Read()` causes Go's HTTP client to use chunked transfer encoding — it cannot determine content length upfront.  Python's `BaseHTTPServer` does not handle chunked POST bodies reliably under TLS and closes the connection mid-write.  Use `bytes.NewReader(payload)` with explicit `req.ContentLength = int64(len(payload))` for all POST requests from Go to Python services.

**2026-04-13 — Redis Streams over Lists for Time-Series Data**
Redis Lists (LPUSH/LTRIM) store raw values with no timestamps.  Time-bounded queries require iterating the full list and filtering client-side.  Redis Streams (XADD/XRANGE) store entries with millisecond-precision timestamps as built-in IDs, enabling `XRANGE minMs maxMs` queries with no client-side filtering.  Any metric or event data that will be queried by time window belongs in a Stream.

**2026-04-13 — AI-lien With Lore Is a Different Instrument Than AI-lien Without It**
An AI analysis without institutional memory is a first-responder with no case history.  Feeding Lore baselines, recent incidents, and known patterns into the analysis context before every query produces qualitatively different assessments.  The loop — Lore feeds AI-lien, AI-lien feeds Lore — means every analysis makes the next one better.  Build the memory layer before depending on the intelligence layer.

**2026-04-13 — The Notifier Must Be a Permanent Stub in the Open Source Distribution**
Notification mechanisms are environment-specific.  A default implementation that "mostly works" (e.g., a generic SMTP sender) gives operators the wrong signal — they ship with a default they did not choose, and humans do not get paged correctly.  An explicit permanent stub with `stub_active: true` in the health response forces operators to make a deliberate decision about how humans get woken up.  The stub is the safeguard.

**2026-04-13 — getFreshJWT in React useEffect Dependencies Causes Infinite Loops**
Functions from hooks (e.g., `useAuth`) typically get a new reference on every render.  Including them in `useEffect` or `useCallback` dependency arrays causes the effect to fire on every render, triggering state updates, causing re-renders.  Fix: store the function in a `useRef` and sync it with a separate effect.  Call `ref.current()` inside effects instead of the function directly.  The dependency array contains only the values that should meaningfully trigger re-runs.

**2026-04-12 — Chunked Transfer Encoding Breaks Node.js TLS POST Endpoints from Go mTLS Clients**
Node.js TLS POST endpoint connections from Go mTLS clients time out consistently when the Go client uses chunked transfer encoding.  AC's suite removes negative POST tests for Node.js services (signal-clearance) rather than fighting the transport mismatch.  Verify via Ring Trial instead.

**2026-04-12 — Plot Test Retry Logic Belongs in Plot-test, Not Callers**
Retry logic for transient failures (pod recycle, connection refused, 5xx) belongs in the executor — plot-test in this case — not in callers or escalation paths.  Only retry on network-level failures and 5xx.  Never retry on 4xx, assertion failures, or chain failures.  Record the attempt count in every step result so the pattern is visible in Lore even when the plot passes.



---

## 21. cert-forge — PKI Abstraction Layer

Every TCA constellation needs certificates. The naive approach — generating all certificates in an init container and distributing them via a shared Docker volume — has a fundamental flaw: every service can read every other service's private key material. A single compromised container exposes the entire constellation's PKI.

cert-forge is the correct architecture. It is a persistent running service that acts as a PKI abstraction layer: generates the CA in memory, issues instance certificates to services on demand over an authenticated enrollment channel, and never writes private key material to any shared storage. Like Augur Canis, cert-forge is a standard TCA component that applies to any constellation regardless of domain.

### Three-Port Design

TLS client authentication is a connection-level property — it cannot be enforced per-path on a single port. cert-forge requires three servers on three distinct ports. The port numbers are chosen from the constellation's port pool at design time — they are not fixed across constellations. This is a deliberate security decision: a known standard port for a high-value PKI service creates a known attack surface.

| Role | Transport | Endpoint | Caller |
|------|-----------|----------|--------|
| Public (base+2) | Plain HTTP | `/ca` | Anyone — CA cert is public |
| Enrollment (base+1) | Enrollment mTLS | `/instance-cert` | Holder of enrollment cert only |
| Sign (base) | Constellation mTLS | `/sign` | Holder of a valid instance cert |

The three ports are consecutive: if the sign port is `N`, enrollment is `N+1`, and public is `N+2`. The certforge client derives enrollment and public URLs automatically from the sign port. Choose ports that are clearly separated from application service ports to avoid confusion.

**Example port assignments:**
- SETI: 4014 (sign), 4015 (enrollment), 4016 (public)
- TCA Vox: 3020 (sign), 3021 (enrollment), 3022 (public)

### Enrollment CA Pattern

cert-forge generates a separate enrollment CA at startup — separate from the constellation CA. It issues exactly one enrollment certificate. This cert's only capability is calling `/instance-cert`. The constellation CA private key never leaves cert-forge's memory.

In Docker Compose, the enrollment cert is written to the shared certs volume and mounted read-only into every service container. In Kubernetes, it is written to a K8s Secret and mounted as a volume.

### Service Startup Sequence

1. Service starts, finds enrollment cert at `/certs/enrollment.crt` and `/certs/enrollment.key`
2. Calls cert-forge's public port to fetch the CA cert (unauthenticated — CA cert is public)
3. Calls cert-forge's enrollment server presenting the enrollment cert over mTLS
4. cert-forge verifies the enrollment cert is signed by the enrollment CA, issues an instance cert
5. Instance cert and key are delivered over the encrypted enrollment channel and held in memory only
6. Service builds its mTLS server and client configs from the in-memory instance cert
7. Service self-registers with AC using its instance cert fingerprint for identity verification

### Key Material Lifecycle

| Material | Storage | Notes |
|----------|---------|-------|
| Constellation CA key | Memory only | Never written anywhere |
| Enrollment CA key | Memory only | Never written anywhere |
| Instance private keys | Memory only | Generated per-service, delivered over enrollment mTLS, never stored |
| CA public cert | Volume / K8s Secret | Public — safe to store |
| Enrollment CA public cert | Volume / K8s Secret | Used by cert-forge to verify enrollment callers |
| Enrollment cert + key | Volume / K8s Secret | Only private key material on shared storage — single capability: call `/instance-cert` |
| star-gazer cert + key | Volume / K8s Secret | Static federation identity — longer lifecycle than instance certs |

### CA Persistence Across Restarts

The constellation CA must survive cert-forge restarts. If cert-forge restarts and generates a new CA, every service's instance cert becomes invalid — inter-service mTLS fails with `unknown certificate authority` because existing certs are signed by the old CA.

**In Docker Compose:** If the certs volume is destroyed (`docker compose down -v`), a new CA is generated on the next startup. All services restart together, obtaining new instance certs from the new CA. This is correct behavior — the volume and the CA have the same lifecycle.

**In Kubernetes:** cert-forge persists the CA cert and key in the K8s Secret (`ca.crt` and `ca.key`). On startup, cert-forge reads the existing CA from the Secret. If a valid CA exists and is not within 30 days of expiry, it loads the existing CA rather than generating a new one. The CA is stable across cert-forge pod restarts. Only the enrollment material and static certs are regenerated on restart.

This means cert-forge can be restarted in K8s without requiring a constellation-wide restart. Services continue using their in-memory instance certs — they never see the new CA because it is the same CA.

### Adding cert-forge to a New TCA Project (Docker Compose)

1. Copy the cert-forge service directory from the SETI repository. cert-forge has no constellation-specific code — copy it verbatim.
2. Choose three consecutive ports from the constellation's port pool. Update `forge.json` with the constellation CA configuration.
3. Replace `cert-init` with cert-forge in `docker-compose.yml`. Use `restart: unless-stopped`, not `restart: "no"`. cert-forge is a persistent service, not an init container.
4. All other services add `depends_on: cert-forge: condition: service_healthy`.
5. Mount the certs volume read-only into every service container. Pass `CERT_FORGE_URL`, `ENROLLMENT_PORT`, `PUBLIC_PORT`, `ENROLLMENT_CERT`, `ENROLLMENT_KEY`, and `ENROLLMENT_CA` as environment variables.
6. Each service calls `obtainCerts(serviceName)` at startup before building any TLS configuration.
7. Remove all pre-generated cert files from the shared volume — only `ca.crt`, `enrollment-ca.crt`, `enrollment.crt`, `enrollment.key`, and static certs (star-gazer) belong on the volume. Instance certs never touch the volume.

### Adding cert-forge to a New TCA Project (Kubernetes)

1. Copy the cert-forge service directory from the SETI repository.
2. Add the cert-forge Helm templates from the SETI chart: `ServiceAccount`, `Role`, `RoleBinding`, `Deployment`, `Service`. The Role grants `get`, `create`, `update` on secrets in the namespace only.
3. cert-forge writes all cert material to a K8s Secret on startup. Every other service mounts the Secret at `/certs` read-only.
4. cert-forge's readiness probe uses `httpGet` on the public port (`/ca`, plain HTTP). Every other service uses `exec: ["/healthcheck"]`.
5. An init container on each service (`wait-for-cert-forge`) polls the public port until it responds before the main container starts.
6. The certs volume from Docker Compose is replaced entirely by the K8s Secret. No PVC required for cert material.

### certforge Client — Language Reference

The certforge client is implemented in all four TCA languages. Copy the appropriate file from the SETI codebase — no modification required except service name.

| Language | Reference file | Key function |
|----------|---------------|--------------|
| Go | `augur-canis/certforge.go` | `obtainCerts(serviceName string) *CertMaterial` |
| TypeScript | `signal-clearance/certforge.ts` | `obtainCerts(serviceName: string): Promise<CertMaterial>` |
| Python | `ai-lien/certforge.py` | `obtain_certs(service_name: str) -> CertMaterial` |
| Elixir | `feed-wrangler/cert_forge.ex` | `CertForge.obtain_certs(service_name)` |

All four implementations follow the same flow: fetch CA cert → request instance cert over enrollment mTLS → hold both in memory → build TLS configs. The Go implementation is the canonical reference.

### What cert-forge Replaces

| Old approach | cert-forge approach |
|-------------|---------------------|
| `cert-init` init container generates all certs | cert-forge persistent service issues certs on demand |
| All private keys written to shared volume | Instance private keys never touch shared storage |
| Any compromised service can read all keys | Each service holds only its own key material |
| Fixed cert set — adding a service requires regenerating all certs | cert-forge issues certs to any service that enrolls |
| `restart: "no"` — one-shot | `restart: unless-stopped` — persistent |
| K8s: volume mount with `ReadWriteMany` PVC | K8s: K8s Secret — no PVC, no RWX storage class required |

---

## 22. SETI — Standard TCA Monitoring Constellation

S.E.T.I. (Search for Erroneous Tessellated Interactions) is the monitoring constellation for TCA applications.  It sits outside the constellations it monitors and watches them through the Augur Canis contract test layer, the Plot test behavioral layer, and the Observability event stream.

SETI is itself a TCA constellation — it follows the same contract-first discipline it enforces on others.  It has its own repository: github.com/Pelentan/tca-seti.

Full reference: [SETI-GUIDELINES.md](SETI-GUIDELINES.md).

### What SETI Adds to AC

AC is the verification layer — it tests contracts, detects anomalies, fires alerts.  SETI is the operational shell around AC:

- **Intelligence layer** — Interactions routes failures through three paths (immediate, AI analysis, silence).  AI-lien performs diagnostic analysis using the double-tap pattern and reads Lore before every analysis.  Lore stores institutional memory in PostgreSQL.
- **Wr4ngler interface** — Dashboard (live event stream), Ring (Trial, Reports, LaE), Admin.
- **Plot tests** — Multi-step behavioral tests verifying real user flows across service boundaries with call chain verification.
- **Notifier** — Human alert pathway (permanent stub — operator implements for their environment).
- **Integration Job** — Versioned external API for monitored application tooling.

### What SETI Does Not Replace

SETI does not replace the observability instrumentation inside a monitored constellation.  Each monitored constellation still needs:
- Its own Observability Job
- Its own `reportEvent()` pattern in each Job
- Its own `x-tca-observability` block in each contract

SETI subscribes to the monitored constellation's event streams externally.  It does not reach inside the constellation to instrument it.

### SETI as Gateway to TCA

The adoption path:  AC first (standalone, one dependency, self-registration snippet).  SETI when the operator is ready for the full intelligence layer.  TCA discipline applies the framework to the monitored constellation.  Each step adds capability without requiring the next step.

---

## 23. Testing Philosophy — Three Layers

TCA testing has three distinct layers.  Each serves a different purpose and requires a different author.

### Layer 1: Contract Tests (AC — Augur Canis)

**What:** Hardcoded behavioral assertions — positive tests for endpoint correctness, negative tests for error handling.

**Who authors:** Engineers who read the contract and write assertions independently from it.

**What AI can do:** Scaffold the positive tests from the contract.  Cannot independently verify the contract.

**Key principle:** Tests generated from contracts re-state them.  Independent authorship is what makes them verification rather than documentation.

**Where they live:** `augur-canis/contracttests.go` — hardcoded, updated by hand when contracts change.

### Layer 2: Plot Tests (SETI — Plot Test Job)

**What:** Multi-step behavioral flows that verify real user journeys across service boundaries, including call chain verification.

**Who authors:** Engineers with domain knowledge of the monitored application; AI can scaffold from contracts and running system.

**What AI can do:** Build the happy-path Plot from contracts and observed behavior.  Cannot verify adversarial scenarios or real-world usage patterns.

**Key principle:** Plots prove the happy path works.  The Show Wr4ngler finds every path that isn't happy.

**Where they live:** `contracts/plots/{application}/` — JSON files, versioned with the constellation.

### Layer 3: Show Wr4ngler Testing (Human — not automatable)

**What:** Adversarial testing from the user perspective — edge cases, concurrent load, real-world usage patterns, security boundary probing.

**Who authors:** The Show Wr4ngler — a defined role, not a job title.

**What AI can do:** Nothing that cannot be derived from the contract.  The Show Wr4ngler brings what AI cannot: operational experience, adversarial intent, and the ability to ask "what would a determined user do here?"

**Key principle:** Plots generated by AI can only verify what the contract already describes.  Show Wr4ngler tests question the contract.

**When Show Wr4ngler tests reveal defects:** Convert to Plot tests or AC contract tests.  Tests that require human judgment or adversarial tooling remain in the Show Wr4ngler's manual playbook.

### What Goes Where

| Question | Answer | Layer |
|----------|--------|-------|
| Does this endpoint return the correct status? | Contract test | AC |
| Does this endpoint return 4xx on bad input? | Contract test (negative) | AC |
| Does this user flow produce the expected sequence of calls? | Plot test | SETI |
| What happens when the user does something unexpected? | Show Wr4ngler | Human |
| Does the mTLS boundary reject plain HTTP? | Rodeo Clown (future) | Security sidecar |

---

## 24. Rodeo Clown (Future — Security Boundary Verification)

Rodeo Clown is a planned SETI sidecar for security boundary verification.  Named deliberately — a Rodeo Clown operates outside the normal structure and absorbs hits.

Where AC's contract test suite verifies that endpoints respond correctly from inside the constellation (AC always presents a valid certificate), Rodeo Clown operates without a valid constellation certificate.  It probes the transport security boundary from outside.

**What it tests:**
- Does the gateway reject connections without a valid client certificate?
- Does it reject self-signed certificates not issued by the constellation CA?
- Does it reject expired certificates?
- Does it reject connections over plain HTTP where mTLS is required?

**What it is not:**  Chaos Monkey.  Chaos Monkey tests failure recovery.  Rodeo Clown tests the transport security boundary.  The scope is deliberately narrow.

**How it reports:**  Findings route to SETI via the Integration Job's external API — the only SETI endpoint accessible without a constellation certificate.  Rodeo Clown cannot authenticate to the mTLS observability endpoint because it deliberately does not have valid credentials.

**Why it does not exist yet:**  Security boundary verification requires adversarial tooling and real traffic to be meaningful.  Building it before there are constellations to probe against would produce a tool with nothing to test.  When TCA Vox or another monitored constellation is running in production, Rodeo Clown has a target.

**Repository:**  Will have its own repository, linked from both the AC and SETI repositories.  It is a SETI sidecar, not a core SETI Job.