#!/bin/bash
set -e

echo "Starting Bluesky Topics Aggregator..."
echo "====================================="

# Load environment variables if .env file exists
if [ -f /app/.env ]; then
    echo "Loading environment variables from .env"
    set -a
    # shellcheck disable=SC1091
    . /app/.env
    set +a
fi

# Validate required environment variables. Never print any part of a key.
if [ -z "$ANTHROPIC_API_KEY" ]; then
    echo "ERROR: ANTHROPIC_API_KEY is required (the classifier API key for an Anthropic-compatible endpoint, e.g. OpenRouter)"
    exit 1
fi
if [ "$DRY_RUN" != "1" ] && [ "$DRY_RUN" != "true" ] && [ -z "$COVES_API_KEY" ]; then
    echo "ERROR: COVES_API_KEY is required unless DRY_RUN=1"
    exit 1
fi

echo "Cron schedule: every hour"

# Cron runs in a separate environment and does not inherit container env vars.
echo "Exporting environment variables for cron..."
printenv | grep -E '^(COVES_|ANTHROPIC_|BLUESKY_|DRY_RUN=|CONFIG_PATH=|STATE_FILE=|PATH=)' > /etc/environment

echo "Starting cron daemon..."
cron

if [ "$RUN_ON_STARTUP" = "true" ]; then
    echo "Running aggregator immediately (RUN_ON_STARTUP=true)..."
    cd /app && python -m src.main
fi

echo "====================================="
echo "Bluesky Topics Aggregator is running"
echo "====================================="
echo ""

exec "$@"
