# fleet-rollout

A control plane for staging software updates across a fleet of unreliable devices, written in Go.
It exposes each update to a growing share of the fleet, watches what happens, and stops before a
bad release reaches everyone.

**Status: early development.** Devices can't receive updates yet. Today you can register a fleet
and its devices, record artifact metadata, create a rollout with a validated safety policy, start
it, and inspect its frozen, deterministic cohort. See [what works](#what-works-today) and the
[roadmap](#roadmap).

All devices are simulated. This isn't meant to run on real hardware.

## Quick start

You need Go, Docker with Compose, `make`, and `openssl`. Run these from the repository root.

```bash
make dev-up build

export ROLLOUTD_DATABASE_URL='postgres://rollout:rollout_dev@127.0.0.1:15431/rollout?sslmode=disable'
export ROLLOUTD_ADMIN_TOKEN="$(openssl rand -hex 24)"
export ROLLOUT_ADMIN_TOKEN="$ROLLOUTD_ADMIN_TOKEN"

./bin/rolloutd migrate
./bin/rolloutd serve 2> rolloutd.log &
sleep 1

./bin/rolloutctl fleet create lab-a
./bin/rolloutctl device register-synthetic --fleet lab-a --count 100 --platform linux/arm64

# Any file works as an artifact. Only its size and digest are recorded for now.
head -c 4096 /dev/urandom > demo-app.bin
./bin/rolloutctl artifact register --name demo-app --version 1.4.0 --platform linux/arm64 \
  --release 14 --file demo-app.bin -o json | tee artifact.json
ARTIFACT_ID="$(sed -n 's/.*"id": "\(.*\)",/\1/p' artifact.json)"

# Choosing the rollout ID makes the cohort below reproducible on any machine.
ROLLOUT_ID=11111111-1111-4111-8111-111111111111
./bin/rolloutctl rollout create --fleet lab-a --name demo-1-4-0 --artifact "$ARTIFACT_ID" \
  --id "$ROLLOUT_ID" --stages 1,10,50,100 --soak 10m --max-failure-rate 2%

./bin/rolloutctl rollout start "$ROLLOUT_ID"
./bin/rolloutctl rollout cohort "$ROLLOUT_ID" --stage 2
```

The last command lists the devices that stage 2 newly exposes:

```text
POSITION  STAGE  DEVICE     RANK KEY
2         2      sim-00056  08aa74be5fc92060
3         2      sim-00036  0a9d9dd1613b3145
4         2      sim-00098  104b051b6effa58f
5         2      sim-00001  1083c6501163dec3
6         2      sim-00010  123fb30c1e5bccb2
7         2      sim-00094  12488d8dd96e83ed
8         2      sim-00086  18c7b8b2c30668f7
9         2      sim-00034  1f78069ef21fc51b
10        2      sim-00091  1ff4c06f99dc2524
```

Then try the parts that refuse to do the wrong thing:

```bash
# A stage that can never reach its minimum sample size is rejected up front.
./bin/rolloutctl rollout create --fleet lab-a --name impossible --artifact "$ARTIFACT_ID" \
  --stages 1,100 --max-failure-rate 2% --min-observations 5,1

./bin/rolloutctl rollout pause "$ROLLOUT_ID" --reason "checking dashboards"
./bin/rolloutctl rollout start "$ROLLOUT_ID"   # rejected: not a valid transition
./bin/rolloutctl rollout resume "$ROLLOUT_ID"
./bin/rolloutctl rollout history "$ROLLOUT_ID"
```

To clean up:

```bash
kill %1                # stop rolloutd
make dev-reset         # stop Postgres and delete this project's data
rm -f artifact.json demo-app.bin rolloutd.log
```

## What works today

| Area | Behavior |
|---|---|
| Fleets and devices | Registration is all or nothing. Device tokens are shown once and stored as hashes. |
| Rollout policy | Validated when created and again when started. A rollout that could never satisfy its own policy is rejected. Defaults are copied into the rollout, so later changes can't affect it. |
| Cohorts | Devices rank by a hash of the rollout ID and device ID, so stages are nested and reproducible. |
| Frozen data | Membership and policy freeze at start. Database triggers enforce this, not just application code. |
| Transitions | Start, pause, resume, and abort each run in one transaction with a row lock and an audit entry. Concurrent attempts have one winner. |
| Operations | Health and readiness endpoints, drain on shutdown, structured logs, and separate admin and device credentials. The server binds to loopback unless told otherwise. |

[docs/rollout-semantics.md](docs/rollout-semantics.md) is the behavior contract, including what is
specified but not built yet. [docs/architecture.md](docs/architecture.md) shows how the pieces fit
together, and [docs/adr](docs/adr) records the decisions.

## Roadmap

None of this exists yet.

- **Rollout core:** assignments, device polling, idempotent reports, and the engine that advances
  stages under a fenced lease.
- **Devices:** signed artifacts, a durable agent with resumable downloads and simulated A/B
  installs, and a failure-injecting simulator that runs the real agent.
- **Kubernetes:** custom resources, an operator, and a Helm chart.
- **Safety and operations:** automatic halt, rollback, stale-device policy, metrics, traces, and
  dashboards.
- **Release:** published benchmarks and a full bad-release demo.

Real hardware, vehicle protocols, multi-region control planes, a web UI, delta updates, and a full
TUF implementation are out of scope.

## Development

Run `make help` to list the targets.

```bash
make dev-up            # Postgres on 127.0.0.1:15431
make test-integration  # all tests, with the race detector, on real Postgres
make check             # lint, tests, vulnerability check, and secret scan
```

`make test` runs without a database, and database tests skip. CI sets `ROLLOUT_TEST_REQUIRE_DB=1`,
so they fail there instead of skipping.

## License

Apache-2.0. See [LICENSE](LICENSE).
