#!/bin/bash
set -e

echo "Starting Reddit Highlights Aggregator..."
echo "========================================="

# Load environment variables if .env file exists
if [ -f /app/.env ]; then
    echo "Loading environment variables from .env"
    export $(grep -v '^#' /app/.env | xargs)
fi

# Validate required environment variables
if [ -z "$COVES_API_KEY" ]; then
    echo "ERROR: Missing required environment variable!"
    echo "Please set COVES_API_KEY (format: ckapi_...)"
    exit 1
fi

echo "API Key prefix: ${COVES_API_KEY:0:12}..."
echo "Cron schedule: Every 30 minutes (with 0-10 min jitter)"

# Export environment variables for cron
# Cron runs in a separate environment and doesn't inherit container env vars
echo "Exporting environment variables for cron..."
printenv | grep -E '^(COVES_|SKIP_|PATH=)' > /etc/environment

# Start cron in the background
echo "Starting cron daemon..."
cron

# Optional: Run aggregator immediately on startup (for testing)
if [ "$RUN_ON_STARTUP" = "true" ]; then
    echo "Running aggregator immediately (RUN_ON_STARTUP=true)..."
    # Skip jitter for immediate run
    cd /app && SKIP_JITTER=true python -m src.main
fi

echo "========================================="
echo "Reddit Highlights Aggregator is running!"
echo "Polling r/nba for streamable links"
echo "Logs will appear below:"
echo "========================================="
echo ""

# Execute the command passed to docker run (defaults to tail -f /var/log/cron.log)
exec "$@"
