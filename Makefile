.DEFAULT_GOAL := help

DEV_DATABASE_URL ?= postgres://rollout:rollout_dev@127.0.0.1:15431/rollout?sslmode=disable
TEST_DATABASE_URL ?= postgres://rollout:rollout_dev@127.0.0.1:15431/postgres?sslmode=disable
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-z0-9-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-18s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build all binaries into ./bin
	go build -o bin/ ./cmd/...

.PHONY: generate
generate: ## Generate deep copy code and CRDs from api/
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=deploy/helm/fleet-rollout/crds

.PHONY: test
test: ## Run unit tests; database tests skip without a database
	go test ./...

.PHONY: test-integration
test-integration: ## Run all tests with the race detector against local Postgres
	ROLLOUT_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' ROLLOUT_TEST_REQUIRE_DB=1 go test -race -count=1 ./...

.PHONY: test-kind
test-kind: ## Run rollouts end to end on a local kind cluster (deletes it after unless KEEP_CLUSTER=1)
	test/kind/run.sh

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: vuln
vuln: ## Check dependencies and code for known vulnerabilities
	govulncheck ./...

.PHONY: secrets
secrets: ## Scan the git history for secrets
	gitleaks git --no-banner --redact .

.PHONY: check
check: lint test-integration vuln secrets ## Run every local verification step

.PHONY: dev-up
dev-up: ## Start local Postgres
	docker compose up -d --wait postgres

.PHONY: dev-down
dev-down: ## Stop local Postgres, keeping its data
	docker compose down

.PHONY: image
image: ## Build the container image fleet-rollout:dev
	docker build -t fleet-rollout:dev .

.PHONY: obs-up
obs-up: image ## Run rolloutd with Prometheus, Grafana, and Jaeger (needs ROLLOUT_ADMIN_TOKEN and release.pub)
	@test -n "$(ROLLOUT_ADMIN_TOKEN)" || { echo "set ROLLOUT_ADMIN_TOKEN first" >&2; exit 1; }
	@test -f "$(or $(ROLLOUT_TRUST_KEYS),release.pub)" || { echo "make a signing key first, with rolloutctl key generate" >&2; exit 1; }
	docker compose --profile observability up -d --wait

.PHONY: obs-down
obs-down: ## Stop the observability profile, keeping its data
	docker compose --profile observability down

.PHONY: dev-reset
dev-reset: ## Stop everything and delete this project's data volumes
	docker compose --profile observability down --volumes
