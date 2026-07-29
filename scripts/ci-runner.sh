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
# /_health rather than /, which redirects to the hosted web UI on the public
# internet — unreachable from this egress-blocked network by design.
wait_for "plc directory (:3002)" "http://localhost:3002/_health" 60
wait_for "appview (:8081)" "http://localhost:8081/xrpc/_health" 90

echo

# ---------------------------------------------------------------------------
# 1b. Type-check the live tier
# ---------------------------------------------------------------------------
# tests/live is excluded from every build the gate runs, so nothing here would
# ever notice it stopped compiling — a rename in internal/ would break it
# silently and the breakage would surface weeks later, to whoever next ran
# `make test-live`. Vet type-checks it without executing anything, so it needs
# no network: the live tier stays buildable on the merge path even though it
# never runs there.
echo "▶ Type-checking the live tier (-tags live, not executed)..."
go vet -tags live ./tests/live/...
echo "  ✓ tests/live compiles"
echo

# ---------------------------------------------------------------------------
# 1c. Violation audit (advisory)
# ---------------------------------------------------------------------------
# docs/TEST_ARCHITECTURE.md §3.6.3. The suite is mid-migration and every count
# below is scheduled against a phase, so this reports and never judges — the
# `|| true` is belt-and-braces on top of the script's own exit 0. It becomes the
# hard lint gate in the final phase, when the counts are zero.
bash /src/scripts/test-audit.sh || true

# ---------------------------------------------------------------------------
# 1d. Test template database
# ---------------------------------------------------------------------------
# Provisions the migrated template that testkit.DB clones per test. Done here,
# once, rather than inside the test binaries: `go test` runs packages as
# separate processes and N of them racing to create the same database is a
# built-in flake.
#
# This ADDS a database. The legacy path — tests/integration running goose
# against the shared coves_test database — is untouched and still works.
echo "▶ Preparing the test template database..."
go run ./tests/testkit/cmd/testdbprepare

# The connection budget, derived from the server's max_connections rather than
# guessed: every test running under t.Parallel() holds its own clone pool.
# Nothing uses t.Parallel() outside tests/testkit yet, so this is inert today —
# it is wired now so that the phase enabling parallelism does not also have to
# discover the ceiling by exhausting it.
TEST_PARALLEL=$(bash /src/scripts/test-db-prepare.sh --print-parallel)
echo "  ✓ connection budget allows -parallel $TEST_PARALLEL"
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
go test -json -p 1 -parallel "$TEST_PARALLEL" -count=1 -timeout "$TEST_TIMEOUT" \
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
