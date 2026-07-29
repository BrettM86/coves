#!/usr/bin/env bash
# The hermetic stack, as a library. Sourced — never executed.
#
# WHY THIS FILE EXISTS
#
# Two host-side entry points need the same stack: scripts/ci.sh (the merge gate,
# which runs every tier) and scripts/test-e2e.sh (T2 alone, for an agent or a
# human iterating on pipeline contracts). docs/TEST_ARCHITECTURE.md §3.5 is
# explicit that there must be exactly ONE way the pipeline tier executes — the
# stack publishes no host ports, so a host-run `go test -tags e2e` cannot reach
# it at all, and a second, subtly different bring-up path is how "it passes for
# me" starts.
#
# So the bring-up lives here once, and both callers get the identical stack:
# same images built from the working tree, same staging (the AppView cannot
# start until its PDS account exists), same egress-blocked network, same cache
# volumes.
#
# CONTRACT FOR CALLERS
#
#   source "$(dirname "$0")/lib/ci-stack.sh"   # sets REPO_ROOT and cds to it
#   stack_preflight                            # docker reachable, caches exist
#   stack_up                                   # build, prefetch modules, start
#   ... do work through `compose run --rm ...` ...
#
# Callers own their own EXIT trap. stack_teardown is provided; when it runs (or
# whether it runs at all) is the caller's decision, because "leave the stack for
# debugging" means different things to a gate and to an iteration loop.
#
# Environment overrides (shared by both callers):
#   COVES_CI_PROJECT      Compose project name (default coves-ci)
#   COVES_CI_KEEP_STACK   1 to leave the stack running for debugging
#   COVES_CI_GOARCH       override the detected build architecture

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
#
# A value already in the environment is honoured rather than overwritten — the
# documented escape hatch for a host whose uname the case below does not know,
# and for reproducing an amd64 build on an arm64 machine.
stack_detect_arch() {
    if [[ -n ${COVES_CI_GOARCH:-} ]]; then
        export COVES_CI_GOARCH
        return 0
    fi
    case "$(uname -m)" in
    arm64 | aarch64) COVES_CI_GOARCH=arm64 ;;
    x86_64 | amd64) COVES_CI_GOARCH=amd64 ;;
    *)
        fail "unsupported host architecture $(uname -m); set COVES_CI_GOARCH manually"
        return 1
        ;;
    esac
    export COVES_CI_GOARCH
}

# ---------------------------------------------------------------------------
# Teardown
# ---------------------------------------------------------------------------
# `down -v` removes this project's state volumes; the Go caches are declared
# external in the compose file precisely so they survive this.
stack_teardown() {
    step "Tearing down the CI stack..."
    compose down -v --remove-orphans --timeout 10 >/dev/null 2>&1 || true
    ok "removed containers, network, and state volumes (Go caches kept)"
}

# compose_cmdline renders a `docker compose` command a human can paste.
#
# The COVES_CI_GOARCH prefix is not decoration. docker-compose.ci.yml declares
# GOARCH as a required build argument, and Compose interpolates the whole file
# on EVERY subcommand — so a bare `docker compose ... down -v` fails before it
# does anything, with "required variable COVES_CI_GOARCH is missing a value".
# Printing the bare form left a trail of copy-pasteable commands that all error,
# in exactly the situation (a stack left up after a failure) where the reader is
# least inclined to debug the instructions.
compose_cmdline() {
    # Resolved rather than defaulted to `uname -m`: on x86_64 hosts uname prints
    # "x86_64", which is not a GOARCH, so the fallback would emit a command that
    # fails differently. Every caller has already run stack_preflight, so this is
    # belt-and-braces — but a belt that prints a broken command is worse than no
    # belt.
    stack_detect_arch >/dev/null 2>&1 || true
    printf "COVES_CI_GOARCH=%s docker compose -f %s -p %s %s" \
        "${COVES_CI_GOARCH:-amd64}" "$COMPOSE_FILE" "$PROJECT" "$*"
}

# stack_keep_notice prints how to inspect a stack that was deliberately left up.
stack_keep_notice() {
    printf "\n${YELLOW}Stack left running (COVES_CI_KEEP_STACK=1).${RESET}\n"
    printf "  Inspect:   %s\n" "$(compose_cmdline ps)"
    printf "  Logs:      %s\n" "$(compose_cmdline logs appview)"
    printf "  Tear down: %s\n" "$(compose_cmdline down -v --remove-orphans)"
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
stack_preflight() {
    step "Preflight"

    if ! docker info >/dev/null 2>&1; then
        fail "the Docker daemon is not reachable — start Docker and retry"
        return 1
    fi
    ok "docker daemon reachable"

    stack_detect_arch || return 1

    local volume
    for volume in "${CACHE_VOLUMES[@]}"; do
        if ! docker volume inspect "$volume" >/dev/null 2>&1; then
            docker volume create "$volume" >/dev/null
            ok "created cache volume $volume"
        fi
    done
    ok "go module and build caches present"

    mkdir -p "$OUT_DIR"
}

# ---------------------------------------------------------------------------
# Is a usable stack already up?
# ---------------------------------------------------------------------------
# Only ever true for a stack this project name owns and whose AppView is
# actually serving — a half-started leftover is worse than none, because the
# suite would grade whatever fraction of it happens to answer.
#
# The probe runs INSIDE the namespace, for the same reason the runner's
# readiness checks do: every service healthchecks itself over its own loopback,
# so container health says nothing about whether the shared namespace is wired.
stack_is_up() {
    local state
    state=$(compose ps --status running --format '{{.Service}}' 2>/dev/null || true)
    grep -q '^appview$' <<<"$state" || return 1
    compose exec -T appview wget -q -O /dev/null http://localhost:8081/xrpc/_health 2>/dev/null
}

# ---------------------------------------------------------------------------
# Bring-up
# ---------------------------------------------------------------------------

stack_discard_previous() {
    # A stack left behind by an interrupted previous run would otherwise be
    # reused with its state intact, which is the one thing the gate must never
    # do.
    step "Discarding any previous CI stack"
    compose down -v --remove-orphans --timeout 10 >/dev/null 2>&1 || true
    ok "clean slate"
}

stack_build() {
    step "Building images (AppView from the current working tree)"
    compose build
    ok "images built"
}

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
stack_prefetch_modules() {
    step "Populating the Go module cache (needs network; the stack itself has none)"
    if ! docker run --rm \
        -v "$REPO_ROOT:/src" -w /src \
        -v coves-ci-go-mod-cache:/go/pkg/mod \
        -v coves-ci-go-build-cache:/go-build-cache \
        --entrypoint go \
        "$PROJECT-runner" mod download; then
        fail "could not download Go modules — check network access and go.sum"
        fail "if the network is fine, the cache volume may be corrupt: run 'make ci-clean' and retry"
        return 1
    fi
    ok "module cache populated"
}

# stack_start brings the services up in the order their dependencies force.
stack_start() {
    # Staged deliberately: the AppView is NOT started with the rest. It
    # authenticates to the PDS as PDS_INSTANCE_HANDLE, and in a fresh PDS that
    # account does not exist yet, so it has to be created in between.
    step "Starting infrastructure (Postgres ×3, PLC, PDS, Turnstile stub)"
    # Jetstream is deliberately absent from this --wait list. `up --wait` fails
    # outright — "has no healthcheck configured" — for any service it is asked
    # to wait on that cannot report health, and the Jetstream image ships no
    # HTTP client to probe itself with (docker-compose.dev.yml disables its
    # healthcheck for the same reason).
    compose up -d --wait netns postgres postgres-test postgres-plc plc-directory pds turnstile-stub
    ok "infrastructure healthy"

    # Started only after the PDS is healthy, since it connects straight to the
    # PDS firehose. Readiness is then gated on its metrics endpoint from inside
    # the namespace — see scripts/lib/runner-ready.sh.
    step "Starting Jetstream"
    compose up -d jetstream
    ok "jetstream started (readiness gated by the runner)"

    step "Seeding the PDS"
    # --no-deps so this does not drag the AppView up before its account exists.
    compose run --rm --no-deps --entrypoint bash runner /src/scripts/ci-bootstrap.sh

    step "Starting the AppView"
    compose up -d --wait appview
    ok "appview healthy on :8081"
}

# stack_up is the whole bring-up: the sequence both callers need, in one call.
stack_up() {
    stack_discard_previous
    stack_build
    stack_prefetch_modules
    stack_start
}
