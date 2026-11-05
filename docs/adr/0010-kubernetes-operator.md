# ADR 0010: A Kubernetes operator that's a client of the API

Status: Accepted

## Context

Teams that run on Kubernetes want to declare a rollout as a resource, apply it with the rest of their configuration,
and watch it with `kubectl`. The control plane already has a complete API, and Postgres is its only authority
(ADR 0001). A second place that decided how rollouts behave could drift from the first.

## Decision

**The operator is an API client, like `rolloutctl`.** It turns `Fleet` and `Rollout` resources into API calls, and
copies what the control plane reports into their status. It holds no rollout rules: validation, cohorts, stage
progression, and halting all stay in the control plane, so the CLI, the operator, and the API can't disagree.

**The resources are cluster-scoped,** in the group `rollouts.bispulm.github.io`, version `v1alpha1`. Fleet names are
global in the control plane, and rollout names are unique per fleet, so namespaced resources could collide there
without Kubernetes noticing. The group is under a domain the project's GitHub account controls, because a CRD group
should be a DNS name its publisher owns.

**A `Rollout` resource's UID is its rollout's ID.** Creating a rollout with a chosen ID is idempotent in the API, so a
retry after a lost response, or a restart of the operator, finds the same rollout instead of making another. The
rollout's name in the control plane is the resource's name, made valid and suffixed with part of the UID, because a
resource deleted and created again keeps its name.

**A `Rollout`'s spec is fixed once it exists, except `paused`.** The control plane freezes a rollout's policy when it
starts, so the resource does the same, with validation rules the API server enforces. A rollout created paused stays
a draft, exposing nobody, until `paused` clears. After that, the operator pauses or resumes only when the resource's
spec changes, conditional on the revision it last saw. A pause made some other way, through `rolloutctl` during an
incident, say, stands until someone changes the resource, rather than being undone on the next pass. Deleting the
resource aborts a rollout that hasn't finished, through a finalizer, and then lets the resource go. Deleting a
`Fleet` stops managing it and leaves the fleet in place, because devices are registered to it.

**Problems a person has to fix are conditions, not errors.** A missing artifact, a missing fleet, a fleet busy with
another rollout, or a policy the fleet can't satisfy sets `Ready` to false with a reason, and is checked again
later. An unreachable control plane is retried with backoff. While a rollout is in progress, the operator checks it
every 10 seconds, because the control plane moves it along without telling anyone.

**Devices are still registered with `rolloutctl`.** A device's token is a secret shown once, and a resource's status
is no place for it.

The operator uses `controller-runtime` v0.25 with the Kubernetes v0.37 client libraries, as ADR 0005 anticipated.
They're imported only by `api/v1alpha1`, `internal/operator`, and `cmd/rollout-operator`, so they're linked only into
the operator. The CRD types depend on nothing but `k8s.io/apimachinery`.

Alternatives considered:

- **The operator as the engine, with resources as the source of truth.** It would make Kubernetes required, and
  give rollouts two homes with different consistency guarantees.
- **Namespaced resources, with the namespace in the control plane's names.** Possible, but it hides the fact that
  fleets are shared, and makes `rolloutctl` and the operator name things differently.
- **Generating the operator with kubebuilder's scaffolding.** It brings a project layout, Kustomize, and a Makefile
  this repository doesn't otherwise use. Only `controller-gen` is used, pinned and run with `go run`.

## Consequences

- A `Rollout` shows the control plane's view with a delay of up to 10 seconds.
- A `Rollout`'s phase can disagree with its `paused` setting, when someone paused or resumed the rollout another way.
  That's deliberate: the status shows what's true, and changing the resource is how to take back control.
- The operator needs the admin token. One shared token means its actions are attributed to `admin`, as the CLI's
  are, until per-client credentials exist.
- A policy that the fleet can't satisfy leaves the resource not ready for good. Fixing it means deleting the resource
  and applying a corrected one, since the spec can't change.
