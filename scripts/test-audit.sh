#!/usr/bin/env bash
# Counts test-suite invariant violations. Progress meter first, lint gate second.
#
# WHAT THIS IS FOR
#
# docs/TEST_ARCHITECTURE.md §3.6.3. Every category below is something the
# refactor is removing, and each has a phase that must drive it to zero. Running
# this after every change turns "will the final enforcement flip pass?" into a
# number you can watch instead of a hope.
#
# WARN MODE. This always exits 0. It prints a table and nothing else, so it can
# sit in the CI pipeline from day one without failing builds for violations
# that are scheduled to be fixed three phases from now. The final phase flips it
# to a hard failure, at which point every count must already be zero.
#
# THESE ARE TRIPWIRES, NOT PROOFS. Every check here is a grep, and a grep is
# bypassable by construction — a URL built by concatenation, a bare IP literal,
# a sleep hidden behind a helper. They catch drift cheaply. The actual guarantee
# that tests never reach the public network is the egress-blocked CI network
# (docker-compose.ci.yml, `internal: true`).
#
# SCOPE. "test code" means every *_test.go file under cmd/, internal/ and
# tests/, plus every .go file under tests/ — the shared helpers in
# tests/integration are test code too, and some of the worst offenders live
# there rather than in files ending _test.go.
#
# Whole-line comments are not counted. This tree explains itself at length, and
# a comment that mentions localhost:5434 while describing the stack is not a
# hardcoded endpoint. It is a deliberate undercount at the margin: a violation
# trailing a comment on the same line still counts, because the code is there.
#
# Usage:
#   scripts/test-audit.sh        summary table
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

# ---------------------------------------------------------------------------
# Scanning
# ---------------------------------------------------------------------------

TOTAL=0
declare -a ROWS=()
declare -a DETAILS=()

# scan <label> <pattern> <scope-fn> <path-exclude-ERE|""> <burns-down-in> [exemption-marker]
#
# grep -H is load-bearing: xargs splits its input into batches, and a batch that
# happens to contain exactly one file makes plain grep omit the filename. The
# comment-exclusion regex below anchors on "file:line:", so without -H it
# silently stops matching on those batches and the counts differ between a Mac
# (one batch) and the Alpine CI runner (several).
scan() {
    local label=$1 pattern=$2 scope=$3 exclude=$4 phase=$5 exemption=${6:-}
    local files hits count

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
        # Scoped to the category that defines it, so an exemption for a
        # hostname cannot quietly excuse a sleep on the same line.
        if [[ -n $exemption ]]; then
            hits=$(printf '%s\n' "$hits" | grep -v "$exemption" || true)
        fi
    fi

    count=$(printf '%s' "$hits" | grep -c . || true)
    TOTAL=$((TOTAL + count))
    ROWS+=("$(printf '  %-52s %6d   %s' "$label" "$count" "$phase")")

    if [[ $VERBOSE -eq 1 && $count -gt 0 ]]; then
        DETAILS+=("$label"$'\n'"$hits")
    fi
}

echo
printf "${CYAN}═══════════════════════════════════════════════════════════════════════${RESET}\n"
printf "${CYAN}  TEST SUITE VIOLATION AUDIT${RESET}   (warn mode — never fails the build)\n"
printf "${CYAN}═══════════════════════════════════════════════════════════════════════${RESET}\n"
echo

# 1. Sleeps. A sleep is a guess about timing that is wrong in both directions:
#    too short under CI load (flake), too long on an idle laptop (slow suite).
#    testkit.WaitFor / testkit.Holds replace every one of them. testkit itself is
#    exempt because it is where the polling loop is implemented.
scan "time.Sleep in test code" \
    'time\.Sleep\(' \
    test_code_files 'tests/testkit/' 'phase 3-4'

# 2. Skips. cmd/ci-report treats a skip as a failure unless it is allowlisted
#    with a reason; the allowlist shrinks to near-zero rather than absorbing
#    these. Environmental gating belongs to the harness, not to test bodies.
scan "t.Skip outside testkit" \
    '\.Skip(f|Now)?\(' \
    test_code_files 'tests/testkit/' 'phase 2'

# 3. testing.Short(). The tier mechanism becomes build tags, which are
#    compile-time and cannot be silently forgotten. -short silently skipped ~161
#    sites while printing green.
scan "testing.Short()" \
    'testing\.Short\(\)' \
    all_go_files '' 'phase 2'

# 4. Raw websocket dialing. The cursor-before-write ordering and the
#    consecutive-timeout guard are written exactly once, in testkit/firehose.go.
#    Ten hand-rolled copies is how the suite ended up with 28 subscribe-after-write
#    races papered over with sleeps.
scan "websocket.DefaultDialer outside testkit" \
    'websocket\.DefaultDialer' \
    all_go_files 'tests/testkit/' 'phase 4'

# 5. Endpoint literals. Every address comes from testkit.Endpoints(), so there
#    is one place that decides whether a test talks to the CI stack or to a
#    developer's dev stack.
scan "hardcoded host:port in test code" \
    '(localhost|127\.0\.0\.1):[0-9]{2,5}' \
    test_code_files 'tests/testkit/' 'phase 3-4'

# 6. Public atProto infrastructure. Hermetic means hermetic: the only tier
#    allowed to reach the real network is tests/live, where it is explicit and
#    opt-in. Production source legitimately names these hosts, so this counts
#    test code only.
#
#    Scope-aware, per §3.7.2: the target is endpoint construction, not fixture
#    data. A handle like "testuser.bsky.social" in a table-driven consumer test
#    is data that is never dialled, so only hostnames inside a URL with a
#    scheme are counted (92 pure-data matches drop out). What remains includes
#    legitimate cases — a URL parser's inputs, a config test asserting the
#    production default — which are exempted individually by putting
#    "coves:allow-public-host: <reason>" on the line, so each exemption is a
#    decision someone made rather than a hole in the pattern.
scan "public atProto hosts outside tests/live" \
    '(https?|wss?)://[^"'"'"'[:space:]]*(plc\.directory|bsky\.network|bsky\.social|bsky\.app)' \
    test_code_files 'tests/live/' 'phase 2' 'coves:allow-public-host'

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------

printf "  %-52s %6s   %s\n" "VIOLATION" "COUNT" "BURNS DOWN IN"
printf "  %-52s %6s   %s\n" "----------------------------------------------------" "------" "-------------"
for row in "${ROWS[@]}"; do
    printf '%s\n' "$row"
done
printf "  %-52s %6s\n" "----------------------------------------------------" "------"
printf "  %-52s %6d\n" "TOTAL" "$TOTAL"
echo

if [[ $VERBOSE -eq 1 ]]; then
    for detail in "${DETAILS[@]}"; do
        printf "${YELLOW}%s${RESET}\n" "${detail%%$'\n'*}"
        printf '%s\n\n' "${detail#*$'\n'}" | sed 's/^/    /'
    done
fi

if [[ $TOTAL -eq 0 ]]; then
    printf "${GREEN}  All categories clear.${RESET}\n"
else
    printf "${YELLOW}  Warn mode: these are tracked, not enforced. Run with -v for file:line.${RESET}\n"
fi
echo

# Always successful, by design. See the header.
exit 0
