# ADR 0003: Deterministic cohorts and frozen membership

Status: Accepted

## Context

Staged rollouts need a device order that is stable, so an earlier device stays exposed as a rollout
expands; fair, so one device isn't always first; and reproducible, so a failure scenario can be
replayed. Changing a rollout's membership or policy while it runs would make its failure rates hard
to interpret and could expose devices the operator didn't intend to.

## Decision

- Rank is SHA-256 over a versioned domain tag and the length-prefixed rollout ID and device ID, with
  the device ID as a tie-breaker. Stage targets are `ceil(percent * N / 100)`.
- Membership is computed and stored once, in the start transaction, with each device's position and
  stage. It isn't recalculated at read time.
- The policy is normalized at creation, with defaults filled in, and stored with its digest. Changing
  a default later can't alter an existing rollout.
- Triggers reject changes to the policy and to frozen membership.
- Callers may supply the rollout ID, which makes cohorts reproducible across databases for simulation
  and tests.

Alternatives considered:

- **Stored random shuffle.** Also stable, but not reproducible from its inputs alone.
- **Non-cryptographic hash.** Faster, but start already writes one row per member, so hashing is unlikely
  to be the bottleneck. SHA-256 also keeps the distribution even when device names follow a pattern, and
  a benchmark can revisit this choice if start time turns out to matter.
- **Clamping `min_observations` to the cohort size.** This keeps tiny fleets working, but it silently
  weakens a safety policy. Rejecting with a precise error makes the operator choose.
- **Rejecting empty stages.** Stages like 1, 10, 50, 100 would fail on any fleet under 10 devices for no
  safety benefit, since a stage exposing nobody risks nothing. They're marked `skipped` instead.

## Consequences

- Starting a rollout writes one row per member in a single transaction. Its cost grows with fleet size
  and will be measured.
- Percentages are integers, so stages below 1% aren't possible in v1.
- A device enrolled mid-rollout waits for the next rollout, which is simple to explain and test.
