package imageproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// The image proxy's PDS fetch, against the destination a stranger chose.
//
// # WHY THIS IS THE WORST OF THE NINE SITES
//
// There is no authentication anywhere on the path. The route is public, guarded
// only by a global 100/min/IP limiter, and the address it dials comes from the
// `serviceEndpoint` of a DID document — a did:plc anyone can mint, for free,
// naming any endpoint they like. So the attacker supplies the destination
// directly and pays nothing to do it.
//
// What comes back is a clean port scanner. handler.go:148 maps the fetch errors
// onto three distinct statuses — 404 when the port answered, 502 when the
// connection was refused, 504 when it was filtered — so a stranger reads the
// AppView's internal network topology one request at a time, and the AppView
// shares that network with its Postgres, its PDS, its Jetstream and, in
// production, a metadata endpoint at 169.254.169.254 that hands credentials to
// anything that can reach it.
//
// # WHY THESE TESTS DRIVE THE FETCHER AND NOT THE SERVICE
//
// service.go:85 checks the disk cache BEFORE calling fetcher.Fetch. A guard test
// written against the service that happened to hit a warm cache would return
// bytes, assert nothing, and pass — having never once evaluated the code it
// claims to test. The guard cases below therefore drive PDSFetcher directly.
// The one service-level case at the bottom exists to prove the assembled path is
// closed too, and it starts by asserting its own cache is cold.
//
// # WHY REACHABILITY IS ASSERTED AND NOT ONLY THE ERROR
//
// This project has already been bitten here. Mutation testing
// produced an implementation that classified every address correctly, emitted a
// byte-identical error message, and refused the request AFTER delivering it.
// Every error-message assertion in the suite passed against it. For a
// destination a stranger named, the packet leaving IS the SSRF — whatever error
// comes back afterwards — so each case below stands up a real listener and
// asserts its handler was never invoked.

// countingPDS is a listener that answers like a PDS and records whether anything
// ever reached it. It listens on loopback, which is exactly the address class
// the guard exists to refuse, so the counter doubles as the assertion.
type countingPDS struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newCountingPDS(t *testing.T) *countingPDS {
	t.Helper()

	pds := &countingPDS{}
	pds.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pds.requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("blob bytes"))
	}))
	t.Cleanup(pds.server.Close)
	return pds
}

// TestPDSFetcher_Fetch_RefusesAPrivateAddressWithoutReachingIt is the binding
// contract for image-proxy egress.
//
// The listener is real and the fetcher is the production one — no options, the
// shape wiring builds outside dev. A guarded fetcher must leave the request
// counter at zero.
func TestPDSFetcher_Fetch_RefusesAPrivateAddressWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	fetcher := NewPDSFetcher(5*time.Second, 10)

	_, err := fetcher.Fetch(context.Background(), pds.server.URL, "did:plc:test123", "bafyreicid123")

	require.Errorf(t, err,
		"a PDS URL pointing at a loopback address was fetched successfully. The endpoint comes from a "+
			"DID document anyone can mint for free, and this route needs no credential at all, so this "+
			"is a stranger making the AppView dial its own internal network")

	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times. The refusal happened, but it happened AFTER the request was "+
			"delivered — which prevents none of the SSRF and is precisely the implementation that passed "+
			"every error-message assertion during mutation testing",
		pds.requests.Load())
}

// TestPDSFetcher_Fetch_BlockedIsItsOwnSentinelAndNotTheOracle pins the internal
// half of the sentinel contract.
//
// A refusal by the guard is not a fetch that failed, and a caller has to be able
// to tell them apart — for logging, for alerting, and so that a future retry or
// circuit-breaker treats "the network hiccuped" and "we refused to make this
// request" differently. That is what ErrPDSBlocked is for.
//
// The two NotErrorIs assertions matter more than the positive one. ErrPDSNotFound
// maps to 404 and ErrPDSTimeout maps to 504 at handler.go:148, so a refusal that
// matched either would hand back exactly the port-scan oracle this guard exists
// to remove — and it would do so while the "is it blocked" assertion above stayed
// green.
func TestPDSFetcher_Fetch_BlockedIsItsOwnSentinelAndNotTheOracle(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	fetcher := NewPDSFetcher(5*time.Second, 10)

	_, err := fetcher.Fetch(context.Background(), pds.server.URL, "did:plc:test123", "bafyreicid123")
	require.Error(t, err, "the guarded fetcher must refuse a loopback PDS URL")

	assert.ErrorIsf(t, err, ErrPDSBlocked,
		"the refusal must carry its own identity, matchable with errors.Is rather than by reading a "+
			"message: a caller that cannot distinguish a security refusal from a network failure logs "+
			"both the same way and retries both the same way; got: %v", err)

	assert.NotErrorIsf(t, err, ErrPDSNotFound,
		"a blocked address classified as ErrPDSNotFound, which handler.go:148 serves as 404. That is "+
			"half the port-scan oracle: 404 tells the stranger who named this address that something "+
			"answered on it; got: %v", err)

	assert.NotErrorIsf(t, err, ErrPDSTimeout,
		"a blocked address classified as ErrPDSTimeout, which handler.go:148 serves as 504 — the "+
			"'filtered' half of the port-scan oracle, distinguishable from 502 by anyone probing; got: %v", err)

	assert.NotErrorIsf(t, err, ErrPDSFetchFailed,
		"the guard refusal also matches ErrPDSFetchFailed, so the two are indistinguishable in-process. "+
			"They must map to the SAME status externally — see the handler test — but a single sentinel "+
			"covering both means no caller and no log line can ever tell a refused destination from an "+
			"unreachable one; got: %v", err)
}

// TestPDSFetcher_Fetch_AllowsAPrivateAddressWhenExplicitlyPermitted is the dev
// hatch, and it is not a nicety.
//
// Every honest test of this fetch serves its PDS from httptest, which listens on
// loopback — proxy_serving_test.go and avatar_serving_test.go both do — and a
// local dev stack runs its PDS on the developer's own machine. Without an
// injectable allowance the guard takes all of that with it.
func TestPDSFetcher_Fetch_AllowsAPrivateAddressWhenExplicitlyPermitted(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	fetcher := NewPDSFetcher(5*time.Second, 10, WithPrivateHostsAllowed())

	data, err := fetcher.Fetch(context.Background(), pds.server.URL, "did:plc:test123", "bafyreicid123")

	require.NoErrorf(t, err,
		"the hatch is what every fixture in this tree and every dev stack depends on: a fetcher built "+
			"with WithPrivateHostsAllowed() must reach a loopback PDS")
	assert.Equal(t, "blob bytes", string(data), "the blob bytes must come back unchanged through the guarded client")
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once", pds.requests.Load())
}

// TestPDSFetcher_Fetch_RefusesSmuggledURLComponents covers what Fetch does NOT
// currently touch.
//
// Fetch overwrites `.Path` and `.RawQuery` and nothing else, so every other
// component of a caller-supplied URL survives into the request: `.User`,
// `.Fragment`, `.Opaque` and `.Scheme`. A `serviceEndpoint` of
// `https://evil@internal-host` dials internal-host carrying userinfo; an
// `.Opaque` form bypasses the path rewrite entirely, since url.URL.String()
// prefers Opaque over Path; and `file://` or `gopher://` is not a fetch this
// service has any business making.
//
// # EVERY ROW RUNS WITH THE HATCH OPEN, DELIBERATELY
//
// If these ran on a guarded fetcher, the loopback address alone would refuse
// them and every row would pass without the URL ever being inspected. Opening
// the hatch removes the address guard from the picture, so the only thing left
// that can refuse the request is the URL's own shape — which is what these rows
// are about. The row above proves a plain URL at this same listener succeeds
// under the same options, so a refusal here is attributable to the smuggled
// component and to nothing else.
func TestPDSFetcher_Fetch_RefusesSmuggledURLComponents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// pdsURL is built from the listener's address where the case needs a
		// reachable destination, and is a fixed string where it does not.
		pdsURL func(base string) string
		why    string
	}{
		{
			name:   "userinfo in the authority",
			pdsURL: func(base string) string { return "http://evil@" + trimScheme(base) },
			why: "a DID document's serviceEndpoint of https://evil@internal-host dials internal-host " +
				"and carries credentials to it; url.Parse puts `evil` in .User, which Fetch never clears",
		},
		{
			name:   "a fragment on the endpoint",
			pdsURL: func(base string) string { return base + "/#fragment" },
			why: "Fetch overwrites .Path and .RawQuery but leaves .Fragment, so a component of the " +
				"attacker's string survives the rewrite that is supposed to replace the whole request target",
		},
		{
			name:   "an opaque URL",
			pdsURL: func(string) string { return "http:internal-host/v1/secrets" },
			why: "url.URL.String() emits .Opaque INSTEAD of .Path when it is set, so the path rewrite " +
				"Fetch performs is discarded and the attacker's own path is what gets requested",
		},
		{
			name:   "the file scheme",
			pdsURL: func(string) string { return "file:///etc/passwd" },
			why: "there must be a positive http/https allowlist. The stdlib refuses this scheme today " +
				"by accident of what http.Transport registers, which is a different mechanism that a " +
				"future transport option could quietly change",
		},
		{
			name:   "the gopher scheme",
			pdsURL: func(string) string { return "gopher://internal-host:70/" },
			why: "gopher is the classic SSRF protocol-smuggling scheme, and the same positive allowlist " +
				"is what refuses it rather than a registration detail of the stdlib transport",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pds := newCountingPDS(t)

			// The hatch is open: the address guard is out of the way, so the URL's
			// shape is the only thing that can refuse this.
			fetcher := NewPDSFetcher(5*time.Second, 10, WithPrivateHostsAllowed())

			_, err := fetcher.Fetch(context.Background(), tt.pdsURL(pds.server.URL), "did:plc:test123", "bafyreicid123")

			require.Errorf(t, err,
				"the PDS URL was accepted with the hatch open, so nothing inspected its shape — %s", tt.why)

			assert.ErrorIsf(t, err, ErrPDSBlocked,
				"the refusal must be the guard's own sentinel and not an incidental failure from "+
					"somewhere in net/http. An error that arrives by accident is one a dependency "+
					"upgrade can take away, and it maps to a status nobody chose — %s; got: %v", tt.why, err)

			assert.Zerof(t, pds.requests.Load(),
				"the listener was reached %d times. A URL this malformed must be refused before a "+
					"packet leaves the process — %s", pds.requests.Load(), tt.why)
		})
	}
}

// trimScheme strips the leading http:// from an httptest URL so a case can
// rebuild the authority with something smuggled in front of it.
func trimScheme(url string) string {
	const prefix = "http://"
	if len(url) > len(prefix) && url[:len(prefix)] == prefix {
		return url[len(prefix):]
	}
	return url
}

// TestPDSFetcher_PreservesTheConfiguredTimeout guards the setting the shared
// client would otherwise swallow.
//
// NewSSRFSafeHTTPClient returns a client with its own 15s ceiling, and
// IMAGE_PROXY_FETCH_TIMEOUT_SECONDS is an operator setting that defaults to 30.
// Adopting the shared client without restoring the caller's own value silently
// re-times every image fetch in production — a change nobody asked for, arriving
// as part of an SSRF fix. blobs.NewBlobService raises its own back to 30s for
// exactly this reason.
func TestPDSFetcher_PreservesTheConfiguredTimeout(t *testing.T) {
	t.Parallel()

	const configured = 27 * time.Second

	fetcher := NewPDSFetcher(configured, 10)

	require.NotNil(t, fetcher.client, "the fetcher must hold an HTTP client")
	assert.Equalf(t, configured, fetcher.client.Timeout,
		"the fetcher's client runs on a %v timeout instead of the configured %v. The shared SSRF client "+
			"ships a 15s ceiling of its own, so a call site that adopts it without re-applying its own "+
			"value hands operators a setting that no longer does anything",
		fetcher.client.Timeout, configured)
}

// TestPDSFetcher_Fetch_HonoursASizeLimitAboveTheTransportDefault covers the
// configuration clamp regression.
//
// The shared transport now caps response bodies at DefaultMaxResponseBytes
// (32 MiB) unless a caller raises it. IMAGE_PROXY_MAX_SOURCE_SIZE_MB is
// operator-configurable and defaults to 10, so nothing breaks today — and that
// is the danger. An operator who raises it to 64 gets a fetcher that still
// refuses at 32 MiB, with the failure surfacing as an image that will not load
// and an error that blames the remote host. A config value that silently does
// not take effect is worse than one that is rejected.
//
// So the conversion has to pass WithMaxResponseBytes explicitly from the
// configured size rather than inheriting the package default. The body below is
// past the transport's default and under this fetcher's own limit, which is the
// only window where the two can be told apart.
func TestPDSFetcher_Fetch_HonoursASizeLimitAboveTheTransportDefault(t *testing.T) {
	t.Parallel()

	// One KiB past the transport's default, so a fetcher that inherited the
	// default fails and a fetcher carrying its own 64 MiB limit does not.
	const bodySize = covesoauth.DefaultMaxResponseBytes + 1024

	var requests atomic.Int64
	chunk := make([]byte, 1<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		for sent := 0; sent < bodySize; {
			end := len(chunk)
			if remaining := bodySize - sent; remaining < end {
				end = remaining
			}
			n, err := w.Write(chunk[:end])
			sent += n
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	// 64 MiB configured — comfortably above the transport's 32 MiB default and
	// above the body, so the only thing that can refuse this response is a cap
	// the conversion failed to raise.
	fetcher := NewPDSFetcher(30*time.Second, 64, WithPrivateHostsAllowed())

	data, err := fetcher.Fetch(context.Background(), server.URL, "did:plc:test123", "bafyreicid123")

	require.NoErrorf(t, err,
		"a %d-byte blob was refused by a fetcher configured for 64 MiB. The limit that stopped it is "+
			"the shared transport's 32 MiB default, applied from another package — so an operator who "+
			"raises IMAGE_PROXY_MAX_SOURCE_SIZE_MB above 32 gets no error, no warning, and a setting "+
			"that quietly does nothing; got: %v", bodySize, err)
	assert.Lenf(t, data, bodySize,
		"the blob came back truncated: %d bytes of %d. A short body that arrives without an error is "+
			"worse than a refusal — the processor would encode whatever partial image it got", len(data), bodySize)
	assert.Equalf(t, int64(1), requests.Load(), "the listener was reached %d times rather than once", requests.Load())
}

// TestPDSFetcher_Fetch_ImageTooLarge_IsStillTheFetchersOwnLimit exists because
// the conversion moved a limit without moving an assertion.
//
// The transport now carries a cap of its own, set to maxSizeBytes+1, and it
// refuses a DECLARED Content-Length above that before Fetch ever sees the
// response. TestPDSFetcher_Fetch_ImageTooLarge_ContentLength declares 2 MiB
// against a 1 MiB fetcher, so it is now satisfied one layer down: its assertion
// (ErrImageTooLarge) still holds, but the branch it used to cover —
// `resp.ContentLength > f.maxSizeBytes` in Fetch — no longer runs for that
// input. Nobody would notice, because the test is still green.
//
// That branch is now reachable through a ONE-BYTE WINDOW: a declared length must
// exceed maxSizeBytes to fail it, and must not exceed maxSizeBytes+1 or the
// transport takes it first. maxSizeBytes+1 is the only value that lands there,
// and it is what the first case below declares.
//
// The mechanism is identified by MESSAGE rather than by errors.Is, which is not
// a stylistic choice: fetcher.go:184 wraps oauth.ErrResponseTooLarge with %v
// rather than %w, so the transport's identity is not in the chain and both
// layers arrive as a bare ErrImageTooLarge. That is worth knowing on its own —
// nothing downstream can tell "the image is too big for us" from "the transport
// refused to hand it over" — and until it changes, the rendered sentence is the
// only thing that distinguishes them.
func TestPDSFetcher_Fetch_ImageTooLarge_IsStillTheFetchersOwnLimit(t *testing.T) {
	t.Parallel()

	// One MiB, matching the fetcher below, spelled out because both cases are
	// positioned relative to it by single bytes.
	const maxSizeBytes = 1 << 20

	t.Run("a declared length in the one-byte window the transport leaves", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(maxSizeBytes+1))
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)

		fetcher := NewPDSFetcher(5*time.Second, 1, WithPrivateHostsAllowed())

		_, err := fetcher.Fetch(context.Background(), server.URL, "did:plc:test123", "bafyreicid123")

		require.ErrorIsf(t, err, ErrImageTooLarge,
			"a declared length of %d against a %d-byte limit must be refused; got: %v",
			maxSizeBytes+1, maxSizeBytes, err)
		assert.Containsf(t, err.Error(), "content length",
			"the refusal came from the transport's cap rather than from Fetch's own Content-Length "+
				"check, which means that branch is now dead code and no test covers it. This is the "+
				"only input that can reach it — one byte lower and it is within the limit, one byte "+
				"higher and the transport refuses first; got: %v", err)
	})

	t.Run("a streamed body with no declared length", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, 2*maxSizeBytes))
		}))
		t.Cleanup(server.Close)

		fetcher := NewPDSFetcher(5*time.Second, 1, WithPrivateHostsAllowed())

		_, err := fetcher.Fetch(context.Background(), server.URL, "did:plc:test123", "bafyreicid123")

		require.ErrorIsf(t, err, ErrImageTooLarge,
			"a %d-byte body against a %d-byte limit must be refused; got: %v", 2*maxSizeBytes, maxSizeBytes, err)
		assert.Containsf(t, err.Error(), "response body exceeds maximum",
			"this is the case the transport's cap must NOT shadow. Fetch reads maxSizeBytes+1 through "+
				"an io.LimitReader precisely so an oversized body is detected rather than truncated, and "+
				"the transport's allowance is one byte above that so the probing read survives. A cap set "+
				"to maxSizeBytes exactly would turn this into a generic read failure — the same refusal, "+
				"reported as something the remote host did wrong; got: %v", err)
	})
}

// TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed is the
// single most important assertion for this call site.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` — the hermetic merge gate,
// T0+T1+T2 — runs the PERMISSIVE branch here and at every other site holding
// such a boolean. A green merge gate therefore proves nothing whatsoever about
// whether the image proxy is guarded in production. This function is the one
// place in the repository where the production branch is ever evaluated, which
// is why the gate must be a pure function and not an `if cfg.IsDevEnv` in
// cmd/server/wiring.go.
//
// The claim is not "the options returned are safe". It is that there are NONE:
// length zero, nothing applied, the constructor's own defaults left untouched.
// An edit that appends a diagnostic option, or returns a one-element slice
// holding a no-op "explicitly deny" closure, keeps every behavioural test green
// while moving the untested branch from "provably applies nothing" to "applies
// something believed harmless". If this assertion is ever in the way, the answer
// is not to relax it.
func TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivateHostOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivateHostOptions(false) returned %d option(s). The production branch — the one IS_DEV_ENV=true "+
			"keeps `make ci` from ever evaluating — must contribute nothing at all, so that what "+
			"production gets is exactly the constructor's own defaults", len(opts))
}

// TestPrivateHostOptions_DisallowedFetcherIsGuarded is the behavioural half of
// the assertion above: zero options has to also MEAN a guarded fetcher.
//
// The length check alone would still pass if the constructor's own default ever
// regressed to permissive — the helper would correctly be returning nothing,
// onto a base that no longer refuses anything.
func TestPrivateHostOptions_DisallowedFetcherIsGuarded(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	fetcher := NewPDSFetcher(5*time.Second, 10, PrivateHostOptions(false)...)

	_, err := fetcher.Fetch(context.Background(), pds.server.URL, "did:plc:test123", "bafyreicid123")

	require.Error(t, err,
		"a fetcher built from PrivateHostOptions(false) reached a loopback PDS. This is the branch "+
			"production runs and CI never does")
	assert.ErrorIsf(t, err, ErrPDSBlocked,
		"the refusal must be the guard's, matchable by identity — a request that failed for some other "+
			"reason is not the same control and would not hold in production; got: %v", err)
	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times, so the packet left the process", pds.requests.Load())
}

// TestPrivateHostOptions_AllowedFetcherReachesTheListener pins the other
// direction through observed behaviour rather than through the shape of the
// slice.
//
// A length check here would be worthless: a helper returning the wrong
// single-element slice satisfies it while leaving every fixture in this tree
// unable to reach loopback.
func TestPrivateHostOptions_AllowedFetcherReachesTheListener(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	fetcher := NewPDSFetcher(5*time.Second, 10, PrivateHostOptions(true)...)

	data, err := fetcher.Fetch(context.Background(), pds.server.URL, "did:plc:test123", "bafyreicid123")

	require.NoErrorf(t, err,
		"a fetcher built from PrivateHostOptions(true) was refused. The permissive branch is what every "+
			"developer and every fixture in this tree runs, so a helper that returns the wrong option — "+
			"or none — breaks local development everywhere at once; got: %v", err)
	assert.Equal(t, "blob bytes", string(data), "the blob must come back through the permissive fetcher")
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once", pds.requests.Load())
}

// TestImageProxyService_GetImage_RefusesAPrivateAddressOnAColdCache proves the
// assembled path is closed, not just the fetcher in isolation.
//
// # THE CACHE TRAP
//
// service.go:85 reads the disk cache before it calls fetcher.Fetch. A test that
// inherited a warm cache — a shared directory, a fixture written by a sibling
// test, a second call in the same function — returns bytes from disk and asserts
// nothing about the guard. So this one starts by asserting its own cache is
// cold, in the same terms the service uses, and only then drives GetImage.
func TestImageProxyService_GetImage_RefusesAPrivateAddressOnAColdCache(t *testing.T) {
	t.Parallel()

	const (
		preset = "avatar"
		did    = "did:plc:test123"
		cid    = "bafyreicid123"
	)

	pds := newCountingPDS(t)

	cache, err := NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err, "creating the disk cache this test owns")

	// The cache is cold, asserted rather than assumed: everything below is
	// meaningless if service.go:85 can answer from disk.
	_, found, err := cache.Get(preset, did, cid)
	require.NoError(t, err, "reading a fresh cache must not error")
	require.False(t, found,
		"this test's cache already holds an entry for (%s, %s, %s), so GetImage would return it without "+
			"ever calling the fetcher and this whole case would pass without exercising the guard",
		preset, did, cid)

	service, err := NewService(cache, NewProcessor(), NewPDSFetcher(5*time.Second, 10), Config{
		Enabled:         true,
		CachePath:       t.TempDir(),
		CacheMaxGB:      1,
		FetchTimeout:    5 * time.Second,
		MaxSourceSizeMB: 10,
	})
	require.NoError(t, err, "creating the image proxy service")

	_, err = service.GetImage(context.Background(), preset, did, cid, pds.server.URL)

	require.Error(t, err,
		"the assembled image proxy fetched a blob from a loopback PDS URL. This is the shape of the "+
			"production request: a public route, no credential, and an endpoint taken from a DID "+
			"document a stranger minted")
	assert.ErrorIsf(t, err, ErrPDSBlocked,
		"the service must pass the guard's refusal through unchanged so the handler can map it; got: %v", err)
	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times through the service", pds.requests.Load())
}
