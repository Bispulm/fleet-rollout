# ADR 0011: Rollback restores what each device ran before

Status: Accepted

## Context

A halt stops a bad release from reaching more devices, but the devices that already installed it keep running it.
The state machine has had rollback states since M1, and the semantics have said what rollback means without saying
how. Devices are pull-based and often offline, so a rollback, like a rollout, has to be something devices learn
about when they poll, and confirm when they're done.

## Decision

**A device restores the release it ran before this rollout, from its own previous slot.** The A/B layout means the
release a device replaced is still on disk, and the agent records it as its previous release when an install
succeeds. Restoring it needs no download, and nothing new has to be trusted. A device whose previous slot doesn't hold
the release it had before reports a failure, which leaves it unresolved. Downloading an older artifact to roll back to
is out of scope for v1.

**The control plane records each member's release when membership freezes.** That's the rollback target, and when a
device confirms, its installed release goes back to it, so the inventory stays right.

**Rolling back is a transition, by command or by policy.** `POST /v1/rollouts/{id}/rollback` works from running,
soaking, paused, halted, and rollback_incomplete. With `on_breach: rollback`, the engine halts a rollout that breaches
and rolls it back in the same transaction, recording both. Starting a rollback supersedes forward work that hasn't
reached installation, as a halt does, and stops the current stage.

**Rollback assignments are issued when devices poll.** A member needs one if its forward assignment succeeded. A device
still installing finishes first, because an install can't be recalled, and is offered its rollback afterward. A
rollback assignment has kind `rollback`, names the release to restore, and has a generation above the forward one, so
late reports from the forward assignment can't be mistaken for it. Each rollback attempt uses a new generation.

**Restoring follows the same rules as installing.** The device asks permission by reporting `installing`, which the
server decides against the rollout as it stands (ADR 0009). It health-checks the previous slot, switches to it in one
save, and reports `health_check` then `succeeded`. The release a device shipped with was never installed by the agent,
so its slot has nothing the installer can check. Restoring that one switches back without a slot check, since the
device was running it before this rollout. Downgrade protection still applies to everything else: the
permission to go back is scoped to this one assignment, and only to the release the device itself recorded.

**A rollback ends when every device that needed it has confirmed.** The engine moves it to `rolled_back`, which is
terminal and frees the fleet. If the rollback deadline passes first, it moves to `rollback_incomplete`, supersedes
rollback work that hasn't started, and lists the devices still unresolved. An operator can try again, which starts a
new attempt with a new deadline and offers a rollback to every member not yet restored, or abort. The deadline is the
policy's `rollback_deadline`, an hour by default.

Alternatives considered:

- **Rolling back with a new forward rollout of the old artifact.** It reuses everything, but downgrade protection would
  refuse it, and loosening that generally is the wrong trade. It also loses the connection between the bad release
  and the devices it reached.
- **Pushing rollbacks to devices.** Devices poll for the same reasons as before (ADR 0002).
- **Counting a device as rolled back once its assignment is issued.** A rollback isn't done until a device says so.

## Consequences

- Rolling back needs every exposed device to come back online, and a rollout can sit in `rollback_incomplete` until
  they do or an operator aborts.
- Devices that failed their forward install already reverted, and devices never reached never changed, so neither
  is offered a rollback.
- A device can only roll back to the release immediately before. A device that installed a later release through a
  different rollout before this rollback reaches it has nothing to restore for this rollout, and reports that.
