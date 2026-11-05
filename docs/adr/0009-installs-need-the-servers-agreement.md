# ADR 0009: A device starts installing only with the server's agreement

Status: Accepted. Supersedes the validity lease in ADR 0006.

## Context

ADR 0006 let a device start installing without contact, as long as its last poll's lease hadn't expired. The idea
was to tolerate a flaky network while bounding how long a halted rollout could keep gaining installs.

The simulator showed what that costs. A device polls, hears proceed, and starts installing. Before its report of
installing arrives, the rollout halts or completes, and the server supersedes the assignment, because as far as it
knows the device hasn't started. The late report is ignored. The server now believes a device is untouched when it
has installed the release. Its installed release is wrong, and after a halt, a device can install the bad release,
fail, revert, and have that failure ignored too. A live run of 40 devices ended with 40 on the new release and 39
counted. The lease bounds how long this can go on. It doesn't stop it, and it can't make the server's record right.

## Decision

**The report of installing is a request to start, and the device waits for the answer.** The server decides it in
the same transaction that records it, against the rollout as it stands:

- If the assignment is still open and may proceed, the report is applied and the device is in flight.
- If a halt, abort, or completion has superseded it, the report is ignored as finished, and the device abandons
  the assignment without installing.
- If the rollout is paused, the report is ignored as `on_hold`, the assignment stays open, and the device waits
  to be released before asking again with a new report.

Superseding updates the assignment, so its row lock orders it against the request. A pause doesn't touch
assignments, so the request also takes a shared lock on the rollout first. Only this one report does, because it's
the only one a pause has to be ordered against, and reports from devices already in flight never wait on the
engine.

The device saves the request before sending it, so a retry is identical. A duplicate report now says what became of
the original, so a device whose answer was lost learns it from the retry. Earlier reports go first, so the server
hears them in order.

**The validity lease goes.** With the server deciding every start, the lease no longer grants anything, so it's
removed from assignments, the API, and configuration rather than left as a setting that looks meaningful.

Alternatives considered:

- **Keep the lease, and let the server wait for it to expire before superseding.** That makes the record right
  eventually, but completion and halts would leave assignments open for a lease length, a halt still couldn't stop
  a device that already heard proceed, and every reader of status would have to reason about leases.
- **Accept late reports on superseded assignments.** Changing a terminal state after the fact makes "superseded"
  mean "probably superseded", and a halt still couldn't stop the install.

## Consequences

- A halted, aborted, or completed rollout gains no new installs from the moment that transaction commits, however
  long a device has been out of contact, and a paused one gains none until it resumes.
- The server's record of who is in flight is exact. A device running the new release always has an accepted
  report of installing behind it.
- A device needs one round trip at the moment of installing. A device that can't reach the server waits, with a
  fully verified download, until it can. The long part, the download, still tolerates disconnects.
- A device that's accepted and then never heard from again stays in flight until it turns unknown, as before.
