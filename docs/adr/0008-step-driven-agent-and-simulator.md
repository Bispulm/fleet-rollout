# ADR 0008: A step-driven agent, and a simulator that runs the real one

Status: Accepted

## Context

A simulator that reimplements device behavior only proves that the simulation works. The simulator has to run the
agent itself, replacing only what's outside it: the network, installation, durable storage, and time. It also has to
be reproducible, since an unreproducible failure scenario isn't much use for testing a safety system.

## Decision

**The agent is a state machine advanced by `Step`.** A step does a bounded amount of work and returns how long to wait
before the next one. Backoff and polling cadence are return values, not sleeps. The standalone agent is a small loop
around `Step`. Nothing else differs between it and the simulator.

**State is written before it's acted on.** Each phase change is saved atomically, along with the reports describing it,
before the action it describes. After a crash, the agent resumes from its last checkpoint, and a report is sent again
with the same sequence number. Reports go through a durable, ordered outbox, so the server sees them in order and
`succeeded` always follows `health_check`.

**A crash during installation reverts.** If the agent restarts and finds an install or health check that it can't
prove finished, it treats the new slot as unusable, keeps the previous one, and reports a failure. The active slot
changes only in the same atomic save that records success.

**Downloads resume only from verified data.** Each chunk is verified before it counts. On restart, anything past the
last verified chunk is discarded. Every Range response has to prove it's the right artifact and offset, and otherwise
the agent restarts safely rather than appending bytes it can't trust.

**The simulator has two modes.**

- *Deterministic:* one goroutine advances every agent and the engine on a fake clock, over HTTP handlers called in
  process, against real Postgres. There's no scheduling to vary, so a seed reproduces the entire run, outcome for outcome.
- *Live:* one goroutine per device, in real time, over real HTTP against a running `rolloutd`. This exercises real
  concurrency, so a seed reproduces each device's fault decisions, but not how they interleave across devices. For
  example, which devices finish before a halt can differ between runs.

**Fault decisions are pure functions.** A decision is a hash of the seed, the device, the kind of operation, and which
occurrence of that operation it is. It doesn't depend on a shared random stream, so one device's behavior can't shift
another's faults.

Alternatives considered:

- **A goroutine and sleeps inside the agent.** It's simpler to write, but makes time untestable without real waiting,
  and rules out the deterministic mode.
- **A separate model of a device in the simulator.** It's faster at scale, but would prove nothing about the real agent.
- **A journal instead of whole-state checkpoints.** More efficient for large state, but a device's state is small, and
  an atomic replace is much easier to get right.

## Consequences

- Deterministic mode is sequential, so it isn't a concurrency test or a throughput measurement. Live mode is both.
- Each step does at most one chunk, so large chunks mean longer steps, and a long step delays a poll.
- A checkpoint is saved for each verified chunk, which costs an fsync per chunk on a real device.
- An agent that loses its state entirely has also lost its report sequence. Its next reports can conflict with what
  the server already has, and they're dropped until its sequence passes them. Recovering properly needs re-enrollment,
  which doesn't exist yet.
