#!/usr/bin/env bash
# The pipeline tier (T2) against the hermetic stack. Driven by `make test-e2e`.
#
# docs/TEST_ARCHITECTURE.md §3.5. The stack publishes no host ports — that is
# what lets it run alongside `make dev-up` without either interfering with the
# other — so `go test -tags e2e` from the host cannot reach it. This brings the
# stack up (or reuses one that is already there) and runs the tier INSIDE the
# runner's network namespace, which is exactly what `make ci` does. One way the
# tier executes, so "green here, red in the gate" cannot happen for reasons of
# plumbing.
#
# It is not a second gate: no ci-report, no skip inversion, no other tier. `make
# ci` remains the merge gate. This is the loop you iterate a contract in.
#
# Environment:
#   COVES_CI_KEEP_STACK=1   leave the stack up afterwards — with a bring-up
#                           costing the better part of a minute, this is what
#                           makes a second run seconds instead. Subsequent runs
#                           reuse it automatically.
#   COVES_CI_REBUILD=1      when reusing a stack, rebuild the AppView from the
#                           working tree first. Without it a kept stack keeps
#                           serving the binary it started with, which is the one
#                           way this loop can grade the wrong code.
#   COVES_CI_PROJECT        Compose project name (default derived from the
#                           checkout dir, see scripts/lib/ci-stack.sh)
#   COVES_CI_TEST_TIMEOUT   go test -timeout (default 1800s)
set -euo pipefail

# shellcheck source=scripts/lib/ci-stack.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/ci-stack.sh"

stack_preflight

# REUSED_STACK decides teardown. A stack this script did not create is not this
# script's to destroy: it belongs to a `COVES_CI_KEEP_STACK=1 make ci` somebody
# is debugging, or to the previous iteration of this very loop.
REUSED_STACK=0
if stack_is_up; then
    REUSED_STACK=1
fi

teardown() {
    local exit_code=$?
    stack_control_stop
    if [[ $REUSED_STACK == 1 ]]; then
        printf "\n${CYAN}Reused an existing stack; leaving it as it was found.${RESET}\n"
        return $exit_code
    fi
    if [[ ${COVES_CI_KEEP_STACK:-0} == 1 ]]; then
        stack_keep_notice
        printf "  Re-run:   make test-e2e   ${CYAN}(reuses this stack)${RESET}\n"
        return $exit_code
    fi
    stack_teardown
    return $exit_code
}
trap teardown EXIT

if [[ $REUSED_STACK == 1 ]]; then
    step "Reusing the running CI stack"
    ok "appview is serving; skipping build and bring-up"
    if [[ ${COVES_CI_REBUILD:-0} == 1 ]]; then
        # The middle option between "grade a stale binary" and "throw the stack
        # away": rebuild only the AppView image and restart that one service.
        # Costs seconds against a warm build cache, keeps the PDS accounts, the
        # PLC registrations and the Jetstream backlog, and — because the process
        # restarts — resets the in-memory rate-limit buckets too.
        step "Rebuilding the AppView from the working tree (COVES_CI_REBUILD=1)"
        compose build appview
        compose up -d --wait appview
        ok "appview rebuilt from the current tree and restarted"
    else
        warn "its AppView is the binary built when the stack STARTED, not your current tree"
        warn "re-run with COVES_CI_REBUILD=1 to rebuild just the AppView into this stack"
        warn "or discard the stack entirely: $(compose_cmdline down -v --remove-orphans)"
    fi
else
    stack_up
fi

# ---------------------------------------------------------------------------
# Run the tier
# ---------------------------------------------------------------------------
# The reliability scenarios (§3.4c) stop, start and reconfigure the AppView
# container. Tests run inside the stack and cannot do that, so the host listens
# for their requests for as long as the tier runs — see stack_control_start in
# lib/ci-stack.sh. Started even for a REUSED stack: the channel belongs to the
# run, not to the stack.
stack_control_start

step "Running the pipeline tier inside the stack"
set +e
compose run --rm --entrypoint bash runner /src/scripts/e2e-runner.sh
TIER_STATUS=$?
set -e

stack_control_stop

if [[ $TIER_STATUS -ne 0 ]]; then
    step "Capturing diagnostics"
    stack_capture_logs
    ok "wrote $(stack_captured_logs) to .ci-out/"
fi

printf "\n"
if [[ $TIER_STATUS -eq 0 ]]; then
    printf "${GREEN}  ✓ PIPELINE TIER PASSED${RESET}\n"
    printf "\n  'make ci' is still the merge gate — this ran T2 only.\n"
else
    printf "${RED}  ✗ PIPELINE TIER FAILED${RESET}\n"
    printf "\n  Service logs (.ci-out/): %s\n" "$(stack_captured_logs)"
    printf "  Keep the stack up to poke at it:  COVES_CI_KEEP_STACK=1 make test-e2e\n"
    printf "  Then, after an edit:              COVES_CI_REBUILD=1 make test-e2e\n"
fi
printf "\n"

exit $TIER_STATUS
