#!/bin/bash

# Script: 3-register-with-coves.sh
# Purpose: Register your aggregator with a Coves instance
#
# This script calls the social.coves.aggregator.register XRPC endpoint
# to register your aggregator DID with the Coves instance.

set -e

echo "================================================"
echo "Step 3: Register with Coves Instance"
echo "================================================"
echo ""

# Load config if available
if [ -f "aggregator-config.env" ]; then
    source aggregator-config.env
    echo "✓ Loaded configuration from aggregator-config.env"
    echo "  DID:    $AGGREGATOR_DID"
    echo "  Domain: $AGGREGATOR_DOMAIN"
    echo ""
else
    echo "Configuration file not found. Please run previous scripts first."
    exit 1
fi

# Validate domain is set
if [ -z "$AGGREGATOR_DOMAIN" ]; then
    echo "Error: AGGREGATOR_DOMAIN not set. Please run 2-setup-wellknown.sh first."
    exit 1
fi

# Get Coves instance URL
read -p "Enter Coves instance URL (default: https://api.coves.social): " COVES_URL
COVES_URL=${COVES_URL:-https://api.coves.social}

echo ""
echo "Verifying .well-known/atproto-did is accessible..."

# Verify .well-known is accessible
WELLKNOWN_URL="https://$AGGREGATOR_DOMAIN/.well-known/atproto-did"
WELLKNOWN_CONTENT=$(curl -s "$WELLKNOWN_URL" || echo "ERROR")

if [ "$WELLKNOWN_CONTENT" = "ERROR" ]; then
    echo "✗ Error: Could not access $WELLKNOWN_URL"
    echo "  Please ensure the file is uploaded and accessible."
    exit 1
elif [ "$WELLKNOWN_CONTENT" != "$AGGREGATOR_DID" ]; then
    echo "✗ Error: .well-known/atproto-did contains wrong DID"
    echo "  Expected: $AGGREGATOR_DID"
    echo "  Got:      $WELLKNOWN_CONTENT"
    exit 1
fi

echo "✓ .well-known/atproto-did is correctly configured"
echo ""

echo "Registering with $COVES_URL..."

# Call registration endpoint
RESPONSE=$(curl -s -X POST "$COVES_URL/xrpc/social.coves.aggregator.register" \
    -H "Content-Type: application/json" \
    -d "{
        \"did\": \"$AGGREGATOR_DID\",
        \"domain\": \"$AGGREGATOR_DOMAIN\"
    }")

# Check if successful
if echo "$RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
    echo "✗ Registration failed:"
    echo "$RESPONSE" | jq '.'
    exit 1
fi

# Extract response
REGISTERED_DID=$(echo "$RESPONSE" | jq -r '.did')
REGISTERED_HANDLE=$(echo "$RESPONSE" | jq -r '.handle')
MESSAGE=$(echo "$RESPONSE" | jq -r '.message')

if [ -z "$REGISTERED_DID" ] || [ "$REGISTERED_DID" = "null" ]; then
    echo "✗ Error: Unexpected response format"
    echo "$RESPONSE" | jq '.'
    exit 1
fi

echo ""
echo "✓ Registration successful!"
echo ""
echo "=== Registration Details ===="
echo "DID:     $REGISTERED_DID"
echo "Handle:  $REGISTERED_HANDLE"
echo "Message: $MESSAGE"
echo "============================="
echo ""

# Save Coves URL to config
echo "" >> aggregator-config.env
echo "COVES_INSTANCE_URL=\"$COVES_URL\"" >> aggregator-config.env

echo "✓ Updated aggregator-config.env with Coves instance URL"
echo ""
echo "Next step: Run ./4-create-service-declaration.sh"
