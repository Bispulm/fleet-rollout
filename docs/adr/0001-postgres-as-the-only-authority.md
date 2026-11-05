# ADR 0001: Postgres is the only durable authority

Status: Accepted

## Context

A rollout's safety depends on knowing, after any crash, exactly which devices were exposed
and what they reported. The control plane has several writers: API replicas handling
operator commands and device reports, and a rollout engine advancing stages. They must agree
on state without losing or double-counting anything.

Adding a message broker or a cache would create a second place where this state lives, with
its own failure modes and its own ordering rules.

## Decision

Postgres holds all rollout state. There is no Kafka, Redis, or other broker.

- API handlers are stateless, and the engine rebuilds everything it needs from Postgres
  after a restart. No safety decision uses an in-memory counter.
- Invariants that must hold under concurrency are enforced by the database wherever possible:
  uniqueness constraints, check constraints, and triggers for frozen and append-only data.
- Work that needs to run in the background is found by querying state, using
  `FOR UPDATE SKIP LOCKED` if contention requires it.
- Schema changes are embedded, forward-only migrations. There are no down migrations, because
  an untested rollback path is worse than none. Reverting a change means writing a new
  migration.
- `rolloutd serve` does not migrate. `rolloutd migrate` is a separate step, and a server whose
  schema is behind reports unready.

## Consequences

- There is one consistency model to reason about, and transactions cover multi-row changes.
- Postgres is a single point of failure and the likely first scaling limit. Device polling
  load is the main risk (see ADR 0002), and it will be measured rather than assumed.
- Multi-region operation is out of scope. This design would need rework to support it.
- Tests must use real Postgres, since much of the correctness lives in the database. Each test
  gets its own database, and CI fails rather than skips if Postgres is missing.
