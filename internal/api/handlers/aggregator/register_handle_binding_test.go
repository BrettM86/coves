package aggregator

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Proving a domain says nothing about a DID unless the domain IS the DID's
// handle.
//
// # WHY ONE DIRECTION IS NOT ENOUGH
//
// Registration is unauthenticated, and asking "does
// https://<the domain you named>/.well-known/atproto-did serve the DID you
// named?" is a real question that authorizes nothing on its own. A DID document
// lists its handle, and any number of OTHER domains may publish that DID in
// their .well-known — an attacker's own among them, since nothing stops anyone
// writing someone else's DID into a file they serve. That check alone proves
// the caller controls A domain mentioning the DID, not THE domain the DID
// answers to.
//
// The other direction carries the evidence: resolve the DID and require that
// the handle it resolves to is the domain being proven. Both together are
// atProto's bidirectional handle verification, and only both make "I control
// this domain" into "I am this account".
//
// This is the T0 statement of that rule; register_erasure_test.go is the same
// rule seen through its consequence, against a real database.
func TestRegister_RefusesADomainThatIsNotTheDIDsHandle(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	// The domain genuinely publishes the DID — the fixture's .well-known serves
	// it, so the ownership half passes completely. The DID answers to somewhere
	// else entirely, which is the whole of the attack: the caller proved control
	// of a domain that has nothing to do with the account.
	f.resolver.identities[registrantDID].Handle = "somewhere.else.dev"

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusForbidden, "HandleMismatch")

	// AND NOTHING WAS FETCHED. The handle check runs before the .well-known
	// request, and that ordering is worth pinning: an AppView that fetched first
	// would make an outbound request, chosen by an unauthenticated stranger, on
	// behalf of a DID it has already decided it will not serve.
	require.Emptyf(t, f.stub.fetched(),
		"the handler fetched %v before refusing a domain that is not the DID's handle. There is "+
			"nothing to verify about a domain the account does not claim, and making the request "+
			"anyway hands a stranger an outbound fetch for free", f.stub.fetched())

	require.Emptyf(t, f.users.created,
		"a caller who proved %q registered the DID %s, whose handle is %q. Publishing someone "+
			"else's DID in your own .well-known is free; being their handle is not, and only the "+
			"second one is evidence of anything",
		f.domain, registrantDID, "somewhere.else.dev")
}

// The comparison is on names, and DNS does not distinguish case, so neither may
// this.
//
// The domain arrives lowercased — validation.NormalizeDomain does that — but the
// handle comes from DID resolution and is whatever the directory says, so the
// two sides can differ in case with nothing wrong. A case-sensitive comparison
// would refuse a legitimate registrant with a 403 naming an attack they did not
// attempt, and the refusal would be invisible to every other test here because
// the fixtures all happen to be lowercase.
func TestRegister_TreatsHandleAndDomainCaseInsensitively(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	// The same name the caller proves, differing only in case.
	f.resolver.identities[registrantDID].Handle = "EXAMPLE.COM"

	rec := f.register(t, registrantDID, f.domain)

	require.Equalf(t, http.StatusOK, rec.Code,
		"the handle %q and the domain %q are the same name — DNS is case-insensitive — so this is "+
			"not a HandleMismatch and must register normally. Compare with strings.EqualFold, not ==. "+
			"Body: %s",
		"EXAMPLE.COM", f.domain, rec.Body.String())
	require.Len(t, f.users.created, 1, "a case-only difference must not stop the registration")
}

// A handle that is not a name must never match a domain, and "handle.invalid"
// is the one that can.
//
// atProto's own convention is that an identity whose handle fails bidirectional
// verification is reported as the literal `handle.invalid` — Indigo's directory
// returns exactly that rather than an error, and this AppView's OAuth callback
// checks for it by name. So `handle.invalid` is not a handle; it is a
// resolver's way of saying it could not establish one.
//
// It is also a syntactically valid domain. A registrant can own it in the sense
// this endpoint cares about — two labels, an alphabetic TLD, and a .well-known
// they choose the contents of — so a plain string comparison would accept
// `handle.invalid` as proof of an identity the directory just said it could not
// establish. That is the whole hole: the one value that means "no handle" is
// the one value an attacker can make the comparison agree with.
//
// The empty handle is the same failure with a different spelling, and is here
// because a resolver that returns "" instead is not a hypothetical — it is what
// a DID document with no alsoKnownAs produces.
func TestRegister_RefusesAHandleThatIsNotAName(t *testing.T) {
	for _, tt := range []struct {
		name   string
		handle string
	}{
		{name: "the handle.invalid sentinel", handle: "handle.invalid"},
		{name: "no handle at all", handle: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRegisterFixture(t, wellKnownServing(registrantDID))
			f.resolver.identities[registrantDID].Handle = tt.handle

			// The domain the caller claims is the sentinel itself, which is the
			// only input that makes the comparison a question at all.
			rec := f.register(t, registrantDID, "handle.invalid")
			assertRegisterError(t, rec, http.StatusForbidden, "HandleMismatch")

			require.Emptyf(t, f.users.created,
				"a DID whose handle is %q was registered. The directory could not establish a handle "+
					"for this account, so there is no name for the caller's domain to match and nothing "+
					"here proves anything about who they are", tt.handle)
			require.Empty(t, f.stub.fetched(),
				"and nothing may be fetched on behalf of an identity that has no established handle")
		})
	}
}
