# Third-Party Libraries on Notice

*Michael Hay — April 2026*

---

## The Standard Model

Most software teams operate on an implicit assumption:  third-party libraries are
free.  You add a dependency, you get capability, and the cost is someone else's
problem.  The package maintainer handles CVEs.  The open-source community handles
edge cases.  Your team focuses on the domain logic that actually differentiates
your product.

This model is so deeply embedded in modern software practice that questioning it
feels almost academic.  And yet it produces systems that are difficult to secure,
expensive to maintain, and structurally fragile in ways that teams have simply
accepted as normal.

This document is about a different model — and a concrete demonstration that it
works.

---

## What We Actually Mean by "Free"

When a team pulls a third-party library, the real costs are deferred, not
eliminated:

**Supply chain exposure.**  A runtime dependency is executable code you did not
write running in your production environment.  A compromised package — whether
through a malicious maintainer, a typosquatting attack, or a dependency
confusion exploit — has the same access to your secrets, your network, and your
data as your own code.  You have accepted that risk for the price of not writing
the library yourself.

**Maintenance dependency.**  When a CVE is published against a package you depend
on, you are on the maintainer's timeline.  You track their issue queue.  You wait
for a patch.  You test their fix against your system.  You have outsourced not
just the original implementation but all future security work — to a party whose
priorities are not yours.

**Version drift and compatibility rot.**  Dependencies have their own dependency
trees.  Major version upgrades break APIs.  Transitive dependencies conflict.
The `node_modules` directory that started at 40MB is 400MB two years later and
nobody knows why.  The Go module graph that compiled cleanly in 2022 requires
three replace directives by 2024.  This is normal, accepted, and entirely
avoidable.

**Cognitive surface area.**  Every dependency is code your team is responsible
for understanding when something goes wrong at 2am.  The stack trace that
disappears into a third-party library is time spent reading code you did not
write, under pressure you did not plan for.

None of these costs appear on the sprint board when someone adds a `require`
statement.  They appear later, compounded.

---

## The SETI Demonstration

SETI (Search for Erroneous Tessellated Interactions) is a constellation-level
monitoring application deployed on Kubernetes.  Its stack spans Go, TypeScript,
Elixir, Python, GnuCOBOL, and Haskell across 20 production jobs.  It handles
mTLS across all services, X.509 client certificate authentication for PostgreSQL,
a custom certificate authority with automated rotation, and a GitOps deployment
pipeline with manual security approval gates.

It is not a simple system.

When the dependency elimination campaign began, the runtime dependency list
included:

- `go-redis` across 8 Go jobs
- `golang-jwt` in gateway and policy
- `redix` in feed-wrangler (Elixir)
- `jason` in feed-wrangler (Elixir)
- `jose`, `uuid`, `redis` (npm), and `express` in signal-clearance (TypeScript)
- `requests` in ai-lien (Python)

When it ended, the runtime dependency list was:

- `lib/pq` in lore — the PostgreSQL wire protocol, no stdlib alternative exists
- `plug_cowboy` in feed-wrangler — maintained by the Elixir and OTP core teams,
  no stdlib HTTP server exists in OTP

Two dependencies.  Both with unambiguous justification.  Both maintained by
language core teams.  Neither replaceable without taking on a multi-month
engineering project with meaningful CVE risk of its own.

Everything else was replaced with stdlib implementations.

---

## What Replaced Them

The replacements were not stubs or shortcuts.  They are correct, production-grade
implementations:

**RESP2 Redis client — three languages.**  The Redis Serialization Protocol is a
real protocol with type prefixes, bulk string framing, array nesting, and
pub/sub push semantics that operate differently from request/response commands.
We implemented it correctly in Go (`redis.go`), TypeScript (`redis.ts`), and
Elixir (`redis.ex`) — each idiomatic to its language and concurrency model.
The Elixir implementation required particular care:  the subscriber connection
runs in a spawned process that takes socket ownership via `:gen_tcp.controlling_process`,
enabling active mode only after the handoff, so no `receive` block ever executes
inside a GenServer handler.

**HS256 JWT — Go and TypeScript.**  Standard HMAC-SHA256 signature generation
and verification using each language's native crypto primitives.  No behavioral
difference from the packages they replaced.

**JWKS client — TypeScript.**  RS256 public key verification using `crypto.subtle`
and `https` — the Web Crypto API built into Node 20.  Fetches the JWKS endpoint,
caches keys, verifies signatures.  Zero npm dependencies.

**HTTP router — TypeScript.**  Request parsing, route matching, middleware chaining,
and JSON response helpers using Node's native `http` module.  Replaced express
and cookie-parser entirely.

Each implementation is governed by a TCA lib contract — a YAML specification that
documents function signatures, error behaviors, constraints, and the prohibition
on external dependencies.  The contract is the source of truth.  The
implementation is bound to it.

---

## The Contracts Are the Key Insight

Removing dependencies creates an obvious objection:  you now own the
maintenance burden.  Every CVE that would have been the package maintainer's
problem is now yours.

This objection is correct but incomplete.  It assumes that owning the
maintenance burden is expensive.  With contracts, it is not.

A CVE against a third-party package requires:  reading the advisory, understanding
the affected code, reverse-engineering what the implementation was supposed to do,
rewriting it, and validating that the behavior is correct.  The hard part is
reconstructing intent.

A CVE against a TCA stdlib implementation requires:  reading the advisory, reading
the contract, implementing the fix to spec.  Intent is already documented.
Function signatures are frozen.  Error behaviors are specified.  The contract test
suite defines done.

The discovery phase — which is where most remediation time is spent — does not
exist.

This extends to the full application.  SETI was built by an engineer-AI team in
under four weeks, including time lost to distractions and parallel projects.  A
traditional team of four to six engineers would realistically need 18 to 24 months
to reach a comparable level of completeness, security rigor, and operational
discipline — and would likely never reach the dependency elimination phase at all,
because they would still be managing the consequences of early architectural
decisions.

Given the contracts, working implementations, and established patterns, the same
engineer-AI team could rebuild the entire application from scratch in a single
working day.  Not because the AI types quickly — but because the hard work is
already done.  Architectural decisions were made once.  The contracts define every
interface.  The existing jobs are starting points, not blank pages.  There is no
design phase, no interface negotiation, no "what was this supposed to do"
archaeology.

A CVE that affects every job in the constellation is, under this model, a morning's
work.

---

## The Isolation Argument

Traditional monolithic codebases fail in two distinct ways.

The first is intentional change with unintended consequences — a refactor that
was scoped to one module bleeds into another because the abstractions were not
clean.  This is widely recognized as a problem and the subject of endless
architectural discussion.

The second is accidental corruption — a stray keystroke in an unrelated file that
the compiler catches at the worst possible moment.  One thing changed, something
completely different fails to compile.  An hour is spent determining that the
problem has nothing to do with the actual work.  This is so common that engineers
have stopped recognizing it as a problem.  It is treated as the cost of working
in a large codebase.

TCA's constellation architecture eliminates both.

Each job is an isolated compilation unit.  A stray character in feed-wrangler does
not fail a lore build.  An accidental edit in signal-clearance does not touch
gateway.  The blast radius of human error — including the mundane, embarrassing
kind — is structurally contained by the architecture itself.

The contract boundary enforces the same isolation intentionally.  A feature change
that should only affect one job can only affect one job.  If it bleeds across job
boundaries, that is a contract violation, not a refactor.  The architecture makes
the right thing the easy thing.  The wrong thing is not just discouraged — it is
structurally difficult.

Most teams do not recognize accidental corruption as a solvable problem.  They
call it technical debt, accept it as normal, and schedule a refactor that never
happens.  The constellation model makes it architecturally impossible instead.

---

## The Defensible Exceptions

Two runtime dependencies remain.  Both are worth defending explicitly.

**`lib/pq` (Go / lore).**  The PostgreSQL wire protocol is a complex, stateful,
binary protocol with authentication handshakes, SSL negotiation, prepared statement
lifecycles, and extended query modes.  Implementing it correctly is months of
engineering work with meaningful CVE risk in the result.  `lib/pq` is the
reference Go implementation, widely deployed, actively maintained, and
well-audited.  lore uses X.509 client certificate authentication — the protocol
complexity is already fully exercised.  A hand-rolled implementation would be a
vulnerability in the making.

**`plug_cowboy` (Elixir / feed-wrangler).**  Cowboy is maintained by the OTP
ecosystem team.  Plug is maintained by the Elixir core team.  These are not
loosely-governed hex packages — they are as close to stdlib as a library can be
without being shipped with the runtime.  HTTP/1.1 done correctly requires chunked
transfer encoding, keepalive, pipelining, and TLS — none of which OTP's `:inets`
covers in a programmable way.  The governance here is appropriate.  The
alternative does not exist.

The framing that applies to both:  we eliminated every dependency that had a stdlib
replacement or represented unnecessary supply chain exposure.  These two have
neither condition.  They are load-bearing infrastructure that exists because
writing them correctly is genuinely hard.  That is the right reason to take a
dependency.

---

## What This Means in Practice

For a team operating under this model:

**Security posture is structural, not procedural.**  You are not depending on
Dependabot alerts, maintainer response times, or patch testing cycles.  Your
runtime supply chain is two dependencies with clear governance.  The attack
surface that does not exist cannot be exploited.

**Maintenance cost is nearly flat.**  Traditional systems see maintenance costs
grow as the codebase ages — more dependencies, more drift, more compatibility
debt.  Under this model the knowledge is in the contracts, not the code.  The
code is almost disposable.  Rebuild cost does not compound over time.

**Engineer-AI collaboration is a force multiplier, not a shortcut.**  The
compression from 24 months to 4 weeks did not come from the AI generating code
quickly.  It came from making every architectural decision correctly the first
time, documenting intent in contracts, and eliminating the rework cycles that
consume traditional projects.  The AI executes against specifications.  The
engineer makes judgment calls and validates output.  Neither works without the
other.

**The right question is not "what libraries should we use."**  It is "what is
the actual cost of this dependency over the life of the system, and is there a
stdlib implementation that eliminates that cost."  For most of what teams reach
for packages to do, the honest answer is yes.

---

*Third-party libraries are not free.  Some are worth the cost.  Most are not.*
*The ones that remain in SETI are the ones that are.*
