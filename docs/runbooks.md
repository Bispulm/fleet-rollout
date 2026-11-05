# Runbooks

What to do when something on the dashboard, or a `Rollout`'s status, needs a person. Each section says how to
recognize the situation, what it means, and what to do. The metrics are described in
[ADR 0012](adr/0012-observability.md), and `rolloutctl rollout status <id>` shows a rollout's own view.

## A rollout halted

**You see:** a rollout in `halted`, `fleet_rollout_rollout_halts_total` going up, or a `Rollout` whose phase is
`halted`. Status shows `Halted: <reason>: <detail>`.

**It means:** a stage crossed a safety limit, and the engine stopped all new exposure and cancelled work that hadn't
started installing, in one transaction. Devices already installing finish, and their results still count. The
reasons are in [rollout-semantics.md](rollout-semantics.md#when-a-stage-halts):

| Reason | What crossed the line |
|---|---|
| `stage_breach` | The stage's own observed failure rate, once it had its minimum sample |
| `certain_breach` | Failures so many that the stage would fail whatever the rest report |
| `rollout_breach` | The failure rate across every exposed device |
| `late_regression` | A stage that had already passed started failing |
| `stage_deadline` | The stage ran out of time, without the coverage, soak, or known devices it needed |

**Do:**

1. Read the detail, and `rolloutctl rollout status <id>` for each stage's counts. A `stage_deadline` halt with many
   unknown devices points at devices that can't reach the control plane, not at the release.
2. Look at why devices failed: status shows `Failures:` counted by the reason each device gave. `unhealthy` is the
   release. `interrupted` is devices losing power mid-install. `integrity` or `invalid_signature` is the artifact or
   its signing.
3. If the release is bad, roll back: `rolloutctl rollout rollback <id> --reason "<why>"`. Each device that updated
   restores what it ran before. If it was a false alarm, abort with `rolloutctl rollout abort <id>`, and start a fresh
   rollout. A halted rollout can't be resumed, so that the halt stays on the record.

## A rollback is incomplete

**You see:** a rollout in `rollback_incomplete`, or a `Rollout` whose phase says so. Status shows
`Rollback: attempt N: X of Y devices restored, F failed, U unresolved`.

**It means:** the rollback deadline passed before every device that updated confirmed its restore. Rollback work that
hadn't started was cancelled. The fleet stays busy, so no other rollout can start on it.

**Do:**

1. Unresolved devices are usually offline. `rolloutctl device list --fleet <fleet>` shows when each was last seen.
2. A failed restore reports `nothing_to_restore`, when the device's previous slot no longer holds the release to go
   back to, or `restore_unhealthy`, when that release failed its own health check. Those devices need attention one
   by one, outside this rollout.
3. When the offline devices are back, `rolloutctl rollout rollback <id>` starts another attempt, with a new deadline,
   for every device not yet restored. Or abort, which leaves devices as they are and frees the fleet.

## Device requests are being shed

**You see:** `fleet_rollout_device_requests_shed_total` going up, by pool, and devices getting `503` with
`Retry-After`.

**It means:** more device requests arrived at once than the pool allows, `ROLLOUTD_MAX_DEVICE_REQUESTS` for polls and
reports, or `ROLLOUTD_MAX_DEVICE_DOWNLOADS` for downloads. The excess is turned away before touching Postgres, and
devices come back later, spread out. A burst after an outage, when a whole fleet reconnects, is expected and passes.

**Do:**

1. If it's a short burst, nothing. Check that it falls off within a few minutes.
2. If it lasts, compare request latency and Postgres load. If both have headroom, raise the limit. If Postgres is
   the bottleneck, raising it only moves the queue into the database: lengthen `ROLLOUTD_POLL_INTERVAL` instead.
3. Downloads have their own pool so that slow downloads can't crowd out reports. If only the download pool is shed,
   devices are on slow links. Raise `ROLLOUTD_MAX_DEVICE_DOWNLOADS`, or use a smaller chunk size when pushing
   artifacts.

## Nothing is advancing rollouts

**You see:** `fleet_rollout_engine_leader` is 0 on every instance, and rollouts stop moving between stages.

**It means:** no rolloutd instance holds the engine lease. Every instance tries to take it every
`ROLLOUTD_ENGINE_INTERVAL`, and keeps it by renewing within `ROLLOUTD_ENGINE_LEASE_TTL`. Device polls and reports
still work, but stages don't progress or halt.

**Do:**

1. Check that at least one instance runs with `ROLLOUTD_ENGINE_ENABLED` left on.
2. Look in the logs for `no longer certain of engine leadership` or `acquire engine lease`. Both mean the database
   is slow or unreachable from that instance.
3. A lease held by an instance that died expires after the TTL, and another instance takes over. Waiting a TTL is
   enough. Never edit the `engine_lease` row by hand: its generation is what keeps a stale leader from committing.

## Rollout metrics can't be read

**You see:** `fleet_rollout_rollout_scrape_errors` is 1, and the rollout panels are empty.

**It means:** rolloutd couldn't read rollout state from Postgres within five seconds, during a scrape. Events like
halts are still counted.

**Do:** check that Postgres is reachable and not overloaded. The rest of rolloutd's metrics and its readiness check
show whether the database connection itself is healthy.

## A `Rollout` resource isn't ready

**You see:** `kubectl get rollout <name> -o yaml` shows the `Ready` condition false, with a reason.

| Reason | Do |
|---|---|
| `ArtifactNotFound` | Push the artifact the resource names, with `rolloutctl artifact push`. The operator checks again every 30 seconds. |
| `FleetNotFound` | Create the fleet, with a `Fleet` resource or `rolloutctl fleet create`. |
| `Blocked` | Another rollout is active on the fleet. It starts once that one finishes, or is aborted. |
| `Invalid` | The policy can't work for this fleet, and the message says why. The spec can't change, so delete the resource and apply a corrected one. |
| `ControlPlaneUnavailable` | The operator can't reach rolloutd, or rolloutd is shedding load. It retries with backoff. Check that rolloutd is running and ready. |

A `Rollout` whose phase disagrees with `spec.paused` was paused or resumed some other way, such as through
`rolloutctl` during an incident. That's deliberate: change the resource to take back control.
