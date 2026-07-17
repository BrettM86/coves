#!/bin/bash
# Development server runner - loads .env.dev before starting
# Uses -tags dev to include dev-only code (localhost OAuth resolvers, etc.)

set -a  # automatically export all variables
source .env.dev
set +a

echo "🚀 Starting Coves server in DEV mode..."
echo "   IS_DEV_ENV: $IS_DEV_ENV"
echo "   PLC_DIRECTORY_URL: $PLC_DIRECTORY_URL"
echo "   JETSTREAM_FEEDS: $JETSTREAM_FEEDS"
echo "   APPVIEW_PUBLIC_URL: $APPVIEW_PUBLIC_URL"
echo "   PDS_URL: $PDS_URL"
echo "   Build tags: dev"
echo ""

go run -tags dev ./cmd/server
