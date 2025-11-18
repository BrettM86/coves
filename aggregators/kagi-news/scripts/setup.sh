#!/bin/bash

# Script: setup-kagi-aggregator.sh
# Purpose: Complete setup script for Kagi News RSS aggregator
#
# This is a reference implementation showing automated setup for a specific aggregator.
# Other aggregator developers can use this as a template.

set -e

echo "================================================"
echo "Kagi News RSS Aggregator - Automated Setup"
echo "================================================"
echo ""

# Configuration for Kagi aggregator
AGGREGATOR_NAME="kagi-news-bot"
DISPLAY_NAME="Kagi News RSS"
DESCRIPTION="Aggregates tech news from Kagi RSS feeds and posts to relevant communities"
SOURCE_URL="https://github.com/coves-social/kagi-aggregator"

# Check if config already exists
if [ -f "kagi-aggregator-config.env" ]; then
    echo "Configuration file already exists. Loading existing configuration..."
    source kagi-aggregator-config.env
    SKIP_ACCOUNT_CREATION=true
else
    SKIP_ACCOUNT_CREATION=false
fi

# Get runtime configuration
if [ "$SKIP_ACCOUNT_CREATION" = false ]; then
    read -p "Enter PDS URL (default: https://bsky.social): " PDS_URL
    PDS_URL=${PDS_URL:-https://bsky.social}

    read -p "Enter email for bot account: " EMAIL
    read -sp "Enter password for bot account: " PASSWORD
    echo ""

    # Generate handle
    TIMESTAMP=$(date +%s)
    HANDLE="$AGGREGATOR_NAME-$TIMESTAMP.bsky.social"

    echo ""
    echo "Creating PDS account..."
    echo "Handle: $HANDLE"

    # Create account
    RESPONSE=$(curl -s -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
        -H "Content-Type: application/json" \
        -d "{
            \"handle\": \"$HANDLE\",
            \"email\": \"$EMAIL\",
            \"password\": \"$PASSWORD\"
        }")

    if echo "$RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
        echo "✗ Error creating account:"
        echo "$RESPONSE" | jq '.'
        exit 1
    fi

    DID=$(echo "$RESPONSE" | jq -r '.did')
    ACCESS_JWT=$(echo "$RESPONSE" | jq -r '.accessJwt')
    REFRESH_JWT=$(echo "$RESPONSE" | jq -r '.refreshJwt')

    echo "✓ Account created: $DID"

    # Save configuration
    cat > kagi-aggregator-config.env <<EOF
# Kagi Aggregator Configuration
AGGREGATOR_DID="$DID"
AGGREGATOR_HANDLE="$HANDLE"
AGGREGATOR_PDS_URL="$PDS_URL"
AGGREGATOR_EMAIL="$EMAIL"
AGGREGATOR_PASSWORD="$PASSWORD"
AGGREGATOR_ACCESS_JWT="$ACCESS_JWT"
AGGREGATOR_REFRESH_JWT="$REFRESH_JWT"
EOF

    echo "✓ Configuration saved to kagi-aggregator-config.env"
fi

# Get domain and Coves instance
read -p "Enter aggregator domain (e.g., kagi-news.example.com): " DOMAIN
read -p "Enter Coves instance URL (default: https://api.coves.social): " COVES_URL
COVES_URL=${COVES_URL:-https://api.coves.social}

# Setup .well-known
echo ""
echo "Setting up .well-known/atproto-did..."
mkdir -p .well-known
echo "$DID" > .well-known/atproto-did
echo "✓ Created .well-known/atproto-did"

echo ""
echo "================================================"
echo "IMPORTANT: Manual Step Required"
echo "================================================"
echo ""
echo "Upload the .well-known directory to your web server at:"
echo "  https://$DOMAIN/.well-known/atproto-did"
echo ""
read -p "Press Enter when the file is uploaded and accessible..."

# Verify .well-known
echo ""
echo "Verifying .well-known/atproto-did..."
WELLKNOWN_CONTENT=$(curl -s "https://$DOMAIN/.well-known/atproto-did" || echo "ERROR")

if [ "$WELLKNOWN_CONTENT" != "$DID" ]; then
    echo "✗ Error: .well-known/atproto-did not accessible or contains wrong DID"
    echo "  Expected: $DID"
    echo "  Got:      $WELLKNOWN_CONTENT"
    exit 1
fi

echo "✓ .well-known/atproto-did verified"

# Register with Coves
echo ""
echo "Registering with Coves instance..."
RESPONSE=$(curl -s -X POST "$COVES_URL/xrpc/social.coves.aggregator.register" \
    -H "Content-Type: application/json" \
    -d "{
        \"did\": \"$DID\",
        \"domain\": \"$DOMAIN\"
    }")

if echo "$RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
    echo "✗ Registration failed:"
    echo "$RESPONSE" | jq '.'
    exit 1
fi

echo "✓ Registered with Coves"

# Create service declaration
echo ""
echo "Creating service declaration..."
SERVICE_RECORD=$(cat <<EOF
{
    "\$type": "social.coves.aggregator.service",
    "did": "$DID",
    "displayName": "$DISPLAY_NAME",
    "description": "$DESCRIPTION",
    "sourceUrl": "$SOURCE_URL",
    "createdAt": "$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
}
EOF
)

RESPONSE=$(curl -s -X POST "$PDS_URL/xrpc/com.atproto.repo.createRecord" \
    -H "Authorization: Bearer $ACCESS_JWT" \
    -H "Content-Type: application/json" \
    -d "{
        \"repo\": \"$DID\",
        \"collection\": \"social.coves.aggregator.service\",
        \"rkey\": \"self\",
        \"record\": $SERVICE_RECORD
    }")

if echo "$RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
    echo "✗ Failed to create service declaration:"
    echo "$RESPONSE" | jq '.'
    exit 1
fi

RECORD_URI=$(echo "$RESPONSE" | jq -r '.uri')
echo "✓ Service declaration created: $RECORD_URI"

# Save final configuration
cat >> kagi-aggregator-config.env <<EOF

# Setup completed on $(date)
AGGREGATOR_DOMAIN="$DOMAIN"
COVES_INSTANCE_URL="$COVES_URL"
SERVICE_DECLARATION_URI="$RECORD_URI"
EOF

echo ""
echo "================================================"
echo "✓ Kagi Aggregator Setup Complete!"
echo "================================================"
echo ""
echo "Configuration saved to: kagi-aggregator-config.env"
echo ""
echo "Your aggregator is now registered and ready to use."
echo ""
echo "Next steps:"
echo "1. Start your aggregator bot: npm start (or appropriate command)"
echo "2. Community moderators can authorize your aggregator"
echo "3. Once authorized, your bot can start posting"
echo ""
echo "See docs/aggregators/SETUP_GUIDE.md for more information"
