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
| Unknown | A cohort device with no terminal outcome and no report within `stale_after` |
| Pending | A cohort device with no terminal outcome that has reported recently or isn't stale yet |

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

**Status: Specified,** except where a subsection says otherwise. The engine isn't built yet.

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
3. **Rollout-wide breach:** the same comparison across every exposed cohort, using
   `rollout_max_failure_bps`.
4. **Late regression:** a stage that already passed now breaches on its current counters.
5. **Deadline:** the stage's deadline has passed and the coverage gate or the unknown gate isn't met.

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

**Status: Implemented** for the clocks. Evaluating safety during a pause is **Specified**.

Pause stops the soak and deadline clocks. On resume, the deadline moves later by the length of the
pause, so a pause can't make a stage time out by itself. Safety checks keep running during a pause,
so a breach moves the rollout from `paused` to `halted`.

Pausing prevents new forward assignments atomically (see "Exposure already in flight").

### Late reports

A stage's verdict is immutable once recorded. A late report still updates the assignment and the
stage's counters:

- A late failure can trigger the rollout-wide or late-regression check. It doesn't rewrite the old
  verdict, and the halt reason names the stage and the time.
- A late success can't undo a halt. Halts are cleared by an operator, not by more data arriving.

### Halted rollouts

A halted rollout doesn't resume. The operator either rolls back or aborts, and a fresh rollout
is the way forward after a false alarm. This keeps "this release tripped a safety check" a permanent
part of the rollout's record.

## States and transitions

**Status: Implemented** for start, pause, resume, and abort, including row locking, revision checks,
and the audit log. Events driven by the engine or by rollback are **Specified**: the table allows them,
but nothing issues them yet.

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

**Status: Specified.** The schema's uniqueness and append-only rules are implemented and tested, but
no code issues assignments or accepts reports yet.

- A device has at most one open assignment, across all rollouts.
- Each assignment has a generation and an expiry. Pause, halt, and rollback stop new forward
  assignments in the same transaction that changes rollout state.
- Reports are bound to the authenticated device, rollout, assignment generation, and a sequence
  number the agent persists.
- An identical repeat of a report is acknowledged and changes nothing. A different payload with an
  already-used sequence is rejected.
- A report for an old generation is recorded in the event log and can't overwrite a newer decision
  or count twice toward an outcome.

### Exposure already in flight

A device that has fetched an assignment can't be recalled until its next poll. Pause and halt
therefore can't undo in-flight work: those devices may still finish installing. Forward exposure
after a halt is bounded by the assignments already issued, and the status API will report that count
so nobody has to guess.

## Rollback

**Status: Specified.**

An authorized rollback tells each exposed device to restore the healthy version it recorded before
this rollout. It doesn't disable downgrade protection in general: the authorization is scoped to one
assignment. A rollout reaches `rolled_back` only when every exposed device has confirmed. Offline
devices and failed reversions stay listed as unresolved in `rollback_incomplete`.

Rolling back a rollout that has already `completed` isn't supported in v1.
