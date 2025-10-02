# Coves Testing Guide

This document explains how testing works in Coves, including setup, running tests, and understanding the test infrastructure.

## Overview

Coves uses a unified testing approach with:
- **Single configuration file**: [.env.dev](.env.dev) for all environments (dev + test)
- **Isolated test database**: PostgreSQL on port 5434 (separate from dev on 5433)
- **Makefile commands**: Simple `make test` command handles everything
- **Docker Compose profiles**: Test database spins up automatically

## Quick Start

```bash
# Run all tests (starts test DB, runs migrations, executes tests)
make test

# Reset test database (clean slate)
make test-db-reset

# Stop test database
make test-db-stop
```

## Test Infrastructure

### Configuration (.env.dev)

All test configuration lives in [.env.dev](.env.dev):

```bash
# Test Database Configuration
POSTGRES_TEST_DB=coves_test
POSTGRES_TEST_USER=test_user
POSTGRES_TEST_PASSWORD=test_password
POSTGRES_TEST_PORT=5434
```

**No separate `.env.test` file needed!** Everything is in `.env.dev`.

### Test Database

The test database runs in Docker via [docker-compose.dev.yml](docker-compose.dev.yml):

- **Service**: `postgres-test` (profile: `test`)
- **Port**: 5434 (separate from dev database on 5433)
- **Automatic startup**: The Makefile handles starting/stopping
- **Isolated data**: Completely separate from development database

### Running Tests Manually

If you need to run tests without the Makefile:

```bash
# 1. Load environment variables
set -a && source .env.dev && set +a

# 2. Start test database
docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test up -d postgres-test

# 3. Wait for it to be ready
sleep 3

# 4. Run migrations
goose -dir internal/db/migrations postgres \
  "postgresql://$POSTGRES_TEST_USER:$POSTGRES_TEST_PASSWORD@localhost:$POSTGRES_TEST_PORT/$POSTGRES_TEST_DB?sslmode=disable" up

# 5. Run tests
go test ./... -v

# 6. Stop test database (optional)
docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test stop postgres-test
```

**Note**: The Makefile automatically loads `.env.dev` variables, so `make test` is simpler than running manually.

## Test Types

### 1. Unit Tests

Test individual components in isolation.

**Example**: [internal/core/users/service_test.go](internal/core/users/service_test.go)

```bash
# Run unit tests for a specific package
go test -v ./internal/core/users/...
```

### 2. Integration Tests

Test full request/response flows with a real database.

**Location**: [tests/integration/](tests/integration/)

**How they work**:
- Start test database
- Run migrations
- Execute HTTP requests against real handlers
- Verify responses

**Example**: [tests/integration/integration_test.go](tests/integration/integration_test.go)

```bash
# Run integration tests
make test
# or
go test -v ./tests/integration/...
```

### 3. Lexicon Validation Tests

Validate AT Protocol Lexicon schemas and test data.

**Components**:
- **Schemas**: [internal/atproto/lexicon/](internal/atproto/lexicon/) - 57 lexicon schema files
- **Test Data**: [tests/lexicon-test-data/](tests/lexicon-test-data/) - Example records for validation
- **Validator**: [cmd/validate-lexicon/](cmd/validate-lexicon/) - Validation tool
- **Library**: [internal/validation/](internal/validation/) - Validation helpers

**Running validation**:

```bash
# Full validation (schemas + test data)
go run cmd/validate-lexicon/main.go

# Schemas only (skip test data)
go run cmd/validate-lexicon/main.go --schemas-only

# Verbose output
go run cmd/validate-lexicon/main.go -v

# Strict mode
go run cmd/validate-lexicon/main.go --strict
```

**Test data naming convention**:
- `*-valid*.json` - Should pass validation
- `*-invalid-*.json` - Should fail validation (tests error detection)

**Current coverage** (as of last update):
- ✅ social.coves.actor.profile
- ✅ social.coves.community.profile
- ✅ social.coves.post.record
- ✅ social.coves.interaction.vote
- ✅ social.coves.moderation.ban

## Database Migrations

Migrations are managed with [goose](https://github.com/pressly/goose) and stored in [internal/db/migrations/](internal/db/migrations/).

### Running Migrations

```bash
# Development database
make db-migrate

# Test database (automatically run by `make test`)
make test-db-reset
```

### Creating Migrations

```bash
# Create a new migration
goose -dir internal/db/migrations create migration_name sql

# This creates:
# internal/db/migrations/YYYYMMDDHHMMSS_migration_name.sql
```

### Migration Best Practices

- **Always test migrations** on test database first
- **Write both Up and Down** migrations
- **Keep migrations atomic** - one logical change per migration
- **Test rollback** - verify the Down migration works
- **Don't modify old migrations** - create new ones instead

## Test Database Management

### Fresh Start

```bash
# Complete reset (deletes all data)
make test-db-reset
```

### Connecting to Test Database

```bash
# Using psql
PGPASSWORD=test_password psql -h localhost -p 5434 -U test_user -d coves_test

# Using docker exec
docker exec -it coves-test-postgres psql -U test_user -d coves_test
```

### Inspecting Test Data

```sql
-- List all tables
\dt

-- View table schema
\d table_name

-- Query data
SELECT * FROM users;

-- Check migrations
SELECT * FROM goose_db_version;
```

## Writing Tests

### Integration Test Template

```go
package integration

import (
	"testing"
	// ... imports
)

func TestYourFeature(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Wire up dependencies
	repo := postgres.NewYourRepository(db)
	service := yourpackage.NewYourService(repo)

	// Create test router
	r := chi.NewRouter()
	r.Mount("/api/path", routes.YourRoutes(service))

	// Make request
	req := httptest.NewRequest("GET", "/api/path", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Assert
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}
```

### Test Helper: setupTestDB

The `setupTestDB` function (in [tests/integration/integration_test.go](tests/integration/integration_test.go)):
- Reads config from environment variables (set by `.env.dev`)
- Connects to test database
- Runs migrations
- Cleans up test data
- Returns ready-to-use `*sql.DB`

## Common Test Commands

```bash
# Run all tests
make test

# Run specific test package
go test -v ./internal/core/users/...

# Run specific test
go test -v ./tests/integration/... -run TestCreateUser

# Run with coverage
go test -v -cover ./...

# Run with race detector
go test -v -race ./...

# Verbose output
go test -v ./...

# Clean and run
make test-db-reset && make test
```

## Troubleshooting

### Test database won't start

```bash
# Check if port 5434 is in use
lsof -i :5434

# Check container logs
docker logs coves-test-postgres

# Nuclear reset
docker-compose -f docker-compose.dev.yml --profile test down -v
make test-db-reset
```

### Migrations failing

```bash
# Load environment and check migration status
set -a && source .env.dev && set +a
goose -dir internal/db/migrations postgres \
  "postgresql://$POSTGRES_TEST_USER:$POSTGRES_TEST_PASSWORD@localhost:$POSTGRES_TEST_PORT/$POSTGRES_TEST_DB?sslmode=disable" status

# Reset and retry
make test-db-reset
```

### Tests can't connect to database

```bash
# Verify test database is running
docker ps | grep coves-test-postgres

# Verify environment variables
set -a && source .env.dev && set +a && echo "Test DB Port: $POSTGRES_TEST_PORT"

# Test connection manually
PGPASSWORD=test_password psql -h localhost -p 5434 -U test_user -d coves_test -c "SELECT 1"
```

### Lexicon validation errors

```bash
# Check schema syntax
cat internal/atproto/lexicon/path/to/schema.json | jq .

# Validate specific schema
go run cmd/validate-lexicon/main.go -v

# Check test data format
cat tests/lexicon-test-data/your-test.json | jq .
```

## Best Practices

### Test Organization
- ✅ Unit tests live next to the code they test (`*_test.go`)
- ✅ Integration tests live in `tests/integration/`
- ✅ Test data lives in `tests/lexicon-test-data/`
- ✅ One test file per feature/endpoint

### Test Data
- ✅ Use `@example.com` emails for test users (auto-cleaned by setupTestDB)
- ✅ Clean up data in tests (or rely on setupTestDB cleanup)
- ✅ Don't rely on specific test execution order
- ✅ Each test should be independent

### Database Tests
- ✅ Always use the test database (port 5434)
- ✅ Never connect to development database (port 5433) in tests
- ✅ Use transactions for fast test cleanup (where applicable)
- ✅ Test with realistic data sizes

### Lexicon Tests
- ✅ Create both valid and invalid test cases
- ✅ Cover all required fields
- ✅ Test edge cases (empty strings, max lengths, etc.)
- ✅ Update tests when schemas change

## CI/CD Integration

When setting up CI/CD, the test pipeline should:

```yaml
# Example GitHub Actions workflow
steps:
  - name: Start test database
    run: |
      docker-compose -f docker-compose.dev.yml --env-file .env.dev --profile test up -d postgres-test
      sleep 5

  - name: Run migrations
    run: make test-db-migrate  # (add this to Makefile if needed)

  - name: Run tests
    run: make test

  - name: Cleanup
    run: docker-compose -f docker-compose.dev.yml --profile test down -v
```

## Related Documentation

- [Makefile](Makefile) - All test commands
- [.env.dev](.env.dev) - Test configuration
- [docker-compose.dev.yml](docker-compose.dev.yml) - Test database setup
- [LOCAL_DEVELOPMENT.md](docs/LOCAL_DEVELOPMENT.md) - Full development setup
- [CLAUDE.md](CLAUDE.md) - Build guidelines

## Getting Help

If tests are failing:
1. Check [Troubleshooting](#troubleshooting) section above
2. Verify `.env.dev` has correct test database config
3. Run `make test-db-reset` for a clean slate
4. Check test database logs: `docker logs coves-test-postgres`

For questions about:
- **Test infrastructure**: This document
- **AT Protocol testing**: [ATPROTO_GUIDE.md](ATPROTO_GUIDE.md)
- **Development setup**: [LOCAL_DEVELOPMENT.md](docs/LOCAL_DEVELOPMENT.md)
