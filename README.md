# fleet-rollout

A control plane for staging software updates across a fleet of unreliable devices, written in Go.
It exposes each update to a growing share of the fleet, watches what happens, and stops before a
bad release reaches everyone.

The control plane issues assignments to devices that poll, takes their reports, advances stages, and halts
a rollout that crosses a safety threshold. The device agent downloads signed artifacts in resumable chunks,
asks before it installs, and reverts a release that fails its health check. A rollout that breaches can roll
back, restoring what each device ran before. A simulator runs fleets of the real agent with faults injected
around it. On Kubernetes, an operator turns `Rollout` resources into rollouts. Metrics, traces, and a
dashboard show it all happen. See [what works](#what-works-today), the [benchmarks](#benchmarks), and the
[limitations](#limitations).

All devices are simulated. This is a portfolio project, not meant to run on real hardware.

```mermaid
flowchart LR
    CLI[rolloutctl] --> API
    CR[Rollout resources] --> OP[rollout-operator] --> API
    AGENT[rollout-agent on each device] -->|poll, download, report| API
    subgraph rolloutd
        API[HTTP API] --> STORE[store]
        ENG[engine] --> STORE
    end
    STORE --> DB[(Postgres)]
    rolloutd --> OBS[Prometheus, Grafana, Jaeger]
```

Postgres holds all state, and any number of rolloutd instances can serve devices, while one at a time runs the
engine. [docs/architecture.md](docs/architecture.md) explains how the pieces fit together.

## Demo

You need Go, Docker with Compose, `make`, and `openssl`. From the repository root:

```bash
make demo
```

It starts Postgres and rolloutd, registers 10,000 simulated devices, and runs each one as the real agent on a
flaky network: dropped and lost requests, corrupted and cut-off downloads, crashes, and devices going offline. Then
it rolls out a good release through stages of 1%, 10%, 50%, and 100%, and after that a release that fails on 30% of
devices, with rollback on breach. It took under 4 minutes on the machine in [docs/benchmarks.md](docs/benchmarks.md).
This is its output from one run, trimmed where a stage only counted up:

```text
==> Registering 10000 devices, each running release 1

==> Starting the fleet: real agents, with dropped and lost requests, corrupted downloads, crashes, and outages

==> Rolling out release 2: four stages, 30s soaks, halting above 5% failures once a stage has 90 outcomes
20:06:31  running  stage 1 of 4: 0 ok, 0 failed, 0 in flight, 100 pending  waiting: coverage: 0 of 100 devices have an outcome, need 90
...
20:06:41  soaking  stage 1 of 4: 93 ok, 1 failed, 1 in flight, 6 pending  waiting: soak: 0s of 30s
...
20:07:16  running  stage 2 of 4: 35 ok, 0 failed, 9 in flight, 865 pending  waiting: coverage: 35 of 900 devices have an outcome, need 810
...
20:07:31  soaking  stage 2 of 4: 866 ok, 4 failed, 0 in flight, 30 pending  waiting: soak: 0s of 30s
...
20:08:01  running  stage 3 of 4: 178 ok, 0 failed, 39 in flight, 3822 pending  waiting: coverage: 178 of 4000 devices have an outcome, need 3600
...
20:08:16  soaking  stage 3 of 4: 3830 ok, 28 failed, 2 in flight, 142 pending  waiting: soak: 0s of 30s
...
20:08:46  running  stage 4 of 4: 236 ok, 0 failed, 50 in flight, 4764 pending  waiting: coverage: 236 of 5000 devices have an outcome, need 4500
...
20:09:01  soaking  stage 4 of 4: 4941 ok, 30 failed, 5 in flight, 29 pending  waiting: soak: 1s of 30s
...
20:09:31  completed  stage 4 of 4: 4964 ok, 30 failed, 0 in flight, 3 pending
STAGE  PERCENT  TARGET  NEW   MIN OBS  SOAK  STATE   OK    FAILED  IN FLIGHT  PENDING  UNKNOWN  DEADLINE
1      1%       100     100   90       30s   passed  99    1       0          0        0        2026-11-05 21:06:30-06:00
2      10%      1000    900   90       30s   passed  896   4       0          0        0        2026-11-05 21:07:15-06:00
3      50%      5000    4000  90       30s   passed  3967  28      0          2        0        2026-11-05 21:08:00-06:00
4      100%     10000   5000  90       30s   passed  4964  30      0          3        0        2026-11-05 21:08:45-06:00

==> Rolling out release 3, which fails on 30% of devices, with rollback on breach
20:09:32  running  stage 1 of 4: 0 ok, 0 failed, 0 in flight, 100 pending  waiting: coverage: 0 of 100 devices have an outcome, need 90
20:09:34  running  stage 1 of 4: 8 ok, 2 failed, 1 in flight, 90 pending  waiting: coverage: 10 of 100 devices have an outcome, need 90
20:09:36  rolling_back  stage 1 of 4: 15 ok, 6 failed, 0 in flight, 76 pending  rollback: 0 of 15 restored, 0 failed  waiting: rollback: 0 of 15 devices restored
20:09:42  rolling_back  stage 1 of 4: 15 ok, 6 failed, 0 in flight, 76 pending  rollback: 2 of 15 restored, 0 failed  waiting: rollback: 2 of 15 devices restored
20:09:44  rolling_back  stage 1 of 4: 15 ok, 6 failed, 0 in flight, 76 pending  rollback: 8 of 15 restored, 0 failed  waiting: rollback: 8 of 15 devices restored
20:09:46  rolling_back  stage 1 of 4: 15 ok, 6 failed, 0 in flight, 76 pending  rollback: 13 of 15 restored, 0 failed  waiting: rollback: 13 of 15 devices restored
20:09:48  rolling_back  stage 1 of 4: 15 ok, 6 failed, 0 in flight, 76 pending  rollback: 15 of 15 restored, 0 failed  waiting: rollback: 15 of 15 devices restored
20:09:52  rolled_back  stage 1 of 4: 15 ok, 6 failed, 0 in flight, 76 pending  rollback: 15 of 15 restored, 0 failed
Halted:         certain_breach: stage 1: 6 failed of 20 outcomes, in a cohort of 100, above the 5.00% limit
Failures:       unhealthy 6
Rollback:       attempt 1: 15 of 15 devices restored, 0 failed, 0 unresolved
STAGE  PERCENT  TARGET  NEW   MIN OBS  SOAK  STATE    OK  FAILED  IN FLIGHT  PENDING  UNKNOWN  DEADLINE
1      1%       100     100   90       30s   failed   15  6       0          76       0        2026-11-05 21:09:31-06:00
2      10%      1000    900   90       30s   pending  0   0       0          0        0        -
3      50%      5000    4000  90       30s   pending  0   0       0          0        0        -
4      100%     10000   5000  90       30s   pending  0   0       0          0        0        -

==> Waiting for the fleet to settle, then stopping it
10000 devices after 3m41s: release 1 74, release 2 9926
failures: interrupted 63, unhealthy 6
faults injected: corrupt 336, crash 300, drop 15896, duplicate_report 2650, lost_response 15392, offline_refusal 5266, truncate 312; agent crashes 300
```

The good release passed every stage while the network dropped about 16,000 requests and lost 15,000 responses, and
300 agents crashed. The 63 devices that crashed mid-install reverted and count as failures, at most 1% of any stage.
The bad release failed on 6 of the first 20 devices to report, already more than 5% of the first stage's 100
devices, so the rollout halted without waiting for more. 21 devices of 10,000 had installed it, and the 15 where it
passed its health check went back to release 2. Everything else is on release 2, apart from the 63 interrupted
devices and 11 the first rollout completed without.

`DEVICES=1000 make demo` runs a smaller fleet. It leaves Postgres running; `make dev-reset` stops it and deletes its
data.

## Quick start

You need Go, Docker with Compose, `make`, and `openssl`. Run these from the repository root.

```bash
make dev-up build

# The private key signs releases. The server, and later every device, trusts the public key.
./bin/rolloutctl key generate --private-key release.key --public-key release.pub

export ROLLOUTD_DATABASE_URL='postgres://rollout:rollout_dev@127.0.0.1:15431/rollout?sslmode=disable'
export ROLLOUTD_ADMIN_TOKEN="$(openssl rand -hex 24)"
export ROLLOUT_ADMIN_TOKEN="$ROLLOUTD_ADMIN_TOKEN"
export ROLLOUTD_TRUST_KEYS_FILE=release.pub
export ROLLOUTD_ARTIFACT_DIR=artifacts
# Devices poll every 2 seconds instead of every minute, to keep the demo quick.
export ROLLOUTD_POLL_INTERVAL=2s

./bin/rolloutd migrate
./bin/rolloutd serve 2> rolloutd.log &
sleep 1

./bin/rolloutctl fleet create lab-a
./bin/rolloutctl device register-synthetic --fleet lab-a --count 100 --platform linux/arm64 \
  --tokens-file tokens.txt

# Any file works as an artifact. It's opaque bytes, and nothing unpacks or runs it. This signs a
# manifest of the file on this machine, then registers and uploads it.
head -c 4096 /dev/urandom > demo-app.bin
./bin/rolloutctl artifact push --name demo-app --version 1.4.0 --platform linux/arm64 \
  --release 14 --file demo-app.bin --key release.key -o json | tee artifact.json
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

Now run a real device. Stage 1 exposes one device, so start the agent for that one. It downloads the
artifact, checks it against the signed manifest, asks the server's permission, installs it into its
inactive slot, checks its health, and reports.

```bash
FIRST="$(./bin/rolloutctl rollout cohort "$ROLLOUT_ID" --stage 1 | awk 'NR == 2 {print $3}')"
ROLLOUT_AGENT_TOKEN="$(awk -v id="$FIRST" '$1 == id {print $2}' tokens.txt)" \
  ./bin/rollout-agent --api http://127.0.0.1:18100 --device-id "$FIRST" --platform linux/arm64 \
  --trust-keys release.pub --state-dir "agent-$FIRST" 2> agent.log &
sleep 10  # a poll, a download, and an engine evaluation

cmp demo-app.bin "agent-$FIRST/slots/slot-b" && echo "$FIRST is running the release"
./bin/rolloutctl rollout status "$ROLLOUT_ID" | grep -E '^(State|Waiting)|^STAGE|^1 '
```

```text
sim-00076 is running the release
State:          soaking
Waiting:        soak: 6s of 10m0s
STAGE  PERCENT  TARGET  NEW  MIN OBS  SOAK   STATE    OK  FAILED  IN FLIGHT  PENDING  UNKNOWN  DEADLINE
1      1%       1       1    1        10m0s  soaking  1   0       0          0        0        2026-11-05 15:30:29-06:00
```

The times will differ. The rest of the fleet waits for stage 1's ten-minute soak, so nothing else has been
offered the release.

To see a whole fleet, `rollout-sim` runs hundreds of real agents against a control plane in process, with
faults injected around them, in a scratch database that it drops afterward. In this preset the network is
flaky, some agents crash, and the release fails its health check on 30% of devices:

```bash
export ROLLOUT_SIM_DATABASE_URL='postgres://rollout:rollout_dev@127.0.0.1:15431/postgres?sslmode=disable'
./bin/rollout-sim run --preset bad-release --seed 7 --devices 500
```

```text
deterministic run of preset bad-release (drop rate 0.05, bad release rate 0.3), seed 7, 500 devices
rollout 50c4dfd0-1a00-4fd2-9d1d-5856003ee584 halted after 2m30s (simulated time): stage_breach, stage 1: 2 failed of 3 outcomes, in a cohort of 50, above the 10.00% limit
  stage 1  failed     50 devices: 1 succeeded, 2 failed, 5 superseded, 42 never assigned
  stage 2  pending    75 devices: 0 succeeded, 0 failed, 0 superseded, 0 never assigned
  stage 3  pending   125 devices: 0 succeeded, 0 failed, 0 superseded, 0 never assigned
  stage 4  pending   250 devices: 0 succeeded, 0 failed, 0 superseded, 0 never assigned
updated 1 of 500 devices; failures unhealthy 2; bad installs 0
faults injected: corrupt 5, crash 2, drop 82, lost_response 76, truncate 3; agent crashes 2
downloaded 374453 bytes in all, of a 65536-byte artifact
digest d642b5a4880a151212be036d913ed3250d3502535e77c53913ee7094c7816baf
```

The first stage's failures halt the rollout. The devices that were still downloading are superseded, 492
devices are never offered the release, and no device installs anything that isn't the signed artifact. The
run is deterministic, so the same seed prints the same digest every time. `rollout-sim live --key
release.key` runs the same presets in real time, over HTTP, against the `rolloutd` started above.

A halted rollout can't be resumed. An operator rolls it back, or aborts it, and a fresh rollout is the way forward.
Rolling back tells each device that updated to restore the release it ran before, from its own previous slot, and
`on_breach: rollback` does that as soon as the rollout halts. Here the same bad release has to show a failure rate
over 20 outcomes before a stage counts it, so more devices update first, and all of them are restored:

```bash
./bin/rollout-sim run --preset bad-release --seed 7 --devices 500 --on-breach rollback --min-observations 20
```

```text
deterministic run of preset bad-release (drop rate 0.05, bad release rate 0.3), seed 7, 500 devices
rollout 50c4dfd0-1a00-4fd2-9d1d-5856003ee584 rolled_back after 2m30s (simulated time): stage_breach, stage 1: 4 failed of 22 outcomes, in a cohort of 50, above the 10.00% limit
  stage 1  failed     50 devices: 18 succeeded, 4 failed, 6 superseded, 22 never assigned
  stage 2  pending    75 devices: 0 succeeded, 0 failed, 0 superseded, 0 never assigned
  stage 3  pending   125 devices: 0 succeeded, 0 failed, 0 superseded, 0 never assigned
  stage 4  pending   250 devices: 0 succeeded, 0 failed, 0 superseded, 0 never assigned
rolled back: 18 of 18 updated devices restored, 0 unresolved
updated 0 of 500 devices; failures unhealthy 4; bad installs 0
faults injected: corrupt 10, crash 4, drop 116, duplicate_report 9, lost_response 110, truncate 16; agent crashes 4
downloaded 1665319 bytes in all, of a 65536-byte artifact
digest eca9aa69763372554855ebe3a7f0468340ac46d0f86e939bf2b258acabe0e149
```

To clean up:

```bash
kill %1 %2             # stop rolloutd and the agent
make dev-reset         # stop Postgres and delete this project's data
rm -rf agent-* agent.log artifact.json artifacts demo-app.bin release.key release.pub rolloutd.log tokens.txt
```

## Kubernetes

The chart in `deploy/helm/fleet-rollout` installs rolloutd, the operator, and its CRDs, plus a single Postgres for
trying it out. With the operator running, a rollout is a resource:

```yaml
apiVersion: rollouts.bispulm.github.io/v1alpha1
kind: Rollout
metadata:
  name: demo-app-1.4.0
spec:
  fleetRef: lab-a
  artifact: {name: demo-app, version: 1.4.0, platform: linux/arm64}
  stages:
    - {percent: 1, soak: 10m}
    - {percent: 10, soak: 30m}
    - {percent: 50, soak: 30m}
    - {percent: 100}
  safety:
    maxFailureRate: "2%"
```

Applying it creates and starts the rollout, and `kubectl get rollouts` follows its phase and stage. Setting
`spec.paused` pauses or resumes it, and deleting the resource aborts a rollout that hasn't finished. The rest of the
spec can't change, because the control plane freezes a rollout's policy when it starts. A `Fleet` resource creates
a fleet. Devices are still registered with `rolloutctl`, because their tokens are shown only once. The operator only
calls the API, so the CLI and the resources can't disagree about how a rollout behaves
([ADR 0010](docs/adr/0010-kubernetes-operator.md)).

`make test-kind` runs it all on a local kind cluster named `fleet-rollout-dev`, with a kubeconfig of its own, so your
current kube context is left alone. It builds the image, installs the chart, creates a fleet from a `Fleet`
resource, and registers 40 simulated devices. Then it rolls out a good release over a flaky simulated network, and a
release that fails on every device:

```text
==> Rolling out a good release from a Rollout resource, over a flaky network
rollout.rollouts.bispulm.github.io/demo-app-1.0.0 created
rollout.rollouts.bispulm.github.io/demo-app-1.0.0 condition met
NAME             FLEET     PHASE       STAGE   AGE
demo-app-1.0.0   lab-k8s   completed   3       40s
40 devices after 40s: release 1 40
failures: none
faults injected: crash 1, drop 55, duplicate_report 10, lost_response 51, offline_refusal 9; agent crashes 1

==> Rolling out a release that fails on every device, which has to halt
rollout.rollouts.bispulm.github.io/demo-app-2.0.0 created
rollout.rollouts.bispulm.github.io/demo-app-2.0.0 condition met
stage_breach: stage 1: 8 failed of 8 outcomes, in a cohort of 10, above the 20.00% limit

==> Deleting the halted resource aborts its rollout
rollout.rollouts.bispulm.github.io "demo-app-2.0.0" deleted

==> Passed
```

The simulated devices run in real time, so the timings and fault counts differ from run to run.

It deletes the cluster afterward, unless you set `KEEP_CLUSTER=1`. To install the chart on a cluster of your own,
make the image available to it, by loading it or pushing it to a registry and setting `image.repository`, then:

```bash
helm install fleet-rollout deploy/helm/fleet-rollout --namespace fleet-rollout --create-namespace \
  --set adminToken.value="$ROLLOUT_ADMIN_TOKEN" --set-file trustKeys=release.pub \
  --set postgres.password="$(openssl rand -hex 16)"
```

## Observability

rolloutd serves Prometheus metrics on an address of its own, `127.0.0.1:18102` by default, and exports traces over
OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set ([ADR 0012](docs/adr/0012-observability.md)). `make obs-up` runs
rolloutd in Compose with Prometheus, Grafana, and Jaeger alongside, all on loopback. Stop the quick start's rolloutd
first, since this one uses the same port:

```bash
./bin/rolloutctl key generate --private-key release.key --public-key release.pub
export ROLLOUT_ADMIN_TOKEN="$(openssl rand -hex 24)"
make obs-up

./bin/rollout-sim live --preset bad-release --devices 100 --key release.key --on-breach rollback \
  --min-observations 10 --quiet
```

The rollout halts and rolls back within seconds. The dashboard at http://127.0.0.1:18103 then shows the halt, its
reason, each stage's outcomes, and the rollback restoring every device that updated. Prometheus is at
http://127.0.0.1:18104, and Jaeger, with a trace for each request and engine decision, at http://127.0.0.1:18105.
[docs/runbooks.md](docs/runbooks.md) says what to do about each thing the dashboard can show. `make dev-reset` stops
it all and deletes its data.

## What works today

| Area | Behavior |
|---|---|
| Fleets and devices | Registration is all or nothing. Device tokens are shown once and stored as hashes. |
| Artifacts | A signed manifest covers the artifact's metadata and every chunk's digest. Unsigned or untrusted artifacts are refused, and bytes that don't match the manifest are never stored. Devices fetch with Range requests, and only while they hold an assignment. |
| Rollout policy | Validated when created and again when started. A rollout that could never satisfy its own policy is rejected. Defaults are copied into the rollout, so later changes can't affect it. |
| Cohorts | Devices rank by a hash of the rollout ID and device ID, so stages are nested and reproducible. |
| Frozen data | Membership and policy freeze at start. Database triggers enforce this, not just application code. |
| Transitions | Start, pause, resume, and abort each run in one transaction with a row lock and an audit entry. Concurrent attempts have one winner. |
| Assignments | Devices poll for work. A device only gets work once its stage is exposed, and a poll can't slip past a pause or halt that's committing. |
| Reports | Bound to the authenticated device, rollout, generation, and sequence. Repeats change nothing, and delayed reports can't undo newer ones. A device's request to start installing is decided against the rollout, so nothing starts once a halt or pause has committed. |
| Stage progression | Each stage is judged by its own cohort, with exact threshold arithmetic. Coverage, soak, and a separate limit on unknown devices all have to pass. Missing reports never count as success. |
| Halting | Stops new exposure and cancels work that hasn't reached installation, in one transaction. The status shows why, and what's still in flight. |
| Rollback | Each device that updated restores the release it ran before, from its previous slot, under a rollback assignment it asks permission to act on. By command or on breach. A rollback that misses its deadline is marked incomplete, and can be tried again. |
| Engine | Any number of instances can run. A fenced lease in Postgres means one decides at a time, and a stale leader can't commit anything. All state survives a restart. |
| Device agent | Polls with jitter, and downloads in verified chunks that survive a crash or a killed process. Checks the signature, platform, size, and release counter. Installs into the inactive slot only once the server agrees, and reverts if the health check fails or the install was interrupted. |
| Kubernetes | `Fleet` and `Rollout` resources, an operator that turns them into API calls, and a Helm chart. Tested end to end on kind. |
| Simulator | Runs fleets of the real agent with dropped and lost requests, corrupted and truncated downloads, crashes, offline devices, and reconnect storms. A seed reproduces a run exactly. |
| Observability | Prometheus metrics for requests, shedding, reports, engine decisions, halts, and each rollout's progress. OpenTelemetry traces. A Grafana dashboard, and runbooks. |
| Operations | Health and readiness endpoints, drain on shutdown, structured logs, and separate admin and device credentials. The server binds to loopback unless told otherwise. |

[docs/rollout-semantics.md](docs/rollout-semantics.md) is the behavior contract, including what is
specified but not built yet. [docs/architecture.md](docs/architecture.md) shows how the pieces fit
together, and [docs/adr](docs/adr) records the decisions.

## Benchmarks

Measured on one Apple M4 Pro laptop, with Postgres in a 4-CPU VM, and the commands to reproduce them are in
[docs/benchmarks.md](docs/benchmarks.md):

| Measure | Result |
|---|---|
| Fleet size | With default settings, 30,000 devices polling every minute take a staged rollout with nothing shed, and every device request served within 7 ms at p99. The limit is the burst of work when a stage opens. Past about 4,000 requests a second, rolloutd sheds the excess, and rollouts still finish. |
| Staging | Across 10,000 devices, a release that fails everywhere reaches 7 devices with staged exposure, and 789 (7.9%) without it. |
| Halting | From the report that crosses a limit to the halt: 2.4 s at p50 and 4.8 s at most, with the default 5-second engine interval. With 1 second: 0.55 s and 1.0 s. |
| Resumable downloads | With 20% of responses cut off partway, devices download 1.12 times the artifact by resuming, and 8.5 times by starting over. |

## Limitations

- Every device is simulated. The agent writes artifacts into slots as opaque bytes, and nothing unpacks or runs
  them.
- Rollback restores only the release a device ran just before, from its own previous slot. It can't fetch an
  older release.
- One shared admin token, so every operator action is recorded as `admin`.
- The filesystem artifact store isn't shared between rolloutd replicas, so more than one replica needs shared
  storage behind it.
- There's no endpoint that lists rollouts or fleets. A rollout is looked up by its ID.
- A breach waits for the engine's next evaluation, every 5 seconds by default, so that bounds how fast a rollout
  halts.
- rolloutd doesn't terminate TLS, and binds to loopback unless told otherwise.

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
