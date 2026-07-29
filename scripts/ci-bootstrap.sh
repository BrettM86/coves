#!/usr/bin/env bash
# Seeds the CI stack's PDS with the account the AppView authenticates as.
#
# WHY THIS STEP EXISTS
#
# The AppView writes community records to the PDS as PDS_INSTANCE_HANDLE (see
# PDSConfig.HasInstanceCredentials in internal/config). The dev stack's PDS
# volume persists, so that account was created once — by hand, or by a
# scripts/create-test-account.sh that no longer exists in the tree — and has
# been there ever since.
#
# CI starts from an empty PDS on every run, so the account has to be created
# each time, and it has to exist before the AppView boots. That ordering is why
# this is a separate stage in scripts/ci.sh rather than part of ci-runner.sh.
#
# This is exactly the class of hidden dependency on accumulated dev-stack state
# that a hermetic gate is supposed to surface.
set -euo pipefail

PDS_URL=${PDS_URL:-http://localhost:3001}
HANDLE=${PDS_INSTANCE_HANDLE:?PDS_INSTANCE_HANDLE must be set (see .env.ci)}
PASSWORD=${PDS_INSTANCE_PASSWORD:?PDS_INSTANCE_PASSWORD must be set (see .env.ci)}
EMAIL=${COVES_CI_INSTANCE_EMAIL:-instance@local.coves.dev}

echo "▶ Creating the instance PDS account ($HANDLE)..."

# The PDS runs with PDS_INVITE_REQUIRED=false, so no invite code is needed.
response=$(
    curl -sS -o /tmp/createAccount.out -w '%{http_code}' \
        -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
        -H 'Content-Type: application/json' \
        -d "{\"handle\":\"$HANDLE\",\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}"
)
body=$(cat /tmp/createAccount.out)

case "$response" in
200 | 201)
    echo "  ✓ created"
    ;;
400)
    # Idempotency: a retried run against a stack that was kept alive
    # (COVES_CI_KEEP_STACK=1) will find the handle already taken. Anything else
    # in the 400 is a real configuration problem and must not be swallowed.
    if grep -qi 'handle.*taken\|already.*exists\|AlreadyExists' <<<"$body"; then
        echo "  ✓ already exists (reusing)"
    else
        echo "  ✗ PDS rejected the account creation: $body" >&2
        exit 1
    fi
    ;;
*)
    echo "  ✗ unexpected HTTP $response from the PDS: $body" >&2
    exit 1
    ;;
esac

# Prove the credentials the AppView will use actually work. Creating the account
# and being able to authenticate as it are different claims, and the AppView
# failing to log in surfaces much later and much more confusingly — as community
# writes failing deep inside a test.
echo "▶ Verifying the instance credentials authenticate..."
session_code=$(
    curl -sS -o /tmp/createSession.out -w '%{http_code}' \
        -X POST "$PDS_URL/xrpc/com.atproto.server.createSession" \
        -H 'Content-Type: application/json' \
        -d "{\"identifier\":\"$HANDLE\",\"password\":\"$PASSWORD\"}"
)
if [[ $session_code != 200 ]]; then
    echo "  ✗ could not authenticate as $HANDLE (HTTP $session_code): $(cat /tmp/createSession.out)" >&2
    exit 1
fi
echo "  ✓ credentials valid"
