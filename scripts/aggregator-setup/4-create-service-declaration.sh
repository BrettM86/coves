#!/bin/bash

# Script: 4-create-service-declaration.sh
# Purpose: Create aggregator service declaration record
#
# This script writes a social.coves.aggregator.service record to your aggregator's repository.
# This record contains metadata about your aggregator (name, description, etc.) and will be
# indexed by Coves' Jetstream consumer into the aggregators table.

set -e

echo "================================================"
echo "Step 4: Create Service Declaration"
echo "================================================"
echo ""

# Load config if available
if [ -f "aggregator-config.env" ]; then
    source aggregator-config.env
    echo "✓ Loaded configuration from aggregator-config.env"
    echo "  DID:       $AGGREGATOR_DID"
    echo "  PDS URL:   $AGGREGATOR_PDS_URL"
    echo ""
else
    echo "Configuration file not found. Please run previous scripts first."
    exit 1
fi

# Validate required fields
if [ -z "$AGGREGATOR_ACCESS_JWT" ]; then
    echo "Error: AGGREGATOR_ACCESS_JWT not set. Please run 1-create-pds-account.sh first."
    exit 1
fi

echo "Enter aggregator metadata:"
echo ""

# Get metadata from user
read -p "Display Name (e.g., 'RSS News Aggregator'): " DISPLAY_NAME
read -p "Description: " DESCRIPTION
read -p "Source URL (e.g., 'https://github.com/yourname/aggregator'): " SOURCE_URL
read -p "Maintainer DID (your personal DID, optional): " MAINTAINER_DID

if [ -z "$DISPLAY_NAME" ]; then
    echo "Error: Display name is required"
    exit 1
fi

echo ""
echo "Creating service declaration record..."

# Build the service record
SERVICE_RECORD=$(cat <<EOF
{
    "\$type": "social.coves.aggregator.service",
    "did": "$AGGREGATOR_DID",
    "displayName": "$DISPLAY_NAME",
    "description": "$DESCRIPTION",
    "sourceUrl": "$SOURCE_URL",
    "maintainer": "$MAINTAINER_DID",
    "createdAt": "$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
}
EOF
)

# Call com.atproto.repo.createRecord
RESPONSE=$(curl -s -X POST "$AGGREGATOR_PDS_URL/xrpc/com.atproto.repo.createRecord" \
    -H "Authorization: Bearer $AGGREGATOR_ACCESS_JWT" \
    -H "Content-Type: application/json" \
    -d "{
        \"repo\": \"$AGGREGATOR_DID\",
        \"collection\": \"social.coves.aggregator.service\",
        \"rkey\": \"self\",
        \"record\": $SERVICE_RECORD
    }")

# Check if successful
if echo "$RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
    echo "✗ Failed to create service declaration:"
    echo "$RESPONSE" | jq '.'
    exit 1
fi

# Extract response
RECORD_URI=$(echo "$RESPONSE" | jq -r '.uri')
RECORD_CID=$(echo "$RESPONSE" | jq -r '.cid')

if [ -z "$RECORD_URI" ] || [ "$RECORD_URI" = "null" ]; then
    echo "✗ Error: Unexpected response format"
    echo "$RESPONSE" | jq '.'
    exit 1
fi

echo ""
echo "✓ Service declaration created successfully!"
echo ""
echo "=== Record Details ===="
echo "URI: $RECORD_URI"
echo "CID: $RECORD_CID"
echo "======================="
echo ""

# Save to config
echo "" >> aggregator-config.env
echo "SERVICE_DECLARATION_URI=\"$RECORD_URI\"" >> aggregator-config.env
echo "SERVICE_DECLARATION_CID=\"$RECORD_CID\"" >> aggregator-config.env

echo "✓ Updated aggregator-config.env"
echo ""
echo "================================================"
echo "Setup Complete!"
echo "================================================"
echo ""
echo "Your aggregator is now registered with Coves!"
echo ""
echo "Next steps:"
echo "1. Wait a few seconds for Jetstream to index your service declaration"
echo "2. Verify your aggregator appears in the aggregators list"
echo "3. Community moderators can now authorize your aggregator"
echo "4. Once authorized, you can start posting to communities"
echo ""
echo "To test posting, use the Coves XRPC endpoint:"
echo "  POST $COVES_INSTANCE_URL/xrpc/social.coves.community.post.create"
echo ""
echo "See docs/aggregators/SETUP_GUIDE.md for more information"
