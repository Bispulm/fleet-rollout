# Benchmarks

Every number here was measured, with the command that produced it, on this machine:

- Apple M4 Pro, 14 cores, 24 GB, macOS 26.6, Go 1.27.3
- Postgres 18 in Docker, in a Colima VM with 4 vCPUs and 8 GB
- rolloutd, the simulated devices, and the benchmark itself as local processes on the same machine, so they
  compete with each other and with Postgres for CPU

Each benchmark records the load average before and after it ran. Nothing else heavy was running: the load
average stayed between 1.5 and 4.5 on 14 cores. The full results, with the machine and every per-run number,
are in [benchmarks/](benchmarks/).

`go run ./test/bench <name>` runs each one, against a Postgres server on which it creates and drops scratch
databases. The `make bench-*` targets use the Compose Postgres from `make dev-up`. The resumable download and
staging benchmarks run in the deterministic simulator, so the same seed gives the same numbers on any machine. The
other two run in real time, against a real rolloutd, so they depend on the machine.

## Bytes saved by resumable downloads

```bash
make bench-resume
```

100 devices each download a 1 MiB artifact in 64 KiB chunks, one request per chunk, over a network that cuts a
share of responses off at a random point. Resuming, an agent keeps every verified chunk and fetches the rest.
Starting over, as a client without resume would, it throws away everything after any failed chunk. Both are the
same agent: the simulator's `RestartDownloads` option discards the partial download and restarts the agent, which
finds less than it had verified and starts again from the first chunk.

The fault model is per response: each chunk request has the same chance of being cut off, wherever it is in the
artifact. That holds the number of requests fixed between the two arms. A client without resume that fetched the
whole artifact in one request would face breaks per byte instead, which this doesn't model.

| Responses broken | Breaks, resuming | Downloaded, resuming | Downloaded, starting over | Saved by resuming |
|---|---|---|---|---|
| 0% | 0 | 1.00× | 1.00× | 0% |
| 1% | 17 | 1.01× | 1.08× | 7.2% |
| 5% | 79 | 1.02× | 1.42× | 28.0% |
| 10% | 159 | 1.05× | 2.15× | 51.3% |
| 20% | 392 | 1.12× | 8.51× | 86.9% |

"Downloaded" is bytes received per device, as a multiple of the artifact's size. Resuming, a break costs half a
chunk on average, so the overhead grows with the break rate. Starting over, a 16-chunk download only finishes when
16 requests in a row succeed, so the cost grows geometrically: at 20%, that's about 1 attempt in 36. At high break
rates the starting-over column varies a lot from seed to seed, because the number of restarts does.

## Devices a bad release reaches, with and without staging

```bash
make bench-staging                        # 1,000 devices
make bench-staging ARGS='--devices 10000'
```

The same fleet, seed, and safety limits, with stages of 1%, 10%, 50%, and 100%, or every device in one stage.
Each rollout halts above a 5% failure rate once a stage has 10 outcomes, or as soon as more than 5% of a stage's
whole cohort has failed. Devices poll every minute and the engine evaluates every 5 seconds, rolloutd's defaults. The network is
clean, so the release is the only thing that fails.

| Fleet | Release fails on | Stages | Devices that installed it | Share of fleet | Devices it failed on |
|---|---|---|---|---|---|
| 1,000 | every device | 1%, 10%, 50%, 100% | 2 | 0.2% | 2 |
| 1,000 | every device | one stage | 81 | 8.1% | 81 |
| 1,000 | 30% of devices | 1%, 10%, 50%, 100% | 2 | 0.2% | 1 |
| 1,000 | 30% of devices | one stage | 81 | 8.1% | 24 |
| 10,000 | every device | 1%, 10%, 50%, 100% | 7 | 0.1% | 7 |
| 10,000 | every device | one stage | 789 | 7.9% | 789 |
| 10,000 | 30% of devices | 1%, 10%, 50%, 100% | 14 | 0.1% | 4 |
| 10,000 | 30% of devices | one stage | 789 | 7.9% | 223 |

Every rollout halted. With staging, the release reaches a handful of devices in the first stage. Without it, the
release still stops at about 8% of the fleet, not 100%, because a device installs only once rolloutd agrees to its
request, and none are agreed to after the halt ([ADR 0009](adr/0009-installs-need-the-servers-agreement.md)). Those
8% are the devices whose requests arrived before the engine's next evaluation: about one engine interval's share of
a poll interval, 5s of 60s. Without staging, the number of devices it reaches grows with the fleet, 81 and then 789.
With staging, it's bounded by the first stage's size.

## Time from breach to halt

```bash
make bench-halt
make bench-halt ARGS='--engine-interval 1s'
```

20 rollouts, each to 200 devices polling every 2 seconds, of a release that fails on every device. The first stage
has 20 devices. It halts above 10% failures once it has 5 outcomes, or as soon as more than 10% of its 20 devices
have failed, which is usually first, at the third failure. The time runs from the moment rolloutd applied the
report that crossed the limit to the moment the engine halted the rollout, both read from Postgres afterward. The
benchmark finds that report by replaying the outcomes rolloutd applied, in order, through the same policy
evaluation the engine runs. The halt's time is the one the engine records, taken as its evaluation starts, so the
few milliseconds the evaluation and its commit take aren't included. Each trial waits a random part of an engine
interval before it starts, so that breaches land at random points between evaluations.

| Engine interval | Min | p50 | p95 | Max | Devices that installed it, p50 |
|---|---|---|---|---|---|
| 5s, the default | 0.06s | 2.43s | 4.65s | 4.77s | 20 |
| 1s | 0.02s | 0.55s | 0.85s | 1.00s | 14 |

The engine evaluates on a fixed interval, so a breach waits for the next evaluation: on average half an interval,
and at most one. The devices that installed the release are the
whole first stage, or most of it, because every one of them asked to install within a second or two of the others.
A shorter interval halts sooner, for the cost of evaluating every active rollout more often.

## Scale

```bash
make bench-scale                                              # 1,000 to 20,000 devices, polling every 10s
make bench-scale ARGS='--rolloutd-env ROLLOUTD_DB_MAX_CONNS=64'
make bench-scale ARGS='--devices 30000,60000 --poll-interval 60s'
```

Each run is a fresh database and a fresh rolloutd, and a fleet of live simulated devices, each one the real agent,
over real HTTP. The fleet takes a good release through four stages, 10%, 25%, 50%, and 100%, with 30-second soaks.
Devices share 1,024 connections, and their first requests are spread over one poll interval, as in a fleet that has
been running for a while. rolloutd runs with its defaults unless the table says otherwise. Latency is the time
rolloutd spent on a request, from its request log: from the request arriving at the handler to the response being
written. Time waiting to be accepted isn't in it. Postgres CPU comes from `docker stats` once a second, and commits
from Postgres's own counters for the run's database. The counts include setting the fleet up, which is a few dozen
transactions.

| Devices | Poll interval | DB pool | Device requests/s | Poll p50 / p95 / p99 (ms) | Report p50 / p95 / p99 (ms) | Shed | rolloutd CPU (cores) | Postgres CPU mean / peak (cores) | Commits/s | Rollout |
|---|---|---|---|---|---|---|---|---|---|---|
| 1,000 | 10s | 16 | 149 | 4.1 / 7.9 / 10.2 | 2.5 / 3.7 / 4.7 | 0 | 0.08 | 0.11 / 0.21 | 316 | completed in 2m45s |
| 5,000 | 10s | 16 | 729 | 2.1 / 3.5 / 6.9 | 3.2 / 6.3 / 14.1 | 0 | 0.23 | 0.27 / 1.09 | 1,543 | completed in 2m55s |
| 10,000 | 10s | 16 | 1,485 | 2.2 / 67.5 / 78.7 | 5.4 / 81.3 / 104.5 | 1.3% | 0.45 | 0.58 / 1.80 | 3,103 | completed in 2m50s |
| 10,000 | 10s | 64 | 1,444 | 2.6 / 6.7 / 41.7 | 5.8 / 49.4 / 58.6 | 0 | 0.44 | 0.56 / 2.22 | 3,048 | completed in 3m0s |
| 20,000 | 10s | 64 | 2,913 | 3.3 / 58.3 / 68.4 | 52.5 / 72.7 / 80.9 | 6.1% | 0.88 | 1.10 / 2.27 | 5,759 | completed in 3m24s |
| 30,000 | 60s | 16 | 1,151 | 2.2 / 3.9 / 5.1 | 3.1 / 4.9 / 6.2 | 0 | 0.36 | 0.46 / 1.36 | 2,548 | completed in 6m9s |
| 60,000 | 60s | 16 | 2,318 | 2.6 / 76.3 / 93.6 | 6.1 / 87.0 / 104.7 | 11.7% | 0.68 | 0.89 / 1.89 | 4,493 | completed in 7m33s |

"Shed" is the share of device requests answered `503`. Each run started with the 1-minute load average under 4.
The 5-minute average was higher, up to 6, from the run before it finishing.

**With rolloutd's defaults, 30,000 devices polling every minute took a staged rollout with nothing shed, and every
device request served within 7 ms at p99.** At 60,000, rolloutd shed 11.7% of requests while the last stage opened,
and the rollout finished anyway.

### What limits it

Not steady polling: outside stage openings, every run served device requests in about 2.5 ms at p50 and under 8 ms
at p99, even at 60,000 devices. The limit is what happens when a stage opens. Every newly exposed device learns of the release at its next poll, and then fetches
the manifest and the chunks and reports each step, about eight requests, all within one poll interval. Opening the
last stage of a 10,000-device fleet polling every 10 seconds adds 5,000 devices' updates to one 10-second window.

Overloaded that way, rolloutd served about 3,900 device requests a second, at most 4,300, with its default pool of 16
database connections, and about 5,000, at most 5,900, with 64. Every route slowed together, even downloads, which
barely touch Postgres, while Postgres itself stayed under 2.3 of its 4 cores: requests were waiting for a database
connection. Past that point rolloutd sheds the excess with `503` and `Retry-After`, as designed, and devices come
back later. No rollout failed or stalled because of it. The 20,000-device run took 3m24s instead of 3m.

A rough rule follows: a stage that newly exposes N devices adds about 8N requests over one poll interval, on top of
the fleet's polls. At a 60-second poll, the 15,000 devices of the 60,000-device fleet's third stage added about
2,000 a second, and nothing slowed. The 30,000 of its last stage added about 4,000, which, with 1,000 polls a second,
went past the ceiling.

So the fleet a single rolloutd can take depends on how many devices a stage exposes per poll interval. Polling less
often, as rolloutd does by default, spreads each stage's work over a longer window. Raising `ROLLOUTD_DB_MAX_CONNS`
raises the ceiling, at the cost of Postgres connections: the default of 16 lets several replicas share the 100
connections Postgres allows by default.

### Caveats

- Everything shares one machine. At 20,000 devices, rolloutd used about 0.9 cores, Postgres up to 2.3 of its 4,
  and the simulator 0.4.
- Colima reaches Postgres through a forwarded port. A round trip took about 0.25 ms and a durable commit about
  0.55 ms, measured separately, which is slower than a Postgres on the same host.
- The release is 8 KiB, so downloads are cheap. Larger artifacts would make the download pool, which is separate
  and limited to 128, matter more.

### How this benchmark changed

The first two versions measured the wrong thing, and are kept here so the numbers above can be trusted:

1. Every simulated device shared 64 connections. That capped a 40,000-device fleet at about 4,000 requests a second
   whatever rolloutd did, and the client's queue smoothed out every burst, so every size passed.
2. With more connections, every device made its first request within the same second and kept polling in step, so
   the fleet arrived in bursts of thousands. That's a fleet reconnecting all at once, which shedding is for, not a
   fleet's steady state.
