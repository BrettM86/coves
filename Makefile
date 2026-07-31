.PHONY: help dev-up dev-up-otel dev-down dev-logs dev-status dev-reset test test-integration test-e2e test-e2e-dev test-live test-db-prepare test-audit ci ci-clean clean mobile-full-setup

# Default target - show help
.DEFAULT_GOAL := help

# Colors for output
CYAN := \033[36m
RESET := \033[0m
GREEN := \033[32m
YELLOW := \033[33m
RED := \033[31m

# Load test database configuration from .env.dev
include .env.dev
export

##@ General

help: ## Show this help message
	@echo ""
	@echo "$(CYAN)Coves Development Commands$(RESET)"
	@echo ""
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make $(CYAN)<target>$(RESET)\n"} \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  $(CYAN)%-18s$(RESET) %s\n", $$1, $$2 } \
		/^##@/ { printf "\n$(YELLOW)%s$(RESET)\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@echo ""

##@ Local Development (All-in-One)

dev-up: ## Start PDS + PostgreSQL + Jetstream + PLC Directory for local development
	@echo "$(GREEN)Starting Coves development stack...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile jetstream --profile plc up -d postgres postgres-plc plc-directory pds jetstream
	@echo ""
	@echo "$(GREEN)✓ Development stack started!$(RESET)"
	@echo ""
	@echo "Services available at:"
	@echo "  - PostgreSQL:        localhost:5435"
	@echo "  - PDS (XRPC):        http://localhost:3001"
	@echo "  - PDS Firehose:      ws://localhost:3001/xrpc/com.atproto.sync.subscribeRepos"
	@echo "  - Jetstream:         ws://localhost:6008/subscribe  $(CYAN)(Read-Forward)$(RESET)"
	@echo "  - Jetstream Metrics: http://localhost:6009/metrics"
	@echo "  - PLC Directory:     http://localhost:3002  $(CYAN)(Local DID registry)$(RESET)"
	@echo ""
	@echo "$(CYAN)Next steps:$(RESET)"
	@echo "  1. Run: make run  (starts AppView)"
	@echo "  2. AppView will auto-index users from Jetstream"
	@echo ""
	@echo "$(CYAN)Optional:$(RESET) Run 'make dev-up-otel' to add Jaeger for tracing"
	@echo "$(CYAN)Note:$(RESET) Using local PLC directory - DIDs registered locally (won't pollute plc.directory)"
	@echo "Run 'make dev-logs' to view logs"

dev-up-otel: ## Start dev stack + Jaeger for OpenTelemetry tracing
	@echo "$(GREEN)Starting Coves development stack with OpenTelemetry...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile jetstream --profile plc --profile observability up -d postgres postgres-plc plc-directory pds jetstream jaeger
	@echo ""
	@echo "$(GREEN)✓ Development stack with tracing started!$(RESET)"
	@echo ""
	@echo "Services available at:"
	@echo "  - PostgreSQL:        localhost:5435"
	@echo "  - PDS (XRPC):        http://localhost:3001"
	@echo "  - Jetstream:         ws://localhost:6008/subscribe"
	@echo "  - PLC Directory:     http://localhost:3002"
	@echo "  - $(CYAN)Jaeger UI:         http://localhost:16686$(RESET)  $(CYAN)(Trace viewer)$(RESET)"
	@echo "  - $(CYAN)OTLP Collector:    localhost:4317$(RESET)  $(CYAN)(gRPC endpoint)$(RESET)"
	@echo ""
	@echo "$(CYAN)To enable tracing in AppView:$(RESET)"
	@echo "  export OTEL_ENABLED=true"
	@echo "  export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317"
	@echo "  export OTEL_EXPORTER_OTLP_INSECURE=true"
	@echo "  make run"

dev-down: ## Stop all development services (including Jaeger if running)
	@echo "$(YELLOW)Stopping Coves development stack...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile jetstream --profile plc --profile observability --profile test down --remove-orphans
	@docker network rm coves-dev-network 2>/dev/null || true
	@echo "$(GREEN)✓ Development stack stopped$(RESET)"

dev-logs: ## Tail logs from all development services
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev logs -f

dev-status: ## Show status of all development containers
	@echo "$(CYAN)Development Stack Status:$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev ps

dev-reset: ## Nuclear option - stop everything and remove all volumes
	@echo "$(YELLOW)⚠️  WARNING: This will delete ALL data (PostgreSQL + PDS)!$(RESET)"
	@read -p "Are you sure? (y/N): " confirm && [ "$$confirm" = "y" ] || exit 1
	@echo "$(YELLOW)Stopping and removing containers and volumes...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev down -v
	@echo "$(GREEN)✓ Reset complete - all data removed$(RESET)"
	@echo "Run 'make dev-up' to start fresh"

##@ Database Management

db-shell: ## Open PostgreSQL shell for development database
	@echo "$(CYAN)Connecting to development database...$(RESET)"
	@docker exec -it coves-dev-postgres psql -U dev_user -d coves_dev

db-migrate: ## Run database migrations
	@echo "$(GREEN)Running database migrations...$(RESET)"
	@goose -dir internal/db/migrations postgres "postgresql://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable" up
	@echo "$(GREEN)✓ Migrations complete$(RESET)"

db-migrate-down: ## Rollback last migration
	@echo "$(YELLOW)Rolling back last migration...$(RESET)"
	@goose -dir internal/db/migrations postgres "postgresql://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable" down
	@echo "$(GREEN)✓ Rollback complete$(RESET)"

db-reset: ## Reset database (delete all data and re-run migrations)
	@echo "$(YELLOW)⚠️  WARNING: This will delete all database data!$(RESET)"
	@read -p "Are you sure? (y/N): " confirm && [ "$$confirm" = "y" ] || exit 1
	@echo "$(YELLOW)Resetting database...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev rm -sf postgres
	@docker volume rm coves-dev-postgres-data || true
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev up -d postgres
	@echo "Waiting for PostgreSQL to be ready..."
	@sleep 3
	@make db-migrate
	@echo "$(GREEN)✓ Database reset complete$(RESET)"

##@ Testing

test: ## T0 unit tier - no Docker, no database, no public network. The inner loop.
	@# Untagged `go test` is the unit tier by construction: every test that
	@# needs something out of process carries a build tag (integration/e2e/live)
	@# and is therefore not in this build at all. That is what lets this target
	@# start no containers and wait for nothing — and it is checked, not hoped
	@# for: the tier is verified by running this selection with the network
	@# switched off entirely.
	@#
	@# "No network" means nothing out of process: in-process httptest servers on
	@# loopback are used freely here, and they are why `--network none` is the
	@# honest check rather than an unreachable ideal.
	@echo "$(GREEN)Running the unit tier (untagged)...$(RESET)"
	@go test ./cmd/... ./internal/... ./tests/...
	@echo "$(GREEN)✓ Unit tier complete$(RESET)"

test-integration: ## T1 integration tier - needs Postgres; starts postgres-test itself
	@echo "$(GREEN)Starting test database...$(RESET)"
	@# Best-effort, deliberately: `compose up` fails when the container already
	@# exists under a different Compose project name, which is the normal case in
	@# a git worktree (the project name comes from the directory, the container
	@# name is pinned). An already-running database is success, not an error.
	@#
	@# This is not a swallowed failure, because the readiness poll below is the
	@# actual assertion — if `up` failed AND no database is listening, that loop
	@# exits non-zero with a message naming the fix.
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test up -d postgres-test 2>/dev/null \
		|| echo "$(YELLOW)  compose up declined (already running under another project?) - checking readiness anyway$(RESET)"
	@# Diagnose the two failures that are NOT "the database is still starting",
	@# because both otherwise surface as a readiness timeout that blames the
	@# wrong thing.
	@docker info >/dev/null 2>&1 || \
		(echo "$(RED)✗ Docker is not responding. Start Docker Desktop (or the daemon) and retry.$(RESET)" && exit 1)
	@docker ps --filter name=^/coves-test-postgres$$ --filter status=running --format '{{.Names}}' \
		| grep -q coves-test-postgres || \
		(echo "$(RED)✗ Container coves-test-postgres is not running, and 'compose up' did not start it.$(RESET)" && \
		 echo "$(RED)  Rebuild it with 'make test-db-reset'.$(RESET)" && exit 1)
	@echo "Waiting for test database to accept connections..."
	@# Provisions the template database that testkit.DB clones per test and
	@# sweeps clones orphaned by killed runs.
	@#
	@# This is also the readiness gate, and deliberately so: it waits by opening
	@# a real connection to POSTGRES_TEST_HOST:PORT — the same host endpoint the
	@# tests dial. An in-container `pg_isready` would prove only that Postgres is
	@# up on its own loopback, so a wrong or already-claimed published port would
	@# sail through the check and then fail as a wall of "connection refused".
	@./scripts/test-db-prepare.sh || \
		(echo "$(RED)✗ Could not reach Postgres at localhost:$(POSTGRES_TEST_PORT) even though the container is running.$(RESET)" && \
		 echo "$(RED)  Check POSTGRES_TEST_PORT in .env.dev against the port the container actually publishes:$(RESET)" && \
		 echo "$(RED)    docker port coves-test-postgres$(RESET)" && exit 1)
	@echo "$(GREEN)Running the integration tier (-tags integration)...$(RESET)"
	@# The tag set is additive: an `integration` build contains the untagged
	@# unit files too, so this compiles and runs T0+T1 in one pass.
	@#
	@# Both concurrency flags come from the server's max_connections, because
	@# they multiply: -p test binaries each running -parallel tests hold that
	@# many clone pools at once. testkit.ConcurrencyBudget does the arithmetic;
	@# hardcoding either number is how a suite discovers its ceiling by hitting
	@# it, as a "too many clients" failure in whichever test was unlucky.
	@#
	@# Captured into a variable and checked, NOT spliced inline. A failed
	@# $$(...) inside the go test line contributes an empty string and does not
	@# fail the recipe, so go test would silently run at its DEFAULT -p
	@# (GOMAXPROCS) — discarding the measured budget precisely when the thing
	@# that measures it is broken. Fail-open on a safety limit is worse than
	@# not having one, because it looks like it worked.
	@set -e; \
	FLAGS=$$(./scripts/test-db-prepare.sh --print-flags) || exit 1; \
	[ -n "$$FLAGS" ] || { echo "$(RED)✗ test-db-prepare printed no concurrency flags$(RESET)"; exit 1; }; \
	echo "  concurrency budget: $$FLAGS"; \
	go test -tags integration $$FLAGS ./cmd/... ./internal/... ./tests/...
	@echo ""
	@echo "$(YELLOW)Note: some packages need a PDS as well as Postgres, and each one's$(RESET)"
	@echo "$(YELLOW)TestMain says so up front — without 'make dev-up' the package fails$(RESET)"
	@echo "$(YELLOW)once, naming the address it could not reach, instead of skipping test$(RESET)"
	@echo "$(YELLOW)by test and reporting green. 'make ci' remains the gate.$(RESET)"

test-e2e: ## T2 pipeline tier - brings up the hermetic stack and runs inside it
	@# docs/TEST_ARCHITECTURE.md §3.5. The hermetic stack publishes no host
	@# ports, so a host-run `go test -tags e2e` cannot reach it; this brings the
	@# stack up (or reuses a COVES_CI_KEEP_STACK one) and runs the tier inside
	@# the runner's network namespace — the same path `make ci` takes, so there
	@# is exactly one way T2 executes.
	@#
	@# Needs no dev stack and conflicts with none: it builds its own AppView
	@# from the working tree and publishes nothing.
	@./scripts/test-e2e.sh

test-e2e-dev: ## T2 against the long-lived DEV stack (debugging only - not how CI runs it)
	@# THE ESCAPE HATCH, and named so nobody reaches for it by accident.
	@#
	@# It grades whatever `make dev-up` and `make run` happen to be serving,
	@# which is the failure mode 'make test-e2e' exists to remove: an AppView
	@# many edits stale, a PDS full of accounts from last week, a Jetstream
	@# replaying a month of backlog. A green run here proves less than a green
	@# 'make test-e2e', and a red one may be about your stack rather than your
	@# code.
	@#
	@# What it buys is a live stack you can attach a debugger to, inspect with
	@# psql, and leave running between edits. That is worth having, explicitly.
	@echo "$(YELLOW)DEBUGGING TARGET: grading the dev stack, not a hermetic one.$(RESET)"
	@echo "$(YELLOW)'make test-e2e' is the real pipeline tier; 'make ci' is the gate.$(RESET)"
	@echo ""
	@echo "$(CYAN)Checking the dev stack is reachable...$(RESET)"
	@curl -sf http://127.0.0.1:3001/xrpc/_health >/dev/null 2>&1 || \
		(echo "$(RED)  ✗ PDS not reachable on :3001. Run 'make dev-up'.$(RESET)" && exit 1)
	@echo "  $(GREEN)✓ PDS (:3001)$(RESET)"
	@curl -sf http://127.0.0.1:6009/metrics >/dev/null 2>&1 || \
		(echo "$(RED)  ✗ Jetstream not reachable on :6009. Run 'make dev-up'.$(RESET)" && exit 1)
	@echo "  $(GREEN)✓ Jetstream (:6008, metrics :6009)$(RESET)"
	@curl -sf http://127.0.0.1:8081/xrpc/_health >/dev/null 2>&1 || \
		(echo "$(RED)  ✗ AppView not reachable on :8081. Run 'make run' in another terminal.$(RESET)" && exit 1)
	@echo "  $(GREEN)✓ AppView (:8081)$(RESET)"
	@echo ""
	@echo "$(CYAN)Contract manifest:$(RESET)"
	@go run ./cmd/contract-manifest
	@echo "$(GREEN)Running the pipeline tier against the dev stack (-tags e2e)...$(RESET)"
	@echo "$(YELLOW)  minus the reliability suite: it stops, starts and reconfigures the AppView$(RESET)"
	@echo "$(YELLOW)  CONTAINER, and this hatch grades a host-run 'make run' process instead.$(RESET)"
	@echo "$(YELLOW)  minus the federation contracts: they need the SECOND PDS and the relay,$(RESET)"
	@echo "$(YELLOW)  which exist only in the hermetic stack (docker-compose.ci.yml). The dev$(RESET)"
	@echo "$(YELLOW)  stack has one PDS and Jetstream wired straight to it.$(RESET)"
	@# run_pipeline_tier is the ONE definition of how T2 is invoked — the gate,
	@# 'make test-e2e' and this hatch all call it, so the flags cannot drift
	@# apart. Sourced here rather than copied for exactly that reason.
	@#
	@# -skip, not a t.Skip: skipping is banned in test bodies (§3.1) and the
	@# pipeline tier fails outright on a SKIP event (scripts/e2e-runner.sh),
	@# both for the same reason — a contract that opts out of proving itself is
	@# indistinguishable from a broken pipeline. Excluding by name at the
	@# selection level keeps that rule intact: these tests do not run here, and
	@# they do not report anything either.
	@#
	@# "Federat" catches every contract in tests/e2e/federation_contract_test.go
	@# (TestPostFederationIngestion, TestCommentFederationIngestion,
	@# TestVoteFederationIngestion, TestFederationRemoteBlobFetch,
	@# TestFederatedIdentityIsNotIndexed) by the substring their names share,
	@# rather than by a list that would go stale the first time one is added.
	@# A federation contract named without it is not a silent pass either:
	@# testkit.NewFederatedPDS fatals on the spot, naming PDS2_URL and this
	@# hatch.
	@bash -c 'source ./scripts/lib/runner-ready.sh && run_pipeline_tier -skip "^TestReliability|Federat"'
	@echo "$(GREEN)✓ Pipeline tier complete (against the dev stack; no reliability suite, no federation contracts)$(RESET)"

test-db-reset: ## Reset test database
	@echo "$(GREEN)Resetting test database...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test rm -sf postgres-test
	@docker volume rm coves-test-postgres-data || true
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test up -d postgres-test
	@echo "Waiting for PostgreSQL to be ready..."
	@# Rebuilds the template testkit.DB clones. NOT `goose up` against
	@# POSTGRES_TEST_DB: no test reads that database's schema any more — it is
	@# only the maintenance database testkit connects to in order to CREATE and
	@# DROP the others. Migrating it produced tables nothing queried and hid the
	@# fact that the template, which every test actually clones, had not been
	@# rebuilt at all.
	@#
	@# This also waits for the server, so the fixed sleep above is only for the
	@# container process, not for Postgres accepting connections.
	@sleep 3
	@./scripts/test-db-prepare.sh --force
	@echo "$(GREEN)✓ Test database reset$(RESET)"

test-db-prepare: ## Create or refresh the template database that testkit.DB clones per test
	@./scripts/test-db-prepare.sh

test-audit: ## Test-suite invariant audit - hard gate, any violation fails (-v for file:line)
	@./scripts/test-audit.sh

test-db-stop: ## Stop test database
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test stop postgres-test
	@echo "$(GREEN)✓ Test database stopped$(RESET)"

test-live: ## Run the opt-in tests that deliberately hit the public internet (NOT part of the merge gate)
	@echo "$(CYAN)═══════════════════════════════════════════════════════════════$(RESET)"
	@echo "$(CYAN)  LIVE TIER - real Bluesky, real PLC, real third-party unfurls  $(RESET)"
	@echo "$(CYAN)═══════════════════════════════════════════════════════════════$(RESET)"
	@echo ""
	@echo "$(YELLOW)These reach the public internet by design, so they can fail for$(RESET)"
	@echo "$(YELLOW)reasons that have nothing to do with your change. 'make ci' is$(RESET)"
	@echo "$(YELLOW)the merge gate; this is a reality check you run deliberately.$(RESET)"
	@echo ""
	@echo "$(CYAN)Requires: the test database on port 5434 ('make dev-up').$(RESET)"
	@echo ""
	@go test -tags live -count=1 -timeout 600s ./tests/live/... -v

ci: ## Hermetic merge gate - builds its own stack from scratch, runs everything, enforces the skip allowlist
	@./scripts/ci.sh

ci-clean: ## Remove the CI Go module/build cache volumes (forces a fully cold next run)
	@echo "$(YELLOW)Removing CI cache volumes...$(RESET)"
	@docker volume rm coves-ci-go-mod-cache coves-ci-go-build-cache 2>/dev/null || true
	@echo "$(GREEN)✓ CI caches removed - the next 'make ci' will be cold$(RESET)"
	@echo "$(YELLOW)  It re-downloads every module before the stack starts (the$(RESET)"
	@echo "$(YELLOW)  stack's network is egress-blocked), so expect a slow run.$(RESET)"

##@ Code Quality

fmt: ## Format all Go code with gofmt
	@echo "$(GREEN)Formatting Go code...$(RESET)"
	@gofmt -w ./cmd ./internal ./tests
	@echo "$(GREEN)✓ Formatting complete$(RESET)"

fmt-check: ## Check if Go code is properly formatted
	@echo "$(GREEN)Checking code formatting...$(RESET)"
	@unformatted=$$(gofmt -l ./cmd ./internal ./tests); \
	if [ -n "$$unformatted" ]; then \
		echo "$(RED)✗ The following files are not formatted:$(RESET)"; \
		echo "$$unformatted"; \
		echo "$(YELLOW)Run 'make fmt' to fix$(RESET)"; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ All files are properly formatted$(RESET)"

lint: fmt-check ## Run golangci-lint on the codebase (includes format check)
	@echo "$(GREEN)Running linter...$(RESET)"
	@golangci-lint run ./cmd/... ./internal/... ./tests/...
	@echo "$(GREEN)✓ Linting complete$(RESET)"

lint-fix: ## Run golangci-lint and auto-fix issues
	@echo "$(GREEN)Running linter with auto-fix...$(RESET)"
	@golangci-lint run --fix ./cmd/... ./internal/... ./tests/...
	@gofmt -w ./cmd ./internal ./tests
	@echo "$(GREEN)✓ Linting complete$(RESET)"

##@ Build & Run

build: ## Build the Coves server (production - no dev code)
	@echo "$(GREEN)Building Coves server (production)...$(RESET)"
	@go build -o server ./cmd/server
	@echo "$(GREEN)✓ Build complete: ./server$(RESET)"

build-dev: ## Build the Coves server with dev mode (includes localhost OAuth resolvers)
	@echo "$(GREEN)Building Coves server (dev mode)...$(RESET)"
	@go build -tags dev -o server ./cmd/server
	@echo "$(GREEN)✓ Build complete: ./server (with dev tags)$(RESET)"

run: ## Run the Coves server with dev environment (requires database running)
	@make db-migrate
	@./scripts/dev-run.sh

##@ Cleanup

clean: ## Clean build artifacts and temporary files
	@echo "$(YELLOW)Cleaning build artifacts...$(RESET)"
	@rm -f server main validate-lexicon
	@go clean
	@echo "$(GREEN)✓ Clean complete$(RESET)"

clean-all: clean ## Clean everything including Docker volumes (DESTRUCTIVE)
	@echo "$(YELLOW)⚠️  WARNING: This will remove ALL Docker volumes!$(RESET)"
	@read -p "Are you sure? (y/N): " confirm && [ "$$confirm" = "y" ] || exit 1
	@make dev-reset
	@echo "$(GREEN)✓ All clean$(RESET)"

##@ Workflows (Common Tasks)

fresh-start: ## Complete fresh start (reset everything, start clean)
	@echo "$(CYAN)Starting fresh development environment...$(RESET)"
	@make dev-reset || true
	@sleep 2
	@make dev-up
	@sleep 3
	@make db-migrate
	@echo ""
	@echo "$(GREEN)✓ Fresh environment ready!$(RESET)"
	@make dev-status

quick-restart: ## Quick restart of development stack (keeps data)
	@make dev-down
	@make dev-up

##@ Mobile Testing

mobile-setup: ## Setup Android port forwarding for USB-connected devices (recommended)
	@echo "$(CYAN)Setting up Android mobile testing environment...$(RESET)"
	@./scripts/setup-mobile-ports.sh

mobile-reset: ## Remove all Android port forwarding
	@echo "$(YELLOW)Removing Android port forwarding...$(RESET)"
	@adb reverse --remove-all || echo "$(YELLOW)No device connected$(RESET)"
	@echo "$(GREEN)✓ Port forwarding removed$(RESET)"

mobile-full-setup: mobile-setup ## Full mobile setup: setup ports
	@echo ""
	@echo "$(GREEN)═══════════════════════════════════════════════════════════$(RESET)"
	@echo "$(GREEN)  Mobile development environment ready!                     $(RESET)"
	@echo "$(GREEN)═══════════════════════════════════════════════════════════$(RESET)"
	@echo ""
	@echo "$(CYAN)Run the Flutter app with:$(RESET)"
	@echo "  $(YELLOW)cd /home/bretton/Code/coves-mobile$(RESET)"
	@echo "  $(YELLOW)flutter run --dart-define=ENVIRONMENT=local$(RESET)"
	@echo ""

ngrok-up: ## Start ngrok tunnels (for iOS or WiFi testing - requires paid plan for 3 tunnels)
	@echo "$(GREEN)Starting ngrok tunnels for mobile testing...$(RESET)"
	@./scripts/start-ngrok.sh

ngrok-down: ## Stop all ngrok tunnels
	@./scripts/stop-ngrok.sh

##@ Web Frontend Development

run-web: ## Run Coves backend configured for web frontend dev (OAuth via :8080 proxy)
	@make db-migrate
	@./scripts/web-dev-run.sh

web-proxy: ## Start Caddy reverse proxy for web frontend dev (combines Vite + Coves on :8080)
	@echo "$(CYAN)Starting web development proxy...$(RESET)"
	@echo ""
	@echo "$(YELLOW)Prerequisites:$(RESET)"
	@echo "  1. Coves backend running on :8081  (make run)"
	@echo "  2. Vite frontend running on :5173  (cd frontend && npm run dev)"
	@echo ""
	@command -v caddy >/dev/null 2>&1 || { echo "$(RED)Error: Caddy not installed. Install with:$(RESET)"; \
		echo "  Ubuntu/Debian: sudo apt install caddy"; \
		echo "  macOS: brew install caddy"; \
		echo "  Or see: https://caddyserver.com/docs/install"; \
		exit 1; }
	@echo "$(GREEN)Starting Caddy on http://localhost:8080$(RESET)"
	@echo "  Backend routes (/oauth/*, /xrpc/*, /api/*) -> 127.0.0.1:8081"
	@echo "  Frontend routes (everything else) -> localhost:5173"
	@echo ""
	@echo "$(CYAN)Access your app at: http://localhost:8080$(RESET)"
	@echo "$(CYAN)Press Ctrl+C to stop$(RESET)"
	@echo ""
	@caddy run --config Caddyfile.dev

web-proxy-bg: ## Start Caddy proxy in background
	@command -v caddy >/dev/null 2>&1 || { echo "$(RED)Error: Caddy not installed$(RESET)"; exit 1; }
	@caddy start --config Caddyfile.dev
	@echo "$(GREEN)✓ Caddy proxy started in background on http://localhost:8080$(RESET)"

web-proxy-stop: ## Stop background Caddy proxy
	@caddy stop 2>/dev/null || echo "$(YELLOW)Caddy not running$(RESET)"
	@echo "$(GREEN)✓ Caddy proxy stopped$(RESET)"

##@ Utilities

validate-lexicon: ## Validate all Lexicon schemas
	@echo "$(GREEN)Validating Lexicon schemas...$(RESET)"
	@./validate-lexicon
	@echo "$(GREEN)✓ Lexicon validation complete$(RESET)"

##@ Documentation

docs: ## Open project documentation
	@echo "$(CYAN)Project Documentation:$(RESET)"
	@echo "  - Setup Guide:        docs/LOCAL_DEVELOPMENT.md"
	@echo "  - Project Structure:  PROJECT_STRUCTURE.md"
	@echo "  - atProto Guide:      ATPROTO_GUIDE.md"
