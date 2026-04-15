# SETI — Search for Erroneous Tessellated Interactions

A TCA constellation that monitors other TCA constellations.

SETI provides continuous behavioral verification for registered TCA applications via Contract Tests and Plot Tests. It aggregates observability streams across all monitored constellations, detects anomalies, routes failures through a three-path triage system, and maintains institutional memory of everything it has seen. It monitors itself first.

SETI is itself a TCA application. It registers in its own Policy Job, runs its own Contract Tests and Plot Tests on schedule, and routes its own failures through its own Interactions Job. The methodology tests itself.

---

## Architecture

20 containers across three infrastructure services and 17 application Jobs.

### Infrastructure

| Service | Role | Port |
|---------|------|------|
| cert-forge | PKI — issues instance certs over mTLS, holds all key material | 4014 (sign), 4015 (enroll), 4016 (CA/public) |
| redis | Pub/sub backbone, metric streams, alert state | 6379 |
| postgres | Lore institutional memory (persistent) | 5432 |

### Application Jobs

| Job | Language | Port | Role |
|-----|----------|------|------|
| gateway | Go | 4000 | External entry point — auth, routing, SSE fan-out |
| signal-clearance | TypeScript | 4001 | Authentication and clearance level management |
| policy | Go | 4002 | Application registry and test schedule |
| contract-test | Go | 4003 | Contract test dispatch and scheduling |
| plot-test | Go | 4004 | Plot execution with call chain verification |
| plot-store | Go | 4005 | Plot definition storage and versioning |
| signal-aggregator | Go | 4006 | Event stream subscription and call chain verification |
| feed-wrangler | Elixir | 4007 | SSE feed lifecycle (OTP-supervised) |
| results | Python | 4008 | Test run result storage and retrieval |
| interactions | Go | 4009 | Failure triage routing — three paths |
| augur-canis | Go | 4010 | Behavioral verification, health monitoring |
| seti-observability | Go | 4011 | Inter-service event ingestion and pub/sub |
| integration | Go | 4013 | Versioned external API for monitored application tooling |
| ai-lien | Python | 4252 | AI diagnostic engine (double-tap pattern, Lore-aware) |
| lore | Go | 4110 | Institutional memory (PostgreSQL-backed) |
| notifier | Go | 4300 | Human notification (permanent stub — implement for your environment) |
| ui | TypeScript/React | 4000 (via gateway) | Wr4ngler interface |

---

## Prerequisites

### For k3d development (recommended)

- Docker Desktop
- k3d: `winget install k3d` or https://k3d.io
- kubectl (already installed with Docker Desktop)
- Helm: `winget install Helm.Helm`

### For Docker Compose (solo Job development only)

- Docker Desktop

---

## Running SETI — k3d (Full Constellation)

k3d is the standard development environment for the full SETI constellation. It runs a real multi-node K8s cluster inside Docker containers alongside your existing Docker Compose workflows without interference.

### First-time cluster setup

```bash
./scripts/k3d-setup.sh
```

This creates a `seti` cluster with 2 agent nodes, a local image registry, and maps `localhost:4000` to the gateway. Run once per machine. Safe to re-run — idempotent.

### Build and push images

```bash
./scripts/build-push.sh
```

Builds all 18 service images and pushes them to the local k3d registry. Run after any code change.

### Install the chart

```bash
helm install seti ./charts/seti -n seti \
  -f charts/seti/values/dev.yaml \
  --set postgresCredentials.password=yourpassword \
  --set jwtSecret=yourjwtsecret \
  --create-namespace
```

Watch startup:

```bash
kubectl get pods -n seti -w
```

Expect approximately 90 seconds from install to all pods `1/1 Running`. Pods start in dependency order — cert-forge first, then augur-canis, then everything that depends on augur-canis. This is correct behavior, not slow startup.

### Access SETI

```
https://localhost:4000
```

Same URL as Docker Compose. No port-forward required.

Dev login credentials (AUTH_MODE=dev):

| Clearance | Username | Password |
|-----------|----------|----------|
| connie-wr4ngler | connie | any |
| sec-wr4ngler | sec | any |
| admin | admin | any |

### Upgrade after code changes

```bash
./scripts/build-push.sh
helm upgrade seti ./charts/seti -n seti \
  -f charts/seti/values/dev.yaml \
  --set postgresCredentials.password=yourpassword \
  --set jwtSecret=yourjwtsecret
```

### Tear down

```bash
helm uninstall seti -n seti
kubectl delete namespace seti --wait
```

To destroy the cluster entirely:

```bash
k3d cluster delete seti
```

---

## Running SETI — Docker Compose (Solo Job Development)

Docker Compose is for iterating on a single Job and its immediate dependencies. It is not the environment for full constellation testing.

```bash
cp .env.example .env
# Edit .env — set POSTGRES_PASSWORD and JWT_SECRET
docker compose up --build
```

The Compose file is not kept in sync with the Helm chart. It is a development convenience, not a deployment artifact. The Helm chart is the source of truth for the constellation definition.

---

## Secrets and Configuration

All secrets are supplied at install time via `--set`. They are never stored in values files or committed to the repository.

| Secret | Flag | Notes |
|--------|------|-------|
| Postgres password | `--set postgresCredentials.password=` | Initializes the postgres data directory on first install |
| JWT signing secret | `--set jwtSecret=` | Used by gateway and signal-clearance |
| remote-apps.json | `--set-file remoteAppsJson=remote-apps.json` | External constellation registry, contains API tokens |

For dev with no external constellations: `--set remoteAppsJson='{}'`

---

## Registering a Monitored Application

Applications are registered via the Admin UI (requires `admin` clearance) or the Policy Job API. SETI requires:

- Application tag and display name
- AC endpoint for behavioral verification
- Repository URL and access token (for contract and plot ingestion)
- Schedule configuration per test tier

SETI self-registers as the first application on startup. Its own Contract Tests and Plot Tests verify all 17 Jobs continuously.

---

## Clearance Levels

SETI uses clearance levels rather than roles.

| Level | Name | Capabilities |
|-------|------|--------------|
| 1 | connie-wr4ngler | Dashboard, feeds, event stream |
| 2 | sec-wr4ngler | + Ring (Trial, Reports, LaE), alert acknowledgment |
| 3 | admin | + Admin page, application registration, AI configuration |

---

## Network Architecture

### In k3d (development and production)

One flat K8s network with NetworkPolicy rules restricting traffic. 22 policies cover the full constellation — default-deny ingress, explicit allow per service. augur-canis has unrestricted egress to reach every service for health checks and contract test execution. Policies are enforced when the CNI supports NetworkPolicy (Calico, Cilium) — automatic in staging and production environments.

### In Docker Compose (solo Job development only)

Two bridge networks: `seti-internal` (all Jobs) and `ac-net` (all Jobs + augur-canis). The ac-net design is an approximation of the NetworkPolicy model — a shared broadcast domain rather than true point-to-point isolation. Sufficient for development, not the production architecture.

---

## Key Concepts

**Contract Tests** — Behavioral assertions against OpenAPI 3.1 contracts. Positive tests verify 2xx on all endpoints. One negative test per Job verifies malformed POST returns 4xx not 5xx. Run on schedule. Production-safe.

**Plots** — Multi-step behavioral flows that verify real user journeys across service boundaries, including call chain verification. Authored by engineers with domain knowledge; AI scaffolds from contracts and observed behavior.

**Augur Canis** — The behavioral verification layer. Executes contract tests, detects anomalies, fires alerts. Inverts the industry health-check model — the healthcheck binary publishes a request, AC verifies behavior over a dedicated channel. No open ports required on monitored services.

**Lore** — PostgreSQL-backed institutional memory. Stores trend points, baselines, incidents, and patterns. AI-lien reads Lore before every analysis and writes assessments back. The loop — Lore feeds AI-lien, AI-lien feeds Lore — makes every analysis better than the last.

**Notifier** — A permanent stub. SETI does not know what "notify a human" means in your environment. Implement the handler body for your notification stack. The contract is the interface. The implementation is yours.

---

## Methodology

SETI is built on Tessellated Constellation Architecture. Every Job has an OpenAPI 3.1 contract written before implementation. Jobs communicate via mTLS-authenticated REST. All inter-service calls are reported to the Observability Job. Security posture is defined per Job in `x-tca-security` contract blocks.

Full methodology: [TCA Guidelines](tca-guidelines.md)
SETI-specific reference: [SETI Guidelines](SETI-GUIDELINES.md)
Augur Canis reference: [AC Guidelines](AC-GUIDELINES.md)
K8s migration reference: [K8s Migration Primer](K8S-MIGRATION-PRIMER.md)

---

## Language and Dependency Constraints

### No Rust Dependencies
No TCA Job or supporting binary may introduce a Rust dependency, directly or transitively. This includes Python packages with Rust extensions (`cryptography`, `pydantic-core`, `orjson`) and any npm package compiled from Rust. The Go standard library, Python standard library, and Node.js built-ins are the correct tools for cryptographic operations.

### The Binary Pattern
Cross-language operations (health checking, cert enrollment) are handled by small static Go binaries compiled with `CGO_ENABLED=0`. These binaries are copied into every container regardless of primary language. One implementation, available everywhere.

---

*SETI is developed through Engineer-AI partnership as a demonstration of TCA methodology maturity.*

*Michael E. Shaffer / AI Implementation Specialist / Systems Architect / Connie Wr4ngler*
