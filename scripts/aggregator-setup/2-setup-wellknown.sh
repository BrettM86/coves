#!/bin/bash

# Script: 2-setup-wellknown.sh
# Purpose: Generate .well-known/atproto-did file for domain verification
#
# This script creates the .well-known/atproto-did file that proves you own your domain.
# You'll need to host this file at https://yourdomain.com/.well-known/atproto-did

set -e

echo "================================================"
echo "Step 2: Setup .well-known/atproto-did"
echo "================================================"
echo ""

# Load config if available
if [ -f "aggregator-config.env" ]; then
    source aggregator-config.env
    echo "✓ Loaded configuration from aggregator-config.env"
    echo "  DID: $AGGREGATOR_DID"
    echo ""
else
    echo "Configuration file not found. Please run 1-create-pds-account.sh first."
    exit 1
fi

# Get domain
read -p "Enter your aggregator's domain (e.g., rss-bot.example.com): " DOMAIN

if [ -z "$DOMAIN" ]; then
    echo "Error: Domain is required"
    exit 1
fi

# Save domain to config
echo "" >> aggregator-config.env
echo "AGGREGATOR_DOMAIN=\"$DOMAIN\"" >> aggregator-config.env

echo ""
echo "Creating .well-known directory..."
mkdir -p .well-known

# Create the atproto-did file
echo "$AGGREGATOR_DID" > .well-known/atproto-did

echo "✓ Created .well-known/atproto-did with content: $AGGREGATOR_DID"
echo ""

echo "================================================"
echo "Next Steps:"
echo "================================================"
echo ""
echo "1. Upload the .well-known directory to your web server"
echo "   The file must be accessible at:"
echo "   https://$DOMAIN/.well-known/atproto-did"
echo ""
echo "2. Verify it's working by running:"
echo "   curl https://$DOMAIN/.well-known/atproto-did"
echo "   (Should return: $AGGREGATOR_DID)"
echo ""
echo "3. Once verified, run: ./3-register-with-coves.sh"
echo ""

# Create nginx example
cat > nginx-example.conf <<EOF
# Example nginx configuration for serving .well-known
# Add this to your nginx server block:

location /.well-known/atproto-did {
    alias /path/to/your/.well-known/atproto-did;
    default_type text/plain;
    add_header Access-Control-Allow-Origin *;
}
EOF

echo "✓ Created nginx-example.conf for reference"
echo ""

# Create Apache example
cat > apache-example.conf <<EOF
# Example Apache configuration for serving .well-known
# Add this to your Apache virtual host:

Alias /.well-known /path/to/your/.well-known
<Directory /path/to/your/.well-known>
    Options None
    AllowOverride None
    Require all granted
    Header set Access-Control-Allow-Origin "*"
</Directory>
EOF

echo "✓ Created apache-example.conf for reference"
