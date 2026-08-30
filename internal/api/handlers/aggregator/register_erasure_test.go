//go:build integration

package aggregator

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Erasure is the one promise this AppView makes that it cannot take back, and
// this is the test that registration cannot take it back either.
//
// # WHAT THE MARKER IS FOR
//
// Deleting a user writes a row into deleted_accounts (migration 036) in the
// same transaction that erases the content. That marker is not bookkeeping: the
// firehose consumers read it, and it is the only thing standing between a
// redriven post event — an acceptance replayed months later, a relay backfill —
// and the erased account's content being written back into this database. Every
// legitimate indexing path refuses a DID the marker names.
//
// # WHY A DATABASE, AND WHY THIS PACKAGE
//
// register_test.go proves the endpoint against a fake user service, and a fake
// cannot show this at all: the marker is a row, and everything that could
// remove it lives below anything a fake stands in for. A test asserting "the
// handler did not call CreateUser" would be naming one particular way to get
// this wrong rather than the property, and would go on passing if some other
// call learned to clear a marker. So this test asks the database directly, from
// outside every unit involved: after a registration attempt, is the marker
// still there?
//
// The endpoint is unauthenticated — anyone who controls an HTTPS domain can POST
// to it — so the caller below is a stranger with a domain and someone else's
// DID. That is the shape of the attack this pins closed: clear the marker, then
// wait for the firehose to rematerialise what the account was promised was
// gone.
func TestRegister_CannotUndoAnAccountErasure(t *testing.T) {
	t.Parallel()

	// The attacker's case. The stub domain publishes the erased DID, which is
	// the half of the proof an attacker can manufacture; the DID resolves to a
	// handle on an entirely different domain, so the caller has proven control
	// of nothing that belongs to the account being registered.
	t.Run("a domain that is not the DID's handle cannot clear the marker", func(t *testing.T) {
		t.Parallel()

		// What matters about this handle is only that it is under a different
		// registrable domain than the one being proven: it is the victim's
		// name, not the attacker's.
		erasedHandle := "victim-" + testkit.UniqueID(t) + ".test.coves.dev"
		assertRegistrationCannotReinstate(t, stubDomain, erasedHandle, "HandleMismatch")
	})

	// The narrower case, and the one that says the erasure gate is a gate
	// rather than a side effect of the handle check: here the caller really
	// does control the domain the DID resolves to. Domain ownership is proven,
	// the handle matches, and registration must STILL refuse — an erased
	// account does not come back through an unauthenticated endpoint, not even
	// for its own operator.
	t.Run("the DID's own proven handle cannot clear the marker", func(t *testing.T) {
		t.Parallel()

		// One label under example.com, because that is what the certificate
		// httptest mints covers (SANs example.com and *.example.com — see
		// stubDomain), and run-unique because the assertion below is about the
		// row THIS test erased.
		provenDomain := testkit.UniqueIDWithPrefix(t, "agg") + "." + stubDomain
		assertRegistrationCannotReinstate(t, provenDomain, provenDomain, wantErasureRefusal)
	})
}

// wantErasureRefusal is the code the second case expects: the caller cleared
// every ownership bar registration sets and is refused on the erasure alone.
const wantErasureRefusal = "AccountErased"

// assertRegistrationCannotReinstate erases an account, then posts a
// registration for its DID against a domain that really does publish it, and
// asserts the three things that together mean the erasure held: the request was
// refused, the marker is still in the table, and no users row came back.
//
// domain is the name the caller proves ownership of AND the name the stub
// answers as; resolvedHandle is what the DID resolver reports for the DID,
// which is what makes the difference between the two cases above.
func assertRegistrationCannotReinstate(t *testing.T, domain, resolvedHandle, wantCode string) {
	t.Helper()

	db := testkit.DB(t)
	ctx := context.Background()

	// Run-scoped, because "the marker is still present" is only meaningful for
	// a DID this test is the sole author of.
	did := "did:plc:" + testkit.UniqueIDWithPrefix(t, "erased")
	pdsURL := "https://pds.example.invalid"

	userRepo := postgres.NewUserRepository(db)

	// The account existed and was erased, which is the only way a marker gets
	// written: Delete refuses a DID with no users row, and the marker is
	// written inside that same transaction.
	_, err := userRepo.Create(ctx, &users.User{DID: did, Handle: resolvedHandle, PDSURL: pdsURL})
	require.NoError(t, err, "seeding the account that will be erased")
	require.NoError(t, userRepo.Delete(ctx, did), "erasing the account")
	require.True(t, erasureMarkerExists(t, db, did),
		"the erasure marker was not written, so this test would prove nothing about preserving it")

	// The resolver stands in for the PLC directory, and is the ONLY source of
	// the handle: a handler that took the handle from the request body would
	// let the caller name its own.
	resolver := &fakeIdentityResolver{identities: map[string]*identity.Identity{
		did: {DID: did, Handle: resolvedHandle, PDSURL: pdsURL, ResolvedAt: time.Now(), Method: identity.MethodHTTPS},
	}}

	// The real service over the real repository, because the statement that
	// clears the marker lives in the repository and nothing above it can be
	// faked without hiding the behaviour under test.
	userService := users.NewUserService(userRepo, resolver, pdsURL, nil, "")

	// The stub answers for this one name and 404s everything else, so a
	// verification aimed at any other host proves nothing — see boundStub.
	stub := httptest.NewTLSServer(&boundStub{host: domain, next: wellKnownServing(did)})
	t.Cleanup(stub.Close)

	handler := NewRegisterHandler(userService, resolver)
	handler.setHTTPClient(stubClient(t, stub))

	body, err := json.Marshal(RegisterRequest{DID: did, Domain: domain})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.HandleRegister(rec, req)

	// The status and code are asserted, but they are the diagnosis rather than
	// the finding: a refusal that still ran the clear would satisfy them and
	// fail below.
	assertRegisterError(t, rec, http.StatusForbidden, wantCode)

	require.Truef(t, erasureMarkerExists(t, db, did),
		"registration erased the deleted_accounts marker for %s (status %d). The marker is what stops "+
			"the firehose reindexing an erased account: without it a replayed post or acceptance event "+
			"for this DID writes its content back into the AppView, and the deletion this instance was "+
			"asked to perform is silently undone.", did, rec.Code)

	_, err = userRepo.GetByDID(ctx, did)
	require.ErrorIsf(t, err, users.ErrUserNotFound,
		"registration recreated the users row for the erased DID %s (status %d)", did, rec.Code)
}

// erasureMarkerExists asks the deleted_accounts table directly, rather than
// through the repository, because the repository is one of the units under
// test: the question is what is in the table, not what a method says about it.
func erasureMarkerExists(t *testing.T, db *sql.DB, did string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRowContext(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM deleted_accounts WHERE did = $1)`, did).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reading the erasure marker for %s: %v", did, err)
	}
	return exists
}
