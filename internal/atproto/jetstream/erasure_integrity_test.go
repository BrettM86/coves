//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the migration-036 erasure marker has to survive.
//
// The marker is the only thing that distinguishes "this account was erased on
// purpose" from "this account has never been indexed" — and under author-owned
// posts the second is a NORMAL state that must index freely (§5.3). So every
// path that can clear the marker, or that can act without consulting it, is a
// way for erased content to come back.
//
// The three below are the ones the review found, and they fail differently:
//
//   - The marker's EXIT is too wide. An account that genuinely comes back must
//     be able to clear it, and can; but so could any firehose-driven index of
//     the same DID, which is not the account coming back at all — it is a
//     stranger's repo emitting a profile event for a DID this AppView was asked
//     to forget.
//   - The GATE has a survivor. postv2 events are checked; a replayed acceptance
//     is a second door into the same admissions table.
//   - The LOOKUP fails open. "I could not read the marker table" and "there is
//     no marker" must not be the same answer, because the second one indexes.

// erasedFixture is a consumer wired with the real erasure lookup plus the real
// users service, so the marker's exit is exercised through the same statement
// production uses rather than through a fake.
type erasedFixture struct {
	consumer    *PostEventConsumer
	userService users.UserService
	admissions  posts.AdmissionRepository
}

func newErasedFixture(t *testing.T, db *sql.DB, opts ...PostEventConsumerOption) erasedFixture {
	t.Helper()

	insertBridgedUser(t, db, accAuthor, "erasureowner.test")
	insertBridgedCommunity(t, db, accCommunity, "erasurecommunity.test", accAuthor)

	admissions := postgres.NewAdmissionRepository(db)
	wired := append([]PostEventConsumerOption{
		WithAdmissions(admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
	}, opts...)

	return erasedFixture{
		consumer: NewPostEventConsumer(
			postgres.NewPostRepository(db), postgres.NewCommunityRepository(db),
			newMockUserService(), db, wired...),
		userService: users.NewUserService(postgres.NewUserRepository(db), nil, bskySocialPDS, nil, ""),
		admissions:  admissions,
	}
}

// failingErasureLookup is a DeletedAccountLookup whose table cannot be read.
type failingErasureLookup struct{ err error }

func (l failingErasureLookup) IsAccountDeleted(context.Context, string) (bool, error) {
	return false, l.err
}

func TestErasure_FirehoseIndexingDoesNotClearTheMarker(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newErasedFixture(t, db)

	erased := fixtures.DID("erased" + testkit.UniqueID(t))
	markAccountDeleted(t, db, erased)

	// IndexUser is what the firehose calls. It is NOT a registration: the DID
	// appearing in a profile or identity event means only that some repo
	// somewhere emitted a record — a bridge, a replay, an overlapping feed —
	// and any of those can arrive months after the account was erased.
	//
	// The marker's exit is an authenticated login — IndexAuthenticatedUser,
	// called after the account's own PDS has attested the DID. Clearing it here
	// instead would make the erasure undone by the very replays it exists to
	// defend against, and undone SILENTLY: the users row reappears, the marker
	// is gone, and the next replayed post indexes normally.
	err := f.userService.IndexUser(ctx, erased, "erased.test", bskySocialPDS)

	var markers int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM deleted_accounts WHERE did = $1`, erased).Scan(&markers))
	assert.Equalf(t, 1, markers,
		"a firehose-driven IndexUser cleared the erasure marker for %s. The marker's only exit is an "+
			"authenticated login; clearing it here means any repo on the network can un-erase an account by emitting "+
			"one record naming its DID (IndexUser returned: %v)", erased, err)

	var indexed int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM users WHERE did = $1`, erased).Scan(&indexed))
	assert.Zerof(t, indexed,
		"the erased account was re-indexed from the firehose; the row the deletion removed is back")
}

func TestErasure_AnAuthenticatedLoginIsTheMarkersExit(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newErasedFixture(t, db)

	// THE OTHER HALF, and it has to be pinned beside the gate test above or the
	// fix has an obvious wrong shape available: never clearing the marker at
	// all. A person who deletes their account and later logs back in on the same
	// DID must be able to, and a marker left standing would make every post they
	// write disappear with nothing to point at.
	//
	// WHICH CALL COUNTS IS THE POINT. The exit is a named method, and the thing
	// it is named after is the only fact that justifies it: the account
	// authenticated, so its own PDS attested the DID and the handle was verified
	// in both directions. A caller reaching IndexAuthenticatedUser is saying
	// that; a caller writing a row is not, which is why the test below pins that
	// the bare insert changes nothing.
	returning := fixtures.DID("returning" + testkit.UniqueID(t))
	markAccountDeleted(t, db, returning)

	require.NoError(t,
		f.userService.IndexAuthenticatedUser(ctx, returning, "returning.test", bskySocialPDS),
		"an account that logs back in must be able to")

	var markers int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM deleted_accounts WHERE did = $1`, returning).Scan(&markers))
	assert.Zerof(t, markers,
		"the erasure marker for %s survived an authenticated login. This is the marker's ONLY exit, so a "+
			"marker left here can never be removed: the account indexes and then has every post it writes "+
			"dropped, silently and forever", returning)

	var indexed int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM users WHERE did = $1`, returning).Scan(&indexed))
	assert.Equalf(t, 1, indexed,
		"clearing the marker without indexing %s leaves the account un-erased and still absent, which is "+
			"neither of the two states this call is allowed to produce", returning)
}

func TestErasure_WritingAUsersRowIsNotTheMarkersExit(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(db)

	// The negative that gives the test above its meaning. Both calls end in the
	// same INSERT, so "the marker is gone after IndexAuthenticatedUser" is
	// satisfied just as well by an insert that clears markers as a side effect —
	// which is exactly the behaviour that made an unauthenticated registration
	// endpoint able to un-erase any account. Only asserting that the bare insert
	// leaves the marker standing separates the two.
	writtenOver := fixtures.DID("insertonly" + testkit.UniqueID(t))
	markAccountDeleted(t, db, writtenOver)

	_, err := repo.Create(ctx, &users.User{
		DID: writtenOver, Handle: "insertonly.test", PDSURL: bskySocialPDS,
	})
	require.NoError(t, err,
		"the insert itself must still succeed — the marker is not a constraint on writing rows, it is a "+
			"record of a promise the ingestion gate keeps")

	var markers int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM deleted_accounts WHERE did = $1`, writtenOver).Scan(&markers))
	assert.Equalf(t, 1, markers,
		"inserting a users row for %s cleared its erasure marker. Clearing a marker re-opens ingestion for "+
			"content this AppView was asked to forget, and it must never happen as a side effect of writing a "+
			"row: any caller that can cause an insert can then cause an erasure to be undone", writtenOver)
}

func TestErasure_AcceptanceForASweptAuthorIsGated(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newErasedFixture(t, db)

	// The mutation survivor. postv2 events are gated, so the obvious door is
	// shut — but an acceptance names the post by URI, and the URI carries the
	// author's DID, so a replayed acceptance can recreate the admission row for
	// an erased account's post without any postv2 event being involved at all.
	erased := fixtures.DID("sweptauthor" + testkit.UniqueID(t))
	markAccountDeleted(t, db, erased)

	uri := "at://" + erased + "/" + PostV2Collection + "/" + testkit.TID()

	err := f.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, uri, "bafyreiswept", testkit.TID(), time.Now().UnixMicro()))

	require.NoError(t, err,
		"an acceptance about an erased account's post must be DROPPED, not refused: refusing it dead-letters an event "+
			"that will be replayed and dropped identically forever")
	assert.Zerof(t, countRows(t, db,
		`SELECT count(*) FROM community_post_admissions WHERE post_uri = $1`, uri),
		"the acceptance recreated the admission row that the account deletion swept — which is the exact resurrection "+
			"migration 036 exists to prevent, arriving through the door the postv2 gate does not cover")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"and nothing may be indexed for it either")
}

func TestErasure_AnUnreadableMarkerTableFailsClosed(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	lookupErr := errors.New("deleted_accounts is unreachable")
	f := newErasedFixture(t, db, WithDeletedAccounts(failingErasureLookup{err: lookupErr}))

	author := fixtures.DID("unknownerasure" + testkit.UniqueID(t))
	rkey := "erasureunknown"
	uri := "at://" + author + "/" + PostV2Collection + "/" + rkey

	err := f.consumer.HandleEvent(ctx, pv2Event(
		author, "create", rkey, testkit.TID(), "bafyreierasureunknown", time.Now().UnixMicro(),
		pv2Record(accCommunity, "indexed while the marker table was down", "body"),
	))

	// "I could not read the marker" and "there is no marker" must never be the
	// same answer. Under §5.3 an unknown author indexes freely, so a lookup that
	// failed open is indistinguishable from a healthy one — and a database blip
	// becomes a window in which every erased account's replayed content is
	// re-indexed, permanently, with nothing recording that it happened.
	require.Errorf(t, err,
		"a failed erasure lookup was treated as 'not erased' and the post was indexed anyway. The lookup gates a "+
			"deletion guarantee, so it has to fail CLOSED: a transient error retried is a delay, while a fail-open is "+
			"content coming back that somebody asked to have removed")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"nothing may be indexed while the gate cannot be consulted")
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"an unreachable table is transient: the redrive is what makes failing closed cheap rather than lossy")
}
