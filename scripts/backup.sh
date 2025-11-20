#!/bin/bash
# Coves Database Backup Script
# Usage: ./scripts/backup.sh
#
# Creates timestamped PostgreSQL backups in ./backups/
# Retention: Keeps last 30 days of backups

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
BACKUP_DIR="$PROJECT_DIR/backups"
COMPOSE_FILE="$PROJECT_DIR/docker-compose.prod.yml"

# Load environment
set -a
source "$PROJECT_DIR/.env.prod"
set +a

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log() { echo -e "${GREEN}[BACKUP]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }

# Create backup directory
mkdir -p "$BACKUP_DIR"

# Generate timestamp
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
BACKUP_FILE="$BACKUP_DIR/coves_${TIMESTAMP}.sql.gz"

log "Starting backup..."

# Run pg_dump inside container
docker compose -f "$COMPOSE_FILE" exec -T postgres \
    pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists \
    | gzip > "$BACKUP_FILE"

# Get file size
SIZE=$(du -h "$BACKUP_FILE" | cut -f1)

log "✅ Backup complete: $BACKUP_FILE ($SIZE)"

# Cleanup old backups (keep last 30 days)
log "Cleaning up backups older than 30 days..."
find "$BACKUP_DIR" -name "coves_*.sql.gz" -mtime +30 -delete

# List recent backups
log ""
log "Recent backups:"
ls -lh "$BACKUP_DIR"/*.sql.gz 2>/dev/null | tail -5

log ""
log "To restore: gunzip -c $BACKUP_FILE | docker compose -f docker-compose.prod.yml exec -T postgres psql -U $POSTGRES_USER -d $POSTGRES_DB"
