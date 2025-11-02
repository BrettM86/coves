#!/bin/bash
# Automated ngrok tunnel starter for mobile testing
# Starts 3 ngrok tunnels and captures their HTTPS URLs

set -e

# Colors
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${CYAN}🚀 Starting ngrok tunnels for Coves mobile testing...${NC}"
echo ""

# Kill any existing ngrok processes
pkill -f "ngrok http" || true
sleep 2

# Start ngrok tunnels using separate processes (simpler, works with any config version)
echo -e "${YELLOW}Starting PDS tunnel (port 3001)...${NC}"
ngrok http 3001 --log=stdout > /tmp/ngrok-pds.log 2>&1 &
sleep 1

echo -e "${YELLOW}Starting PLC tunnel (port 3002)...${NC}"
ngrok http 3002 --log=stdout > /tmp/ngrok-plc.log 2>&1 &
sleep 1

echo -e "${YELLOW}Starting AppView tunnel (port 8081)...${NC}"
ngrok http 8081 --log=stdout > /tmp/ngrok-appview.log 2>&1 &

# Get all PIDs
PIDS=$(pgrep -f "ngrok http")
NGROK_PID=$PIDS

# Save PID for cleanup
echo "$NGROK_PID" > /tmp/ngrok-pids.txt

# Wait for ngrok to initialize
echo ""
echo -e "${YELLOW}Waiting for tunnels to initialize...${NC}"
sleep 7

# Fetch URLs from ngrok API (single API at port 4040)
echo ""
echo -e "${GREEN}✅ Tunnels started successfully!${NC}"
echo ""
echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo -e "${CYAN}                   NGROK TUNNEL URLS                       ${NC}"
echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo ""

# Get all tunnel info
TUNNELS=$(curl -s http://localhost:4040/api/tunnels 2>/dev/null || echo "")

# Extract URLs by matching port in config.addr
PDS_URL=$(echo "$TUNNELS" | jq -r '.tunnels[] | select(.config.addr | contains("3001")) | select(.proto=="https") | .public_url' 2>/dev/null | head -1)
PLC_URL=$(echo "$TUNNELS" | jq -r '.tunnels[] | select(.config.addr | contains("3002")) | select(.proto=="https") | .public_url' 2>/dev/null | head -1)
APPVIEW_URL=$(echo "$TUNNELS" | jq -r '.tunnels[] | select(.config.addr | contains("8081")) | select(.proto=="https") | .public_url' 2>/dev/null | head -1)

# Fallback if jq filtering fails - just get first 3 HTTPS URLs
if [ -z "$PDS_URL" ] || [ -z "$PLC_URL" ] || [ -z "$APPVIEW_URL" ]; then
    echo -e "${YELLOW}⚠️  Port-based matching failed, using fallback...${NC}"
    URLS=($(echo "$TUNNELS" | jq -r '.tunnels[] | select(.proto=="https") | .public_url' 2>/dev/null))
    PDS_URL=${URLS[0]:-ERROR}
    PLC_URL=${URLS[1]:-ERROR}
    APPVIEW_URL=${URLS[2]:-ERROR}
fi

echo -e "${GREEN}PDS (3001):${NC}      $PDS_URL"
echo -e "${GREEN}PLC (3002):${NC}      $PLC_URL"
echo -e "${GREEN}AppView (8081):${NC}  $APPVIEW_URL"

echo ""
echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo ""

# Check if any URLs failed
if [[ "$PDS_URL" == "ERROR" ]] || [[ "$PLC_URL" == "ERROR" ]] || [[ "$APPVIEW_URL" == "ERROR" ]]; then
    echo -e "${YELLOW}⚠️  Some tunnels failed to start. Check logs:${NC}"
    echo "  tail -f /tmp/ngrok-pds.log"
    echo "  tail -f /tmp/ngrok-plc.log"
    echo "  tail -f /tmp/ngrok-appview.log"
    exit 1
fi

# Extract clean URLs (remove https://)
PDS_CLEAN=$(echo $PDS_URL | sed 's|https://||')
PLC_CLEAN=$(echo $PLC_URL | sed 's|https://||')
APPVIEW_CLEAN=$(echo $APPVIEW_URL | sed 's|https://||')

echo -e "${CYAN}📱 Next Steps:${NC}"
echo ""
echo -e "1. Update ${YELLOW}coves-mobile/lib/config/environment_config.dart${NC}:"
echo ""
echo -e "${GREEN}static const local = EnvironmentConfig(${NC}"
echo -e "${GREEN}  environment: Environment.local,${NC}"
echo -e "${GREEN}  apiUrl: '$APPVIEW_URL',${NC}"
echo -e "${GREEN}  handleResolverUrl: '$PDS_URL/xrpc/com.atproto.identity.resolveHandle',${NC}"
echo -e "${GREEN}  plcDirectoryUrl: '$PLC_URL',${NC}"
echo -e "${GREEN});${NC}"
echo ""
echo -e "2. Run mobile app:"
echo -e "   ${YELLOW}cd /home/bretton/Code/coves-mobile${NC}"
echo -e "   ${YELLOW}flutter run --dart-define=ENVIRONMENT=local${NC}"
echo ""
echo -e "3. Login with:"
echo -e "   Handle:   ${CYAN}bob.local.coves.dev${NC}"
echo -e "   Password: ${CYAN}bobpass123${NC}"
echo ""
echo -e "${YELLOW}💡 Tip: Leave this terminal open. Press Ctrl+C to stop tunnels.${NC}"
echo -e "${YELLOW}    Or run: make ngrok-down${NC}"
echo ""

# Keep script running (can be killed with Ctrl+C or make ngrok-down)
wait
