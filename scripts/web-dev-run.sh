#!/bin/bash
# Web frontend development server runner
# Starts both Coves backend AND Vite frontend, uses Caddy proxy on port 8080
#
# Usage: make run-web (or ./scripts/web-dev-run.sh)

set -a  # automatically export all variables
source .env.dev
set +a

# Override for web frontend development
# OAuth callback needs to use the proxy port (8080) for cookie sharing
# MUST use 127.0.0.1 (not localhost) per RFC 8252 - PDS rejects localhost in redirect_uri
export APPVIEW_PUBLIC_URL="http://127.0.0.1:8080"

# Resolve paths relative to this script (no hardcoded absolute paths)
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
COVES_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Frontend location (override with KELP_DIR env var)
KELP_DIR="${KELP_DIR:-$COVES_DIR/../kelp}"

# Cleanup function
cleanup() {
    echo ""
    echo "🛑 Shutting down..."
    # Kill the Vite process if it's running
    if [ -n "$VITE_PID" ]; then
        kill $VITE_PID 2>/dev/null
        wait $VITE_PID 2>/dev/null
    fi
    exit 0
}

# Set up trap for clean shutdown
trap cleanup SIGINT SIGTERM

echo "🌐 Starting Coves WEB FRONTEND development environment..."
echo ""
echo "   IS_DEV_ENV: $IS_DEV_ENV"
echo "   PLC_DIRECTORY_URL: $PLC_DIRECTORY_URL"
echo "   APPVIEW_PUBLIC_URL: $APPVIEW_PUBLIC_URL (via Caddy proxy)"
echo "   PDS_URL: $PDS_URL"
echo "   Build tags: dev"
echo ""

# Check if kelp directory exists
if [ ! -d "$KELP_DIR" ]; then
    echo "❌ Frontend directory not found: $KELP_DIR"
    exit 1
fi

# Start Vite in background
echo "🚀 Starting Vite frontend (kelp) on :5173..."
cd "$KELP_DIR" && npm run dev &
VITE_PID=$!

# Give Vite a moment to start
sleep 2

# Return to Coves directory
cd "$COVES_DIR"

echo ""
echo "🚀 Starting Coves backend on :8081..."
echo ""
echo "   ⚠️  Make sure Caddy proxy is running: make web-proxy"
echo "   📍 Access your app at: http://localhost:8080"
echo ""
echo "   Press Ctrl+C to stop both services"
echo ""

# Run Go server (foreground - will block)
go run -tags dev ./cmd/server

# If Go server exits, clean up Vite
cleanup
