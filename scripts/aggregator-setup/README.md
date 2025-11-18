# Aggregator Setup Scripts

This directory contains scripts to help you set up and register your aggregator with Coves instances.

## Overview

Aggregators are automated services that post content to Coves communities. They are similar to Bluesky's feed generators and labelers. To use aggregators with Coves, you need to:

1. Create a PDS account for your aggregator (gets you a DID)
2. Prove you own a domain via `.well-known/atproto-did`
3. Register with a Coves instance
4. Create a service declaration record

These scripts automate this process for you.

## Prerequisites

- **Domain ownership**: You must own a domain where you can host the `.well-known/atproto-did` file
- **Web server**: Ability to serve static files over HTTPS
- **Tools**: `curl`, `jq` (for JSON processing)
- **Account**: Email address for creating the PDS account

## Quick Start

### Interactive Setup (Recommended)

Run the scripts in order:

```bash
# Make scripts executable
chmod +x *.sh

# Step 1: Create PDS account
./1-create-pds-account.sh

# Step 2: Generate .well-known file
./2-setup-wellknown.sh

# Step 3: Register with Coves (after uploading .well-known)
./3-register-with-coves.sh

# Step 4: Create service declaration
./4-create-service-declaration.sh
```

### Automated Setup Example

For a reference implementation of automated setup, see the Kagi News aggregator at [aggregators/kagi-news/scripts/setup.sh](../../aggregators/kagi-news/scripts/setup.sh).

The Kagi script shows how to automate all 4 steps (with the manual .well-known upload step in between).

## Script Reference

### 1-create-pds-account.sh

**Purpose**: Creates a PDS account for your aggregator

**Prompts for**:
- PDS URL (default: https://bsky.social)
- Handle (e.g., mynewsbot.bsky.social)
- Email
- Password

**Outputs**:
- `aggregator-config.env` - Configuration file with DID and credentials
- Prints your DID and access tokens

**Notes**:
- Keep the config file secure! It contains your credentials
- The PDS automatically generates a DID:PLC for you
- You can use any PDS service, not just bsky.social

### 2-setup-wellknown.sh

**Purpose**: Generates the `.well-known/atproto-did` file for domain verification

**Prompts for**:
- Your domain (e.g., rss-bot.example.com)

**Outputs**:
- `.well-known/atproto-did` - File containing your DID
- `nginx-example.conf` - Example nginx configuration
- `apache-example.conf` - Example Apache configuration

**Manual step required**:
Upload the `.well-known` directory to your web server. The file must be accessible at:
```
https://yourdomain.com/.well-known/atproto-did
```

**Verify it works**:
```bash
curl https://yourdomain.com/.well-known/atproto-did
# Should return your DID (e.g., did:plc:abc123...)
```

### 3-register-with-coves.sh

**Purpose**: Registers your aggregator with a Coves instance

**Prompts for**:
- Coves instance URL (default: https://api.coves.social)

**Prerequisites**:
- `.well-known/atproto-did` must be accessible from your domain
- Scripts 1 and 2 must be completed

**What it does**:
1. Verifies your `.well-known/atproto-did` is accessible
2. Calls `social.coves.aggregator.register` XRPC endpoint
3. Coves verifies domain ownership
4. Inserts your aggregator into the `users` table

**Outputs**:
- Updates `aggregator-config.env` with Coves instance URL
- Prints registration confirmation

### 4-create-service-declaration.sh

**Purpose**: Creates the service declaration record in your repository

**Prompts for**:
- Display name (e.g., "RSS News Aggregator")
- Description
- Source URL (GitHub repo, etc.)
- Maintainer DID (optional)

**What it does**:
1. Creates a `social.coves.aggregator.service` record at `at://your-did/social.coves.aggregator.service/self`
2. Jetstream consumer will index this into the `aggregators` table
3. Communities can now discover and authorize your aggregator

**Outputs**:
- Updates `aggregator-config.env` with record URI and CID
- Prints record details

## Configuration File

After running the scripts, you'll have an `aggregator-config.env` file with:

```bash
AGGREGATOR_DID="did:plc:..."
AGGREGATOR_HANDLE="mynewsbot.bsky.social"
AGGREGATOR_PDS_URL="https://bsky.social"
AGGREGATOR_EMAIL="bot@example.com"
AGGREGATOR_PASSWORD="..."
AGGREGATOR_ACCESS_JWT="..."
AGGREGATOR_REFRESH_JWT="..."
AGGREGATOR_DOMAIN="rss-bot.example.com"
COVES_INSTANCE_URL="https://api.coves.social"
SERVICE_DECLARATION_URI="at://did:plc:.../social.coves.aggregator.service/self"
SERVICE_DECLARATION_CID="..."
```

**Use this in your aggregator code** to authenticate and post.

## What Happens Next?

After completing all 4 steps:

1. **Your aggregator is registered** in the Coves instance's `users` table
2. **Your service declaration is indexed** in the `aggregators` table (takes a few seconds)
3. **Community moderators can now authorize** your aggregator for their communities
4. **Once authorized**, your aggregator can post to those communities

## Creating an Authorization

Authorizations are created by community moderators, not by aggregators. The moderator writes a `social.coves.aggregator.authorization` record to their community's repository.

See `docs/aggregators/SETUP_GUIDE.md` for more information on the authorization process.

## Posting to Communities

Once authorized, your aggregator can post using:

```bash
curl -X POST https://api.coves.social/xrpc/social.coves.community.post.create \
  -H "Authorization: Bearer $AGGREGATOR_ACCESS_JWT" \
  -H "Content-Type: application/json" \
  -d '{
    "communityDid": "did:plc:...",
    "post": {
      "text": "Your post content",
      "createdAt": "2024-01-15T12:00:00Z"
    }
  }'
```

## Troubleshooting

### Error: "DomainVerificationFailed"

- Verify `.well-known/atproto-did` is accessible: `curl https://yourdomain.com/.well-known/atproto-did`
- Check the content matches your DID exactly (no extra whitespace)
- Ensure HTTPS is working (not HTTP)
- Check CORS headers if accessing from browser

### Error: "AlreadyRegistered"

- You've already registered this DID with this Coves instance
- This is safe to ignore if you're re-running the setup

### Error: "DIDResolutionFailed"

- Your DID might be invalid or not found in the PLC directory
- Verify your DID exists: `curl https://plc.directory/<your-did>`
- Wait a few seconds and try again (PLC directory might be propagating)

### Service declaration not appearing

- Wait 5-10 seconds for Jetstream consumer to index it
- Check the Jetstream logs for errors
- Verify the record was created: Check your PDS at `at://your-did/social.coves.aggregator.service/self`

## Example: Kagi News Aggregator

For a complete reference implementation, see the Kagi News aggregator at `aggregators/kagi-news/`.

The Kagi aggregator includes an automated setup script at [aggregators/kagi-news/scripts/setup.sh](../../aggregators/kagi-news/scripts/setup.sh) that demonstrates how to:

- Automate the entire registration process
- Use environment variables for configuration
- Handle errors gracefully
- Integrate the setup into your aggregator project

This shows how you can package scripts 1-4 into a single automated flow for your specific aggregator.

## Security Notes

- **Never commit `aggregator-config.env`** to version control
- Store credentials securely (use environment variables or secret management)
- Rotate access tokens regularly
- Use HTTPS for all API calls
- Validate community authorization before posting

## More Information

- [Aggregator Setup Guide](../../docs/aggregators/SETUP_GUIDE.md)
- [Aggregator PRD](../../docs/aggregators/PRD_AGGREGATORS.md)
- [atProto Identity Guide](../../ATPROTO_GUIDE.md)
- [Coves Communities PRD](../../docs/PRD_COMMUNITIES.md)

## Support

If you encounter issues:

1. Check the troubleshooting section above
2. Review the full documentation in `docs/aggregators/`
3. Open an issue on GitHub with:
   - Which script failed
   - Error message
   - Your domain (without credentials)
