//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The read side: the three shapes anything serving admissions actually asks for.
//
//   - one subject, for the consumer deciding what an incoming event means;
//   - one post across every community, for the author's own view of where their
//     post stands — the "fork" shape of PRD §2, and the reason the primary key
//     is (community, post) rather than a status column on the post;
//   - one community's queue in one status, for moderation.
//
// The middle one is why community_post_admissions exists as a table at all, so
// it is built here the way the design says it occurs: ONE author-owned post URI
// with an admission row under two different communities. Migration 034 carries
// no foreign key from these rows to `posts` or `communities` (an acceptance can
// arrive before the post it is about), which is exactly what makes that
// representable.

// authorOwnedPostURI mints a post URI in an author's own repo.
//
// Author-owned means the URI is scoped to the AUTHOR rather than to a
// community, which is precisely what lets one post carry independent decisions
// from several communities.
func authorOwnedPostURI(t *testing.T) string {
	t.Helper()
	return "at://" + fixtures.DID(testkit.UniqueID(t)) + "/social.coves.community.postv2/" + testkit.TID()
}

// seedCommunities returns the DIDs of n freshly indexed communities.
func seedCommunities(t *testing.T, db *sql.DB, n int) []string {
	t.Helper()

	ctx := context.Background()
	dids := make([]string, n)
	for i := range dids {
		name := testkit.UniqueIDWithPrefix(t, "fork")
		communityDID, err := fixtures.Community(ctx, db, name, "owner"+name)
		require.NoErrorf(t, err, "seeding community %d", i)
		dids[i] = communityDID
	}
	return dids
}

func TestAdmissionRepo_GetOnAnUnseenSubject(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	subject := newAdmissionSubject(t, db)

	got, err := repo.Get(context.Background(), subject.CommunityDID, subject.PostURI)
	assert.ErrorIs(t, err, posts.ErrNotFound,
		"a community that has never seen a post has no opinion about it, and that has to be distinguishable "+
			"from an infrastructure failure — the consumer branches on it to decide whether to insert")
	assert.Nil(t, got)
}

func TestAdmissionRepo_GetByPostURIs(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	postURI := authorOwnedPostURI(t)
	communityDIDs := seedCommunities(t, db, 2)
	acceptingCommunity, removingCommunity := communityDIDs[0], communityDIDs[1]
	contentIdentifier := contentCID(t, "forked")

	// The same post, two communities, two different answers. This is the state
	// a status column on `posts` could not represent.
	acceptanceURI, acceptanceRkey := acceptanceRecord(t, acceptingCommunity)
	_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: acceptingCommunity, PostURI: postURI, EvaluatedCID: contentIdentifier,
	})
	require.NoError(t, err)
	_, err = repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   acceptingCommunity,
		PostURI:        postURI,
		AcceptanceURI:  acceptanceURI,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      contentIdentifier,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)

	_, err = repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: removingCommunity,
		PostURI:      postURI,
		DecisionCode: "rule_violation",
		Watermark:    posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)

	// A second post, so the batch has to key its results rather than return one
	// flat list the caller re-sorts.
	otherPostURI := authorOwnedPostURI(t)
	_, err = repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: seedCommunities(t, db, 1)[0], PostURI: otherPostURI, EvaluatedCID: contentCID(t, "other"),
	})
	require.NoError(t, err)

	unseenPostURI := authorOwnedPostURI(t)

	byURI, err := repo.GetByPostURIs(ctx, []string{postURI, otherPostURI, unseenPostURI})
	require.NoError(t, err)

	require.Contains(t, byURI, postURI)
	assert.Len(t, byURI[postURI], 2, "both communities' decisions must come back, not just the first")

	statusByCommunity := map[string]posts.AdmissionStatus{}
	for _, admission := range byURI[postURI] {
		assert.Equal(t, postURI, admission.PostURI)
		statusByCommunity[admission.CommunityDID] = admission.Status
	}
	assert.Equal(t, posts.AdmissionStatusAccepted, statusByCommunity[acceptingCommunity])
	assert.Equal(t, posts.AdmissionStatusRemoved, statusByCommunity[removingCommunity],
		"one community removing a post says nothing about another community that accepted it")

	require.Contains(t, byURI, otherPostURI)
	assert.Len(t, byURI[otherPostURI], 1)

	assert.NotContains(t, byURI, unseenPostURI,
		"a post no community has an opinion about must be ABSENT rather than present-with-an-empty-slice, "+
			"so a caller ranging over the map cannot mistake it for a post that was considered and passed over")

	t.Run("an empty request is not a query", func(t *testing.T) {
		byURI, err := repo.GetByPostURIs(ctx, nil)
		require.NoError(t, err, "an empty batch is a caller with nothing to hydrate, not an error")
		assert.Empty(t, byURI)
	})
}

func TestAdmissionRepo_ListByStatusForCommunity(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// One community's moderation queue. The rows are created one at a time so
	// their created_at values are genuinely distinct — NOW() is transaction
	// time, and rows written in a single statement would share it and make the
	// ordering assertion below vacuous.
	const queueDepth = 5
	queueCommunity := seedCommunities(t, db, 1)[0]

	var queued []string
	for i := 0; i < queueDepth; i++ {
		uri := authorOwnedPostURI(t)
		_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: queueCommunity, PostURI: uri, EvaluatedCID: contentCID(t, "queued"),
		})
		require.NoErrorf(t, err, "queueing admission %d", i)
		queued = append(queued, uri)
	}

	// Two negative controls: a row in the same community in a DIFFERENT status,
	// and a row in a different community in the SAME status. A listing that
	// filtered on only one of the two columns would still return five rows on
	// the happy path and be wrong in production.
	removedURI := authorOwnedPostURI(t)
	_, err := repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: queueCommunity, PostURI: removedURI, DecisionCode: "rule_violation",
		Watermark: posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)

	otherURI := authorOwnedPostURI(t)
	_, err = repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: seedCommunities(t, db, 1)[0], PostURI: otherURI, EvaluatedCID: contentCID(t, "elsewhere"),
	})
	require.NoError(t, err)

	full, cursor, err := repo.ListByStatusForCommunity(ctx, queueCommunity, posts.AdmissionStatusPending, 10, nil)
	require.NoError(t, err)
	require.Len(t, full, queueDepth,
		"the listing must be scoped by BOTH community and status: a removed row in this community and a "+
			"pending row in another one are both present and must both be excluded")
	assert.Nil(t, cursor, "a page that exhausted the queue must not offer a cursor to nowhere")

	listedURIs := map[string]bool{}
	for i, admission := range full {
		assert.Equal(t, queueCommunity, admission.CommunityDID)
		assert.Equal(t, posts.AdmissionStatusPending, admission.Status)
		listedURIs[admission.PostURI] = true
		if i > 0 {
			assert.False(t, admission.CreatedAt.Before(full[i-1].CreatedAt),
				"the queue is oldest-first, matching the (community_did, status, created_at) index; "+
					"a moderator working a queue that reorders under them re-reviews what they cleared")
		}
	}
	for _, uri := range queued {
		assert.Contains(t, listedURIs, uri)
	}
	assert.NotContains(t, listedURIs, removedURI)
	assert.NotContains(t, listedURIs, otherURI)

	t.Run("the cursor pages through with no overlap and no gap", func(t *testing.T) {
		// Asserted against the full listing rather than against a literal
		// order: what a caller needs is that paging sees each row exactly once,
		// in the order a single unpaged read would have given.
		var paged []*posts.Admission
		var pageCursor *string
		for page := 0; page < queueDepth+1; page++ {
			batch, next, err := repo.ListByStatusForCommunity(ctx, queueCommunity, posts.AdmissionStatusPending, 2, pageCursor)
			require.NoErrorf(t, err, "page %d", page)
			require.LessOrEqualf(t, len(batch), 2, "page %d exceeded the requested limit", page)

			paged = append(paged, batch...)
			pageCursor = next
			if pageCursor == nil {
				break
			}
		}
		require.Nil(t, pageCursor, "paging did not terminate: the cursor never came back nil")
		assert.Equal(t, full, paged,
			"paged reads must reconstruct the unpaged listing exactly — a duplicate means an overlap, a "+
				"missing row means a moderator never sees it")
	})

	t.Run("a malformed cursor is refused rather than silently reset", func(t *testing.T) {
		// Silently restarting at page one would make a moderator re-review
		// everything they had already cleared, and would look like a UI bug.
		garbage := "not-a-cursor"
		_, _, err := repo.ListByStatusForCommunity(ctx, queueCommunity, posts.AdmissionStatusPending, 2, &garbage)
		assert.ErrorIs(t, err, posts.ErrInvalidCursor)
	})
}

// seedPendingAdmissionsAt inserts n pending admission rows for one community in
// a single statement, all sharing exactly the given created_at.
//
// Direct SQL on purpose, twice over: the repo API stamps created_at with NOW()
// per statement, so it cannot manufacture an exact timestamp tie on demand, and
// one multi-row statement is what makes seeding a page-maximum's worth of rows
// cheap enough for the clamp test below.
func seedPendingAdmissionsAt(t *testing.T, db *sql.DB, communityDID string, n int, createdAt time.Time) []string {
	t.Helper()

	uris := make([]string, n)
	for i := range uris {
		uris[i] = authorOwnedPostURI(t)
	}

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO community_post_admissions (community_did, post_uri, status, evaluated_cid, created_at, updated_at)
		SELECT $1, uri, 'pending', 'bafyreiseeded', $3, $3
		FROM unnest($2::text[]) AS uri
	`, communityDID, pq.Array(uris), createdAt)
	require.NoErrorf(t, err, "seeding %d pending admissions at %s", n, createdAt)

	return uris
}

func TestAdmissionRepo_ListByStatusForCommunityLimitClamps(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// The clamp values are pinned as literals, deliberately: asserting against
	// the repo's constants would stay green if someone changed the constants to
	// something unbounded, which is the exact regression this test exists to
	// catch. 50 is admissionQueuePageSize, 200 is admissionQueuePageMaximum.
	const (
		defaultPageSize = 50
		maximumPageSize = 200
	)

	// One row more than the maximum, so both clamps are observable as a page
	// size rather than inferable from internals: an unclamped limit would
	// return all 201 rows.
	queueCommunity := seedCommunities(t, db, 1)[0]
	seedPendingAdmissionsAt(t, db, queueCommunity, maximumPageSize+1, time.Now().UTC())

	t.Run("limit zero means the default page, not an empty or unbounded one", func(t *testing.T) {
		page, cursor, err := repo.ListByStatusForCommunity(ctx, queueCommunity, posts.AdmissionStatusPending, 0, nil)
		require.NoError(t, err)
		assert.Len(t, page, defaultPageSize,
			"limit 0 is a caller expressing no preference; it must get the default page size, not the whole queue and not nothing")
		assert.NotNil(t, cursor, "the queue holds more than a default page, so the page must offer a cursor")
	})

	t.Run("a limit above the maximum is clamped to the maximum", func(t *testing.T) {
		page, cursor, err := repo.ListByStatusForCommunity(ctx, queueCommunity, posts.AdmissionStatusPending, 100000, nil)
		require.NoError(t, err)
		assert.Len(t, page, maximumPageSize,
			"an oversized limit must be clamped: an unbounded listing is a table scan a moderator's browser cannot render")
		assert.NotNil(t, cursor, "the row past the maximum proves the clamp; the cursor must point at it")
	})
}

func TestAdmissionRepo_ListByStatusForCommunityCreatedAtTieBreak(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// Two rows sharing created_at to the microsecond — what rows written in one
	// transaction genuinely look like — so the (created_at, post_uri) keyset's
	// tie-break column is load-bearing: a cursor on created_at alone could not
	// separate them and would either repeat the pair or skip its second half.
	queueCommunity := seedCommunities(t, db, 1)[0]
	sharedCreatedAt := time.Now().UTC().Truncate(time.Microsecond)
	tiedURIs := seedPendingAdmissionsAt(t, db, queueCommunity, 2, sharedCreatedAt)

	full, cursor, err := repo.ListByStatusForCommunity(ctx, queueCommunity, posts.AdmissionStatusPending, 10, nil)
	require.NoError(t, err)
	require.Len(t, full, 2)
	assert.Nil(t, cursor)

	// The order within a tie is deterministic — post_uri ascending, per the
	// ORDER BY the keyset mirrors — not whatever the storage layer felt like.
	wantFirst, wantSecond := tiedURIs[0], tiedURIs[1]
	if wantSecond < wantFirst {
		wantFirst, wantSecond = wantSecond, wantFirst
	}
	assert.Equal(t, wantFirst, full[0].PostURI, "equal created_at must order by post_uri ascending")
	assert.Equal(t, wantSecond, full[1].PostURI, "equal created_at must order by post_uri ascending")

	// Page size one forces the cursor to land exactly ON the tie: the second
	// page's keyset carries the shared created_at and must advance past the
	// first row without skipping the second or repeating the first.
	var paged []*posts.Admission
	var pageCursor *string
	for page := 0; page < 4; page++ {
		batch, next, err := repo.ListByStatusForCommunity(ctx, queueCommunity, posts.AdmissionStatusPending, 1, pageCursor)
		require.NoErrorf(t, err, "page %d", page)

		paged = append(paged, batch...)
		pageCursor = next
		if pageCursor == nil {
			break
		}
	}
	require.Nil(t, pageCursor, "paging across the tie did not terminate")
	assert.Equal(t, full, paged,
		"paging across an exact created_at tie must reconstruct the unpaged listing — a duplicate means the "+
			"cursor re-delivered the tied row, a missing row means it skipped the tie's second half")
}
