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
# Overlay for the ONE scenario that needs a differently-configured AppView; see
# the file's own header and stack_control_run_verb below.
COMPOSE_FILE_TWO_FEED=docker-compose.ci.two-feed.yml
# ci_project_for_checkout derives the Compose project name from a checkout
# directory's basename: "coves" -> coves-ci, "coves-<suffix>" -> coves-ci-<suffix>,
# anything else -> coves-ci-<basename>. Lowercased and squeezed to the
# characters Compose accepts (lowercase alphanumerics, "-" and "_", leading
# alphanumeric).
#
# WHY A DERIVED DEFAULT: every worktree used to fall back to the one literal
# "coves-ci", so two sessions running `make ci` or `make test-e2e` from
# different worktrees shared one stack — one run's bring-up restarted the
# other's AppView mid-suite (bogus e2e failures) and one run's teardown
# removed the other's containers (2026-09-01, three sessions at once). The
# stack was already fully project-scoped (unnamed network and state volumes,
# no host ports, project-suffixed control channel); only the default name was
# shared. Deriving it from the checkout makes concurrent worktrees isolated by
# construction. COVES_CI_PROJECT still overrides it.
ci_project_for_checkout() {
    local base suffix
    base=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9_-]+/-/g; s/^[^a-z0-9]+//; s/-+$//')
    case "$base" in
        coves) suffix="" ;;
        coves-*) suffix=${base#coves-} ;;
        *) suffix=$base ;;
    esac
    if [[ -z $suffix ]]; then
        printf 'coves-ci'
    else
        printf 'coves-ci-%s' "$suffix"
    fi
}

PROJECT=${COVES_CI_PROJECT:-$(ci_project_for_checkout "$(basename "$REPO_ROOT")")}
# Exported so docker-compose.ci.yml's `${COVES_CI_PROJECT:-coves-ci}`
# interpolations (the runner image tag, COVES_STACK_CONTROL_DIR) and the runner
# container's own view of the name resolve to THIS project, not the literal
# fallback they carry for hand-run compose commands.
export COVES_CI_PROJECT="$PROJECT"
OUT_DIR="$REPO_ROOT/.ci-out"

# Named so the volumes match docker-compose.ci.yml's external declarations.
CACHE_VOLUMES=(coves-ci-go-mod-cache coves-ci-go-build-cache)

# The services whose logs are captured when a run fails: every hop of the
# ingest path, in the order events travel it. See stack_capture_logs.
CAPTURED_SERVICES=(appview pds pds2 relay jetstream postgres-relay)

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

# Said once per run so a log makes clear which stack it drove — the name is
# derived from the checkout and two worktrees' logs otherwise look identical.
printf "${CYAN}▶ Compose project: %s${RESET}\n" "$PROJECT"

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
#
# The AppView caveat is not hypothetical. The pipeline tier's reliability
# scenarios stop it and reconfigure it with a second Jetstream feed, restoring it
# through t.Cleanup — which does not run if the run dies without unwinding (a
# -timeout kill, a panic off the test goroutine, docker being stopped). What
# survives is either a stopped AppView or, worse because it looks fine, a healthy
# TWO-FEED one that the next run happily reuses and whose every consumer-health
# delta is then doubled. The tier refuses to start against it
# (requireSingleFeedTopology in tests/e2e/contracts_test.go), so this is a
# recovery hint rather than a warning about silent corruption.
stack_keep_notice() {
    printf "\n${YELLOW}Stack left running (COVES_CI_KEEP_STACK=1).${RESET}\n"
    printf "  Inspect:   %s\n" "$(compose_cmdline ps)"
    printf "  Logs:      %s\n" "$(compose_cmdline logs appview)"
    printf "  Tear down: %s\n" "$(compose_cmdline down -v --remove-orphans)"
    printf "  If a run died mid-reliability-suite the AppView may be stopped or left on two feeds:\n"
    printf "             %s\n" "$(compose_cmdline up -d --force-recreate appview)"
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

    # VERIFIED, not assumed. `down` is best-effort here on purpose — there may
    # be nothing to remove, and its complaints in that case are noise — but
    # suppressing its status meant this step printed "clean slate" over a stack
    # it had failed to remove, and the run then died forty seconds later inside
    # `up`, on a container-name conflict that named a container this function
    # had just claimed to delete. Observed after a reliability run left a kept
    # stack behind: `down` reported nothing and left the namespace container.
    #
    # So the postcondition is checked. A survivor is removed by force (these are
    # throwaway CI containers; nothing in them is worth preserving), and a
    # survivor that outlives THAT is fatal — proceeding would mean grading a
    # stack with a previous run's state in it.
    local leftovers
    leftovers=$(compose ps -aq 2>/dev/null)
    if [[ -n $leftovers ]]; then
        warn "compose down left containers behind; removing them by force"
        # Word-splitting is intended: one container id per line.
        # shellcheck disable=SC2086
        docker rm -f $leftovers >/dev/null 2>&1 || true
        leftovers=$(compose ps -aq 2>/dev/null)
    fi
    if [[ -n $leftovers ]]; then
        fail "could not remove the previous CI stack; these containers survived:"
        printf '      %s\n' $leftovers >&2
        fail "remove them by hand and retry: $(compose_cmdline down -v --remove-orphans)"
        return 1
    fi
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
    step "Starting infrastructure (Postgres ×4, PLC, PDS ×2, relay, Turnstile stub)"
    # Jetstream is deliberately absent from this --wait list. `up --wait` fails
    # outright — "has no healthcheck configured" — for any service it is asked
    # to wait on that cannot report health, and the Jetstream image ships no
    # HTTP client to probe itself with (docker-compose.dev.yml disables its
    # healthcheck for the same reason).
    #
    # The relay IS on the list, and waiting for it here rather than letting
    # Jetstream's depends_on handle it is what makes the next step possible:
    # the crawl announcement is an HTTP call against a relay that has to be
    # serving already.
    compose up -d --wait \
        netns postgres postgres-test postgres-plc postgres-relay \
        plc-directory pds pds2 relay turnstile-stub
    ok "infrastructure healthy"

    # Started only after the relay is healthy, since it consumes the relay's
    # merged firehose rather than a PDS' directly (see docker-compose.ci.yml's
    # header). Readiness is then gated on its metrics endpoint from inside the
    # namespace — see scripts/lib/runner-ready.sh.
    step "Starting Jetstream"
    compose up -d jetstream
    ok "jetstream started (readiness gated by the runner)"

    step "Seeding the stack (relay crawl announcements, instance PDS account)"
    # --no-deps so this does not drag the AppView up before its account exists.
    compose run --rm --no-deps --entrypoint bash runner /src/scripts/ci-bootstrap.sh

    step "Starting the AppView"
    compose up -d --wait appview
    ok "appview healthy on :8081"
}

# stack_capture_logs writes the logs of every service in the ingest path to
# .ci-out/, and echoes the filenames it wrote.
#
# WHICH SERVICES, AND WHY IT IS NO LONGER THREE
#
# It used to be appview, pds and jetstream, which was the whole path when
# Jetstream read the PDS directly. The federation topology put three more
# services INSIDE that path — pds2, the relay, and the relay's Postgres — and a
# fault in any of them reddens contracts that mention none of them: a relay that
# stopped crawling pds2 looks exactly like a consumer that stopped indexing.
# Capturing them costs nothing on a green run, because this only runs on
# failure, and re-running a whole gate to collect a log nobody thought to keep
# costs several minutes.
#
# Every capture is `|| true`: a service that never started has no logs, and a
# diagnostics step must not be the thing that fails the diagnosis.
stack_capture_logs() {
    local service tail_lines
    for service in "${CAPTURED_SERVICES[@]}"; do
        # The AppView's own log is the one that usually answers the question, so
        # it gets the deeper tail.
        tail_lines=200
        [[ $service == appview ]] && tail_lines=400
        compose logs --no-color --tail "$tail_lines" "$service" \
            >"$OUT_DIR/$service.log" 2>&1 || true
    done
}

# stack_captured_logs is that same list as a printable string, so a message
# pointing at the artifacts cannot name a file nobody wrote.
stack_captured_logs() {
    local service names=""
    for service in "${CAPTURED_SERVICES[@]}"; do
        names+="${names:+, }$service.log"
    done
    echo "$names"
}

# stack_up is the whole bring-up: the sequence both callers need, in one call.
stack_up() {
    stack_discard_previous
    stack_build
    stack_prefetch_modules
    stack_start
}

# ---------------------------------------------------------------------------
# The stack control channel
# ---------------------------------------------------------------------------
#
# WHY THE PIPELINE TIER NEEDS ONE
#
# docs/TEST_ARCHITECTURE.md §3.4c asks for reliability scenarios that CRUD
# contracts cannot reach: resume from a persisted cursor after a restart, replay
# a rewound stream exactly once, refuse a stale event after a delete, recover a
# dead letter, consume two overlapping feeds. Every one of those needs the
# AppView to stop, start, or come back with a different configuration — and the
# tests run INSIDE the stack, in a container with no Docker socket and, by
# §3.7's `internal: true` network, no route to anything but the stack itself.
# Only the host can act on containers.
#
# THE SHAPE, AND THE ALTERNATIVES IT WAS CHOSEN OVER
#
# A directory in .ci-out (already bind-mounted into the runner, already
# gitignored) is the channel. The container writes a one-word request file; this
# watcher, running on the host beside `compose run`, executes the matching
# command and writes a response file back. That keeps each scenario a single
# readable Go function — stop, write, start, assert, all in one place, with
# t.Cleanup restoring the stack however the test ends.
#
#   * Splitting the suite into host-orchestrated phases (`go test -run X_part1`,
#     compose restart, `go test -run X_part2`) needs no channel at all, but it
#     spreads one scenario across bash and three processes and has to hand state
#     between them through a file anyway — the same plumbing, minus the
#     readability.
#   * Mounting /var/run/docker.sock into a sidecar the tests could call is the
#     obvious "control plane" answer and hands host root to anything running in
#     the stack. This trades that for a bash `case` with five arms.
#
# WHAT IT WILL AND WILL NOT DO
#
# The verbs are a closed set, take NO arguments, and are dispatched by exact
# string match. A request naming anything else is refused and logged. So the
# most a compromised or buggy test can do is stop and start the AppView of a
# throwaway CI stack — which it could achieve anyway by making the AppView
# crash. Nothing here evaluates a string from the container as a command, and
# the two-feed topology lives in a compose overlay in the repo rather than in
# the request, precisely so that no request can name a configuration.
#
# LIFECYCLE
#
# Started by the host entry point before `compose run` and stopped after, so it
# exists exactly as long as a suite is running. Both callers register it in
# their EXIT trap: an orphaned watcher would sit polling a directory forever.

# PER-PROJECT, not per-repository. COVES_CI_PROJECT exists so two stacks can run
# from one checkout; a single shared channel directory would let one run's
# watcher answer the other run's requests and restart the wrong AppView. The
# container side is kept in step by docker-compose.ci.yml, which interpolates
# the same project name into COVES_STACK_CONTROL_DIR.
STACK_CONTROL_DIR="$OUT_DIR/stack-control-$PROJECT"
STACK_CONTROL_PIDFILE="$STACK_CONTROL_DIR/watcher.pid"
STACK_CONTROL_PID=""

# stack_control_start wipes the channel and begins serving it in the background.
#
# The wipe matters: a response left by a previous run under a filename a new run
# happened to reuse would be read as an instant answer to a request nobody
# executed. Run-scoped request ids make that improbable; starting empty makes it
# impossible.
#
# The pidfile check happens BEFORE the wipe, and guards the case the wipe would
# otherwise make worse: a watcher orphaned by a killed run is still polling this
# directory, so a second watcher would race it for each *.req — the `[[ -e
# $response ]]` guard in the loop is a check-then-act, not an atomic claim, and
# both could serve the same request. Refusing to start is right rather than
# harsh: two watchers on one channel means container operations firing twice
# with no way to tell which run asked.
stack_control_start() {
    step "Starting the stack control channel"

    if [[ -f $STACK_CONTROL_PIDFILE ]]; then
        local existing
        existing=$(cat "$STACK_CONTROL_PIDFILE" 2>/dev/null || true)
        if [[ -n $existing ]] && kill -0 "$existing" 2>/dev/null; then
            fail "a stack-control watcher (pid $existing) is already serving ${STACK_CONTROL_DIR#"$REPO_ROOT"/}"
            printf "  Another run is using project %q, or a previous run was killed and left it behind.\n" "$PROJECT"
            printf "  Kill it:            kill %s\n" "$existing"
            printf "  Or use a project:   COVES_CI_PROJECT=<name> make ci\n"
            return 1
        fi
        printf "${YELLOW}  stale watcher pidfile (pid %s is gone) — taking over the channel${RESET}\n" "${existing:-unknown}"
    fi

    rm -rf "$STACK_CONTROL_DIR"
    mkdir -p "$STACK_CONTROL_DIR"
    stack_control_loop &
    STACK_CONTROL_PID=$!
    printf '%s\n' "$STACK_CONTROL_PID" >"$STACK_CONTROL_PIDFILE"
    ok "watcher (pid $STACK_CONTROL_PID) serving ${STACK_CONTROL_DIR#"$REPO_ROOT"/}"
}

stack_control_stop() {
    [[ -n $STACK_CONTROL_PID ]] || return 0
    kill "$STACK_CONTROL_PID" 2>/dev/null || true
    wait "$STACK_CONTROL_PID" 2>/dev/null || true
    rm -f "$STACK_CONTROL_PIDFILE"
    STACK_CONTROL_PID=""
}

# stack_control_loop polls for requests. Polling rather than inotify/fswatch
# because neither is installed everywhere this runs, and because the latency
# that matters here is measured against a container restart.
stack_control_loop() {
    # This runs as a background job of a `set -e` script: without this, a single
    # non-zero command anywhere below would kill the watcher silently and every
    # subsequent request would time out with no explanation.
    set +e
    while true; do
        local request response
        for request in "$STACK_CONTROL_DIR"/*.req; do
            [[ -e $request ]] || continue
            response="${request%.req}.res"
            [[ -e $response ]] && continue
            stack_control_serve "$request" "$response"
        done
        sleep 0.2
    done
}

# stack_control_serve executes one request and answers it.
#
# The answer is written to a temporary name and renamed, because the reader
# polls for the response file's existence: a partially-written response would
# otherwise be read as a complete one. Rename within a directory is atomic, and
# the reader never sees the temporary name (it does not end in .res).
stack_control_serve() {
    local request=$1 response=$2 verb output status
    IFS= read -r verb <"$request"

    printf "${CYAN}  ⟲ stack-control: %s${RESET}\n" "${verb:-<empty>}"
    output=$(stack_control_run_verb "$verb" 2>&1)
    status=$?
    if [[ $status -eq 0 ]]; then
        ok "stack-control: $verb"
    else
        fail "stack-control: $verb exited $status"
    fi

    {
        printf 'exit %d\n' "$status"
        printf '%s\n' "$output"
    } >"$response.tmp"
    mv "$response.tmp" "$response"
}

# stack_control_run_verb is the closed set. Adding an arm here is the only way
# to widen what the pipeline tier can do to the stack, which is the point.
stack_control_run_verb() {
    case $1 in
    ping)
        # Answers the one question a test must ask before it depends on any of
        # this: is a watcher listening at all? Without it the first restart
        # would hang for its full timeout and report a stack problem, when the
        # truth is that the suite was launched a way that has no host beside it
        # (`make test-e2e-dev`, which grades a host-run AppView and therefore
        # skips this suite by name).
        printf 'pong\n'
        ;;
    appview-stop)
        # -t 20 rather than Compose's default 10s: SIGTERM starts a graceful
        # drain in which every connector flushes its cursor
        # (connector.go's flushCursorOnShutdown), and a cursor lost to SIGKILL
        # would make the resume scenario measure the wrong thing.
        compose stop -t 20 appview
        ;;
    appview-start)
        compose start appview
        ;;
    appview-two-feed)
        docker compose -f "$COMPOSE_FILE" -f "$COMPOSE_FILE_TWO_FEED" -p "$PROJECT" \
            up -d --force-recreate appview
        ;;
    appview-single-feed)
        # The restore, and it is a --force-recreate rather than a restart on
        # purpose: the container has to be rebuilt from the base compose file
        # alone for the overlay's environment to actually go away.
        compose up -d --force-recreate appview
        ;;
    *)
        printf 'refused: %q is not a stack-control verb\n' "$1"
        return 64
        ;;
    esac
}
