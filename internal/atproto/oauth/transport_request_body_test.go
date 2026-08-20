package oauth

import (
	"bytes"
	"context"
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

// RoundTrip must close the request body on EVERY return, and this transport's
// refusals do not.
//
// # THIS IS A STANDARD LIBRARY CONTRACT, NOT A STYLE PREFERENCE
//
// net/http's RoundTripper documentation states it outright: "RoundTrip must
// always close the body, including on errors". http.Client is written against
// that promise — it does not close the body itself after calling RoundTrip — so
// a RoundTripper that returns early without closing leaks whatever the body was
// holding. For the callers being wired onto this client that is a file handle,
// a pipe, or a buffer with a finalizer, one per refused request.
//
// # WHY THE REFUSAL PATHS ARE EXACTLY THE ONES THAT LEAK
//
// The three returns below all happen BEFORE the base transport is reached, and
// the base transport is what would otherwise have closed the body. They are
// also the paths a hostile input takes: a caller pointed at an attacker-chosen
// URL refuses often, and the leak scales with how well the guard is working.
//
// # WHY THIS DRIVES RoundTrip DIRECTLY RATHER THAN client.Do
//
// Because the contract belongs to RoundTrip. Going through http.Client would
// measure whatever the standard library does around it and could not tell a
// transport that closes the body from one that does not.
func TestSSRFSafeTransport_ClosesTheRequestBodyOnEveryRefusal(t *testing.T) {
	t.Parallel()

	// Checked, not assumed: isPrivateIP(nil) returns false, so a typo here would
	// classify as public and the private-address row would stop being one.
	private := net.ParseIP("10.99.13.37")
	require.NotNil(t, private, "the test's own private address must parse")

	tests := []struct {
		name    string
		host    string
		answers map[string][]net.IP
		why     string
	}{
		{
			// A genuinely public address, so only the literal-shape check can
			// refuse it before resolution.
			name:    "an IP literal, refused before resolution",
			host:    "8.8.8.8",
			answers: map[string][]net.IP{},
			why:     "the literal check returns before anything else in RoundTrip runs",
		},
		{
			name:    "a hostname that does not resolve",
			host:    "unresolvable.test",
			answers: map[string][]net.IP{},
			why:     "the resolution-failure return is taken on every DNS hiccup, not only on hostile input",
		},
		{
			name:    "a hostname resolving to a private address",
			host:    "blocked.test",
			answers: map[string][]net.IP{"blocked.test": {private}},
			why:     "the classification refusal is the guard's main path and therefore the most frequent leak",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := NewSSRFSafeHTTPClient()
			transport, ok := client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
			transport.lookupIP = (&hostRoutedResolver{answers: tt.answers}).lookup

			// A dialler that records and refuses, so a guard that let any of
			// these through fails loudly here instead of opening a socket.
			var dialled atomic.Bool
			transport.base = &http.Transport{
				DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
					dialled.Store(true)
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: net.UnknownNetworkError(addr)}
				},
			}

			body := &countingBody{Reader: bytes.NewReader([]byte("a request body"))}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+tt.host+"/", body)
			require.NoError(t, err, "building the request")

			resp, err := transport.RoundTrip(req)
			if err == nil {
				_ = resp.Body.Close()
			}

			require.Error(t, err, "%s must be refused", tt.host)
			require.False(t, dialled.Load(), "%s must be refused before any dial", tt.host)

			assert.EqualValues(t, 1, body.closes.Load(),
				"RoundTrip returned without closing the request body (Close called %d times). net/http "+
					"documents that a RoundTripper must always close the body, INCLUDING ON ERRORS, and "+
					"http.Client relies on that promise rather than closing it itself — so every refusal on "+
					"this path leaks whatever the body was holding. %s",
				body.closes.Load(), tt.why)
		})
	}

	// The fourth early return, and it is a FENCE rather than a RED.
	//
	// By the time an oversized declared length is refused the request has been
	// sent, so the base transport has already closed the body — verified by
	// probe: exactly one Close today. It is here so that the fix for the three
	// rows above is applied to the returns that need it rather than as a blanket
	// `defer` on entry, which would close this body a SECOND time. Once is the
	// contract, both directions.
	t.Run("fence: an oversized declared length closes the body exactly once", func(t *testing.T) {
		t.Parallel()

		const declared = 1 << 20

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Length", strconv.Itoa(declared))
			w.WriteHeader(http.StatusOK)
			writeChunks(w, declared)
		}))
		defer server.Close()

		// The hatch is open because the listener is on loopback; the response
		// cap is what this row is about.
		client := NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed(), WithMaxResponseBytes(testCap))
		transport, ok := client.Transport.(*ssrfSafeTransport)
		require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)

		body := &countingBody{Reader: bytes.NewReader([]byte("a request body"))}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, body)
		require.NoError(t, err, "building the request")

		resp, err := transport.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}

		require.Error(t, err, "a response declaring %d bytes through a %d-byte cap must be refused", declared, testCap)
		assert.EqualValues(t, 1, body.closes.Load(),
			"the request body was closed %d times on the declared-length refusal. The base transport has "+
				"already closed it by this point, so closing again here — which is what a blanket defer on "+
				"RoundTrip's entry would do — breaks the contract in the other direction: a body whose Close "+
				"is not idempotent (a file, a pipe) reports an error the second time",
			body.closes.Load())
	})
}

// countingBody is a request body that counts how many times it was closed.
//
// atomic because http.Transport writes the request on a goroutine of its own,
// so the close in the fence subtest above happens off the test's goroutine.
type countingBody struct {
	io.Reader
	closes atomic.Int64
}

func (b *countingBody) Close() error {
	b.closes.Add(1)
	return nil
}
