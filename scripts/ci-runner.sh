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
# 1b. Type-check every tag set
# ---------------------------------------------------------------------------
# Build tags make tiers invisible to builds that did not ask for them, which
# also makes their rot invisible: a rename in internal/ can break a tier for
# weeks, until whoever next runs that tier discovers it. Vet type-checks each
# selection without executing anything, so none of this needs infrastructure.
#
# The untagged and integration passes are belt-and-braces — the suite below
# compiles both anyway — but they fail here with a clear message instead of
# inside a 20-minute test run. The live pass is the load-bearing one: tests/live
# never executes on the merge path at all.
echo "▶ Type-checking every tag set (nothing is executed)..."
go vet ./...
echo "  ✓ untagged (unit tier)"
go vet -tags integration ./...
echo "  ✓ -tags integration"
go vet -tags e2e ./tests/e2e/...
echo "  ✓ -tags e2e"
go vet -tags live ./tests/live/...
echo "  ✓ -tags live"
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

# The concurrency budget, derived from the server's max_connections rather than
# guessed: every test running under t.Parallel() holds its own clone pool, and
# -p multiplies that by the number of test binaries running at once. Both flags
# come from testkit.ConcurrencyBudget, which also documents why the package
# dimension is currently 1 (the shared Jetstream, not the old shared database).
TEST_FLAGS=$(bash /src/scripts/test-db-prepare.sh --print-flags)
echo "  ✓ connection budget allows $TEST_FLAGS"
echo

# ---------------------------------------------------------------------------
# 2. Run the suite
# ---------------------------------------------------------------------------

# The gate runs two selections, because tiers are build tags and a tag set is a
# compilation, not a filter:
#
#   -tags integration ./cmd/... ./internal/... ./tests/...   T0 + T1
#   -tags e2e         ./tests/e2e/...                        T2
#
# Tags are additive, so the first selection compiles the untagged unit files in
# alongside the integration ones and covers both tiers in a single pass. T2 is a
# separate compilation because `e2e` and `integration` are disjoint sets, and it
# runs second so the pipeline contracts are graded last, against a stack the
# earlier tier has already exercised.
#
# -count=1 defeats the test result cache. The toolchain hashes inputs it knows
# about, and it does not know about PostgreSQL, the PDS, or the firehose — so a
# cached PASS can survive an infrastructure change that would have failed. A
# gate must actually execute.
echo "▶ Running the full suite ($TEST_FLAGS -count=1, timeout $TEST_TIMEOUT)..."
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

# Both runs APPEND to the same raw stream, and neither is piped, for the reason
# above: ci-report consumes one -json stream covering every tier, and a
# downstream reader must never be able to affect whether a run completes.
#
# Their exit statuses are KEPT, not discarded. ci-report is the verdict on what
# the stream says, but it can only judge events that reached the file: if a run
# is killed (OOM, SIGKILL, a panic that takes the harness down) the stream is
# truncated *without* fail events, and a report built from the surviving prefix
# reads as green. The statuses are the out-of-band evidence that the run
# actually finished, and they are cross-checked against the report below.
#
# T2 is pinned to -p 1 -parallel 1 rather than the computed budget: the pipeline
# contracts share one AppView, one PDS and one firehose cursor space, so they
# are serial by design (docs/TEST_ARCHITECTURE.md §3.4). The budget applies to
# the integration tier, where per-test database clones are the constraint.
#
# TEST_FLAGS is deliberately unquoted: it carries two flags and two values.
set +e
# shellcheck disable=SC2086
go test -json -tags integration $TEST_FLAGS -count=1 -timeout "$TEST_TIMEOUT" \
    ./cmd/... ./internal/... ./tests/... \
    >>"$RAW" 2>&1
integration_status=$?
go test -json -tags e2e -p 1 -parallel 1 -count=1 -timeout "$TEST_TIMEOUT" \
    ./tests/e2e/... \
    >>"$RAW" 2>&1
e2e_status=$?
set -e

# Let the reader drain the tail of the file before its output is interleaved
# with the report below.
sleep 1
kill "$progress_pid" 2>/dev/null || true
wait "$progress_pid" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 3. Judge
# ---------------------------------------------------------------------------

# ci-report is the primary verdict. `go test`'s exit code alone is not a gate:
# it reports a suite that skipped itself into nothing as success, which is the
# whole reason this tool exists. ci-report reads the stream and applies the
# stricter rules.
#
# Built rather than `go run`, so the exit code is ci-report's own and the
# toolchain does not print its own "exit status 1" line over the report.
go build -o /tmp/ci-report ./cmd/ci-report
set +e
/tmp/ci-report \
    -allowlist "$ALLOWLIST" \
    -summary "$SUMMARY" \
    -allow-stale "${COVES_CI_ALLOW_STALE:-false}" \
    <"$RAW"
report_status=$?
set -e

# ---------------------------------------------------------------------------
# 3b. Cross-check the report against the runs that produced it
# ---------------------------------------------------------------------------
#
# ci-report can only judge what reached the stream, so it is blind to a run that
# died without writing failure events. Two rules close that hole, and both are
# about the STREAM's integrity rather than about any individual test:
#
#   * status > 1 is not a test failure. `go test` exits 1 when tests fail and 2
#     when it could not run them (bad flags, a package that would not build);
#     a signal death (OOM killer, SIGKILL) surfaces as 128+signo. None of those
#     are guaranteed to leave fail events behind, so they fail the gate outright
#     regardless of what the report says.
#
#   * status != 0 while ci-report says ok is a contradiction. `go test` saw
#     something wrong that the stream does not contain — the definition of a
#     truncated capture. Trusting the report here is exactly the false-green
#     this check exists to prevent.
#
# An ordinary failing test satisfies neither rule (status 1, report not ok), so
# it is still reported by ci-report in ci-report's own words.
gate_status=$report_status

for tier_status in "integration:$integration_status" "e2e:$e2e_status"; do
    tier=${tier_status%%:*}
    status=${tier_status##*:}

    if [ "$status" -gt 1 ]; then
        echo
        echo "✗ the $tier run exited $status — that is a crashed or unrunnable"
        echo "  suite, not a test failure, and its -json stream may be truncated."
        echo "  Failing the gate on the exit status rather than on the report."
        gate_status=1
    elif [ "$status" -ne 0 ] && [ "$report_status" -eq 0 ]; then
        echo
        echo "✗ the $tier run exited $status but the report says every test passed."
        echo "  go test saw a failure that never reached $RAW, so the captured"
        echo "  stream is incomplete and the report cannot be trusted."
        gate_status=1
    fi
done

exit "$gate_status"
