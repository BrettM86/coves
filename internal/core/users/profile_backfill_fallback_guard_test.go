package users

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// The hole in the work that just shipped.
//
// # WHAT WAS ACTUALLY CLOSED
//
// NewProfileBackfillClient is the guard boundary used by
// cmd/server/wiring.go at it. What it guarded was the client wiring INJECTS.
// WithProfileBackfill still ends with
//
//	if client == nil {
//		client = &http.Client{Timeout: 10 * time.Second}
//	}
//
// so the service has TWO ways to acquire a backfill client and only one of them
// goes past the gate. Anything that enables backfill without handing over a
// client — a second wiring path, an integration harness, a future caller
// copying the option's own documented "Pass nil to use a default client" —
// silently gets an unguarded one, at a call site whose whole job is to fetch an
// address a stranger chose.
//
// # WHY A NIL DEFAULT IS THE WORST PLACE FOR THIS
//
// The guarded path is the one a reader checks. `WithProfileBackfill(client)`
// with a real argument reads as guarded, because the thing being passed in was
// built by the gate — and the reader stops there, having answered the question.
// The nil branch is not a call site anyone reviews; it is what a call site
// DEGRADES TO. So the failure mode is not "someone wired this wrongly", it is
// "someone wired this in the way the doc comment suggests".
//
// The severity is unchanged from the injected path — see
// profile_backfill_guard_test.go for what `pdsURL` is and why nothing observes
// this goroutine — so these tests do not restate it. They pin one claim: THE
// FALLBACK IS THE GATE'S OWN CLIENT, not a second client that happens to exist.

// TestWithProfileBackfill_NilYieldsAGuardedClient is the binding contract.
//
// It drives FetchProfileRecord with whatever the option left on the service,
// rather than inspecting the client, because "guarded" is a behaviour: a field
// assertion would pass against any client built by any constructor that happened
// to look right.
func TestWithProfileBackfill_NilYieldsAGuardedClient(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	service := &userService{}
	WithProfileBackfill(nil)(service)
	require.NotNil(t, service.profileBackfillClient,
		"WithProfileBackfill(nil) must still enable backfill — a nil client here disables the "+
			"feature silently instead of defaulting it")

	_, err := FetchProfileRecord(context.Background(),
		service.profileBackfillClient, pds.server.URL, backfillGuardDID)

	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times through the NIL-argument path. The explicit guard "+
			"covers only the client wiring injects; this branch hands back a bare "+
			"http.Client and the PDS URL it dials comes from an indexed user's record",
		pds.requests.Load())

	require.Error(t, err,
		"a service configured with WithProfileBackfill(nil) fetched a loopback address "+
			"successfully. Nothing waits on this goroutine in production, so this would leave no "+
			"trace at all")

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the fetch failed, but not because the guard refused the address. Without the guard's own "+
			"identity this assertion would pass against the current build, where the fallback is "+
			"`&http.Client{Timeout: 10 * time.Second}` and the failure is merely a PDS that could "+
			"not be reached; got: %v", err)
}

// TestWithProfileBackfill_NilFallbackIsTheGatesOwnClient ties the fallback to
// the constructor whose classification pass is already pinned.
//
// # WHY A BEHAVIOURAL TEST IS NOT ENOUGH ON ITS OWN
//
// The case above proves the fallback refuses a loopback LITERAL. A client that
// refuses every address refuses that one too, and so does one whose transport
// was hand-rolled to check `IsLoopback` and nothing else — neither of which
// would refuse a name that RESOLVES to 169.254.169.254, which is the address
// this whole remediation exists for.
//
// The seam that proves classification (covesoauth.WithHostResolver) cannot be
// pushed through WithProfileBackfill: the option takes a whole client and there
// is nothing to pass options to. So this test makes the bridge instead — the
// fallback must be built by NewProfileBackfillClient, whose classification is
// pinned by TestFetchProfileRecord_RefusesAWellFormedHostThatResolvesPrivate
// and its hatch-open control. The transport TYPE is the observable that shows
// which constructor built it; a bare http.Client carries a nil Transport, and
// stdlib's default carries *http.Transport.
func TestWithProfileBackfill_NilFallbackIsTheGatesOwnClient(t *testing.T) {
	t.Parallel()

	service := &userService{}
	WithProfileBackfill(nil)(service)
	require.NotNil(t, service.profileBackfillClient, "WithProfileBackfill(nil) must leave a client")

	fallback := reflect.TypeOf(service.profileBackfillClient.Transport)
	gated := reflect.TypeOf(NewProfileBackfillClient(false).Transport)

	require.NotNilf(t, service.profileBackfillClient.Transport,
		"the fallback client has a nil Transport, which is stdlib's DefaultTransport — an "+
			"unguarded client wearing no marking at all")
	assert.Equalf(t, gated, fallback,
		"the nil fallback is a %v, not the %v NewProfileBackfillClient(false) builds. Only the "+
			"gate's transport carries the classification pass; a second client that merely refuses "+
			"loopback would satisfy the behavioural test above and still dial 169.254.169.254",
		fallback, gated)
}

// TestWithProfileBackfill_NilFallbackKeepsTheBackfillDeadline pins the value the
// conversion is most likely to lose.
//
// The shared SSRF client ships a 15s ceiling. backfillProfile detaches from its
// caller with context.WithoutCancel, so profileBackfillTimeout is the ONLY
// deadline this goroutine has — adopting the gate without re-applying it (which
// NewProfileBackfillClient already does) would silently extend every backfill
// started through the nil path, with no request lifetime underneath to bound it.
func TestWithProfileBackfill_NilFallbackKeepsTheBackfillDeadline(t *testing.T) {
	t.Parallel()

	service := &userService{}
	WithProfileBackfill(nil)(service)
	require.NotNil(t, service.profileBackfillClient, "WithProfileBackfill(nil) must leave a client")

	assert.Equalf(t, profileBackfillTimeout, service.profileBackfillClient.Timeout,
		"the nil-fallback client runs on a %v timeout instead of profileBackfillTimeout (%v). This "+
			"goroutine is detached from its caller's context, so this value is its only deadline",
		service.profileBackfillClient.Timeout, profileBackfillTimeout)
}

// TestWithProfileBackfill_KeepsTheInjectedClient is the falsifiability control,
// and it is not a formality.
//
// The cheapest way to make every assertion above pass is to ignore the argument
// and always build the gate's guarded client — which would delete the dev hatch
// wiring depends on, and every httptest-backed fixture with it, since loopback
// is exactly what the guard refuses. The claim is pointer identity: an injected
// client must arrive at the field UNTOUCHED, so the fallback is reachable only
// when there is genuinely nothing to fall back from.
func TestWithProfileBackfill_KeepsTheInjectedClient(t *testing.T) {
	t.Parallel()

	injected := NewProfileBackfillClient(true)

	service := &userService{}
	WithProfileBackfill(injected)(service)

	assert.Samef(t, injected, service.profileBackfillClient,
		"WithProfileBackfill replaced the client it was handed. cmd/server passes the gate's "+
			"output — including the DEV build, where the hatch is open — so overriding the "+
			"argument would guard the fallback by making every caller's own choice unreachable")
}

// TestWithProfileBackfill_TheInjectedHatchStillReachesThePDS is the other half
// of that control, stated as behaviour rather than as a pointer.
//
// Pointer identity would still hold if a later change copied the client and
// rebuilt its transport. This is the property that actually matters to a
// developer's local stack: an injected hatch-open client reaches loopback.
func TestWithProfileBackfill_TheInjectedHatchStillReachesThePDS(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	service := &userService{}
	WithProfileBackfill(NewProfileBackfillClient(true))(service)

	input, err := FetchProfileRecord(context.Background(),
		service.profileBackfillClient, pds.server.URL, backfillGuardDID)

	require.NoErrorf(t, err,
		"an injected hatch-open client must still reach a loopback PDS — this is what every dev "+
			"stack and every httptest fixture in this tree depends on; got: %v", err)
	require.NotNil(t, input, "the fixture serves a displayName, so a profile must come back")
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once", pds.requests.Load())
}
