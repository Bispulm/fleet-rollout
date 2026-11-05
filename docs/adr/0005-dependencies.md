# ADR 0005: Standard library first, with few dependencies

Status: Accepted

## Context

Each dependency adds supply-chain risk and upgrade work. This is one Go module, and the standard
library's HTTP server and routing now cover what the API needs.

## Decision

Use the standard library unless a dependency clearly earns its place.

| Need | Choice | Why not the standard library or an alternative |
|---|---|---|
| Postgres driver | `jackc/pgx` | `database/sql` has no driver of its own. Direct pgx gives `COPY` for writing membership and typed errors with SQLSTATE codes. |
| Migrations | `pressly/goose` | Embedded migrations with a session lock are small to use but fiddly to get right. Library use needs no extra binary. |
| CLI | `spf13/cobra` | `rolloutctl` has nested commands with generated help. It's linked only into the CLI. |
| HTTP routing | `net/http` | `ServeMux` handles methods and path values. |
| Logging | `log/slog` | Structured logging is in the standard library. |
| Integration tests | Plain `pgx` against a server from Compose or CI | `testcontainers-go` would add a large dependency tree to save a few lines, and the local container runtime needs extra configuration for it. |

Each test creates its own database through `internal/testdb`, so tests can run in parallel without
sharing state.

## Consequences

- The Kubernetes operator will bring in `controller-runtime` and its large dependency tree. That's
  unavoidable for an operator, but it's a reason to keep operator code to its own package and binary.
- Tests need a Postgres server to be running. `ROLLOUT_TEST_REQUIRE_DB=1` makes CI fail rather than
  skip if it's missing.
