package aggregator

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// A lookup that BROKE is not a lookup that found nothing, and registration is
// the place where confusing the two costs the most.
//
// The already-registered check exists to turn a second registration into a 409
// rather than a silent no-op — CreateUser is idempotent, so without it a caller
// re-registering would get a 200 and no way to tell. Reading the error as
// "there is no such user" inverts that: a database blip makes the endpoint fall
// through to CreateUser for a DID it could not check, and the one guard against
// registering over an existing aggregator is gone precisely while the database
// is unhealthy.
//
// 500 is the answer because the server does not know. The caller may well be
// entitled to register, and a 409 would tell them to stop over a fact nobody
// established.
func TestRegister_RefusesWhenTheRegistrationLookupFails(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	// Everything ahead of the lookup succeeds: the DID resolves, the handle is
	// the domain, the account is not erased, and the domain serves the DID. The
	// broken lookup is the only reason to refuse.
	f.users.getErr = errors.New("the users table could not be read")

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusInternalServerError, "RegistrationFailed")

	require.Emptyf(t, f.users.created,
		"registration could not tell whether %s was already registered and created it anyway. "+
			"ErrUserNotFound means 'nobody has this DID'; any other error means the question was "+
			"never answered, and only the first of those is a reason to proceed", registrantDID)
}

// Registration must read the directory, not a memory of it.
//
// # WHY A CACHED ANSWER IS THE WRONG ONE HERE
//
// The resolver in front of the PLC directory caches, which is right for the
// reads that made it worth having: a feed hydrating a hundred authors should
// not make a hundred directory calls, and a handle that changed an hour ago is
// a cosmetic staleness.
//
// This call site is different, because the cached value is the credential. The
// handle decides whether the caller may register this DID at all, and handles
// move: an account transfers to a new domain, or a stale entry names one the
// caller has since acquired. Either way a registration decided on a cached
// handle is decided on a claim the directory no longer makes, and the endpoint
// is unauthenticated, so the attacker chooses when to ask.
//
// The cost of being wrong is asymmetric and that is what settles it. Reading
// fresh costs one directory call on an endpoint rate-limited to ten requests
// per ten minutes. Reading stale hands an aggregator identity to whoever the
// handle used to point at.
func TestRegister_ResolvesTheDIDFreshRatherThanFromCache(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	rec := f.register(t, registrantDID, f.domain)
	require.Equalf(t, http.StatusOK, rec.Code, "the fixture must register successfully: %s", rec.Body.String())

	// The ORDER is the whole assertion. A purge after the resolve drops a cache
	// entry the handler has already trusted, which is housekeeping for the next
	// caller and does nothing for this one.
	require.Equalf(t, []string{"purge:" + registrantDID, "resolve:" + registrantDID}, f.resolver.calls,
		"registration asked the resolver for %v. The cached handle is what decides whether this "+
			"caller may register this DID, so it has to be dropped BEFORE the lookup — a resolve that "+
			"can be answered from cache is a registration decided on a claim the directory may no "+
			"longer make", f.resolver.calls)
}

// A cache that could not be dropped means the next answer might be stale, and
// registration cannot tell whether it is.
//
// Proceeding would be reading the cache after deciding not to trust it. The
// refusal is a 500 rather than a 4xx because nothing is wrong with the request:
// the server could not put itself in a position to answer, and a retry may well
// succeed.
func TestRegister_RefusesWhenTheIdentityCacheCannotBeDropped(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	f.resolver.purgeErr = errors.New("the identity cache is unreachable")

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusInternalServerError, "RegistrationFailed")

	require.Emptyf(t, f.users.created,
		"the identity cache could not be dropped, so the handle this registration would be decided on "+
			"may be one the directory no longer serves — and %s was registered on it anyway", registrantDID)
	require.Empty(t, f.stub.fetched(),
		"and nothing may be fetched on behalf of an identity the handler cannot resolve freshly")
}
