package unfurl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// The unfurl fetch, against a destination a signed-up account chose.
//
// # WHY THIS IS THE MOST CAPABLE OF THE NINE SITES
//
// Every other site on the remediation list either discards the response body or
// treats it as opaque bytes. This one PARSES the body and RETURNS ITS CONTENT to
// the caller: og:title, og:description and og:image come back in an UnfurlResult,
// get written to the unfurl cache, and are then served to whoever reads the post.
// So the primitive is not "can the AppView reach this address" — that is the
// image proxy's, and it is a port scanner. This one is "tell me what it said".
//
// The URL is a link pasted into a post. The cost of the credential is a signup.
// The AppView shares a network with its Postgres, its PDS, its Jetstream and, in
// production, a metadata endpoint at 169.254.169.254 that hands credentials to
// anything that can reach it — and an internal HTTP endpoint that answers with a
// JSON banner, an error page naming a version, or an admin console title is an
// endpoint whose content lands in a public post.
//
// # WHY THREE PATHS AND NOT ONE
//
// providers.go built THREE separate `&http.Client{Timeout: timeout}` values, one
// per fetch function. Converting two of three is the shape this failure takes:
// each is a few lines apart, each looks like the one above it, and the survivor
// is reachable by choosing a different link. So every case below runs against all
// three individually rather than against whichever one the service happens to
// route to.
//
// # WHY REACHABILITY IS ASSERTED AND NOT ONLY THE ERROR
//
// Mutation testing produced an implementation that classified
// every address correctly, emitted a byte-identical error message, and refused
// the request AFTER delivering it. Every error-message assertion in the suite
// passed against it. For a destination a stranger named, the packet leaving IS
// the SSRF — so each case stands up a real listener and asserts its handler was
// never invoked.
//
// # WHY THESE TESTS ARE NOT PARALLEL
//
// The oEmbed path routes through the package-level oEmbedEndpoints map: the
// endpoint it dials is looked up from the link's domain, not taken from the link,
// so the only way to point that path at a listener this test owns is to register
// a domain in the map. Mutating it races TestIsOEmbedProvider and TestIsSupported,
// which read it under t.Parallel(). A test that does not call t.Parallel() runs in
// the sequential phase, and the testing package resumes parallel tests only once
// that phase is over — so serial is what makes the registration safe, and it is
// cheap here because nothing below waits on anything.

// secretInternalHTML is what an internal endpoint answers with, written so that a
// leak is visible in the assertion rather than inferred from a nil check.
//
// It carries a kagiproxy.com <img> as well as the og: tags because fetchKagiKite
// fails with "no image found" without one, and a path that errors for an
// unrelated reason proves nothing about the guard.
const secretInternalHTML = `<!DOCTYPE html>
<html>
<head>
	<title>Internal Admin Console</title>
	<meta property="og:title" content="Internal Admin Console" />
	<meta property="og:description" content="cluster credentials rotate at 04:00 UTC" />
	<meta property="og:image" content="https://kagiproxy.com/img/internal-topology.png" />
</head>
<body>
	<img src="https://kagiproxy.com/img/internal-topology.png" alt="internal network topology" />
</body>
</html>`

// secretInternalOEmbed is the same leak in the shape the oEmbed path parses.
const secretInternalOEmbed = `{
	"version": "1.0",
	"type": "video",
	"title": "Internal Admin Console",
	"description": "cluster credentials rotate at 04:00 UTC",
	"thumbnail_url": "https://internal.invalid/img/internal-topology.png",
	"provider_name": "Internal"
}`

// leakedTitle is the one string that must never come back from a refused unfurl.
const leakedTitle = "Internal Admin Console"

// countingTarget is a listener that answers like an internal service and records
// whether anything ever reached it. It listens on loopback, which is exactly the
// address class the guard exists to refuse, so the counter doubles as the
// assertion.
type countingTarget struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newCountingTarget(t *testing.T, contentType, body string) *countingTarget {
	t.Helper()

	target := &countingTarget{}
	target.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target.requests.Add(1)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(target.server.Close)
	return target
}

// unfurledContent is what a caller of any of the three paths ends up holding,
// normalised across their three different return types.
//
// returned is separate from the three strings on purpose: "an error came back
// alongside a populated result" and "nothing came back" are different outcomes,
// and only the second one is a closed site. A caller that ignores the error — or
// logs it and carries on, which is what a best-effort enrichment path does — gets
// the leak either way.
type unfurledContent struct {
	title       string
	description string
	image       string
	returned    bool
}

// providerPath describes one of the three guarded unfurl constructions.
type providerPath struct {
	name        string
	why         string
	contentType string
	body        string

	// targetURL maps a listener's base URL onto the URL this path must be asked
	// to unfurl, registering whatever package state the path needs to route
	// there. It takes *testing.T so a registration can be undone by t.Cleanup.
	targetURL func(t *testing.T, base string) string

	fetch func(ctx context.Context, target string, client *http.Client) (unfurledContent, error)
}

// providerPaths enumerates the three fetch sites, so the refusal test and the
// hatch test cannot drift apart in which paths they cover.
func providerPaths() []providerPath {
	return []providerPath{
		{
			name:        "OpenGraph",
			contentType: "text/html; charset=utf-8",
			body:        secretInternalHTML,
			why: "the default path: every link that is not a known oEmbed provider lands here, so this " +
				"is the one an attacker reaches by pasting any URL at all",
			targetURL: func(_ *testing.T, base string) string { return base + "/article" },
			fetch: func(ctx context.Context, target string, client *http.Client) (unfurledContent, error) {
				result, err := fetchOpenGraph(ctx, target, client, "CovesBot/1.0")
				if result == nil {
					return unfurledContent{}, err
				}
				return unfurledContent{
					title:       result.Title,
					description: result.Description,
					image:       result.ThumbnailURL,
					returned:    true,
				}, err
			},
		},
		{
			name:        "Kagi Kite",
			contentType: "text/html; charset=utf-8",
			body:        secretInternalHTML,
			why: "a third client construction, converted or not independently of the other two. It is " +
				"unreachable through UnfurlURL today (see the dead-branch note below), which is exactly " +
				"why it is the one a conversion forgets",
			targetURL: func(_ *testing.T, base string) string { return base + "/abc/science/9" },
			fetch: func(ctx context.Context, target string, client *http.Client) (unfurledContent, error) {
				result, err := fetchKagiKite(ctx, target, client, "TestBot/1.0")
				if result == nil {
					return unfurledContent{}, err
				}
				return unfurledContent{
					title:       result.Title,
					description: result.Description,
					image:       result.ThumbnailURL,
					returned:    true,
				}, err
			},
		},
		{
			name:        "oEmbed",
			contentType: "application/json",
			body:        secretInternalOEmbed,
			why: "the endpoint here is looked up from a package map rather than taken from the link, so " +
				"it is the path that looks safest and is the easiest to leave unconverted. The map is " +
				"not a control: it is a routing table, and a single entry pointed anywhere private — by " +
				"an edit, or by a provider domain whose DNS answer changes — dials it with no guard at all",
			targetURL: func(t *testing.T, base string) string {
				t.Helper()
				// A reserved TLD, so a lookup can never escape even if this
				// registration were ever left behind by a failed cleanup.
				const domain = "oembed-ssrf-probe.invalid"
				oEmbedEndpoints[domain] = base
				t.Cleanup(func() { delete(oEmbedEndpoints, domain) })
				return "https://" + domain + "/watch"
			},
			fetch: func(ctx context.Context, target string, client *http.Client) (unfurledContent, error) {
				oembed, err := fetchOEmbed(ctx, target, client, "CovesBot/1.0")
				if oembed == nil {
					return unfurledContent{}, err
				}
				return unfurledContent{
					title:       oembed.Title,
					description: oembed.Description,
					image:       oembed.ThumbnailURL,
					returned:    true,
				}, err
			},
		},
	}
}

// serviceHTTPClient builds a service the way cmd/server does and hands back the
// client it constructed.
//
// GOING THROUGH NewService IS THE POINT. A test that built its own SSRF-safe
// client would assert that internal/atproto/oauth works, which its own tests
// covers exhaustively. What is unproven here is that THIS package's constructor
// wires one — with this site's own byte cap and this site's own timeout — and
// hands it to all three fetch functions. Reaching for the field is how that stays
// the thing under test.
func serviceHTTPClient(t *testing.T, opts ...ServiceOption) *http.Client {
	t.Helper()

	svc, ok := NewService(nil, opts...).(*service)
	require.True(t, ok, "NewService must return the concrete *service this package's tests drive")
	require.NotNil(t, svc.httpClient,
		"NewService built no HTTP client, so every fetch below would nil-panic rather than assert anything")
	return svc.httpClient
}

// guardedClient is what production gets: the dev gate answering false, and
// nothing else applied.
func guardedClient(t *testing.T, timeout time.Duration) *http.Client {
	t.Helper()
	return serviceHTTPClient(t, append([]ServiceOption{WithTimeout(timeout)}, PrivateHostOptions(false)...)...)
}

// hatchOpenClient is what a developer and every httptest fixture in this package
// gets: the dev gate answering true.
func hatchOpenClient(t *testing.T, timeout time.Duration) *http.Client {
	t.Helper()
	return serviceHTTPClient(t, append([]ServiceOption{WithTimeout(timeout)}, PrivateHostOptions(true)...)...)
}

// TestUnfurlProviders_RefuseAPrivateAddressWithoutReachingIt is the binding
// contract for unfurl egress.
//
// Each of the three fetch sites is pointed at a real listener on loopback and
// must refuse it before a packet leaves the process — and must hand back nothing.
//
// Not parallel: see the oEmbed registration note in the file header.
func TestUnfurlProviders_RefuseAPrivateAddressWithoutReachingIt(t *testing.T) {
	for _, path := range providerPaths() {
		t.Run(path.name, func(t *testing.T) {
			target := newCountingTarget(t, path.contentType, path.body)

			content, err := path.fetch(
				context.Background(),
				path.targetURL(t, target.server.URL),
				guardedClient(t, 5*time.Second),
			)

			require.Errorf(t, err,
				"the %s path fetched a loopback address successfully. The URL is a link an account "+
					"pasted into a post, and this path returns what the address ANSWERED — %s",
				path.name, path.why)

			assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
				"the %s path failed, but not because the guard refused it: the error does not match "+
					"covesoauth.ErrBlockedAddress. An unguarded client pointed at an address that "+
					"happens to be unreachable fails too, and looks identical from here — so without "+
					"this assertion the case above passes on a site that was never converted; got: %v",
				path.name, err)

			assert.Zerof(t, target.requests.Load(),
				"the %s path reached the listener %d times. The refusal happened, but it happened "+
					"AFTER the request was delivered — which prevents none of the SSRF and is precisely "+
					"the implementation that passed every error-message assertion during "+
					"mutation testing", path.name, target.requests.Load())

			assert.Falsef(t, content.returned,
				"the %s path returned a result alongside its error. This site is the one that hands "+
					"RESPONSE CONTENT back, and a caller that logs the error and carries on — which is "+
					"what a best-effort enrichment path does — publishes it anyway. A refused unfurl "+
					"must be nothing at all, not an error with a payload attached", path.name)

			assert.NotContainsf(t, content.title, leakedTitle,
				"the %s path returned the internal page's title (%q) from an address it was supposed "+
					"to refuse. This is the harm: the og: tags of an internal endpoint, parsed and "+
					"handed to the caller that named it", path.name, content.title)
			assert.Emptyf(t, content.description,
				"the %s path returned a description (%q) from a refused address", path.name, content.description)
			assert.Emptyf(t, content.image,
				"the %s path returned an image URL (%q) from a refused address", path.name, content.image)
		})
	}
}

// TestUnfurlProviders_ReachTheListenerWhenTheHatchIsOpen is the other direction,
// and it is not a nicety.
//
// Every unit test in this package serves its fixture from httptest, which listens
// on loopback — kagi_test.go and opengraph_test.go both do, through this same
// helper — and a local dev stack runs everything on the developer's own machine.
// Without a working hatch the guard takes all of that with it.
//
// It is also the behavioural half of the PrivateHostOptions contract: the client
// here is built from PrivateHostOptions(true), so a helper that returns the wrong
// option — or none — fails here rather than silently leaving developers with a
// client that cannot reach anything local.
//
// Not parallel: see the oEmbed registration note in the file header.
func TestUnfurlProviders_ReachTheListenerWhenTheHatchIsOpen(t *testing.T) {
	for _, path := range providerPaths() {
		t.Run(path.name, func(t *testing.T) {
			target := newCountingTarget(t, path.contentType, path.body)

			content, err := path.fetch(
				context.Background(),
				path.targetURL(t, target.server.URL),
				hatchOpenClient(t, 5*time.Second),
			)

			require.NoErrorf(t, err,
				"the %s path was refused with the hatch open. Every fixture in this package and every "+
					"dev stack depends on this: PrivateHostOptions(true) must produce a client that "+
					"reaches loopback; got: %v", path.name, err)
			require.Truef(t, content.returned, "the %s path returned no result with the hatch open", path.name)

			assert.Equalf(t, leakedTitle, content.title,
				"the %s path came back with title %q. The body must survive the guarded transport "+
					"unchanged — a conversion that reaches the listener but mangles what it parses is "+
					"a different regression wearing the same green", path.name, content.title)
			assert.NotEmptyf(t, content.description, "the %s path lost the description", path.name)
			assert.NotEmptyf(t, content.image, "the %s path lost the image URL", path.name)
			assert.Equalf(t, int64(1), target.requests.Load(),
				"the %s path reached the listener %d times rather than once",
				path.name, target.requests.Load())
		})
	}
}

// recordingRepo is a cold unfurl cache that records what was written to it.
type recordingRepo struct {
	mu     sync.Mutex
	stored []*UnfurlResult
}

func (r *recordingRepo) Get(context.Context, string) (*UnfurlResult, error) { return nil, nil }

func (r *recordingRepo) Set(_ context.Context, _ string, result *UnfurlResult, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stored = append(r.stored, result)
	return nil
}

func (r *recordingRepo) writes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stored)
}

// TestUnfurlService_UnfurlURL_RefusesAPrivateAddressAndCachesNothing proves the
// assembled path is closed, not just the fetch function in isolation.
//
// # WHY THE CACHE ASSERTION IS HERE
//
// UnfurlURL writes every successful result to the unfurl cache with a 24h TTL,
// and the read path serves that cache before it checks anything else. So a leak
// that got through once is served from Postgres for a day afterwards, to every
// reader of the post, with no further requests to the internal address and
// nothing left in the logs to connect the two. "Nothing was cached" is therefore
// a distinct claim from "an error was returned", and it is the one that bounds
// the blast radius.
//
// The repository starts cold and says so: if Get ever returned a hit, UnfurlURL
// would answer from it without calling the fetch at all, and this case would pass
// having exercised nothing.
func TestUnfurlService_UnfurlURL_RefusesAPrivateAddressAndCachesNothing(t *testing.T) {
	t.Parallel()

	target := newCountingTarget(t, "text/html; charset=utf-8", secretInternalHTML)
	repo := &recordingRepo{}

	svc := NewService(repo, append([]ServiceOption{WithTimeout(5 * time.Second)}, PrivateHostOptions(false)...)...)

	result, err := svc.UnfurlURL(context.Background(), target.server.URL+"/article")

	require.Error(t, err,
		"the assembled unfurl service fetched a loopback address. This is the production shape: a link "+
			"pasted into a post by anyone with an account")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the service must pass the guard's refusal through with its identity intact, so a log line and "+
			"an alert can tell a security refusal from a site that was merely down; got: %v", err)
	assert.Nil(t, result,
		"the service returned a result for a refused address. UnfurlURL's result is embedded in the post "+
			"and served to every reader of it")
	assert.Zerof(t, target.requests.Load(),
		"the service reached the listener %d times", target.requests.Load())
	assert.Zerof(t, repo.writes(),
		"the service wrote %d entr(y/ies) to the unfurl cache for a refused address. A cached leak is "+
			"served for the full 24h TTL without any further request to the internal address, so a "+
			"single success outlives the request that caused it", repo.writes())
}

// TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed is the
// single most important assertion for this call site.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` — the hermetic merge gate,
// T0+T1+T2 — runs the PERMISSIVE branch here and at every other site holding such
// a boolean. A green merge gate therefore proves nothing whatsoever about whether
// unfurl is guarded in production. This function is the one place in the
// repository where the production branch is ever evaluated, which is why the gate
// must be a pure function and not an `if cfg.IsDevEnv` in cmd/server/wiring.go.
//
// The claim is not "the options returned are safe". It is that there are NONE:
// length zero, nothing applied, the constructor's own defaults left untouched. An
// edit that appends a diagnostic option, or returns a one-element slice holding a
// no-op "explicitly deny" closure, keeps every behavioural test green while moving
// the untested branch from "provably applies nothing" to "applies something
// believed harmless". If this assertion is ever in the way, the answer is not to
// relax it.
func TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivateHostOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivateHostOptions(false) returned %d option(s). The production branch — the one IS_DEV_ENV=true "+
			"keeps `make ci` from ever evaluating — must contribute nothing at all, so that what "+
			"production gets is exactly the constructor's own defaults", len(opts))
}

// TestPrivateHostOptions_DisallowedServiceIsGuarded is the behavioural half of
// the assertion above: zero options has to also MEAN a guarded client.
//
// The length check alone would still pass if NewService's own default ever
// regressed to permissive — the helper would correctly be returning nothing, onto
// a base that no longer refuses anything.
func TestPrivateHostOptions_DisallowedServiceIsGuarded(t *testing.T) {
	t.Parallel()

	target := newCountingTarget(t, "text/html; charset=utf-8", secretInternalHTML)

	result, err := fetchOpenGraph(
		context.Background(),
		target.server.URL+"/article",
		guardedClient(t, 5*time.Second),
		"CovesBot/1.0",
	)

	require.Error(t, err,
		"a client built from PrivateHostOptions(false) reached a loopback address. This is the branch "+
			"production runs and CI never does")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must be the guard's, matchable by identity — a request that failed for some other "+
			"reason is not the same control and would not hold in production; got: %v", err)
	assert.Nil(t, result, "a refused fetch must return no metadata")
	assert.Zerof(t, target.requests.Load(),
		"the listener was reached %d times, so the packet left the process", target.requests.Load())
}

// TestUnfurlFetch_BodyCapStaysAtThisSitesOwnLimit is the clamp trap in the
// direction this site can fall.
//
// providers.go caps both HTML reads at 10 MB through an io.LimitReader. The
// shared transport carries a cap of its own, defaulting to 32 MiB, and a
// conversion that simply adopts NewSSRFSafeHTTPClient() inherits that default —
// which does not fail anything, does not warn, and quietly triples what a remote
// host can make this process allocate before anything objects. Nothing in the
// suite would notice: 32 MiB is larger, so every existing fixture still passes.
//
// The window between the two limits is where they can be told apart. 12 MiB is
// above this site's 10 MB and below the transport's 32 MiB default, so it is
// refused by a correctly converted client and accepted by one carrying the
// package default.
//
// # WHY A DECLARED LENGTH AND NOT A REAL BODY
//
// A streamed 12 MiB body would NOT discriminate, and the reason is worth knowing
// before setting the cap: the site reads through io.LimitReader(resp.Body, 10MB),
// which stops requesting bytes at exactly 10 MB, so a transport cap set to 10 MB
// is never asked for the byte that would trip it. The oversized body arrives
// silently truncated — the pre-existing behaviour — and no error is produced by
// either layer. The announced-length branch is the one that actually fires, and
// the one this asserts.
func TestUnfurlFetch_BodyCapStaysAtThisSitesOwnLimit(t *testing.T) {
	t.Parallel()

	// Above providers.go's 10 MB, below covesoauth.DefaultMaxResponseBytes.
	const declared = 12 << 20

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(declared))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	// The hatch is open so the ADDRESS is out of the picture entirely: the only
	// thing left that can refuse this response is a byte cap.
	result, err := fetchOpenGraph(
		context.Background(),
		server.URL+"/article",
		hatchOpenClient(t, 5*time.Second),
		"CovesBot/1.0",
	)

	require.Errorf(t, err,
		"a response declaring %d bytes was accepted by a site whose own limit is 10 MB. The cap that "+
			"should have refused it is the shared transport's, and it is only this permissive if the "+
			"conversion inherited covesoauth.DefaultMaxResponseBytes (%d) instead of passing this "+
			"site's own limit", declared, covesoauth.DefaultMaxResponseBytes)
	assert.ErrorIsf(t, err, covesoauth.ErrResponseTooLarge,
		"the response was refused, but not by the byte cap. Any other failure here means the cap is "+
			"still whatever the transport defaults to and this assertion is passing by accident; got: %v", err)
	assert.Nil(t, result, "an over-sized response must yield no metadata")
}

// TestUnfurlService_PreservesTheConfiguredTimeout guards the setting the shared
// client would otherwise swallow.
//
// NewSSRFSafeHTTPClient returns a client with its own 15s ceiling. This service's
// own default is 10s, and cmd/server passes the same value explicitly through
// unfurl.WithTimeout. Adopting the shared client without restoring the caller's
// value LOOSENS every unfurl by five seconds — a change nobody asked for,
// arriving as part of an SSRF fix, on the path a post write blocks behind.
// blobs.NewBlobService and imageproxy.NewPDSFetcher both restore their own for
// the same reason.
func TestUnfurlService_PreservesTheConfiguredTimeout(t *testing.T) {
	t.Parallel()

	const configured = 27 * time.Second

	client := serviceHTTPClient(t, WithTimeout(configured))

	assert.Equalf(t, configured, client.Timeout,
		"the service's client runs on a %v timeout instead of the configured %v. The shared SSRF client "+
			"ships a 15s ceiling of its own, so a call site that adopts it without re-applying its own "+
			"value hands operators a setting that no longer does anything",
		client.Timeout, configured)
}
