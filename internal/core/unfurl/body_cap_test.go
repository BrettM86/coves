package unfurl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// The byte cap at the ONLY site in this tree that returns a response body to its
// caller.
//
// # THE DEFECT THESE TESTS WERE WRITTEN AGAINST
//
// NewService told the transport maxUnfurlBodyBytes, and both HTML read paths
// then wrapped the body in io.LimitReader(resp.Body, maxUnfurlBodyBytes) — the
// SAME number. A LimitReader stops at exactly its allowance, so cappedBody was
// never asked for the byte past the cap that IS its overrun signal. An over-cap
// page therefore arrived TRUNCATED, with no error, and went straight into
// parseOpenGraph and html.Parse — both of which are error-tolerant by design and
// will happily produce a document from half a file. What came back was an
// og:title and og:description that read as a complete page, cached for 24 hours
// and served into a post.
//
// imageproxy/fetcher.go and posts' newGuardedRematerializeBlobClient both set
// their transport cap to their own limit PLUS ONE for exactly this reason, and
// both say so in a comment. Unfurl was the one converted site that did not — and
// it is the one where the consequence is content rather than a failed fetch.
//
// # WHY THE FIXTURES DECLARE NO CONTENT-LENGTH
//
// The transport has a second, earlier control: it refuses a response whose
// ANNOUNCED length exceeds the cap, before a byte of body is read. A fixture
// that sets Content-Length is stopped there and proves nothing about the read
// path — the defect above would look fixed. A host that wants its body read is
// simply chunked, which costs an attacker nothing and is what these fixtures do.
//
// # WHY THE PAGES ARE REALLY 10 MB
//
// The cap is a package constant that no seam parameterises, so shrinking it for
// a test would mean testing a number production does not run. Ten megabytes over
// loopback is cheap enough to pay for testing the real one.

// overCapPage builds an HTML page of exactly size bytes whose og: tags sit in
// the first few hundred, so a truncated read still yields a complete-looking
// result. That ordering is the point: it is what makes the silent truncation
// LOOK like a successful unfurl rather than like a parse failure.
func overCapPage(t *testing.T, size int) string {
	t.Helper()

	// The kagiproxy.com <img> is what fetchKagiKite needs to return a result at
	// all; without it that path fails with "no image found" and would report
	// green for a reason that has nothing to do with the cap.
	head := `<!DOCTYPE html><html><head>` +
		`<meta property="og:title" content="` + leakedTitle + `" />` +
		`<meta property="og:description" content="cluster credentials rotate at 04:00 UTC" />` +
		`<meta property="og:image" content="https://kagiproxy.com/img/internal-topology.png" />` +
		`</head><body><img src="https://kagiproxy.com/img/internal-topology.png" alt="topology" /><p>`
	tail := `</p></body></html>`

	padding := size - len(head) - len(tail)
	require.Positivef(t, padding, "the fixture must be larger than its own markup (%d bytes)", len(head)+len(tail))

	page := head + strings.Repeat("a", padding) + tail
	require.Lenf(t, page, size, "the fixture must be EXACTLY %d bytes, not near it", size)
	return page
}

// chunkedTarget serves body without declaring its length, so the transport's
// announced-length refusal is out of the picture and what is exercised is the
// streamed read.
func chunkedTarget(t *testing.T, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Deliberately no Content-Length. Flushing after the header commits the
		// response to chunked encoding, so net/http cannot infer one from a
		// buffered body.
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// htmlFetch is one of the two paths that reads a body into a document. The
// oEmbed path is excluded because it decodes JSON: json.Decode over a truncated
// document FAILS, so that path already refuses rather than truncating.
type htmlFetch struct {
	name  string
	fetch func(ctx context.Context, url string, client *http.Client) (*UnfurlResult, error)
}

func htmlFetchPaths() []htmlFetch {
	return []htmlFetch{
		{
			name: "opengraph",
			fetch: func(ctx context.Context, url string, client *http.Client) (*UnfurlResult, error) {
				return fetchOpenGraph(ctx, url, client, "CovesBot/1.0")
			},
		},
		{
			name: "kagi",
			fetch: func(ctx context.Context, url string, client *http.Client) (*UnfurlResult, error) {
				return fetchKagiKite(ctx, url, client, "CovesBot/1.0")
			},
		},
	}
}

// TestUnfurl_AnOverCapPageIsRefusedRatherThanParsedAsComplete is the binding
// assertion.
//
// One byte past the cap is the tightest fixture that can distinguish "refused"
// from "truncated and parsed": under the defect it is silently clipped back to
// the cap, the og: tags at the top survive, and a caller gets a result it has no
// way to know is short.
func TestUnfurl_AnOverCapPageIsRefusedRatherThanParsedAsComplete(t *testing.T) {
	t.Parallel()

	page := overCapPage(t, maxUnfurlBodyBytes+1)
	server := chunkedTarget(t, page)

	for _, path := range htmlFetchPaths() {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()

			result, err := path.fetch(context.Background(), server.URL, hatchOpenClient(t, 30*time.Second))

			require.Errorf(t, err,
				"the %s path accepted a %d-byte page through a %d-byte cap and returned a result. The "+
					"transport cap equals the io.LimitReader's allowance, so the reader stops at exactly "+
					"the cap and cappedBody is never asked for the byte whose EXISTENCE is the overrun "+
					"signal — the page is clipped, handed to an error-tolerant parser, and comes back "+
					"looking whole. This is the one fetch site in the tree that returns the response body "+
					"to its caller, so what a truncated read produces is a truncated og:title cached for "+
					"24 hours and served into a post",
				path.name, len(page), maxUnfurlBodyBytes)

			assert.Nilf(t, result,
				"the %s path returned a result ALONGSIDE the error. An enrichment path that logs the "+
					"error and carries on — which is what unfurl is — then caches the truncated document "+
					"anyway", path.name)

			// ErrPageTooLarge and not ErrResponseTooLarge, because WHICH of the
			// two caps fired is the fence around the transport's +1. A transport
			// told exactly maxUnfurlBodyBytes also refuses this page — from the
			// wrong layer, clipping readCappedBody's probing byte and making its
			// own overrun branch unreachable in production. The page is refused
			// either way, so nothing but the sentinel can tell the two apart.
			assert.ErrorIsf(t, err, ErrPageTooLarge,
				"the %s path refused the over-cap page, but not through this site's own probe. "+
					"readCappedBody reads maxUnfurlBodyBytes+1 so the extra byte's existence is the "+
					"signal; if the transport cap does not leave room for that byte, the probe is clipped "+
					"and `len(data) > maxUnfurlBodyBytes` becomes dead code — which is the state the "+
					"truncation defect was found in; got: %v", path.name, err)
		})
	}
}

// TestUnfurl_APageAtTheCapStillArrivesWhole is the fence around the assertion
// above, and it is why the transport is told maxUnfurlBodyBytes+1 rather than
// having the read paths tightened to refuse earlier.
//
// A cap that refused at exactly its own limit would satisfy the over-cap test
// and quietly reject every page in the last byte of the allowance. The boundary
// belongs one byte higher than the largest page that must succeed.
func TestUnfurl_APageAtTheCapStillArrivesWhole(t *testing.T) {
	t.Parallel()

	page := overCapPage(t, maxUnfurlBodyBytes)
	server := chunkedTarget(t, page)

	for _, path := range htmlFetchPaths() {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()

			result, err := path.fetch(context.Background(), server.URL, hatchOpenClient(t, 30*time.Second))

			require.NoErrorf(t, err,
				"the %s path refused a page of EXACTLY the %d-byte cap. The limit is the largest body "+
					"that must be accepted, not the first one refused; got: %v",
				path.name, maxUnfurlBodyBytes, err)
			require.NotNilf(t, result, "the %s path returned no result for a page inside the cap", path.name)
			assert.Equalf(t, leakedTitle, result.Title,
				"the %s path parsed a page at the cap but lost its og:title, so the body did not survive "+
					"the read intact", path.name)
		})
	}
}

// TestUnfurl_AnAnnouncedOverCapLengthIsStillRefusedBeforeTheBody keeps the
// EARLIER half of the control from being lost while the read paths are fixed.
//
// The transport refuses a response whose declared Content-Length exceeds its cap
// before allocating anything, and raising that cap to make room for the probing
// byte is exactly the kind of edit that could slacken it to the shared 32 MiB
// default instead. A host that declares its length is the ordinary case; the
// chunked fixtures above deliberately dodge this check, so nothing else covers
// it.
func TestUnfurl_AnAnnouncedOverCapLengthIsStillRefusedBeforeTheBody(t *testing.T) {
	t.Parallel()

	// Comfortably above this site's 10 MB and comfortably below the shared
	// 32 MiB default, so it separates the two rather than sitting on an edge.
	const announced = maxUnfurlBodyBytes * 2
	require.Less(t, int64(announced), int64(covesoauth.DefaultMaxResponseBytes),
		"the fixture must declare a length the SHARED default would allow, or it proves nothing")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", fmt.Sprint(announced))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(secretInternalHTML))
	}))
	t.Cleanup(server.Close)

	for _, path := range htmlFetchPaths() {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()

			_, err := path.fetch(context.Background(), server.URL, hatchOpenClient(t, 30*time.Second))

			require.Errorf(t, err, "the %s path accepted a response declaring %d bytes", path.name, announced)
			assert.ErrorIsf(t, err, covesoauth.ErrResponseTooLarge,
				"the %s path did not refuse a declared length of %d on the ANNOUNCED-length check, so this "+
					"client is running on oauth.DefaultMaxResponseBytes (%d) rather than on a cap derived "+
					"from maxUnfurlBodyBytes (%d). The read paths bound what is ALLOCATED; this check is "+
					"the half that refuses before a byte of body arrives; got: %v",
				path.name, announced, covesoauth.DefaultMaxResponseBytes, maxUnfurlBodyBytes, err)
		})
	}
}
