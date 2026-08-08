package blobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FetchImageForURL's guard, against the two things a remote host controls that
// it currently does not defend: WHERE the request goes, and HOW MUCH comes back.
//
// # THE URL IS ATTACKER-INFLUENCED, TWICE
//
// This function fetches a thumbnail. The URL comes from unfurling a page, and a
// client chooses the page — so an attacker picks the origin. Worse, the origin
// then picks the thumbnail URL out of its own OpenGraph tags, and an HTTP
// redirect gives it a third bite. At no point does a human review the address
// the AppView is about to dial.
//
// # WHAT THAT MEANS FOR SSRF
//
// The AppView runs inside the same network as its Postgres, its PDS, its
// Jetstream and — in production — a cloud metadata endpoint at 169.254.169.254
// that answers credential requests to anything that can reach it. A default
// http.Client dials all of them. So "post a link whose page advertises
// http://169.254.169.254/latest/meta-data/iam/security-credentials/ as its
// preview image" is a request the AppView makes on the attacker's behalf, from
// inside the perimeter.
//
// The image never comes back to them directly — the response has to survive the
// Content-Type allowlist to become a blob — but the ORACLE does: response
// timing and the distinction between "connection refused", "unsupported MIME
// type" and "HTTP 403" map the internal network one URL at a time. And a
// metadata endpoint that answers `text/plain` still had the request made to it.
//
// The tree already solved this. internal/atproto/oauth's NewSSRFSafeHTTPClient
// resolves the host, refuses private/loopback/link-local addresses, and dials
// only the address it vetted (closing the check-then-dial window a naive guard
// leaves open). blueskypost already routes its attacker-influenced fetch through
// it. This is the same gap at a second call site, tracked since task 5.
//
// # WHY THE ALLOWANCE IS CONSTRUCTION STATE, NOT AN ENV READ
//
// Every honest test of a remote fetch serves it from httptest, which listens on
// loopback — precisely what the guard blocks. So the allowance has to be
// injectable. It must NOT be read from the process environment inside the fetch:
// Go's testing package refuses t.Setenv alongside t.Parallel, so an env read
// would make the guarded branch untestable in parallel and the whole package
// serial. blueskypost hit this exact wall and resolved it with a struct field
// (blueskyAPI.allowPrivateHost) set only by its own tests, with production
// deriving the value once at construction from config. The same shape belongs
// here, which is why these tests construct the service both ways.

// countingOrigin is a remote host that records what it was asked for and how
// much of the body it managed to send before the client stopped reading.
type countingOrigin struct {
	server *httptest.Server

	requests atomic.Int64
	written  atomic.Int64
}

// newEndlessOrigin serves a Content-Type the allowlist accepts and then writes
// until the client goes away — the shape of an origin that lies about, or simply
// omits, its Content-Length.
func newEndlessOrigin(t *testing.T, total int) *countingOrigin {
	t.Helper()

	origin := &countingOrigin{}
	chunk := make([]byte, 1<<20) // 1MB
	origin.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		origin.requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		for sent := 0; sent < total; sent += len(chunk) {
			n, err := w.Write(chunk)
			origin.written.Add(int64(n))
			if err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(origin.server.Close)
	return origin
}

func TestFetchImageForURL_RefusesAPrivateAddressBeforeDialling(t *testing.T) {
	t.Parallel()

	// httptest listens on loopback, which IS the private range the guard exists
	// to refuse — so the server doubles as the assertion: a guarded fetch must
	// leave its request counter at zero. Checking the counter rather than only
	// the error is the point. A guard that dialled, fetched, and then rejected
	// the response would return an error too, and would have performed exactly
	// the internal request an SSRF guard exists to prevent.
	origin := newEndlessOrigin(t, 1<<20)

	service := NewBlobService(origin.server.URL)

	_, _, err := service.FetchImageForURL(context.Background(), origin.server.URL+"/thumb.png")
	require.Errorf(t, err, "a thumbnail URL pointing at a private address must be refused: the AppView "+
		"shares a network with its database, its PDS and a cloud metadata endpoint, and the URL is "+
		"chosen by whoever controls the page being unfurled")

	assert.Zerof(t, origin.requests.Load(),
		"the guarded fetch reached the private address anyway — the request was MADE, which is the "+
			"whole of the SSRF, and rejecting the response afterwards prevents none of it")
}

func TestFetchImageForURL_AllowsAPrivateAddressWhenExplicitlyPermitted(t *testing.T) {
	t.Parallel()

	// The escape hatch, and it is not a nicety: every integration test in the
	// tree serves its fixtures from loopback, so a guard with no injectable
	// allowance would take the whole T1 blob suite with it. Production
	// constructs the service without this; only tests and a dev environment
	// turn it on.
	origin := newEndlessOrigin(t, 1<<10)

	service := NewBlobService(origin.server.URL, WithPrivateHostsAllowed())

	data, mimeType, err := service.FetchImageForURL(context.Background(), origin.server.URL+"/thumb.png")
	require.NoError(t, err)
	assert.Equal(t, "image/png", mimeType)
	assert.NotEmpty(t, data)
	assert.Equal(t, int64(1), origin.requests.Load())
}

func TestFetchImageForURL_StopsReadingAtTheCapInsteadOfBufferingEverything(t *testing.T) {
	t.Parallel()

	// THE CAP IS CHECKED AFTER io.ReadAll, WHICH IS THE SAME AS NOT HAVING ONE
	// for the failure it is supposed to prevent. `len(data) > maxSize` is a
	// perfectly correct test of a slice that is already in memory: the whole
	// body has been read, allocated and held before anyone asks how big it is.
	// A hostile origin advertising image/png and streaming without a
	// Content-Length gets the AppView to buffer whatever it feels like sending,
	// once per unfurled link, and the 6MB "limit" is only the point at which the
	// result is thrown away.
	//
	// io.LimitReader(body, max+1) makes the cap a READ bound: at most one byte
	// past the limit ever enters memory, and that extra byte is what
	// distinguishes "exactly at the cap" from "over it".
	const total = 64 << 20 // 64MB, an order of magnitude past the 6MB cap
	origin := newEndlessOrigin(t, total)

	service := NewBlobService(origin.server.URL, WithPrivateHostsAllowed())

	_, _, err := service.FetchImageForURL(context.Background(), origin.server.URL+"/enormous.png")
	require.Error(t, err, "a body past the cap must be refused")

	// The origin got nowhere near sending it all. The threshold is deliberately
	// loose — kernel and http.Server buffering let the origin run some way ahead
	// of a client that has stopped reading — but it is nowhere near 64MB, which
	// is what a full io.ReadAll would have consumed.
	written := origin.written.Load()
	assert.Lessf(t, written, int64(total/2),
		"the origin managed to send %d bytes of a %d-byte body, so the client was still reading long "+
			"past the 6MB cap: the limit is applied to a buffer that has already been filled, which "+
			"prevents the allocation it exists to prevent not at all", written, total)
}

func TestFetchImageForURL_StillRefusesTheThingsItAlreadyRefused(t *testing.T) {
	t.Parallel()

	// The guard's existing behaviour, re-pinned here because both fixes rewrite
	// this function and neither may cost it. A rewrite that routed through the
	// SSRF client and dropped the MIME allowlist would pass every assertion
	// above.
	t.Run("a Content-Type outside the image allowlist", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>not an image</html>"))
		}))
		t.Cleanup(server.Close)

		_, _, err := NewBlobService(server.URL, WithPrivateHostsAllowed()).
			FetchImageForURL(context.Background(), server.URL+"/page.html")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported MIME type")
	})

	t.Run("a response with no Content-Type at all", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = nil
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)

		_, _, err := NewBlobService(server.URL, WithPrivateHostsAllowed()).
			FetchImageForURL(context.Background(), server.URL+"/mystery")
		require.Error(t, err)
	})

	t.Run("an empty URL", func(t *testing.T) {
		t.Parallel()

		_, _, err := NewBlobService("http://example.invalid").
			FetchImageForURL(context.Background(), "")
		require.Error(t, err)
	})
}
