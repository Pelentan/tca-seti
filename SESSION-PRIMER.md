# SETI Session Primer
*Last updated: 2026-04-22 — Phase S4 complete, ready for feed-wrangler redix replacement*

## Who We Are

**Michael** — Connie Wr4ngler, architect, all decisions.
**Claude** — source of truth for all code, implements, tracks state, flags issues once.

## Project

**SETI** (Search for Erroneous Tessellated Interactions) — TCA constellation-level monitoring application. K8s deployment on k3d cluster (`tca`), namespace `seti`.

## Current State

**All phases S1–S4 complete. Constellation is healthy — 53/53 contract tests, 11/11 plots passing.**

### What Was Completed This Session

**Phase S1** — go-redis eliminated across all Go Jobs. TCA stdlib Redis client (`redis.go`) deployed to 8 Jobs. healthcheck embedded build stage cleaned across 14 Dockerfiles.

**Phase S2** — golang-jwt eliminated. gateway and policy use stdlib HS256 JWT (`jwt.go`).

**Phase S3** — Secrets, PostgreSQL X.509, Graceful Shutdown:
- helm install/upgrade requires zero `--set` flags
- cert-forge generates `postgres-password` and `jwt-secret` on first startup, persists in `seti-certs` Secret
- PostgreSQL authenticates lore via X.509 client certificate — no application password anywhere
- All 20 Jobs handle SIGTERM gracefully via `shutdown.go`/`shutdown.py`/`shutdown.ts` (separate file, infrastructure not domain)
- `terminationGracePeriodSeconds: 20` on all Deployments, 30 on postgres StatefulSet
- CA rotation fix — forge.json lists all 18 deployments, fresh CA triggers immediate roll
- `results` dead redis dependency removed, Dockerfile simplified
- `ai-lien` requests replaced with stdlib urllib

**Phase S4** — signal-clearance zero runtime npm dependencies:
- `jose` → `jwks.ts` (TCA JWKS client, crypto.subtle + https)
- `uuid` → `crypto.randomUUID()` (Node 20 builtin)
- `redis` npm → `redis.ts` (TCA RESP2 client, net.Socket)
- `express` + `cookie-parser` → `router.ts` (TCA HTTP router)
- Three new TCA lib contracts: `jwks-client.yaml`, `redis.yaml` (TS), `router.yaml`

## Next Session Goal

**Eliminate `redix` in feed-wrangler** — same RESP2 pattern, Elixir implementation. Then assess `jason` (needs OTP 27+) and `plug_cowboy` (no stdlib alternative).

## Remaining Third-Party Dependencies

| Job | Dependency | Action |
|-----|-----------|--------|
| lore | lib/pq | Stays — no Go stdlib PostgreSQL driver |
| feed-wrangler | redix | Replace with stdlib RESP2 client in Elixir |
| feed-wrangler | jason | Stays unless OTP 27+ is available |
| feed-wrangler | plug_cowboy | Stays — no Elixir stdlib HTTP server |
| ui | react, vite, etc. | Build-time only, never runs in production |

## Architecture Quick Reference

**Stack:** Go, TypeScript/Node.js, React 19/Vite, Python, Elixir, GnuCOBOL, Haskell
**Infrastructure:** k3d (`tca` cluster), namespace `seti`, Helm chart at `charts/seti/`
**Registry:** `localhost:5000` (push) / `tca-registry:5000` (pull)
**Dev values:** `charts/seti/values/dev.yaml`
**Ingress:** `seti.tca.local` via Traefik

**Key ports:**
- gateway: 4000 (external TLS), proxies to all Jobs over mTLS
- cert-forge: 4016 (public /ca), 4015 (enrollment mTLS), 4014 (sign mTLS)
- augur-canis: 4010 (mTLS admin)
- signal-clearance: 4001
- lore: 4110

## TCA Lib Index (current)

All in `contracts/lib/`:
- `redis-client.yaml` → `redis.go` — Go Redis RESP2 client
- `jwks-client.yaml` → `jwks.ts` — TypeScript JWKS/RS256 verification
- `redis.yaml` (TS) → `redis.ts` — TypeScript Redis RESP2 client
- `router.yaml` → `router.ts` — TypeScript HTTP router
- `jwt.go` / `jwt.ts` — HS256 JWT sign/verify (Go and TypeScript)

**Rule:** Check the lib index before any import. If no lib covers it, surface the gap — do not pull a package.

## Critical Operational Notes

- **Tarball delivery:** All changes delivered as `.tar.gz`. One file via `present_files`. Always includes `CHANGED_FILES.md`. Extract at project root: `tar -xzf <file>`
- **Build command:** `./scripts/build-push.sh` then `kubectl delete pods --all -n seti` for full roll
- **Selective rebuild:** `docker build -f <job>/Dockerfile -t localhost:5000/seti-<job>:dev . && docker push localhost:5000/seti-<job>:dev && kubectl rollout restart deployment/<job> -n seti`
- **Test:** Run contract tests and plot tests from SETI Ring → Reports tab
- **PVC deletion sequence:** `kubectl scale statefulset postgres -n seti --replicas=0` → `kubectl delete pvc postgres-data-postgres-0 -n seti` → helm upgrade
- **node_modules on Windows/OneDrive:** `rmdir /s /q node_modules` then `npm install` (not PowerShell)

## Key Bugs Fixed This Session (for continuity)

- `selfRegisterWithAC` 10-attempt cap removed across all 14 certforge implementations — was masking startup race condition
- Subscribe context race in healthcheck — independent `context.WithCancel` prevents premature channel close
- `net.Error` timeout detection — `isTimeoutError()` helper with string fallback for Go 1.22 Linux wrapping
- `listen_addresses = '*'` required in custom postgresql.conf — postgres defaults to localhost-only otherwise
- `pg_ident.conf` maps CN=lore-db to lore pg role — clientcert verify-full uses CN as username
- `defaultMode: 0640` on postgres Secret volume (fsGroup: 70) + `defaultMode: 0600` on lore Secret volume — postgres rejects world-readable key files
- `waitForPostgresPassword` init container polls the actual file, not the HTTP endpoint — kubelet sync latency
- router.ts `use()` empty-prefix mount — copy routes directly, don't combine empty pattern with route pattern
- CA rotation: forge.json must list all deployments; fresh CA triggers immediate roll after Secret write

## Reference Files in Project

- `tca-guidelines.md` — living operational reference (updated this session: section 8 graceful shutdown, section 11 lib index discipline)
- `contracts/lib/tca-lib-index.yaml` — supply chain gate
- `PHASE-PLAN.md` — full phase history with lessons learned
- `SETI-GUIDELINES.md`, `AC-GUIDELINES.md`, `K8S-MIGRATION-PRIMER.md` — always apply
