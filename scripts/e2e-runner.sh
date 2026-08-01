#!/usr/bin/env bash
# The pipeline tier (T2) alone, inside the CI runner container.
#
# `make test-e2e` → scripts/test-e2e.sh (brings the stack up) → this, via
# `compose run`. It is the same container, the same network namespace and the
# same go test invocation the merge gate uses in scripts/ci-runner.sh — that
# invocation is run_pipeline_tier, defined once in scripts/lib/runner-ready.sh.
# The only difference is that this one runs T2 and nothing else.
#
# docs/TEST_ARCHITECTURE.md §3.5: the hermetic stack publishes no host ports, so
# a host-run `go test -tags e2e` cannot reach it. Running here is not a
# convenience, it is the only way the tier can execute against the stack that
# the gate will also grade it against.
#
# Deliberately NOT run here: cmd/ci-report. The skip-inversion gate judges a
# whole-suite -json stream; a single-tier run has nothing to say about the
# suite's shape, and pretending otherwise would give two different verdicts on
# the same code. `make ci` is the gate. This is the iteration loop.
#
# What IS enforced here is narrower and belongs to this tier specifically: T2
# has no legitimate skips at all (see the verdict section below).
set -euo pipefail

OUT_DIR=${COVES_CI_OUT_DIR:-/src/.ci-out}
RAW="$OUT_DIR/e2e.json"

mkdir -p "$OUT_DIR"

# shellcheck source=scripts/lib/runner-ready.sh
source /src/scripts/lib/runner-ready.sh

# First, because it needs no infrastructure: a missing contract is knowable
# before the stack is even probed.
check_contract_manifest

wait_for_stack

# Type-check the tier before running it, so a compile error in a contract
# arrives as a compile error rather than as a package that quietly ran nothing.
echo "▶ Type-checking the pipeline tier..."
go vet -tags e2e ./tests/e2e/...
echo "  ✓ -tags e2e"
echo

echo "▶ Running the pipeline tier (serial, timeout ${COVES_CI_TEST_TIMEOUT:-1800s})..."
echo

# Captured as -json and replayed as text, rather than piped: a downstream reader
# must never be able to affect whether the run completes (scripts/ci-runner.sh
# has the full story — a BusyBox grep in a pipeline once killed the suite before
# it started). The status is kept for the same reason it is kept there: a run
# killed mid-stream leaves no fail events, so the stream alone cannot be trusted.
set +e
run_pipeline_tier -json -v >"$RAW" 2>&1
tier_status=$?
set -e

# Human-readable replay of what just happened.
grep -o '"Output":"[^"]*"' "$RAW" |
    sed -e 's/^"Output":"//' -e 's/"$//' -e 's/\\n$//' -e 's/\\t/    /g' |
    grep -E '^(=== RUN|--- (PASS|FAIL|SKIP)|ok|FAIL|PASS|panic)|_test\.go:' || true
echo

# ---------------------------------------------------------------------------
# Verdict
# ---------------------------------------------------------------------------
# T2 IS A ZERO-SKIP TIER, and unlike the rest of the suite it does not need an
# allowlist to say so. Every other tier has environmental gates worth arguing
# about; this one asked for the whole stack by invoking -tags e2e, and TestMain
# already failed the package if the stack was not there. So a skip inside a
# contract can only mean a contract quietly opted out of proving its collection
# — which is indistinguishable, from the outside, from the collection not being
# ingested at all. That is the exact false-green the manifest check exists to
# prevent, and it would walk straight through a check that only counts markers.
skipped=$(grep -c '"Action":"skip","Package":"[^"]*","Test":' "$RAW" || true)
passed=$(grep -c '"Action":"pass","Package":"[^"]*","Test":' "$RAW" || true)
failed=$(grep -c '"Action":"fail","Package":"[^"]*","Test":' "$RAW" || true)

echo "▶ Pipeline tier: $passed passed, $failed failed, $skipped skipped"

if [ "$skipped" -ne 0 ]; then
    echo
    echo "✗ $skipped test(s) SKIPPED in the pipeline tier. This tier has no legitimate"
    echo "  skips: it runs against a stack whose absence is already a hard failure, so"
    echo "  a skip is a contract declining to prove itself. Make it run or delete it."
    grep -o '"Action":"skip","Package":"[^"]*","Test":"[^"]*"' "$RAW" |
        sed -E 's/.*"Package":"([^"]*)","Test":"([^"]*)"/    ⊘ \1 \2/' | sort -u
    tier_status=1
fi

if [ "$tier_status" -eq 0 ] && [ "$passed" -eq 0 ]; then
    echo
    echo "✗ the pipeline tier reported success without running a single test — refusing"
    echo "  to call an empty run green."
    tier_status=1
fi

exit "$tier_status"
