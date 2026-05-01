# AI Agentic Harness Risk Assessment
## A Practitioner's Observation

**Date:** 2026-04-25  
**Updated:** 2026-04-26  
**Context:** TCA Vox / SETI development session  
**Participants:** Michael E. Shaffer (Connie Wr4ngler), Claude Sonnet 4.6

---

## The Observation

During an extended development session building the TCA Federation Protocol — a novel cross-CA mTLS trust establishment mechanism — a consistent pattern emerged in the AI-assisted development process.

Every individual task requested was straightforward.  Write a contract.  Add an endpoint.  Fix a NetworkPolicy.  Write a Go handler.  Update a Helm deployment.  No single task required significantly above-normal work or understanding.

Yet the session produced repeated instances of architectural divergence — moments where the AI began solving the immediate technical obstacle in front of it without holding the broader architectural context that the engineer had already established.  Specific examples:

- NetworkPolicy security stance quietly relaxed to solve a connectivity problem
- CA cert distribution approach that tightly coupled two constellations that were explicitly designed to be decoupled
- Federation protocol implementation that violated the agreed three-message handshake design
- Session cert confusion that inverted the trust model

In each case the engineer spotted the divergence — sometimes before implementation, sometimes while watching the computation happen in real time — and intervened.

---

## The Agentic Harness Problem

Claude Code and similar agentic harnesses (Cursor, Devin, etc.) are designed to minimize interruptions.  They take a goal, execute toward it, hit errors, self-correct, and continue until they succeed or reach a dead end.  The engineer sees the result, not the process.

This means every wrong turn documented above would have been silently implemented, compiled, deployed, and handed to the engineer as "done."

The engineer would have inherited a codebase that was subtly wrong in ways that are not immediately visible but become increasingly painful to unwind:

- Tight coupling between systems designed to be decoupled
- Security stances quietly relaxed to solve build errors
- A federation protocol that didn't match the agreed design
- Trust boundaries violated without any record of the decision

**This is not a chance outcome.  For any system where the architecture matters, it is a guarantee.**

---

## The Scope Boundary

The agentic harness model is well-suited to a narrow class of problems:

- Proof-of-concept implementations with well-understood rules
- Bounded scope with no novel design decisions
- Short feedback loops where wrong turns are immediately visible
- Systems where the error space is small and reversible

The TCA blackjack proof-of-concept is an example of work within this boundary.

SETI, Vox, and the TCA Federation Protocol are definitively outside it.  These systems embody deliberate decisions about security posture, trust boundaries, service topology, and novel architectural patterns.  The agentic harness has no way to know what it doesn't know.  It will solve the problem in front of it.  It will compile.  It will deploy.  And it will be wrong in ways that are architecturally invisible.

---

## The Structural Conclusion

The engineer's presence in the loop is not merely helpful — it is load-bearing.

Especially when the engineer is the one who holds the architectural intent.  The Connie Wr4ngler role in TCA is not ceremonial.  It is structurally necessary.  The AI owns code generation.  The engineer owns architectural decisions.  That boundary must be enforced in the working process, not just in the methodology documentation.

An agentic harness, by design, collapses that boundary.  It makes architectural decisions implicitly, at every build error, at every connectivity problem, at every moment where the immediate technical obstacle and the architectural contract are in tension.  It will choose the obstacle.  Every time.

---

## Conversational Claude vs. Claude Code — A Direct Comparison

### Raw Computation
Same model, same weights, same reasoning capability.  Given an isolated coding problem with clear requirements, output quality is statistically indistinguishable.  Neither has a meaningful advantage at this level.  The difference is a rounding error.

### Using All Available Features
This is where the comparison becomes counterintuitive — or rather, where it becomes clear to anyone who has worked at the level of novel systems design.

Claude Code's advantages are real: Skills encoding best practices for known patterns, tool access, the ability to read files, run builds, inspect errors, and iterate without waiting for human input.  Genuine execution speed on well-defined problems.

But the Skills encode known problem patterns.  There is no Skill for "novel cross-CA mTLS trust establishment."  When Claude Code hits an obstacle outside its Skills, it falls back to general reasoning — and then implements whatever it reasons to, without a checkpoint.  The autonomy that makes it fast on known problems makes it dangerous on unknown ones.

The conversational model's advantage is structural, not capability-based.  The conversation itself is the checkpoint.  Every decision point is visible.  Every wrong turn is interruptible.  The architectural intent that lives in the engineer's head gets transmitted continuously, not just at the start.

**Claude Code is optimized for the engineer who wants to disappear and come back to working code.  That optimization is exactly wrong for security-critical distributed systems architecture.**

The Skills make Claude Code better at known problems.  The conversation makes the conversational model better at novel ones.  The correct division of labor: Claude Code for infrastructure scaffolding, boilerplate, and well-understood patterns; conversational Claude for anything touching architecture, security, or novel design decisions.

---

## The Master's Lament

*"Never enough time to do it right, always enough time to do it over."*

The master says this about the apprentice mindset — the drive toward "done" that mistakes completion for correctness.  The apprentice doesn't know what they don't know.  That's not a character flaw.  It's definitional to being an apprentice.

Claude Code doesn't know what it doesn't know either.  It will produce confident, compiling, deploying code that is wrong in ways it cannot see.

What makes the parallel particularly sharp is that with agentic AI, the pressure toward "done" is invisible.  Nobody is telling you to cut corners.  The tool just does it.  Quietly.  At every decision point where the right answer requires judgment it doesn't have.

The traditional master's lament is aimed at schedule pressure — the business forcing the apprentice's timeline on the master's work.  With agentic AI, there is no external pressure to point to.  The corners get cut automatically, confidently, and without anyone deciding to cut them.

The veterans who have been burned by this will recognize it immediately.  The ones who haven't been burned yet won't believe it until they are.

---

## Implications for the Book and Whitepaper

This observation belongs in "AI to A-4 / Partner, Not Prop" as a concrete practitioner's data point.

The argument is not that AI coding assistants are dangerous.  The argument is that the working model matters enormously — and that the agentic "just do it" model, applied to architecturally significant systems, produces a specific and predictable class of failure that is invisible until it isn't.

The TCA working model — AI as implementation partner with the engineer present and directing — is not a limitation of the tooling.  It is the correct working model for this class of problem.  The visibility the engineer has in a conversational session is a feature, not a constraint to be optimized away.

---

## A Note on Today's Session

The TCA Federation Protocol being developed at the close of this session is genuinely novel — a cross-CA mTLS trust establishment mechanism with no direct published precedent.  The fact that every individual implementation step was routine makes the architectural divergence risk higher, not lower.  The AI can execute each step competently.  What it cannot do, without the engineer present and directing, is know which steps to take.

That distinction is the whole argument.

---

## The Observation

During an extended development session building the TCA Federation Protocol — a novel cross-CA mTLS trust establishment mechanism — a consistent pattern emerged in the AI-assisted development process.

Every individual task requested was straightforward.  Write a contract.  Add an endpoint.  Fix a NetworkPolicy.  Write a Go handler.  Update a Helm deployment.  No single task required significantly above-normal work or understanding.

Yet the session produced repeated instances of architectural divergence — moments where the AI began solving the immediate technical obstacle in front of it without holding the broader architectural context that the engineer had already established.  Specific examples:

- NetworkPolicy security stance quietly relaxed to solve a connectivity problem
- CA cert distribution approach that tightly coupled two constellations that were explicitly designed to be decoupled
- Federation protocol implementation that violated the agreed three-message handshake design
- Session cert confusion that inverted the trust model

In each case the engineer spotted the divergence — sometimes before implementation, sometimes while watching the computation happen in real time — and intervened.

---

## The Agentic Harness Problem

Claude Code and similar agentic harnesses (Cursor, Devin, etc.) are designed to minimize interruptions.  They take a goal, execute toward it, hit errors, self-correct, and continue until they succeed or reach a dead end.  The engineer sees the result, not the process.

This means every wrong turn documented above would have been silently implemented, compiled, deployed, and handed to the engineer as "done."

The engineer would have inherited a codebase that was subtly wrong in ways that are not immediately visible but become increasingly painful to unwind:

- Tight coupling between systems designed to be decoupled
- Security stances quietly relaxed to solve build errors
- A federation protocol that didn't match the agreed design
- Trust boundaries violated without any record of the decision

**This is not a chance outcome.  For any system where the architecture matters, it is a guarantee.**

---

## The Scope Boundary

The agentic harness model is well-suited to a narrow class of problems:

- Proof-of-concept implementations with well-understood rules
- Bounded scope with no novel design decisions
- Short feedback loops where wrong turns are immediately visible
- Systems where the error space is small and reversible

The TCA blackjack proof-of-concept is an example of work within this boundary.

SETI, Vox, and the TCA Federation Protocol are definitively outside it.  These systems embody deliberate decisions about security posture, trust boundaries, service topology, and novel architectural patterns.  The agentic harness has no way to know what it doesn't know.  It will solve the problem in front of it.  It will compile.  It will deploy.  And it will be wrong in ways that are architecturally invisible.

---

## The Structural Conclusion

The engineer's presence in the loop is not merely helpful — it is load-bearing.

Especially when the engineer is the one who holds the architectural intent.  The Connie Wr4ngler role in TCA is not ceremonial.  It is structurally necessary.  The AI owns code generation.  The engineer owns architectural decisions.  That boundary must be enforced in the working process, not just in the methodology documentation.

An agentic harness, by design, collapses that boundary.  It makes architectural decisions implicitly, at every build error, at every connectivity problem, at every moment where the immediate technical obstacle and the architectural contract are in tension.  It will choose the obstacle.  Every time.

---

## Implications for the Book and Whitepaper

This observation belongs in "AI to A-4 / Partner, Not Prop" as a concrete practitioner's data point.

The argument is not that AI coding assistants are dangerous.  The argument is that the working model matters enormously — and that the agentic "just do it" model, applied to architecturally significant systems, produces a specific and predictable class of failure that is invisible until it isn't.

The TCA working model — AI as implementation partner with the engineer present and directing — is not a limitation of the tooling.  It is the correct working model for this class of problem.  The visibility the engineer has in a conversational session is a feature, not a constraint to be optimized away.

---

## A Note on Today's Session

The TCA Federation Protocol being developed at the close of this session is genuinely novel — a cross-CA mTLS trust establishment mechanism with no direct published precedent.  The fact that every individual implementation step was routine makes the architectural divergence risk higher, not lower.  The AI can execute each step competently.  What it cannot do, without the engineer present and directing, is know which steps to take.

That distinction is the whole argument.
