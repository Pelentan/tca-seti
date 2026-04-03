# TCA Contract Reference

**Status:** Living Document  
**Last Updated:** 2026-03-04  
**Audience:** AI partners and engineers implementing TCA Jobs  
**Scope:** Everything about contracts — format, rules, the two TCA extension blocks, and why the rules are the rules

---

## The Contract Is the Architecture

In TCA, the contract is not documentation. It is not a README. It is not something you write after the code to describe what you built.

The contract is a machine-readable, verifiable, enforceable definition of exactly what crosses a Job's boundary — in either direction. It is written before implementation begins and it does not change once callers exist. Every other architectural property of TCA — polyglot implementation, three-year lifecycle, independent deployability, zero-trust security posture, observable inter-service communication — depends on the contract holding. If the contract drifts, the architecture drifts. If the contract is written after the code, you have documentation, not a contract.

This document covers the format, the required TCA extension blocks, and the rules that make contracts work as the lynchpin they are meant to be.

---

## Format: OpenAPI 3.1 YAML

All TCA contracts are OpenAPI 3.1 YAML files. Location: `contracts/openapi/{service-name}.yaml` in the project root.

The full anatomy of a TCA contract, in order:

```
openapi: 3.1.0
info:          — title, description, version
x-tca-security    — TCA extension (see Section 4)
x-tca-observability  — TCA extension (see Section 5)
servers:       — service URL by name, not IP
tags:          — optional grouping
paths:         — every endpoint, every response
components:    — schemas, parameters, responses
```

The `x-tca-security` and `x-tca-observability` blocks appear after `info` and before `servers`. They are top-level extension fields. An AI implementing a Job must process both blocks before writing a single line of implementation code — they are requirements, not suggestions.

---

## Section-by-Section Reference

### `info`

```yaml
info:
  title: TCA Blackjack - Hand Evaluator
  description: |
    Pure function service. Cards in → hand value out.
    No state, no side effects, no database.

    Internal service — mTLS only, not externally exposed.
  version: 0.1.0
```

The `description` field is the one place in a contract where prose is appropriate. Use it to explain the Job's primary purpose, any non-obvious implementation rationale (language choice, architectural constraints), and the service's exposure level. Keep it accurate — this description is what an AI partner reads to understand what it is building before touching the schemas.

**Version discipline:** `0.x.0` during initial development. `1.0.0` when the contract is locked and callers exist. Version increments signal a new Job, not a modification.

---

### `servers`

```yaml
servers:
  - url: https://hand-evaluator:3003
    description: Internal Docker network (mTLS enforced)
```

The URL references the **service name**, not an IP address, not `localhost`, not a hardcoded hostname. In Docker Compose and Kubernetes, the service name resolves correctly regardless of which host the container is running on or how many instances exist. Hardcoding an IP address here means the contract breaks every time the container moves. The service name never changes — that is the point.

For internal services, the scheme is `https` (mTLS enforced). For the gateway's external face, `http` on the development port is acceptable during local development only.

---

### `paths`

Every endpoint the Job exposes must be defined. Every response the Job can return — success and failure — must be defined. There are no undocumented endpoints and no undocumented response codes.

```yaml
paths:
  /evaluate:
    post:
      summary: Evaluate a blackjack hand
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/EvaluateRequest'
      responses:
        '200':
          description: Hand evaluated
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/HandResult'
        '400':
          description: Invalid request body
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
```

**The implementation rule:** If the contract doesn't define it, the Job doesn't return it. No undocumented error codes, no undocumented response shapes, no "convenience" endpoints added during development that aren't in the contract.

**Examples are valuable.** OpenAPI 3.1 supports request and response examples inline. Use them. They make the contract self-documenting and give an AI implementing the Job concrete test cases to validate against.

---

### `components`

All schemas, reusable parameters, and reusable response definitions live here. Paths reference components with `$ref`. Duplication in path definitions is a signal that something belongs in components.

**Schema discipline:**
- Required fields are listed explicitly under `required:`
- Types are specific — `string`, `integer`, `boolean`, not `object` with no further definition
- Enums are used wherever the valid values are a closed set
- Financial values are `type: string` with a decimal format description, never `type: number` — floating point arithmetic near money is a contract error

---

## The `x-tca-security` Block

Security posture is defined at the contract layer. This means the AI implementing the Job reads the security requirements from the contract before writing implementation code — not after, not as a separate pass. A Job built without `x-tca-security` in its contract has no defined security posture, which means the AI implementing it will make its own decisions. Those decisions will be inconsistent across Jobs and inconsistent across AI sessions.

### Internal Service Pattern (Standard)

The large majority of TCA Jobs are internal services with no external exposure. Their security posture is mutual TLS on all connections — every service presents a certificate, every service verifies the certificate presented to it.

```yaml
x-tca-security:
  transport: mtls
  tls-min-version: "1.2"
  client-cert-required: true
  reject-without-client-cert: true
  ca-cert: /certs/ca.crt
  service-cert: /certs/{service-name}.crt
  service-key: /certs/{service-name}.key
  implementation:
    server:
      load-ca-cert: true
      load-service-cert: true
      load-service-key: true
      require-and-verify-client-cert: true
    client:
      load-ca-cert: true
      load-service-cert: true
      load-service-key: true
      verify-server-cert: true
  notes: >
    Every inbound connection must present a certificate signed by the TCA CA.
    Connections without a valid client certificate must be rejected — not
    downgraded to unauthenticated. This applies to all endpoints including
    /health. Services that cannot present client certificates (e.g. Docker
    healthcheck probes) should be excluded from healthcheck configuration
    rather than relaxing this requirement.
```

**`tls-min-version`:** `"1.3"` is preferred. `"1.2"` is acceptable where library constraints force it — the constraint must be documented in the `notes` field so it is visible for the next rewrite cycle. `"1.2"` documented is better than `"1.3"` stated but not enforced.

**`reject-without-client-cert`:** This is not a default behavior in most TLS libraries. It must be explicitly configured. The value is always `true` for internal services. A service that accepts unauthenticated connections because it forgot to enforce client certificate verification is not participating in the zero-trust posture — it is creating an unmonitored gap in it.

**The `/health` endpoint is not an exception.** Docker healthcheck probes that cannot present client certificates should be configured out of the mTLS requirement at the infrastructure level, not by relaxing the service's certificate enforcement. Do not create a carve-out in the code.

### Gateway Pattern (Dual Transport)

The gateway is the single service with an external-facing boundary. External clients are browsers and API consumers — they do not present client certificates. The gateway's security posture is therefore different from all other Jobs: standard TLS inbound, mTLS outbound.

```yaml
x-tca-security:
  external-transport: tls
  internal-transport: mtls
  notes: >
    External (browser-facing): standard TLS. Browsers do not present client
    certificates. JWT scope enforcement at the routing layer substitutes for
    mTLS on the external boundary.
    Internal (service-to-service): gateway acts as mTLS client when calling
    all upstream services. Must load its own certificate and key and present
    them on every upstream connection. Must verify upstream service certificates
    against the TCA CA. Upstream services will reject connections without a
    valid client certificate.
  internal-client:
    load-ca-cert: /certs/ca.crt
    load-service-cert: /certs/gateway.crt
    load-service-key: /certs/gateway.key
    verify-server-cert: true
```

The gateway's internal-facing behavior is identical to any other TCA client: load its certificate, present it on every upstream connection, verify what comes back. The external-facing difference is that JWT validation substitutes for client certificate verification at that boundary. This is not a weakening of the posture — it is the correct adaptation for a boundary that terminates user traffic.

---

## The `x-tca-observability` Block

Observability of inter-service communication is a contract obligation, not optional instrumentation. Every Job reports every outbound HTTP call to the Observability Job. The block in the contract defines exactly what that reporting looks like and how it is implemented.

```yaml
x-tca-observability:
  report-outbound-calls: true
  endpoint: https://observability-service:3009/event
  pattern: fire-and-forget
  caller-name: "{service-name}"
  required-fields:
    - caller       # this service's name — must match observability-service allowlist
    - callee       # name of the service being called
    - method       # HTTP method: GET POST PUT DELETE PATCH
    - path         # request path — sanitized by observability-service
    - status_code  # HTTP response status received
    - latency_ms   # round-trip time in milliseconds
    - protocol     # http | https | mtls | sse | websocket
  implementation:
    timing: wrap-every-outbound-http-call
    non-blocking: true
    on-failure: log-and-drop
    do-not-instrument:
      - observability-service
  notes: >
    Every outbound HTTP call this service makes must be reported to
    observability-service. This is a contract obligation, not optional
    instrumentation. The report must be non-blocking — observability is
    never on the critical path. If observability-service is unavailable,
    log the failure locally and continue normally. Do not retry.
    Do not report calls made to observability-service itself.
```

**`pattern: fire-and-forget`** is not a shorthand for "try to report." It means the Job does not await the observability report, does not check its status, does not retry on failure, and does not alter its behavior based on whether the Observability Job is reachable. The game proceeds whether the dashboard is watching or not.

**`do-not-instrument: [observability-service]`** prevents a reporting loop. Every Job that calls observability-service to report a call would generate another call to report, infinitely. The exclusion is explicit in the contract so the AI implementing the Job cannot miss it.

**`caller-name`** must exactly match the service name in the Observability Job's allowlist. Events from unknown callers are dropped silently. Getting the name wrong means the Job's traffic is invisible to the observability layer with no error to diagnose.

**Sensitive data never goes in the report.** The `path` field is sanitized by the Observability Job after receipt — UUIDs are replaced with `[id]`, JWT tokens with `[token]`, IPs with `[ip]`. Individual Jobs do not sanitize before sending. The Observability Job owns all sanitization, once, before anything reaches Redis or the dashboard.

---

## Contract Immutability

Once a contract has callers — once any other Job has been written to call it — the contract is frozen.

This rule is the most important one in TCA and the one under the most pressure during development. When implementation friction surfaces, the first instinct is often to adjust the contract slightly. Resist this. Understand why the rule is not negotiable.

**The practical reason:** Every caller was written against the contract as it existed when the caller was built. A contract change may break callers that are currently working. In a polyglot system with multiple Jobs and multiple AI sessions, tracking which callers depend on which contract version is not feasible. The contract being frozen is what makes caller independence real — it is not a policy preference, it is what allows each Job to be built and maintained without coordination with every other Job.

**The right response to contract friction:** Go back to the Job definition stage. Re-examine what the Job is supposed to do. If the implementation language is fighting the problem domain, consider a language swap — the contract does not change when the implementation language changes. If a new function is genuinely needed, it is a new endpoint on the existing contract (acceptable during initial development window) or a new Job (always acceptable). Modifying an existing endpoint's request or response shape after callers exist is not an option.

**The initial development window:** Contracts may be adjusted during initial development — before any callers are written. This is the one window where iteration is acceptable. The moment the first caller is implemented against a contract, that window closes.

**AI-specific note:** If you encounter implementation friction and find yourself reasoning toward a contract modification, stop. Surface the conflict to the engineer explicitly: "The contract specifies X but the implementation requires Y. The contract cannot change. Here are the options that resolve this without touching the contract." Do not silently adjust the contract to make the implementation easier.

---

## What the Contract Does Not Specify

The contract defines the surface. Everything inside the container is entirely opaque to it.

The contract does not specify:
- Implementation language
- Internal data structures or variable names
- Database technology or schema
- Internal logic or algorithms
- How the Job communicates with its own database
- Third-party libraries used
- Anything else inside the container boundary

This opacity is intentional and structural. It is what makes language swaps possible with zero upstream impact. It is what makes the three-year rewrite cycle feasible. The moment a contract begins leaking internal implementation details, the Job's interior becomes load-bearing for callers, and the ability to replace the implementation independently is compromised.

---

## Stub Contracts

A stub is a fully designed interface with deferred implementation. The contract for a stubbed Job is identical in format and completeness to the contract for a fully implemented Job. There is no "stub contract format."

What distinguishes a stub at the implementation level:
- A single, clearly marked swap point where real behavior replaces stub behavior
- Visible logging that makes stubbing apparent during development: `"STUB: auth check passed for {service} — real policy not enforced"`
- Correct response types and shapes — the stub returns the right structure, just not with real data

What a stub does not do:
- Return incorrect types to avoid implementation complexity
- Omit response fields that the real implementation will include
- Accept malformed requests that the real implementation would reject

The test of a correctly designed stub: when the real implementation replaces it, no caller changes. Not one line in any other Job. If callers need to change when a stub is replaced, the stub's contract was wrong.

---

## Quick Reference: Contract Checklist

Before marking a contract complete:

- [ ] `openapi: 3.1.0` at the top
- [ ] `info` block with accurate title, description (exposure level stated), and version
- [ ] `x-tca-security` block present — correct pattern for this service's exposure level
- [ ] `x-tca-observability` block present — `caller-name` matches observability allowlist exactly
- [ ] `servers` block uses service name, not IP or localhost
- [ ] Every endpoint defined with full path
- [ ] Every request body schema defined with required fields
- [ ] Every response schema defined for every status code including errors
- [ ] Financial values use `type: string` decimal format, not `type: number`
- [ ] Components section has all schemas referenced by paths
- [ ] No undocumented endpoints
- [ ] No undocumented response codes

---

---

## Health Probes — Watchdog-Mediated, No Open Ports

TCA Jobs do not expose plain HTTP health ports. The zero-trust posture means every network surface is authenticated. A plain HTTP health port is an unauthenticated, unmonitored surface that represents exactly the kind of gap Augur Canis is designed to detect.

Health verification is entirely handled by the Augur Canis (AC) via Redis pub/sub coordination. See tca-guidelines Section 20 for the full mechanism.

**What this means for contract authors:**

No contract defines a health port or a plain HTTP health endpoint intended for Docker or K8s probes. The `/health` endpoint that appears in some contracts is an mTLS-authenticated admin diagnostic endpoint — not the Docker/K8s health probe mechanism.

**The healthcheck binary:**

Every Job's Docker image includes a static `/healthcheck` binary. It publishes a check request to Redis `tca:check-requests`, waits for a result on `tca:check-results:{request_id}` with a configured TTL, and exits 0 or 1. This is what Docker and K8s execute for health probing — no network call to the Job, no unauthenticated port.

**Docker Compose healthcheck pattern (standard for all Jobs):**

```yaml
healthcheck:
  test: ["/healthcheck"]
  interval: 30s
  timeout: 10s
  retries: 3
  start_period: 30s
environment:
  SERVICE_NAME: "job-name"
  REDIS_URL: "redis:6379"
```

**The `/health` endpoint (where it exists):**

Some Jobs expose a `/health` endpoint on their mTLS port for admin diagnostic purposes — confirming service state, dependency connectivity, and runtime statistics. This endpoint requires a valid client certificate. It is not what Docker or K8s use for health probing. It is for Wr4ngler and operator visibility only.

## A Note on OpenAPI Extensions

The `x-tca-security` and `x-tca-observability` blocks are OpenAPI extension fields. The `x-` prefix is the OpenAPI 3.1 convention for vendor or application-specific extensions. Standard OpenAPI validators will not reject these fields, but they will not validate their contents either — validation of TCA extension blocks is the responsibility of the engineer reviewing the contract, not the validator.

Future tooling may formalize validation of these blocks. For now, the blocks are contracts between the engineer and the AI implementing the Job. An AI encountering these blocks should treat their contents as implementation requirements with the same weight as any other part of the contract.
