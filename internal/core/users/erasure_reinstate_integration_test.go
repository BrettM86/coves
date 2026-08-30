//go:build integration

package users_test

import (
	"context"
	"database/sql"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The two doors into the users table, and the erasure marker answering them
// differently.
//
// # THE ASYMMETRY IS THE FEATURE
//
// Both methods index a user, and both take the same three arguments, so the
// only thing separating them is what the caller knows. IndexUser is called by
// firehose consumers: a profile or identity event says that SOME repo emitted a
// record naming this DID, which a bridge, a replay or an overlapping feed can
// produce months after the account was erased. That is not the account asking
// for anything, so IndexUser refuses and the erasure stands.
//
// IndexAuthenticatedUser is called after an OAuth login, where the account's own
// PDS attested the DID and the handle was verified bidirectionally. That IS the
// account, and an account that comes back is the one case the marker was never
// meant to block — otherwise a mistaken deletion is permanent, and its owner's
// posts are dropped forever with nothing anywhere explaining why.
//
// # WHY BOTH IN ONE TEST, AGAINST A REAL DATABASE
//
// Two DIDs, erased identically, differing only in which method is called. That
// is what makes the assertion about the METHODS rather than about either one's
// setup: a gate that refused everything satisfies half of this, a gate that let
// everything through satisfies the other half, and only a working pair satisfies
// both. And it takes a database because the marker is a row — the claim is that
// one call removed it and the other did not, which is a fact about the table and
// not about what a double was asked.
func TestIndexAuthenticatedUser_ReinstatesWhileIndexUserStaysGated(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, testPDSURL(), nil, "")

	// Two erased accounts, alike in everything the code can see. Run-scoped
	// because the assertions are about rows THESE calls did or did not touch.
	authenticated := eraseFreshAccount(t, db, userRepo, "authed")
	replayed := eraseFreshAccount(t, db, userRepo, "replay")

	// The account itself, logging in.
	require.NoErrorf(t,
		userService.IndexAuthenticatedUser(ctx, authenticated.did, authenticated.handle, testPDSURL()),
		"an authenticated login for the erased account %s failed", authenticated.did)

	require.Falsef(t, markerExists(t, db, authenticated.did),
		"the erasure marker for %s survived an authenticated login. This is the marker's ONLY exit: "+
			"the repository insert no longer clears it, so a marker left here can never be removed, and "+
			"the returning account indexes its profile and then has every post it writes dropped forever",
		authenticated.did)
	require.Truef(t, userRowExists(t, db, authenticated.did),
		"the marker question aside, %s must actually be indexed: clearing the marker without writing "+
			"the row leaves the account un-erased and still absent", authenticated.did)

	// A firehose event naming the other one. Nothing about it says the account
	// asked for anything.
	require.NoErrorf(t, userService.IndexUser(ctx, replayed.did, replayed.handle, testPDSURL()),
		"a firehose event for an erased DID is not a failure — it is an event with nothing to do, and "+
			"returning an error would dead-letter and redrive it forever")

	require.Truef(t, markerExists(t, db, replayed.did),
		"a firehose event cleared the erasure marker for %s. A replayed profile event is not the "+
			"account asking to come back; if it can clear a marker, every erasure is undone by the "+
			"next redrive", replayed.did)
	require.Falsef(t, userRowExists(t, db, replayed.did),
		"a firehose event indexed the erased DID %s. The row is what the marker exists to prevent: "+
			"once it is there the account's content indexes normally again", replayed.did)
}

// erasedAccount is a DID that existed and was then erased.
type erasedAccount struct {
	did    string
	handle string
}

// eraseFreshAccount creates an account and erases it, which is the only way a
// marker comes to exist: Delete refuses a DID with no users row, and writes the
// marker inside that same transaction.
func eraseFreshAccount(t *testing.T, db *sql.DB, repo users.UserRepository, prefix string) erasedAccount {
	t.Helper()

	ctx := context.Background()
	// One run-scoped label under a real TLD: the service validates handle
	// syntax on the way in and rejects the reserved TLDs.
	handle := testkit.UniqueIDWithPrefix(t, prefix) + ".test.coves.dev"
	account := erasedAccount{did: "did:plc:" + testkit.UniqueIDWithPrefix(t, prefix), handle: handle}

	_, err := repo.Create(ctx, &users.User{DID: account.did, Handle: account.handle, PDSURL: testPDSURL()})
	require.NoError(t, err, "seeding the account that will be erased")
	require.NoError(t, repo.Delete(ctx, account.did), "erasing the account")
	require.Truef(t, markerExists(t, db, account.did),
		"fixture: erasing %s left no marker, so this test would prove nothing about what happens to one",
		account.did)

	return account
}

// markerExists and userRowExists read the two tables directly, because the
// repository and the service are what is under test: the question is what is in
// the database, not what a method says about it.
func markerExists(t *testing.T, db *sql.DB, did string) bool {
	t.Helper()
	return rowExists(t, db, `SELECT EXISTS(SELECT 1 FROM deleted_accounts WHERE did = $1)`, did)
}

func userRowExists(t *testing.T, db *sql.DB, did string) bool {
	t.Helper()
	return rowExists(t, db, `SELECT EXISTS(SELECT 1 FROM users WHERE did = $1)`, did)
}

func rowExists(t *testing.T, db *sql.DB, query, did string) bool {
	t.Helper()

	var exists bool
	if err := db.QueryRowContext(context.Background(), query, did).Scan(&exists); err != nil {
		t.Fatalf("querying for %s: %v", did, err)
	}
	return exists
}
