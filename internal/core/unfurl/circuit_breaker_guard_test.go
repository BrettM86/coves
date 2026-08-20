package unfurl

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// The circuit breaker, and the one class of error it must never count.
//
// # THE DEFECT THESE TESTS WERE WRITTEN AGAINST
//
// UnfurlURL fed every fetch error into recordFailure, including the SSRF guard's
// refusal. The breaker opens after 3 consecutive failures and stays open for 5
// minutes, and its key for the OpenGraph path is the constant string
// "opengraph" — one bucket for the whole instance, not one per host. So three
// pasted `http://127.0.0.1/` links disabled link previews SITE-WIDE for five
// minutes, for every user, repeatably, at the cost of three posts.
//
// That is a denial of service delivered THROUGH the security control: the guard
// does its job perfectly on each request and the aggregate is an outage. It is
// also a mis-attribution — the breaker exists to stop hammering a provider that
// is failing, and a refused address says nothing about whether the provider is
// up. The log line it writes names a healthy provider as the failing party.
//
// # WHY THE CHECK LIVES IN recordFailure AND NOT AT THE CALL SITES
//
// UnfurlURL calls recordFailure from THREE places — kagi, oEmbed and OpenGraph —
// a few lines apart, each looking like the one above it. That is the exact shape
// the three-client defect in providers.go took, and converting two of three is
// how it survives review. One check at the single point where a failure is
// counted cannot be forgotten by the fourth path someone adds.

// alwaysMissRepo is a cache that never hits and never stores, so every UnfurlURL
// call below reaches the fetch. Nothing here is about caching.
type alwaysMissRepo struct{}

func (alwaysMissRepo) Get(context.Context, string) (*UnfurlResult, error) { return nil, nil }

func (alwaysMissRepo) Set(context.Context, string, *UnfurlResult, time.Duration) error { return nil }

// TestUnfurlService_AGuardRefusalDoesNotOpenTheCircuit is the binding assertion,
// stated the way a user experiences it.
//
// The count is failureThreshold+1 refusals and then one more: the breaker opens
// AT the threshold, so the assertion has to be made on a request that comes
// after it would have opened. What that request must come back with is the
// guard's own refusal — proof it was ATTEMPTED — rather than a breaker error
// naming a provider nobody has spoken to.
func TestUnfurlService_AGuardRefusalDoesNotOpenTheCircuit(t *testing.T) {
	t.Parallel()

	// The service as production builds it: PrivateHostOptions(false), so the
	// guard is on and loopback is refused before a packet leaves the process.
	svc, ok := NewService(alwaysMissRepo{}, PrivateHostOptions(false)...).(*service)
	require.True(t, ok, "NewService must return the concrete *service this package's tests drive")

	threshold := svc.circuitBreaker.failureThreshold
	require.Positive(t, threshold, "the premise: the breaker must have a threshold to trip")

	// Distinct paths on the same host, because the breaker's bucket is the
	// PROVIDER ("opengraph"), not the URL — which is precisely why three links
	// take out previews for everyone.
	var lastErr error
	for i := 0; i <= threshold; i++ {
		//nolint:noctx // the guard refuses before any dial; the context is irrelevant here
		_, lastErr = svc.UnfurlURL(context.Background(),
			fmt.Sprintf("http://127.0.0.1/%d", i)) // coves:allow-host-literal: the address the guard must refuse, and the payload of the DoS

		require.Errorf(t, lastErr, "attempt %d against a loopback address must be refused", i+1)
		require.ErrorIsf(t, lastErr, covesoauth.ErrBlockedAddress,
			"attempt %d failed for a reason other than the guard, so this test is measuring the wrong "+
				"thing; got: %v", i+1, lastErr)
	}

	// The subject. This request is one past the threshold, so under the defect
	// the breaker is open and the error names a provider instead of an address.
	assert.NotContains(t, lastErr.Error(), "circuit breaker open",
		"after %d guard refusals the OpenGraph circuit is open, so link previews are disabled "+
			"INSTANCE-WIDE for %s — for every user, at the cost of %d pasted loopback links, repeatable "+
			"on expiry. A refusal is the guard working; counting it as provider failure turns the "+
			"security control into the outage",
		threshold+1, svc.circuitBreaker.openDuration, threshold)

	canAttempt, breakerErr := svc.circuitBreaker.canAttempt("opengraph")
	assert.Truef(t, canAttempt,
		"the OpenGraph circuit refuses further attempts after %d refused ADDRESSES. The breaker exists "+
			"to stop hammering a provider that is failing, and nothing here has learned anything about "+
			"whether opengraph is up: not one packet left the process; got: %v", threshold+1, breakerErr)
}

// TestUnfurlService_AGenuineProviderFailureStillOpensTheCircuit is the fence.
//
// The cheapest wrong fix for the test above is to stop recording failures, or to
// widen the exclusion until it swallows the timeouts and 5xx the breaker was
// built for. This is the behaviour that must survive: a provider that really is
// failing still gets cut off.
func TestUnfurlService_AGenuineProviderFailureStillOpensTheCircuit(t *testing.T) {
	t.Parallel()

	breaker := newCircuitBreaker()
	for i := 0; i < breaker.failureThreshold; i++ {
		breaker.recordFailure("opengraph", fmt.Errorf("HTTP request returned status 503"))
	}

	canAttempt, err := breaker.canAttempt("opengraph")
	assert.Falsef(t, canAttempt,
		"%d consecutive 503s left the circuit closed. Excluding guard refusals must not disarm the "+
			"breaker for the failures it was built for — a provider answering 503 in a loop is exactly "+
			"what it stops hammering", breaker.failureThreshold)
	require.Error(t, err, "an open circuit must explain itself to the caller")
	assert.Contains(t, err.Error(), "circuit breaker open",
		"the open circuit must keep the message UnfurlURL logs and the tests above assert the ABSENCE "+
			"of; got: %v", err)
}

// TestCircuitBreaker_AWrappedGuardRefusalIsRecognised pins the matching
// mechanism at the unit level.
//
// The refusal that arrives at recordFailure has been through http.Client.Do,
// which wraps it in a *url.Error, and then through UnfurlURL's own
// fmt.Errorf("failed to fetch OpenGraph data: %w"). A check comparing errors
// with == or reading err.Error() would pass against a bare sentinel and fail
// against every real one, so the test hands it the shape production produces.
func TestCircuitBreaker_AWrappedGuardRefusalIsRecognised(t *testing.T) {
	t.Parallel()

	breaker := newCircuitBreaker()
	wrapped := fmt.Errorf("failed to fetch OpenGraph data: %w",
		fmt.Errorf(`Get "http://127.0.0.1/": %w`, // coves:allow-host-literal: the *url.Error wrapping production produces
			&covesoauth.BlockedAddressError{Host: "127.0.0.1"})) // coves:allow-host-literal: the refused host, carried on the typed error

	require.ErrorIs(t, wrapped, covesoauth.ErrBlockedAddress,
		"the premise: the fixture must be a wrapped guard refusal")

	for i := 0; i < breaker.failureThreshold*2; i++ {
		breaker.recordFailure("opengraph", wrapped)
	}

	canAttempt, err := breaker.canAttempt("opengraph")
	assert.Truef(t, canAttempt,
		"%d wrapped guard refusals opened the circuit. The refusal reaches recordFailure through a "+
			"*url.Error and one fmt.Errorf, so the exclusion has to be errors.Is and not an identity or "+
			"substring comparison — either of those passes on a bare sentinel and fails on every "+
			"refusal production actually produces; got: %v", breaker.failureThreshold*2, err)

	// Not merely "the circuit stayed closed" — the counter must never have moved.
	// A breaker that counted refusals but refused to open on them would satisfy
	// the assertion above and still cut off a provider on its first REAL failure
	// after a run of refusals, because the count would already be at the
	// threshold's door.
	assert.Zerof(t, breaker.failures["opengraph"],
		"the breaker recorded %d failures against 'opengraph' for refusals that never left the process. "+
			"A leftover count means the next genuine failure trips a breaker it did not earn",
		breaker.failures["opengraph"])
}
