// Package audit tests the SSRF regression fence itself.
//
// # WHY AN AUDIT SCRIPT NEEDS TESTS
//
// scripts/ssrf-audit.sh is the only mechanism in this repository that can
// notice a new unguarded HTTP client. `.env.ci:140` sets IS_DEV_ENV=true, so
// `make ci` takes the permissive branch at every call site holding an
// allow-private boolean — a green merge gate is compatible with every guarded
// site being unguarded in production. The grep is the entire detection surface.
//
// A grep that matches nothing prints the same green as a grep that matches
// nothing because the tree is clean. This branch has already found four tests
// that were green while measuring nothing, and an audit rule is the single
// easiest place in a codebase for that to happen and never be noticed: it is
// SUPPOSED to print zero, so the failure mode and the success mode look
// identical from the outside.
//
// So no rule here is asserted to work. Each one is watched biting a planted
// violation, and watched staying quiet on the same tree with the violation
// removed. Both halves are load-bearing: a rule that fires on the violation but
// also on the clean fixture is not a rule, it is a permanent floor that
// reviewers learn to ignore.
//
// # THE FIXTURE ROOT
//
// Every test builds a throwaway tree under t.TempDir() shaped like the
// repository — cmd/, internal/, a production Dockerfile — and points the script
// at it with SSRF_AUDIT_ROOT. Planting a violation in the real tree would mean
// mutating the repository during a test run, and the audit is a hard gate, so a
// crashed test would leave the build failing.
//
// TestRealRepositoryPasses is the exception and the point: it runs the script
// against the actual repository with no override, which is the fence actually
// standing rather than a fence proven to work on fixtures.
package audit

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// auditScript is resolved from the package directory, which is where `go test`
// puts the working directory.
const auditScript = "../../scripts/ssrf-audit.sh"

// fixture is a throwaway repository-shaped tree plus the ability to run the
// audit against it.
type fixture struct {
	t    *testing.T
	root string
}

// cleanDockerfile is the production build as it stands today: CGO_ENABLED=0 on
// the same line as the `go build` for ./cmd/server.
//
// The flag is load-bearing SECURITY configuration, not a size optimisation, and
// that is not obvious from the comment sitting above it in the real Dockerfile
// ("for static binary (no libc dependency)"). net.ParseIP returns nil for
// 0x7f.0.0.1, 2130706433, 127.1, 127.0.0.1., 127.0.0.0x1 and 127.0.0.01, so all
// six reach the resolver as hostnames rather than being refused on shape.
//
// FIVE of the six are the ones getaddrinfo turns into loopback. 127.0.0.1. — the
// trailing dot — resolves nowhere, on any platform or resolver mode, so it is
// refused for a reason that has nothing to do with this flag.
//
// AND THE FLAG IS NOT THE WHOLE STORY, which an earlier version of this comment
// got wrong by generalising from a dev machine. Probed in golang:1.25-alpine,
// the Dockerfile's own builder: with CGO_ENABLED=1 and no GODEBUG, musl still
// selects the PURE-GO resolver and all five still fail. Only GODEBUG=netdns=cgo
// reaches getaddrinfo. So the flag is defence in depth on this image rather than
// the sole control — worth keeping, and worth not overstating, because a claim
// that a control is load-bearing when it is not is how the real control goes
// unnoticed.
//
// THE COMMENT IS PART OF THE FIXTURE, AND IT IS THE POINT. Dockerfile:21 in the
// real tree is a comment reading "# CGO_ENABLED=0 for static binary (no libc
// dependency)", so any rule that greps the whole file for the flag's text is
// satisfied by the explanation of the flag rather than by the flag. Deleting the
// flag from the build line leaves that comment behind, which is exactly the
// drift the rule exists to catch, and a fixture without the comment cannot see
// the difference.
const cleanDockerfile = `FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY . .

# Build the binary
# CGO_ENABLED=0 for static binary (no libc dependency)
# -ldflags="-s -w" strips debug info for smaller binary
ARG GOARCH=amd64
ARG BUILD_TAGS=""
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} go build \
    -tags "${BUILD_TAGS}" \
    -ldflags="-s -w" \
    -o /build/coves-server \
    ./cmd/server

FROM alpine:3.19
COPY --from=builder /build/coves-server /app/coves-server
ENTRYPOINT ["/app/coves-server"]
`

// buildLine is the fragment every Dockerfile mutation below rewrites.
//
// Mutations anchor on the RUN rather than on the bare flag because
// cleanDockerfile now mentions CGO_ENABLED=0 twice — once in the comment, once
// on the build command — and a strings.Replace with a count of 1 would otherwise
// rewrite the COMMENT and leave the build untouched, producing a test that
// passes while mutating nothing.
const buildLine = "RUN CGO_ENABLED=0 GOOS=linux"

// cleanWiring is a cmd/server wiring line in its sanctioned form: the hatch is
// reached only through the dev-gate helper, and the helper's argument is derived
// from config rather than written as a literal.
const cleanWiring = `package main

func (a *App) wire() {
	a.unfurl = unfurl.NewService(unfurl.PrivateHostOptions(a.cfg.IsDevEnv)...)
	a.identity = identity.DefaultConfig(identity.PrivateHostOptions(a.cfg.IsDevEnv)...)
}
`

// cleanService is a converted call site: it takes its client from the shared
// guard rather than building one.
const cleanService = `package example

func newService(allowPrivate bool) *service {
	return &service{
		httpClient: oauth.NewSSRFSafeHTTPClient(oauth.PrivateAddressOptions(allowPrivate)...),
	}
}
`

// newFixture builds a tree that MUST pass every rule. TestCleanFixturePasses
// guards this: if the baseline is dirty, every violation test below would pass
// for the wrong reason and prove nothing.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := newBareFixture(t)
	f.write("cmd/server/wiring.go", cleanWiring)
	f.write("internal/example/service.go", cleanService)
	return f
}

// newBareFixture builds the same tree WITHOUT cmd/ and internal/, and it exists
// because newFixture always creating both made the empty-corpus path untestable
// by construction.
//
// That is not a hypothetical gap. prod_code_files runs `find cmd internal`; if
// either directory is renamed or moved, find returns nothing and every grep rule
// below reports zero over a corpus of no files. The audit then prints "All
// categories clear" and exits 0 — the same green a clean tree prints. Every
// fixture in this file calling newFixture meant the one shape that makes the
// whole fence vanish was the one shape no test could build.
func newBareFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{t: t, root: t.TempDir()}
	f.write("Dockerfile", cleanDockerfile)
	return f
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()

	path := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatalf("mkdir for fixture %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write fixture %s: %v", rel, err)
	}
}

func (f *fixture) remove(rel string) {
	f.t.Helper()

	if err := os.Remove(filepath.Join(f.root, rel)); err != nil {
		f.t.Fatalf("remove fixture %s: %v", rel, err)
	}
}

func (f *fixture) removeTree(rel string) {
	f.t.Helper()

	if err := os.RemoveAll(filepath.Join(f.root, rel)); err != nil {
		f.t.Fatalf("remove fixture tree %s: %v", rel, err)
	}
}

// run executes the audit against the fixture root and returns its exit code and
// combined output.
//
// An exit code above 1 is fatal rather than reported: 0 and 1 are the audit's
// two judgements, and anything else — a bash syntax error, a failed cd — is the
// script not running at all. Letting that count as "detected a violation" is how
// a broken audit tests green.
func (f *fixture) run(args ...string) (int, string) {
	f.t.Helper()
	return runAudit(f.t, f.root, args...)
}

func runAudit(t *testing.T, root string, args ...string) (int, string) {
	t.Helper()

	cmd := exec.Command("bash", append([]string{auditScript}, args...)...)
	if root != "" {
		cmd.Env = append(os.Environ(), "SSRF_AUDIT_ROOT="+root)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %s: %v\n%s", auditScript, err, out)
		}
	}

	code := cmd.ProcessState.ExitCode()
	if code < 0 || code > 1 {
		t.Fatalf("audit exited %d, which is neither pass (0) nor violation (1) — "+
			"the script did not run:\n%s", code, out)
	}
	return code, string(out)
}

// requireViolation asserts the audit failed, and is the assertion every rule
// test below is really making.
func requireViolation(t *testing.T, code int, out, what string) {
	t.Helper()

	if code != 1 {
		t.Errorf("audit exited %d — it did NOT flag %s.\n"+
			"A rule that cannot fail is the most useless artifact in this repository.\n%s",
			code, what, out)
	}
}

func requireClean(t *testing.T, code int, out, what string) {
	t.Helper()

	if code != 0 {
		t.Errorf("audit exited %d on %s, which is legitimate and must not be flagged.\n"+
			"A rule that fires on clean code is a permanent floor reviewers learn to ignore.\n%s",
			code, what, out)
	}
}

// ---------------------------------------------------------------------------
// The baseline
// ---------------------------------------------------------------------------

// TestCleanFixturePasses is the precondition for every other test in this file.
//
// If the baseline tree were itself a violation, each "planted violation fails"
// test below would pass without the planted violation being detected at all.
func TestCleanFixturePasses(t *testing.T) {
	f := newFixture(t)

	code, out := f.run()
	requireClean(t, code, out, "the baseline fixture tree")
}

// TestRealRepositoryPasses is the fence standing rather than the fence proven.
//
// It runs with no SSRF_AUDIT_ROOT, so it scans the actual working tree — which
// means an unguarded client added anywhere in cmd/ or internal/ fails the unit
// tier, not merely `make ssrf-audit`.
func TestRealRepositoryPasses(t *testing.T) {
	code, out := runAudit(t, "")
	if code != 0 {
		t.Errorf("the repository does not pass its own SSRF audit (exit %d).\n"+
			"Either the new code needs the guard, or it needs a marker with a reason.\n%s",
			code, out)
	}
}

// ---------------------------------------------------------------------------
// Rule 0 — the corpus itself
// ---------------------------------------------------------------------------
//
// Every grep below is only as real as the file list it runs over, and that list
// is `find cmd internal`. An audit whose corpus is empty greps nothing, counts
// nothing, prints "All categories clear" and exits 0 — indistinguishable from a
// clean tree, over a tree nobody looked at.
//
// This is the single highest-leverage failure in the script, because it disables
// EVERY rule at once and it is reached by an ordinary refactor: moving internal/
// under a new parent, renaming cmd/, vendoring the tree one level down.

// TestViolationOutsideTheAuditedRootsIsNotSilentlyGreen is the reviewer's
// fixture: production Go living somewhere the corpus never looks.
//
// Two unmistakable violations — an http.Get and a bare client — sit in a tree
// with no cmd/ and no internal/, and the audit used to pass. It must not: either
// the roots are there and get scanned, or their absence is itself the finding.
func TestViolationOutsideTheAuditedRootsIsNotSilentlyGreen(t *testing.T) {
	f := newBareFixture(t)
	f.write("pkg/fetch/fetch.go", `package fetch

func fetch(url string) {
	http.Get(url)
	client := &http.Client{}
	_ = client
}
`)

	code, out := f.run()
	requireViolation(t, code, out,
		"a bare client and an http.Get in a tree whose corpus is empty")
}

// TestMissingProductionRootFails narrows the same defect to one root.
//
// `find cmd internal` with internal/ absent still returns cmd/'s files, so the
// corpus is non-empty and the `[[ -n $files ]]` guard is satisfied — while every
// rule silently stops covering the larger half of the tree. A partial corpus is
// the harder case precisely because the audit still looks like it is working.
func TestMissingProductionRootFails(t *testing.T) {
	for _, root := range []string{"cmd", "internal"} {
		t.Run(root, func(t *testing.T) {
			f := newFixture(t)
			f.removeTree(root)

			code, out := f.run()
			requireViolation(t, code, out, "the audited root "+root+"/ being absent")
		})
	}
}

// TestProductionRootWithOnlyTestFilesFails is the same hole reached without
// deleting anything. The directory is present, so an existence check passes, but
// every file in it is filtered out as a test file and the rules scan nothing.
func TestProductionRootWithOnlyTestFilesFails(t *testing.T) {
	f := newFixture(t)
	f.removeTree("internal")
	f.write("internal/example/service_test.go", `package example

func TestNothing(t *testing.T) {}
`)

	code, out := f.run()
	requireViolation(t, code, out, "an audited root holding no production Go at all")
}

// ---------------------------------------------------------------------------
// Rule 1 — bare HTTP clients in production code
// ---------------------------------------------------------------------------

// TestBareClientStructLiteralFails covers the shape twelve of the converted call
// sites had before the remediation.
func TestBareClientStructLiteralFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch.go", `package example

func fetch(url string) {
	client := &http.Client{Timeout: 10 * time.Second}
	client.Get(url)
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "a bare &http.Client{} in internal/")
}

// TestDefaultClientFails covers the other half of the same mistake. It is worse
// than the struct literal, not better: http.DefaultClient has no timeout either.
func TestDefaultClientFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch.go", `package example

func fetch(req *http.Request) {
	http.DefaultClient.Do(req)
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "http.DefaultClient in internal/")
}

// TestPackageLevelHelpersFail is the extension the two grep patterns in the
// brief do not cover, and it is not hypothetical: cmd/reindex-votes/main.go
// already reaches the network twice this way.
//
// http.Get, http.Post, http.Head and http.PostForm are DefaultClient with a
// shorter spelling — same absent timeout, same absent guard — and they match
// neither `&http.Client{` nor `http.DefaultClient`. A fence that misses them
// leaves the single most convenient way to write an unguarded fetch as the one
// way the fence cannot see.
func TestPackageLevelHelpersFail(t *testing.T) {
	for _, call := range []string{
		"http.Get(url)",
		"http.Post(url, \"application/json\", body)",
		"http.Head(url)",
		"http.PostForm(url, values)",
	} {
		t.Run(call, func(t *testing.T) {
			f := newFixture(t)
			f.write("cmd/tool/main.go", "package main\n\nfunc fetch() {\n\t"+call+"\n}\n")

			code, out := f.run()
			requireViolation(t, code, out, call+" in cmd/")
		})
	}
}

// TestNewHTTPClientFails covers new(http.Client), which is the struct literal
// with the one punctuation change that defeats an `&http.Client{` grep.
func TestNewHTTPClientFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch.go", `package example

func fetch() {
	client := new(http.Client)
	_ = client
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "new(http.Client) in internal/")
}

// TestBareClientInTestFileIsIgnored fixes the scope. A test that stands up an
// httptest server has to dial loopback, which is precisely the address class the
// guard refuses; auditing test files here would make the guard's own tests
// unwritable.
func TestBareClientInTestFileIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch_test.go", `package example

func TestFetch(t *testing.T) {
	client := &http.Client{Timeout: time.Second}
	http.DefaultClient.Do(nil)
	_ = client
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a bare client in a _test.go file")
}

// TestBareClientCommentIsIgnored keeps the audit compatible with a tree that
// explains itself. Several converted sites carry a comment naming the bare
// client they replaced — internal/core/blobs/service.go and
// internal/core/users/service.go both do — and counting those would mean the
// remediation's own documentation failed the gate it added.
func TestBareClientCommentIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch.go", `package example

// This used to be a bare &http.Client{Timeout: 30 * time.Second}, which reached
// http.DefaultClient's zero timeout by another name.
func fetch() {}
`)

	code, out := f.run()
	requireClean(t, code, out, "a comment describing a bare client")
}

// TestBareClientMarkerExempts is the allowlist mechanism. The reason is not
// decoration: the marker converts an oversight into a recorded decision to be
// unguarded, and the reason is the only part of it a reviewer can disagree with.
func TestBareClientMarkerExempts(t *testing.T) {
	f := newFixture(t)
	f.write("internal/notify/vendor/client.go", `package vendor

func newClient() *http.Client {
	// coves:allow-bare-client: fixed vendor API host, no caller-supplied URL
	return &http.Client{Timeout: 10 * time.Second}
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a bare client carrying coves:allow-bare-client")
}

// TestBareClientFileMarkerExemptsAndIsReported covers the file-scope form and
// the constraint that makes it safe to have one.
//
// A whole-file exemption is the broadest thing this audit can grant, so it is
// printed on EVERY run with its reason and hit count. A broad exemption that
// goes quiet is indistinguishable from no exemption at all, which is how the
// next unguarded client lands inside an already-exempted file.
func TestBareClientFileMarkerExemptsAndIsReported(t *testing.T) {
	f := newFixture(t)
	f.write("internal/atproto/oauth/transport.go", `package oauth

// coves:allow-bare-client-file: this file IS the guarded client's construction

func NewSSRFSafeHTTPClient(opts ...Option) *http.Client {
	return &http.Client{Transport: newSSRFSafeTransport(opts...)}
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a file declaring coves:allow-bare-client-file")

	if !strings.Contains(out, "internal/atproto/oauth/transport.go") {
		t.Errorf("the file-scope exemption was not printed. A broad exemption that "+
			"goes quiet is indistinguishable from no exemption at all.\n%s", out)
	}
	if !strings.Contains(out, "this file IS the guarded client's construction") {
		t.Errorf("the file-scope exemption printed without its reason. The reason is "+
			"the only part a reviewer can disagree with.\n%s", out)
	}
}

// TestWrongMarkerDoesNotExempt fences the marker to its own category.
//
// scripts/test-audit.sh already learned this: an exemption for a hostname must
// not quietly excuse a sleep on the same line. Here the failure would be worse —
// a reason about a bare client silently licensing a resolver override is a
// justification that was never written for the thing it ends up permitting.
func TestWrongMarkerDoesNotExempt(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch.go", `package example

func newClient() {
	// coves:allow-bare-client: fixed vendor API host
	oauth.NewSSRFSafeHTTPClient(oauth.WithHostResolver(fakeLookup))
}
`)

	code, out := f.run()
	requireViolation(t, code, out,
		"a WithHostResolver call carrying an unrelated coves:allow-bare-client marker")
}

// TestZeroValueClientVarFails closes the shape that needs no punctuation at all.
//
// `var client http.Client` is a usable client — every field has a working zero
// value, including a Timeout of zero, which in net/http means wait forever. It
// matches neither `http.Client{` (no brace) nor `new(http.Client)` (no call),
// and it is ordinary Go that any reviewer would read straight past.
func TestZeroValueClientVarFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch.go", `package example

func fetch(url string) {
	var client http.Client
	client.Get(url)
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "a zero-value var client http.Client")
}

// TestClientPointerFieldIsIgnored fixes the boundary the rule above must not
// cross. A `*http.Client` field or parameter is how a GUARDED client is carried
// — internal/core/blobs, internal/core/users and the unfurl service all hold
// one — so flagging the type's every appearance would put a floor under the
// audit roughly the size of the remediation.
func TestClientPointerFieldIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/service.go", `package example

type service struct {
	httpClient *http.Client
}

func newService(client *http.Client) *service {
	return &service{httpClient: client}
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a *http.Client field holding the shared guarded client")
}

// TestValueTypedClientFieldFails closes the shape between the two tests above.
//
// Rule 1 required the keyword `var` before the identifier, so it saw
// `var c http.Client` in a function body and missed the identical zero-value
// client declared as a struct field or a parameter — same absent guard, same
// wait-forever zero Timeout, and one keyword less to notice.
//
// The pointer form stays out, and that boundary is what makes this safe to add:
// `*http.Client` is how a GUARDED client is carried. The distinguishing text is
// the whitespace directly before `http.Client`, which a pointer, a slice and an
// address-of all break.
func TestValueTypedClientFieldFails(t *testing.T) {
	for name, decl := range map[string]string{
		"struct field": "type cfg struct {\n\thc http.Client\n}\n",
		"parameter":    "func fetch(c http.Client, url string) {\n\tc.Get(url)\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/config.go", "package example\n\n"+decl)

			code, out := f.run()
			requireViolation(t, code, out, "a value-typed http.Client "+name)
		})
	}
}

// TestBareTransportFails covers the layer below the client.
//
// A Transport is what actually dials, so `&http.Transport{}` reaches the network
// with no address classification whether or not a Client is ever wrapped around
// it. The shared guard IS a Transport, which is the tell: anything else building
// one is building the thing the guard replaces.
func TestBareTransportFails(t *testing.T) {
	for name, body := range map[string]string{
		"struct literal": "\ttr := &http.Transport{}\n\t_ = tr\n",
		"RoundTrip on a bare transport": "\ttr := &http.Transport{}\n" +
			"\ttr.RoundTrip(req)\n",
		"http.DefaultTransport.RoundTrip": "\thttp.DefaultTransport.RoundTrip(req)\n",
		// new(http.Transport) is the brace form with one character's difference,
		// exactly as new(http.Client) is for rule 1. Rule 1 covered both spellings
		// from the start; this rule covered only the brace, for no reason anyone
		// wrote down. The gap surfaced because a rule 1e test using
		// `Transport: new(http.Transport)` under a coves:allow-bare-client marker
		// passed with rule 1e reverted — the marker was silencing nothing, so the
		// audit was failing on rule 5's stale-marker check instead, and the
		// subtest was measuring the wrong rule.
		"new(http.Transport)": "\ttr := new(http.Transport)\n\t_ = tr\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/fetch.go",
				"package example\n\nfunc fetch(req *http.Request) {\n"+body+"}\n")

			code, out := f.run()
			requireViolation(t, code, out, name+" in internal/")
		})
	}
}

// TestReverseProxyFails.
//
// httputil.NewSingleHostReverseProxy builds its own Transport-backed proxy and
// forwards to whatever URL it is handed. It mentions no client and no transport,
// so it matches nothing else here, and a proxy fed a caller-supplied target is a
// textbook SSRF.
func TestReverseProxyFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/proxy.go", `package example

func proxy(target *url.URL) http.Handler {
	return httputil.NewSingleHostReverseProxy(target)
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "httputil.NewSingleHostReverseProxy in internal/")
}

// TestHandBuiltReverseProxyFails covers the proxy the rule above does not build
// for you.
//
// NewSingleHostReverseProxy is a two-line constructor around the same struct, so
// a hand-written `&httputil.ReverseProxy{Director: ...}` is the same object
// reached by the spelling the rule could not see. Its Transport field is
// OPTIONAL, and a nil Transport means http.DefaultTransport — so the literal
// that mentions no client, no transport and no constructor is a
// DefaultTransport-backed proxy over whatever URL its Director was handed.
func TestHandBuiltReverseProxyFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/proxy.go", `package example

func proxy(target *url.URL) http.Handler {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
		},
	}
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "a hand-built httputil.ReverseProxy with no Transport")
}

// TestReverseProxyWithNilTransportFails states the same thing at its most
// deliberate-looking. `Transport: nil` is the spelling that reads as though
// someone considered the field, and it is byte-for-byte the DefaultTransport
// case above.
func TestReverseProxyWithNilTransportFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/proxy.go", `package example

func proxy(target *url.URL) http.Handler {
	return &httputil.ReverseProxy{
		Director:  func(req *http.Request) { req.URL = target },
		Transport: nil,
	}
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "an httputil.ReverseProxy with an explicit nil Transport")
}

// TestReverseProxyWithGuardedTransportIsIgnored is the fix the rule is asking
// for, and without it the rule would be a floor rather than a fence: a proxy is
// not forbidden, leaving its transport to the library default is.
func TestReverseProxyWithGuardedTransportIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/proxy.go", `package example

func proxy(target *url.URL, allowPrivate bool) http.Handler {
	return &httputil.ReverseProxy{
		Director:  func(req *http.Request) { req.URL = target },
		Transport: oauth.NewSSRFSafeRoundTripper(oauth.PrivateAddressOptions(allowPrivate)...),
	}
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a ReverseProxy handed the guarded transport")
}

// TestReverseProxySatisfiedByABareTransportFails is rule 1e's half of the same
// defect the xrpc test above covers: a Transport field set to the thing the rule
// exists to refuse.
//
// The marker exempts rule 1b, which matches on the `Transport:` line. This rule
// fires on the `httputil.ReverseProxy{` line, so it has to refuse the value on
// its own.
func TestReverseProxySatisfiedByABareTransportFails(t *testing.T) {
	for name, field := range map[string]string{
		"new(http.Transport)":   "Transport: new(http.Transport),",
		"empty literal":         "Transport: &http.Transport{},",
		"http.DefaultTransport": "Transport: http.DefaultTransport,",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/proxy.go", `package example

func proxy(target *url.URL) http.Handler {
	return &httputil.ReverseProxy{
		Director:  func(req *http.Request) { req.URL = target },
		`+field+` // coves:allow-bare-client: exempts rule 1b, must not exempt 1e
	}
}
`)

			code, out := f.run()
			requireViolation(t, code, out, "a ReverseProxy handed "+name)
		})
	}
}

// ---------------------------------------------------------------------------
// Rule 1f — the identity directory whose client field is a VALUE
// ---------------------------------------------------------------------------

// baseDirectoryAliases is every spelling this tree gives indigo's identity
// package, and the reason rule 1f matches the type through ANY qualifier.
//
// cmd/server/wiring.go says `indigoidentity`, internal/atproto/oauth/client.go
// says `identity`, internal/atproto/identity/base_resolver.go says
// `indigoIdentity` — three files, three names, one type. A rule naming one of
// them covers a third of the sites, and nobody has to rename anything for that
// to happen: an import alias is per-file and always was.
var baseDirectoryAliases = []string{"identity", "indigoidentity", "indigoIdentity"}

// TestIdentityDirectoryWithoutHTTPClientFails is the sharpest member of the
// handoff class, and it is proved BY DELETION rather than by a flip.
//
// indigo's BaseDirectory carries `HTTPClient http.Client` — a VALUE, not a
// pointer. So a literal that simply omits the field COMPILES, and what it gets
// is a zero-value http.Client: http.DefaultTransport underneath, and a Timeout
// of zero, which in net/http means wait forever. There is no nil to panic on and
// no error to return. The omission is silent at compile time and silent at run
// time, and the directory then resolves DID documents named by a firehose record
// and by the unverified `iss` of an inbound service JWT.
//
// A rule proved only by flipping a field to a bad value would not catch this,
// which is the whole distinction: deletion is the mutation that actually happens
// here, because deletion is the one the compiler permits.
func TestIdentityDirectoryWithoutHTTPClientFails(t *testing.T) {
	for _, alias := range baseDirectoryAliases {
		t.Run(alias, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/directory.go", `package example

func newDirectory(plcURL string) *`+alias+`.BaseDirectory {
	return &`+alias+`.BaseDirectory{
		PLCURL:    plcURL,
		UserAgent: "Coves/1.0",
	}
}
`)

			code, out := f.run()
			requireViolation(t, code, out,
				"a "+alias+".BaseDirectory with its HTTPClient field omitted")
		})
	}
}

// TestIdentityDirectoryWithGuardedClientIsIgnored is the form all three real
// sites have, and without it the rule would be a floor rather than a fence.
//
// The dereference is the point: the field is a value, so the guarded *http.Client
// is copied into it. That copy shares the same *ssrfSafeTransport pointer, so the
// guard, the resolver and the connection pool behind it are one instance.
func TestIdentityDirectoryWithGuardedClientIsIgnored(t *testing.T) {
	for _, alias := range baseDirectoryAliases {
		t.Run(alias, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/directory.go", `package example

func newDirectory(plcURL string, allowPrivate bool) *`+alias+`.BaseDirectory {
	client := oauth.NewSSRFSafeHTTPClient(oauth.PrivateAddressOptions(allowPrivate)...)
	return &`+alias+`.BaseDirectory{
		PLCURL:     plcURL,
		HTTPClient: *client,
	}
}
`)

			code, out := f.run()
			requireClean(t, code, out,
				"a "+alias+".BaseDirectory handed the guarded client")
		})
	}
}

// TestIdentityDirectoryWithZeroValueClientFails is what `Client: nil` means for a
// value-typed field: the spelling that names the field while asking for exactly
// the default the omission would have produced.
//
// It is the same shape rule 1c learned from — the literal that most looks like
// someone considered the problem is the one that defeats a check asking only
// whether the field appears.
func TestIdentityDirectoryWithZeroValueClientFails(t *testing.T) {
	for name, value := range map[string]string{
		"empty literal":    "http.Client{}",
		"new(http.Client)": "*new(http.Client)",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/directory.go", `package example

func newDirectory(plcURL string) *identity.BaseDirectory {
	return &identity.BaseDirectory{
		PLCURL:     plcURL,
		HTTPClient: `+value+`, // coves:allow-bare-client: exempts rule 1, must not exempt 1f
	}
}
`)

			code, out := f.run()
			requireViolation(t, code, out, "a BaseDirectory handed "+name)
		})
	}
}

// TestIdentityDirectoryMarkerExempts keeps the escape hatch on the same terms as
// every other rule here.
func TestIdentityDirectoryMarkerExempts(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/directory.go", `package example

func newDirectory(plcURL string) *identity.BaseDirectory {
	// coves:allow-bare-client: PLC URL is operator config, never caller-supplied
	return &identity.BaseDirectory{PLCURL: plcURL}
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a BaseDirectory carrying coves:allow-bare-client")
}

// ---------------------------------------------------------------------------
// Rule 1g — library constructors that install http.DefaultClient for you
// ---------------------------------------------------------------------------

// TestLibraryConstructorWithBuiltInClientFails covers the half of the handoff
// class that has no composite literal to ask a question about.
//
// Each of these four was read in the dependency source, because the call site
// says nothing at all — no `http`, no `Client`, no `Transport` on the line:
//
//	oauth.NewClientApp             → Client: http.DefaultClient
//	atclient.NewAPIClient          → Client: http.DefaultClient
//	atclient.LoginWithPasswordHost → calls NewAPIClient, and POSTs the CLEARTEXT
//	                                 password through it before it returns
//	retryablehttp.NewClient        → HTTPClient: cleanhttp.DefaultPooledClient()
//
// So `apiClient := atclient.NewAPIClient(host)` is http.DefaultClient — no
// guard, and a Timeout of zero meaning wait forever — spelled without a single
// token any other rule in this file matches.
//
// THIS RULE CANNOT BE PROVED BY DELETION, and that is a property of the rule
// rather than of the test. The remediation at all four real sites is an
// assignment on the FOLLOWING line, and `apiClient.Client = newBearerHTTPClient()`
// is textually indistinguishable from an assignment of anything else, so no grep
// can tell the remediated site from the abandoned one. The marker's reason is
// where the replacement line gets named, which is the only place a reviewer can
// check it.
func TestLibraryConstructorWithBuiltInClientFails(t *testing.T) {
	for name, call := range map[string]string{
		"oauth.NewClientApp":             "app := oauth.NewClientApp(&cfg, store)\n\t_ = app",
		"atclient.NewAPIClient":          "c := atclient.NewAPIClient(host)\n\t_ = c",
		"atclient.LoginWithPasswordHost": "c, _ := atclient.LoginWithPasswordHost(ctx, host, handle, pw, \"\", nil)\n\t_ = c",
		"retryablehttp.NewClient":        "rc := retryablehttp.NewClient()\n\t_ = rc",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/pds.go",
				"package example\n\nfunc build() {\n\t"+call+"\n}\n")

			code, out := f.run()
			requireViolation(t, code, out, name+" in production code")
		})
	}
}

// TestLibraryConstructorUnderAnAliasFails is why three of the four patterns
// match through ANY package qualifier rather than the one this tree writes
// today.
//
// An import alias is per-file and costs nothing, and rule 1f exists because this
// tree already spells one indigo import three different ways without anyone
// intending a bypass. NewClientApp, NewAPIClient and LoginWithPasswordHost are
// distinctive enough to carry that; retryablehttp's NewClient is not, and is
// matched by its package name — a rule that misses an alias, in exchange for not
// putting a floor under every SDK constructor in the tree.
func TestLibraryConstructorUnderAnAliasFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/pds.go", `package example

import indigoclient "github.com/bluesky-social/indigo/atproto/atclient"

func build(host string) {
	c := indigoclient.NewAPIClient(host)
	_ = c
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "NewAPIClient reached through an import alias")
}

// TestLibraryConstructorMarkerExempts is the form all four real sites have. The
// reason names the line that replaces the default, which is the part a reviewer
// can go and check — and the part that becomes visibly false if that line is
// ever deleted.
func TestLibraryConstructorMarkerExempts(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/pds.go", `package example

func build(host string, opts ...ClientOption) *atclient.APIClient {
	// coves:allow-bare-client: NewAPIClient installs http.DefaultClient; the next line replaces it before any request is made
	apiClient := atclient.NewAPIClient(host)
	apiClient.Client = newBearerHTTPClient(opts...)
	return apiClient
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a library constructor whose default is replaced under a marker")
}

// TestAliasedNetHTTPImportFails closes the bypass at the import rather than at
// the call.
//
// Every pattern in rule 1 spells the package `http`, so an alias renames its way
// past all of them at once. The call site cannot be greppable — `nh.Get(url)`
// has no distinguishing text — but the IMPORT must exist for the alias to, and
// there is no reason in this tree to rename net/http. Catching the one line that
// must be present is worth more than chasing the many that need not be.
//
// The alias here deliberately does not contain the substring "http": an alias
// that does (`nethttp.Get`) is caught by rule 1 already, by accident rather than
// by design, and a test resting on that accident would prove nothing.
func TestAliasedNetHTTPImportFails(t *testing.T) {
	for name, imports := range map[string]string{
		"grouped alias":     "import (\n\t\"context\"\n\tnh \"net/http\"\n)",
		"single-line alias": "import nh \"net/http\"",
		"dot import":        "import (\n\t\"context\"\n\t. \"net/http\"\n)",
		"blank import":      "import _ \"net/http\"",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/fetch.go",
				"package example\n\n"+imports+"\n\nfunc fetch(url string) {\n\tnh.Get(url)\n}\n")

			code, out := f.run()
			requireViolation(t, code, out, name+" of net/http")
		})
	}
}

// TestPlainNetHTTPImportIsIgnored is the other half, and without it the rule
// above would flag every converted call site in the repository.
func TestPlainNetHTTPImportIsIgnored(t *testing.T) {
	for name, imports := range map[string]string{
		"grouped": "import (\n\t\"context\"\n\t\"net/http\"\n)",
		"single":  "import \"net/http\"",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/fetch.go",
				"package example\n\n"+imports+
					"\n\nfunc fetch() *http.Client {\n"+
					"\treturn oauth.NewSSRFSafeHTTPClient()\n}\n")

			code, out := f.run()
			requireClean(t, code, out, "an unaliased "+name+" import of net/http")
		})
	}
}

// ---------------------------------------------------------------------------
// Rule 1c — xrpc.Client, which mentions no HTTP client at all
// ---------------------------------------------------------------------------

// TestXRPCClientWithoutInjectedClientFails.
//
// github.com/bluesky-social/indigo/xrpc.Client carries an OPTIONAL
// `Client *http.Client` field, and its getClient() (xrpc/xrpc.go:31-36) falls
// back to util.RobustHTTPClient() when that field is nil — an unguarded client
// that additionally RETRIES, so a blocked-looking failure gets attempted again.
//
// This is the shape no existing pattern could ever see, because a site that
// leaves the field nil contains no `http.Client` text at all. Four production
// sites are built this way today and two of them carry live credentials: a
// refresh token in internal/core/communities/token_refresh.go, and a cleartext
// account password and email in pds_provisioning.go. The nil field is what sends
// those to whatever host the record named.
func TestXRPCClientWithoutInjectedClientFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/pds.go", `package example

func refresh(pdsURL, refreshToken string) {
	client := &xrpc.Client{
		Host: pdsURL,
		Auth: &xrpc.AuthInfo{
			AccessJwt:  refreshToken,
			RefreshJwt: refreshToken,
		},
	}
	_ = client
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "an xrpc.Client leaving its Client field nil")
}

// TestXRPCClientWithInjectedClientIsIgnored is the fix this rule is asking for,
// and the rule is worth nothing without it: the point is not that xrpc.Client is
// forbidden, it is that the guarded transport must be handed to it. A rule that
// flagged the remediated form too would be a permanent floor, and the four sites
// would end up carrying markers instead of guards.
//
// The nested AuthInfo literal is in the fixture on purpose. It closes before the
// outer literal does, so a rule that stopped looking at the first closing brace
// would miss a Client field written after it.
func TestXRPCClientWithInjectedClientIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/pds.go", `package example

func refresh(pdsURL, refreshToken string, allowPrivate bool) {
	client := &xrpc.Client{
		Host: pdsURL,
		Auth: &xrpc.AuthInfo{
			AccessJwt:  refreshToken,
			RefreshJwt: refreshToken,
		},
		Client: oauth.NewSSRFSafeHTTPClient(oauth.PrivateAddressOptions(allowPrivate)...),
	}
	_ = client
}
`)

	code, out := f.run()
	requireClean(t, code, out, "an xrpc.Client handed the shared guarded client")
}

// TestXRPCClientWithExplicitNilClientFails is the bypass that reads as
// remediation.
//
// The rule asked "does this literal set Client:" and `Client: nil` answers yes.
// But indigo's getClient() substitutes util.RobustHTTPClient() for a NIL field —
// so `Client: nil` is not a client injected, it is the unguarded retrying
// default requested by name. The spelling that most looks like someone thought
// about the problem was the one spelling that defeated the check.
func TestXRPCClientWithExplicitNilClientFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/pds.go", `package example

func refresh(pdsURL string) {
	client := &xrpc.Client{
		Host:   pdsURL,
		Client: nil,
	}
	_ = client
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "an xrpc.Client with an explicit nil Client field")
}

// TestXRPCClientSatisfiedOnlyByACommentFails is the same defect reached without
// writing any field at all.
//
// The satisfied-check greps the literal's TEXT, and a comment is text. A literal
// whose only mention of the field is a note about setting it later is an
// unguarded xrpc.Client that the audit reports as remediated — the failure mode
// rule 4 already had once, where the Dockerfile's own explanation of a flag
// satisfied the check for the flag.
func TestXRPCClientSatisfiedOnlyByACommentFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/pds.go", `package example

func refresh(pdsURL string) {
	client := &xrpc.Client{
		Host: pdsURL,
		// Client: injected by the caller once the guard is wired up.
	}
	_ = client
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "an xrpc.Client whose only Client: is in a comment")
}

// TestXRPCClientSatisfiedByABareClientFails is the comment case one step on: the
// field is set, in code rather than in a comment, and what it is set to is a
// bare unguarded client.
//
// `Client: new(http.Client)` and `Client: &http.Client{}` name the field the
// rule asked about, so the literal read as remediated. Rule 1 does match those
// two spellings — but on the `Client:` LINE, not on the `xrpc.Client{` line this
// rule fires on, so a single coves:allow-bare-client marker written about the
// bare client exempts rule 1 and leaves rule 1c silently satisfied. The marker
// is in each fixture below for exactly that reason: without it the test would
// pass on rule 1 alone and prove nothing about this one.
func TestXRPCClientSatisfiedByABareClientFails(t *testing.T) {
	for name, field := range map[string]string{
		"new(http.Client)": "Client: new(http.Client),",
		"empty literal":    "Client: &http.Client{},",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/pds.go", `package example

func refresh(pdsURL string) {
	client := &xrpc.Client{
		Host: pdsURL,
		`+field+` // coves:allow-bare-client: exempts rule 1, must not exempt 1c
	}
	_ = client
}
`)

			code, out := f.run()
			requireViolation(t, code, out, "an xrpc.Client handed "+name)
		})
	}
}

// TestXRPCClientMarkerExempts keeps the escape hatch on the same terms as every
// other rule here: a documented decision, not a silence.
func TestXRPCClientMarkerExempts(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/pds.go", `package example

func admin(pdsURL string) {
	// coves:allow-bare-client: pdsURL is the AppView's own configured PDS
	client := &xrpc.Client{Host: pdsURL}
	_ = client
}
`)

	code, out := f.run()
	requireClean(t, code, out, "an xrpc.Client carrying coves:allow-bare-client")
}

// ---------------------------------------------------------------------------
// Rule 2 — the resolver seam outside test files
// ---------------------------------------------------------------------------

// TestHostResolverInProductionFails.
//
// WithHostResolver cannot open the guard — whatever it answers is classified by
// the identical pass a real DNS answer goes through — which is exactly why it is
// safe to export and exactly why it must never appear in production. What it CAN
// do is make production code stop consulting real DNS, and there is no
// legitimate reason for the AppView to resolve names from a table.
func TestHostResolverInProductionFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/client.go", `package example

func newClient() *http.Client {
	return oauth.NewSSRFSafeHTTPClient(
		oauth.WithHostResolver(func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		}),
	)
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "oauth.WithHostResolver in production code")
}

// TestHostResolverInTestFileIsIgnored is the seam's whole purpose. There is no
// hermetic way to make a hostname answer with a chosen address — the hermetic
// tiers block egress and nothing here can write /etc/hosts — so a package proves
// its guard by injecting a lookup.
func TestHostResolverInTestFileIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/guard_test.go", `package example

func TestGuardRefusesLoopback(t *testing.T) {
	c := newClient(oauth.WithHostResolver(func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}))
	_ = c
}
`)

	code, out := f.run()
	requireClean(t, code, out, "WithHostResolver in a _test.go file")
}

// TestHostResolverDeclarationIsIgnored.
//
// The option has to be declared somewhere, and that somewhere is production
// source by definition. A rule that flags a function's own `func` line carries a
// permanent floor of one, which is the state scripts/test-audit.sh explicitly
// refused for its websocket category — a count reviewers learn to read past is a
// count that stops being read.
func TestHostResolverDeclarationIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/atproto/oauth/transport.go", `package oauth

func WithHostResolver(lookup func(ctx context.Context, host string) ([]net.IP, error)) Option {
	return func(t *ssrfSafeTransport) {
		if lookup == nil {
			return
		}
		t.lookupIP = lookup
	}
}
`)

	code, out := f.run()
	requireClean(t, code, out, "the WithHostResolver declaration itself")
}

// ---------------------------------------------------------------------------
// Rule 3 — the dev hatch outside a dev-gated wiring line
// ---------------------------------------------------------------------------

// TestHatchOptionCalledDirectlyFails.
//
// Named options make the hatch greppable so this rule can exist. The
// old spelling was a positional `true`, which is not greppable and which said
// nothing at the call site about being the difference between a guarded client
// and an unguarded one.
func TestHatchOptionCalledDirectlyFails(t *testing.T) {
	for _, call := range []string{
		"oauth.WithPrivateAddressesAllowed()",
		"unfurl.WithPrivateHostsAllowed()",
	} {
		t.Run(call, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/client.go",
				"package example\n\nfunc newClient() {\n\tbuild("+call+")\n}\n")

			code, out := f.run()
			requireViolation(t, code, out, call+" called in production code")
		})
	}
}

// TestHatchHardcodedTrueFails covers the remaining literal-gate risk: the
// helper is still reachable with a literal, and `PrivateHostOptions(true)` opens
// the guard in production while looking like the sanctioned dev-gated call.
func TestHatchHardcodedTrueFails(t *testing.T) {
	for _, call := range []string{
		"oauth.PrivateAddressOptions(true)",
		"blobs.PrivateHostOptions(true)",
	} {
		t.Run(call, func(t *testing.T) {
			f := newFixture(t)
			f.write("cmd/server/extra.go",
				"package main\n\nfunc wireMore() {\n\tbuild("+call+"...)\n}\n")

			code, out := f.run()
			requireViolation(t, code, out, call+" with a hardcoded true")
		})
	}
}

// TestHatchDerivedFromConfigIsIgnored is the sanctioned form, and it is the
// entire reason the helper exists as a function rather than an inline `if`.
func TestHatchDerivedFromConfigIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("cmd/server/extra.go", `package main

func wireMore(a *App) {
	blobs.NewBlobService(a.cfg.PDS.URL, blobs.PrivateHostOptions(a.cfg.IsDevEnv)...)
	imageproxy.NewFetcher(imageproxy.PrivateHostOptions(a.cfg.IsDevEnv)...)
}
`)

	code, out := f.run()
	requireClean(t, code, out, "PrivateHostOptions derived from cfg.IsDevEnv")
}

// TestHatchInTestFileIsIgnored. A test serving fixtures from httptest passes the
// hatch because loopback is exactly what the guard refuses.
func TestHatchInTestFileIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/guard_test.go", `package example

func TestPermissiveClientReachesLoopback(t *testing.T) {
	c := newClient(oauth.WithPrivateAddressesAllowed())
	d := serviceClient(PrivateHostOptions(true)...)
	_, _ = c, d
}
`)

	code, out := f.run()
	requireClean(t, code, out, "the hatch in a _test.go file")
}

// TestHatchDeclarationAndGateHelperAreIgnored.
//
// Ten packages declare the option and a PrivateXOptions helper whose body is
// the one sanctioned call to it. Both are production source and both name the
// identifier, so a naive rule carries a floor of twenty. The declaration is
// excluded structurally — a `func` line is a declaration, never a call — and the
// helper body carries a marker, so adding an eleventh package's helper is a
// visible edit in review rather than a silent one.
//
// THE `func` LINE CARRIES A MARKER TOO, and it is not redundant with the
// structural exclude. The exclude drops the declaration; the marker covers the
// line BELOW it, which is the option's one-line body — and that body is the
// hatch's actual mechanism, `s.allowPrivateHosts = true`. This fixture used to
// omit the marker, which made it a shape no file in the real tree has: all ten
// declarations carry one. The rule keyed on the assignment found the difference.
func TestHatchDeclarationAndGateHelperAreIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/core/unfurl/service.go", `package unfurl

// WithPrivateHostsAllowed disables the SSRF address guard on every unfurl fetch.
//
// THE NAME IS THE CONTRACT: production must not call this.
func WithPrivateHostsAllowed() ServiceOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(s *service) { s.allowPrivateHosts = true }
}

// PrivateHostOptions returns the hatch when the boolean is set, and NOTHING when
// it is not.
func PrivateHostOptions(allowPrivate bool) []ServiceOption {
	if !allowPrivate {
		return nil
	}
	// coves:allow-ssrf-hatch: the fixture's dev gate; the sole sanctioned call
	return []ServiceOption{WithPrivateHostsAllowed()}
}
`)

	code, out := f.run()
	requireClean(t, code, out, "the hatch declaration and its dev-gate helper body")
}

// TestHatchFieldAssignmentFailsUnderAnyOptionName is the mechanism rule, and it
// is the one test here that would still hold if every identifier in the tree
// were renamed.
//
// Rule 3 knew the CONVENTION — `WithPrivateHostsAllowed`, `PrivateHostOptions` —
// and the convention was beaten twice by code already in this repository, in
// neither case by anyone trying: internal/atproto/jetstream/authorpost.go
// declares `withPrivateHostsAllowed` (unexported, for reasons of its own) and
// gates it behind `PrivatePostFetcherOptions`. One lowercase letter and one
// different noun, and that file's hatch, its gate helper and its allow-branch
// were all invisible to the rule meant to count them.
//
// The fixture below carries NEITHER conventional spelling. There is no `With`,
// no `Allowed`, no `Options`; the option is called `devMode`. The only thing
// left to match is the assignment the compiler actually acts on, which is the
// point: a hatch is a field set to true, and its name is decoration.
func TestHatchFieldAssignmentFailsUnderAnyOptionName(t *testing.T) {
	for name, body := range map[string]string{
		"assignment":        "func devMode() Option {\n\treturn func(t *transport) { t.allowPrivateAddresses = true }\n}\n",
		"struct field":      "func wire() Config {\n\treturn Config{AllowPrivateIPs: true}\n}\n",
		"short declaration": "func wire() {\n\tallowPrivateHosts := true\n\tbuild(allowPrivateHosts)\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/opt.go", "package example\n\n"+body)

			code, out := f.run()
			requireViolation(t, code, out, "an allow-private field set to true via "+name)
		})
	}
}

// TestUnexportedHatchSpellingsFail is the same defect at the two spellings that
// actually occurred, kept separate from the test above because these are not
// hypotheses — they are what the tree contained while the rule reported nine
// exemptions and zero violations.
func TestUnexportedHatchSpellingsFail(t *testing.T) {
	for name, line := range map[string]string{
		"unexported option":           "build(withPrivateHostsAllowed())",
		"gate helper by another name": "build(PrivatePostFetcherOptions(true)...)",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("cmd/server/extra.go",
				"package main\n\nfunc wireMore() {\n\t"+line+"\n}\n")

			code, out := f.run()
			requireViolation(t, code, out, name+" in production code")
		})
	}
}

// TestHatchFieldDerivedFromConfigIsIgnored is the boundary the rule above must
// not cross, and it is the sanctioned production form: the field is set from the
// dev gate rather than from a literal.
//
// cmd/server/wiring.go and cmd/rematerialize-posts/main.go both write exactly
// this. A rule that flagged it would put a floor under the two lines the whole
// dev-gate design exists to produce.
func TestHatchFieldDerivedFromConfigIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("cmd/server/extra.go", `package main

func wireMore(a *application) oauth.Config {
	return oauth.Config{
		AllowPrivateIPs: a.allowPrivateHosts(),
	}
}

func (a *application) allowPrivateHosts() bool { return a.cfg.IsDevEnv }
`)

	code, out := f.run()
	requireClean(t, code, out, "an allow-private field derived from cfg.IsDevEnv")
}

// TestHatchOptionBodyOnOneLineIsIgnored pins the shape all ten real hatch
// options have, and the reason they have it.
//
// The assignment alternative means every option BODY now matches, so each needs
// the marker in the window scan() looks at: its own line, or the line above. All
// ten are one-liners whose `func` line above carries the marker, so the cost of
// the mechanism rule across the whole repository is zero new markers on that
// shape.
//
// An option body spread over three lines puts the assignment outside that window
// and fails. That is deliberate and is the next test.
func TestHatchOptionBodyOnOneLineIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write("internal/core/unfurl/service.go", `package unfurl

// WithPrivateHostsAllowed disables the SSRF address guard on every unfurl fetch.
func WithPrivateHostsAllowed() ServiceOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(s *service) { s.allowPrivateHosts = true }
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a one-line hatch option body under its func-line marker")
}

// TestHatchOptionBodySpreadPastItsMarkerFails is the cost of the marker window,
// stated rather than discovered.
//
// The marker is on the `func` line and the assignment is two lines below it, so
// it falls outside the line/line-above window and the rule fires. This is the
// real internal/atproto/oauth/transport.go as it stood before this rule landed;
// it was collapsed onto one line to match the other nine rather than given a
// second marker, because ten hatches that look alike is itself a defense — a
// reader can see at a glance that none of them does anything extra.
//
// The failure is a feature: a hatch that stops looking like the other nine is
// worth one review, and the fix is one marker or one line.
func TestHatchOptionBodySpreadPastItsMarkerFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/opt.go", `package example

func WithPrivateAddressesAllowed() Option { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(t *ssrfSafeTransport) {
		t.allowPrivate = true
	}
}
`)

	code, out := f.run()
	requireViolation(t, code, out,
		"a hatch assignment two lines below its marker, outside the marker window")
}

// ---------------------------------------------------------------------------
// Rule 4 — CGO_ENABLED=0 in the production build
// ---------------------------------------------------------------------------

// TestProductionDockerfileCGODisabledPasses pins today's form.
func TestProductionDockerfileCGODisabledPasses(t *testing.T) {
	f := newFixture(t)

	code, out := f.run()
	requireClean(t, code, out, "a Dockerfile building ./cmd/server with CGO_ENABLED=0")
}

// TestCGOEnabledFails is the flag being flipped.
//
// A build that actually reaches getaddrinfo hands 0x7f.0.0.1, 2130706433, 127.1,
// 127.0.0.0x1 and 127.0.0.01 to it, and it resolves every one to loopback.
// net.ParseIP returns nil for all five, so none is refused on shape.
//
// "ACTUALLY REACHES" IS DOING WORK IN THAT SENTENCE. On the alpine image this
// Dockerfile builds, CGO_ENABLED=1 alone does NOT get there — musl still selects
// the pure-Go resolver, and only GODEBUG=netdns=cgo switches it. The rule still
// earns its place: the flag is one of the things keeping that path out of reach,
// and the audit cannot see a GODEBUG set at runtime. But this test proves the
// RULE fires, not that flipping the flag opens the hole on this image. Keeping
// those two claims apart is the point — see cleanDockerfile, where conflating
// them is a mistake this suite already made once.
func TestCGOEnabledFails(t *testing.T) {
	f := newFixture(t)
	f.write("Dockerfile", strings.Replace(cleanDockerfile,
		buildLine, "RUN CGO_ENABLED=1 GOOS=linux", 1))

	code, out := f.run()
	requireViolation(t, code, out, "CGO_ENABLED=1 in the production build")
}

// TestCGOFlagRemovedFails is the drift that actually happens, and it is the
// mutation the rule could not see.
//
// Nobody sets CGO_ENABLED=1 on purpose. What happens is that the flag gets
// dropped during an unrelated Dockerfile edit — and the comment above it in the
// real Dockerfile says "for static binary (no libc dependency)", which reads
// like a size optimisation and invites exactly that deletion. Go's default is
// CGO_ENABLED=1 when a C toolchain is present, so removal is not neutral.
//
// The comment SURVIVES the deletion, because deleting a flag and deleting the
// paragraph explaining it are separate edits and nobody makes the second one. A
// rule that greps the file for the flag's text therefore reads its own
// explanation and reports green over a cgo build — which is what
// scripts/ssrf-audit.sh did until this test.
func TestCGOFlagRemovedFails(t *testing.T) {
	f := newFixture(t)
	mutated := strings.Replace(cleanDockerfile, buildLine, "RUN GOOS=linux", 1)

	if !strings.Contains(mutated, "# CGO_ENABLED=0 for static binary") {
		t.Fatalf("the fixture lost the explanatory comment, so this test no longer "+
			"exercises the deletion it exists for:\n%s", mutated)
	}
	f.write("Dockerfile", mutated)

	code, out := f.run()
	requireViolation(t, code, out,
		"a build command with no CGO_ENABLED, the flag surviving only in a comment")
}

// TestCGOInCommentOnlyFails is the same defect stated at its narrowest: a
// Dockerfile whose ONLY mention of the flag is prose.
//
// The distinction matters because the rule is about a property of the produced
// binary, and a comment produces nothing. An audit satisfied by a file
// describing the configuration it does not have is an audit measuring its own
// documentation.
func TestCGOInCommentOnlyFails(t *testing.T) {
	f := newFixture(t)
	f.write("Dockerfile", `FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY . .

# CGO_ENABLED=0 keeps the binary static and the pure-Go resolver in play.
RUN go build -o /build/coves-server ./cmd/server

FROM alpine:3.19
COPY --from=builder /build/coves-server /app/coves-server
ENTRYPOINT ["/app/coves-server"]
`)

	code, out := f.run()
	requireViolation(t, code, out, "a Dockerfile mentioning CGO_ENABLED=0 only in a comment")
}

// TestCGODisabledInAnotherStageFails.
//
// ENV is scoped to the stage that declares it, so `ENV CGO_ENABLED=0` in the
// alpine RUNTIME stage has no effect whatsoever on the builder's `go build`. It
// is nevertheless the shape a file-wide grep is happiest with, and the shape a
// careless refactor produces when a stage gets reordered.
func TestCGODisabledInAnotherStageFails(t *testing.T) {
	f := newFixture(t)
	mutated := strings.Replace(cleanDockerfile, buildLine, "RUN GOOS=linux", 1)
	mutated = strings.Replace(mutated, "FROM alpine:3.19",
		"FROM alpine:3.19\nENV CGO_ENABLED=0", 1)
	f.write("Dockerfile", mutated)

	code, out := f.run()
	requireViolation(t, code, out,
		"CGO_ENABLED=0 set in the runtime stage, where it cannot reach the build")
}

// TestDockerfileWithNoBuildCommandFails is the missing-Dockerfile rule one step
// in: a Dockerfile that exists but compiles nothing gives the rule no build to
// assert about, and a rule with no subject must not report green. It is the same
// silent pass as an absent file, arrived at by a different route.
func TestDockerfileWithNoBuildCommandFails(t *testing.T) {
	f := newFixture(t)
	f.write("Dockerfile", `FROM alpine:3.19
COPY coves-server /app/coves-server
ENTRYPOINT ["/app/coves-server"]
`)

	code, out := f.run()
	requireViolation(t, code, out, "a Dockerfile that builds nothing")
}

// TestCGODisabledViaENVPasses keeps the rule about the OUTCOME rather than about
// one line's spelling. `ENV CGO_ENABLED=0` earlier in the stage disables cgo for
// the build just as effectively, and a rule that demanded the flag be on the
// `go build` line would reject a legitimate refactor and teach whoever hit it
// that the audit is wrong — which is the first step to it being bypassed.
func TestCGODisabledViaENVPasses(t *testing.T) {
	f := newFixture(t)
	f.write("Dockerfile", strings.Replace(cleanDockerfile,
		"RUN CGO_ENABLED=0 GOOS=linux go build",
		"ENV CGO_ENABLED=0\nRUN GOOS=linux go build", 1))

	code, out := f.run()
	requireClean(t, code, out, "CGO_ENABLED=0 set via ENV in the build stage")
}

// TestMissingDockerfileFails.
//
// The rule asserts a property of the production build, so it cannot be satisfied
// by there being no production build to check. An audit that passes when its
// subject is absent is the same silent green as an audit that greps for nothing.
func TestMissingDockerfileFails(t *testing.T) {
	f := newFixture(t)
	f.remove("Dockerfile")

	code, out := f.run()
	requireViolation(t, code, out, "a missing production Dockerfile")
}

// ---------------------------------------------------------------------------
// Rule 5 — the exemption mechanism itself
// ---------------------------------------------------------------------------
//
// An exemption is the audit's only way to say "unguarded on purpose", and the
// header stakes everything on it being a DOCUMENTED DECISION: an allowlist entry
// converts an oversight into a recorded choice, and the reason is the only part
// of it a reviewer can disagree with. Three properties have to hold for that
// sentence to be true, and none of them was checked.

// TestMarkerWithoutAReasonDoesNotExempt.
//
// The header advertises `// coves:allow-bare-client: <reason>`, but the check
// was a fixed-string grep for the marker without its colon — so the bare token,
// which documents nothing, silenced the rule exactly as well as a justification
// would. A reason nobody has to write is a reason nobody writes.
func TestMarkerWithoutAReasonDoesNotExempt(t *testing.T) {
	for name, marker := range map[string]string{
		"no colon":     "// coves:allow-bare-client",
		"empty reason": "// coves:allow-bare-client:",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/fetch.go", "package example\n\n"+
				"func newClient() *http.Client {\n\t"+marker+"\n"+
				"\treturn &http.Client{Timeout: 10 * time.Second}\n}\n")

			code, out := f.run()
			requireViolation(t, code, out, "a bare client under a marker with "+name)
		})
	}
}

// TestMarkerInsideAStringLiteralDoesNotExempt.
//
// The marker was matched anywhere on the line, so any string carrying its text
// disabled the rule for the line below. That is not only a bypass someone could
// write on purpose — it is what happens when a log message, a test name or an
// error string quotes the convention while explaining it.
//
// The URL case is the one a naive "must contain //" fix still gets wrong: the
// slashes are inside the quotes.
func TestMarkerInsideAStringLiteralDoesNotExempt(t *testing.T) {
	for name, line := range map[string]string{
		"plain string": `	log.Print("coves:allow-bare-client: this is prose, not a decision")`,
		"url string":   `	const doc = "https://coves.social/docs#coves:allow-bare-client: prose"`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("internal/example/fetch.go", "package example\n\n"+
				"func newClient() *http.Client {\n"+line+"\n"+
				"\treturn &http.Client{Timeout: 10 * time.Second}\n}\n")

			code, out := f.run()
			requireViolation(t, code, out, "a marker appearing in a "+name)
		})
	}
}

// TestFileMarkerWithoutAReasonDoesNotExempt holds the file-scope form to the
// same terms. It is the broadest grant the audit can make — every bare-client
// rule, over the whole file, forever — so an unreasoned one is the largest
// undocumented decision in the repository.
func TestFileMarkerWithoutAReasonDoesNotExempt(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/transport.go", `package example

// coves:allow-bare-client-file:

func newClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "a file-scope marker carrying no reason")
}

// TestStaleLineMarkerFails is the third property: an exemption outliving the
// thing it exempted.
//
// A marker whose violation was fixed or deleted is not harmless. It is a
// standing grant sitting in the file, invisible to every rule because there is
// nothing there to match, and the next bare client written under it inherits a
// justification written about different code — which is precisely the "wrong
// entry converts an oversight into a recorded choice nobody revisits" failure
// the header names as worse than a noisy audit.
func TestStaleLineMarkerFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch.go", `package example

func newClient(allowPrivate bool) *http.Client {
	// coves:allow-bare-client: fixed vendor API host, no caller-supplied URL
	return oauth.NewSSRFSafeHTTPClient(oauth.PrivateAddressOptions(allowPrivate)...)
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "an exemption marker with nothing left to exempt")
}

// TestStaleFileMarkerFails is the same for the broad form, and it is the one
// that matters more: a whole-file grant over a file that no longer builds a
// client is a blank cheque, and the file-scope exemption table cannot show it
// because that table only prints files with hits.
func TestStaleFileMarkerFails(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/doc.go", `package example

// coves:allow-bare-client-file: this file IS the guarded client's construction

func describe() string {
	return "the guard moved to internal/atproto/oauth"
}
`)

	code, out := f.run()
	requireViolation(t, code, out, "a file-scope exemption over a file with no hits")
}

// TestMarkerAboveItsViolationIsNotStale is the other half, and without it the
// rule above would flag most of the real tree.
//
// Go's convention puts the explanation above the statement, and the two markers
// in cmd/reindex-votes/main.go are written that way. A staleness rule that could
// not see the line below the marker would be a permanent floor over exactly the
// sites the remediation documented most carefully.
func TestMarkerAboveItsViolationIsNotStale(t *testing.T) {
	f := newFixture(t)
	f.write("cmd/reindex/main.go", `package main

func fetchAll(pdsURL string) {
	// coves:allow-bare-client: pdsURL is PDS_URL from the environment, so the host is operator config
	resp, err := http.Get(pdsURL + "/xrpc/com.atproto.sync.listRepos")
	_, _ = resp, err
}
`)

	code, out := f.run()
	requireClean(t, code, out, "a marker on the line above its violation")
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// TestVerboseReportsFileAndLine. A hard gate that says "1 violation" without
// saying where is a gate people disable.
func TestVerboseReportsFileAndLine(t *testing.T) {
	f := newFixture(t)
	f.write("internal/example/fetch.go", `package example

func fetch() {
	client := &http.Client{Timeout: 10 * time.Second}
	_ = client
}
`)

	code, out := f.run("-v")
	requireViolation(t, code, out, "a bare client under -v")

	if !strings.Contains(out, "internal/example/fetch.go:4") {
		t.Errorf("-v did not report the offending file:line.\n%s", out)
	}
}

// TestVerboseOnCleanTreeDoesNotCrash.
//
// scripts/test-audit.sh carries a length guard for this exact reason: under
// `set -u`, bash 3.2 — which is what ships on macOS, where developers run this —
// treats "${ARRAY[@]}" on an EMPTY array as an unbound variable and aborts. So
// `-v` on a clean tree, the run most likely to be someone's first, is the one
// run that crashes. runAudit already rejects any exit code above 1, so a crash
// here fails rather than being read as a violation.
func TestVerboseOnCleanTreeDoesNotCrash(t *testing.T) {
	f := newFixture(t)

	code, out := f.run("-v")
	requireClean(t, code, out, "a clean tree under -v")
}
