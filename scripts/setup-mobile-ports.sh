#!/bin/bash
# Setup adb reverse port forwarding for mobile testing
# This allows the mobile app to access localhost services on the dev machine

set -e

# Colors
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

echo -e "${CYAN}📱 Setting up Android port forwarding for Coves mobile testing...${NC}"
echo ""

# Check if adb is available
if ! command -v adb &> /dev/null; then
    echo -e "${RED}✗ adb not found${NC}"
    echo "Install Android SDK Platform Tools: https://developer.android.com/studio/releases/platform-tools"
    exit 1
fi

# Check if device is connected
DEVICES=$(adb devices | grep -v "List" | grep "device$" | wc -l)
if [ "$DEVICES" -eq 0 ]; then
    echo -e "${RED}✗ No Android devices connected${NC}"
    echo "Connect a device via USB or start an emulator"
    exit 1
fi

echo -e "${YELLOW}Setting up port forwarding...${NC}"

# Forward ports from Android device to localhost
adb reverse tcp:3000 tcp:3001  # PDS (internal port in DID document)
adb reverse tcp:3001 tcp:3001  # PDS (external port)
adb reverse tcp:3002 tcp:3002  # PLC Directory
adb reverse tcp:8081 tcp:8081  # AppView

echo ""
echo -e "${GREEN}✅ Port forwarding configured successfully!${NC}"
echo ""
echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo -e "${CYAN}                   PORT FORWARDING                         ${NC}"
echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "${GREEN}PDS (3000):${NC}      localhost:3001 → device:3000 ${YELLOW}(DID document port)${NC}"
echo -e "${GREEN}PDS (3001):${NC}      localhost:3001 → device:3001"
echo -e "${GREEN}PLC (3002):${NC}      localhost:3002 → device:3002"
echo -e "${GREEN}AppView (8081):${NC}  localhost:8081 → device:8081"
echo ""
echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "${CYAN}📱 Next Steps:${NC}"
echo ""
echo -e "1. Mobile app is already configured for localhost (environment_config.dart)"
echo ""
echo -e "2. Run mobile app:"
echo -e "   ${YELLOW}cd /home/bretton/Code/coves-mobile${NC}"
echo -e "   ${YELLOW}flutter run --dart-define=ENVIRONMENT=local${NC}"
echo ""
echo -e "3. Login with:"
echo -e "   Handle:   ${CYAN}charlie.local.coves.dev${NC}"
echo -e "   Password: ${CYAN}charliepass123${NC}"
echo ""
echo -e "${YELLOW}💡 Note: Port forwarding persists until device disconnects or you run:${NC}"
echo -e "${YELLOW}    adb reverse --remove-all${NC}"
echo ""
