#!/usr/bin/env bash
# Counts test-suite invariant violations. HARD GATE — a nonzero count fails.
#
# WHAT THIS IS FOR
#
# docs/TEST_ARCHITECTURE.md §3.6.3. Every category below is something the
# refactor removed, and this is what stops it coming back. It ran in warn mode
# through phases 0-5 as the migration's progress meter (911 violations at the
# phase-1 baseline); phase 6 flipped it to a failure, which is only honest
# because the counts reached their floor first.
#
# THE FLOOR IS ZERO — with exemptions that are declared, not assumed.
#
# Every check here is a grep over text, and text has legitimate reasons to
# contain the thing being counted: a URL-parser test's inputs are Bluesky URLs,
# a config test's assertions are the production defaults it parses, and a test
# that proves a cache entry expires has to let the deadline pass. Those are
# exempted individually, in the source, with a reason — so each one is a
# decision someone made and can be found by grepping for the marker:
#
#     someCall(x) // coves:allow-sleep: <reason>
#     someCall(x) // coves:allow-host-literal: <reason>
#     someCall(x) // coves:allow-public-host: <reason>
#     someCall(x) // coves:allow-raw-body-decode: <reason>
#
# and, where per-line markers would be noise because the whole file's subject
# matter IS the literal (the Bluesky URL parser; the config parser), a
# file-scope form declared once near the top of the file:
#
#     // coves:allow-public-host-file: <reason>
#
# File-scope exemptions are printed on EVERY run with their reason and hit
# count, so a broad exemption cannot go quiet. Adding either kind is a visible
# edit in review; that is the whole enforcement model — the greps stop drift,
# the annotations record the exceptions, and neither pretends to be a proof.
#
# THESE ARE TRIPWIRES, NOT PROOFS. A grep is bypassable by construction — a URL
# built by concatenation, a bare IP literal, a sleep hidden behind a helper.
# They catch drift cheaply. The actual guarantee that tests never reach the
# public network is the egress-blocked CI network (docker-compose.ci.yml,
# `internal: true`).
#
# SCOPE. "test code" means every *_test.go file under cmd/, internal/ and
# tests/, plus every .go file under tests/ — shared helpers that do not end in
# _test.go (tests/testkit, tests/fixtures) are test code too, and historically
# some of the worst offenders lived in exactly those files. Production sources
# are out of scope for the test-hygiene categories, because production code
# legitimately dials websockets and names public hosts, and auditing it here
# would mean exempting the AppView from a rule written for its tests. Three
# categories run wider on purpose: testing.Short() scans everything (nothing
# in this tree may call it), and the raw-request-body and unchecked-DecodeJSON
# rules scan production sources only — they are API-hardening fences, not test
# hygiene.
#
# Usage:
#   scripts/test-audit.sh        summary table; exits nonzero on any violation
#   scripts/test-audit.sh -v     table plus every offending file:line
set -uo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$REPO_ROOT"

VERBOSE=0
if [[ ${1:-} == "-v" || ${1:-} == "--verbose" ]]; then
    VERBOSE=1
fi

CYAN='\033[36m'
YELLOW='\033[33m'
GREEN='\033[32m'
RED='\033[31m'
RESET='\033[0m'

# ---------------------------------------------------------------------------
# File scopes
# ---------------------------------------------------------------------------

test_code_files() {
    {
        find cmd internal tests -type f -name '*_test.go'
        find tests -type f -name '*.go'
    } 2>/dev/null | sort -u
}

all_go_files() {
    find cmd internal tests -type f -name '*.go' 2>/dev/null | sort -u
}

production_go_files() {
    find cmd internal -type f -name '*.go' ! -name '*_test.go' 2>/dev/null | sort -u
}

# ---------------------------------------------------------------------------
# Scanning
# ---------------------------------------------------------------------------

TOTAL=0
declare -a ROWS=()
declare -a DETAILS=()
declare -a EXEMPT_FILES=()

# scan <label> <pattern> <scope-fn> <path-exclude-ERE|""> [exemption-marker]
#
# grep -H is load-bearing: xargs splits its input into batches, and a batch that
# happens to contain exactly one file makes plain grep omit the filename. The
# comment-exclusion regex below anchors on "file:line:", so without -H it
# silently stops matching on those batches and the counts differ between a Mac
# (one batch) and the Alpine CI runner (several).
#
# Whole-line comments are not counted. This tree explains itself at length, and
# a comment that mentions localhost:5434 while describing the stack is not a
# hardcoded endpoint. It is a deliberate undercount at the margin: a violation
# trailing a comment on the same line still counts, because the code is there.
scan() {
    local label=$1 pattern=$2 scope=$3 exclude=$4 exemption=${5:-}
    local files hits count exempt=0 file reason fhits

    files=$($scope)
    if [[ -n $exclude ]]; then
        files=$(printf '%s\n' "$files" | grep -vE "$exclude" || true)
    fi

    if [[ -z $files ]]; then
        hits=""
    else
        hits=$(printf '%s\n' "$files" | tr '\n' '\0' |
            xargs -0 grep -HnE "$pattern" 2>/dev/null |
            grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' || true)

        if [[ -n $exemption ]]; then
            # File scope first: a file that declares the exemption drops out
            # wholesale, and is reported by name so the breadth stays visible.
            for file in $(printf '%s\n' "$files" | tr '\n' '\0' |
                xargs -0 grep -lF "${exemption}-file:" 2>/dev/null || true); do
                fhits=$(printf '%s\n' "$hits" | grep -c "^${file}:" || true)
                [[ $fhits -eq 0 ]] && continue
                reason=$(grep -m1 -oE "${exemption}-file:.*" "$file" |
                    sed "s/${exemption}-file:[[:space:]]*//")
                EXEMPT_FILES+=("$(printf '  %-58s %3d  %s' "$file" "$fhits" "$reason")")
                exempt=$((exempt + fhits))
                hits=$(printf '%s\n' "$hits" | grep -v "^${file}:" || true)
            done

            # Then per line. Scoped to the category that defines the marker, so
            # an exemption for a hostname cannot quietly excuse a sleep on the
            # same line.
            local before after
            before=$(printf '%s' "$hits" | grep -c . || true)
            hits=$(printf '%s\n' "$hits" | grep -v "$exemption" || true)
            after=$(printf '%s' "$hits" | grep -c . || true)
            exempt=$((exempt + before - after))
        fi
    fi

    count=$(printf '%s' "$hits" | grep -c . || true)
    TOTAL=$((TOTAL + count))
    ROWS+=("$(printf '  %-52s %6d %8d' "$label" "$count" "$exempt")")

    if [[ $VERBOSE -eq 1 && $count -gt 0 ]]; then
        DETAILS+=("$label"$'\n'"$hits")
    fi
}

echo
printf "${CYAN}═══════════════════════════════════════════════════════════════════════${RESET}\n"
printf "${CYAN}  TEST SUITE VIOLATION AUDIT${RESET}   (hard gate — any violation fails)\n"
printf "${CYAN}═══════════════════════════════════════════════════════════════════════${RESET}\n"
echo

# 1. Sleeps. A sleep spent waiting for another party to finish is a guess about
#    timing that is wrong in both directions: too short under CI load (flake),
#    too long on an idle laptop (slow suite). testkit.WaitFor / testkit.Holds
#    replace every one of them.
#
#    testkit is scanned like everything else. It is the one package that MUST
#    sleep — `WaitFor` and `Holds` are a poll loop and a poll loop sleeps — but
#    "the package that implements waiting" is not a licence for the package's
#    own tests, and a directory-wide pass would have been exactly that: an
#    unexamined sleep in testkit_test would have counted as zero. Both real
#    sleeps carry a per-line marker in wait.go, so the exemption is two declared
#    lines rather than a directory.
#
#    The exempted residue is a different animal, and the distinction is the one
#    worth keeping in mind when adding a marker: a sleep that waits for a
#    DEADLINE THE TEST ITSELF SET to pass (a 40ms cache TTL, a circuit
#    breaker's open window) is sound in the only direction that matters —
#    oversleeping cannot break it — where a sleep that waits for WORK SOMEONE
#    ELSE IS DOING is a race with a timer attached. The first kind is
#    exemptible with a reason; the second is a bug being scheduled.
scan "time.Sleep in test code" \
    'time\.Sleep\(' \
    test_code_files '' 'coves:allow-sleep'

# 2. Skips. cmd/ci-report treats a skip as a failure unless it is allowlisted
#    with a reason in tests/ci/allowed_skips.txt, which is empty and argues for
#    itself in its header. Environmental gating belongs to the harness, not to
#    test bodies: an invoked tier that cannot reach its infrastructure fails and
#    names the make target that brings it up (§3.1). There is deliberately NO
#    exemption marker for this category — the allowlist is the mechanism, and
#    two mechanisms for the same thing is how the first one rots.
scan "t.Skip outside testkit" \
    '\.Skip(f|Now)?\(' \
    test_code_files 'tests/testkit/' ''

# 3. testing.Short(). The tier mechanism is build tags, which are compile-time
#    and cannot be silently forgotten. -short silently skipped ~161 sites while
#    printing green. Scanned across production sources too: nothing in this tree
#    may branch on it.
scan "testing.Short()" \
    'testing\.Short\(\)' \
    all_go_files '' ''

# 4. Raw websocket dialing. The cursor-before-write ordering and the
#    consecutive-timeout guard are written exactly once, in testkit/firehose.go.
#    Ten hand-rolled copies is how the suite ended up with 28
#    subscribe-after-write races papered over with sleeps; phase 4 deleted the
#    last one, and no test in the tree dials a websocket now.
#
#    Test code only. The AppView's own connector (internal/atproto/jetstream/
#    connector.go) is the production consumer this suite exists to test — it
#    dials by definition, and counting it here would have meant carrying a
#    permanent floor of 1 that reviewers learn to ignore.
#
#    The pattern is every CLIENT-side entry point gorilla offers, not just the
#    package-level `DefaultDialer`: a hand-rolled `websocket.Dialer{}` (or
#    `var d websocket.Dialer`, or `new(websocket.Dialer)`) breaks the
#    single-subscriber rule identically and would have sailed past a
#    DefaultDialer-only grep — and egress isolation cannot catch it either,
#    because the Jetstream it would dial is INSIDE the stack. `Upgrader`,
#    `*websocket.Conn` and the message-type constants are deliberately not
#    matched: those are the server side, which the two fake-Jetstream test
#    servers legitimately use.
scan "websocket client dial in test code" \
    'websocket\.(DefaultDialer|Dialer|NewClient)' \
    test_code_files 'tests/testkit/' ''

# 5. Endpoint literals. Every address comes from testkit.Endpoints(), so there
#    is one place that decides whether a test talks to the CI stack or to a
#    developer's dev stack. The exempted residue is literals that are DATA — a
#    config parser's expected defaults, a URL builder's expected output — which
#    have to name the string to assert on it, and would be tautological if they
#    read it from the same source as the code under test.
scan "hardcoded host:port in test code" \
    '(localhost|127\.0\.0\.1):[0-9]{2,5}' \
    test_code_files 'tests/testkit/' 'coves:allow-host-literal'

# 6. Public atProto infrastructure. Hermetic means hermetic: the only tier
#    allowed to reach the real network is tests/live, where it is explicit and
#    opt-in. Production source legitimately names these hosts, so this counts
#    test code only.
#
#    Scope-aware, per §3.7.2: the target is endpoint construction, not fixture
#    data. A handle like "testuser.bsky.social" in a table-driven consumer test
#    is data that is never dialled, so only hostnames inside a URL with a
#    scheme are counted (92 pure-data matches drop out on the pattern alone).
#    What remains and is still legitimate — a URL parser's inputs, a config test
#    asserting the production default — carries a marker.
scan "public atProto hosts outside tests/live" \
    '(https?|wss?)://[^"'"'"'[:space:]]*(plc\.directory|bsky\.network|bsky\.social|bsky\.app)' \
    test_code_files 'tests/live/' 'coves:allow-public-host'

# 7. Raw request-body reads in production code. Every JSON request body is
#    read through internal/api/reqbody, which is the single place body size
#    caps, 413 semantics, and trailing-data rejection live. A bare
#    json.NewDecoder(r.Body), io.ReadAll(r.Body), or un-capped form parse is
#    an unbounded allocation an unauthenticated caller controls — the exact
#    DoS the reqbody package closed. reqbody is excluded by path because it
#    holds the one sanctioned json.NewDecoder(r.Body) call; anything else
#    that must read a body raw declares why with a marker (the delete-account
#    form in internal/web does, with its own MaxBytesReader cap). The
#    identifier alternation (r|req|request) is a tripwire, not a proof — a
#    body reached through an unusual variable name evades it, same as every
#    other grep in this file.
scan "raw request-body read outside reqbody" \
    'json\.NewDecoder\((r|req|request)\.Body\)|(io|ioutil)\.ReadAll\((r|req|request)\.Body\)|\.ParseForm\(\)|\.ParseMultipartForm\(' \
    production_go_files 'internal/api/reqbody/' 'coves:allow-raw-body-decode'

# 8. Discarded xrpc.DecodeJSON result. The wrapper returns bool, and an
#    ignored bool is invisible to errcheck and vet — but a call at statement
#    position has already written the 413/400 and the handler then continues
#    with a zero-valued request and double-writes a response. A used result
#    always appears after `if !` or an assignment, never at the start of a
#    statement, so this pattern has no false positives.
scan "unchecked xrpc.DecodeJSON result" \
    '^[[:space:]]*xrpc\.DecodeJSON\(' \
    production_go_files '' ''

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------

printf "  %-52s %6s %8s\n" "VIOLATION" "COUNT" "EXEMPT"
printf "  %-52s %6s %8s\n" "----------------------------------------------------" "------" "--------"
for row in "${ROWS[@]}"; do
    printf '%s\n' "$row"
done
printf "  %-52s %6s\n" "----------------------------------------------------" "------"
printf "  %-52s %6d\n" "TOTAL" "$TOTAL"
echo

if [[ ${#EXEMPT_FILES[@]} -gt 0 ]]; then
    printf "  ${CYAN}FILE-SCOPE EXEMPTIONS${RESET}  (declared in-file; printed every run)\n"
    printf "  %-58s %3s  %s\n" "FILE" "N" "REASON"
    for entry in "${EXEMPT_FILES[@]}"; do
        printf '%s\n' "$entry"
    done
    echo
fi

# The length guard is not decoration: under `set -u`, bash 3.2 — which is what
# ships on macOS, where developers run this — treats "${DETAILS[@]}" on an EMPTY
# array as an unbound variable and aborts. So `-v` on a clean tree, the run most
# likely to be someone's first, used to be the one run that crashed.
if [[ $VERBOSE -eq 1 && ${#DETAILS[@]} -gt 0 ]]; then
    for detail in "${DETAILS[@]}"; do
        printf "${YELLOW}%s${RESET}\n" "${detail%%$'\n'*}"
        printf '%s\n\n' "${detail#*$'\n'}" | sed 's/^/    /'
    done
fi

if [[ $TOTAL -eq 0 ]]; then
    printf "${GREEN}  All categories clear.${RESET}\n"
    echo
    exit 0
fi

printf "${RED}  %d violation(s). Fix them, or annotate each with a reason:${RESET}\n" "$TOTAL"
printf "    coves:allow-sleep / coves:allow-host-literal / coves:allow-public-host / coves:allow-raw-body-decode\n"
printf "    (append '-file:' for a whole-file exemption; see this script's header)\n"
printf "  Run with -v for file:line. t.Skip has no marker — see tests/ci/allowed_skips.txt.\n"
echo
exit 1
