# TCA / SETI / AC — Kubernetes Migration Primer

**Purpose:** Reference document for the SETI K8s migration. Records all decisions made, the rationale behind them, and lessons learned. The open questions from the previous version are now closed.
**Status:** Complete — Migration delivered 2026-04-14.
**Time to completion:** Approximately 4 hours, engineer-AI partnership.

---

## What Was Built

The full SETI constellation (20 containers) runs on k3d with the same operational behavior as Docker Compose. The Helm chart is the authoritative constellation definition. cert-forge writes cert material to K8s Secrets. NetworkPolicy declares the communication topology. The dev/prod gap is eliminated.

See `charts/seti/` for the complete Helm chart.
See `scripts/` for cluster setup and image build tooling.
See `cert-forge/k8s.go` for the K8s Secret integration.
See `PHASE-PLAN.md` Phase 10 for the complete architecture decision record.

---

## All Decisions — What Was Decided and Why

### Finish SETI in Docker Compose, then migrate
Correct. The migration source was a healthy, fully-functional 20-container constellation. Migrating a moving target would have conflated application bugs with infrastructure bugs.

### k3d for local development
Correct. k3d runs k3s inside Docker containers. Multi-node, true K8s API, fast spin-up, tears down cleanly. k3d clusters are named and independent — developers do not toggle Docker Desktop between K8s and Compose modes when switching projects. Docker Desktop's built-in K8s is single-node and shares the Docker socket with regular Docker, making it unsuitable for multi-node topology testing and disruptive to other projects.

### Docker Desktop built-in K8s considered and rejected
Single-node means pod affinity rules, domain scheduling, and multi-node topology are untestable. The K8s toggle is a single shared cluster — enabling it for SETI disables it for everything else on the machine. k3d has neither of these problems.

### Helm over raw manifests
Correct. Templating collapses 20 nearly-identical Job deployments into a consistent pattern. Values files provide per-environment behavior without duplicating manifests. The chart is the handoff artifact between tiers — ArgoCD points at the same chart for staging and production.

### cert-forge writes to K8s Secret
Correct. The shared volume was a Docker Compose constraint, not an architectural choice. K8s Secrets are the correct primitive. cert-forge gets a namespace-scoped ServiceAccount with `get`, `create`, `update` on secrets only. Detection is automatic via the projected ServiceAccount token — same binary, same image, environment-appropriate behavior.

### All `.env` secrets become K8s Secrets
Correct. JWT secret, postgres credentials, remote-apps.json (which contains registry tokens) — all K8s Secrets supplied via `--set` at install time. Never in values files. Never in git.

### ac-net replaced by NetworkPolicy
Correct and an improvement. ac-net was a shared broadcast domain — every service on ac-net could technically reach every other. NetworkPolicy gives true point-to-point isolation. augur-canis can reach gateway. augur-canis can reach signal-clearance. Gateway cannot reach signal-clearance through augur-canis's path. 22 policies cover the full constellation topology.

### NetworkPolicy: dev unenforced, staging/production enforced
Correct. k3d uses flannel which does not enforce NetworkPolicy. The policies are in the chart as authoritative documentation of the topology regardless of enforcement. A CNI that supports NetworkPolicy (Calico, Cilium) enforces them automatically in staging and production without any chart changes.

### Docker Compose is Tier 0, not deprecated
Correct. Compose stays as a development convenience for single-Job iteration. The Helm chart is the source of truth. The Compose file is not kept in sync with the chart.

### ArgoCD for staging and production, not development
Correct. ArgoCD's GitOps overhead is friction in the inner dev loop. `helm upgrade` is the dev workflow. ArgoCD picks up the same chart for staging and production.

### k3s as street deployment target
Correct. k3s is Apache 2.0, K8s-compatible, and runs on a single modest VM or small cluster. A small operator deploying SETI does not need AWS or GKE.

### Docker Swarm — not adopted
The 32-network ceiling was the original argument for Swarm. K8s NetworkPolicy removes that ceiling correctly. Swarm doesn't compose with ArgoCD or the k8s-conductor operator concept.

### Nomad — not adopted
BSL 1.1 license. Mandatory HashiCorp ecosystem (Consul + Vault) for service discovery and secrets. Architectural fork that doesn't align with the K8s target.

---

## The cert-forge Model — Clarified

The certs volume in Docker Compose held:
- `ca.crt` — constellation CA public cert
- `enrollment-ca.crt` — enrollment CA cert
- `enrollment.crt` + `enrollment.key` — bootstrap credential every service uses to call `/instance-cert`
- `star-gazer.crt` + `star-gazer.key` — SETI's federation identity (static, longer lifecycle)

In K8s all of this goes into the `seti-certs` Secret. cert-forge writes it on startup. Pods mount the Secret as a read-only volume at `/certs`. The enrollment flow is unchanged.

The star-gazer cert is SETI's federation identity for connecting to external constellations. It is not the enrollment cert. These are different things with different purposes.

---

## Startup Ordering

Init containers replace `depends_on: condition: service_healthy`. The dependency chain is derived directly from the Compose file. Reusable wait helpers in `charts/seti/templates/_init.tpl`.

cert-forge uses `httpGet` on `/ca` (plain HTTP, port 4016) for its readiness probe. Every other service uses `exec: ["/healthcheck"]` — the same binary, same Redis pub/sub mechanism, same AC Watchdog verification as Docker Compose.

The Startup contract test run fires before AC has completed its first check cycle and shows failures. This is correct behavior. Subsequent scheduled runs pass cleanly. No artificial delay is the right answer.

---

## Deployment Tiers (TCA 2.0)

| Tier | Tool | When | Notes |
|------|------|------|-------|
| 0 — Solo Job | Docker Compose (partial stack) | Active Job development | Fast inner loop, no K8s overhead |
| 1 — Dev Constellation | k3d + Helm | Full constellation testing | `helm upgrade`, `k9s` for observability |
| 2 — Staging | k3s or managed K8s + ArgoCD | Pre-production validation | NetworkPolicy enforced, ArgoCD drift detection |
| 3 — Production | k3s (street) or managed K8s + ArgoCD | Live | Same chart, different values |

---

## Open Questions — Now Closed

**Cluster target:** k3d for dev, k3s or managed K8s for staging/production. Swarm and Nomad rejected.

**cert-manager:** Not required. cert-forge writes to K8s Secret directly. cert-manager integration remains available for operators who need it but is not a dependency.

**Storage:** PostgreSQL uses StatefulSet with PVC (ReadWriteOnce). Redis stateless in dev, AOF persistence for staging/production via values override.

**Single vs multi-cluster:** Single cluster with namespace isolation for most deployments. Separate cluster required for constellations handling regulated data (PII, PHI) — namespace isolation is real but insufficient for compliance scope containment.

**Helm chart structure:** Single chart, per-service subdirectory. Shared helpers. Values files per environment. No library chart.

**Network policy design:** 22 NetworkPolicy objects. Default-deny ingress. Explicit allow per service. Unenforced in dev, automatic in staging/production.

**Resource limits and pod affinity:** Deferred to Phase 11. Baseline behavior established. Limits and TCA Section 9 domain affinity rules set from observed behavior.

**SETI as operator (k8s-conductor):** Architecture deferred. Concept is sound — a TCA Job subscribing to SETI feeds and translating decisions into K8s API calls, scaling based on understanding rather than CPU. TCA 2.0+ territory.

---

## Lessons Learned

- `helm install` without `-n <namespace>` deploys to `default`. Always specify `-n seti` explicitly.
- k3d image pull address is `seti-registry:5000`, not `localhost:5000`. Push and pull addresses differ. Must be correct in `values/dev.yaml`.
- `exec: ["/healthcheck"]` is the correct probe for all mTLS services. `httpGet` with `scheme: HTTPS` fails the handshake without a client cert. `tcpSocket` bypasses AC.
- Init container TCP check (`nc`) is sufficient. AC handles health verification once the service is running.
- Helm multi-line `printf` is unreliable for Secret values. Assign variables at the top of the template with `{{- $var := ... -}}` and reference them cleanly.
- Startup test failures are correct. Degraded-healthy during startup is not a bug.
- ConfigMaps for contracts and plots are cleaner than volume mounts. 440KB fits under the 1MB limit.
- A 20-service polyglot constellation migrated from Docker Compose to K8s in approximately 4 hours via engineer-AI partnership. Traditional team estimate: 2-6 weeks.

---

## Next K8s Work Items

1. Resource limits per Job (Phase 11) — set from observed behavior
2. Pod affinity rules per TCA Section 9 domain — hot-path, security, async
3. k8s-conductor architecture document — not yet built
4. Rodeo Clown — security boundary verification sidecar (separate repo)

---

## How to Start the Next K8s Conversation

Load this document plus `PHASE-PLAN.md`, `SETI-GUIDELINES.md`, `AC-GUIDELINES.md`, and `tca-guidelines.md`. The code zip contains the full current state including the Helm chart. Read all four before the first exchange.

---

## Addendum — NetworkPolicy Lessons Learned (2026-04-14)

These were discovered during the first live run of the constellation under NetworkPolicy and are not theoretical.

**k3d enforces NetworkPolicy.** The primer stated flannel does not enforce NetworkPolicy. This is wrong. k3d ships with a network policy controller that does enforce it. Policies are active in dev. Plan accordingly.

**Redis must allow ingress from all constellation pods.** The healthcheck binary connects to Redis directly from inside every service pod to publish check requests and subscribe to results. The initial Redis ingress policy only listed services with known Redis usage. This caused the healthcheck to fail for unlisted services, which triggered liveness probe failures and cascading restarts across the constellation. Fix: use the `app.kubernetes.io/part-of: seti` label selector for Redis ingress — every pod in the constellation needs Redis access.

**augur-canis must allow ingress from all constellation pods.** Every service's init container probes `augur-canis:4010` via TCP to gate startup. The initial augur-canis ingress policy only listed services that call augur-canis at runtime. Init containers run inside the service pod and carry the service pod's labels, but services like lore, notifier, feed-wrangler, and results are not runtime callers of augur-canis — only their init containers need TCP access to confirm it's up. Fix: use the `app.kubernetes.io/part-of: seti` label selector for augur-canis ingress.

**The label selector pattern applies to universal dependencies.** Any service that sits in the dependency chain of all other services — cert-forge, redis, augur-canis, seti-observability — should use `app.kubernetes.io/part-of: seti` for ingress rather than enumerating callers. Enumerated caller lists are incomplete by design because init containers create transient access patterns that don't exist at runtime.

**Self-registration exhausts retries before NetworkPolicy is applied.** Services call `selfRegisterWithAC()` on startup with 10 retry attempts. If NetworkPolicy is restrictive when a service starts, it exhausts retries and gives up — the service runs but is never registered with AC, so AC marks its contract tests as `skip: job_not_deployed`. The fix is correct NetworkPolicy from the start, not more retries. Once the policy is correct, a pod restart causes successful registration on the first attempt.

**cert-forge restart without CA persistence invalidates the entire constellation.** When cert-forge restarted, it generated a new CA. Services hold instance certs from the old CA in memory. Inter-service mTLS fails with `unknown certificate authority` because each service's cert is signed by a CA the other services no longer trust. Fix: cert-forge now persists the CA cert and key in the K8s Secret (`ca.crt` and `ca.key`). On restart it loads the existing CA rather than generating a new one. The CA is stable across cert-forge pod restarts. Only enrollment material and static certs are regenerated.

**`helm upgrade` does not restart pods when only a ConfigMap changes.** Pods mount ConfigMaps as volumes. K8s updates the mounted files when the ConfigMap changes, but does not restart the pod. Services that read config at startup (like plot-store reading plot JSON files) require a manual `kubectl rollout restart` or a pod template annotation change to pick up ConfigMap updates.

**Plot test expected_status must match the Go struct type.** The PlotStep struct defines `expected_status` as `int`. Changing it to a JSON array `[200, 201]` causes `json: cannot unmarshal array into Go struct field` and silently drops the plot from plot-store's loaded set. Verify the struct type before changing JSON field shapes.

**Plot tests that assume clean state are fragile across environments.** Docker Compose's `down -v` workflow incidentally reset in-memory state between test sessions. K8s does not. A plot that expects 201 on `POST /resource` will fail if the resource already exists from a previous run. Plots must clean up after themselves — add a DELETE step at the end of any plot that creates persistent state.
