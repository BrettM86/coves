package oauth

import (
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The response-size cap, at the unit level. The outer acceptance contract in
// transport_response_cap_test.go pins the headline property — an over-large
// chunked body fails the read rather than truncating — and this file covers the
// shapes around it.
//
// # WHY THE CAP BELONGS TO THE TRANSPORT
//
// Four callers already implement this control themselves, at four different
// limits: blobs at 6 MB, the image proxy at 10 MB by default, unfurl at 10 MB,
// profile backfill at 1 MiB. Nine more fetch sites are about to be wired onto
// this client, and the ones that forget are the ones that matter — an
// unbounded io.ReadAll on an attacker-chosen URL is an out-of-memory the remote
// host triggers whenever it likes. A control every caller must remember is a
// control that will be missing somewhere.
//
// # REDIRECTS ARE PER-HOP, DELIBERATELY
//
// RoundTrip runs once per hop, so every hop's body is wrapped, but the cap does
// not accumulate across a redirect chain. That is adequate rather than a
// compromise: http.Client drains a redirect body at roughly 2 KB before
// following, far under any cap worth setting. There is no test for it here
// because it is an incidental of where the wrapping happens, and pinning an
// incidental is how a test suite acquires assertions nobody can change.

// capPayload is the size of the over-cap bodies below, and testCap the limit
// they are read through. Small on purpose: the property is a boundary, and a
// test that proves it with 64 KiB proves it exactly as well as one that moves
// 32 MiB, in a fraction of the time.
const (
	testCap    = 64 << 10
	capPayload = 1 << 20
)

// writeChunks writes n bytes and stops early if the client has gone away, which
// is the expected end of an over-cap response rather than a failure: once the
// cap engages the transport drops the connection and the handler's next write
// fails.
func writeChunks(w io.Writer, n int) {
	chunk := make([]byte, 4<<10)
	for sent := 0; sent < n; {
		// Clamped to the remainder. A loop that always writes a full chunk
		// overshoots any n that is not a multiple of the chunk size, which is
		// fatal to a boundary test: it would send 4096 bytes for a 4097-byte cap
		// and "one byte over" would silently become "one byte under".
		if len(chunk) > n-sent {
			chunk = chunk[:n-sent]
		}
		written, err := w.Write(chunk)
		if err != nil {
			return
		}
		sent += written
	}
}

// cappedClient is the client under test: the hatch is open because every
// listener here is on loopback, and classification is not what these tests are
// about.
func cappedClient(t *testing.T, max int64) *http.Client {
	t.Helper()
	return NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed(), WithMaxResponseBytes(max))
}

// fetchBody performs the request and reads the body to exhaustion, returning
// whichever error a caller would actually see — the request's or the read's.
//
// The two are deliberately collapsed for the boundary test below, because the
// cap may legitimately be enforced at either point: a Content-Length early-out
// refuses before the body, a body wrapper fails during it. Which mechanism
// catches an over-cap response is the implementer's choice; that it is caught is
// the property. Where the DISTINCTION is the point — the early-out, the lying
// header — the tests below drive Get and ReadAll separately instead.
func fetchBody(t *testing.T, client *http.Client, url string) ([]byte, error) {
	t.Helper()

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// TestSSRFSafeHTTPClient_RefusesAnOversizedContentLengthBeforeReading pins the
// cheap early-out: a response that ANNOUNCES more than the cap is refused
// without its body being read at all.
//
// This is an optimisation rather than a security control — the header is chosen
// by the same party as the body, so it is only ever a hint, and the test below
// this one is what pins the control proper. The distinction matters for what is
// asserted: the property here is that the failure arrives from client.Get
// itself, with no response to read, rather than partway through a body that has
// already been streamed into this process.
//
// # THE TRIPWIRE, AND WHY THE ERROR ALONE WAS NOT ENOUGH
//
// This test used to assert only that some error came back, and it passed for
// the wrong reason. Mutation-proven: insert `io.Copy(io.Discard, resp.Body)`
// BEFORE the refusal — so the whole megabyte is read into the process and then
// discarded, which is precisely the failure the test's name rejects — and it
// stayed green. "Refuses before reading" was in the name and nowhere in the
// assertions.
//
// What discriminates is a SERVER-SIDE byte count: the handler cannot finish
// writing a body nobody is reading, because the client drops the connection and
// the next write fails. So a handler that completed all its writes is a handler
// whose body was drained. It is the byte-level analogue of the `reached`
// tripwire in transport_response_cap_test.go, and it is measured at the server
// because the client side of a drained-and-discarded body looks identical to
// one that was never read.
//
// The payload is deliberately far larger than any socket buffer: the handler
// gets to write whatever the kernel absorbs before the close is noticed
// (~250 KB on the machine this was probed on), and the assertion has to sit
// well clear of that number to mean anything.
func TestSSRFSafeHTTPClient_RefusesAnOversizedContentLengthBeforeReading(t *testing.T) {
	t.Parallel()

	// Eight megabytes, thirty-odd times what a socket buffer holds. See above.
	const declaredPayload = 8 << 20

	var handlerWrote atomic.Int64
	// Closed when the handler returns, so the count below is read after the
	// handler has finished rather than while it is still writing. A channel and
	// not a poll loop: the handler's exit is an event, and waiting for an event
	// with a timer is the race this suite exists without.
	handlerDone := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(handlerDone)

		w.Header().Set("Content-Length", strconv.Itoa(declaredPayload))
		w.WriteHeader(http.StatusOK)

		chunk := make([]byte, 4<<10)
		for sent := 0; sent < declaredPayload; {
			if len(chunk) > declaredPayload-sent {
				chunk = chunk[:declaredPayload-sent]
			}
			written, err := w.Write(chunk)
			handlerWrote.Add(int64(written))
			if err != nil {
				// The expected end: the client refused and hung up.
				return
			}
			sent += written
		}
	}))
	defer server.Close()

	resp, err := cappedClient(t, testCap).Get(server.URL)
	if err == nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err,
		"a response announcing %d bytes through a %d-byte cap must be refused by the request itself. Reading "+
			"the body first and checking afterwards is the same as having no cap: the bytes are already in "+
			"memory by the time the check runs", declaredPayload, int64(testCap))

	// By identity, not by substring. Nothing else in this file asserts the
	// sentinel, so a refusal that came back as any other error — a dial failure,
	// a parse error, a nil-deref recovered somewhere — would have satisfied the
	// require above while meaning something entirely different.
	require.ErrorIs(t, err, ErrResponseTooLarge,
		"the refusal must be the cap's, matchable by identity: the callers being wired onto this client "+
			"have to tell an over-large body from a transport failure to choose a status code; got: %v", err)

	<-handlerDone
	assert.Lessf(t, handlerWrote.Load(), int64(declaredPayload),
		"the handler wrote all %d bytes of a body that was supposed to be refused unread. A server cannot "+
			"finish writing to a client that has hung up, so a complete write means this process read and "+
			"discarded the whole body first — which is the same as having no cap at all, and is exactly "+
			"what this test's name says does not happen. Wrote %d bytes.",
		declaredPayload, handlerWrote.Load())
}

// TestSSRFSafeHTTPClient_DeclaredContentLengthBoundsTheBody is a
// CHARACTERIZATION test, and it passes today. It exists because the early-out
// above is only sound if this holds.
//
// # WHY THIS IS NOT THE "LYING HEADER" TEST IT LOOKS LIKE
//
// The obvious companion to the early-out is a server that declares a small
// Content-Length and then writes far more, proving the header cannot be trusted
// and the body wrapper must catch it. That test cannot be written, because the
// scenario does not exist: net/http's client frames the body by the declared
// length and stops there. Verified by probe against a HIJACKED handler, so the
// Go server's own Content-Length enforcement was out of the way and the bytes on
// the wire were a genuinely lying response — the caller received exactly 128
// bytes with a nil error while the server wrote a megabyte.
//
// So an understated header cannot overflow a caller, and a test asserting the
// wrapper catches it would be asserting an artifact of the standard library
// rather than anything this package does.
//
// The header is still not the control, but the reason is different and worth
// getting right: it can be ABSENT. A chunked response has no length, and a
// transparently decompressed one has its length deleted by the transport. Those
// are the cases the wrapper exists for, and they are pinned in
// TestSSRFSafeHTTPClient_CapsDecompressedBytesNotCompressedBytes and
// TestSSRFSafeHTTPClient_UnknownLengthIsNeitherTrustedNorRejected.
//
// What this test protects is the assumption underneath the early-out: if the
// client ever stopped honouring the declared length, a response could sail past
// a header check and then deliver more than it promised. That would turn the
// cheap optimisation into a hole, and this is what would notice.
func TestSSRFSafeHTTPClient_DeclaredContentLengthBoundsTheBody(t *testing.T) {
	t.Parallel()

	const declared = 128

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Hijacked so the response on the wire really does understate itself.
		// Through the normal ResponseWriter the Go SERVER would refuse the
		// excess writes, and the test would be pinning the server's behavior
		// rather than the client's.
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(declared) + "\r\n\r\n")
		writeChunks(buf, capPayload)
		_ = buf.Flush()
	}))
	defer server.Close()

	resp, err := cappedClient(t, testCap).Get(server.URL)
	require.NoError(t, err, "a response declaring %d bytes is far under the cap and must not be refused", declared)
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)

	require.NoError(t, readErr, "the declared body is well under the cap and must read cleanly; got: %v", readErr)
	assert.Len(t, body, declared,
		"the caller received %d bytes from a response that declared %d and then wrote %d. net/http no longer "+
			"frames the body by the declared length, which means a Content-Length early-out can now be walked "+
			"past by a server that understates itself — the cap must not rely on the header alone",
		len(body), declared, capPayload)
}

// TestSSRFSafeHTTPClient_CapsDecompressedBytesNotCompressedBytes is the case
// most easily missed, and the one that quietly disables every other check.
//
// http.Transport adds Accept-Encoding: gzip on its own and transparently
// decompresses the reply. When it does, it DELETES Content-Length and sets
// resp.ContentLength to -1 — so the header early-out above is a no-op against
// any compressed response, and enabling compression is all it takes to walk past
// it.
//
// Decompressed bytes are the only honest unit. They are what io.ReadAll
// allocates, and zeros compress about a thousand to one: a cap counting
// compressed bytes lets a body three orders of magnitude over the limit through,
// which is the classic decompression bomb.
//
// This is also why the wrapper belongs on the response the base transport
// RETURNS — wrapping there wraps the gzip reader's output, and this case then
// costs nothing to satisfy.
func TestSSRFSafeHTTPClient_CapsDecompressedBytesNotCompressedBytes(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)

		gz := gzip.NewWriter(w)
		writeChunks(gz, capPayload)
		_ = gz.Close()
	}))
	defer server.Close()

	resp, err := cappedClient(t, testCap).Get(server.URL)
	require.NoError(t, err, "the request must succeed: compressed, this response is a few kilobytes")
	defer func() { _ = resp.Body.Close() }()

	// Precondition, not decoration. If the transport did not decompress this
	// transparently the test would be reading gzip bytes and its premise would
	// be gone, so it is checked rather than assumed.
	require.True(t, resp.Uncompressed,
		"http.Transport did not transparently decompress this response, so the test is not exercising the "+
			"decompressed path it was written for")
	require.EqualValues(t, -1, resp.ContentLength,
		"a transparently decompressed response must report an unknown length, which is precisely why the "+
			"Content-Length early-out cannot be the control here")

	body, readErr := io.ReadAll(resp.Body)

	require.Error(t, readErr,
		"%d decompressed bytes were read through a %d-byte cap. The compressed body was a few kilobytes, so a "+
			"cap counting bytes off the wire never fired — and the memory a decompression bomb costs is "+
			"decompressed memory", len(body), int64(testCap))
	assert.LessOrEqual(t, int64(len(body)), int64(testCap),
		"the caller obtained %d decompressed bytes through a %d-byte cap", len(body), int64(testCap))
}

// TestSSRFSafeHTTPClient_CapBoundary pins the two sizes that decide whether the
// comparison is > or >=.
//
// blobs.(*blobService).FetchImageForURL documents the technique — read
// max+1 bytes, and the presence of that one extra byte is what separates "at the
// limit" from "over it". A cap implemented by measuring after an io.ReadAll gets
// the boundary right and the property wrong, since the whole body is in memory
// by then.
//
// No Content-Length is sent, so the body wrapper is what has to catch the
// over-cap case; the sizes are small enough that Go would otherwise buffer and
// declare a length.
func TestSSRFSafeHTTPClient_CapBoundary(t *testing.T) {
	t.Parallel()

	const boundary = 4 << 10

	tests := []struct {
		name      string
		size      int
		mustFail  bool
		assertion string
	}{
		{
			name:     "exactly at the cap",
			size:     boundary,
			mustFail: false,
			assertion: "a body of exactly the cap must be readable in full. A cap that refuses its own limit " +
				"is off by one in the direction that breaks callers rather than the one that protects them",
		},
		{
			name:     "one byte over the cap",
			size:     boundary + 1,
			mustFail: true,
			assertion: "a body one byte over the cap must fail. This is the assertion that distinguishes a real " +
				"limit from one that rounds, buffers, or compares after the fact",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Flushing forces a chunked response, so nothing here declares a
				// Content-Length and the wrapper is the only thing that can catch
				// the over-cap case.
				w.WriteHeader(http.StatusOK)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				writeChunks(w, tt.size)
			}))
			defer server.Close()

			body, err := fetchBody(t, cappedClient(t, boundary), server.URL)

			if tt.mustFail {
				require.Error(t, err, "%s; read %d bytes", tt.assertion, len(body))
				assert.LessOrEqual(t, int64(len(body)), int64(boundary),
					"the caller obtained %d bytes through a %d-byte cap", len(body), int64(boundary))
				return
			}
			require.NoError(t, err, "%s; got: %v", tt.assertion, err)
			assert.Len(t, body, tt.size,
				"a body at exactly the cap must arrive complete, not one byte short")
		})
	}
}

// TestSSRFSafeHTTPClient_UnknownLengthIsNeitherTrustedNorRejected pins the
// meaning of ContentLength == -1.
//
// It means "unknown", and it arrives from two ordinary places: a chunked
// response, and any response the transport transparently decompressed. So it can
// be read neither as 0 ("nothing to check") nor as a reason to refuse — the
// first waves through every streaming body, the second breaks chunked responses
// and, by way of the gzip case above, a large share of the real internet.
//
// Both directions are pinned because an implementation can get one right and the
// other wrong.
func TestSSRFSafeHTTPClient_UnknownLengthIsNeitherTrustedNorRejected(t *testing.T) {
	t.Parallel()

	newChunkedServer := func(size int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			writeChunks(w, size)
		}))
	}

	t.Run("an unknown length under the cap is delivered", func(t *testing.T) {
		t.Parallel()

		server := newChunkedServer(testCap / 4)
		defer server.Close()

		body, err := fetchBody(t, cappedClient(t, testCap), server.URL)
		require.NoError(t, err,
			"a chunked response well under the cap was refused. ContentLength is -1 for every chunked body and "+
				"every decompressed one, so treating an unknown length as a reason to refuse rejects ordinary "+
				"traffic; got: %v", err)
		assert.Len(t, body, testCap/4, "the body must arrive complete")
	})

	t.Run("an unknown length over the cap is stopped", func(t *testing.T) {
		t.Parallel()

		server := newChunkedServer(capPayload)
		defer server.Close()

		body, err := fetchBody(t, cappedClient(t, testCap), server.URL)
		require.Error(t, err,
			"a chunked response of %d bytes was read in full through a %d-byte cap. An unknown length read as "+
				"'nothing to check' waves through every streaming body, which is exactly the shape an attacker "+
				"would choose; read %d bytes", capPayload, int64(testCap), len(body))
	})
}

// TestSSRFSafeHTTPClient_UnderCapResponseReusesTheConnection pins that the
// wrapper delegates Close to the body it wraps.
//
// A wrapper that implements Read and forgets Close is invisible in every
// functional test — bodies still arrive, caps still fire — and leaks a
// connection per request. http.Transport returns a connection to the pool when
// the body is closed and drained; if the close never reaches it, MaxIdleConns
// and IdleConnTimeout describe a pool nothing is ever returned to, and a busy
// AppView accumulates sockets until it runs out.
//
// Asserted through the server's connection count rather than the wrapper's
// shape, so the implementer keeps room: two sequential requests over one client
// must be observed as ONE connection.
func TestSSRFSafeHTTPClient_UnderCapResponseReusesTheConnection(t *testing.T) {
	t.Parallel()

	var connections atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeChunks(w, 1<<10)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	client := cappedClient(t, testCap)

	// Sequential, and each body fully read and closed — which is exactly what a
	// pooled connection requires, and what makes a missing Close observable.
	for i := range 2 {
		body, err := fetchBody(t, client, server.URL)
		require.NoErrorf(t, err, "request %d must succeed: %d bytes is far under the cap", i+1, 1<<10)
		require.Lenf(t, body, 1<<10, "request %d must return the whole body", i+1)
	}

	assert.EqualValues(t, 1, connections.Load(),
		"the server saw %d connections for two sequential requests. The second did not reuse the first, which "+
			"means the response body never reported itself closed to http.Transport — a capping wrapper that "+
			"implements Read and forgets Close passes every functional test and leaks one connection per "+
			"request", connections.Load())
}

// TestSSRFSafeHTTPClient_EmptyBodiesDoNotTripTheCap covers the responses that
// have no body to cap.
//
// Both rows exist because zero is a suspicious-looking number: an implementation
// that treats ContentLength == 0 as "unknown, therefore check harder", or that
// requires at least one byte before deciding a response is well-formed, breaks
// on traffic that is entirely ordinary.
func TestSSRFSafeHTTPClient_EmptyBodiesDoNotTripTheCap(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/no-content" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		writeChunks(w, 1<<10)
	}))
	// t.Cleanup, NOT defer. This server is shared with PARALLEL subtests, and a
	// deferred Close runs when this function returns — which is the moment the
	// first subtest calls t.Parallel and pauses, long before any of them make a
	// request. The result is a connection-refused failure that looks like a bug
	// in the code under test. t.Cleanup runs after the subtests finish.
	t.Cleanup(server.Close)

	client := cappedClient(t, testCap)

	t.Run("204 No Content", func(t *testing.T) {
		t.Parallel()

		body, err := fetchBody(t, client, server.URL+"/no-content")
		require.NoError(t, err, "a 204 carries no body and must not be treated as a capped read; got: %v", err)
		assert.Empty(t, body, "a 204 must produce an empty body")
	})

	t.Run("HEAD request", func(t *testing.T) {
		t.Parallel()

		resp, err := client.Head(server.URL + "/")
		require.NoError(t, err, "a HEAD request must succeed; got: %v", err)
		defer func() { _ = resp.Body.Close() }()

		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr, "a HEAD response has no body to read; got: %v", readErr)
		assert.Empty(t, body, "a HEAD response must produce an empty body")
	})
}

// TestSSRFSafeHTTPClient_DefaultCap pins what a caller gets without an option.
//
// The VALUE is asserted against the constant so the number is stated once. 32
// MiB is chosen to sit above every cap in this tree — the largest is the image
// proxy's 10 MB — so that pointing an existing caller at this client cannot make
// a request that used to work start failing. That cross-package claim is not
// asserted here on purpose: those packages import this one, so a test importing
// them back would be an import cycle.
//
// The behavioral half is the one that catches a real mistake. A cap stored in a
// zero-valued field, because the default was never applied when no option was
// passed, refuses EVERY response — and a test that only ever constructs clients
// with WithMaxResponseBytes, as every other test in this file does, would not
// notice.
func TestSSRFSafeHTTPClient_DefaultCap(t *testing.T) {
	t.Parallel()

	assert.EqualValues(t, 32<<20, DefaultMaxResponseBytes,
		"the default cap must be 32 MiB: above every caller's own limit in this tree (the largest is 10 MB), "+
			"so adopting this client changes no existing behavior, and still a bound on what a remote host can "+
			"make this process allocate")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeChunks(w, 1<<20)
	}))
	defer server.Close()

	// No WithMaxResponseBytes: the default is what is under test.
	body, err := fetchBody(t, NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed()), server.URL)
	require.NoError(t, err,
		"a 1 MiB response was refused by a client with no cap option, so the default was not applied and the "+
			"limit is sitting at a zero value — which refuses every response there is; got: %v", err)
	assert.Len(t, body, 1<<20, "the body must arrive complete under the default cap")
}
