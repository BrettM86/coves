#!/bin/bash
#
# Step 5: Create API Key for Aggregator
#
# This script guides you through generating an API key for your aggregator.
# API keys are used for authentication instead of PDS JWTs.
#
# Prerequisites:
# - Completed steps 1-4 (PDS account, .well-known, Coves registration, service declaration)
# - Aggregator indexed by Coves (check: curl https://coves.social/xrpc/social.coves.aggregator.get?did=YOUR_DID)
#
# Usage: ./5-create-api-key.sh
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}╔════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║         Coves Aggregator - Step 5: Create API Key         ║${NC}"
echo -e "${BLUE}╚════════════════════════════════════════════════════════════╝${NC}"
echo

# Load existing configuration
CONFIG_FILE="aggregator-config.env"
if [ -f "$CONFIG_FILE" ]; then
    echo -e "${GREEN}✓${NC} Loading existing configuration from $CONFIG_FILE"
    source "$CONFIG_FILE"
else
    echo -e "${YELLOW}⚠${NC} No $CONFIG_FILE found. Please run steps 1-4 first."
    echo
    read -p "Enter your Coves instance URL [https://coves.social]: " COVES_INSTANCE_URL
    COVES_INSTANCE_URL=${COVES_INSTANCE_URL:-https://coves.social}
fi

echo
echo -e "${YELLOW}═══════════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW} API Key Generation Process${NC}"
echo -e "${YELLOW}═══════════════════════════════════════════════════════════${NC}"
echo
echo "API keys allow your aggregator to authenticate without managing"
echo "OAuth token refresh. The key is shown ONCE and cannot be retrieved later."
echo
echo -e "${BLUE}Steps:${NC}"
echo "1. Complete OAuth login in your browser"
echo "2. Call the createApiKey endpoint"
echo "3. Save the key securely"
echo

# Check if aggregator is indexed
echo -e "${BLUE}Checking if aggregator is indexed...${NC}"
if [ -n "$AGGREGATOR_DID" ]; then
    AGGREGATOR_CHECK=$(curl -s "${COVES_INSTANCE_URL}/xrpc/social.coves.aggregator.get?did=${AGGREGATOR_DID}" 2>/dev/null || echo "error")
    if echo "$AGGREGATOR_CHECK" | grep -q "error"; then
        echo -e "${YELLOW}⚠${NC} Could not verify aggregator status. Proceeding anyway..."
    else
        echo -e "${GREEN}✓${NC} Aggregator found in Coves instance"
    fi
fi

echo
echo -e "${YELLOW}═══════════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW} Step 5.1: OAuth Login${NC}"
echo -e "${YELLOW}═══════════════════════════════════════════════════════════${NC}"
echo
echo "Open this URL in your browser to authenticate:"
echo
AGGREGATOR_HANDLE=${AGGREGATOR_HANDLE:-"your-aggregator.example.com"}
echo -e "  ${BLUE}${COVES_INSTANCE_URL}/oauth/login?handle=${AGGREGATOR_HANDLE}${NC}"
echo
echo "This will:"
echo "  1. Redirect you to your PDS for authentication"
echo "  2. Return you to Coves with an OAuth session"
echo
echo -e "${YELLOW}After authenticating, press Enter to continue...${NC}"
read

echo
echo -e "${YELLOW}═══════════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW} Step 5.2: Create API Key${NC}"
echo -e "${YELLOW}═══════════════════════════════════════════════════════════${NC}"
echo
echo "In your browser's Developer Console (F12 → Console), run:"
echo
echo -e "${GREEN}fetch('/xrpc/social.coves.aggregator.createApiKey', {"
echo "  method: 'POST',"
echo "  credentials: 'include'"
echo "})"
echo ".then(r => r.json())"
echo -e ".then(data => console.log('API Key:', data.key))${NC}"
echo
echo "This will return your API key. It looks like:"
echo "  ckapi_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
echo
echo -e "${RED}⚠ IMPORTANT: Save this key immediately! It cannot be retrieved again.${NC}"
echo
read -p "Enter the API key you received: " API_KEY

# Validate API key format
if [[ ! $API_KEY =~ ^ckapi_[a-f0-9]{64}$ ]]; then
    echo -e "${RED}✗ Invalid API key format. Expected: ckapi_ followed by 64 hex characters${NC}"
    echo "  Example: ckapi_dcbdec0a0d1b3c440125547d21fe582bbf1587d2dcd364c56ad285af841cc934"
    exit 1
fi

echo -e "${GREEN}✓${NC} API key format valid"

# Save to config
echo
echo "COVES_API_KEY=\"$API_KEY\"" >> "$CONFIG_FILE"
echo -e "${GREEN}✓${NC} API key saved to $CONFIG_FILE"

echo
echo -e "${YELLOW}═══════════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW} Step 5.3: Update Your .env File${NC}"
echo -e "${YELLOW}═══════════════════════════════════════════════════════════${NC}"
echo
echo "Update your aggregator's .env file with:"
echo
echo -e "${GREEN}COVES_API_KEY=${API_KEY}${NC}"
echo -e "${GREEN}COVES_API_URL=${COVES_INSTANCE_URL}${NC}"
echo
echo "You can remove the old AGGREGATOR_HANDLE and AGGREGATOR_PASSWORD variables."
echo

echo
echo -e "${GREEN}╔════════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║                    Setup Complete!                         ║${NC}"
echo -e "${GREEN}╚════════════════════════════════════════════════════════════╝${NC}"
echo
echo "Your aggregator is now configured with API key authentication."
echo
echo "Next steps:"
echo "  1. Update your aggregator's .env file with COVES_API_KEY"
echo "  2. Rebuild your Docker container: docker compose build --no-cache"
echo "  3. Start the aggregator: docker compose up -d"
echo "  4. Check logs: docker compose logs -f"
echo
echo -e "${YELLOW}Security Reminders:${NC}"
echo "  - Never commit your API key to version control"
echo "  - Store it securely (environment variables or secrets manager)"
echo "  - Rotate periodically by generating a new key (revokes the old one)"
echo
