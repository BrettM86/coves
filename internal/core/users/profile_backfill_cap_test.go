package users

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

// The backfill fetch's byte ceiling, which the conversion onto the shared client
// left behind.
//
// FetchProfileRecord has capped itself at maxProfileResponseBytes (1 MiB) since
// before this remediation, through an io.LimitReader. What the conversion did
// not carry across is the TRANSPORT's own cap, which defaults to
// oauth.DefaultMaxResponseBytes — 32 MiB, THIRTY-TWO TIMES this site's own
// limit. oauth.DefaultMaxResponseBytes documents the obligation by name: a
// caller with its own limit has to state it.
//
// The gap is not academic. The LimitReader bounds what this process ALLOCATES
// and nothing else; the transport's cap is the half that refuses an announced
// length before a byte of body is read, and refuses it on a fetch whose host is
// whatever the indexed user's PDS says it is, from a goroutine detached with
// context.WithoutCancel that nothing waits on.

// TestNewProfileBackfillClient_AppliesThisSitesOwnByteCap pins the announced
// length at the boundary the two caps disagree about.
//
// A declared length between 1 MiB and 32 MiB is delivered under the shared
// default and refused under this site's own. Asserted through the header rather
// than by moving a megabyte, because the transport's check is
// `resp.ContentLength > maxResponseBytes` in RoundTrip and is reached without a
// body: the fetch fails either way, and what the test reads is WHICH failure.
func TestNewProfileBackfillClient_AppliesThisSitesOwnByteCap(t *testing.T) {
	t.Parallel()

	// Comfortably above this site's 1 MiB and comfortably below the shared
	// 32 MiB, so it separates the two and cannot be confused for either edge.
	const announced = maxProfileResponseBytes * 4
	require.Less(t, int64(announced), int64(covesoauth.DefaultMaxResponseBytes),
		"the fixture must declare a length the SHARED default would allow, or it proves nothing")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(announced))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":{}}`))
	}))
	t.Cleanup(server.Close)

	// The hatch is open because the fixture is on loopback. The byte cap is a
	// different control from the address guard.
	_, err := FetchProfileRecord(context.Background(), NewProfileBackfillClient(true),
		server.URL, "did:plc:profilebackfillcap22222")

	require.Error(t, err, "the declared length is not delivered, so this fetch fails either way")
	assert.ErrorIsf(t, err, covesoauth.ErrResponseTooLarge,
		"a PDS declaring %d bytes was not refused by the transport, so this client is running on "+
			"oauth.DefaultMaxResponseBytes (%d) rather than on maxProfileResponseBytes (%d) — its own "+
			"limit, thirty-two times smaller. The site's io.LimitReader bounds only what io.ReadAll "+
			"allocates; the announced-length refusal is the half that was dropped; got: %v",
		announced, covesoauth.DefaultMaxResponseBytes, maxProfileResponseBytes, err)
}

// TestNewProfileBackfillClient_AProfileAtTheCapStillArrives is what stops the
// cap from being tightened by accident, and it is why the number applied is
// maxProfileResponseBytes EXACTLY rather than the +1 the rematerialize and image
// proxy sites use.
//
// Those two read one byte PAST their own limit through an io.LimitReader so that
// an overrun is detected rather than silently truncated, and a transport cap set
// to exactly their limit would clip that probing byte. FetchProfileRecord reads
// exactly maxProfileResponseBytes and no more, so there is no probe to make room
// for — and a cap one byte above its own limit would be a number describing
// nothing.
func TestNewProfileBackfillClient_AProfileAtTheCapStillArrives(t *testing.T) {
	t.Parallel()

	// A well-formed record padded out to exactly the cap, so the boundary is
	// exercised by a response the caller must accept AND parse.
	prefix := `{"uri":"at://x","value":{"$type":"` + ProfileCollection + `","displayName":"`
	suffix := `"}}`
	padding := maxProfileResponseBytes - len(prefix) - len(suffix)
	require.Positive(t, padding, "the fixture must fit inside the cap it is testing")
	body := prefix + strings.Repeat("a", padding) + suffix
	require.Len(t, body, maxProfileResponseBytes, "the fixture must be EXACTLY the cap, not near it")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	_, err := FetchProfileRecord(context.Background(), NewProfileBackfillClient(true),
		server.URL, "did:plc:profilebackfillcap22222")

	assert.NoErrorf(t, err,
		"a response of exactly maxProfileResponseBytes (%d) was refused. The cap is this site's own "+
			"limit and a body at it is legitimate; a transport cap BELOW it would refuse profiles the "+
			"LimitReader was written to accept", maxProfileResponseBytes)
}
