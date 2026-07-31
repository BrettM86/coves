#!/usr/bin/env bash
# Readiness checks that run INSIDE the CI runner container. Sourced, never
# executed.
#
# Shared by scripts/ci-runner.sh (the full gate) and scripts/e2e-runner.sh
# (`make test-e2e`, the pipeline tier alone) so both agree on what "the stack is
# ready" means. A tier that started against a half-ready stack and a tier that
# waited would disagree about whether the code is broken, which is the failure
# mode this file removes.

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

# wait_for_stack gates on every service a test may touch.
#
# These are redundant with Compose healthchecks by design. If the namespace were
# misconfigured, the healthchecks would still pass — each service probing itself
# over its own loopback — while the runner could reach nothing. Failing here,
# loudly, beats a suite that skips every infrastructure test.
wait_for_stack() {
    echo "▶ Verifying the stack is reachable from the runner..."

    # Jetstream ships no HTTP client, so it cannot healthcheck itself and
    # docker-compose.ci.yml disables its healthcheck (as the dev compose does).
    # Its metrics endpoint is the readiness signal, and it must come up *after*
    # the PDS firehose is accepting connections, which is precisely the ordering
    # that would otherwise race.
    wait_for "jetstream (metrics :6009)" "http://localhost:6009/metrics" 90

    wait_for "pds (:3001)" "http://localhost:3001/xrpc/_health" 60
    wait_for "pds2 (:3011)" "http://localhost:3011/xrpc/_health" 60
    wait_for "relay (:2470)" "http://localhost:2470/xrpc/_health" 90
    # /_health rather than /, which redirects to the hosted web UI on the public
    # internet — unreachable from this egress-blocked network by design.
    wait_for "plc directory (:3002)" "http://localhost:3002/_health" 60
    wait_for "appview (:8081)" "http://localhost:8081/xrpc/_health" 90

    wait_for_relay_crawling

    echo
}

# wait_for_relay_crawling gates the suite on the relay having a LIVE upstream
# connection to both PDSes.
#
# A healthy relay proves nothing about this. `/xrpc/_health` answers as soon as
# the process is serving, and the crawl announcements (scripts/ci-bootstrap.sh)
# only queue a subscription — the relay dials each PDS afterwards, and on a
# cold, emulated start that takes a moment. A suite that began in that window
# would write records to a PDS nobody was listening to, and every contract
# would fail on a timeout that named the AppView.
#
# /admin/subs/getUpstreamConns is the relay's own answer to "who am I connected
# to right now": the slurper's active host list, not its configured one. So a
# host that was announced but whose dial failed does NOT appear here, which is
# exactly the distinction worth gating on.
#
# The failure NAMES THE HOST THAT IS MISSING rather than dumping the list of
# hosts that are present, because those are opposite instructions: "pds2 is not
# being crawled" sends whoever is reading to pds2's bootstrap announcement and
# its logs, while a raw connection list leaves them to work out which of two
# similar-looking addresses is the absent one.
wait_for_relay_crawling() {
    local admin_key=${RELAY_ADMIN_KEY:-ci-relay-admin-key}
    local i conns host missing
    for ((i = 1; i <= 60; i++)); do
        conns=$(curl -fsS --max-time 3 \
            -H "Authorization: Bearer $admin_key" \
            "http://localhost:2470/admin/subs/getUpstreamConns" 2>/dev/null || true)
        missing=""
        for host in localhost:3001 localhost:3011; do
            [[ $conns == *"\"$host\""* ]] || missing+="${missing:+, }$(relay_host_label "$host")"
        done
        if [[ -z $missing ]]; then
            echo "  ✓ relay crawling both PDSes"
            return 0
        fi
        sleep 1
    done
    echo "  ✗ after 60s the relay is still not crawling: $missing" >&2
    echo "    Every ingestion contract depends on this: the relay is the only path from" >&2
    echo "    either PDS to Jetstream. Check that host's own health, the relay's logs, and" >&2
    echo "    whether scripts/ci-bootstrap.sh's requestCrawl announcement for it succeeded." >&2
    echo "    The relay's live upstream list was: ${conns:-<no response>}" >&2
    return 1
}

# relay_host_label turns a crawl target into something a reader can act on: the
# ports differ by two characters and the roles do not.
relay_host_label() {
    case $1 in
        localhost:3001) echo "the AppView's PDS ($1)" ;;
        localhost:3011) echo "the federated PDS, pds2 ($1)" ;;
        *) echo "$1" ;;
    esac
}

# check_contract_manifest enforces docs/TEST_ARCHITECTURE.md §3.6.2: every
# collection the AppView's consumers ingest has an ingestion contract in the
# pipeline tier, or a burn-down entry naming the task that owes it one.
#
# It needs no infrastructure — it reads the consumer table and the test sources
# — but it runs here rather than as a host-side lint so that both entry points
# get it from one place, and so it fails in the same log as the tier it governs.
check_contract_manifest() {
    echo "▶ Checking the pipeline contract manifest (docs/TEST_ARCHITECTURE.md §3.4a)..."
    # -allow-pending=false is the phase-4 ratchet, flipped by task 16 now that
    # every consumed collection has a contract and tests/ci/pending_contracts.txt
    # is empty. Until the flip, a collection could be added to a consumer and
    # deferred by writing a line in that file; now the line itself fails the
    # gate, so the only way to add a collection is to prove it.
    #
    # Passed here rather than changed as the tool's default so that the gate is
    # the thing that judges: an ad-hoc run still reports contracted/pending/
    # missing without an opinion about the burn-down.
    go run ./cmd/contract-manifest -allow-pending=false
}

# run_pipeline_tier executes T2, and is the ONLY place its go test invocation is
# written down.
#
# It had drifted into three copies (the gate, this tier's own runner, and the
# dev escape hatch) within one task of being introduced, which is exactly how
# the pre-refactor suite ended up with ten near-identical firehose subscribers.
# The flags are not incidental:
#
#   -p 1 -parallel 1  the contracts share one AppView, one PDS and one firehose
#                     cursor space, so the tier is serial by design (§3.4 rule 2)
#   -count=1          defeats the test cache, which cannot see that the PDS or
#                     the firehose changed
#
# Extra arguments are appended, which is how the caller adds -json or -v.
run_pipeline_tier() {
    go test -tags e2e -p 1 -parallel 1 -count=1 \
        -timeout "${COVES_CI_TEST_TIMEOUT:-1800s}" "$@" ./tests/e2e/...
}
