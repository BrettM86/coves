#!/bin/bash
# Stop all ngrok tunnels

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${YELLOW}Stopping ngrok tunnels...${NC}"

# Kill processes by PID if available
if [ -f /tmp/ngrok-pids.txt ]; then
    PIDS=$(cat /tmp/ngrok-pids.txt)
    for pid in $PIDS; do
        kill $pid 2>/dev/null || true
    done
    rm /tmp/ngrok-pids.txt
fi

# Fallback: kill all ngrok processes
pkill -f "ngrok http" || true

# Clean up logs
rm -f /tmp/ngrok-*.log

echo -e "${GREEN}✓ ngrok tunnels stopped${NC}"
