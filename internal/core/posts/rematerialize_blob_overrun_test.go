package posts

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	covesoauth "Coves/internal/atproto/oauth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The overrun probe, driven through the client production actually builds.
//
// TestRematerializeBlobClient_Fetch_FailsRatherThanTruncating already covers the
// probe, but it hands newRematerializeBlobClient an httptest server.Client() —
// an UNGUARDED client with no byte cap of its own. So it proves Fetch's
// arithmetic and nothing about the transport underneath, which is exactly how
// the transport came to clip the probing byte off with every existing test
// green.

// TestDefaultRematerializeBlobClient_AnOverrunKeepsItsCIDExplanation is the
// end-to-end version, at a size a test can move.
//
// # THE TWO CAPS ARE OFF BY ONE FROM EACH OTHER, ON PURPOSE
//
// Fetch reads c.maxBytes+1 through an io.LimitReader because io.ReadAll cannot
// tell "the body ended" from "the limit was reached" — the extra byte's
// existence IS the overrun signal, and its whole point is to produce an error
// that explains the hazard: a truncated blob is DIFFERENT bytes, so it lands
// under a different CID and the postv2 ends up pointing at media the repo does
// not serve. A transport cap set to exactly c.maxBytes clips that byte, the
// probe never completes, and the overrun surfaces as a generic
// ErrResponseTooLarge naming a limit no operator configured.
//
// # WHY THE BODY HERE IS CHUNKED
//
// The transport ALSO refuses an announced Content-Length above its cap, before
// a byte is read. That branch fires first for any response that declares its
// length, so it would mask the read path entirely. A chunked body — which is
// what a large blob stream and any transparently-decompressed response both are
// — reports ContentLength -1 and reaches the probing read, which is the path
// this off-by-one lives on.
//
// Not a security hole either way: an oversized blob is refused in both worlds.
// What the fix buys is the message, during a one-shot migration that has already
// written the postv2 and is about to delete the only intact copy.
func TestDefaultRematerializeBlobClient_AnOverrunKeepsItsCIDExplanation(t *testing.T) {
	t.Parallel()

	const copyCap = 1024
	oversized := strings.Repeat("x", copyCap+64)

	// The hatch is open because the fixture is on loopback. The byte cap is a
	// different control from the address guard.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "the fixture needs a Flusher to send a chunked body")
		w.WriteHeader(http.StatusOK)
		flusher.Flush() // no Content-Length: the response is chunked from here
		_, _ = w.Write([]byte(oversized))
	}))
	t.Cleanup(server.Close)

	client := newGuardedRematerializeBlobClient(true, copyCap)
	_, err := client.Fetch(context.Background(), server.URL,
		rematerializeGuardDID, rematerializeGuardCID)

	require.Error(t, err, "a body over the copy cap must never be accepted")
	assert.NotErrorIsf(t, err, covesoauth.ErrResponseTooLarge,
		"the overrun was refused by the TRANSPORT's cap instead of by Fetch's own probe. The transport is "+
			"clipping at exactly the copy cap, so the byte Fetch reads past it to DETECT an overrun never "+
			"arrives and `len(data) > c.maxBytes` is unreachable in production; got: %v", err)
	assert.Containsf(t, err.Error(), "truncated",
		"the overrun error no longer explains the hazard. An operator mid-migration needs to read that a "+
			"truncated copy is DIFFERENT bytes under a different CID, not a byte count against a limit "+
			"they never set; got: %v", err)
}

// TestDefaultRematerializeBlobClient_TheBoundaryBlobIsRefusedByItsOwnCap is the
// declared-length half of the same boundary.
//
// A body of exactly copyCap+1 announces copyCap+1, so the transport's header
// check sees the ONE length that separates the two caps: refused when the
// transport clips at copyCap, delivered when it allows the probing byte. It is
// the smallest input that tells the two implementations apart with a
// Content-Length present.
func TestDefaultRematerializeBlobClient_TheBoundaryBlobIsRefusedByItsOwnCap(t *testing.T) {
	t.Parallel()

	const copyCap = 1024
	body := strings.Repeat("x", copyCap+1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	client := newGuardedRematerializeBlobClient(true, copyCap)
	_, err := client.Fetch(context.Background(), server.URL,
		rematerializeGuardDID, rematerializeGuardCID)

	require.Error(t, err, "a blob one byte over the copy cap must be refused")
	assert.Containsf(t, err.Error(), "truncated",
		"a blob of exactly copyCap+1 was refused by the transport's announced-length check rather than by "+
			"Fetch's overrun probe, so the CID hazard goes unexplained at the one size the two caps "+
			"disagree about; got: %v", err)
}

// TestDefaultRematerializeBlobClient_ABlobAtTheCapStillArrives is the other side
// of the boundary, and it is what stops "+1" from drifting upward: a body of
// exactly the copy cap is legitimate and must come back byte-for-byte.
func TestDefaultRematerializeBlobClient_ABlobAtTheCapStillArrives(t *testing.T) {
	t.Parallel()

	const copyCap = 1024
	body := strings.Repeat("y", copyCap)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	client := newGuardedRematerializeBlobClient(true, copyCap)
	data, err := client.Fetch(context.Background(), server.URL,
		rematerializeGuardDID, rematerializeGuardCID)

	require.NoError(t, err, "a blob of exactly the copy cap is inside the limit and must arrive")
	assert.Equalf(t, body, string(data),
		"a blob at exactly the copy cap came back changed, so the probing byte is being counted as "+
			"payload; got %d bytes", len(data))
}
