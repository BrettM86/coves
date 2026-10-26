#!/usr/bin/env bash
# Runs the production Caddyfile in the production Caddy image against a stub
# upstream and checks that the public tdpl.io hosts never forward Tidepool's
# /admin API (operators reach it on the box at 127.0.0.1:8091), while the
# bridge's public routes still reach Tidepool.
#
# The real TLS issuers (ACME over Cloudflare DNS) are swapped for Caddy's
# internal CA after adapting, so the test needs no tokens and no network.
#
# Usage: scripts/caddy-edge-test.sh   (builds docker/caddy if needed)
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${CADDY_TEST_IMAGE:-coves/caddy:edge-test}"
NAME="coves-caddy-edge-test-$$"
NETWORK="$NAME"
WORKDIR="$(mktemp -d)"

cleanup() {
	docker rm -f "$NAME-caddy" "$NAME-tidepool" >/dev/null 2>&1 || true
	docker network rm "$NETWORK" >/dev/null 2>&1 || true
	rm -rf "$WORKDIR"
}
trap cleanup EXIT

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	docker build -q -t "$IMAGE" docker/caddy >/dev/null
fi

# Adapt the real Caddyfile, then point every TLS policy at the internal CA.
docker run --rm -e ACME_EMAIL=edge-test@example.invalid \
	-v "$PWD/Caddyfile:/etc/caddy/Caddyfile:ro" "$IMAGE" \
	caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile |
	jq '.apps.tls.automation.policies |= map(.issuers = [{"module": "internal"}])' \
		>"$WORKDIR/caddy.json"

# Stub Tidepool: echoes the path it received, and approves on-demand certs.
cat >"$WORKDIR/stub.Caddyfile" <<'EOF'
:80 {
	respond "upstream {path}" 200
}
EOF

docker network create "$NETWORK" >/dev/null
docker run -d --name "$NAME-tidepool" --network "$NETWORK" --network-alias tidepool \
	-v "$WORKDIR/stub.Caddyfile:/etc/caddy/Caddyfile:ro" "$IMAGE" >/dev/null
docker run -d --name "$NAME-caddy" --network "$NETWORK" -p 127.0.0.1::443 \
	-v "$WORKDIR/caddy.json:/etc/caddy/caddy.json:ro" "$IMAGE" \
	caddy run --config /etc/caddy/caddy.json >/dev/null

PORT="$(docker port "$NAME-caddy" 443/tcp | head -1 | sed 's/.*://')"

# Hosts covering each tdpl.io site block: the apex, a pre-issued instance
# wildcard, and the on-demand catch-all (two-label and one-label forms).
HOSTS=(tdpl.io alice.lemmy-world.tdpl.io bob.not-listed-instance.tdpl.io single.tdpl.io)

request() { # host method path -> "status body"
	local host="$1" method="$2" path="$3" body status
	body="$(curl -sk --max-time 10 --path-as-is -X "$method" \
		--resolve "$host:$PORT:127.0.0.1" -o - -w '\n%{http_code}' \
		"https://$host:$PORT$path")"
	status="${body##*$'\n'}"
	body="${body%$'\n'*}"
	echo "$status $body"
}

wait_ready() {
	for _ in $(seq 1 50); do
		if [[ "$(request tdpl.io GET /.well-known/nodeinfo)" == "200 upstream"* ]]; then
			return 0
		fi
		sleep 0.2
	done
	echo "Caddy did not become ready" >&2
	docker logs "$NAME-caddy" 2>&1 | tail -20 >&2
	exit 1
}
wait_ready

failures=0

expect_blocked() { # host method path
	local result
	result="$(request "$1" "$2" "$3")"
	if [[ "$result" != "404"* || "$result" == *upstream* ]]; then
		echo "FAIL blocked:   $2 https://$1$3 -> $result"
		failures=$((failures + 1))
	else
		echo "ok   blocked:   $2 https://$1$3"
	fi
}

expect_forwarded() { # host method path
	local result
	result="$(request "$1" "$2" "$3")"
	if [[ "$result" != "200 upstream $3" ]]; then
		echo "FAIL forwarded: $2 https://$1$3 -> $result"
		failures=$((failures + 1))
	else
		echo "ok   forwarded: $2 https://$1$3"
	fi
}

for host in "${HOSTS[@]}"; do
	expect_blocked "$host" GET /admin
	expect_blocked "$host" GET /admin/
	expect_blocked "$host" GET /admin/.env
	expect_blocked "$host" GET /admin/phpinfo.php
	expect_blocked "$host" GET /admin/metrics
	expect_blocked "$host" POST /admin/handles/migrate
	expect_blocked "$host" GET /ADMIN/metrics
	expect_blocked "$host" GET //admin/metrics
	expect_blocked "$host" GET /./admin/metrics
	expect_blocked "$host" GET /x/../admin/metrics
	expect_blocked "$host" GET /admin%2Fmetrics
	expect_blocked "$host" GET /%61dmin/metrics

	expect_forwarded "$host" GET /.well-known/atproto-did
	expect_forwarded "$host" GET /.well-known/webfinger
	expect_forwarded "$host" POST /inbox
	expect_forwarded "$host" GET /xrpc/com.atproto.sync.getRepo
done

if ((failures > 0)); then
	echo "$failures check(s) failed"
	exit 1
fi
echo "all checks passed"
