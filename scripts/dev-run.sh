#!/bin/bash
# Development server runner - loads .env.dev before starting

set -a  # automatically export all variables
source .env.dev
set +a

echo "🚀 Starting Coves server in DEV mode..."
echo "   IS_DEV_ENV: $IS_DEV_ENV"
echo "   PLC_DIRECTORY_URL: $PLC_DIRECTORY_URL"
echo "   JETSTREAM_URL: $JETSTREAM_URL"
echo ""

go run ./cmd/server
