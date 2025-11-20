#!/bin/bash
# Coves Production Setup Script
# Run this once on a fresh server to set up everything
#
# Prerequisites:
#   - Docker and docker-compose installed
#   - Git installed
#   - .env.prod file configured

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log() { echo -e "${GREEN}[SETUP]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

cd "$PROJECT_DIR"

# Check prerequisites
log "Checking prerequisites..."

if ! command -v docker &> /dev/null; then
    error "Docker is not installed. Install with: curl -fsSL https://get.docker.com | sh"
fi

if ! docker compose version &> /dev/null; then
    error "docker compose is not available. Install with: apt install docker-compose-plugin"
fi

# Check for .env.prod
if [ ! -f ".env.prod" ]; then
    error ".env.prod not found! Copy from .env.prod.example and configure secrets."
fi

# Load environment
set -a
source .env.prod
set +a

# Create required directories
log "Creating directories..."
mkdir -p backups
mkdir -p static/.well-known

# Check for did.json
if [ ! -f "static/.well-known/did.json" ]; then
    warn "static/.well-known/did.json not found!"
    warn "Run ./scripts/generate-did-keys.sh to create it."
fi

# Note: Caddy logs are written to Docker volume (caddy-data)
# If you need host-accessible logs, uncomment and run as root:
# mkdir -p /var/log/caddy && chown 1000:1000 /var/log/caddy

# Pull Docker images
log "Pulling Docker images..."
docker compose -f docker-compose.prod.yml pull postgres pds caddy

# Build AppView
log "Building AppView..."
docker compose -f docker-compose.prod.yml build appview

# Start services
log "Starting services..."
docker compose -f docker-compose.prod.yml up -d

# Wait for PostgreSQL
log "Waiting for PostgreSQL to be ready..."
until docker compose -f docker-compose.prod.yml exec -T postgres pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB" > /dev/null 2>&1; do
    sleep 2
done
log "PostgreSQL is ready!"

# Run migrations
log "Running database migrations..."
# The AppView runs migrations on startup, but you can also run them manually:
# docker compose -f docker-compose.prod.yml exec appview /app/coves-server migrate

# Final status
log ""
log "============================================"
log "  Coves Production Setup Complete!"
log "============================================"
log ""
log "Services running:"
docker compose -f docker-compose.prod.yml ps
log ""
log "Next steps:"
log "  1. Configure DNS for coves.social and coves.me"
log "  2. Run ./scripts/generate-did-keys.sh to create DID keys"
log "  3. Test health endpoints:"
log "     curl https://coves.social/xrpc/_health"
log "     curl https://coves.me/xrpc/_health"
log ""
log "Useful commands:"
log "  View logs:     docker compose -f docker-compose.prod.yml logs -f"
log "  Deploy update: ./scripts/deploy.sh appview"
log "  Backup DB:     ./scripts/backup.sh"
