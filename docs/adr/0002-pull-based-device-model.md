# ADR 0002: Devices poll the control plane

Status: Accepted

## Context

Devices go offline without warning and usually can't accept inbound connections. The control
plane has to tolerate an entire fleet reconnecting at once after an outage.

## Decision

Devices initiate all communication. Each device polls for its assignment on an interval with
random jitter, downloads artifacts over HTTP Range requests, and reports results. The server
never opens a connection to a device.

Poll responses can tell a device when to poll next, which gives the server a way to shed load
during a reconnect storm.

## Capacity estimate

These numbers are estimates to check against measurements, not results.

| Fleet size | Poll interval | Mean polls per second |
|---|---:|---:|
| 100 | 60s | 1.7 |
| 10,000 | 60s | 167 |
| 10,000 | 15s | 667 |

If most polls need a single indexed read, 167 per second is within reach of one Postgres
instance. The harder case is a reconnect storm, where unjittered clients could send the whole
fleet's requests within seconds: at 10,000 devices that is thousands of requests per second,
far above the steady rate. Handling it relies on jitter, bounded server concurrency, and
overload responses that tell clients to back off. Load tests must cover this case specifically.

## Consequences

- Exposure changes reach devices only when they next poll, so pausing a rollout can't recall an
  update a device has already picked up. Assignments carry expiry and generation checks to limit
  this, and the remaining exposure is documented in the rollout semantics.
- A device that stops polling is indistinguishable from a broken one. The system treats it as
  unknown, never as healthy.
- Polling costs requests even when nothing is happening. A longer interval reduces the load at
  the cost of slower rollouts, and it's a configuration choice.
