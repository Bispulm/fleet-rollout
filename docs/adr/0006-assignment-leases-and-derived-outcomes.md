# ADR 0006: Assignment leases, and outcomes derived from assignments

Status: Accepted. The validity lease is superseded by [ADR 0009](0009-installs-need-the-servers-agreement.md).

## Context

A halt is only as good as its effect on devices, and a pull-based device hears about it only when it next
polls. Meanwhile, reports get retried and delayed, so anything that counts them has to tolerate
duplicates. Stage decisions also have to survive a restart of the process making them.

## Decision

**Assignments carry a validity lease.** Each poll renews `expires_at` while a device may proceed. An agent
must not begin installing after the lease expires. This turns "a device that lost contact might install
at any time" into a bound the operator can reason about, without the server being able to reach the device.

**Halting supersedes work that hasn't reached installation,** in the transaction that halts. Work that has
reached installation is left to finish and report. The point of no return is the start of installation,
because before that the device has changed nothing.

**Outcomes are derived, not counted.** A stage's numbers come from a query over assignments at evaluation
time, with at most one forward assignment per device per rollout. A duplicate report can't double count,
because there's no counter to increment, and a restart loses nothing, because there's no memory to lose.

**Unknown is derived too,** from `last_seen_at` and the stage's start. Polling counts as being heard from, so
a device holding during a long pause doesn't turn unknown.

**Decisions are a pure function.** `Policy.Evaluate` takes outcomes, timers, and the time, and returns a
verdict. The engine calls it under the row lock and the lease, and status calls it to explain what a rollout
is waiting for, so the explanation can't disagree with the decision.

Alternatives considered:

- **Incrementing counters on each report.** Cheaper to read, but idempotency then depends on never missing a
  case, and a bug silently corrupts safety decisions. If evaluation turns out to be too expensive at scale, a
  cache can be added and checked against this query.
- **Marking devices stale with a background job.** It adds writes and a value that can lag behind reality,
  for something a query can answer exactly.
- **Recalling devices by a push channel.** Devices are often unreachable, which is why they poll (ADR 0002).

## Consequences

- Each evaluation runs one aggregate query per active rollout. Its cost grows with fleet size, and it needs
  measuring at 10,000 devices.
- A poll writes `last_seen_at`. That's a write per poll, and it's the first thing to batch if polling load
  turns out to be a problem.
- The lease only bounds exposure if agents honor it, which arrives with the agent.
- Members offline at completion need a follow-up rollout. The existing eligibility rules already handle this,
  since members on the release are excluded from it, and the rest aren't.
