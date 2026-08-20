#!/usr/bin/env bash
# Counts SSRF-guard regressions in PRODUCTION code. HARD GATE — a nonzero count
# fails.
#
# WHAT THIS IS FOR
#
# The shared guard protects attacker-influenced egress across the application;
# docs/SSRF_SECURITY.md describes its maintained security contract. This audit
# is the mechanism that stops a new fetch site appearing unguarded, and it exists
# because the ordinary test suite cannot identify every client construction.
#
# `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` — the hermetic merge gate,
# running T0+T1+T2 — takes the PERMISSIVE branch at every call site that holds an
# allow-private boolean. A green merge gate is therefore compatible with every
# guarded site being unguarded in production. The suite cannot see this; only a
# grep over the source can.
#
# It is also a grep because human enumeration was tried and measured: the
# remediation document's own list of call sites undercounted four separate times.
#
# THIS IS A SIBLING OF scripts/test-audit.sh, NOT A CATEGORY IN IT. That script
# audits TEST code and says so in its scope note ("Production sources are out of
# scope for every category except testing.Short()"). This one audits production
# sources exclusively. The exemption mechanism, output format and failure
# semantics are deliberately identical, so there is one convention to learn.
#
# THE FLOOR IS ZERO — with exemptions that are declared, not assumed.
#
# Every check here is a grep over text, and production code has legitimate
# reasons to build an HTTP client: a fixed vendor API, the AppView's own
# configured PDS, the guarded client's own construction. Those are exempted
# individually, in the source, with a reason — so each one is a decision someone
# made and can be found by grepping for the marker:
#
#     client := &http.Client{...} // coves:allow-bare-client: <reason>
#     opts := PrivateHostOptions(true) // coves:allow-ssrf-hatch: <reason>
#
# and, where per-line markers would be noise because the whole file's subject
# matter IS the thing being counted (the guard's own transport), a file-scope
# form declared once near the top of the file:
#
#     // coves:allow-bare-client-file: <reason>
#
# File-scope exemptions are printed on EVERY run with their reason and hit count,
# so a broad exemption cannot go quiet.
#
# WHAT THE MARKER ACTUALLY REQUIRES, stated because each of these was once looser
# than the paragraph above implied:
#
#   * IT MUST BE IN A COMMENT, quote-aware. A marker inside a string literal used
#     to silence the rule, which is what happens when a log message or an error
#     string quotes the convention while explaining it.
#   * IT MUST CARRY A NON-EMPTY REASON. The bare token used to be enough. A
#     reason nobody has to write is a reason nobody writes, and the reason is the
#     only part of an entry a reviewer can disagree with.
#   * IT APPLIES ON ITS OWN LINE OR THE LINE BELOW, and only to its own category:
#     a justification about a bare client cannot license a resolver override.
#   * IT MUST STILL HAVE SOMETHING TO EXEMPT. An exemption outliving its
#     violation is a standing grant that no rule reports, and the next unguarded
#     client written under it inherits a reason composed about different code.
#     Rule 5 counts those.
#
# ONE CAVEAT THE NAMES HIDE: `coves:allow-bare-client` is a marker FAMILY, not a
# per-rule marker. Rules 1, 1b, 1c, 1d, 1e, 1f and 1g all name it, so one marker
# — and especially one `-file:` marker — exempts a file from every bare-client
# rule at once, not from the one that fired. That is a real breadth, it is why
# the file-scope table prints on every run, and it is why splitting a file is
# usually the better fix than broadening its exemption.
#
# IT IS ALSO WHY 1c, 1e AND 1f REFUTE THE DEFAULT BY NAME. Those three fire on a
# literal's OPENING line while the value they object to sits on a later line, so
# a marker written about the bare client on that later line exempts rule 1 or 1b
# and leaves the literal reading as remediated — one reason, written about one of
# the two rules, silencing both. The refuted list is what keeps the second
# question separate from the first.
#
# AN ALLOWLIST ENTRY IS A DOCUMENTED DECISION TO BE UNGUARDED. That is the whole
# weight of the marker, and it is why an unlisted occurrence fails the build
# rather than being snapshotted into a baseline: a wrong entry is worse than a
# noisy audit, because it converts an oversight into a recorded choice nobody
# revisits.
#
# THESE ARE TRIPWIRES, NOT PROOFS. A grep is bypassable by construction — a
# client built by a helper in another package, a transport assigned after
# construction. They catch drift cheaply. The actual guarantee is that each
# converted site's own guard test refuses a private address.
#
# WHAT IS KNOWINGLY NOT COVERED, because a false claim of coverage is worse than
# a documented hole — the hole gets read, the claim gets trusted:
#
#   * PRODUCTION GO OUTSIDE cmd/ AND internal/. The corpus is those two roots and
#     nothing else. Today that leaves scripts/setup_dev_aggregator.go unscanned —
#     `go list` reports it as a real package, and it reaches the network twice
#     with http.Post — so this is a live hole and not a theoretical one.
#
#     Widening the corpus is the obvious fix and cannot be done from this file:
#     the widened set immediately contains violations, and the markers that would
#     settle them have to be written in the files themselves. What IS enforced is
#     that the roots exist and hold production Go (rule 0), because the failure
#     that hides everything is not a file outside the corpus, it is a corpus of
#     no files reporting green over the whole tree.
#
#   * A METHOD CALL ON AN ALREADY-BUILT CLIENT. `c.Do(req)` and `c.Get(url)` are
#     how every CONVERTED site uses its guarded client; today the tree holds 26
#     `.Do(` and 141 `.Get(` calls in production code, almost all of them
#     correct. There is no text distinguishing a call on a guarded client from a
#     call on an unguarded one, so a rule here would be a permanent floor of ~167
#     over exactly the code the remediation fixed — the count reviewers learn to
#     read past.
#
#     THE SHAPES ARE CAUGHT AT CONSTRUCTION INSTEAD — but only the constructions
#     named here, and the claim is worth no more than its list. Rule 1 covers the
#     struct literal, new(), DefaultClient, the package-level helpers, and the
#     zero-value client however it is declared (`var c http.Client`, a struct
#     field, a parameter). Rule 1b covers the transport underneath and the
#     single-host proxy constructor, rule 1c the xrpc.Client that names no client
#     at all, rule 1e the reverse proxy assembled by hand, rule 1f the identity
#     directory whose client field is a VALUE (so omitting it compiles and yields
#     a zero-value client rather than a nil-pointer panic), rule 1g the four
#     library constructors that install http.DefaultClient for you, and rule 1d
#     the aliased import that renames past all of them.
#
#     NOT COVERED AT CONSTRUCTION: any OTHER library type with an optional
#     `*http.Client`, `http.Client` or `Transport` field. The list in rules
#     1c/1e/1f/1g is an ENUMERATION OF THIS TREE, not a rule that generalises —
#     it grew from two entries to seven because four more were already here when
#     someone looked, which is the honest measure of how well the enumeration
#     tracks the code. A type this list does not name is invisible.
#
#     WHAT MAKES THAT SURVIVABLE IS THAT THE HATCH IS NOT ENUMERATED. Rule 3 asks
#     about the field assignment every hatch performs, so a new subsystem's guard
#     can be spelled however it likes and its OFF switch is still counted. The
#     enumerated rules are the tripwires; that one is the mechanism.
#
#   * A CLIENT BUILT BY A HELPER IN ANOTHER PACKAGE and returned. Nothing at the
#     call site says what it is. Rule 1 catches the helper's own construction,
#     which is inside this tree; a client from a VENDORED dependency is not
#     visible to any rule here.
#
#   * A TRANSPORT ASSIGNED AFTER CONSTRUCTION — `c.Transport = tr` where tr came
#     from elsewhere.
#
# The aliased-import case IS covered, by rule 1d, and not at the call site: the
# call cannot be greppable (`nh.Get(url)` has no distinguishing text) but the
# import that creates the alias must exist, and there is no reason in this tree
# to rename net/http.
#
# Usage:
#   scripts/ssrf-audit.sh        summary table; exits nonzero on any violation
#   scripts/ssrf-audit.sh -v     table plus every offending file:line
#
# Environment:
#   SSRF_AUDIT_ROOT   directory to scan instead of the repository root.
#
#     This is a test seam, and it is the reason this script has tests at all.
#     An audit rule that cannot fail is the most useless artifact in the
#     repository, and asserting that a rule bites is not the same as watching it
#     bite; tests/audit plants a violating fixture under a temporary root and
#     requires a nonzero exit. It cannot weaken any rule — whatever root it
#     names is scanned by the identical pass the repository root goes through.
set -uo pipefail

if [[ -n ${SSRF_AUDIT_ROOT:-} ]]; then
    REPO_ROOT=$SSRF_AUDIT_ROOT
else
    REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fi
cd "$REPO_ROOT" || exit 2

VERBOSE=0
if [[ ${1:-} == "-v" || ${1:-} == "--verbose" ]]; then
    VERBOSE=1
fi

CYAN='\033[36m'
YELLOW='\033[33m'
GREEN='\033[32m'
RED='\033[31m'
RESET='\033[0m'

TOTAL=0
ALL_PATTERNS=""
ALL_MARKERS=""
declare -a ROWS=()
declare -a DETAILS=()
declare -a EXEMPT_FILES=()

echo
printf "${CYAN}═══════════════════════════════════════════════════════════════════════${RESET}\n"
printf "${CYAN}  SSRF GUARD REGRESSION AUDIT${RESET}   (hard gate — any violation fails)\n"
printf "${CYAN}═══════════════════════════════════════════════════════════════════════${RESET}\n"
echo

# ---------------------------------------------------------------------------
# File scope
# ---------------------------------------------------------------------------

# Production code: every Go file under cmd/ and internal/ that is not a test.
#
# TEST FILES ARE OUT OF SCOPE, and not as a convenience. A guard test has to
# stand up an httptest server and dial it, and httptest listens on LOOPBACK —
# exactly the address class the guard refuses. Auditing test files here would
# make the guard's own tests unwritable, and tests/ is where the hatch and the
# resolver seam are SUPPOSED to appear.
#
# tests/ is excluded for the same reason even though tests/testkit holds
# non-_test.go files: it is test support, and scripts/test-audit.sh already owns
# that tree.
PROD_ROOTS=(cmd internal)

prod_code_files() {
    find "${PROD_ROOTS[@]}" -type f -name '*.go' 2>/dev/null |
        grep -v '_test\.go$' | sort -u
}

# ---------------------------------------------------------------------------
# Reading a line the way Go reads it
# ---------------------------------------------------------------------------

# split_go_line code|comment
#
# Filters stdin, printing for each line either the code before its `//` comment
# or the comment itself. Line count is preserved, so a caller can still address
# lines by number after filtering.
#
# THE QUOTE TRACKING IS THE POINT, and it is why this is not `sed 's|//.*||'`.
# Three separate checks in this script ask "does this line's TEXT contain X",
# and text is not the same question as code:
#
#   * `Client:` written in a COMMENT inside an xrpc.Client literal satisfied the
#     remediation check for rule 1c. An audit satisfied by a note about the fix
#     is measuring its own documentation — the same defect rule 4 shipped with,
#     where the Dockerfile's explanation of CGO_ENABLED=0 satisfied the check for
#     CGO_ENABLED=0.
#   * An exemption marker written inside a STRING silenced a rule, which is what
#     happens when a log line or an error message quotes the convention.
#   * `"https://example.com"` is in nearly every literal this script reads, so a
#     naive comment strip would cut a line in half at the URL's slashes and take
#     any brace after it with it — miscounting the literal.
split_go_line() {
    awk -v mode="$1" '
        function comment_at(s,   i, n, c, q, inq, inraw) {
            n = length(s)
            for (i = 1; i <= n; i++) {
                c = substr(s, i, 1)
                if (inraw) { if (c == "`") { inraw = 0 }; continue }
                if (inq) {
                    if (c == "\\") { i++; continue }
                    if (c == q) { inq = 0 }
                    continue
                }
                if (c == "`") { inraw = 1; continue }
                if (c == "\"" || c == "\047") { inq = 1; q = c; continue }
                if (c == "/" && substr(s, i + 1, 1) == "/") { return i }
            }
            return 0
        }
        {
            at = comment_at($0)
            if (mode == "code") { print (at ? substr($0, 1, at - 1) : $0) }
            else { print (at ? substr($0, at) : "") }
        }
    '
}

# marker_on_line <file> <line> <marker>
#
# True when <line> carries <marker> IN A COMMENT and with a non-empty reason
# after the colon.
#
# Both halves are load-bearing, and neither was checked. The old test was a
# fixed-string grep for the marker without its trailing colon, anywhere on the
# line — so `// coves:allow-bare-client` with nothing after it silenced the rule
# as well as a justification would, and so did the same text inside a string.
#
# The reason is not decoration: the header stakes the whole allowlist on an entry
# being a DOCUMENTED DECISION, and the reason is the only part of one a reviewer
# can disagree with. A reason nobody has to write is a reason nobody writes.
marker_on_line() {
    local file=$1 line=$2 marker=$3

    [[ $line -ge 1 ]] || return 1
    sed -n "${line}p" "$file" 2>/dev/null | split_go_line comment |
        grep -qE "${marker}:[[:space:]]*[^[:space:]]"
}

# ---------------------------------------------------------------------------
# Scanning
# ---------------------------------------------------------------------------

# composite_literal_body <file> <line>
#
# Prints the composite literal that OPENS on <line>, from that line through the
# line where its braces balance again. Used by the fifth argument to scan, so a
# rule can ask about the literal's contents rather than about one line's text.
#
# Brace counting rather than a fixed window, and rather than stopping at the
# first closing brace, because these literals nest: every xrpc.Client in this
# tree holds an &xrpc.AuthInfo{...} whose `},` closes several lines before the
# outer one does. A window that stopped there would miss any field written after
# it and report the remediated form as a violation — which is the failure that
# teaches people the audit is wrong.
#
# COMMENTS ARE STRIPPED BEFORE THE LITERAL IS READ, which is the difference
# between asking what a literal DOES and asking what it SAYS. `Client:` written
# in a comment inside an xrpc.Client literal used to satisfy rule 1c's
# remediation check, so the literal that merely mentions the fix passed as though
# it had applied it. Stripping also keeps a brace inside a comment out of the
# depth count.
composite_literal_body() {
    local file=$1 start=$2

    split_go_line code < "$file" 2>/dev/null | awk -v start="$start" '
        NR < start { next }
        {
            body = $0
            # Everything before the opening brace belongs to the enclosing
            # expression; counting it would start the depth in the wrong place.
            if (NR == start) { sub(/^[^{]*/, "", body) }
            print $0
            depth += gsub(/\{/, "{", body) - gsub(/\}/, "}", body)
            if (depth <= 0) { exit }
        }
    '
}

# scan <label> <pattern> <exemption-marker> [structural-exclude-ERE] [satisfied-ERE]
#
# THE FIFTH ARGUMENT IS NOT A FOURTH KIND OF EXEMPTION. A hit that matches it is
# not excused, it is COMPLIANT — the rule asked a question about the composite
# literal and the answer was the right one. That is why it is neither counted nor
# reported as exempt: an xrpc.Client handed the guarded client is not a
# documented decision to be unguarded, it is a guarded site.
#
# grep -H is load-bearing: xargs splits its input into batches, and a batch that
# happens to contain exactly one file makes plain grep omit the filename. The
# comment-exclusion regex below anchors on "file:line:", so without -H it
# silently stops matching on those batches and the counts differ between a Mac
# (one batch) and the Alpine CI runner (several).
#
# WHOLE-LINE COMMENTS ARE NEVER COUNTED. This tree explains itself at length, and
# several converted sites name the bare client they replaced —
# internal/core/blobs/service.go and internal/core/users/service.go both do.
# Counting those would mean the remediation's own documentation failing the gate
# the remediation added. A violation TRAILING a comment on the same line still
# counts, because the code is there.
#
# THE STRUCTURAL EXCLUDE IS FOR DECLARATIONS, NOT FOR EXCEPTIONS. `func
# WithHostResolver(...)` names the identifier because it IS the identifier; a
# rule that counted it would carry a permanent floor, and a count reviewers learn
# to read past is a count that stops being read. A declaration is excluded by its
# shape rather than by a marker precisely because it is not a decision anyone
# made — there is no reason to write down.
#
# THE SIXTH ARGUMENT IS THE FIFTH ONE'S RETRACTION. A literal can name the field
# the rule asked about and still be the unremediated form — `Client: nil` is not
# a client injected, it is indigo's unguarded util.RobustHTTPClient() requested
# by name, because getClient() substitutes it for a NIL field. The spelling that
# most looks like someone considered the problem was the one that defeated the
# check. Where a field has a nil meaning, the rule has to say so explicitly.
scan() {
    local label=$1 pattern=$2 exemption=$3 structural=${4:-} satisfied=${5:-} refuted=${6:-}
    local files raw kept count exempt=0 file reason fhits body

    # Recorded for the stale-marker rule, which needs to know whether a marker
    # still has anything to exempt and must not carry its own copy of the
    # patterns to answer that.
    ALL_PATTERNS+="${ALL_PATTERNS:+|}($pattern)"
    case "|${ALL_MARKERS}|" in
    *"|${exemption}|"*) ;;
    *) ALL_MARKERS+="${ALL_MARKERS:+|}${exemption}" ;;
    esac

    files=$(prod_code_files)

    raw=""
    if [[ -n $files ]]; then
        raw=$(printf '%s\n' "$files" | tr '\n' '\0' |
            xargs -0 grep -HnE "$pattern" 2>/dev/null || true)
    fi
    raw=$(printf '%s\n' "$raw" | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' || true)
    if [[ -n $structural ]]; then
        raw=$(printf '%s\n' "$raw" | grep -vE "$structural" || true)
    fi

    # File scope first: a file that declares the exemption drops out wholesale,
    # and is reported by name so the breadth stays visible.
    for file in $(printf '%s\n' "$raw" | sed -n 's/^\([^:]*\):[0-9]*:.*$/\1/p' | sort -u); do
        reason=$(split_go_line comment < "$file" 2>/dev/null |
            grep -m1 -oE "${exemption}-file:[[:space:]]*[^[:space:]].*" |
            sed "s/${exemption}-file:[[:space:]]*//")
        [[ -n $reason ]] || continue
        fhits=$(printf '%s\n' "$raw" | grep -c "^${file}:" || true)
        [[ $fhits -eq 0 ]] && continue
        EXEMPT_FILES+=("$(printf '  %-58s %3d  %s' "$file" "$fhits" "$reason")")
        exempt=$((exempt + fhits))
        raw=$(printf '%s\n' "$raw" | grep -v "^${file}:" || true)
    done

    # Then per line. The marker counts on the offending line OR on the line
    # directly above it, because Go's own convention puts an explanation above
    # the statement it explains and a reason worth writing is usually longer than
    # the tail of a line. Scoped to the category that defines the marker, so a
    # justification written about a bare client cannot quietly license a resolver
    # override it was never written for.
    kept=""
    while IFS= read -r hit; do
        [[ -z $hit ]] && continue

        local hfile hline rest
        hfile=${hit%%:*}
        rest=${hit#*:}
        hline=${rest%%:*}

        if marker_on_line "$hfile" "$hline" "$exemption" ||
            marker_on_line "$hfile" "$((hline - 1))" "$exemption"; then
            exempt=$((exempt + 1))
            continue
        fi

        if [[ -n $satisfied ]]; then
            body=$(composite_literal_body "$hfile" "$hline")
            if printf '%s\n' "$body" | grep -qE "$satisfied" &&
                { [[ -z $refuted ]] || ! printf '%s\n' "$body" | grep -qE "$refuted"; }; then
                continue
            fi
        fi

        kept+="$hit"$'\n'
    done <<< "$raw"

    count=$(printf '%s' "$kept" | grep -c . || true)
    TOTAL=$((TOTAL + count))
    ROWS+=("$(printf '  %-52s %6d %8d' "$label" "$count" "$exempt")")

    if [[ $VERBOSE -eq 1 && $count -gt 0 ]]; then
        DETAILS+=("$label"$'\n'"$(printf '%s' "$kept")")
    fi
}

# 0. THE CORPUS ITSELF. Every rule below is a grep, and a grep is only as real as
#    the file list it runs over. `find cmd internal` over a tree where either
#    directory has been renamed, moved or emptied returns nothing, every rule then
#    counts zero over zero files, and the script prints "All categories clear" and
#    exits 0 — byte-identical to the output over a clean tree.
#
#    That is the highest-leverage failure available here, because it disables
#    EVERY rule at once and it is reached by an ordinary refactor rather than by
#    anything anyone would review as security-relevant.
#
#    THE CHOICE MADE HERE IS TO FAIL LOUDLY ON A MISSING ROOT, NOT TO WIDEN THE
#    CORPUS. Widening to every non-test .go in the tree would pull in
#    scripts/setup_dev_aggregator.go, which `go list` reports as a real package
#    and which reaches the network twice with http.Post — so widening cannot be
#    done from this script alone; it needs markers written in a file this rule
#    does not own. The consequence is stated plainly in the header's
#    not-covered list rather than left for a reader to infer.
#
#    A root is required to EXIST and to hold at least one production Go file. The
#    existence check alone would pass a directory emptied down to its _test.go
#    files, which reaches the same zero-file corpus by a different route.
corpus_count=0
corpus_detail=""
for root in "${PROD_ROOTS[@]}"; do
    if [[ ! -d $root ]]; then
        corpus_count=$((corpus_count + 1))
        corpus_detail+="  ${root}/: audited source root is missing."$'\n'
        corpus_detail+="  Every rule in this audit greps the files under it; with the directory"$'\n'
        corpus_detail+="  gone they all count zero and the audit reports green over code nobody"$'\n'
        corpus_detail+="  scanned. Restore the path, or update PROD_ROOTS in this script."$'\n'
    elif [[ -z $(find "$root" -type f -name '*.go' 2>/dev/null | grep -v '_test\.go$' | head -1) ]]; then
        corpus_count=$((corpus_count + 1))
        corpus_detail+="  ${root}/: audited source root holds no production Go files."$'\n'
        corpus_detail+="  A root emptied down to its _test.go files gives every rule the same"$'\n'
        corpus_detail+="  zero-file corpus as a missing directory, and the same silent green."$'\n'
    fi
done
ROWS+=("$(printf '  %-52s %6d %8d' "audited source root missing or empty" "$corpus_count" 0)")
TOTAL=$((TOTAL + corpus_count))
if [[ $VERBOSE -eq 1 && $corpus_count -gt 0 ]]; then
    DETAILS+=("audited source root missing or empty"$'\n'"${corpus_detail%$'\n'}")
fi

# 1. Bare HTTP clients. Every one of them dials whatever it is handed, and at
#    thirteen sites in this tree what it was handed came from a DID document, a
#    firehose record, a database column or a pasted link. The shared client in
#    internal/atproto/oauth resolves the host, classifies every answer, and dials
#    only what it vetted.
#
#    FIVE SHAPES, because the obvious two are not the ones that hide. The struct
#    literal is what twelve converted sites looked like; new(http.Client) is the
#    same object with one character's difference; http.DefaultClient is worse
#    than either, since its timeout is ZERO and zero in net/http means wait
#    forever. And http.Get/Post/Head/PostForm are DefaultClient under a name that
#    mentions no client at all — which is how cmd/reindex-votes/main.go reached
#    the network twice without matching any grep for `http.Client`. The most
#    convenient way to write an unguarded fetch must not be the one way the fence
#    cannot see.
#
#    The zero-value client is the fifth and the quietest: no brace, no call, no
#    ampersand, and every field's zero value works — including a Timeout of zero,
#    which is the same wait-forever as DefaultClient. It is ordinary Go that a
#    reviewer reads straight past.
#
#    IT IS MATCHED BY THE WHITESPACE BEFORE THE TYPE, NOT BY `var`, because the
#    keyword is the one part of the shape that is optional. `var c http.Client`
#    in a function body, `hc http.Client` as a struct field and `c http.Client`
#    as a parameter are the same zero-value client with the same absent guard,
#    and requiring `var` saw only the first.
#
#    THE POINTER FORM IS DELIBERATELY NOT MATCHED. `*http.Client` in a field, a
#    parameter or a return type is how a GUARDED client is CARRIED — the blobs,
#    users and unfurl services each hold one — so flagging the type's every
#    appearance would put a floor under the audit the size of the remediation.
#    Requiring whitespace IMMEDIATELY before `http.Client` is what separates the
#    two: a pointer, a slice and an address-of each put a character there, and
#    that character is the difference between declaring a client and being handed
#    one.
#
#    Anything unmarked fails the build. A snapshot of today's set would document
#    nothing, and the tenth site got in precisely by being unremarkable.
scan "bare HTTP client in production code" \
    '(http\.Client\{|new\(http\.Client\)|http\.DefaultClient|http\.(Get|Post|Head|PostForm)\(|[A-Za-z_][A-Za-z0-9_]*[[:space:]]+http\.Client([^A-Za-z0-9_]|$))' \
    'coves:allow-bare-client'

# 1b. The transport underneath the client, and the proxy that hides one.
#
#     A Transport is what actually dials; a Client is a policy wrapper around
#     one. So `&http.Transport{}` reaches the network with no address
#     classification whether or not a Client is ever built around it, and
#     `tr.RoundTrip(req)` skips the Client layer entirely. http.DefaultTransport
#     is the same object shared process-wide.
#
#     The shared guard IS a Transport, and that is the tell rather than an
#     awkwardness: anything else in this tree constructing one is constructing
#     the thing the guard replaces. The guard's own two lines carry markers.
#
#     httputil.NewSingleHostReverseProxy is here because it names neither a
#     client nor a transport, builds its own, and forwards to whatever URL it is
#     handed — a proxy over a caller-supplied target is SSRF with a router in
#     front of it.
#
#     new(http.Transport) is here because rule 1 above covers BOTH `http.Client{`
#     and `new(http.Client)` while this rule covered only the brace form — an
#     asymmetry with no reason behind it, found by a test that passed for the
#     wrong reason. The header's own argument for new(http.Client), "the same
#     object with one character's difference", is not weaker one layer down.
scan "bare HTTP transport or proxy in production code" \
    '(http\.Transport\{|new\(http\.Transport\)|http\.DefaultTransport|\.RoundTrip\(|httputil\.NewSingleHostReverseProxy\()' \
    'coves:allow-bare-client'

# 1g. A LIBRARY CONSTRUCTOR THAT INSTALLS http.DefaultClient FOR YOU. This is the
#     rest of the class rules 1c, 1e and 1f each cover one member of, and it is
#     the half none of them can reach: those three ask about a composite literal,
#     and a constructor call has no literal to ask about.
#
#     Every entry below was read in the dependency source rather than assumed,
#     because the whole point is that the call site says nothing:
#
#       oauth.NewClientApp        auth/oauth/oauth.go:55   Client: http.DefaultClient
#       atclient.NewAPIClient     atclient/apiclient.go:43 Client: http.DefaultClient
#       atclient.LoginWithPasswordHost                     calls NewAPIClient, and
#                                 atclient/password_auth.go:255-265 POSTS THE
#                                 CLEARTEXT PASSWORD through it before returning
#       retryablehttp.NewClient   client.go:431            HTTPClient: cleanhttp.DefaultPooledClient()
#
#     So `apiClient := atclient.NewAPIClient(host)` is `http.DefaultClient` — no
#     guard, and a Timeout of zero meaning wait forever — written without the
#     words http, Client or Transport appearing anywhere on the line.
#
#     THE REMEDIATION IS ON THE NEXT LINE AND NO GREP CAN SEE IT. Each of the
#     four sites assigns the guarded client immediately after constructing, and
#     `apiClient.Client = newBearerHTTPClient(...)` is textually indistinguishable
#     from an assignment of anything else. That is why this rule takes a MARKER
#     rather than a satisfied-check: the marker's reason is where the replacement
#     line gets named, which is the only place a reviewer can check it. DELETING
#     the assignment leaves the marker's reason false — still better than today,
#     where deleting it leaves nothing at all.
#
#     Matched through any package qualifier for the three indigo constructors,
#     whose names are distinctive enough to carry it. retryablehttp is named
#     outright: `NewClient` is far too common a spelling to match on its own, so
#     the trade here is a rule that misses an aliased import rather than a rule
#     with a floor under every SDK in the tree.
scan "library constructor with a built-in unguarded client" \
    '([A-Za-z_][A-Za-z0-9_]*\.(NewClientApp|NewAPIClient|LoginWithPasswordHost)\(|retryablehttp\.NewClient\()' \
    'coves:allow-bare-client'

# 1c. xrpc.Client with no client injected — the shape that contains no
#     `http.Client` text anywhere, and so could never match a rule above.
#
#     github.com/bluesky-social/indigo/xrpc.Client has an OPTIONAL
#     `Client *http.Client` field. Its getClient() (xrpc/xrpc.go:31-36) returns
#     util.RobustHTTPClient() when that field is nil — an unguarded client that
#     additionally RETRIES, so a refusal-shaped failure is tried again rather
#     than surfaced. Four production sites are built this way, two of them
#     carrying live credentials: a refresh token in communities/token_refresh.go
#     and a cleartext account password and email in pds_provisioning.go. The nil
#     field is what sends those to whatever host a record named.
#
#     THE RULE ASKS ABOUT THE LITERAL, NOT THE LINE. xrpc.Client is not forbidden
#     — leaving its transport to a library default is. A literal that sets
#     Client: is the remediated form and is not counted, so the fix satisfies the
#     rule directly instead of the four sites ending up carrying markers.
#     AND `Client: nil` IS NOT SETTING IT. getClient() substitutes
#     RobustHTTPClient() for a NIL field, so the explicit nil asks for the
#     unguarded retrying default BY NAME while spelling the exact text the rule
#     was looking for — the shape that most looks like someone considered the
#     problem was the one shape that defeated the check. A `Client:` written in a
#     COMMENT inside the literal satisfied it too, which is why the literal is
#     read with comments stripped.
#
#     THE REFUTED LIST IS EVERY SPELLING OF "THE DEFAULT, BY NAME", not just
#     `nil`. `Client: new(http.Client)` and `Client: &http.Client{}` are a bare
#     unguarded client handed over deliberately; both name the field, so both
#     used to read as remediated. Rule 1 does match those two spellings on their
#     own line — but a single `coves:allow-bare-client` marker there exempts rule
#     1 AND satisfies this one, so the site disappears from both counts on one
#     reason written about neither. Naming them here means this rule still has to
#     be answered separately.
scan "xrpc.Client with no guarded client injected" \
    'xrpc\.Client\{' \
    'coves:allow-bare-client' \
    '' \
    '(^|[^A-Za-z0-9_])Client:' \
    '(^|[^A-Za-z0-9_])Client:[[:space:]]*(nil|new\(http\.Client\)|&?http\.Client\{[[:space:]]*\})([^A-Za-z0-9_]|$)'

# 1e. A reverse proxy assembled by hand, which is the constructor above with its
#     one distinguishing identifier removed.
#
#     httputil.NewSingleHostReverseProxy is four lines around this struct, so
#     `&httputil.ReverseProxy{Director: ...}` is the same object reached by a
#     spelling rule 1b cannot see. Its Transport field is OPTIONAL and a nil
#     Transport means http.DefaultTransport — so a literal mentioning no client,
#     no transport and no constructor is a DefaultTransport-backed proxy over
#     whatever URL its Director was handed.
#
#     This one asks about the literal rather than requiring a marker, because
#     unlike a bare http.Transport a proxy has a remediated form: hand it the
#     guarded round tripper. `Transport: nil` is refuted for the same reason it
#     is on rule 1c — it is the default requested by name.
scan "reverse proxy with no guarded transport" \
    'httputil\.ReverseProxy\{' \
    'coves:allow-bare-client' \
    '' \
    '(^|[^A-Za-z0-9_])Transport:' \
    '(^|[^A-Za-z0-9_])Transport:[[:space:]]*(nil|new\(http\.Transport\)|&?http\.Transport\{[[:space:]]*\}|http\.DefaultTransport)([^A-Za-z0-9_]|$)'

# 1f. An identity directory handed no client, which is the same handoff as 1c
#     with one difference that changes the failure mode completely: the field is
#     a VALUE.
#
#     indigo's atproto/identity.BaseDirectory carries `HTTPClient http.Client`,
#     not a pointer. So a literal that OMITS the field compiles, and what it gets
#     is a zero-value http.Client — http.DefaultTransport underneath and a
#     Timeout of zero, which in net/http means wait forever. There is no nil to
#     panic on and no error to notice: the omission is silent at compile time,
#     silent at run time, and the directory then resolves whatever DID document
#     or handle it is handed. This tree fetches those from a firehose record and
#     from the `iss` of an UNVERIFIED inbound service JWT.
#
#     THE TYPE NAME IS MATCHED THROUGH ANY PACKAGE QUALIFIER, because this tree
#     spells the import three different ways in three files — `identity`,
#     `indigoidentity` and `indigoIdentity` — and a rule naming one of them is a
#     rule two of the three sites walk past. That is the same defect as rule 1d's
#     aliased import, reached without renaming anything: the alias was always
#     free to differ per file.
#
#     Like 1c and 1e this asks about the literal rather than requiring a marker,
#     because a directory is not forbidden — handing it the default client is.
#     The refuted list spells the zero value, which is what "nil" means for a
#     value-typed field.
scan "identity directory with no guarded client injected" \
    '[A-Za-z_][A-Za-z0-9_]*\.BaseDirectory\{' \
    'coves:allow-bare-client' \
    '' \
    '(^|[^A-Za-z0-9_])HTTPClient:' \
    '(^|[^A-Za-z0-9_])HTTPClient:[[:space:]]*(http\.Client\{[[:space:]]*\}|\*?new\(http\.Client\)|\*http\.DefaultClient)([^A-Za-z0-9_]|$)'

# 1d. An aliased or dot import of net/http, which renames its way past every
#     pattern above at once.
#
#     Each rule here spells the package `http`, so `import nh "net/http"` makes
#     `nh.Get(url)`, `nh.Client{}` and `nh.DefaultClient` all invisible. The CALL
#     cannot be caught — `nh.Get(url)` has no text distinguishing it from any
#     other package's Get — but the import creating the alias must exist, and it
#     is one line in a fixed position. Catching the line that must be there beats
#     chasing the many that need not be.
#
#     There is no reason in this tree to rename net/http, so the rule has no
#     legitimate form to spare. A dot import is worse still: it makes the calls
#     `Get(url)` with no package qualifier at all.
#
#     The structural exclude keeps `import "net/http"` — the single-line
#     unaliased form — out, where the token before the path is the keyword rather
#     than an alias. `import nh "net/http"` still counts: the exclude requires
#     the path to follow the keyword directly.
scan "aliased or dot import of net/http" \
    '[A-Za-z_.][A-Za-z0-9_]*[[:space:]]+"net/http"' \
    'coves:allow-bare-client' \
    '^[^:]+:[0-9]+:[[:space:]]*import[[:space:]]+"net/http"'

# 2. The DNS seam outside test code. oauth.WithHostResolver lets a package prove
#    its own guard hermetically — necessary because egress is blocked in the
#    hermetic tiers and nothing here can write /etc/hosts, so a caller whose
#    validator refuses IP literals has no other way to reach its own classifier.
#
#    It CANNOT open the guard: whatever it answers is classified by the same pass
#    a real DNS answer goes through, and the dial still reaches only addresses
#    that survived it. What it CAN do is stop production consulting real DNS, and
#    there is no legitimate reason for the AppView to resolve names from a table.
#
#    Its own declaration is excluded structurally. A production CALL is meant to
#    fail here — if one is ever justified, that is a conversation and not an
#    annotation.
scan "SSRF DNS seam outside test code" \
    'WithHostResolver\(' \
    'coves:allow-dns-seam' \
    '^[^:]+:[0-9]+:[[:space:]]*func[[:space:]]'

# 3. The guard's hatch outside dev-gated wiring. The implementation replaced a positional
#    boolean with named options for exactly this reason: `NewSSRFSafeHTTPClient(true)`
#    was ungreppable and said nothing at a call site about being the difference
#    between a guarded client and an unguarded one.
#
#    The legitimate production form is a wiring line deriving the value from
#    cfg.IsDevEnv — `PrivateHostOptions(a.cfg.IsDevEnv)` — which matches nothing
#    here. A literal `true` is a client that is unguarded in production and is
#    meant to fail.
#
#    THE THIRD ALTERNATIVE IS THE MECHANISM; THE FIRST TWO ARE ITS CONVENTIONAL
#    SPELLINGS, and the ordering of that sentence is the whole lesson of this
#    rule's history. Every hatch in this tree is ultimately one assignment —
#    `s.allowPrivateHosts = true` — and the option function is only the
#    conventional wrapper around it. A rule that knew the wrapper's NAME was
#    beaten twice by the tree it was already running over:
#
#      * jetstream/authorpost.go declares `withPrivateHostsAllowed()`, unexported
#        for good reasons of its own. One lowercase letter, and the hatch, its
#        gate helper and its allow-branch were all invisible to this rule.
#      * the same file's gate helper is `PrivatePostFetcherOptions`, which is not
#        `Private(Address|Host)Options` either, so `PrivatePostFetcherOptions(true)`
#        was a hardcoded hatch that matched nothing.
#
#    Neither was written to evade anything; both are ordinary Go. That is the
#    point — a fence keyed on a convention is beaten by a spelling, without
#    intent and without anyone noticing. The field assignment is what the
#    compiler acts on, so it is what the rule asks about, and the two name
#    patterns are widened to case-insensitive initials and a free middle so the
#    conventional spellings stay covered for the same reason belt and braces are
#    both worn.
#
#    Each package's option declaration is excluded structurally, so the nine
#    `func WithPrivateHostsAllowed()` lines carry no floor. What the assignment
#    alternative DOES cost is that each option BODY now matches: nine of them are
#    one-liners whose `func` line above carries the marker already, which is why
#    the marker window is the line and the line above. An option body spread over
#    three lines puts the assignment out of that window and fails — deliberately,
#    since a hatch that no longer looks like the other nine is worth one review.
scan "SSRF hatch outside dev-gated wiring" \
    '([Ww]ithPrivate[A-Za-z0-9_]*Allowed\(\)|[Pp]rivate[A-Za-z0-9_]*Options\(true\)|[Aa]llowPrivate[A-Za-z0-9_]*[[:space:]]*(:=|:|=)[[:space:]]*true([^A-Za-z0-9_]|$))' \
    'coves:allow-ssrf-hatch' \
    '^[^:]+:[0-9]+:[[:space:]]*func[[:space:]]'

# 5. EXEMPTIONS THAT NO LONGER EXEMPT ANYTHING. The rules above ask what the code
#    does; this one asks whether the allowlist still describes it.
#
#    A marker whose violation was fixed or deleted is not harmless leftover. It is
#    a standing grant that no rule can see — there is nothing there to match, so
#    nothing reports it — and the next bare client written under it silently
#    inherits a justification composed about different code. That is exactly the
#    failure the header calls worse than a noisy audit: a wrong entry converts an
#    oversight into a recorded choice nobody revisits.
#
#    The file-scope form is the one that matters most and the one the exemption
#    table cannot show, because that table only prints files that still have hits.
#    A whole-file grant over a file that no longer builds a client is a blank
#    cheque, printed nowhere.
#
#    STALE MEANS "NOTHING HERE TO EXEMPT", NOT "EXEMPTED NOTHING THIS RUN". A
#    marker on a line the rules exclude STRUCTURALLY — the nine
#    `func WithPrivateHostsAllowed()` declarations each carry one — exempts no hit
#    and is still describing the identifier it sits on. So the test is whether the
#    marker's own line or the line below it matches any rule's pattern at all,
#    with comments stripped so a marker cannot vouch for itself by quoting the
#    shape in its own reason.
stale_count=0
stale_detail=""
stale_files=""
stale_scan_files=$(prod_code_files)
if [[ -n $stale_scan_files ]]; then
    stale_files=$(printf '%s\n' "$stale_scan_files" | tr '\n' '\0' |
        xargs -0 grep -lE "(${ALL_MARKERS})(-file)?:" 2>/dev/null || true)
fi
for file in $stale_files; do
    [[ -z $file ]] && continue

    file_has_subject=0
    # Do not use grep -q in this full-file pipeline. Some grep implementations
    # exit as soon as they find a match; with pipefail, split_go_line can then
    # receive SIGPIPE and turn a successful match into a failed condition. This
    # was observable with BusyBox grep in Alpine 3.24. Let grep drain the input
    # so the predicate has the same result on the host and in CI.
    if split_go_line code < "$file" | grep -E "$ALL_PATTERNS" >/dev/null; then
        file_has_subject=1
    fi

    while IFS=: read -r mline mtext; do
        [[ -z $mline ]] && continue

        if printf '%s' "$mtext" | grep -qE "(${ALL_MARKERS})-file:"; then
            [[ $file_has_subject -eq 1 ]] && continue
            stale_count=$((stale_count + 1))
            stale_detail+="  ${file}:${mline}: file-scope exemption over a file that matches no rule."$'\n'
            continue
        fi

        # Select the two candidate lines before starting the pipeline. BusyBox
        # sed may stop reading after its last addressed line; placing it after
        # split_go_line therefore has the same SIGPIPE/pipefail ambiguity as a
        # short-circuiting grep. The quote-aware parser only needs these lines.
        if sed -n "${mline}p;$((mline + 1))p" "$file" |
            split_go_line code | grep -E "$ALL_PATTERNS" >/dev/null; then
            continue
        fi
        stale_count=$((stale_count + 1))
        stale_detail+="  ${file}:${mline}: exemption marker with nothing left to exempt."$'\n'
    done < <(split_go_line comment < "$file" | grep -nE "(${ALL_MARKERS})(-file)?:")
done
if [[ $stale_count -gt 0 ]]; then
    stale_detail+="  An exemption outliving its violation is a standing grant nothing reports."$'\n'
    stale_detail+="  Delete the marker, or move it onto the line it is meant to describe."
fi
ROWS+=("$(printf '  %-52s %6d %8d' "stale SSRF exemption marker" "$stale_count" 0)")
TOTAL=$((TOTAL + stale_count))
if [[ $VERBOSE -eq 1 && $stale_count -gt 0 ]]; then
    DETAILS+=("stale SSRF exemption marker"$'\n'"$stale_detail")
fi

# 4. CGO_ENABLED=0 in the production build. The one rule here that is not a grep
#    over source, because the build flag is load-bearing SECURITY configuration
#    and nothing else in the repository says so — the comment beside it reads
#    "for static binary (no libc dependency)", which sounds like a size
#    optimisation and invites exactly the deletion this catches.
#
#    Verified by probe: net.ParseIP returns nil for 0x7f.0.0.1, 2130706433,
#    127.1, 127.0.0.1., 127.0.0.0x1 (a hex label satisfies a "contains a letter"
#    rule) and 127.0.0.01 (leading zeros). Every one of them reaches the resolver
#    rather than the address classifier, and they fail closed ONLY because Go's
#    pure-Go resolver rejects them as hostnames. A cgo build hands each to
#    getaddrinfo, which resolves every one to loopback.
#
#    So flipping this flag silently re-opens six obfuscated spellings at every
#    one of the thirteen guarded sites, with no test failing anywhere.
#
#    THE RULE IS ABOUT THE OUTCOME, NOT ONE LINE'S SPELLING. `ENV CGO_ENABLED=0`
#    earlier in the SAME STAGE disables cgo just as effectively, and demanding the
#    flag sit on the `go build` line would reject a legitimate refactor and teach
#    whoever hit it that the audit is wrong — which is the first step to it being
#    bypassed. A MISSING Dockerfile is a violation too: this asserts a property of
#    the production build, and it cannot be satisfied by there being no
#    production build to check.
#
#    BUT THE OUTCOME IS A PROPERTY OF THE BUILD COMMAND, NOT OF THE FILE'S TEXT,
#    and a file-wide `grep -q CGO_ENABLED=0` is not the same assertion. This rule
#    used to be one, and Dockerfile:21 is a COMMENT reading "# CGO_ENABLED=0 for
#    static binary (no libc dependency)" — so deleting the flag from the build
#    command on line 32 left the audit reading its own explanation and reporting
#    green over a cgo build. Deletion is the drift that actually happens; nobody
#    writes =1 on purpose, and the comment survives the deletion because removing
#    a flag and removing the paragraph about it are separate edits.
#
#    So the Dockerfile is read as Docker reads it. Comment lines are dropped,
#    backslash continuations are joined into one logical line, and each `go build`
#    is required to carry the flag itself or to sit in a stage that already set it
#    with ENV or ARG. Stage scope is tracked because ENV does not cross a FROM: the
#    flag set in the alpine runtime stage reaches nothing, and is exactly what a
#    file-wide grep is happiest with. A Dockerfile that compiles NOTHING is a
#    violation for the same reason an absent one is — a rule with no subject must
#    not report green.
cgo_count=0
cgo_detail=""
if [[ ! -f Dockerfile ]]; then
    cgo_count=1
    cgo_detail="  Dockerfile: there is no production Dockerfile to check."$'\n'
    cgo_detail+="  This rule asserts a property of the production build; an audit that passes"$'\n'
    cgo_detail+="  when its subject is absent is the same silent green as a grep that matches"$'\n'
    cgo_detail+="  nothing. See docs/SSRF_SECURITY.md."
else
    cgo_why="  Resolver mode changes whether legacy numeric hostname forms are rejected as"$'\n'
    cgo_why+="  malformed or normalized to an address before the SSRF classifier sees them."$'\n'
    cgo_why+="  Both paths remain guarded; CGO_ENABLED=0 pins deterministic production"$'\n'
    cgo_why+="  behavior. See docs/SSRF_SECURITY.md."

    cgo_verdict=$(grep -vE '^[[:space:]]*#' Dockerfile |
        awk '
            # Join backslash continuations, so a RUN spanning five lines is one
            # logical command — which is what Docker executes and what the flag
            # has to be on.
            { logical = logical $0 }
            logical ~ /\\[[:space:]]*$/ { sub(/\\[[:space:]]*$/, " ", logical); next }
            { lines[++n] = logical; logical = "" }
            END {
                if (logical != "") { lines[++n] = logical }

                for (i = 1; i <= n; i++) {
                    line = lines[i]
                    if (line ~ /^[[:space:]]*FROM[[:space:]]/) { staged = 0; continue }
                    if (line ~ /^[[:space:]]*(ENV|ARG)[[:space:]]+CGO_ENABLED=0([[:space:]]|$)/) { staged = 1 }
                    if (line ~ /CGO_ENABLED=[^0[:space:]]/) { nonzero = 1 }
                    if (line ~ /go[[:space:]]+(build|install)([[:space:]]|$)/) {
                        builds++
                        if (line !~ /CGO_ENABLED=0([[:space:]]|$)/ && !staged) { unset = 1 }
                    }
                }

                if (builds == 0) { print "nobuild" }
                else if (nonzero) { print "nonzero" }
                else if (unset) { print "unset" }
                else { print "ok" }
            }
        ')

    case $cgo_verdict in
    ok) ;;
    nonzero)
        cgo_count=1
        cgo_detail="  Dockerfile: CGO_ENABLED is set to something other than 0."$'\n'"$cgo_why"
        ;;
    unset)
        cgo_count=1
        cgo_detail="  Dockerfile: a go build command runs without CGO_ENABLED=0."$'\n'
        cgo_detail+="  Put it on the build command, or ENV/ARG it earlier in the SAME stage — a"$'\n'
        cgo_detail+="  comment mentioning the flag, or an ENV in the runtime stage, sets nothing."$'\n'
        cgo_detail+="$cgo_why"
        ;;
    nobuild)
        cgo_count=1
        cgo_detail="  Dockerfile: no go build command to check."$'\n'
        cgo_detail+="  This rule asserts a property of the production build. A Dockerfile that"$'\n'
        cgo_detail+="  compiles nothing gives it no subject, which is the same silent green as"$'\n'
        cgo_detail+="  an absent Dockerfile. See docs/SSRF_SECURITY.md."
        ;;
    *)
        cgo_count=1
        cgo_detail="  Dockerfile: the CGO check did not run (verdict '$cgo_verdict')."
        ;;
    esac
fi
ROWS+=("$(printf '  %-52s %6d %8d' "production build is not CGO_ENABLED=0" "$cgo_count" 0)")
TOTAL=$((TOTAL + cgo_count))
if [[ $VERBOSE -eq 1 && $cgo_count -gt 0 ]]; then
    DETAILS+=("production build is not CGO_ENABLED=0"$'\n'"$cgo_detail")
fi

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
printf "    coves:allow-bare-client / coves:allow-dns-seam / coves:allow-ssrf-hatch\n"
printf "    (append '-file:' for a whole-file exemption; see this script's header)\n"
printf "  Run with -v for file:line.\n"
echo
exit 1
