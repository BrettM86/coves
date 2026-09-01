//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

const (
	bridgeAPDSURL = "https://bridge-a.test" // coves:allow-host-literal: exact inert communities.pds_url fixture under test; never dialled
	otherPDSURL   = "https://other.test"    // coves:allow-host-literal: exact inert communities.pds_url fixture under test; never dialled
)

type bridgedVotesCandidatesFixture struct {
	p1 string
	p2 string
	p3 string
	p4 string
	c1 string
	c2 string
	c3 string
	c4 string
}

func TestBridgedVotesRepository_SelectCandidates(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedBridgedVotesCandidates(t, ctx, db)

	got, err := NewBridgedVotesRepository(db).SelectCandidates(ctx, []string{bridgeAPDSURL}, 90*24*time.Hour, 50)
	require.NoError(t, err)
	require.ElementsMatch(t, []bridgedvotes.Candidate{
		{URI: fixture.p1, StoredPDSURL: bridgeAPDSURL},
		{URI: fixture.c1, StoredPDSURL: bridgeAPDSURL},
	}, got)
}

func TestBridgedVotesRepository_SelectCandidatesHonorsLimit(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedBridgedVotesCandidates(t, ctx, db)

	got, err := NewBridgedVotesRepository(db).SelectCandidates(ctx, []string{bridgeAPDSURL}, 90*24*time.Hour, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Contains(t, []string{fixture.p1, fixture.c1}, got[0].URI)
	require.Equal(t, bridgeAPDSURL, got[0].StoredPDSURL)
}

func TestBridgedVotesRepository_DistinctCommunityPDSURLs(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	seedBridgedVotesCandidates(t, ctx, db)
	_, err := db.ExecContext(ctx, `
		INSERT INTO communities
			(did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		VALUES
			('did:plc:bridgedvotescandidated', '!candidate-d@local.test', 'candidate-d',
			 'did:plc:bridgedvotescandidated', 'did:plc:bridgedvotescandidated',
			 'did:plc:bridgedvotescandidated', $1, NOW())
	`, bridgeAPDSURL)
	require.NoError(t, err, "seed duplicate community PDS URL")

	got, err := NewBridgedVotesRepository(db).DistinctCommunityPDSURLs(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{bridgeAPDSURL, otherPDSURL}, got)
}

func seedBridgedVotesCandidates(t *testing.T, ctx context.Context, db *sql.DB) bridgedVotesCandidatesFixture {
	t.Helper()

	const (
		communityA = "did:plc:bridgedvotescandidatea"
		communityB = "did:plc:bridgedvotescandidateb"
		communityC = "did:plc:bridgedvotescandidatec"
	)
	fixture := bridgedVotesCandidatesFixture{
		p1: "at://did:plc:author1/social.coves.community.postv2/p1",
		p2: "at://did:plc:author2/social.coves.community.postv2/p2",
		p3: "at://did:plc:author3/social.coves.community.postv2/p3",
		p4: "at://did:plc:author4/social.coves.community.postv2/p4",
		c1: "at://did:plc:commenter1/social.coves.community.comment/c1",
		c2: "at://did:plc:commenter2/social.coves.community.comment/c2",
		c3: "at://did:plc:commenter3/social.coves.community.comment/c3",
		c4: "at://did:plc:commenter4/social.coves.community.comment/c4",
	}
	recent := time.Now().UTC()
	old := recent.Add(-120 * 24 * time.Hour)
	deletedAt := recent.Add(-time.Hour)

	_, err := db.ExecContext(ctx, `
		INSERT INTO communities
			(did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		VALUES
			($1, '!candidate-a@local.test', 'candidate-a', $1, $1, $1, $2, $6),
			($3, '!candidate-b@local.test', 'candidate-b', $3, $3, $3, $4, $6),
			($5, '!candidate-c@local.test', 'candidate-c', $5, $5, $5, '', $6)
	`, communityA, bridgeAPDSURL, communityB, otherPDSURL, communityC, recent)
	require.NoError(t, err, "seed candidate communities")

	_, err = db.ExecContext(ctx, `
		INSERT INTO posts
			(uri, cid, rkey, author_did, community_did, title, created_at, deleted_at)
		VALUES
			($1, 'bafycandidatep1', 'p1', 'did:plc:author1', $2, 'p1', $7, NULL),
			($3, 'bafycandidatep2', 'p2', 'did:plc:author2', $2, 'p2', $7, $8),
			($4, 'bafycandidatep3', 'p3', 'did:plc:author3', $2, 'p3', $9, NULL),
			($5, 'bafycandidatep4', 'p4', 'did:plc:author4', $6, 'p4', $7, NULL)
	`, fixture.p1, communityA, fixture.p2, fixture.p3, fixture.p4, communityB, recent, deletedAt, old)
	require.NoError(t, err, "seed candidate posts")

	const ghostRootURI = "at://did:plc:ghost/social.coves.community.postv2/unindexed"
	_, err = db.ExecContext(ctx, `
		INSERT INTO comments
			(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at, deleted_at)
		VALUES
			($1, 'bafycandidatec1', 'c1', 'did:plc:commenter1', $5, 'bafycandidatep1', $5, 'bafycandidatep1', 'c1', $8, NULL),
			($2, 'bafycandidatec2', 'c2', 'did:plc:commenter2', $6, 'bafycandidatep2', $6, 'bafycandidatep2', 'c2', $8, NULL),
			($3, 'bafycandidatec3', 'c3', 'did:plc:commenter3', $5, 'bafycandidatep1', $5, 'bafycandidatep1', 'c3', $8, $9),
			($4, 'bafycandidatec4', 'c4', 'did:plc:commenter4', $7, 'bafyghost', $7, 'bafyghost', 'c4', $8, NULL)
	`, fixture.c1, fixture.c2, fixture.c3, fixture.c4, fixture.p1, fixture.p2, ghostRootURI, recent, deletedAt)
	require.NoError(t, err, "seed candidate comments")

	return fixture
}

func TestBridgedVotesRepository_MarkPolledAdvancesPostsAndComments(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedBridgedVotesCandidates(t, ctx, db)
	const missingURI = "at://did:plc:missing/social.coves.community.postv2/not-indexed"

	require.NoError(t, NewBridgedVotesRepository(db).MarkPolled(ctx, []string{fixture.p1, fixture.c1, missingURI}))

	postPolledAt := readBridgedPolledAt(t, ctx, db, "posts", fixture.p1)
	commentPolledAt := readBridgedPolledAt(t, ctx, db, "comments", fixture.c1)
	unnamedPostPolledAt := readBridgedPolledAt(t, ctx, db, "posts", fixture.p4)

	require.True(t, postPolledAt.Valid, "named post bridged_polled_at must be populated")
	require.WithinDuration(t, time.Now().UTC(), postPolledAt.Time.UTC(), time.Minute)
	require.True(t, commentPolledAt.Valid, "named comment bridged_polled_at must be populated")
	require.WithinDuration(t, time.Now().UTC(), commentPolledAt.Time.UTC(), time.Minute)
	require.False(t, unnamedPostPolledAt.Valid, "post omitted from MarkPolled must retain a NULL watermark")
}

func TestBridgedVotesRepository_SelectCandidatesRotatesNeverPolledThenOldest(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedBridgedVotesRotation(t, ctx, db)
	repo := NewBridgedVotesRepository(db)

	got, err := repo.SelectCandidates(ctx, []string{bridgeAPDSURL}, 90*24*time.Hour, 50)
	require.NoError(t, err)
	require.Equal(t, []bridgedvotes.Candidate{
		{URI: fixture.pNever, StoredPDSURL: bridgeAPDSURL},
		{URI: fixture.pOld, StoredPDSURL: bridgeAPDSURL},
		{URI: fixture.pNew, StoredPDSURL: bridgeAPDSURL},
	}, got)

	got, err = repo.SelectCandidates(ctx, []string{bridgeAPDSURL}, 90*24*time.Hour, 1)
	require.NoError(t, err)
	require.Equal(t, []bridgedvotes.Candidate{
		{URI: fixture.pNever, StoredPDSURL: bridgeAPDSURL},
	}, got)
}

func readBridgedPolledAt(t *testing.T, ctx context.Context, db *sql.DB, table, uri string) sql.NullTime {
	t.Helper()

	query := `SELECT bridged_polled_at FROM posts WHERE uri = $1`
	if table == "comments" {
		query = `SELECT bridged_polled_at FROM comments WHERE uri = $1`
	}

	var polledAt sql.NullTime
	require.NoError(t, db.QueryRowContext(ctx, query, uri).Scan(&polledAt), "read %s watermark for %s", table, uri)
	return polledAt
}

type bridgedVotesRotationFixture struct {
	pNever string
	pOld   string
	pNew   string
}

func seedBridgedVotesRotation(t *testing.T, ctx context.Context, db *sql.DB) bridgedVotesRotationFixture {
	t.Helper()

	const communityDID = "did:plc:bridgedvotesrotation"
	fixture := bridgedVotesRotationFixture{
		pNever: "at://did:plc:rotationnever/social.coves.community.postv2/never",
		pOld:   "at://did:plc:rotationold/social.coves.community.postv2/old",
		pNew:   "at://did:plc:rotationnew/social.coves.community.postv2/new",
	}
	now := time.Now().UTC()

	_, err := db.ExecContext(ctx, `
		INSERT INTO communities
			(did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		VALUES
			($1, '!rotation@local.test', 'rotation', $1, $1, $1, $2, $3)
	`, communityDID, bridgeAPDSURL, now)
	require.NoError(t, err, "seed rotation community")

	_, err = db.ExecContext(ctx, `
		INSERT INTO posts
			(uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES
			($1, 'bafyrotationnever', 'never', 'did:plc:rotationnever', $4, 'never', $5),
			($2, 'bafyrotationold', 'old', 'did:plc:rotationold', $4, 'old', $5),
			($3, 'bafyrotationnew', 'new', 'did:plc:rotationnew', $4, 'new', $5)
	`, fixture.pNever, fixture.pOld, fixture.pNew, communityDID, now)
	require.NoError(t, err, "seed rotation posts")

	_, err = db.ExecContext(ctx, `
		UPDATE posts
		SET bridged_polled_at = CASE uri
			WHEN $1 THEN $3::timestamptz
			WHEN $2 THEN $4::timestamptz
		END
		WHERE uri IN ($1, $2)
	`, fixture.pOld, fixture.pNew, now.Add(-2*time.Hour), now.Add(-time.Minute))
	require.NoError(t, err, "seed rotation watermarks")

	return fixture
}

func TestBridgedVotesRepository_ApplyAggregateFirstApply(t *testing.T) {
	t.Parallel()

	t1 := time.Date(2026, 8, 31, 2, 4, 1, 80_000_000, time.UTC)
	tests := []struct {
		name  string
		table string
		uri   func(applyAggregateFixture) string
	}{
		{name: "post", table: "posts", uri: func(f applyAggregateFixture) string { return f.postURI }},
		{name: "comment", table: "comments", uri: func(f applyAggregateFixture) string { return f.commentURI }},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			ctx := context.Background()
			fixture := seedApplyAggregateFixture(t, ctx, db)
			uri := test.uri(fixture)

			require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
				URI: uri, Upvotes: 5, Downvotes: 2, AsOf: t1,
			}))
			requireAggregateStats(t, ctx, db, test.table, uri, expectedAggregateStats{
				nativeUp: 3, nativeDown: 1, bridgedUp: 5, bridgedDown: 2, score: 5, asOf: &t1,
			})
		})
	}
}

func TestBridgedVotesRepository_ApplyAggregateNewerAsOfOverwrites(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedApplyAggregateFixture(t, ctx, db)
	t1 := time.Date(2026, 8, 31, 2, 4, 1, 80_000_000, time.UTC)
	t2 := t1.Add(time.Minute)
	seedStoredAggregate(t, ctx, db, "posts", fixture.postURI, 5, 2, t1)

	require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI: fixture.postURI, Upvotes: 7, Downvotes: 2, AsOf: t2,
	}))
	requireAggregateStats(t, ctx, db, "posts", fixture.postURI, expectedAggregateStats{
		nativeUp: 3, nativeDown: 1, bridgedUp: 7, bridgedDown: 2, score: 7, asOf: &t2,
	})
}

func TestBridgedVotesRepository_ApplyAggregateEqualAsOfReapplies(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedApplyAggregateFixture(t, ctx, db)
	t2 := time.Date(2026, 8, 31, 2, 5, 1, 80_000_000, time.UTC)
	seedStoredAggregate(t, ctx, db, "posts", fixture.postURI, 7, 2, t2)

	require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI: fixture.postURI, Upvotes: 7, Downvotes: 2, AsOf: t2,
	}))
	requireAggregateStats(t, ctx, db, "posts", fixture.postURI, expectedAggregateStats{
		nativeUp: 3, nativeDown: 1, bridgedUp: 7, bridgedDown: 2, score: 7, asOf: &t2,
	})
}

func TestBridgedVotesRepository_ApplyAggregateStaleAsOfIsNoOp(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedApplyAggregateFixture(t, ctx, db)
	t1 := time.Date(2026, 8, 31, 2, 4, 1, 80_000_000, time.UTC)
	t0 := t1.Add(-time.Minute)
	seedStoredAggregate(t, ctx, db, "posts", fixture.postURI, 5, 2, t1)

	require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI: fixture.postURI, Upvotes: 99, Downvotes: 41, AsOf: t0,
	}))
	requireAggregateStats(t, ctx, db, "posts", fixture.postURI, expectedAggregateStats{
		nativeUp: 3, nativeDown: 1, bridgedUp: 5, bridgedDown: 2, score: 5, asOf: &t1,
	})
}

func TestBridgedVotesRepository_ApplyAggregateDeletedRowIsNoOpSuccess(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedApplyAggregateFixture(t, ctx, db)
	t1 := time.Date(2026, 8, 31, 2, 4, 1, 80_000_000, time.UTC)

	require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI: fixture.deletedPostURI, Upvotes: 5, Downvotes: 2, AsOf: t1,
	}))
	requireAggregateStats(t, ctx, db, "posts", fixture.deletedPostURI, expectedAggregateStats{
		nativeUp: 3, nativeDown: 1, bridgedUp: 0, bridgedDown: 0, score: 2,
	})
}

func TestBridgedVotesRepository_ApplyAggregateAbsentRowIsNoOpSuccess(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	seedApplyAggregateFixture(t, ctx, db)
	t1 := time.Date(2026, 8, 31, 2, 4, 1, 80_000_000, time.UTC)

	require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI:     "at://did:plc:missing/social.coves.community.postv2/not-indexed",
		Upvotes: 5, Downvotes: 2, AsOf: t1,
	}))
}

func TestBridgedVotesRepository_ApplyAggregateZeroAsOfIsAnError(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedApplyAggregateFixture(t, ctx, db)

	// Counts and their stamp form one atomic validated trio. The client never
	// produces a zero AsOf, so one reaching the store is a caller bug and must
	// surface as an error rather than a silent no-op that marks the subject polled.
	err := NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI: fixture.postURI, Upvotes: 5, Downvotes: 2,
	})
	require.ErrorIs(t, err, bridgedvotes.ErrMissingAsOf)
	requireAggregateStats(t, ctx, db, "posts", fixture.postURI, expectedAggregateStats{
		nativeUp: 3, nativeDown: 1, bridgedUp: 0, bridgedDown: 0, score: 2,
	})
}

type applyAggregateFixture struct {
	postURI        string
	commentURI     string
	deletedPostURI string
}

func seedApplyAggregateFixture(t *testing.T, ctx context.Context, db *sql.DB) applyAggregateFixture {
	t.Helper()

	const communityDID = "did:plc:applyaggregatecommunity"
	fixture := applyAggregateFixture{
		postURI:        "at://did:plc:applyaggregateauthor/social.coves.community.postv2/post",
		commentURI:     "at://did:plc:applyaggregatecommenter/social.coves.community.comment/comment",
		deletedPostURI: "at://did:plc:applyaggregatedeleted/social.coves.community.postv2/deleted",
	}
	now := time.Now().UTC()
	deletedAt := now.Add(-time.Minute)

	_, err := db.ExecContext(ctx, `
		INSERT INTO communities
			(did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		VALUES
			($1, '!apply-aggregate@local.test', 'apply-aggregate', $1, $1, $1, $2, $3)
	`, communityDID, bridgeAPDSURL, now)
	require.NoError(t, err, "seed apply-aggregate community")

	_, err = db.ExecContext(ctx, `
		INSERT INTO posts
			(uri, cid, rkey, author_did, community_did, title, created_at, deleted_at,
			 upvote_count, downvote_count, score, bridged_upvote_count, bridged_downvote_count, bridged_stats_as_of)
		VALUES
			($1, 'bafyapplyaggregatepost', 'post', 'did:plc:applyaggregateauthor', $3, 'post', $4, NULL,
			 3, 1, 2, 0, 0, NULL),
			($2, 'bafyapplyaggregatedeleted', 'deleted', 'did:plc:applyaggregatedeleted', $3, 'deleted', $4, $5,
			 3, 1, 2, 0, 0, NULL)
	`, fixture.postURI, fixture.deletedPostURI, communityDID, now, deletedAt)
	require.NoError(t, err, "seed apply-aggregate posts")

	_, err = db.ExecContext(ctx, `
		INSERT INTO comments
			(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at,
			 upvote_count, downvote_count, score, bridged_upvote_count, bridged_downvote_count, bridged_stats_as_of)
		VALUES
			($1, 'bafyapplyaggregatecomment', 'comment', 'did:plc:applyaggregatecommenter',
			 $2, 'bafyapplyaggregatepost', $2, 'bafyapplyaggregatepost', 'comment', $3,
			 3, 1, 2, 0, 0, NULL)
	`, fixture.commentURI, fixture.postURI, now)
	require.NoError(t, err, "seed apply-aggregate comment")

	return fixture
}

func seedStoredAggregate(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	table string,
	uri string,
	bridgedUp int,
	bridgedDown int,
	asOf time.Time,
) {
	t.Helper()

	query := `
		UPDATE posts
		SET bridged_upvote_count = $2,
			bridged_downvote_count = $3,
			bridged_stats_as_of = $4,
			score = (upvote_count + $2::int) - (downvote_count + $3::int)
		WHERE uri = $1
	`
	if table == "comments" {
		query = `
			UPDATE comments
			SET bridged_upvote_count = $2,
				bridged_downvote_count = $3,
				bridged_stats_as_of = $4,
				score = (upvote_count + $2::int) - (downvote_count + $3::int)
			WHERE uri = $1
		`
	}

	result, err := db.ExecContext(ctx, query, uri, bridgedUp, bridgedDown, asOf)
	require.NoError(t, err, "seed stored %s aggregate for %s", table, uri)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows, "seeded aggregate must update exactly one %s row", table)
}

type storedAggregateStats struct {
	nativeUp    int
	nativeDown  int
	bridgedUp   int
	bridgedDown int
	score       int
	asOf        sql.NullTime
}

type expectedAggregateStats struct {
	nativeUp    int
	nativeDown  int
	bridgedUp   int
	bridgedDown int
	score       int
	asOf        *time.Time
}

func requireAggregateStats(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	table string,
	uri string,
	want expectedAggregateStats,
) {
	t.Helper()

	query := `
		SELECT upvote_count, downvote_count, bridged_upvote_count,
		       bridged_downvote_count, score, bridged_stats_as_of
		FROM posts WHERE uri = $1
	`
	if table == "comments" {
		query = `
			SELECT upvote_count, downvote_count, bridged_upvote_count,
			       bridged_downvote_count, score, bridged_stats_as_of
			FROM comments WHERE uri = $1
		`
	}

	var got storedAggregateStats
	require.NoError(t, db.QueryRowContext(ctx, query, uri).Scan(
		&got.nativeUp,
		&got.nativeDown,
		&got.bridgedUp,
		&got.bridgedDown,
		&got.score,
		&got.asOf,
	), "read %s aggregate stats for %s", table, uri)
	require.Equal(t, want.nativeUp, got.nativeUp, "%s native upvotes", uri)
	require.Equal(t, want.nativeDown, got.nativeDown, "%s native downvotes", uri)
	require.Equal(t, want.bridgedUp, got.bridgedUp, "%s bridged upvotes", uri)
	require.Equal(t, want.bridgedDown, got.bridgedDown, "%s bridged downvotes", uri)
	require.Equal(t, want.score, got.score, "%s score", uri)
	if want.asOf == nil {
		require.False(t, got.asOf.Valid, "%s bridged stats as-of must remain NULL", uri)
		return
	}
	require.True(t, got.asOf.Valid, "%s bridged stats as-of must be populated", uri)
	require.True(t, got.asOf.Time.UTC().Equal(want.asOf.UTC()),
		"%s bridged stats as-of: got %s, want %s", uri, got.asOf.Time.UTC(), want.asOf.UTC())
}

func TestBridgedVotesRepository_SelectCandidatesOrdersNeverPolledByCreatedAt(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedNeverPolledCandidateOrder(t, ctx, db)
	repo := NewBridgedVotesRepository(db)
	want := []bridgedvotes.Candidate{
		{URI: fixture.oldest, StoredPDSURL: bridgeAPDSURL},
		{URI: fixture.middle, StoredPDSURL: bridgeAPDSURL},
		{URI: fixture.newest, StoredPDSURL: bridgeAPDSURL},
	}

	got, err := repo.SelectCandidates(ctx, []string{bridgeAPDSURL}, 90*24*time.Hour, 50)
	require.NoError(t, err)
	require.Equal(t, want, got,
		"never-polled candidates must rotate oldest-created first, then by URI")

	got, err = repo.SelectCandidates(ctx, []string{bridgeAPDSURL}, 90*24*time.Hour, 1)
	require.NoError(t, err)
	require.Equal(t, want[:1], got,
		"a bounded sweep must deterministically choose the oldest never-polled candidate")
}

func TestBridgedVotesRepository_ApplyAggregateCommentNewerAsOfOverwrites(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedApplyAggregateFixture(t, ctx, db)
	t1 := time.Date(2026, 8, 31, 2, 4, 1, 80_000_000, time.UTC)
	t2 := t1.Add(time.Minute)
	seedStoredAggregate(t, ctx, db, "comments", fixture.commentURI, 5, 2, t1)

	require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI: fixture.commentURI, Upvotes: 7, Downvotes: 2, AsOf: t2,
	}))
	requireAggregateStats(t, ctx, db, "comments", fixture.commentURI, expectedAggregateStats{
		nativeUp: 3, nativeDown: 1, bridgedUp: 7, bridgedDown: 2, score: 7, asOf: &t2,
	})
}

func TestBridgedVotesRepository_ApplyAggregateCommentEqualAsOfReapplies(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedApplyAggregateFixture(t, ctx, db)
	t2 := time.Date(2026, 8, 31, 2, 5, 1, 80_000_000, time.UTC)
	seedStoredAggregate(t, ctx, db, "comments", fixture.commentURI, 7, 2, t2)

	require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI: fixture.commentURI, Upvotes: 7, Downvotes: 2, AsOf: t2,
	}))
	requireAggregateStats(t, ctx, db, "comments", fixture.commentURI, expectedAggregateStats{
		nativeUp: 3, nativeDown: 1, bridgedUp: 7, bridgedDown: 2, score: 7, asOf: &t2,
	})
}

func TestBridgedVotesRepository_ApplyAggregateCommentStaleAsOfIsNoOp(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	fixture := seedApplyAggregateFixture(t, ctx, db)
	t1 := time.Date(2026, 8, 31, 2, 4, 1, 80_000_000, time.UTC)
	t0 := t1.Add(-time.Minute)
	seedStoredAggregate(t, ctx, db, "comments", fixture.commentURI, 5, 2, t1)

	require.NoError(t, NewBridgedVotesRepository(db).ApplyAggregate(ctx, bridgedvotes.Aggregate{
		URI: fixture.commentURI, Upvotes: 99, Downvotes: 41, AsOf: t0,
	}))
	requireAggregateStats(t, ctx, db, "comments", fixture.commentURI, expectedAggregateStats{
		nativeUp: 3, nativeDown: 1, bridgedUp: 5, bridgedDown: 2, score: 5, asOf: &t1,
	})
}

type neverPolledCandidateOrderFixture struct {
	oldest string
	middle string
	newest string
}

func seedNeverPolledCandidateOrder(t *testing.T, ctx context.Context, db *sql.DB) neverPolledCandidateOrderFixture {
	t.Helper()

	const communityDID = "did:plc:neverpolledorder"
	fixture := neverPolledCandidateOrderFixture{
		oldest: "at://did:plc:neverpolledorder/social.coves.community.postv2/zzz-oldest",
		middle: "at://did:plc:neverpolledorder/social.coves.community.postv2/mmm-middle",
		newest: "at://did:plc:neverpolledorder/social.coves.community.postv2/aaa-newest",
	}
	now := time.Now().UTC()

	_, err := db.ExecContext(ctx, `
		INSERT INTO communities
			(did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		VALUES
			($1, '!never-polled-order@local.test', 'never-polled-order', $1, $1, $1, $2, $3)
	`, communityDID, bridgeAPDSURL, now)
	require.NoError(t, err, "seed never-polled ordering community")

	// Deliberately insert newest, oldest, then middle. With every watermark NULL,
	// heap/insertion order differs from the required created_at order.
	_, err = db.ExecContext(ctx, `
		INSERT INTO posts
			(uri, cid, rkey, author_did, community_did, title, created_at, bridged_polled_at)
		VALUES
			($1, 'bafyneverpollednewest', 'aaa-newest', 'did:plc:neverpollednewest', $4, 'newest', $5, NULL),
			($2, 'bafyneverpolledoldest', 'zzz-oldest', 'did:plc:neverpolledoldest', $4, 'oldest', $6, NULL),
			($3, 'bafyneverpolledmiddle', 'mmm-middle', 'did:plc:neverpolledmiddle', $4, 'middle', $7, NULL)
	`, fixture.newest, fixture.oldest, fixture.middle, communityDID,
		now.Add(-time.Minute), now.Add(-time.Hour), now.Add(-30*time.Minute))
	require.NoError(t, err, "seed never-polled ordering posts")

	return fixture
}
