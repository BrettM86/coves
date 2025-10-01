.PHONY: help dev-up dev-down dev-logs dev-status dev-reset dev-db-up dev-db-down dev-db-reset test clean

# Default target - show help
.DEFAULT_GOAL := help

# Colors for output
CYAN := \033[36m
RESET := \033[0m
GREEN := \033[32m
YELLOW := \033[33m

##@ General

help: ## Show this help message
	@echo ""
	@echo "$(CYAN)Coves Development Commands$(RESET)"
	@echo ""
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make $(CYAN)<target>$(RESET)\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  $(CYAN)%-15s$(RESET) %s\n", $$1, $$2 } \
		/^##@/ { printf "\n$(YELLOW)%s$(RESET)\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@echo ""

##@ Local Development (All-in-One)

dev-up: ## Start PDS + PostgreSQL for local development
	@echo "$(GREEN)Starting Coves development stack...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev up -d postgres pds
	@echo ""
	@echo "$(GREEN)✓ Development stack started!$(RESET)"
	@echo ""
	@echo "Services available at:"
	@echo "  - PostgreSQL:        localhost:5433"
	@echo "  - PDS (XRPC):        http://localhost:3001"
	@echo "  - PDS Firehose (WS): ws://localhost:3001/xrpc/com.atproto.sync.subscribeRepos"
	@echo "  - AppView (API):     http://localhost:8081 (when uncommented)"
	@echo ""
	@echo "Run 'make dev-logs' to view logs"

dev-down: ## Stop all development services
	@echo "$(YELLOW)Stopping Coves development stack...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev down
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
	@goose -dir internal/db/migrations postgres "postgresql://dev_user:dev_password@localhost:5433/coves_dev?sslmode=disable" up
	@echo "$(GREEN)✓ Migrations complete$(RESET)"

db-migrate-down: ## Rollback last migration
	@echo "$(YELLOW)Rolling back last migration...$(RESET)"
	@goose -dir internal/db/migrations postgres "postgresql://dev_user:dev_password@localhost:5433/coves_dev?sslmode=disable" down
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

test: ## Run all tests with test database
	@echo "$(GREEN)Starting test database...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test up -d postgres-test
	@echo "Waiting for test database to be ready..."
	@sleep 3
	@echo "$(GREEN)Running migrations on test database...$(RESET)"
	@goose -dir internal/db/migrations postgres "postgresql://test_user:test_password@localhost:5434/coves_test?sslmode=disable" up || true
	@echo "$(GREEN)Running tests...$(RESET)"
	@TEST_DATABASE_URL="postgresql://test_user:test_password@localhost:5434/coves_test?sslmode=disable" go test ./... -v
	@echo "$(GREEN)✓ Tests complete$(RESET)"

test-db-reset: ## Reset test database
	@echo "$(GREEN)Resetting test database...$(RESET)"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test rm -sf postgres-test
	@docker volume rm coves-test-postgres-data || true
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test up -d postgres-test
	@echo "Waiting for PostgreSQL to be ready..."
	@sleep 3
	@goose -dir internal/db/migrations postgres "postgresql://test_user:test_password@localhost:5434/coves_test?sslmode=disable" up || true
	@echo "$(GREEN)✓ Test database reset$(RESET)"

test-db-stop: ## Stop test database
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test stop postgres-test
	@echo "$(GREEN)✓ Test database stopped$(RESET)"

##@ Build & Run

build: ## Build the Coves server
	@echo "$(GREEN)Building Coves server...$(RESET)"
	@go build -o server ./cmd/server
	@echo "$(GREEN)✓ Build complete: ./server$(RESET)"

run: ## Run the Coves server (requires database running)
	@echo "$(GREEN)Starting Coves server...$(RESET)"
	@go run ./cmd/server

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
	@echo "  - Build Guide:        CLAUDE.md"
	@echo "  - atProto Guide:      ATPROTO_GUIDE.md"
	@echo "  - PRD:                PRD.md"
