#!/bin/bash

# Script: 1-create-pds-account.sh
# Purpose: Create a PDS account for your aggregator
#
# This script helps you create an account on a PDS (Personal Data Server).
# The PDS will automatically create a DID:PLC for you.

set -e

echo "================================================"
echo "Step 1: Create PDS Account for Your Aggregator"
echo "================================================"
echo ""

# Get PDS URL
read -p "Enter PDS URL (default: https://bsky.social): " PDS_URL
PDS_URL=${PDS_URL:-https://bsky.social}

# Get credentials
read -p "Enter desired handle (e.g., mynewsbot.bsky.social): " HANDLE
read -p "Enter email: " EMAIL
read -sp "Enter password: " PASSWORD
echo ""

# Validate inputs
if [ -z "$HANDLE" ] || [ -z "$EMAIL" ] || [ -z "$PASSWORD" ]; then
    echo "Error: All fields are required"
    exit 1
fi

echo ""
echo "Creating account on $PDS_URL..."

# Create account via com.atproto.server.createAccount
RESPONSE=$(curl -s -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
    -H "Content-Type: application/json" \
    -d "{
        \"handle\": \"$HANDLE\",
        \"email\": \"$EMAIL\",
        \"password\": \"$PASSWORD\"
    }")

# Check if successful
if echo "$RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
    echo "Error creating account:"
    echo "$RESPONSE" | jq '.'
    exit 1
fi

# Extract DID and access token
DID=$(echo "$RESPONSE" | jq -r '.did')
ACCESS_JWT=$(echo "$RESPONSE" | jq -r '.accessJwt')
REFRESH_JWT=$(echo "$RESPONSE" | jq -r '.refreshJwt')

if [ -z "$DID" ] || [ "$DID" = "null" ]; then
    echo "Error: Failed to extract DID from response"
    echo "$RESPONSE" | jq '.'
    exit 1
fi

echo ""
echo "✓ Account created successfully!"
echo ""
echo "=== Save these credentials ===="
echo "DID:          $DID"
echo "Handle:       $HANDLE"
echo "PDS URL:      $PDS_URL"
echo "Email:        $EMAIL"
echo "Password:     [hidden]"
echo "Access JWT:   $ACCESS_JWT"
echo "Refresh JWT:  $REFRESH_JWT"
echo "==============================="
echo ""

# Save to config file
CONFIG_FILE="aggregator-config.env"
cat > "$CONFIG_FILE" <<EOF
# Aggregator Account Configuration
# Generated: $(date)

AGGREGATOR_DID="$DID"
AGGREGATOR_HANDLE="$HANDLE"
AGGREGATOR_PDS_URL="$PDS_URL"
AGGREGATOR_EMAIL="$EMAIL"
AGGREGATOR_PASSWORD="$PASSWORD"
AGGREGATOR_ACCESS_JWT="$ACCESS_JWT"
AGGREGATOR_REFRESH_JWT="$REFRESH_JWT"
EOF

echo "✓ Configuration saved to $CONFIG_FILE"
echo ""
echo "IMPORTANT: Keep this file secure! It contains your credentials."
echo ""
echo "Next step: Run ./2-setup-wellknown.sh"
