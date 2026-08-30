//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 036's marker table, and the thing it exists to make impossible.
//
// Account deletion used to leave NO trace. userRepo.Delete removes the users
// row, the posts, and (since 034) the admission rows — and then the firehose
// redelivers a post event for that same author, or a dead letter for it is
// redriven, and every one of those swept rows comes straight back. The AppView
// re-indexes the content of an account it was asked to erase, and nothing in
// the schema can tell it not to: an absent users row is indistinguishable from
// an author who simply has not been indexed yet, which under author-owned posts
// (§5.3) is a state the consumer is REQUIRED to accept.
//
// deleted_accounts is what makes those two cases distinguishable. A row here
// means "this DID was erased on purpose"; no row means "never seen". The
// ingestion gate that reads it is tested in internal/atproto/jetstream; what is
// tested here is the half that has to be true for the gate to mean anything:
// the marker is written, it is written ATOMICALLY WITH the deletion, and
// re-registration clears it.
//
// See docs/PRD_AUTHOR_OWNED_POSTS.md rev 2.7 (§5 status header).

const deletedAccountsTable = "deleted_accounts"

func TestDeletedAccountsTable_Columns(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	requireTableExists(t, db, deletedAccountsTable)

	type columnShape struct {
		dataType string
		nullable bool
	}
	want := map[string]columnShape{
		"did":         {"text", false},
		"deleted_at":  {"timestamp with time zone", false},
		"deleted_rev": {"text", true},
	}

	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
	`, deletedAccountsTable)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	got := map[string]columnShape{}
	for rows.Next() {
		var name, dataType, isNullable string
		require.NoError(t, rows.Scan(&name, &dataType, &isNullable))
		got[name] = columnShape{dataType: dataType, nullable: isNullable == "YES"}
	}
	require.NoError(t, rows.Err())

	for name, wantShape := range want {
		gotShape, ok := got[name]
		if !assert.Truef(t, ok, "%s.%s is missing", deletedAccountsTable, name) {
			continue
		}
		assert.Equalf(t, wantShape.dataType, gotShape.dataType, "%s.%s type", deletedAccountsTable, name)
		assert.Equalf(t, wantShape.nullable, gotShape.nullable, "%s.%s nullability", deletedAccountsTable, name)
	}

	assert.Equal(t, []string{"did"}, primaryKeyColumns(t, db, deletedAccountsTable),
		"the DID is the whole key: one marker per account, so a re-delete updates rather than accumulating rows the gate would have to deduplicate")

	// deleted_at is NOT NULL because the marker's only job is to be READ by a
	// consumer deciding whether to index an event, and a marker with no time is
	// a marker that cannot participate in any retention or audit answer later.
	// deleted_rev is nullable because nothing knows the account's repo revision
	// at AppView-deletion time — the deletion is a local administrative act, not
	// a commit — and a column that had to be filled would be filled with a lie.
	assert.Falsef(t, got["deleted_at"].nullable, "deleted_at must be NOT NULL")
	assert.Truef(t, got["deleted_rev"].nullable, "deleted_rev must be nullable")
}

func TestUserRepo_Delete_LeavesADeletionMarker(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, deletedAccountsTable)

	handle := testkit.UniqueIDWithPrefix(t, "delmark")
	did := fixtures.DID(handle)
	fixtures.User(t, db, handle+".test", did)

	before := time.Now().Add(-time.Second)
	require.NoError(t, NewUserRepository(db).Delete(ctx, did))

	var deletedAt time.Time
	var deletedRev *string
	err := db.QueryRowContext(ctx,
		`SELECT deleted_at, deleted_rev FROM deleted_accounts WHERE did = $1`, did,
	).Scan(&deletedAt, &deletedRev)
	require.NoErrorf(t, err,
		"deleting %s left no marker row. Without one, a redriven post event for this author re-indexes the content the deletion erased, "+
			"and the consumer cannot tell an erased account from one that has simply not been indexed yet", did)

	assert.Truef(t, deletedAt.After(before), "deleted_at (%s) must record when the deletion happened", deletedAt)
	assert.Nil(t, deletedRev,
		"deleted_rev must be left NULL: the AppView does not know the account's repo revision at deletion time, and inventing one would put a fabricated watermark where a real comparison happens")
}

func TestUserRepo_Delete_MarkerIsWrittenInTheSameTransaction(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, deletedAccountsTable)

	// A DID with content but NO users row. Delete sweeps the content and then
	// finds nothing to delete from users, which is the one failure the method
	// already reports (users.ErrUserNotFound) — and it makes this the cheapest
	// honest probe of atomicity there is.
	//
	// The claim under test is narrow and load-bearing: the marker INSERT must be
	// a statement of the deletion transaction, not a separate write afterwards.
	// A marker written outside it survives a rollback, and then a DID that was
	// never actually erased is permanently refused by the ingestion gate — the
	// account's own future posts stop indexing, silently, with no row anywhere
	// explaining why.
	name := testkit.UniqueIDWithPrefix(t, "delatomic")
	communityDID, err := fixtures.Community(ctx, db, name, "owner"+name)
	require.NoError(t, err)

	ghostAuthor := fixtures.DID(testkit.UniqueID(t))
	postURI := "at://" + ghostAuthor + "/social.coves.community.postv2/" + testkit.TID()
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_post_admissions (community_did, post_uri, status, created_at, updated_at)
		VALUES ($1, $2, 'pending', NOW(), NOW())
	`, communityDID, postURI)
	require.NoError(t, err)

	deleteErr := NewUserRepository(db).Delete(ctx, ghostAuthor)
	require.ErrorIsf(t, deleteErr, users.ErrUserNotFound,
		"fixture: deleting a DID with no users row must fail, which is what gives this test a rolled-back transaction to inspect")

	var markers int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM deleted_accounts WHERE did = $1`, ghostAuthor).Scan(&markers))
	assert.Zerof(t, markers,
		"the deletion FAILED and rolled back, but a marker for %s survived: the marker insert is running outside the deletion transaction. "+
			"A marker for an account that still exists is worse than no marker at all — the ingestion gate refuses every future event from a live account", ghostAuthor)

	// And the rollback really did roll back, so the surviving marker above could
	// only have come from a write outside the transaction.
	var admissions int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM community_post_admissions WHERE post_uri = $1`, postURI).Scan(&admissions))
	require.Equal(t, 1, admissions,
		"fixture: the failed deletion must have rolled its content sweep back, or this test proves nothing about where the marker was written")
}

// Creating a users row does NOT clear the erasure marker.
//
// Clearing a marker re-opens ingestion for content the AppView was asked to
// forget, so it has to be a decision somebody makes rather than a side effect
// of a statement they happened to run. An INSERT that also cleared markers
// would put that decision within reach of every caller able to cause a row to
// be written — including an unauthenticated endpoint that asks only for a
// domain — and nothing at any of those call sites would say so. ReinstateAccount
// below is the only thing that removes a marker, which is what makes "who may
// un-erase an account?" a question you can answer by reading the call sites.
//
// What this leaves is a state that reads oddly and is correct: a users row for
// a DID that still has a marker. It is what an erased account's own replayed
// profile event would produce if it ever reached Create, and the ingestion gate
// goes on dropping that account's content, which is what the erasure promised.
func TestUserRepo_Create_LeavesTheDeletionMarkerStanding(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, deletedAccountsTable)

	repo := NewUserRepository(db)

	handle := testkit.UniqueIDWithPrefix(t, "rereg")
	did := fixtures.DID(handle)
	fixtures.User(t, db, handle+".test", did)
	require.NoError(t, repo.Delete(ctx, did))

	var markers int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM deleted_accounts WHERE did = $1`, did).Scan(&markers))
	require.Equal(t, 1, markers, "fixture: the deletion must have left a marker for this test to be about")

	_, err := repo.Create(ctx, &users.User{
		DID:    did,
		Handle: handle + ".test",
		PDSURL: testkit.Endpoints().PDS.BaseURL,
	})
	require.NoError(t, err,
		"the insert itself must still succeed — the marker is not a constraint on writing rows, "+
			"it is a record of a promise the ingestion gate keeps")

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM deleted_accounts WHERE did = $1`, did).Scan(&markers))
	assert.Equalf(t, 1, markers,
		"inserting a users row for %s cleared its erasure marker. Clearing a marker re-opens ingestion "+
			"for content this AppView was asked to forget, and it must never happen as a side effect of "+
			"writing a row: any caller that can cause an insert can then cause an erasure to be undone. "+
			"Use ReinstateAccount, which says so", did)
}

// ReinstateAccount is the marker's only exit, and this is what it has to do.
//
// An account that genuinely comes back — the same person logging in again, an
// account restored after a mistaken deletion — must be able to index, so the
// marker needs an exit. What makes a named method safe where a side effect is
// not is that a reader can find every caller and ask whether that caller knows
// the account authenticated.
//
// IT REPORTS WHETHER THERE WAS ANYTHING TO REMOVE. An account returning from
// erasure is rare and consequential — it is the AppView reversing a deletion it
// promised to keep — and the caller is the only layer that can record it against
// the login that caused it. A method returning only an error makes that
// indistinguishable from the overwhelmingly common case of a login by an
// account that was never erased.
func TestUserRepo_ReinstateAccount_RemovesTheMarker(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, deletedAccountsTable)

	repo := NewUserRepository(db)
	store, ok := repo.(users.ErasureMarkerStore)
	require.Truef(t, ok,
		"the PostgreSQL repository must implement users.ErasureMarkerStore: it is the only "+
			"implementation that can, and the service type-asserts for it rather than requiring it "+
			"of every UserRepository")

	handle := testkit.UniqueIDWithPrefix(t, "reinst")
	did := fixtures.DID(handle)
	fixtures.User(t, db, handle+".test", did)
	require.NoError(t, repo.Delete(ctx, did))

	var markers int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM deleted_accounts WHERE did = $1`, did).Scan(&markers))
	require.Equal(t, 1, markers, "fixture: the deletion must have left a marker for this test to remove")

	reinstated, err := store.ReinstateAccount(ctx, did)
	require.NoError(t, err)
	assert.Truef(t, reinstated,
		"reinstating the erased account %s reported that there was no marker to remove. This is the "+
			"one call that reverses a deletion, and a caller told nothing happened cannot record that "+
			"anything did", did)

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM deleted_accounts WHERE did = $1`, did).Scan(&markers))
	assert.Zerof(t, markers,
		"ReinstateAccount left the marker for %s standing. It is the only exit the marker has, so a "+
			"no-op here means an account that came back can never index again: its profile is written "+
			"and every post it writes is dropped, silently and forever", did)
}

// Reinstating is idempotent, in both of the ways a caller can hit.
//
// This is not defensive padding. The callers of ReinstateAccount cannot know
// whether a marker is there — an authenticated login carries no information
// about whether the account was ever erased, and the overwhelmingly common case
// is that it was not. If "there was nothing to remove" were an error, every
// ordinary login would produce one, and the only way to avoid that would be to
// look first, which is a second round trip and a race. So the absence of a
// marker is a successful outcome: the account is not erased, which is what the
// caller wanted to be true.
func TestUserRepo_ReinstateAccount_IsIdempotent(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, deletedAccountsTable)

	store, ok := NewUserRepository(db).(users.ErasureMarkerStore)
	require.True(t, ok, "the PostgreSQL repository must implement users.ErasureMarkerStore")

	t.Run("a DID that was never erased", func(t *testing.T) {
		// No fixtures.User and no Delete: this DID has no marker and no row,
		// which is every login by every account that was never erased.
		did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "nevererased"))

		reinstated, err := store.ReinstateAccount(ctx, did)
		require.NoErrorf(t, err,
			"reinstating %s, which was never erased, reported an error. Callers cannot know whether a "+
				"marker exists without a second query and a race, so 'there was nothing to remove' has "+
				"to be success — otherwise every ordinary login fails", did)
		assert.Falsef(t, reinstated,
			"reinstating %s reported that an account came back from erasure. It was never erased, and "+
				"this is what every ordinary login looks like: a caller that logged or alerted on this "+
				"would be reporting a reversed deletion on every sign-in", did)
	})

	t.Run("a second reinstatement of the same DID", func(t *testing.T) {
		handle := testkit.UniqueIDWithPrefix(t, "reinst2")
		did := fixtures.DID(handle)
		fixtures.User(t, db, handle+".test", did)
		require.NoError(t, NewUserRepository(db).Delete(ctx, did))

		first, err := store.ReinstateAccount(ctx, did)
		require.NoError(t, err)
		require.Truef(t, first, "fixture: the first reinstatement of %s must be the one that removed a marker", did)

		second, err := store.ReinstateAccount(ctx, did)
		require.NoErrorf(t, err,
			"the second reinstatement of %s failed. A retried login, or two devices logging in at "+
				"once, both produce this call twice", did)
		assert.Falsef(t, second,
			"the second reinstatement of %s also reported a marker removed. Only one of these calls "+
				"reversed anything; a caller counting them would report the same deletion coming back "+
				"once per device", did)
	})
}
