package oauth

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WithMaxResponseBytes takes an int64 and does nothing with it, so three values
// an operator can plausibly pass turn the client into something worse than
// uncapped.
//
// # WHAT EACH ONE DOES TODAY, VERIFIED BY PROBE
//
//	WithMaxResponseBytes(0)              every non-empty response is refused
//	WithMaxResponseBytes(-1)             Read returns n = -1
//	WithMaxResponseBytes(math.MaxInt64)  Read panics: slice bounds out of range
//
// The negative case is the one that is not merely wrong but ILLEGAL. io.Reader
// forbids a negative count, and the standard library does not defend against
// one: bytes.Buffer.ReadFrom panics with "reader returned negative count from
// Read". So a caller doing the most ordinary thing there is with a response body
// takes a panic out of a configuration typo, on a goroutine it may not own.
//
// MaxInt64 is a typo away from being what someone writes for "no limit", and it
// panics inside cappedBody.Read on `remaining+1` overflowing to MinInt64 and
// being used as a slice bound.
//
// Zero is the value a struct field, a missing config key or an unparsed
// environment variable arrives as. It refuses every response, which reads at the
// call site as "this remote host is broken" — for every host.
//
// # WHAT THIS ASSERTS, AND WHAT IT DELIBERATELY DOES NOT
//
// The property is that a cap outside the usable range does not break the
// client: an ordinary small response still arrives, whole, without a panic and
// without a negative count. The clamp VALUE is left to the implementation —
// asserting "0 becomes DefaultMaxResponseBytes" would pin a number the
// behaviour does not require, and the next person to tune it would have to
// change a test that was never about that.
//
// TestSSRFSafeHTTPClient_DefaultCap and the boundary tests in
// transport_body_cap_test.go remain the fence in the other direction: whatever
// clamping is added must not turn a SENSIBLE cap into a larger one.
func TestWithMaxResponseBytes_ACapOutsideTheUsableRangeDoesNotBreakTheClient(t *testing.T) {
	t.Parallel()

	// Deliberately tiny. Any cap a reasonable clamp could produce is above this,
	// so the assertion is about the cap being usable at all rather than about
	// where it landed.
	const payload = 512

	tests := []struct {
		name string
		cap  int64
		why  string
	}{
		{
			name: "zero",
			cap:  0,
			why: "zero is what an unset field, a missing config key or an unparsed environment variable " +
				"arrives as, and it currently refuses every non-empty response there is",
		},
		{
			name: "negative",
			cap:  -1,
			why: "a negative cap makes cappedBody.Read return n = -1, which io.Reader forbids outright — " +
				"bytes.Buffer.ReadFrom panics on it, so a configuration typo becomes a panic in a caller " +
				"doing nothing unusual",
		},
		{
			name: "math.MaxInt64",
			cap:  math.MaxInt64,
			why: "MaxInt64 is what someone writes for 'no limit'; remaining+1 overflows to MinInt64 and is " +
				"used as a slice bound, so the first read panics",
		},
	}

	// Both response shapes, because the two are stopped by different mechanisms
	// and only one of them reaches cappedBody.Read at all. A declared length is
	// refused by the header early-out — which is where a negative or zero cap
	// currently fails, before any byte is read — while a chunked response has no
	// length to check and goes through the wrapper, which is where the negative
	// count and the overflow panic actually live. Testing only the first would
	// pin the symptom and miss the io.Reader violation entirely.
	shapes := []struct {
		name    string
		chunked bool
	}{
		{name: "declared length", chunked: false},
		{name: "chunked, no declared length", chunked: true},
	}

	for _, tt := range tests {
		for _, shape := range shapes {
			t.Run(tt.name+"/"+shape.name, func(t *testing.T) {
				t.Parallel()

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					if shape.chunked {
						// Flushing the header commits the response before the
						// body is known, so Go frames it chunked and declares
						// no length.
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
					}
					writeChunks(w, payload)
				}))
				defer server.Close()

				// allowPrivate, because the listener is on loopback and
				// classification is not what is under test here.
				resp, err := NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed(), WithMaxResponseBytes(tt.cap)).Get(server.URL)
				require.NoErrorf(t, err,
					"a %d-byte response was refused by the request itself under WithMaxResponseBytes(%d): %s; got: %v",
					payload, tt.cap, tt.why, err)
				defer func() { _ = resp.Body.Close() }()

				n, readErr := readWithoutPanicking(resp.Body)

				require.NoErrorf(t, readErr,
					"reading a %d-byte body through WithMaxResponseBytes(%d) failed: %s; got: %v",
					payload, tt.cap, tt.why, readErr)
				assert.EqualValuesf(t, payload, n,
					"the caller obtained %d of %d bytes under WithMaxResponseBytes(%d). A cap outside the "+
						"usable range must fall back to a usable one, not truncate: %s", n, payload, tt.cap, tt.why)
			})
		}
	}
}

// readWithoutPanicking drains r through bytes.Buffer.ReadFrom and turns a panic
// into an error.
//
// bytes.Buffer.ReadFrom is the reader chosen on purpose: it is what panics on a
// negative count, so it is the detector for the io.Reader violation this test is
// about, and it is what any caller building a body in memory ends up using.
//
// The recover is not defensive decoration. An unrecovered panic takes the whole
// package's test binary down with it, so the RED run for one row would hide
// every other failure in the package behind a stack trace — and the point of
// this cycle is a readable list of what is broken.
func readWithoutPanicking(r io.Reader) (n int64, err error) {
	defer func() {
		if p := recover(); p != nil {
			n, err = 0, fmt.Errorf("reading the response body PANICKED: %v", p)
		}
	}()

	var buf bytes.Buffer
	n, err = buf.ReadFrom(r)
	return n, err
}
