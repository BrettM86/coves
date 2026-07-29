#!/usr/bin/env bash
# Entrypoint for the CI test-runner container (docker-compose.ci.yml, service
# "runner"). Runs inside the stack's shared network namespace, so every service
# is reachable on loopback at the same port the dev stack publishes it on.
#
# Responsibilities, in order:
#   1. Gate on readiness that Compose healthchecks cannot express.
#   2. Run the suite, capturing the raw -json stream.
#   3. Hand that stream to ci-report, which is the authority on pass/fail.
set -euo pipefail

OUT_DIR=${COVES_CI_OUT_DIR:-/src/.ci-out}
RAW="$OUT_DIR/gotest.json"
SUMMARY="$OUT_DIR/summary.json"
ALLOWLIST=${COVES_CI_ALLOWLIST:-/src/tests/ci/allowed_skips.txt}
TEST_TIMEOUT=${COVES_CI_TEST_TIMEOUT:-1800s}

mkdir -p "$OUT_DIR"

# ---------------------------------------------------------------------------
# 1. Readiness
# ---------------------------------------------------------------------------

# wait_for polls a URL until it answers, then returns. Compose's `--wait`
# already gated the services that can healthcheck themselves; this covers the
# one that cannot, and doubles as a check that the shared namespace is wired the
# way the rest of this design assumes.
wait_for() {
    local name=$1 url=$2 attempts=${3:-60}
    local i
    for ((i = 1; i <= attempts; i++)); do
        if curl -fsS --max-time 3 "$url" >/dev/null 2>&1; then
            echo "  ✓ $name"
            return 0
        fi
        sleep 1
    done
    echo "  ✗ $name did not become ready at $url after ${attempts}s" >&2
    return 1
}

echo "▶ Verifying the stack is reachable from the runner..."

# Jetstream ships no HTTP client, so it cannot healthcheck itself and
# docker-compose.ci.yml disables its healthcheck (as the dev compose does). Its
# metrics endpoint is the readiness signal, and it must come up *after* the PDS
# firehose is accepting connections, which is precisely the ordering that would
# otherwise race.
wait_for "jetstream (metrics :6009)" "http://localhost:6009/metrics" 90

# These are redundant with Compose healthchecks by design. If the namespace were
# misconfigured, the healthchecks would still pass — each service probing itself
# over its own loopback — while the runner could reach nothing. Failing here,
# loudly, beats a suite that skips every infrastructure test.
wait_for "pds (:3001)" "http://localhost:3001/xrpc/_health" 60
wait_for "plc directory (:3002)" "http://localhost:3002/" 60
wait_for "appview (:8081)" "http://localhost:8081/xrpc/_health" 90

echo

# ---------------------------------------------------------------------------
# 2. Run the suite
# ---------------------------------------------------------------------------

# -p 1 serialises packages. The integration suite's setup issues unscoped
# DELETEs against shared tables in the test database, so packages running
# concurrently delete each other's fixtures — the same reason `make test`
# already passes -p 1. `make test-all` does not pass it for ./cmd/... and
# ./internal/..., which is a latent race there.
#
# -count=1 defeats the test result cache. The toolchain hashes inputs it knows
# about, and it does not know about PostgreSQL, the PDS, or the firehose — so a
# cached PASS can survive an infrastructure change that would have failed. A
# gate must actually execute.
echo "▶ Running the full suite (-p 1 -count=1, timeout $TEST_TIMEOUT)..."
echo

# The capture is deliberately NOT a pipeline.
#
# An earlier version piped `go test -json` through tee into a grep/sed progress
# filter. BusyBox grep rejects --line-buffered, so the filter died immediately,
# closed the pipe, and took `go test` down with it — the suite never ran at all.
# Redirecting straight to a file means no downstream process can ever affect
# whether the run completes or whether its output is captured in full.
#
# Live progress is a strictly optional reader of that file, in the background.
: >"$RAW"

progress() {
    # Package-level events only: test-level events carry a "Test" field between
    # Package and Elapsed, so this pattern selects just the per-package results.
    # Purely cosmetic — if Go reorders these fields the progress lines quietly
    # stop appearing, and nothing else changes.
    tail -f "$RAW" 2>/dev/null |
        grep --line-buffered -E '"Action":"(pass|fail)","Package":"[^"]+","Elapsed"' |
        sed -E 's/.*"Action":"(pass|fail)","Package":"([^"]+)".*/  \1  \2/'
}

progress &
progress_pid=$!
# Kill the progress reader however this script exits, so a failure mid-suite
# cannot leave a stray tail holding the container open.
trap 'kill "$progress_pid" 2>/dev/null || true' EXIT

set +e
go test -json -p 1 -count=1 -timeout "$TEST_TIMEOUT" \
    ./cmd/... ./internal/... ./tests/... \
    >>"$RAW" 2>&1
set -e

# Let the reader drain the tail of the file before its output is interleaved
# with the report below.
sleep 1
kill "$progress_pid" 2>/dev/null || true
wait "$progress_pid" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 3. Judge
# ---------------------------------------------------------------------------

# go test's own exit code is deliberately ignored: it reports a skipped suite as
# success, which is the whole reason ci-report exists. ci-report reads the same
# stream and applies the stricter rules.
#
# Built rather than `go run`, so the exit code is ci-report's own and the
# toolchain does not print its own "exit status 1" line over the report.
go build -o /tmp/ci-report ./cmd/ci-report
exec /tmp/ci-report \
    -allowlist "$ALLOWLIST" \
    -summary "$SUMMARY" \
    -allow-stale "${COVES_CI_ALLOW_STALE:-false}" \
    <"$RAW"
