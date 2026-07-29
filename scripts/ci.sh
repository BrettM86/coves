#!/usr/bin/env bash
# The hermetic merge gate. Driven by `make ci`.
#
# WHAT THIS DOES THAT THE PER-TIER TARGETS DO NOT
#
#   * Creates its own infrastructure instead of asserting that a human already
#     started it. `make test-integration` and `make test-e2e` grade whatever the
#     dev stack happens to be serving; this builds the stack it grades against.
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

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

COMPOSE_FILE=docker-compose.ci.yml
PROJECT=${COVES_CI_PROJECT:-coves-ci}
OUT_DIR="$REPO_ROOT/.ci-out"

# Named so the volumes match docker-compose.ci.yml's external declarations.
CACHE_VOLUMES=(coves-ci-go-mod-cache coves-ci-go-build-cache)

CYAN='\033[36m'
GREEN='\033[32m'
YELLOW='\033[33m'
RED='\033[31m'
RESET='\033[0m'

step() { printf "\n${CYAN}▶ %s${RESET}\n" "$1"; }
ok() { printf "${GREEN}  ✓ %s${RESET}\n" "$1"; }
warn() { printf "${YELLOW}  ⚠ %s${RESET}\n" "$1"; }
fail() { printf "${RED}  ✗ %s${RESET}\n" "$1" >&2; }

compose() { docker compose -f "$COMPOSE_FILE" -p "$PROJECT" "$@"; }

# ---------------------------------------------------------------------------
# Architecture
# ---------------------------------------------------------------------------
# The Dockerfile defaults GOARCH to amd64 for the production deploy target.
# Building that under emulation on an arm64 machine makes every run
# substantially slower, so CI builds natively. Derived from uname rather than
# `go env` so this needs no Go toolchain on the host — Docker is the only
# prerequisite.
case "$(uname -m)" in
arm64 | aarch64) COVES_CI_GOARCH=arm64 ;;
x86_64 | amd64) COVES_CI_GOARCH=amd64 ;;
*)
    fail "unsupported host architecture $(uname -m); set COVES_CI_GOARCH manually"
    exit 1
    ;;
esac
export COVES_CI_GOARCH

# ---------------------------------------------------------------------------
# Teardown
# ---------------------------------------------------------------------------
# Registered before anything is created, so an interrupt or an early failure
# still cleans up. `down -v` removes this project's state volumes; the Go caches
# are declared external in the compose file precisely so they survive this.
teardown() {
    local exit_code=$?
    if [[ ${COVES_CI_KEEP_STACK:-0} == 1 ]]; then
        printf "\n${YELLOW}Stack left running (COVES_CI_KEEP_STACK=1).${RESET}\n"
        printf "  Inspect:  docker compose -f %s -p %s ps\n" "$COMPOSE_FILE" "$PROJECT"
        printf "  Logs:     docker compose -f %s -p %s logs appview\n" "$COMPOSE_FILE" "$PROJECT"
        printf "  Tear down: docker compose -f %s -p %s down -v --remove-orphans\n" "$COMPOSE_FILE" "$PROJECT"
        return $exit_code
    fi
    step "Tearing down the CI stack..."
    compose down -v --remove-orphans --timeout 10 >/dev/null 2>&1 || true
    ok "removed containers, network, and state volumes (Go caches kept)"
    return $exit_code
}
trap teardown EXIT

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
step "Preflight"

if ! docker info >/dev/null 2>&1; then
    fail "the Docker daemon is not reachable — start Docker and retry"
    exit 1
fi
ok "docker daemon reachable"

for volume in "${CACHE_VOLUMES[@]}"; do
    if ! docker volume inspect "$volume" >/dev/null 2>&1; then
        docker volume create "$volume" >/dev/null
        ok "created cache volume $volume"
    fi
done
ok "go module and build caches present"

mkdir -p "$OUT_DIR"

# Snapshot the working tree so the post-run check can compare against how the
# tree started, not against HEAD. Comparing against HEAD would flag every
# uncommitted change the developer already had in progress, which says nothing
# about whether the *run* modified anything.
TREE_BEFORE=$(git status --porcelain --untracked-files=all | sort)

# A stack left behind by an interrupted previous run would otherwise be reused
# with its state intact, which is the one thing this gate must never do.
step "Discarding any previous CI stack"
compose down -v --remove-orphans --timeout 10 >/dev/null 2>&1 || true
ok "clean slate"

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
step "Building images (AppView from the current working tree)"
compose build
ok "images built"

# ---------------------------------------------------------------------------
# Module cache (the only stage with network access)
# ---------------------------------------------------------------------------
# The stack's network is `internal: true` (see docker-compose.ci.yml), so once
# it is up nothing in it can reach the internet — including the Go toolchain,
# which downloads modules at test time rather than at image-build time. On a
# cold cache that would leave the runner unable to compile anything.
#
# So the modules are fetched here, in a throwaway container on Docker's default
# bridge, writing into the same external volume the runner mounts. Deliberately
# `docker run` and not `compose run`: every service in the compose file is
# pinned to the egress-blocked namespace, which is the whole point.
#
# A warm cache makes this a no-op costing a second or two. A failure here is
# fatal — continuing would produce a confusing "cannot find module" failure
# inside the suite instead of "the network was down before we started".
step "Populating the Go module cache (needs network; the stack itself has none)"
if ! docker run --rm \
    -v "$REPO_ROOT:/src" -w /src \
    -v coves-ci-go-mod-cache:/go/pkg/mod \
    -v coves-ci-go-build-cache:/go-build-cache \
    --entrypoint go \
    "$PROJECT-runner" mod download; then
    fail "could not download Go modules — check network access and go.sum"
    fail "if the network is fine, the cache volume may be corrupt: run 'make ci-clean' and retry"
    exit 1
fi
ok "module cache populated"

# ---------------------------------------------------------------------------
# Bring up infrastructure
# ---------------------------------------------------------------------------
# Staged deliberately: the AppView is NOT started here. It authenticates to the
# PDS as PDS_INSTANCE_HANDLE, and in a fresh PDS that account does not exist
# yet, so it has to be created between these two stages.
step "Starting infrastructure (Postgres ×3, PLC, PDS, Turnstile stub)"
# Jetstream is deliberately absent from this --wait list. `up --wait` fails
# outright — "has no healthcheck configured" — for any service it is asked to
# wait on that cannot report health, and the Jetstream image ships no HTTP
# client to probe itself with (docker-compose.dev.yml disables its healthcheck
# for the same reason).
compose up -d --wait netns postgres postgres-test postgres-plc plc-directory pds turnstile-stub
ok "infrastructure healthy"

# Started only after the PDS is healthy, since it connects straight to the PDS
# firehose. Readiness is then gated on its metrics endpoint from inside the
# namespace — see scripts/ci-runner.sh.
step "Starting Jetstream"
compose up -d jetstream
ok "jetstream started (readiness gated by the runner)"

step "Seeding the PDS"
# --no-deps so this does not drag the AppView up before its account exists.
compose run --rm --no-deps --entrypoint bash runner /src/scripts/ci-bootstrap.sh

# ---------------------------------------------------------------------------
# Bring up the system under test
# ---------------------------------------------------------------------------
step "Starting the AppView"
compose up -d --wait appview
ok "appview healthy on :8081"

# ---------------------------------------------------------------------------
# Run the suite
# ---------------------------------------------------------------------------
step "Running the suite"
set +e
compose run --rm runner
GATE_STATUS=$?
set -e

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
fi
printf "\n"

exit $GATE_STATUS
