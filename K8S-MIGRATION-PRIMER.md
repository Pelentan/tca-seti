# TCA / SETI / AC — Kubernetes Migration Primer

**Purpose:** Session primer for the next conversation on migrating TCA, SETI, and AC from Docker Compose to a Kubernetes-based development and production structure.  
**Current State:** SETI is complete and healthy at 20 containers on Docker Compose.  AC and SETI each have their own repositories.  TCA guidelines are current.  
**Goal:** Define the K8s architecture, tooling choices, and migration path.  Helm charts as the primary deliverable.

---

## What Was Decided in the Previous Conversation

These decisions are already made.  They should be treated as constraints in the next session, not open questions.

**Finish SETI in Docker Compose, then migrate.**  The migration to K8s is part of TCA 2.0.  SETI's current Docker Compose state is the migration source.

**AI partner handles Helm chart authoring.**  The engineer architects, the AI implements.  Same partnership model as the codebase.

**k3d for local development.**  k3s running inside Docker containers.  Spins up in seconds, supports multiple nodes, loads local images without a registry, tears down cleanly.  The inner dev loop stays fast.  (`k3d` not `minikube` — minikube is single-node and doesn't reflect the multi-node target.)

**Helm over raw manifests.**  Reasons:  templating collapses the repetition across 20 nearly-identical Job deployments; release management gives atomic rollback; per-environment values files (dev/staging/prod) without duplicating manifests; dependency management for cert-manager, Redis, PostgreSQL.

**K8s network policies replace Docker Compose's Docker network isolation.**  The ac-net architecture (AC joins a shared network with all services) maps directly to K8s NetworkPolicy objects.  K8s removes the 32-network ceiling that forced compromises in Docker Compose.

**cert-forge integrates with cert-manager in production.**  cert-forge's signing backend points to cert-manager or the organizational CA.  No other service changes.  The constellation is decoupled from infrastructure PKI choices.

**K8s container replacement detection via container ID change.**  AC already tracks `container_id` per service from the health check payload (`$HOSTNAME` = container ID in Docker/K8s).  When a service reappears with a new container ID after an active alert, AC auto-resolves as `container_replaced`.  No Kubernetes API access required.  No RBAC.  No sidecar.  Already implemented.

**PreStop lifecycle hook as optional enhancement.**  A tiny binary published before container termination gives AC advance notice of pod replacement.  Optional — the container ID detection catches replacements even when the hook cannot execute.

**SETI as a K8s-native operator (future).**  A `k8s-conductor` Job that subscribes to SETI's existing feeds and translates decisions into Kubernetes API calls.  This is TCA 2.0+ territory — architect it but don't build it yet.

---

## Open Questions for the Next Conversation

These were explicitly flagged as points for discussion.  Approach each one fresh.

### Cluster Target
- **k3s** — Rancher/SUSE, Apache 2.0, K8s-compatible, designed for edge and resource-constrained environments, eliminates most K8s operational overhead.  Strong candidate for the "street" deployment (small business, no dedicated DevOps team).
- **Full K8s** — EKS, GKE, AKS, or bare metal.  Higher operational cost, full ecosystem.
- **Docker Swarm** — Mirantis committed through 2030, overlay networks solve the 32-network ceiling, compose file format is largely compatible.  Discussed as a viable intermediate tier.
- **Nomad** — Single binary, no etcd, genuinely simpler base form.  BSL 1.1 license (not Apache 2.0) and HashiCorp ecosystem dependency (Consul + Vault for service discovery and secrets) are concerns.

The TCA 2.0 question:  should the target be a single cluster type, or should TCA define deployment tiers (Compose → Swarm → k3s → full K8s)?

### cert-manager
- Available in the target environment, or needs to be added?
- Self-signed CA vs organizational CA vs Let's Encrypt?
- cert-forge's K8s integration points — cert-forge's signing backend should point to cert-manager in production.

### Storage
- **Lore (PostgreSQL)** — needs a persistent volume with a reliable storage class.  What's available?  Local path, NFS, cloud block storage?
- **Redis** — AOF persistence for SETI's metric streams and alert state.  Same storage question.
- Backup strategy for both.

### Single Cluster vs Multi-Cluster
- Single cluster with namespace isolation (SETI in one namespace, monitored applications in others)?
- Separate cluster for SETI vs monitored constellations?
- Federation between clusters?

### Helm Chart Structure
- One chart for the entire SETI constellation, or per-Job charts with dependencies?
- Shared chart library for the common patterns (cert-forge client setup, enrollment env vars, health check config, network policy shape)?
- Chart versioning strategy — locked to a TCA version, or independently versioned?

### Network Policy Design
- Direct translation of Docker Compose networks to K8s NetworkPolicy objects
- AC's access pattern: AC can reach all services; services cannot reach each other through AC's network
- Gateway is the sole external entry point — `Ingress` resource or `LoadBalancer` service?
- mTLS terminates at the service level, not at the ingress — does this change with K8s?

### Resource Limits and Pod Affinity
- TCA Section 9 defines the K8s deployment patterns: hot-path domain (hard affinity, same node), security/financial domain (co-located but isolated from hot path), async domain (float freely)
- SETI's hot path: gateway, signal-clearance, policy, seti-observability
- SETI's async: feed-wrangler, signal-aggregator, ai-lien (long-running)
- Resource limits per Job — establish starting points from observed Docker Compose behavior

### SETI as Operator (Architecture Only — Don't Build Yet)
- `k8s-conductor` Job subscribing to SETI's feeds and calling the K8s API
- Safety model — what changes can be made automatically vs require human confirmation?
- RBAC scope — minimum permissions for the decisions SETI should make autonomously
- The observation from the prior conversation:  the existing tools (CAST AI, VPA) scale based on CPU.  k8s-conductor scales based on understanding — it knows AC's canned query cycle from user load, knows which service is the bottleneck vs which is upstream of the bottleneck.

---

## Current Stack Reference

Everything that needs to move.

### SETI Containers (20 total)

| Service | Language | Port | State | Notes |
|---------|----------|------|-------|-------|
| cert-forge | Go | 4014/4015/4016 | Stateless | Three-port PKI architecture |
| postgres | — | 5432 | Stateful | Lore persistence |
| redis | — | 6379 | Stateful | Metric streams, alert state, pub/sub |
| augur-canis | Go | 4010 | Stateless | AC — standalone repo |
| seti-observability | Go | 4011 | Stateless | |
| signal-clearance | TypeScript | 4001 | Stateless | |
| gateway | Go | 4000 | Stateless | External entry point |
| ui | TypeScript | 4020 | Stateless | Served via gateway |
| policy | Go | 4002 | Stateless | |
| contract-test | Go | 4003 | Stateless | |
| results | Python | 4008 | Stateless | |
| signal-aggregator | Go | 4006 | Stateless | |
| plot-store | Go | 4005 | Stateless | |
| plot-test | Go | 4004 | Stateless | |
| interactions | Go | 4009 | Stateless | |
| feed-wrangler | Elixir | 4007 | Stateless | OTP supervision is load-bearing |
| integration | Go | 4013 | Stateless | |
| ai-lien | Python | 4252 | Stateless | Long-running analysis calls |
| lore | Go | 4110 | Stateless | Depends on postgres |
| notifier | Go | 4300 | Stateless | Permanent stub |

### Networks (Docker Compose)

- `seti-internal` — primary service network, all Jobs
- `ac-net` — AC's access network, all Jobs plus augur-canis

### Volumes

- `certs` — cert-forge generated material (readonly mounts)
- `postgres_data` — Lore persistence

### Environment Variables That Will Change

- `REDIS_URL` — `redis:6379` → K8s Service DNS
- `CERT_FORGE_URL` — same pattern, K8s Service
- `POSTGRES_*` — will use K8s Secret references, not plaintext env vars
- `JWT_SECRET` — K8s Secret, not `.env` file
- `AC_UI_PORT=4666` — remove before production deployment

---

## What a Good Session Outcome Looks Like

By the end of the K8s migration conversation:

1. **Cluster target decided** — k3s, full K8s, or tiered.  Rationale documented.
2. **Helm chart structure decided** — single chart, per-Job charts, or library approach.
3. **cert-manager integration designed** — how cert-forge connects.
4. **Storage strategy decided** — for PostgreSQL and Redis.
5. **Network policy design complete** — AC access pattern, gateway ingress, mTLS posture.
6. **Working k3d local dev environment** — `helm install` produces a running SETI constellation.
7. **PHASE-PLAN.md for the migration** — phased, observable, each phase independently deployable.
8. **k8s-conductor architecture documented** — not built, but the design is on paper.

---

## How to Start That Conversation

Load this document.  Load AC-GUIDELINES.md, SETI-GUIDELINES.md, and tca-guidelines.md.  The AI should read all four before the first exchange.  Then:

> "I want to migrate SETI and TCA to a Kubernetes-based structure.  Start by reading the K8s primer and the three guidelines documents.  Then walk me through the open questions in the order you think is most important to resolve first."

The AI will have full context on what's already decided, what's open, what the current stack looks like, and what a successful session produces.
