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

func TestUserRepo_Create_ClearsTheDeletionMarker(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	requireTableExists(t, db, deletedAccountsTable)

	// Re-registration is the marker's exit. A DID that comes back — the same
	// person signing up again on the same PDS, or an account restored after a
	// mistaken deletion — must index normally, and a marker left behind would
	// make the AppView refuse their new posts forever with nothing to show for
	// it. Create is the assertion point because it is the one statement both
	// service paths funnel through: IndexUser calls CreateUser (service.go:457)
	// and RegisterAccount ends in the same repository insert.
	repo := NewUserRepository(db)

	handle := testkit.UniqueIDWithPrefix(t, "rereg")
	did := fixtures.DID(handle)
	fixtures.User(t, db, handle+".test", did)
	require.NoError(t, repo.Delete(ctx, did))

	var markers int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM deleted_accounts WHERE did = $1`, did).Scan(&markers))
	require.Equal(t, 1, markers, "fixture: the deletion must have left a marker for the re-registration to clear")

	_, err := repo.Create(ctx, &users.User{
		DID:    did,
		Handle: handle + ".test",
		PDSURL: testkit.Endpoints().PDS.BaseURL,
	})
	require.NoError(t, err, "a deleted DID must be able to register again")

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM deleted_accounts WHERE did = $1`, did).Scan(&markers))
	assert.Zerof(t, markers,
		"re-registering %s left the deletion marker standing. The ingestion gate reads this table, so the account would index its profile and then have every post it writes silently dropped", did)
}
