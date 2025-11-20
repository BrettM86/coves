#!/bin/bash
# Coves Deployment Script
# Usage: ./scripts/deploy.sh [service]
#
# Examples:
#   ./scripts/deploy.sh           # Deploy all services
#   ./scripts/deploy.sh appview   # Deploy only AppView
#   ./scripts/deploy.sh --pull    # Pull from git first, then deploy

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
COMPOSE_FILE="$PROJECT_DIR/docker-compose.prod.yml"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log() {
    echo -e "${GREEN}[DEPLOY]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

# Parse arguments
PULL_GIT=false
SERVICE=""

for arg in "$@"; do
    case $arg in
        --pull)
            PULL_GIT=true
            ;;
        *)
            SERVICE="$arg"
            ;;
    esac
done

cd "$PROJECT_DIR"

# Load environment variables
if [ ! -f ".env.prod" ]; then
    error ".env.prod not found! Copy from .env.prod.example and configure secrets."
fi

log "Loading environment from .env.prod..."
set -a
source .env.prod
set +a

# Optional: Pull from git
if [ "$PULL_GIT" = true ]; then
    log "Pulling latest code from git..."
    git fetch origin
    git pull origin main
fi

# Check database connectivity before deployment
log "Checking database connectivity..."
if docker compose -f "$COMPOSE_FILE" exec -T postgres pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB" > /dev/null 2>&1; then
    log "Database is ready"
else
    warn "Database not ready yet - it will start with the deployment"
fi

# Build and deploy
if [ -n "$SERVICE" ]; then
    log "Building $SERVICE..."
    docker compose -f "$COMPOSE_FILE" build --no-cache "$SERVICE"

    log "Deploying $SERVICE..."
    docker compose -f "$COMPOSE_FILE" up -d "$SERVICE"
else
    log "Building all services..."
    docker compose -f "$COMPOSE_FILE" build --no-cache

    log "Deploying all services..."
    docker compose -f "$COMPOSE_FILE" up -d
fi

# Health check
log "Waiting for services to be healthy..."
sleep 10

# Wait for database to be ready before running migrations
log "Waiting for database..."
for i in {1..30}; do
    if docker compose -f "$COMPOSE_FILE" exec -T postgres pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB" > /dev/null 2>&1; then
        break
    fi
    sleep 1
done

# Run database migrations
# The AppView runs migrations on startup, but we can also trigger them explicitly
log "Running database migrations..."
if docker compose -f "$COMPOSE_FILE" exec -T appview /app/coves-server migrate 2>/dev/null; then
    log "✅ Migrations completed"
else
    warn "⚠️  Migration command not available or failed - AppView will run migrations on startup"
fi

# Check AppView health
if docker compose -f "$COMPOSE_FILE" exec -T appview wget --spider -q http://localhost:8080/xrpc/_health 2>/dev/null; then
    log "✅ AppView is healthy"
else
    warn "⚠️  AppView health check failed - check logs with: docker compose -f docker-compose.prod.yml logs appview"
fi

# Check PDS health
if docker compose -f "$COMPOSE_FILE" exec -T pds wget --spider -q http://localhost:3000/xrpc/_health 2>/dev/null; then
    log "✅ PDS is healthy"
else
    warn "⚠️  PDS health check failed - check logs with: docker compose -f docker-compose.prod.yml logs pds"
fi

log "Deployment complete!"
log ""
log "Useful commands:"
log "  View logs:     docker compose -f docker-compose.prod.yml logs -f"
log "  Check status:  docker compose -f docker-compose.prod.yml ps"
log "  Rollback:      docker compose -f docker-compose.prod.yml down && git checkout HEAD~1 && ./scripts/deploy.sh"
