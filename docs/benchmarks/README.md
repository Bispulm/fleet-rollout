# Raw benchmark results

The JSON each run of `go run ./test/bench` wrote, for the results in [../benchmarks.md](../benchmarks.md). Each file
records the machine and the load average before and after. Durations are in nanoseconds. Runs of several fleet sizes
record the load only before the first size and after the last.

| File | Command |
|---|---|
| `resume.json` | `bench resume` |
| `staging-1000.json` | `bench staging` |
| `staging-10000.json` | `bench staging --devices 10000` |
| `halt-engine-5s.json` | `bench halt` |
| `halt-engine-1s.json` | `bench halt --engine-interval 1s` |
| `scale-poll-10s.json` | `bench scale --devices 1000,5000,10000,20000,40000,60000,80000`, which stopped after 10,000, the first size that shed |
| `scale-poll-10s-pool-64-10000.json` | `bench scale --devices 10000 --rolloutd-env ROLLOUTD_DB_MAX_CONNS=64` |
| `scale-poll-10s-pool-64-20000.json` | `bench scale --devices 20000,40000,60000,80000 --rolloutd-env ROLLOUTD_DB_MAX_CONNS=64`, which stopped after 20,000 |
| `scale-poll-60s.json` | `bench scale --devices 30000,60000 --poll-interval 60s` |

Every scale run used the defaults the scale files don't record: 1,024 shared connections, first requests spread
over one poll interval, and 30-second soaks. Later versions of the harness write these settings into the file.
