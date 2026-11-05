# Rollout semantics

This is the contract for how rollouts behave. Each section is marked with its implementation
status, since the semantics were settled before all of the code that enforces them exists.

- **Implemented:** enforced by code and covered by tests.
- **Specified:** decided, and not built yet. Code must follow this when it's written.

## Vocabulary

| Term | Meaning |
|---|---|
| Member | A device in a rollout's frozen membership |
| Stage | A step that raises exposure to a cumulative percentage of members |
| Cohort | The members newly exposed by a stage |
| Observed | A cohort device with a terminal outcome, `succeeded` or `failed` |
| Unknown | A cohort device with no terminal outcome that hasn't been heard from, by poll or report, within `stale_after` |
| Pending | A cohort device with no terminal outcome that has been heard from recently |
| In flight | A device past the point where it can be recalled: `installing`, `health_check`, or `reverting` |

Unknown is computed when it's needed, from `last_seen_at` and the stage's start time, and never stored.
There's nothing to keep in sync, and a device that comes back stops being unknown immediately.

## Membership

**Status: Implemented.**

Membership freezes in the transaction that starts a rollout and never changes afterward. Database
triggers reject inserts, updates, and deletes on frozen membership, so an application bug can't
alter it either.

A device is a member if all of these hold at that moment:

1. It belongs to the rollout's fleet and its status is `active`.
2. Its platform equals the artifact's platform.
3. Its labels contain every key and value in the rollout's selector.
4. It isn't listed in the rollout's `exclude_devices`.
5. Its installed release counter is lower than the artifact's.

Every other device in the fleet gets a recorded exclusion with a reason, so "why didn't this device
update?" has an answer.

| Situation | Behavior |
|---|---|
| Device enrolled after start | Not a member. A later rollout covers it. |
| Device relabeled after start | No effect on this rollout. |
| Device excluded or decommissioned after start | Remains a member. It goes unknown if it stops reporting, and it isn't silently dropped from the denominator. |
| Fleet with no eligible devices | The rollout is rejected. |

## Cohorts

**Status: Implemented.**

Each device's rank key is SHA-256 over a domain tag and the length-prefixed rollout ID and device
ID. Members sort by rank key, then by device ID as a tie-breaker, and get positions 1 to N.

A stage at `percent` covers the first `ceil(percent * N / 100)` positions. Stages must have strictly
increasing integer percentages ending at 100, which makes every stage a prefix of the next: a
device exposed early stays included as the rollout expands.

The same rollout ID and device set always yield the same order. A different rollout ID yields an
independent order, so the same devices aren't always first.

## Small fleets and sample sizes

**Status: Implemented.**

Rounding up means every non-empty stage exposes at least one device, and rounding can leave a stage
with no new devices. On a fleet of 3 with stages 1, 10, 50, 100, the targets are 1, 1, 2, 3.

- A stage with an empty cohort is marked `skipped`. It's shown in status, gets no soak, and isn't
  evaluated, because it exposes nobody.
- A non-empty cohort smaller than the stage's `min_observations` can never satisfy its own gate, so
  the rollout is rejected with an error naming the stage and sizes. The minimum isn't lowered to
  fit, because that would silently weaken the policy the operator wrote.

Feasibility is checked at creation against the devices eligible then, and again at start against
the frozen membership. A rollout that passes both can't get stuck on sample size.

## Stage progression

**Status: Implemented.**

All comparisons use integer arithmetic on basis points, where 10,000 is 100%, so there is no
floating-point rounding at a boundary.

### Failure rate

`failed` and `succeeded` count terminal outcomes in the stage's own cohort. Earlier stages'
successes can't dilute a failing new cohort.

```text
rate    = failed / (failed + succeeded)
breach  = failed * 10000 > max_failure_bps * (failed + succeeded)
```

A rate exactly at the threshold is not a breach: `max_failure_bps` is the highest allowed value. At
200 bps, 1 failure in 50 passes and 2 in 99 breach. With `max_failure_bps` of 0, any failure breaches.

Missing reports are never successes. A device with no terminal outcome is in neither the numerator
nor the denominator, and the coverage and unknown gates below account for it.

### When a stage halts

The engine halts the rollout when any of these is true, while it's running, soaking, or paused:

1. **Observed breach:** `observed >= min_observations` and the rate breaches.
2. **Certain breach:** `failed * 10000 > max_failure_bps * cohort`. The stage would breach even if
   every remaining device succeeded, so waiting adds exposure and no information.
3. **Rollout-wide breach:** the same rate comparison across every exposed cohort, using
   `rollout_max_failure_bps`. It applies once the total observed reaches the current stage's
   `min_observations`, so a single early failure can't bypass the sample size the operator chose.
4. **Late regression:** a stage that already passed now breaches on its current counters.
5. **Deadline:** the stage's deadline has passed and the coverage gate or the unknown gate isn't met.
   This one doesn't apply while paused, because the deadline clock is stopped.

### Coverage and soak

Soak starts when the coverage gate is first met, not when assignments go out, because an update
that hasn't finished installing can't be soaking.

```text
coverage gate: observed >= min_observations
               and observed * 10000 >= required_coverage_bps * cohort
```

A soak of zero is allowed. The stage then promotes at the first evaluation that passes.

### Promotion

A stage promotes only when all of these hold at the same evaluation:

1. The coverage gate is met.
2. There's no observed or certain breach for the stage.
3. `unknown * 10000 <= max_unknown_bps * cohort`, for the stage and for all exposed devices.
4. There's no rollout-wide breach.
5. Accumulated soak time is at least the stage's soak.

Before the deadline, too many unknown devices block promotion without halting, because they may just be
slow to report. The deadline bounds that wait: a rollout can't sit blocked forever, because past the
deadline the same condition halts it. Validation requires `deadline` to exceed `stale_after`, so a
device has time to be classified as unknown before the deadline applies.

### Pause

Pause stops the soak and deadline clocks. On resume, the deadline moves later by the length of the
pause, so a pause can't make a stage time out by itself. Safety checks keep running during a pause,
so a breach moves the rollout from `paused` to `halted`.

Pausing prevents new forward assignments atomically, and tells devices that haven't started installing to
hold. Devices already installing finish. A device that asks to start installing during a pause is refused,
and that request is ordered against the pause, so once a pause has committed no new install starts.

### Late reports

A stage's verdict is immutable once recorded. A late report still updates the assignment and the
stage's counters:

- A late failure can trigger the rollout-wide or late-regression check. It doesn't rewrite the old
  verdict, and the halt reason names the stage. The stage that happened to be current ends as `stopped`
  rather than `failed`, because its own devices did nothing wrong. An abort stops the current stage the
  same way.
- A late success can't undo a halt. Halts are cleared by an operator, not by more data arriving.

### Halted rollouts

A halted rollout doesn't resume. The operator either rolls back or aborts, and a fresh rollout
is the way forward after a false alarm. This keeps "this release tripped a safety check" a permanent
part of the rollout's record.

## States and transitions

**Status: Implemented,** except for the rollback events, which the table allows but nothing issues yet.

| From | Event | To |
|---|---|---|
| draft | start | running |
| draft | abort | aborted |
| running | coverage met | soaking |
| soaking | soak passed, more stages | running |
| soaking | soak passed, final stage | completed |
| running, soaking | pause | paused |
| paused | resume | the state it was paused from |
| running, soaking, paused | breach | halted |
| running, soaking, paused, halted | rollback | rolling_back |
| running, soaking, paused, halted | abort | aborted |
| rolling_back | all members resolved | rolled_back |
| rolling_back | rollback deadline passed with devices unresolved | rollback_incomplete |
| rollback_incomplete | rollback | rolling_back |
| rollback_incomplete | abort | aborted |

`completed`, `aborted`, and `rolled_back` are terminal. A fleet may have one non-terminal started
rollout at a time, enforced by a partial unique index, so a device can't get competing instructions.

Every transition happens in one transaction that locks the rollout row, checks the current state
and any revision the caller supplied, bumps the revision, and appends an audit row.

## Assignments and reports

**Status: Implemented.**

### Issuing

A device gets a forward assignment when it polls, if all of these hold in that transaction:

1. Its fleet has a rollout that is `running` or `soaking`.
2. It's a member whose stage is at or before the current stage.
3. Its status is `active`. A device excluded after the rollout started stays a member, but gets no new
   work, so it's counted as pending or unknown and isn't dropped from the denominator.
4. It has no open assignment, and no earlier forward assignment in this rollout.

The poll takes a shared lock on the rollout row, so it serializes with pause, halt, and abort: once one
of those commits, no new forward assignment can be issued. A device has at most one open assignment
across all rollouts, and at most one forward assignment per rollout, both enforced by unique indexes.
A device that polls twice at once gets the same assignment both times.

A decommissioned device can't authenticate at all.

### Directives

Each poll returns the device's open assignment with a directive.

| Rollout state | Assignment not yet installing | Assignment in flight |
|---|---|---|
| `running`, `soaking` | `proceed` | `proceed` |
| `paused` | `hold` | `proceed` |
| `halted`, `aborted`, `completed`, or rolling back | Superseded, so no assignment is returned | `proceed`, to finish and report |

A directive is advice about the next step, and can be out of date by the time the device acts on it. An agent
starts installing only once the server has accepted its report of `installing`, as described under Reports and in
[ADR 0009](adr/0009-installs-need-the-servers-agreement.md), so a stale `proceed` can't start an install the
server would refuse. An agent holding an assignment that a poll no longer returns must abandon it, unless it's
already in flight.

### Reports

- A report names its rollout, assignment, generation, and a sequence number the agent persists. The
  assignment must belong to the authenticated device. Anything else is treated as not found.
- An identical repeat of a report is acknowledged as a duplicate and changes nothing. The response says
  what became of the original, so a device that lost the first response learns the same thing from the
  retry.
- A different report with an already-used sequence is rejected as a conflict.
- Every new report is recorded in the append-only event log, whether or not it's applied, with the reason
  it was ignored if it wasn't.
- A report is ignored, not rejected, if its assignment has already finished, if a later sequence has
  already been applied, or if it isn't a valid progression. So a delayed report can't overwrite a newer
  decision, and can't count twice, since outcomes come from one row per device.
- Progress only moves forward, and may skip steps. `succeeded` requires having reached `health_check`, so
  a device can't claim success without having checked. `failed` is allowed from any open state.
- A report that moves an assignment into an in-flight state from one that isn't is a request to start
  installing, and it's decided against the rollout as it stands. It's refused as `on_hold` while the
  assignment is held, and the assignment stays open. Once a halt, abort, or completion has superseded the
  assignment, it's ignored as finished. Those transitions update the assignment, so its row lock orders
  them against the report. A pause doesn't, so this one report also takes a shared lock on the rollout
  first. Reports from devices already in flight never wait on it.
- Stage counts are derived from assignments at evaluation time. There are no counters that could drift
  from them, and nothing in memory to lose on restart.

### Exposure already in flight

A device can't be recalled until its next poll. When a rollout halts or aborts:

1. No new forward assignment can be issued, from that transaction onward.
2. Open assignments that haven't started installing are superseded in the same transaction. A report
   from one afterward is recorded and ignored.
3. A device can't start installing until the server accepts its request, and from that transaction onward
   the server refuses. So a halted rollout gains no new installs, however long a device was out of contact.
4. Installs already in flight run to completion and report. That exposure is unavoidable, and status
   shows the count for each stage.

### Devices left over at completion

A rollout completes at its required coverage, so some members may have no outcome. They aren't offered
the release afterward, and any assignment that hasn't reached installation is superseded, so it can't
stand in the way of later work. A follow-up rollout of the same artifact covers these devices, since
members already on that release are excluded from it as `already_current`.

## Artifacts and devices

**Status: Implemented.** The agent, `rollout-agent`, installs by copying an artifact's bytes into a slot, and
nothing runs them.

### What a device checks

Before installing, a device verifies all of these, and any failure is permanent: it reports `failed` with a reason and
installs nothing.

1. The manifest's signature, from a key on the device's own trust list.
2. That the manifest matches what the assignment describes, and the device's platform.
3. The size, against the manifest and against the device's own limit.
4. Every chunk's digest as it arrives, and the full digest at the end.
5. Downgrade protection: the release counter must be above what's running. If it's equal and the digest matches,
   the device already has this release, and reports success without reinstalling. Anything else is rejected.

### Starting to install

Once everything checks out, the device asks the server's permission by reporting `installing`, after any earlier
reports have been delivered. It saves the request first, so a retry is identical.

| Answer | What the device does |
|---|---|
| Applied, or a duplicate of one that was applied | Install |
| Ignored as `on_hold` | Keep the verified download, hold until a poll says proceed, then ask again with a new report |
| Ignored as finished, or a permanent error | Abandon the assignment and install nothing |
| No answer | Wait and send the same request again, for as long as it takes |

### Downloads

A partial download only ever contains verified chunks, because a chunk is checked before it's kept. A response has to
be `206`, with a `Content-Range` starting at the requested offset, a total matching the manifest, and an `ETag` naming
the expected digest.

| What goes wrong | What the device does |
|---|---|
| Connection drops, or a server error | Retry from the last verified chunk, with backoff and jitter |
| Server ignores the Range request and sends `200` | Start over from byte 0 with that response, checking each chunk as it arrives |
| A different `ETag`, offset, or total | Discard the response and count an integrity strike |
| A chunk fails its digest | Discard the chunk and count an integrity strike |
| Too many integrity strikes | Report `failed`, and install nothing |
| No longer assigned, or a `403` | Abandon the download and delete the partial data |

The server only serves an artifact to a device with an open assignment for it, so superseding an assignment also cuts
off the download.

### Retries

Network failures, `429`, and `5xx` are retried with exponential backoff, waiting a random time between half the current
limit and all of it, and respecting `Retry-After`.
Authentication, integrity, and policy failures aren't retried as if they were transient. A `401` means the device can't
do anything useful, so it slows to its longest backoff and keeps its state.

The server serves a bounded number of device requests at once, `ROLLOUTD_MAX_DEVICE_REQUESTS` for polls, reports, and
manifests, and `ROLLOUTD_MAX_DEVICE_DOWNLOADS` for downloads, which hold their slot while a chunk streams. Past that it
answers `503` with a `Retry-After` of a few seconds, before touching the database, rather than queueing. Downloads have
their own pool so that slow ones can't crowd out the reports of devices already installing. So when a whole fleet
reconnects at once, the excess is turned away cheaply, and the devices come back spread out by the server's
`Retry-After` and their own jitter.

### Crashes

Everything needed to recover is saved atomically before it's acted on: the assignment, verified download progress, the
active and pending slots, the previous healthy artifact, the report sequence, and reports not yet acknowledged.

| Crash during | On restart |
|---|---|
| Downloading | Discard anything past the last verified chunk, and continue |
| Verifying | Verify again from the start |
| Installing or health check | The new slot can't be trusted. Keep the previous one, and report `reverting` then `failed` |
| Reverting | Finish reverting |
| After recording success | The new slot is active. Send any reports still outstanding |
| Between sending a report and recording that it was acknowledged | Send it again. The server treats it as a duplicate |
| Between asking to install and recording the answer | Ask again with the same request. The server answers a duplicate with what became of the first, so the device installs only if it was accepted |

### Simulator faults

The simulator runs the real agent and injects faults only at its boundaries: dropped requests, responses lost after
the server acted, corrupted and truncated downloads, duplicate reports, crashes at any save or during an install,
devices offline for a while, everyone reconnecting at once, latency in live runs, and releases that fail their health
check on a share of devices. See [ADR 0008](adr/0008-step-driven-agent-and-simulator.md) for what a seed does and
doesn't reproduce.

A crash during an install or health check counts against the release, because a release that reboots the device looks
exactly the same. A fleet that loses power often therefore needs a failure limit above that background rate, and early
stages big enough that one interrupted install doesn't decide them. The simulator's presets are sized that way.

## Rollback

**Status: Specified,** in detail in [ADR 0011](adr/0011-rollback.md). Until it's built, a policy with
`on_breach: rollback` is rejected at creation rather than accepted and silently treated as a halt.

An authorized rollback tells each exposed device to restore the healthy version it recorded before
this rollout. It doesn't disable downgrade protection in general: the authorization is scoped to one
assignment. A rollout reaches `rolled_back` only when every exposed device has confirmed. Offline
devices and failed reversions stay listed as unresolved in `rollback_incomplete`.

### Starting

`rollback` is allowed from `running`, `soaking`, `paused`, `halted`, and `rollback_incomplete`. With `on_breach:
rollback`, a breach halts the rollout and rolls it back in one transaction, and the audit log shows both. Starting a
rollback:

1. Supersedes forward assignments that haven't reached installation, so no new device installs the release.
2. Stops the current stage, which keeps any verdict it already had.
3. Starts a new rollback attempt, with a deadline of `rollback_deadline` from now, an hour by default.

### Who rolls back

Each member records the release it ran when membership froze. A member needs a rollback if its forward assignment
succeeded. A device whose forward assignment is still in flight finishes first, and then needs one if it succeeded.
Devices that failed already reverted, and devices never reached never changed.

A device that needs a rollback gets a rollback assignment when it polls: kind `rollback`, with the release to restore,
and a generation of one more than the attempt number, so the first attempt uses generation 2. A rollback assignment
follows the same report rules as a forward one, including asking permission with `installing`.

### What a device does

| The device is running | It does |
|---|---|
| The rollout's release, with its previous slot holding the release to restore | Asks permission, health-checks the previous slot, switches to it, and reports `succeeded` |
| The release to restore already | Reports `succeeded`, since there's nothing to undo |
| Anything else, or its previous slot holds some other release | Reports `failed` with `nothing_to_restore` |

The release a device shipped with was never installed by the agent, so there's nothing in its slot to check. Restoring
it switches back without a slot health check.

### Finishing

The engine moves a rollback to `rolled_back` once every device that needed one has a succeeded rollback assignment,
and no forward install is still in flight. `rolled_back` is terminal, and frees the fleet. On success, each device's
installed release returns to the one it had before.

If the deadline passes first, the rollout moves to `rollback_incomplete`. Rollback assignments that haven't reached
installation are superseded, and the status lists the devices still unresolved. Rolling back again starts a new
attempt, which offers a rollback to every member that needed one and hasn't been restored. Aborting leaves the
devices as they are.

Rolling back a rollout that has already `completed` isn't supported in v1.
