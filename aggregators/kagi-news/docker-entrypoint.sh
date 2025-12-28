#!/bin/bash
set -e

echo "Starting Kagi News RSS Aggregator..."
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
echo "Cron schedule loaded from /etc/cron.d/kagi-aggregator"

# Start cron in the background
echo "Starting cron daemon..."
cron

# Optional: Run aggregator immediately on startup (for testing)
if [ "$RUN_ON_STARTUP" = "true" ]; then
    echo "Running aggregator immediately (RUN_ON_STARTUP=true)..."
    cd /app && python -m src.main
fi

echo "========================================="
echo "Kagi News Aggregator is running!"
echo "Cron schedule: Daily at 1 PM UTC"
echo "Logs will appear below:"
echo "========================================="
echo ""

# Execute the command passed to docker run (defaults to tail -f /var/log/cron.log)
exec "$@"
