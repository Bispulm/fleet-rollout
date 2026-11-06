# Changelog

Each milestone is tagged. Releases with binaries and images start at the first plain version tag.

## v0.6.0-m5: Release

- Published benchmarks, measured with `go run ./test/bench`: bytes saved by resumable downloads, devices a bad
  release reaches with and without staging, time from breach to halt, and latency and load at fleets of up to
  60,000 devices, with what limits a single rolloutd. See [docs/benchmarks.md](docs/benchmarks.md).
- `make demo`: 10,000 simulated devices take a good release through every stage over a flaky network, then a bad
  release halts in its first stage and rolls back.
- `rolloutctl rollout status --watch` follows a rollout until it finishes or stops for a person, and fails unless it
  completed.
- `rollout-sim fleet --bad-release` makes one release bad and the others fine, so one fleet can take both.
- The simulator can break downloads partway through a response, act like a client without resume, open more
  connections, and spread a fleet's first requests over a window.
- Release builds for Linux and macOS with goreleaser on version tags, as a draft release. Publishing the release
  pushes a multi-platform image.
- An architecture document that covers the whole system.

## v0.5.0-m4: Safety

- Rollback: devices that updated restore the release they ran before, from their previous slot, under the same
  claim rule as installs. By command, or automatically on breach. A rollback that misses its deadline is marked
  incomplete and can be tried again.
- Prometheus metrics, OpenTelemetry traces, a Grafana dashboard, and runbooks. `make obs-up` runs them locally.
- Failure reasons counted in a rollout's status, and a stale device policy.

## v0.4.0-m3: Kubernetes

- `Fleet` and `Rollout` custom resources, and an operator that turns them into API calls.
- A container image with every binary, a Helm chart, and an end-to-end test on kind.

## v0.3.0-m2: Devices

- The device agent: resumable, verified downloads, simulated A/B installs, and health checks that revert.
- Signed artifact manifests, with Ed25519 and a digest per chunk.
- Installs only after the server agrees, so a halt stops every install that hasn't started.
- A simulator that runs fleets of the real agent with faults injected, and reproduces a run from its seed.

## v0.2.0-m1: Rollout core

- The rollout state machine, cohorts frozen at start, staged exposure, and automatic halts.
- The HTTP API, the engine with a fenced lease, and `rolloutctl`.

## v0.1.0-m0: Foundation

- The repository, CI, Postgres migrations, and the first decisions recorded as ADRs.
