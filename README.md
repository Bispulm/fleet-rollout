# fleet-rollout

A control plane that stages signed software updates across unreliable simulated
devices, watches the outcomes, stops unsafe progression, and restores a previously
healthy version when rollback is authorized.

This project is in early development. Nothing here is usable yet.

## Development

You need Go, Docker with Compose, and `make`. Run `make help` to list the targets.

```bash
make dev-up            # Postgres on 127.0.0.1:15431
make test-integration  # all tests, with the race detector, on real Postgres
make check             # lint, tests, vulnerability check, and secret scan
```

To run the server, set a database URL and an admin token, apply the migrations, and start it.
It listens on `127.0.0.1:18100` by default.

```bash
export ROLLOUTD_DATABASE_URL='postgres://rollout:rollout_dev@127.0.0.1:15431/rollout?sslmode=disable'
export ROLLOUTD_ADMIN_TOKEN="$(openssl rand -hex 24)"
go run ./cmd/rolloutd migrate
go run ./cmd/rolloutd serve
curl -i http://127.0.0.1:18100/readyz
```

Design decisions are recorded in [docs/adr](docs/adr).
