#!/usr/bin/env bash
# Seeds the CI stack: registers both PDSes with the relay, then creates the
# instance account and two moderation admin accounts before the AppView boots.
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
ADMIN_ONE_HANDLE=${CI_MODERATION_ADMIN_ONE_HANDLE:?CI_MODERATION_ADMIN_ONE_HANDLE must be set (see .env.ci)}
ADMIN_ONE_PASSWORD=${CI_MODERATION_ADMIN_ONE_PASSWORD:?CI_MODERATION_ADMIN_ONE_PASSWORD must be set (see .env.ci)}
ADMIN_TWO_HANDLE=${CI_MODERATION_ADMIN_TWO_HANDLE:?CI_MODERATION_ADMIN_TWO_HANDLE must be set (see .env.ci)}
ADMIN_TWO_PASSWORD=${CI_MODERATION_ADMIN_TWO_PASSWORD:?CI_MODERATION_ADMIN_TWO_PASSWORD must be set (see .env.ci)}
CI_PROJECT=${COVES_CI_PROJECT:?COVES_CI_PROJECT must be set by the CI runner}

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
# PDS accounts
# ---------------------------------------------------------------------------
provision_account() {
    local label=$1 handle=$2 email=$3 password=$4 response body session_code account_did
    echo "▶ Creating the $label PDS account ($handle)..."

    # The PDS runs with PDS_INVITE_REQUIRED=false, so no invite code is needed.
    response=$(
        curl -sS -o /tmp/createAccount.out -w '%{http_code}' \
            -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
            -H 'Content-Type: application/json' \
            -d "{\"handle\":\"$handle\",\"email\":\"$email\",\"password\":\"$password\"}"
    )
    body=$(cat /tmp/createAccount.out)

    case "$response" in
    200 | 201)
        echo "  ✓ created"
        ;;
    400)
        # A kept stack can already have this handle. Any other 400 is a real
        # configuration error, not a successful retry.
        if grep -qi 'handle.*taken\|already.*exists\|AlreadyExists' <<<"$body"; then
            echo "  ✓ already exists (reusing)"
        else
            echo "  ✗ PDS rejected the account creation: $body" >&2
            return 1
        fi
        ;;
    *)
        echo "  ✗ unexpected HTTP $response from the PDS: $body" >&2
        return 1
        ;;
    esac

    # Creating an account and authenticating as it are different claims.
    echo "▶ Verifying the $label credentials authenticate..."
    session_code=$(
        curl -sS -o /tmp/createSession.out -w '%{http_code}' \
            -X POST "$PDS_URL/xrpc/com.atproto.server.createSession" \
            -H 'Content-Type: application/json' \
            -d "{\"identifier\":\"$handle\",\"password\":\"$password\"}"
    )
    if [[ $session_code != 200 ]]; then
        echo "  ✗ could not authenticate as $handle (HTTP $session_code): $(cat /tmp/createSession.out)" >&2
        return 1
    fi
    echo "  ✓ credentials valid"

    # The runner has GNU grep but no jq. Do not print the createSession body:
    # it contains access and refresh tokens alongside the DID.
    if ! account_did=$(grep -oE '"did"[[:space:]]*:[[:space:]]*"did:[a-z]+:[^"]+"' /tmp/createSession.out | cut -d '"' -f 4) ||
        [[ ! $account_did =~ ^did:[a-z]+:[a-zA-Z0-9._:%-]+$ ]]; then
        echo "  ✗ PDS session for $handle did not contain a valid DID" >&2
        return 1
    fi
    PROVISIONED_DID=$account_did
}

provision_account "instance" "$HANDLE" "$EMAIL" "$PASSWORD"
provision_account "moderation admin one" "$ADMIN_ONE_HANDLE" "${CI_MODERATION_ADMIN_ONE_EMAIL:-modadmin-one@local.coves.dev}" "$ADMIN_ONE_PASSWORD"
admin_one_did=$PROVISIONED_DID
provision_account "moderation admin two" "$ADMIN_TWO_HANDLE" "${CI_MODERATION_ADMIN_TWO_EMAIL:-modadmin-two@local.coves.dev}" "$ADMIN_TWO_PASSWORD"
admin_two_did=$PROVISIONED_DID

if [[ -z $admin_one_did || -z $admin_two_did || $admin_one_did == "$admin_two_did" ]]; then
    echo "  ✗ moderation admin accounts must have distinct, non-empty DIDs" >&2
    exit 1
fi

admins_dir=/src/.ci-out
admins_file="$admins_dir/moderation-admins-${CI_PROJECT}.env"
mkdir -p "$admins_dir"
admins_tmp=$(mktemp "$admins_file.tmp.XXXXXX")
trap 'rm -f "$admins_tmp"' EXIT
printf 'MODERATION_ADMINS=%s,%s\n' "$admin_one_did" "$admin_two_did" >"$admins_tmp"
# The runner writes as root, but host-side Compose must be able to read it.
chmod 644 "$admins_tmp"
mv "$admins_tmp" "$admins_file"
trap - EXIT
echo "  ✓ moderation admin DIDs saved for the AppView"
