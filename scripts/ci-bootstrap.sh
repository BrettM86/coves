#!/usr/bin/env bash
# Seeds the CI stack: registers both PDSes with the relay, then creates the
# account the AppView authenticates as.
#
# Both halves must happen after the infrastructure is healthy and before the
# AppView boots, which is why they share a stage in scripts/lib/ci-stack.sh
# rather than being two `compose run` invocations.
#
# WHY THE PDS ACCOUNT STEP EXISTS
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
PDS2_URL=${PDS2_URL:-http://localhost:3011}
RELAY_URL=${RELAY_URL:-http://localhost:2470}
RELAY_ADMIN_KEY=${RELAY_ADMIN_KEY:-ci-relay-admin-key}
HANDLE=${PDS_INSTANCE_HANDLE:?PDS_INSTANCE_HANDLE must be set (see .env.ci)}
PASSWORD=${PDS_INSTANCE_PASSWORD:?PDS_INSTANCE_PASSWORD must be set (see .env.ci)}
EMAIL=${COVES_CI_INSTANCE_EMAIL:-instance@local.coves.dev}

# ---------------------------------------------------------------------------
# The relay: raise the crawl limit, then announce both PDSes
# ---------------------------------------------------------------------------
# A FRESH BigSky refuses every non-admin requestCrawl. Its slurper config
# starts with new_pds_per_day_limit = 0 and that limiter is checked BEFORE the
# trusted-domain list, so there is no configuration-only way around it: the
# admin API has to raise it first. The failure without this step is a 401 from
# requestCrawl, which reads like an auth problem with the announcement rather
# than a limit on the relay.
echo "▶ Raising the relay's new-host-per-day limit..."
limit_code=$(
    curl -sS -o /tmp/setPerDayLimit.out -w '%{http_code}' \
        -X POST "$RELAY_URL/admin/subs/setPerDayLimit?limit=1000" \
        -H "Authorization: Bearer $RELAY_ADMIN_KEY"
)
if [[ $limit_code != 200 ]]; then
    echo "  ✗ relay refused the admin limit change (HTTP $limit_code): $(cat /tmp/setPerDayLimit.out)" >&2
    exit 1
fi
echo "  ✓ per-day host limit raised"

# requestCrawl is how a PDS tells a relay it exists. The relay validates the
# hostname by calling back into that host's com.atproto.server.describeServer
# (over http, because of --crawl-insecure-ws) and only then subscribes to its
# firehose. A hostname MAY carry a port — BigSky keeps `u.Host` verbatim and
# special-cases a `localhost:` prefix back to http when it later resolves DID
# documents — which is what makes the shared-namespace topology work at all.
#
# Idempotent: re-announcing a host the relay already has is a no-op, so a
# COVES_CI_KEEP_STACK re-run needs no special case.
announce_host() {
    local label=$1 hostport=$2 code
    echo "▶ Announcing $label ($hostport) to the relay..."
    code=$(
        curl -sS -o /tmp/requestCrawl.out -w '%{http_code}' \
            -X POST "$RELAY_URL/xrpc/com.atproto.sync.requestCrawl" \
            -H 'Content-Type: application/json' \
            -d "{\"hostname\":\"$hostport\"}"
    )
    if [[ $code != 200 ]]; then
        echo "  ✗ relay refused to crawl $hostport (HTTP $code): $(cat /tmp/requestCrawl.out)" >&2
        exit 1
    fi
    echo "  ✓ crawling $hostport"
}

announce_host "the AppView's PDS" "${PDS_URL#http://}"
announce_host "the federated PDS" "${PDS2_URL#http://}"

# ---------------------------------------------------------------------------
# The instance account
# ---------------------------------------------------------------------------
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
