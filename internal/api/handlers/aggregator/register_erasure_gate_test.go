package aggregator

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// An erased account does not come back through this endpoint, not even for the
// operator who really does control its handle.
//
// # WHY THE GATE IS HERE AND NOT ONLY IN THE REPOSITORY
//
// Deleting a user writes a deleted_accounts marker (migration 036), and the
// firehose consumers read it: it is what stops a replayed post rematerialising
// content the AppView was asked to forget. Every ingestion path already refuses
// a DID the marker names. Registration was the exception — it reached the
// repository's insert, which cleared the marker on its way past — so the one
// unauthenticated endpoint in the domain was also the only exit from erasure.
//
// The refusal belongs in the handler rather than being left to the repository
// statement because the two say different things. "The insert no longer clears
// the marker" would leave a registration half-succeeding: a users row for an
// erased DID whose content the consumers still drop, which is a state nothing
// in the product knows how to read. Refusing outright is the only answer that
// leaves the database in a shape someone can reason about, and it is the answer
// a caller can act on.
//
// This is the T0 statement of it. register_erasure_test.go is the same property
// asserted against a real database, where the marker itself can be looked at;
// here the point is narrower and sharper — WHICH refusal, and that CreateUser is
// never reached.
func TestRegister_RefusesToRegisterAnErasedAccount(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	// Everything else about this registration is in order: the domain publishes
	// the DID, and the DID's handle is that domain. The erasure is the only
	// reason to refuse, which is what makes this the test for the erasure gate
	// rather than for the handle check next door.
	f.users.erased[registrantDID] = true

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusForbidden, "AccountErased")

	require.Emptyf(t, f.users.created,
		"registration created a users row for %s, an erased DID. A row for an account whose content "+
			"every consumer still drops is a state nothing in the product knows how to read", registrantDID)

	// AND THE OWNERSHIP CHECK RAN FIRST, which is what stops this refusal being
	// an oracle. "AccountErased" is a fact about somebody else's account, and
	// answering it to a caller who has proven nothing turns the endpoint into a
	// way to ask "was this DID deleted here?" about any DID at all. Making the
	// caller prove the domain first means only someone who already controls the
	// account can learn it — which is someone entitled to know.
	require.NotEmptyf(t, f.stub.fetched(),
		"registration answered AccountErased for %s without ever fetching .well-known. The erasure "+
			"gate must run AFTER domain ownership is proven: answered before, it tells any stranger "+
			"which DIDs this instance was asked to erase", registrantDID)
}

// The erasure of an account is not something to tell a caller who has not
// proven they are it.
//
// This is the same erased DID as the test above, and the only difference is
// that the caller's domain does not serve it — so ownership fails. The answer
// must be the ownership failure, not the erasure: a handler that checked the
// marker first would answer AccountErased here, and an attacker could then
// enumerate deleted accounts by registering DIDs against a domain they own and
// reading which ones come back 403 instead of 401.
//
// Nothing about the erasure is secret from the account holder. It is secret
// from everyone else, and "everyone else" is exactly who fails this check.
func TestRegister_DoesNotRevealAnErasureToACallerWhoProvesNothing(t *testing.T) {
	// The stub answers, and does not serve this DID — an attacker's own domain.
	f := newRegisterFixture(t, http.NotFoundHandler())
	f.users.erased[registrantDID] = true

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusUnauthorized, "DomainVerificationFailed")

	require.Empty(t, f.users.created,
		"a registration that failed domain verification must not create anyone, erased or not")
}

// A lookup that FAILS is not a lookup that answered "no".
//
// The gate reads a table, and reading it can fail — a dead connection, a
// statement timeout. If that failure were treated as "not erased", the endpoint
// would register erased accounts precisely when the database was unhealthy,
// which is both the worst moment to find out and the moment nobody is watching.
// So the failure is a 500: the caller is told to try again, and nothing is
// written.
//
// 500 rather than 403 because the two mean opposite things to the caller. 403
// says "you may not, and retrying will not help"; this caller may well be
// entitled to register, and the server simply does not know yet.
func TestRegister_RefusesWhenTheErasureLookupFails(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	f.users.erasedErr = errors.New("the deleted_accounts table could not be read")

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusInternalServerError, "RegistrationFailed")

	require.Empty(t, f.users.created,
		"the erasure marker could not be read, so whether this DID was erased is unknown — "+
			"and an unknown answer must not register anyone")
}
