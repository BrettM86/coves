package unfurl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
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

// fixtureDeliveryAttempts bounds how many fresh fetches fetchDelivered makes
// when the fixture server's host, not the client, refuses the server's write.
const fixtureDeliveryAttempts = 3

// fixtureDelivery names which of the fixture server's writes decide whether a
// fetch is evidence about the read path.
type fixtureDelivery int

const (
	// bodyWritten judges the handler's write of the body. net/http writes the
	// chunk terminator after the handler returns, and a client that stops at
	// the cap and closes never reads it, so a failed terminator write says
	// nothing about that fetch.
	bodyWritten fixtureDelivery = iota
	// responseWritten judges every write of the response on the connection,
	// chunk terminator included. A client that reads the page to its end must
	// read the terminator, so a failed write anywhere in the response reaches
	// it as unexpected EOF.
	responseWritten
)

// writeFault, when it returns an error, makes the fixture connection refuse a
// write of size bytes after written bytes on the given attempt, the way a loaded
// host refuses a loopback write. Only TestUnfurl_FixtureRetriesOnlyItsHostsWriteFailures
// sets one.
type writeFault func(attempt int, written int64, size int) error

// fixtureWrites is what the fixture server's side of the connection recorded.
type fixtureWrites struct {
	connErr error      // the first failed write anywhere in the response
	bodyErr error      // the handler's write of the body
	mu      sync.Mutex // guards every field of fixtureWrites
	written int64
}

// failure is the write error that delivery judges.
func (w *fixtureWrites) failure(delivery fixtureDelivery) (written int64, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if delivery == bodyWritten {
		return w.written, w.bodyErr
	}
	return w.written, w.connErr
}

// fixtureConn is the fixture server's end of a connection. It records every
// write's outcome, so fetchDelivered can tell a write the server's host refused
// from one that failed because the client hung up.
type fixtureConn struct {
	net.Conn
	writes  *fixtureWrites
	fault   writeFault
	attempt int
}

func (c *fixtureConn) Write(p []byte) (int, error) {
	c.writes.mu.Lock()
	written := c.writes.written
	c.writes.mu.Unlock()

	var n int
	var err error
	if c.fault != nil {
		err = c.fault(c.attempt, written, len(p))
	}
	if err == nil {
		n, err = c.Conn.Write(p)
	}

	c.writes.mu.Lock()
	defer c.writes.mu.Unlock()
	c.writes.written += int64(n)
	if err != nil && c.writes.connErr == nil {
		c.writes.connErr = err
	}
	return n, err
}

// fixtureListener hands the fixture server a fixtureConn for each connection.
type fixtureListener struct {
	net.Listener
	writes  *fixtureWrites
	fault   writeFault
	attempt int
}

func (l fixtureListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &fixtureConn{Conn: conn, writes: l.writes, attempt: l.attempt, fault: l.fault}, nil
}

// chunkedTarget serves body without declaring its length, so the transport's
// announced-length refusal is out of the picture and what is exercised is the
// streamed read. The returned record is complete once server.Close returns.
func chunkedTarget(t *testing.T, body string, attempt int, fault writeFault) (*httptest.Server, *fixtureWrites) {
	t.Helper()

	writes := &fixtureWrites{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Deliberately no Content-Length. Flushing after the header commits the
		// response to chunked encoding, so net/http cannot infer one from a
		// buffered body.
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, err := w.Write([]byte(body))
		writes.mu.Lock()
		writes.bodyErr = err
		writes.mu.Unlock()
	}))
	server.Listener = fixtureListener{Listener: server.Listener, writes: writes, attempt: attempt, fault: fault}
	server.Start()
	t.Cleanup(server.Close)
	return server, writes
}

// fixtureFetch is what the fetch that fetchDelivered kept returned.
type fixtureFetch struct {
	result   *UnfurlResult
	err      error
	attempts int
}

// fetchDelivered fetches page through path from a fresh chunkedTarget, and
// fetches again only when the server's host refused the server's own write with
// ENOBUFS. A loaded macOS host does that to loopback writes mid-response; the
// client then reads "unexpected EOF", which the assertions cannot tell apart
// from a read path that clipped the page.
//
// Any other write failure (EPIPE, ECONNRESET, a closed connection) is what the
// server sees when the client hangs up, so that fetch goes to the assertions:
// retrying it would turn a read path that sometimes closes early into a pass.
// fetchDelivered returns an error only when ENOBUFS refused every attempt.
func fetchDelivered(t *testing.T, page string, path htmlFetch, delivery fixtureDelivery, fault writeFault) (fixtureFetch, error) {
	t.Helper()

	var refused error
	for attempt := 1; attempt <= fixtureDeliveryAttempts; attempt++ {
		server, writes := chunkedTarget(t, page, attempt, fault)
		result, err := path.fetch(context.Background(), server.URL, hatchOpenClient(t, 30*time.Second))
		// Close waits for every connection to close, so the server has made, and
		// writes has recorded, every write it was going to make.
		server.Close()

		written, failure := writes.failure(delivery)
		if !errors.Is(failure, syscall.ENOBUFS) {
			if failure != nil {
				t.Logf("attempt %d: the fixture server's write failed after %d bytes with %v, which is not "+
					"the server's host refusing it; the assertions judge this fetch",
					attempt, written, failure)
			}
			return fixtureFetch{result: result, err: err, attempts: attempt}, nil
		}
		refused = failure
		t.Logf("attempt %d: the fixture server's host refused its write with ENOBUFS after %d bytes, so this "+
			"fetch says nothing about the %s read path; client saw: %v", attempt, written, path.name, err)
	}
	return fixtureFetch{attempts: fixtureDeliveryAttempts}, fmt.Errorf(
		"the fixture server's host refused its writes with ENOBUFS on all %d attempts to deliver the %d-byte "+
			"page to the %s path: %w", fixtureDeliveryAttempts, len(page), path.name, refused)
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

	for _, path := range htmlFetchPaths() {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()

			fetched, deliveryErr := fetchDelivered(t, page, path, bodyWritten, nil)
			require.NoError(t, deliveryErr)
			result, err := fetched.result, fetched.err

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

	for _, path := range htmlFetchPaths() {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()

			fetched, deliveryErr := fetchDelivered(t, page, path, responseWritten, nil)
			require.NoError(t, deliveryErr)
			result, err := fetched.result, fetched.err

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

// TestUnfurl_FixtureRetriesOnlyItsHostsWriteFailures pins what fetchDelivered
// may throw away. A fetch is retried only when the fixture server's host refused
// its write (ENOBUFS); a write that failed because the client hung up is the
// read path's doing, so that fetch reaches the assertions on its first attempt.
// Faults are injected by the fixture connection, so every case is deterministic.
func TestUnfurl_FixtureRetriesOnlyItsHostsWriteFailures(t *testing.T) {
	t.Parallel()

	opengraph := htmlFetchPaths()[0]
	overCap := overCapPage(t, maxUnfurlBodyBytes+1)
	atCap := overCapPage(t, maxUnfurlBodyBytes)

	// inBody fails the write that carries the response past half an at-cap
	// page, which only the body's single large write does: the headers are a few
	// hundred bytes. atTerminator fails any write that starts after a whole page
	// has gone out, which is only the chunk terminator.
	inBody := func(errno syscall.Errno, onAttempt func(int) bool) writeFault {
		return func(attempt int, written int64, size int) error {
			if onAttempt(attempt) && written+int64(size) > int64(len(atCap))/2 {
				return &net.OpError{Op: "write", Net: "tcp", Err: errno}
			}
			return nil
		}
	}
	atTerminator := func(errno syscall.Errno, onAttempt func(int) bool) writeFault {
		return func(attempt int, written int64, _ int) error {
			if onAttempt(attempt) && written >= int64(len(atCap)) {
				return &net.OpError{Op: "write", Net: "tcp", Err: errno}
			}
			return nil
		}
	}
	firstAttempt := func(attempt int) bool { return attempt == 1 }
	everyAttempt := func(int) bool { return true }

	refused := func(t *testing.T, fetched fixtureFetch) {
		assert.ErrorIs(t, fetched.err, ErrPageTooLarge)
		assert.Nil(t, fetched.result)
	}
	whole := func(t *testing.T, fetched fixtureFetch) {
		require.NoError(t, fetched.err)
		require.NotNil(t, fetched.result)
		assert.Equal(t, leakedTitle, fetched.result.Title)
	}
	clipped := func(t *testing.T, fetched fixtureFetch) {
		assert.ErrorIs(t, fetched.err, io.ErrUnexpectedEOF,
			"the client must see the truncation the assertions would have judged")
		assert.Nil(t, fetched.result)
	}

	cases := []struct {
		fault        writeFault
		check        func(*testing.T, fixtureFetch)
		name         string
		page         string
		delivery     fixtureDelivery
		wantAttempts int
	}{
		{
			name: "over-cap body refused with ENOBUFS once is fetched again", page: overCap,
			delivery: bodyWritten, fault: inBody(syscall.ENOBUFS, firstAttempt), wantAttempts: 2, check: refused,
		},
		{
			name: "at-cap body refused with ENOBUFS once is fetched again", page: atCap,
			delivery: responseWritten, fault: inBody(syscall.ENOBUFS, firstAttempt), wantAttempts: 2, check: whole,
		},
		{
			name: "at-cap terminator refused with ENOBUFS once is fetched again", page: atCap,
			delivery: responseWritten, fault: atTerminator(syscall.ENOBUFS, firstAttempt), wantAttempts: 2, check: whole,
		},
		{
			// The client stops at cap+1 bytes and never reads the terminator.
			name: "over-cap terminator refused with ENOBUFS is kept because the body arrived", page: overCap,
			delivery: bodyWritten, fault: atTerminator(syscall.ENOBUFS, firstAttempt), wantAttempts: 1, check: refused,
		},
		{
			name: "over-cap body failing with EPIPE is not fetched again", page: overCap,
			delivery: bodyWritten, fault: inBody(syscall.EPIPE, firstAttempt), wantAttempts: 1, check: clipped,
		},
		{
			name: "at-cap terminator failing with ECONNRESET is not fetched again", page: atCap,
			delivery: responseWritten, fault: atTerminator(syscall.ECONNRESET, firstAttempt), wantAttempts: 1, check: clipped,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fetched, deliveryErr := fetchDelivered(t, tc.page, opengraph, tc.delivery, tc.fault)
			require.NoError(t, deliveryErr)
			assert.Equal(t, tc.wantAttempts, fetched.attempts)
			tc.check(t, fetched)
		})
	}

	t.Run("ENOBUFS on every attempt fails the fixture, not the assertions", func(t *testing.T) {
		t.Parallel()

		fetched, deliveryErr := fetchDelivered(t, overCap, opengraph, bodyWritten, inBody(syscall.ENOBUFS, everyAttempt))
		require.ErrorIs(t, deliveryErr, syscall.ENOBUFS)
		assert.ErrorContains(t, deliveryErr, "ENOBUFS")
		assert.Equal(t, fixtureDeliveryAttempts, fetched.attempts)
	})
}
