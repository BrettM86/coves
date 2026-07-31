#!/usr/bin/env bash
# The hermetic merge gate. Driven by `make ci`.
#
# WHAT THIS DOES THAT THE PER-TIER TARGETS DO NOT
#
#   * Creates its own infrastructure instead of asserting that a human already
#     started it. `make test-integration` grades whatever the dev stack happens
#     to be serving; this builds the stack it grades against. (`make test-e2e`
#     now shares this script's bring-up — see scripts/lib/ci-stack.sh — so the
#     pipeline tier runs the same way in both.)
#
#   * Tests a binary built from the current working tree, rather than whatever
#     long-running `make run` started in another terminal, which may be many
#     edits stale. A gate that grades the wrong binary is worse than no gate.
#
#   * Starts from empty state every run: fresh PDS, fresh PLC registry, fresh
#     databases. No accumulated accounts, no handle collisions, no fixtures from
#     a previous run.
#
#   * Publishes no host ports, so it runs concurrently with `make dev-up` and
#     cannot interfere with it in either direction. Keep hacking while it runs.
#
#   * Refuses to report success when the suite silently shrank. See
#     cmd/ci-report — a skip is a failure unless the committed allowlist says
#     otherwise.
#
# Every step is non-interactive: no `read -p` prompts anywhere in this path, so
# an agent can run it unattended.
#
# Environment overrides:
#   COVES_CI_PROJECT      Compose project name (default coves-ci)
#   COVES_CI_KEEP_STACK   1 to leave the stack running for debugging
#   COVES_CI_ALLOW_STALE  true to downgrade stale-allowlist entries to warnings
set -euo pipefail

# Sets REPO_ROOT, cds to it, and defines compose/step/ok/warn/fail plus the
# stack_* bring-up used identically by scripts/test-e2e.sh.
# shellcheck source=scripts/lib/ci-stack.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/ci-stack.sh"

# ---------------------------------------------------------------------------
# Teardown
# ---------------------------------------------------------------------------
# Registered before anything is created, so an interrupt or an early failure
# still cleans up.
teardown() {
    local exit_code=$?
    stack_control_stop
    if [[ ${COVES_CI_KEEP_STACK:-0} == 1 ]]; then
        stack_keep_notice
        return $exit_code
    fi
    stack_teardown
    return $exit_code
}
trap teardown EXIT

stack_preflight

# Snapshot the working tree so the post-run check can compare against how the
# tree started, not against HEAD. Comparing against HEAD would flag every
# uncommitted change the developer already had in progress, which says nothing
# about whether the *run* modified anything.
TREE_BEFORE=$(git status --porcelain --untracked-files=all | sort)

# ---------------------------------------------------------------------------
# Bring up the system under test
# ---------------------------------------------------------------------------
stack_up

# ---------------------------------------------------------------------------
# Run the suite
# ---------------------------------------------------------------------------
# The pipeline tier's reliability suite restarts and reconfigures the AppView,
# which only the host can do — see stack_control_start in lib/ci-stack.sh. It
# serves for exactly as long as the suite runs, and the EXIT trap stops it.
stack_control_start

step "Running the suite"
set +e
compose run --rm runner
GATE_STATUS=$?
set -e

stack_control_stop

# ---------------------------------------------------------------------------
# Diagnostics on failure
# ---------------------------------------------------------------------------
# When an E2E test fails, the AppView's own logs are usually the only record of
# what the server actually did. Capturing them unconditionally on failure means
# the information is already there rather than requiring a re-run with
# COVES_CI_KEEP_STACK=1.
if [[ $GATE_STATUS -ne 0 ]]; then
    step "Capturing diagnostics"
    compose logs --no-color --tail 400 appview >"$OUT_DIR/appview.log" 2>&1 || true
    compose logs --no-color --tail 200 pds >"$OUT_DIR/pds.log" 2>&1 || true
    compose logs --no-color --tail 200 jetstream >"$OUT_DIR/jetstream.log" 2>&1 || true
    ok "wrote appview.log, pds.log, jetstream.log to .ci-out/"
fi

# ---------------------------------------------------------------------------
# The run must not have modified the checkout
# ---------------------------------------------------------------------------
# The runner mounts the repository read-write, because the Go toolchain and
# goose both expect a normal working tree. That makes it possible for a test to
# write into the repo, which would be a real defect — a suite that mutates its
# own source is not reproducible. .ci-out/ is gitignored, so a clean run leaves
# no trace.
step "Checking the run left the checkout unchanged"
TREE_AFTER=$(git status --porcelain --untracked-files=all | sort)
if [[ $TREE_BEFORE != "$TREE_AFTER" ]]; then
    warn "the run modified the working tree:"
    diff <(printf '%s\n' "$TREE_BEFORE") <(printf '%s\n' "$TREE_AFTER") |
        grep -E '^[<>]' | sed 's/^/      /' || true
    warn "a test writing into the checkout is a defect worth fixing"
else
    ok "checkout unchanged by the run"
fi

# ---------------------------------------------------------------------------
# Result
# ---------------------------------------------------------------------------
printf "\n"
if [[ $GATE_STATUS -eq 0 ]]; then
    printf "${GREEN}═══════════════════════════════════════════════════════════════${RESET}\n"
    printf "${GREEN}  ✓ CI GATE PASSED${RESET}\n"
    printf "${GREEN}═══════════════════════════════════════════════════════════════${RESET}\n"
else
    printf "${RED}═══════════════════════════════════════════════════════════════${RESET}\n"
    printf "${RED}  ✗ CI GATE FAILED${RESET}\n"
    printf "${RED}═══════════════════════════════════════════════════════════════${RESET}\n"
fi
printf "\n  Machine-readable summary: .ci-out/summary.json\n"
printf "  Raw go test -json stream: .ci-out/gotest.json\n"
if [[ $GATE_STATUS -ne 0 ]]; then
    printf "  Service logs:             .ci-out/appview.log, pds.log, jetstream.log\n"
    printf "\n  To investigate against a live stack:\n"
    printf "    COVES_CI_KEEP_STACK=1 make ci\n"
    printf "  ...then iterate on the pipeline tier alone against that same stack:\n"
    printf "    COVES_CI_REBUILD=1 make test-e2e\n"
fi
printf "\n"

exit $GATE_STATUS
