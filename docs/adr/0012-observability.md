# ADR 0012: Metrics from Prometheus, traces from OpenTelemetry, both optional

Status: Accepted

## Context

An operator needs to see a rollout halt as it happens, and to tell a bad release from a sick control plane: requests
being shed, the engine without a leader, devices that stopped reporting. Logs have every detail but answer none of
those questions at a glance.

## Decision

**rolloutd serves Prometheus metrics on a listener of its own,** `ROLLOUTD_METRICS_ADDR`, `127.0.0.1:18102` by
default. Metrics stay off the API's port, so exposing the API doesn't expose them, and the same loopback rule applies.
An empty address turns them off.

**Events are counted as they happen:** HTTP requests by route pattern, method, and status, with a latency histogram;
device requests shed, by pool; reports, by state and what became of them; engine decisions, by event; halts, by
reason; and whether this instance leads the engine. Labels come from fixed sets, such as route patterns rather than
paths, so a fleet of devices can't create a series each.

**Rollout state is read from Postgres when Prometheus scrapes,** for every rollout that hasn't finished, and for an
hour after one does: its state, current stage, each stage's outcomes, and a rollback's progress. Postgres is the only
authority (ADR 0001), so this can't disagree with the API, and an instance that doesn't lead the engine still reports
it. Each scrape runs one read per listed rollout, which is fine for the handful of rollouts a control plane runs at
once.

**Traces are OpenTelemetry, and off unless configured.** With `OTEL_EXPORTER_OTLP_ENDPOINT` set, rolloutd exports a span
for each HTTP request and each engine decision over OTLP, using the standard environment variables. Without it, the
tracer is a no-op, and nothing is exported.

**A Compose profile runs Prometheus, Grafana, and Jaeger locally,** with a provisioned dashboard: rollout states,
stage outcomes, halts, rollback progress, shed requests, and request latency.

Alternatives considered:

- **Gauges kept up to date by the engine.** Only the leader would have them, and they'd go stale when it changed.
- **The default Prometheus registry.** Tests would share state, and every package could add metrics unseen.
- **Logs only, with metrics derived from them.** It needs a log pipeline before anything can be graphed.

## Consequences

- `prometheus/client_golang` and the OpenTelemetry SDK become direct dependencies of rolloutd.
- The rollout metrics cost a few queries per scrape, and need measuring with many rollouts in flight.
- Device IDs never appear as labels, so finding one device's trouble still means logs.
