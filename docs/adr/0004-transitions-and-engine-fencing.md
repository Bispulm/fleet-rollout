# ADR 0004: Transactional transitions and a fenced engine lease

Status: Accepted

## Context

Operators, API replicas, and the rollout engine can all try to change a rollout at once. Leader
election alone doesn't protect against this: a paused process can wake up believing it still leads
and write stale decisions after another instance has taken over.

## Decision

### Transitions

Each transition runs in one transaction: lock the rollout row with `FOR UPDATE`, validate the
transition against the current state, compare any revision the caller supplied, bump the revision, and
append an audit row. Pure rules live in a package with no database dependency.

Callers may omit the expected revision. An operator pausing a rollout during an incident wants the
pause to apply regardless of what changed in the last second, while the state check still rejects
anything invalid. Callers that act on a value they read should pass it.

### Engine leadership

Leadership is a lease row in Postgres rather than an advisory lock:

```sql
INSERT INTO engine_lease AS l (name, holder, generation, expires_at)
VALUES ($name, $me, 1, clock_timestamp() + $ttl)
ON CONFLICT (name) DO UPDATE
    SET holder = EXCLUDED.holder, generation = l.generation + 1, expires_at = EXCLUDED.expires_at
    WHERE l.expires_at <= clock_timestamp()
RETURNING generation;
```

Every engine transaction begins with this check, and aborts unless it matches its own generation:

```sql
SELECT generation FROM engine_lease
WHERE name = $name AND holder = $me AND generation = $mine AND expires_at > clock_timestamp()
FOR SHARE;
```

This fences stale writers. Takeover needs an exclusive row lock, so it waits for in-flight engine
transactions to finish. Any transaction that starts after takeover fails the check, however long its
process was paused. Only Postgres's clock is used, so clock skew between engine instances doesn't
matter.

An engine that loses the lease, or can't renew it, stops work and tries to reacquire. That includes a
network error, since it can no longer be sure that it leads. An expired lease can't be renewed, even if
nobody took it, because the holder can't know what happened in the meantime.

Alternatives considered:

- **Session advisory lock.** The lock's lifetime is tied to a connection, so the holder has to detect
  connection loss, and a stale process's in-flight work still needs separate fencing. A lease gives
  both in one mechanism.
- **Kubernetes Lease.** It doesn't work outside a cluster, and it can't participate in the Postgres
  transaction that writes the decision.

## Consequences

- Failover takes up to one lease TTL, and during that time no stage advances. That's acceptable, since
  it only delays progress. API-driven pauses and reports aren't affected.
- Each engine transaction carries an extra row read, and a share lock that serializes with takeover.
- Timers like soak use timestamps from the application's injected clock, so tests can control them.
  Skew between API replicas can shift a soak by that skew, which is small against soaks measured in
  minutes. Lease validity still uses the database clock only.
