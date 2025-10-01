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

##@ Local Development (atProto Stack)

dev-up: ## Start PDS for local development
	@echo "$(GREEN)Starting Coves development stack...$(RESET)"
	@echo "$(YELLOW)Note: Make sure PostgreSQL is running on port 5433$(RESET)"
	@echo "Run 'make dev-db-up' if database is not running"
	@docker-compose -f docker-compose.dev.yml --env-file .env.dev up -d pds
	@echo ""
	@echo "$(GREEN)✓ Development stack started!$(RESET)"
	@echo ""
	@echo "Services available at:"
	@echo "  - PDS (XRPC):        http://localhost:3001"
	@echo "  - PDS Firehose (WS): ws://localhost:3001/xrpc/com.atproto.sync.subscribeRepos"
	@echo "  - AppView (API):     http://localhost:8081 (when uncommented)"
	@echo ""
	@echo "Run 'make dev-logs' to view logs"

dev-down: ## Stop the atProto development stack
	@echo "$(YELLOW)Stopping Coves development stack...$(RESET)"
	@docker-compose -f docker-compose.dev.yml down
	@echo "$(GREEN)✓ Development stack stopped$(RESET)"

dev-logs: ## Tail logs from all development services
	@docker-compose -f docker-compose.dev.yml logs -f

dev-status: ## Show status of all development containers
	@echo "$(CYAN)Development Stack Status:$(RESET)"
	@docker-compose -f docker-compose.dev.yml ps
	@echo ""
	@echo "$(CYAN)Database Status:$(RESET)"
	@cd internal/db/local_dev_db_compose && docker-compose ps

dev-reset: ## Nuclear option - stop everything and remove all volumes
	@echo "$(YELLOW)⚠️  WARNING: This will delete all PDS data and volumes!$(RESET)"
	@read -p "Are you sure? (y/N): " confirm && [ "$$confirm" = "y" ] || exit 1
	@echo "$(YELLOW)Stopping and removing containers and volumes...$(RESET)"
	@docker-compose -f docker-compose.dev.yml down -v
	@echo "$(GREEN)✓ Reset complete - all data removed$(RESET)"
	@echo "Run 'make dev-up' to start fresh"

##@ Database Management

dev-db-up: ## Start local PostgreSQL database (port 5433)
	@echo "$(GREEN)Starting local PostgreSQL database...$(RESET)"
	@cd internal/db/local_dev_db_compose && docker-compose up -d
	@echo "$(GREEN)✓ Database started on port 5433$(RESET)"
	@echo "Connection: postgresql://dev_user:dev_password@localhost:5433/coves_dev"

dev-db-down: ## Stop local PostgreSQL database
	@echo "$(YELLOW)Stopping local PostgreSQL database...$(RESET)"
	@cd internal/db/local_dev_db_compose && docker-compose down
	@echo "$(GREEN)✓ Database stopped$(RESET)"

dev-db-reset: ## Reset database (delete all data and restart)
	@echo "$(YELLOW)⚠️  WARNING: This will delete all database data!$(RESET)"
	@read -p "Are you sure? (y/N): " confirm && [ "$$confirm" = "y" ] || exit 1
	@echo "$(YELLOW)Resetting database...$(RESET)"
	@cd internal/db/local_dev_db_compose && docker-compose down -v
	@cd internal/db/local_dev_db_compose && docker-compose up -d
	@echo "$(GREEN)✓ Database reset complete$(RESET)"

##@ Testing

test: ## Run all tests with test database
	@echo "$(GREEN)Starting test database...$(RESET)"
	@cd internal/db/test_db_compose && ./start-test-db.sh
	@echo "$(GREEN)Running tests...$(RESET)"
	@./run-tests.sh
	@echo "$(GREEN)✓ Tests complete$(RESET)"

test-db-reset: ## Reset test database
	@echo "$(GREEN)Resetting test database...$(RESET)"
	@cd internal/db/test_db_compose && ./reset-test-db.sh
	@echo "$(GREEN)✓ Test database reset$(RESET)"

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
	@make dev-db-reset
	@echo "$(GREEN)✓ All clean$(RESET)"

##@ Workflows (Common Tasks)

fresh-start: ## Complete fresh start (reset DB, reset stack, start everything)
	@echo "$(CYAN)Starting fresh development environment...$(RESET)"
	@make dev-db-reset
	@make dev-reset || true
	@sleep 2
	@make dev-db-up
	@sleep 2
	@make dev-up
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

db-shell: ## Open PostgreSQL shell for local database
	@echo "$(CYAN)Connecting to local database...$(RESET)"
	@PGPASSWORD=dev_password psql -h localhost -p 5433 -U dev_user -d coves_dev

##@ Documentation

docs: ## Open project documentation
	@echo "$(CYAN)Project Documentation:$(RESET)"
	@echo "  - Setup Guide:        docs/LOCAL_DEVELOPMENT.md"
	@echo "  - Project Structure:  PROJECT_STRUCTURE.md"
	@echo "  - Build Guide:        CLAUDE.md"
	@echo "  - atProto Guide:      ATPROTO_GUIDE.md"
	@echo "  - PRD:                PRD.md"
