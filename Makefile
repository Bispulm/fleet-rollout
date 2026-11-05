.DEFAULT_GOAL := help

DEV_DATABASE_URL ?= postgres://rollout:rollout_dev@127.0.0.1:15431/rollout?sslmode=disable
TEST_DATABASE_URL ?= postgres://rollout:rollout_dev@127.0.0.1:15431/postgres?sslmode=disable

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-z0-9-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-18s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build all binaries into ./bin
	go build -o bin/ ./cmd/...

.PHONY: test
test: ## Run unit tests; database tests skip without a database
	go test ./...

.PHONY: test-integration
test-integration: ## Run all tests with the race detector against local Postgres
	ROLLOUT_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' ROLLOUT_TEST_REQUIRE_DB=1 go test -race -count=1 ./...

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

.PHONY: dev-reset
dev-reset: ## Stop local Postgres and delete this project's data volume
	docker compose down --volumes
