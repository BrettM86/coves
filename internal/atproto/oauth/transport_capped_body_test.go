package oauth

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cappedBody at the unit level, for the two properties the end-to-end tests
// structurally cannot see.
//
// transport_body_cap_test.go drives everything through a real listener, which
// is the right level for a cap: it proves what a caller experiences. But two of
// the wrapper's decisions are invisible from there, and both were confirmed
// invisible by mutation:
//
//   - CLOSE. Deleting the delegation in cappedBody.Close leaves the whole suite
//     green, including TestSSRFSafeHTTPClient_UnderCapResponseReusesTheConnection,
//     which was written for exactly this. That test reads both bodies to EOF,
//     and http.Transport recycles a connection when the body is EXHAUSTED as
//     well as when it is closed — so the connection count it asserts on is
//     satisfied by the reads alone.
//   - THE STICKY exceeded FLAG. Deleting the branch leaves the suite green,
//     because every test stops reading at the first error. A caller that reads
//     again is the case the flag exists for, and nothing calls it.
//
// A fake ReadCloser is the only way to observe either. That makes these
// structural tests, which is a cost worth naming: they know cappedBody has a
// Close and a remaining counter, so a rewrite that changes the wrapper's shape
// has to change them too. It is the right trade only because the alternative is
// what the mutations found — no coverage at all.

// countingReadCloser records how many times it was closed and what Close
// returned, which is the whole of what the delegation test can observe.
type countingReadCloser struct {
	io.Reader
	closes   int
	closeErr error
}

func (c *countingReadCloser) Close() error {
	c.closes++
	return c.closeErr
}

func TestCappedBody_CloseReachesTheBodyUnderneath(t *testing.T) {
	t.Parallel()

	t.Run("close is delegated exactly once", func(t *testing.T) {
		t.Parallel()

		underlying := &countingReadCloser{Reader: bytes.NewReader(make([]byte, 8))}
		body := &cappedBody{body: underlying, remaining: testCap}

		require.NoError(t, body.Close(), "closing an ordinary body must not fail")

		assert.Equal(t, 1, underlying.closes,
			"cappedBody.Close closed the body underneath %d times, want 1. http.Transport returns a "+
				"connection to the idle pool when its body reports itself closed, so a wrapper that "+
				"implements Read and swallows Close leaks one connection per request while passing every "+
				"functional test — MaxIdleConns then describes a pool nothing is ever returned to",
			underlying.closes)
	})

	t.Run("close is delegated on a body that was never read", func(t *testing.T) {
		t.Parallel()

		// The path an early refusal takes: the caller closes without reading a
		// byte. Separate from the row above because a delegation that only
		// happens once something has been read is not a delegation.
		underlying := &countingReadCloser{Reader: bytes.NewReader(make([]byte, 8))}
		body := &cappedBody{body: underlying, remaining: testCap}

		require.NoError(t, body.Close(), "closing an unread body must not fail")
		assert.Equal(t, 1, underlying.closes,
			"a body that was never read must still be closed through to the connection underneath")
	})

	t.Run("close is delegated after the cap has fired", func(t *testing.T) {
		t.Parallel()

		// The case that matters most in production: an over-cap response is
		// precisely the one whose connection must not be leaked, because it is
		// the one an attacker can produce at will.
		underlying := &countingReadCloser{Reader: bytes.NewReader(make([]byte, 64))}
		body := &cappedBody{body: underlying, remaining: 8}

		_, readErr := io.ReadAll(body)
		require.ErrorIs(t, readErr, ErrResponseTooLarge, "the premise: this read must trip the cap")

		require.NoError(t, body.Close(), "closing after the cap fired must not fail")
		assert.Equal(t, 1, underlying.closes,
			"the connection behind an OVER-CAP response was not closed. That is the response an attacker "+
				"chooses to send, so a leak on this path is one they can repeat until the process runs out "+
				"of sockets")
	})

	t.Run("the underlying close error is not swallowed", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("the connection underneath failed to close")
		underlying := &countingReadCloser{Reader: bytes.NewReader(make([]byte, 8)), closeErr: sentinel}
		body := &cappedBody{body: underlying, remaining: testCap}

		assert.ErrorIs(t, body.Close(), sentinel,
			"cappedBody.Close discarded the error from the body underneath. A wrapper that always reports "+
				"success hides exactly the failures a caller checking Close is looking for")
	})
}

// TestCappedBody_TheFailureIsStickyAndIsNeverEOF pins the branch the whole cap
// is built to protect, and the sentinel it is built to report.
//
// # WHY STICKY
//
// A caller that reads again after the cap fires must not be told the stream
// ended cleanly. io.ReadAll treats io.EOF as the end of the body and returns a
// nil error, so a wrapper that failed once and then reported EOF would hand
// back a SHORT BODY WITH NO ERROR — which is the silent truncation this design
// rejected in favour of a sentinel. transport.go's ErrResponseTooLarge comment
// spells out what that costs downstream: a parser reads half a document, a size
// check never fires, a truncated image is written to a user's PDS as a whole
// one.
//
// # WHY NOTHING ELSE COVERS IT
//
// Every other test in the package stops at the first error, because that is
// what io.ReadAll does. The second read is the entire subject here, and
// deleting the sticky branch leaves the rest of the suite green.
func TestCappedBody_TheFailureIsStickyAndIsNeverEOF(t *testing.T) {
	t.Parallel()

	const limit = 8

	underlying := &countingReadCloser{Reader: bytes.NewReader(make([]byte, limit*8))}
	body := &cappedBody{body: underlying, remaining: limit}

	// First read: over the cap, so it fails. This much the end-to-end tests
	// already cover; it is the premise for what follows.
	first := make([]byte, limit*4)
	n, err := body.Read(first)
	require.ErrorIs(t, err, ErrResponseTooLarge,
		"the premise: reading %d bytes through a %d-byte cap must fail with ErrResponseTooLarge, and it "+
			"must be ASSERTABLE BY IDENTITY — nothing else in this package checks that sentinel, so a cap "+
			"that failed with any old error would look identical; got: %v", len(first), limit, err)
	require.GreaterOrEqual(t, n, 0, "io.Reader forbids a negative count")
	require.LessOrEqual(t, n, limit, "the failing read must not deliver more than the cap")

	// The subject. A caller that reads on — a decoder in a loop, a copy that
	// did not check — must be told the same thing again.
	second := make([]byte, limit*4)
	n, err = body.Read(second)

	assert.Equal(t, 0, n,
		"the read after the cap fired returned %d bytes. Once the limit is breached there is nothing more "+
			"this body may deliver", n)
	assert.ErrorIs(t, err, ErrResponseTooLarge,
		"the read after the cap fired did not report ErrResponseTooLarge; got: %v", err)
	assert.NotErrorIs(t, err, io.EOF,
		"the read after the cap fired reported io.EOF (possibly wrapped). io.ReadAll treats EOF as the "+
			"clean end of a body and returns a nil error, so a caller that reads again would come away "+
			"with a short body it has no way to know is short — which is the silent truncation this "+
			"sentinel exists to prevent, and is worse than having no cap at all; got: %v", err)

	// And again, because "sticky" means it does not decay. A flag cleared by a
	// later read would satisfy both assertions above.
	third := make([]byte, limit*4)
	n, err = body.Read(third)
	assert.Equal(t, 0, n, "the third read returned %d bytes; the failure must not decay", n)
	assert.ErrorIs(t, err, ErrResponseTooLarge,
		"the third read stopped reporting ErrResponseTooLarge, so the failure is not sticky but merely "+
			"delayed; got: %v", err)
}

// TestCappedBody_TheAllowanceIsSpentWhenTheCapFires closes the gap between the
// two fields that describe one state.
//
// exceeded and remaining are not independent: once the cap has fired the body
// will never deliver another byte, so an allowance that is still positive
// describes bytes that can never be spent. `(exceeded: true, remaining: 1024)`
// is representable and means nothing, and the only reason it is currently
// harmless is that Read consults exceeded FIRST. That is a read-order
// coincidence, not an invariant — the next edit to this wrapper (a Reset, a
// second reader, a metric that reports the unused allowance) reads remaining
// and gets a number that was true before the refusal and has been stale ever
// since.
//
// Zeroing it alongside the flag makes the illegal state unrepresentable, which
// is the cheaper half of the two ways to fix this. The other is collapsing the
// pair into one field; the flag is kept because ErrResponseTooLarge must stay
// distinguishable from "the allowance happened to land on zero".
func TestCappedBody_TheAllowanceIsSpentWhenTheCapFires(t *testing.T) {
	t.Parallel()

	const limit = 8

	underlying := &countingReadCloser{Reader: bytes.NewReader(make([]byte, limit*8))}
	body := &cappedBody{body: underlying, remaining: limit}

	// A read far larger than the allowance, so the refusal happens with plenty
	// of it notionally unspent — the shape in which a stale counter is most
	// obviously wrong.
	_, err := body.Read(make([]byte, limit*4))
	require.ErrorIs(t, err, ErrResponseTooLarge,
		"the premise: reading %d bytes through a %d-byte cap must trip the cap; got: %v",
		limit*4, limit, err)

	require.True(t, body.exceeded, "the premise: the refusal must have set the sticky flag")
	assert.Zero(t, body.remaining,
		"the cap fired and left an allowance of %d bytes behind. The two fields describe one state, so a "+
			"positive remaining alongside exceeded is a number about bytes this body will never deliver — "+
			"harmless only for as long as Read keeps checking the flag before the counter, which is an "+
			"ordering coincidence rather than an invariant", body.remaining)
}

// TestCappedBody_AtTheCapTheBodyArrivesWhole is the boundary at the unit level,
// and it is the fence around every assertion above: a wrapper that failed
// eagerly would satisfy all of them.
//
// The end-to-end boundary test owns the same property through a real listener.
// This one owns it against a reader that cannot be affected by framing,
// buffering or chunk sizes, so a failure here names the wrapper and nothing
// else.
func TestCappedBody_AtTheCapTheBodyArrivesWhole(t *testing.T) {
	t.Parallel()

	const limit = 8

	underlying := &countingReadCloser{Reader: bytes.NewReader(bytes.Repeat([]byte{'x'}, limit))}
	body := &cappedBody{body: underlying, remaining: limit}

	got, err := io.ReadAll(body)

	require.NoError(t, err,
		"a body of exactly the cap must read cleanly. Reading one byte past the allowance is how the "+
			"boundary is detected, and that extra read must find EOF rather than be mistaken for an "+
			"overflow; got: %v", err)
	assert.Len(t, got, limit, "a body at exactly the cap must arrive complete, not one byte short")
}
