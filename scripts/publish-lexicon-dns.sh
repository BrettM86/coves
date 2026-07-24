#!/usr/bin/env bash
# Creates/updates the _lexicon TXT records on Cloudflare that delegate the
# social.coves.* NSID namespaces to the lexicon publishing account's DID.
#
# Usage:
#   CF_API_TOKEN=<token with Zone:DNS:Edit on coves.social> \
#   LEXICON_DID=did:plc:xxxxxxxxxxxx \
#   ./scripts/publish-lexicon-dns.sh [--include-moderation]
#
# Idempotent: existing records are updated in place, missing ones created.
set -euo pipefail

# NEVER hardcode CF_API_TOKEN in this file — it is tracked in git and pushed
# to public remotes. Put secrets in scripts/.env.lexicon (gitignored):
#   CF_API_TOKEN=...
#   LEXICON_DID=did:web:coves.social
ENV_FILE="$(dirname "$0")/.env.lexicon"
if [[ -f "$ENV_FILE" ]]; then
  # shellcheck disable=SC1090
  source "$ENV_FILE"
fi

ZONE_NAME="coves.social"

if [[ -z "${CF_API_TOKEN:-}" || -z "${LEXICON_DID:-}" ]]; then
  echo "error: CF_API_TOKEN and LEXICON_DID must be set (env or scripts/.env.lexicon)" >&2
  exit 1
fi

# One record per unique NSID authority (NSID minus its last segment, reversed).
# social.coves.community.post.get -> authority social.coves.community.post
# -> DNS name _lexicon.post.community.coves.social
AUTHORITIES=(
  "_lexicon.actor.${ZONE_NAME}"
  "_lexicon.aggregator.${ZONE_NAME}"
  "_lexicon.community.${ZONE_NAME}"
  "_lexicon.comment.community.${ZONE_NAME}"
  "_lexicon.post.community.${ZONE_NAME}"
  "_lexicon.embed.${ZONE_NAME}"
  "_lexicon.feed.${ZONE_NAME}"
  "_lexicon.vote.feed.${ZONE_NAME}"
  "_lexicon.richtext.${ZONE_NAME}"
)
if [[ "${1:-}" == "--include-moderation" ]]; then
  AUTHORITIES+=("_lexicon.moderation.${ZONE_NAME}")
fi

API="https://api.cloudflare.com/client/v4"
AUTH=(-H "Authorization: Bearer ${CF_API_TOKEN}" -H "Content-Type: application/json")

ZONE_ID=$(curl -sf "${AUTH[@]}" "${API}/zones?name=${ZONE_NAME}" | python3 -c \
  "import json,sys; r=json.load(sys.stdin)['result']; print(r[0]['id'] if r else '')")
if [[ -z "$ZONE_ID" ]]; then
  echo "error: zone ${ZONE_NAME} not found (token missing zone read access?)" >&2
  exit 1
fi
echo "zone ${ZONE_NAME}: ${ZONE_ID}"

CONTENT="did=${LEXICON_DID}"
for NAME in "${AUTHORITIES[@]}"; do
  EXISTING=$(curl -sf "${AUTH[@]}" \
    "${API}/zones/${ZONE_ID}/dns_records?type=TXT&name=${NAME}" | python3 -c \
    "import json,sys; r=json.load(sys.stdin)['result']; print(r[0]['id'] if r else '')")
  BODY=$(python3 -c "import json; print(json.dumps({
    'type': 'TXT', 'name': '${NAME}', 'content': '\"${CONTENT}\"', 'ttl': 3600}))")
  if [[ -n "$EXISTING" ]]; then
    curl -sf "${AUTH[@]}" -X PUT "${API}/zones/${ZONE_ID}/dns_records/${EXISTING}" \
      -d "$BODY" > /dev/null
    echo "updated ${NAME} -> ${CONTENT}"
  else
    curl -sf "${AUTH[@]}" -X POST "${API}/zones/${ZONE_ID}/dns_records" \
      -d "$BODY" > /dev/null
    echo "created ${NAME} -> ${CONTENT}"
  fi
done

echo
echo "done — verify propagation with:"
echo "  dig +short TXT _lexicon.actor.${ZONE_NAME}"
echo "  goat lex check-dns internal/atproto/lexicon/social/coves"
