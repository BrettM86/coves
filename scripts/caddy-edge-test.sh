#!/usr/bin/env bash
# Runs the production Caddyfile in the production Caddy image against a stub
# upstream and checks that the public tdpl.io hosts never forward Tidepool's
# /admin API (operators reach it on the box at 127.0.0.1:8091), while the
# bridge's public routes still reach Tidepool. It also checks the coves.social
# request-body limits: the frontend catch-all accepts large POST bodies only on
# the image-upload and profile-update proxy routes, rejects oversized bodies
# everywhere else with 413 before a complete body reaches the frontend, and the
# AppView's own /xrpc route is not capped by the frontend limit.
#
# The real TLS issuers (ACME over Cloudflare DNS) are swapped for Caddy's
# internal CA after adapting, so the test needs no tokens and no network.
#
# Usage: scripts/caddy-edge-test.sh   (builds docker/caddy if needed)
#        CADDYFILE=/path/to/copy scripts/caddy-edge-test.sh   (test another file)
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${CADDY_TEST_IMAGE:-coves/caddy:edge-test}"
CADDYFILE="$(realpath "${CADDYFILE:-Caddyfile}")" # override to test a mutated copy
NAME="coves-caddy-edge-test-$$"
NETWORK="$NAME"
WORKDIR="$(mktemp -d)"

cleanup() {
	docker rm -f "$NAME-caddy" "$NAME-upstream" >/dev/null 2>&1 || true
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
	-v "$CADDYFILE:/etc/caddy/Caddyfile:ro" "$IMAGE" \
	caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile |
	jq '.apps.tls.automation.policies |= map(.issuers = [{"module": "internal"}])' \
		>"$WORKDIR/caddy.json"

# Stub upstream for every proxied service, one process on three ports:
# tidepool (80, also approves on-demand certs), coves-prod-frontend (3000) and
# appview (8080, health_uri /xrpc/_health). It reads the whole request body,
# counts the bytes it actually read (a peer that disconnects mid-body yields a
# short count; Content-Length is never echoed), replies
# "upstream <path> port=<port> bytes=<n>", and logs one line per request to
# stdout keyed by the X-Edge-Test-Id request header.
cat >"$WORKDIR/stub.py" <<'EOF'
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def read_body(handler):
    """Return (bytes_read, complete)."""
    rfile = handler.rfile
    count = 0
    try:
        if "chunked" in handler.headers.get("Transfer-Encoding", "").lower():
            while True:
                line = rfile.readline(65537)
                if not line.endswith(b"\n"):
                    return count, False
                size = int(line.split(b";")[0].strip() or b"0", 16)
                if size == 0:
                    while True:
                        trailer = rfile.readline(65537)
                        if not trailer.endswith(b"\n"):
                            return count, False
                        if trailer in (b"\r\n", b"\n"):
                            return count, True
                remaining = size
                while remaining > 0:
                    chunk = rfile.read(min(remaining, 65536))
                    if not chunk:
                        return count, False
                    count += len(chunk)
                    remaining -= len(chunk)
                if rfile.readline(65537) not in (b"\r\n", b"\n"):
                    return count, False
        declared = int(handler.headers.get("Content-Length") or 0)
        remaining = declared
        while remaining > 0:
            chunk = rfile.read1(min(remaining, 65536))
            if not chunk:
                return count, False
            count += len(chunk)
            remaining -= len(chunk)
        return count, True
    except (OSError, ValueError):
        return count, False


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def handle_any(self):
        port = self.server.server_address[1]
        count, complete = read_body(self)
        request_id = self.headers.get("X-Edge-Test-Id", "-")
        print(
            f"request id={request_id} port={port} method={self.command} "
            f"path={self.path} bytes={count} complete={'yes' if complete else 'no'}",
            flush=True,
        )
        if not complete:
            self.close_connection = True
            return
        body = f"upstream {self.path} port={port} bytes={count}".encode()
        try:
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except OSError:
            self.close_connection = True

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_OPTIONS = handle_any

    def log_message(self, *args):
        pass


servers = [ThreadingHTTPServer(("0.0.0.0", port), Handler) for port in (80, 3000, 8080)]
for server in servers[1:]:
    threading.Thread(target=server.serve_forever, daemon=True).start()
print("stub ready", flush=True)
servers[0].serve_forever()
EOF

docker network create "$NETWORK" >/dev/null
docker run -d --name "$NAME-upstream" --network "$NETWORK" \
	--network-alias tidepool --network-alias coves-prod-frontend --network-alias appview \
	-v "$WORKDIR/stub.py:/stub.py:ro" python:3.11-slim python -u /stub.py >/dev/null
# Caddy health-checks appview at provisioning and marks it unhealthy for the
# whole 30s interval if port 8080 is not bound yet, so wait for the stub first.
stub_ready=no
for _ in $(seq 1 50); do
	if docker logs "$NAME-upstream" 2>/dev/null | grep -q '^stub ready$'; then
		stub_ready=yes
		break
	fi
	sleep 0.2
done
if [[ "$stub_ready" != yes ]]; then
	echo "stub upstream did not become ready" >&2
	docker logs "$NAME-upstream" 2>&1 | tail -20 >&2
	exit 1
fi
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
	if [[ "$result" != "200 upstream $3 port=80 bytes=0" ]]; then
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

# ── coves.social request-body limits ────────────────────────────────────
# Each case: label method path content-type body-bytes expected-status
# expected-port. A 200 case must show the receiving port and the full byte
# count; a 413 case must not leave a complete body in the upstream log (Caddy
# may forward part of an over-limit body before answering, so partial reads
# are allowed).
UPLOAD=/api/proxy/xrpc/social.coves.media.uploadImage
PROFILE=/api/proxy/xrpc/social.coves.actor.updateProfile
POST_CREATE=/api/proxy/xrpc/social.coves.community.post.create
BODY_CASES=(
	"B1 POST $UPLOAD image/jpeg 9900000 200 3000"
	"B1 POST $UPLOAD image/jpeg 10000000 200 3000"
	"B2 POST $PROFILE application/json 1400000 200 3000"
	"B3 POST $POST_CREATE application/json 1000000 200 3000"
	"B3 POST $POST_CREATE application/json 1040000 413 -"
	"B3 POST $POST_CREATE application/json 1100000 413 -"
	"B3 PUT $UPLOAD image/jpeg 1100000 413 -"
	"B3 PUT $PROFILE application/json 1100000 413 -"
	"B4 POST $UPLOAD image/jpeg 10600000 413 -"
	"B4 POST $PROFILE application/json 1600000 413 -"
	"B5 POST /xrpc/social.coves.media.uploadImage image/jpeg 10000000 200 8080"
	"B5 POST /xrpc/social.coves.media.uploadImage image/jpeg 12000000 200 8080"
)

body_file() { # bytes -> path of a file of exactly that many bytes
	local file="$WORKDIR/body-$1"
	[[ -f "$file" ]] || head -c "$1" /dev/zero >"$file"
	echo "$file"
}

# Sends one body to coves.social; sets send_rc, send_status, send_body.
send_body() { # id method path content-type bytes
	local out="$WORKDIR/response-$1"
	send_rc=0
	send_status="$(curl -sk --http1.1 -H 'Expect:' --noproxy '*' --max-time 120 \
		--resolve "coves.social:$PORT:127.0.0.1" -X "$2" \
		-H "Content-Type: $4" -H "X-Edge-Test-Id: $1" \
		--data-binary "@$(body_file "$5")" -o "$out" -w '%{http_code}' \
		"https://coves.social:$PORT$3")" || send_rc=$?
	send_body="$(cat "$out" 2>/dev/null || true)"
}

# Prints the upstream's log lines for one request id. A 413 can reach the
# client before the upstream logs its short read, so poll (bounded) for the
# line to appear; no line at all means nothing was forwarded.
upstream_records() { # id
	local lines
	for _ in $(seq 1 15); do
		lines="$(docker logs "$NAME-upstream" 2>/dev/null | grep -F "request id=$1 " || true)"
		if [[ -n "$lines" ]]; then
			echo "$lines"
			return 0
		fi
		sleep 0.2
	done
}

case_number=0
for body_case in "${BODY_CASES[@]}"; do
	read -r label method path content_type bytes want_status want_port <<<"$body_case"
	case_number=$((case_number + 1))
	id="edge-$case_number-$$"
	desc="$label $method https://coves.social$path ($bytes bytes)"
	send_body "$id" "$method" "$path" "$content_type" "$bytes"
	if ((send_rc != 0)); then
		echo "FAIL body:      $desc -> curl exit $send_rc status=$send_status"
		failures=$((failures + 1))
		continue
	fi
	if [[ "$want_status" == 200 ]]; then
		want_body="upstream $path port=$want_port bytes=$bytes"
		if [[ "$send_status" != 200 || "$send_body" != "$want_body" ]]; then
			echo "FAIL body:      $desc -> $send_status $send_body"
			failures=$((failures + 1))
		else
			echo "ok   body:      $desc -> 200 port=$want_port bytes=$bytes"
		fi
	else
		records="$(upstream_records "$id")"
		if [[ "$send_status" != "$want_status" ]]; then
			echo "FAIL body:      $desc -> $send_status $send_body"
			failures=$((failures + 1))
		elif grep -qE "bytes=$bytes complete=yes" <<<"$records"; then
			echo "FAIL body:      $desc -> $send_status but upstream received the complete body: $records"
			failures=$((failures + 1))
		else
			echo "ok   body:      $desc -> $send_status, no complete body upstream"
		fi
	fi
done

if ((failures > 0)); then
	echo "$failures check(s) failed"
	exit 1
fi
echo "all checks passed"
