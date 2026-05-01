# SESSION PRIMER — 2026-04-28

## Current State

SETI and Vox federation are working.  Both tabs show correctly on the dashboard.
Health checks, inter-service traffic, and contract test events all flow correctly
to the TCA Vox tab.

## What's Working

- SETI tab: health checks every 30 seconds, clean feed, no repeating
- TCA Vox tab: health checks, inter-service traffic, contract test events
- Federation handshake: 3-message protocol with signature verification
- Per-constellation Redis channels: `federation:tca-vox:events`
- Gateway dynamic subscription via `seti:federation:announce`
- IngressRouteTCP in Helm chart (no more manual apply)
- Readiness probes removed from both SETI and Vox
- Vox liveness probe corrected to 30 seconds (was 15)
- `augur-canis` added to Vox observability known callers
- Vox observability callee validation removed
- Policy dynamic registration from connie-agent body

## What's Broken / In Progress

### Constellation Proxy (Run Contract Tests / Ring Reports)

The flow is:
  UI → gateway → connie-agent → signal-aggregator → Vox AC (mTLS monitor cert)

signal-aggregator has `handleFederationProxy` at `/federation/proxy/{tag}/...`
connie-agent has `handleConstellationProxy` which forwards to SA.
Gateway has `/constellations/` route that proxies to connie-agent.
UI uses `active.isSelf` to pick SETI vs constellation endpoint.

**The Problem:** When SA restarts, `fedApps` is empty.  Connie-agent's
`isFederationActive` check hits the OLD SA pod (still alive during rolling update)
and returns true — skipping the handshake.  New SA has no state.
Result: `handleFederationProxy` returns "constellation not active" (503).

**Proposed Fix Being Considered:**
Remove `isFederationActive` check entirely.  Always run `initiateFederation`
on every sync cycle.  Signal-aggregator's `connectFederatedApp` already handles
idempotency — cancels old goroutines via `done` channel before starting new ones.
The cost is a re-handshake every 10 minutes, but the SSE stream stays live
during the old goroutine's teardown.

Michael wants to sleep on this — may have a cleaner approach.

## Key Files Changed This Session

### SETI repo (`~/OneDrive/Coding/AI-Projects/tca-seti`)
- `connie-agent/main.go` — constellation proxy, activateInPolicy with body, cleanupStaleFederations
- `signal-aggregator/main.go` — `/federation/proxy/` route
- `signal-aggregator/federation.go` — handleFederationProxy, ca_cert in GET response, verifyFeedEvent fix
- `gateway/main.go` — `/constellations/` route, connieAgentURL, upstream error logging
- `ui/src/pages/Dashboard.tsx` — Run Contract Tests button uses active constellation
- `ui/src/pages/Ring.tsx` — Reports tab fetches from active constellation, isSelf type fix
- `charts/seti/templates/gateway/ingress.yaml` — NEW: IngressRouteTCP in Helm chart
- `charts/seti/templates/network-policies/policies.yaml` — connie-agent ingress from gateway
- `charts/seti/templates/connie-agent/deployment.yaml` — waitForSignalAggregator init container (may revert)
- `charts/seti/templates/configmaps/remote-apps-configmap.yaml` — hardcoded JSON (not from values)
- All deployment templates — readiness probes removed
- `contracts/openapi/connie-agent.yaml` — constellation proxy endpoints
- `contracts/openapi/gateway.yaml` — constellation proxy routes
- `contracts/openapi/signal-aggregator.yaml` — federation proxy endpoints

### Vox repo (`~/OneDrive/Coding/AI-Projects/tca-vox`)
- `observability-service/handler.go` — augur-canis added to callers, callee validation removed
- All deployment templates — readiness probes removed, liveness probe 15→30s

## Next Steps

1. Resolve the `isFederationActive` / SA restart race condition
2. Test Run Contract Tests button on TCA Vox tab
3. Test Ring → Reports tab for TCA Vox
4. Plot tests for remote constellations

## Build Commands

```bash
# SETI
docker build --no-cache -t localhost:5000/seti-{service}:dev -f {service}/Dockerfile .
docker push localhost:5000/seti-{service}:dev
helm upgrade seti charts/seti -f charts/seti/values/dev.yaml -n seti

# Vox
docker build --no-cache -t localhost:5000/vox-{service}:dev -f {service}/Dockerfile .
docker push localhost:5000/vox-{service}:dev
helm upgrade vox charts/vox -f charts/vox/values/dev.yaml -n tca-vox
```
