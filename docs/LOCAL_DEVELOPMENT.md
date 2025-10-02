# Coves Local Development Guide

Complete guide for setting up and running the Coves atProto development environment.

## Table of Contents
- [Quick Start](#quick-start)
- [Architecture Overview](#architecture-overview)
- [Prerequisites](#prerequisites)
- [Setup Instructions](#setup-instructions)
- [Using the Makefile](#using-the-makefile)
- [Development Workflow](#development-workflow)
- [Troubleshooting](#troubleshooting)
- [Environment Variables](#environment-variables)

## Quick Start

```bash
# 1. Start the PostgreSQL database
make dev-db-up

# 2. Start the PDS
make dev-up

# 3. View logs
make dev-logs

# 4. Check status
make dev-status

# 5. When done
make dev-down
```

## Architecture Overview

Coves uses a simplified single-database architecture with direct PDS firehose subscription:

```
┌─────────────────────────────────────────────┐
│      Coves Local Development Stack          │
├─────────────────────────────────────────────┤
│                                              │
│  ┌──────────────┐                           │
│  │  PDS         │                           │
│  │  :3001       │                           │
│  │              │                           │
│  │  Firehose───────────┐                    │
│  └──────────────┘      │                    │
│                        │                     │
│                        ▼                     │
│                ┌──────────────┐             │
│                │ Coves AppView│             │
│                │  (Go)        │             │
│                │  :8081       │             │
│                └──────┬───────┘             │
│                       │                      │
│                ┌──────▼───────┐             │
│                │  PostgreSQL  │             │
│                │  :5433       │             │
│                └──────────────┘             │
│                                              │
└─────────────────────────────────────────────┘

Your Production PDS (:3000) ← Runs independently
```

### Components

1. **PDS (Port 3001)** - Bluesky's Personal Data Server with:
   - User repositories and CAR files (stored in Docker volume)
   - Internal SQLite database for PDS metadata
   - Firehose WebSocket: `ws://localhost:3001/xrpc/com.atproto.sync.subscribeRepos`
2. **PostgreSQL (Port 5433)** - Database for Coves AppView data only
3. **Coves AppView (Port 8081)** - Your Go application that:
   - Subscribes directly to PDS firehose
   - Indexes Coves-specific data to PostgreSQL

**Key Points:**
- ✅ Ports chosen to avoid conflicts with production PDS on :3000
- ✅ PDS is self-contained with its own SQLite database and CAR storage
- ✅ PostgreSQL is only used by the Coves AppView for indexing
- ✅ AppView subscribes directly to PDS firehose (no relay needed)
- ✅ Simple, clean architecture for local development

## Prerequisites

- **Docker & Docker Compose** - For running containerized services
- **Go 1.22+** - For building the Coves AppView
- **PostgreSQL client** (optional) - For database inspection
- **Make** (optional but recommended) - For convenient commands

## Setup Instructions

### Step 1: Start the Database

The PostgreSQL database must be running first:

```bash
# Start the database
make dev-db-up

# Verify it's running
make dev-status
```

**Connection Details:**
- Host: `localhost`
- Port: `5433`
- Database: `coves_dev`
- User: `dev_user`
- Password: `dev_password`

### Step 2: Start the PDS

Start the Personal Data Server:

```bash
# Start PDS
make dev-up

# View logs (follows in real-time)
make dev-logs
```

Wait for health checks to pass (~10-30 seconds).

### Step 3: Verify Services

```bash
# Check PDS is running
make dev-status

# Test PDS health endpoint
curl http://localhost:3001/xrpc/_health

# Test PDS firehose endpoint (should get WebSocket upgrade response)
curl -i -N -H "Connection: Upgrade" -H "Upgrade: websocket" \
  http://localhost:3001/xrpc/com.atproto.sync.subscribeRepos
```

### Step 4: Run Coves AppView (When Ready)

When you have a Dockerfile for the AppView:

1. Uncomment the `appview` service in `docker-compose.dev.yml`
2. Restart the stack: `make dev-down && make dev-up`

Or run the AppView locally:

```bash
# Set environment variables
export DATABASE_URL="postgresql://dev_user:dev_password@localhost:5433/coves_dev?sslmode=disable"
export FIREHOSE_URL="ws://localhost:3001/xrpc/com.atproto.sync.subscribeRepos"
export PDS_URL="http://localhost:3001"
export PORT=8081

# Run the AppView
go run ./cmd/server
```

## Using the Makefile

The Makefile provides convenient commands for development. Run `make help` to see all available commands:

### General Commands

```bash
make help              # Show all available commands with descriptions
```

### Development Stack Commands

```bash
make dev-up            # Start PDS for local development
make dev-down          # Stop the stack
make dev-logs          # Tail logs from PDS
make dev-status        # Show status of containers
make dev-reset         # Nuclear option - remove all data and volumes
```

### Database Commands

```bash
make dev-db-up         # Start PostgreSQL database
make dev-db-down       # Stop PostgreSQL database
make dev-db-reset      # Reset database (delete all data)
make db-shell          # Open psql shell to the database
```

### Testing Commands

```bash
make test              # Run all tests (starts test DB, runs migrations, executes tests)
make test-db-reset     # Reset test database (clean slate)
make test-db-stop      # Stop test database
```

**See [TESTING_SUMMARY.md](../TESTING_SUMMARY.md) for complete testing documentation.**

### Workflow Commands

```bash
make fresh-start       # Complete fresh start (reset everything)
make quick-restart     # Quick restart (keeps data)
```

### Build Commands

```bash
make build             # Build the Coves server binary
make run               # Run the Coves server
make clean             # Clean build artifacts
```

### Utilities

```bash
make validate-lexicon  # Validate all Lexicon schemas
make docs              # Show documentation file locations
```

## Development Workflow

### Typical Development Session

```bash
# 1. Start fresh environment
make fresh-start

# 2. Work on code...

# 3. Restart services as needed
make quick-restart

# 4. View logs
make dev-logs

# 5. Run tests
make test

# 6. Clean up when done
make dev-down
```

### Testing Lexicon Changes

```bash
# 1. Edit Lexicon files in internal/atproto/lexicon/

# 2. Validate schemas
make validate-lexicon

# 3. Restart services to pick up changes
make quick-restart
```

### Database Inspection

```bash
# Open PostgreSQL shell
make db-shell

# Or use psql directly
PGPASSWORD=dev_password psql -h localhost -p 5433 -U dev_user -d coves_dev
```

### Viewing Logs

```bash
# Follow all logs
make dev-logs

# Or use docker-compose directly
docker-compose -f docker-compose.dev.yml logs -f pds
docker-compose -f docker-compose.dev.yml logs -f relay
```

## Troubleshooting

### Port Already in Use

**Problem:** Error binding to port 3000, 5433, etc.

**Solution:**
- The dev environment uses non-standard ports to avoid conflicts
- PDS: 3001 (not 3000)
- PostgreSQL: 5433 (not 5432)
- Relay: 2471 (not 2470)
- AppView: 8081 (not 8080)

If you still have conflicts, check what's using the port:

```bash
# Check what's using a port
lsof -i :3001
lsof -i :5433

# Kill the process
kill -9 <PID>
```

### Database Connection Failed

**Problem:** Services can't connect to PostgreSQL

**Solution:**

```bash
# Ensure database is running
make dev-db-up

# Check database logs
cd internal/db/local_dev_db_compose && docker-compose logs

# Verify connection manually
PGPASSWORD=dev_password psql -h localhost -p 5433 -U dev_user -d coves_dev
```

### PDS Health Check Failing

**Problem:** PDS container keeps restarting

**Solution:**

```bash
# Check PDS logs
docker-compose -f docker-compose.dev.yml logs pds

# Common issues:
# 1. Database not accessible - ensure DB is running
# 2. Invalid environment variables - check .env.dev
# 3. Port conflict - ensure port 3001 is free
```

### AppView Not Receiving Firehose Events

**Problem:** AppView isn't receiving events from PDS firehose

**Solution:**

```bash
# Check PDS logs for firehose activity
docker-compose -f docker-compose.dev.yml logs pds

# Verify firehose endpoint is accessible
curl -i -N -H "Connection: Upgrade" -H "Upgrade: websocket" \
  http://localhost:3001/xrpc/com.atproto.sync.subscribeRepos

# Check AppView is connecting to correct URL:
# FIREHOSE_URL=ws://localhost:3001/xrpc/com.atproto.sync.subscribeRepos
```

### Fresh Start Not Working

**Problem:** `make fresh-start` fails

**Solution:**

```bash
# Manually clean everything
docker-compose -f docker-compose.dev.yml down -v
cd internal/db/local_dev_db_compose && docker-compose down -v
docker volume prune -f
docker network prune -f

# Then start fresh
make dev-db-up
sleep 2
make dev-up
```

### Production PDS Interference

**Problem:** Dev environment conflicts with your production PDS

**Solution:**
- Dev PDS runs on port 3001 (production is 3000)
- Dev services use different handle domain (`.local.coves.dev`)
- They should not interfere unless you have custom networking

```bash
# Verify production PDS is still accessible
curl http://localhost:3000/xrpc/_health

# Verify dev PDS is separate
curl http://localhost:3001/xrpc/_health
```

## Environment Variables

All configuration is in [.env.dev](../.env.dev) - a single file for both development and testing:

### Development Database (Port 5433)
```bash
POSTGRES_HOST=localhost
POSTGRES_PORT=5433
POSTGRES_DB=coves_dev
POSTGRES_USER=dev_user
POSTGRES_PASSWORD=dev_password
```

### Test Database (Port 5434)
```bash
POSTGRES_TEST_DB=coves_test
POSTGRES_TEST_USER=test_user
POSTGRES_TEST_PASSWORD=test_password
POSTGRES_TEST_PORT=5434
```

### PDS Configuration
```bash
PDS_HOSTNAME=localhost
PDS_PORT=3001
PDS_JWT_SECRET=local-dev-jwt-secret-change-in-production
PDS_ADMIN_PASSWORD=admin
PDS_SERVICE_HANDLE_DOMAINS=.local.coves.dev
```

### AppView Configuration
```bash
APPVIEW_PORT=8081
FIREHOSE_URL=ws://localhost:3001/xrpc/com.atproto.sync.subscribeRepos
PDS_URL=http://localhost:3001
```

### Development Settings
```bash
ENV=development
LOG_LEVEL=debug
```

**No separate `.env.test` file needed!** All configuration (dev + test) is in `.env.dev`.

## Next Steps

1. **Build the Firehose Subscriber** - Create the AppView component that subscribes to the relay
2. **Define Custom Lexicons** - Create Coves-specific schemas in `internal/atproto/lexicon/social/coves/`
3. **Implement XRPC Handlers** - Build the API endpoints for Coves features
4. **Create Integration Tests** - Use Testcontainers to test the full stack

## Additional Resources

- [CLAUDE.md](../CLAUDE.md) - Build guidelines and security practices
- [ATPROTO_GUIDE.md](../ATPROTO_GUIDE.md) - Comprehensive atProto implementation guide
- [PROJECT_STRUCTURE.md](../PROJECT_STRUCTURE.md) - Project organization
- [PRD.md](../PRD.md) - Product requirements and roadmap

## Getting Help

- Check logs: `make dev-logs`
- View status: `make dev-status`
- Reset everything: `make fresh-start`
- Inspect database: `make db-shell`

For issues with atProto concepts, see [ATPROTO_GUIDE.md](../ATPROTO_GUIDE.md).

For build process questions, see [CLAUDE.md](../CLAUDE.md).
