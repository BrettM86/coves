//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type postV2MechanismRow struct {
	CID, Title, Content                        string
	Upvotes, Downvotes, BridgedUp, BridgedDown int
	Score                                      int
	DeletedAt, EditedAt, BridgedAsOf           *time.Time
	IndexedAt                                  time.Time
}

func readPostV2MechanismRow(t *testing.T, db *sql.DB, uri string) postV2MechanismRow {
	t.Helper()
	var row postV2MechanismRow
	require.NoErrorf(t, db.QueryRow(`
		SELECT cid, COALESCE(title, ''), COALESCE(content, ''),
		       upvote_count, downvote_count, bridged_upvote_count, bridged_downvote_count, score,
		       deleted_at, edited_at, bridged_stats_as_of, indexed_at
		FROM posts WHERE uri = $1
	`, uri).Scan(
		&row.CID, &row.Title, &row.Content,
		&row.Upvotes, &row.Downvotes, &row.BridgedUp, &row.BridgedDown, &row.Score,
		&row.DeletedAt, &row.EditedAt, &row.BridgedAsOf, &row.IndexedAt,
	), "no post row for %s", uri)
	return row
}

func readPostV2MechanismRev(t *testing.T, db *sql.DB, uri string) string {
	t.Helper()
	var rev string
	require.NoError(t, db.QueryRow(
		`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, uri,
	).Scan(&rev))
	return rev
}

func pv2RecordWithBridgedStats(communityDID, title, content string, stats map[string]interface{}) map[string]interface{} {
	record := pv2Record(communityDID, title, content)
	record["bridgedStats"] = stats
	return record
}

func newPostV2BridgedFixture(t *testing.T, db *sql.DB, authorPDS, communityPDS string) pv2Fixture {
	t.Helper()
	f := newPV2Fixture(t, db)
	insertBridgedUserOnPDS(t, db, pv2Author, "pv2author.test", authorPDS)
	insertBridgedCommunityOnPDS(t, db, pv2Community, "pv2community.test", pv2Author, communityPDS)
	f.users.users[pv2Author].PDSURL = authorPDS
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db),
		f.users,
		db,
		WithAdmissions(f.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostBridgeTrust(bridgeTrustForTests()),
	)
	return f
}

func assertPostV2BridgedStats(t *testing.T, row postV2MechanismRow, up, down, score int, asOf string) {
	t.Helper()
	assert.Equal(t, up, row.BridgedUp)
	assert.Equal(t, down, row.BridgedDown)
	assert.Equal(t, score, row.Score)
	if asOf == "" {
		assert.Nil(t, row.BridgedAsOf)
		return
	}
	require.NotNil(t, row.BridgedAsOf)
	expected, err := time.Parse(time.RFC3339, asOf)
	require.NoError(t, err)
	assert.Equal(t, expected, row.BridgedAsOf.UTC())
}

func TestPostV2Mechanisms_DeleteBeforeCreate_TombstoneRejectsLateCreate(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)
	const rkey = "pv2mechdeletefirst"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx,
		pv2Event(pv2Author, "delete", rkey, revs[1], "", base, nil)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2mechdeletefirst", base+1_000_000,
		pv2Record(pv2Community, "late create", "must not be indexed"),
	)))

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
	assert.Equal(t, revs[1], readPostV2MechanismRev(t, db, uri))
}

func TestPostV2Mechanisms_StaleCreateReplayAfterDelete_DoesNotResurrect(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)
	const (
		rkey = "pv2mechresurrect"
		cid  = "bafyreipv2mechresurrect"
	)
	uri := pv2URI(pv2Author, rkey)
	record := pv2Record(pv2Community, "original title", "original content")

	require.NoError(t, f.consumer.HandleEvent(ctx,
		pv2Event(pv2Author, "create", rkey, revs[0], cid, base, record)))
	require.NoError(t, f.consumer.HandleEvent(ctx,
		pv2Event(pv2Author, "delete", rkey, revs[1], "", base+1_000_000, nil)))
	first := readPostV2MechanismRow(t, db, uri)
	require.NotNil(t, first.DeletedAt)

	require.NoError(t, f.consumer.HandleEvent(ctx,
		pv2Event(pv2Author, "create", rkey, revs[0], cid, base+2_000_000, record)))

	after := readPostV2MechanismRow(t, db, uri)
	require.NotNil(t, after.DeletedAt)
	assert.Equal(t, first.DeletedAt.UTC(), after.DeletedAt.UTC())
	assert.Equal(t, cid, after.CID)
	assert.Equal(t, "original title", after.Title)
	assert.Equal(t, "original content", after.Content)
	assert.Equal(t, revs[1], readPostV2MechanismRev(t, db, uri))
}

func TestPostV2Mechanisms_UpdateForUnknownURI_IndexesLikeACreate(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	const rkey = "pv2mechghostupdate"
	uri := pv2URI(pv2Author, rkey)
	event := pv2Event(
		pv2Author, "update", rkey, testkit.TID(), "bafyreipv2mechghostupdate", time.Now().UnixMicro(),
		pv2Record(pv2Community, "update arrived first", "cross-feed lag must not lose this post"),
	)

	require.NoError(t, f.consumer.HandleEvent(context.Background(), event))
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"an update may be the first delivery for an author-owned record")
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM community_post_admissions WHERE post_uri = $1`, uri),
		"the upsert must open the same pending admission as a create")

	require.NoError(t, f.consumer.HandleEvent(context.Background(), event))
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"redelivery must not duplicate the post row")
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM community_post_admissions WHERE post_uri = $1`, uri),
		"redelivery must not duplicate the admission row")
}

func TestPostV2Mechanisms_UpdateForSoftDeletedPost_IsSkipped(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 3)
	const rkey = "pv2mechdeletedupdate"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2mechdeletedupdate1", base,
		pv2Record(pv2Community, "original title", "original content"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx,
		pv2Event(pv2Author, "delete", rkey, revs[1], "", base+1_000_000, nil)))
	before := readPostV2MechanismRow(t, db, uri)
	require.NotNil(t, before.DeletedAt)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[2], "bafyreipv2mechdeletedupdate2", base+2_000_000,
		pv2Record(pv2Community, "changed title", "changed content"),
	)))

	after := readPostV2MechanismRow(t, db, uri)
	require.NotNil(t, after.DeletedAt)
	assert.Equal(t, before.DeletedAt.UTC(), after.DeletedAt.UTC())
	assert.Equal(t, before.CID, after.CID)
	assert.Equal(t, before.Title, after.Title)
	assert.Equal(t, before.Content, after.Content)
}

func TestPostV2Mechanisms_StaleUpdateReplay_DoesNotClobberContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 3)
	const rkey = "pv2mechstaleupdate"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2mechv1", base,
		pv2Record(pv2Community, "title v1", "content v1"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[1], "bafyreipv2mechv2", base+1_000_000,
		pv2Record(pv2Community, "title v2", "content v2"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[2], "bafyreipv2mechv3", base+2_000_000,
		pv2Record(pv2Community, "title v3", "content v3"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[1], "bafyreipv2mechv2", base+3_000_000,
		pv2Record(pv2Community, "title v2", "content v2"),
	)))

	row := readPostV2MechanismRow(t, db, uri)
	assert.Equal(t, "bafyreipv2mechv3", row.CID)
	assert.Equal(t, "title v3", row.Title)
	assert.Equal(t, "content v3", row.Content)
	assert.Nil(t, row.DeletedAt)
	assert.Equal(t, revs[2], readPostV2MechanismRev(t, db, uri))
}

func TestPostV2Mechanisms_StaleRedrivenUpdate_CannotRevertNewerContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMicro()
	t0, t1, t2 := base, base+1_000_000, base+2_000_000
	const rkey = "pv2mechredrive"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, "", "bafyreipv2mechredrivev0", t0,
		pv2Record(pv2Community, "v0 title", "v0 content"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, "", "bafyreipv2mechredrivev2", t2,
		pv2Record(pv2Community, "v2 title", "v2 content"),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, "", "bafyreipv2mechredrivev1", t1,
		pv2Record(pv2Community, "v1 title", "v1 content"),
	)))

	row := readPostV2MechanismRow(t, db, uri)
	assert.Equal(t, "bafyreipv2mechredrivev2", row.CID)
	assert.Equal(t, "v2 title", row.Title)
	assert.Equal(t, "v2 content", row.Content)
	assert.Equal(t, time.UnixMicro(t2).UTC(), row.IndexedAt.UTC())

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, "", "bafyreipv2mechredrivev2", t2,
		pv2Record(pv2Community, "v2 title", "v2 content"),
	)))
	assert.Equal(t, "v2 title", readPostV2MechanismRow(t, db, uri).Title)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, "", "bafyreipv2mechredrivev3", t2+1_000_000,
		pv2Record(pv2Community, "v3 title", "v3 content"),
	)))
	assert.Equal(t, "v3 title", readPostV2MechanismRow(t, db, uri).Title)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, "", "bafyreipv2mechredrivev4", 0,
		pv2Record(pv2Community, "v4 title", "v4 content"),
	)))
	assert.Equal(t, "v4 title", readPostV2MechanismRow(t, db, uri).Title)
}

func TestPostV2Mechanisms_DuplicateCreate_IndexesExactlyOnce(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()
	const rkey = "pv2mechdupcreate"
	uri := pv2URI(pv2Author, rkey)
	event := pv2Event(
		pv2Author, "create", rkey, testkit.TID(), "bafyreipv2mechdupcreate", time.Now().UnixMicro(),
		pv2Record(pv2Community, "duplicate target", "index once"),
	)

	require.NoError(t, f.consumer.HandleEvent(ctx, event))
	first := readPostV2MechanismRow(t, db, uri)
	require.NoError(t, f.consumer.HandleEvent(ctx, event))
	after := readPostV2MechanismRow(t, db, uri)

	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
	assert.Equal(t, first, after)
}

func TestPostV2Mechanisms_DuplicateDelete_TombstoneStaysAtFirstDelete(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 3)
	const rkey = "pv2mechdupdelete"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2mechdupdelete", base,
		pv2Record(pv2Community, "delete target", "content survives"),
	)))
	deleteEvent := pv2Event(pv2Author, "delete", rkey, revs[1], "", base+1_000_000, nil)
	require.NoError(t, f.consumer.HandleEvent(ctx, deleteEvent))
	first := readPostV2MechanismRow(t, db, uri)
	require.NotNil(t, first.DeletedAt)

	require.NoError(t, f.consumer.HandleEvent(ctx, deleteEvent))
	require.NoError(t, f.consumer.HandleEvent(ctx,
		pv2Event(pv2Author, "delete", rkey, revs[2], "", base+2_000_000, nil)))

	after := readPostV2MechanismRow(t, db, uri)
	require.NotNil(t, after.DeletedAt)
	assert.Equal(t, first.DeletedAt.UTC(), after.DeletedAt.UTC())
	assert.Equal(t, "delete target", after.Title)
	assert.Equal(t, "content survives", after.Content)
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
	assert.Equal(t, revs[2], readPostV2MechanismRev(t, db, uri))
}

func TestPostV2Mechanisms_MissingRequiredField_IsPermanent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	const rkey = "pv2mechinvalid"
	uri := pv2URI(pv2Author, rkey)

	err := f.consumer.HandleEvent(context.Background(), pv2Event(
		pv2Author, "create", rkey, testkit.TID(), "bafyreipv2mechinvalid", time.Now().UnixMicro(),
		map[string]interface{}{
			"$type":     PostV2Collection,
			"title":     "missing community",
			"createdAt": "2026-03-01T00:00:00Z",
		},
	))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent)
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
}

func TestPostV2Mechanisms_CreateMissingCreatedAt_IsPermanent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	const rkey = "pv2mechmissingcreatedat"
	uri := pv2URI(pv2Author, rkey)

	err := f.consumer.HandleEvent(context.Background(), pv2Event(
		pv2Author, "create", rkey, testkit.TID(), "bafyreipv2mechmissingcreatedat", time.Now().UnixMicro(),
		map[string]interface{}{
			"$type":     PostV2Collection,
			"community": pv2Community,
			"title":     "missing createdAt",
		},
	))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent)
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
}

func TestPostV2Mechanisms_BridgedStats_TrustedAuthorCreateApplies(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPostV2BridgedFixture(t, db, bridgedTestPDS, bridgedTestNativePDS)
	const rkey = "pv2mechtrustedstats"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
		pv2Author, "create", rkey, testkit.TID(), "bafyreipv2mechtrustedstats", time.Now().UnixMicro(),
		pv2RecordWithBridgedStats(pv2Community, "trusted stats", "content", bridgedStatsRecord(10, 3, asOfEarly)),
	)))

	row := readPostV2MechanismRow(t, db, uri)
	assert.Zero(t, row.Upvotes)
	assert.Zero(t, row.Downvotes)
	assertPostV2BridgedStats(t, row, 10, 3, 7, asOfEarly)
}

func TestPostV2Mechanisms_BridgedStats_UntrustedAuthorCreateIgnores(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPostV2BridgedFixture(t, db, bridgedTestNativePDS, bridgedTestPDS)
	const rkey = "pv2mechuntrustedstats"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
		pv2Author, "create", rkey, testkit.TID(), "bafyreipv2mechuntrustedstats", time.Now().UnixMicro(),
		pv2RecordWithBridgedStats(pv2Community, "untrusted stats", "still indexes", bridgedStatsRecord(50, 5, asOfEarly)),
	)))

	row := readPostV2MechanismRow(t, db, uri)
	assert.Equal(t, "untrusted stats", row.Title)
	assert.Equal(t, "still indexes", row.Content)
	assert.Zero(t, row.Upvotes)
	assert.Zero(t, row.Downvotes)
	assertPostV2BridgedStats(t, row, 0, 0, 0, "")
}

func TestPostV2Mechanisms_BridgedStats_UpdateAsOfGuard(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPostV2BridgedFixture(t, db, bridgedTestPDS, bridgedTestNativePDS)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 4)
	const rkey = "pv2mechstatsguard"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2mechstatsguard1", base,
		pv2RecordWithBridgedStats(pv2Community, "initial", "content", bridgedStatsRecord(5, 1, asOfEarly)),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[1], "bafyreipv2mechstatsguard2", base+1_000_000,
		pv2RecordWithBridgedStats(pv2Community, "newer aggregate", "content 2", bridgedStatsRecord(20, 4, asOfLate)),
	)))
	row := readPostV2MechanismRow(t, db, uri)
	assert.Equal(t, "newer aggregate", row.Title)
	assertPostV2BridgedStats(t, row, 20, 4, 16, asOfLate)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[2], "bafyreipv2mechstatsguard3", base+2_000_000,
		pv2RecordWithBridgedStats(pv2Community, "older aggregate", "content 3", bridgedStatsRecord(1, 1, asOfEarly)),
	)))
	row = readPostV2MechanismRow(t, db, uri)
	assert.Equal(t, "bafyreipv2mechstatsguard3", row.CID)
	assert.Equal(t, "older aggregate", row.Title)
	assertPostV2BridgedStats(t, row, 20, 4, 16, asOfLate)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[3], "bafyreipv2mechstatsguard4", base+3_000_000,
		pv2RecordWithBridgedStats(pv2Community, "equal aggregate", "content 4", bridgedStatsRecord(99, 5, asOfLate)),
	)))
	row = readPostV2MechanismRow(t, db, uri)
	assert.Equal(t, "equal aggregate", row.Title)
	assertPostV2BridgedStats(t, row, 99, 5, 94, asOfLate)
}

func TestPostV2Mechanisms_BridgedStats_StatsOnlyLeavesEditedAtUnchanged(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPostV2BridgedFixture(t, db, bridgedTestPDS, bridgedTestNativePDS)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 3)
	const rkey = "pv2mechstatsedited"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], "bafyreipv2mechstatsedited1", base,
		pv2RecordWithBridgedStats(pv2Community, "same title", "same content", bridgedStatsRecord(5, 1, asOfEarly)),
	)))
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[1], "bafyreipv2mechstatsedited2", base+1_000_000,
		pv2RecordWithBridgedStats(pv2Community, "same title", "same content", bridgedStatsRecord(9, 1, asOfLate)),
	)))
	row := readPostV2MechanismRow(t, db, uri)
	assert.Nil(t, row.EditedAt)
	assertPostV2BridgedStats(t, row, 9, 1, 8, asOfLate)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[2], "bafyreipv2mechstatsedited3", base+2_000_000,
		pv2RecordWithBridgedStats(pv2Community, "changed title", "same content", bridgedStatsRecord(9, 1, asOfLate)),
	)))
	assert.NotNil(t, readPostV2MechanismRow(t, db, uri).EditedAt)
}

func TestPostV2Mechanisms_BridgedStats_InvalidAggregateIgnoredWhole(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"negative count": bridgedStatsRecord(-1, 9, asOfEarly),
		"absurd count":   bridgedStatsRecord(maxBridgedCount+1, 9, asOfEarly),
		"invalid asOf":   bridgedStatsRecord(9, 4, "not-a-timestamp"),
	}
	for name, stats := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			f := newPostV2BridgedFixture(t, db, bridgedTestPDS, bridgedTestNativePDS)
			rkey := "pv2mechhygiene" + testkit.TID()
			uri := pv2URI(pv2Author, rkey)

			require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
				pv2Author, "create", rkey, testkit.TID(), "bafyreipv2mechhygiene", time.Now().UnixMicro(),
				pv2RecordWithBridgedStats(pv2Community, "hygiene", "post still indexes", stats),
			)))

			row := readPostV2MechanismRow(t, db, uri)
			assert.Equal(t, "post still indexes", row.Content)
			assertPostV2BridgedStats(t, row, 0, 0, 0, "")
		})
	}
}

func TestPostV2Mechanisms_BridgedStats_AtCapAccepted(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPostV2BridgedFixture(t, db, bridgedTestPDS, bridgedTestNativePDS)
	const rkey = "pv2mechatcap"
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
		pv2Author, "create", rkey, testkit.TID(), "bafyreipv2mechatcap", time.Now().UnixMicro(),
		pv2RecordWithBridgedStats(
			pv2Community, "at cap", "boundary counts are valid",
			bridgedStatsRecord(maxBridgedCount, maxBridgedCount, asOfEarly),
		),
	)))

	assertPostV2BridgedStats(t, readPostV2MechanismRow(t, db, uri),
		maxBridgedCount, maxBridgedCount, 0, asOfEarly)
}

func TestPostV2Mechanisms_BridgedStats_NativeVotesStack(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPostV2BridgedFixture(t, db, bridgedTestPDS, bridgedTestNativePDS)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 2)
	const (
		rkey = "pv2mechstack"
		cid  = "bafyreipv2mechstack1"
	)
	uri := pv2URI(pv2Author, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", rkey, revs[0], cid, base,
		pv2RecordWithBridgedStats(pv2Community, "stacked score", "content", bridgedStatsRecord(10, 2, asOfEarly)),
	)))
	require.NoError(t, newVoteConsumer(db).HandleEvent(ctx,
		aggVoteEvent("create", "pv2mechstackvote", "up", uri, cid)))

	row := readPostV2MechanismRow(t, db, uri)
	assert.Equal(t, 1, row.Upvotes)
	assert.Zero(t, row.Downvotes)
	assertPostV2BridgedStats(t, row, 10, 2, 9, asOfEarly)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "update", rkey, revs[1], "bafyreipv2mechstack2", base+1_000_000,
		pv2RecordWithBridgedStats(pv2Community, "stacked score", "content", bridgedStatsRecord(30, 2, asOfLate)),
	)))
	row = readPostV2MechanismRow(t, db, uri)
	assert.Equal(t, 1, row.Upvotes)
	assertPostV2BridgedStats(t, row, 30, 2, 29, asOfLate)

	accepted, err := f.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   pv2Community,
		PostURI:        uri,
		AcceptanceURI:  "at://" + pv2Community + "/" + posts.AcceptanceCollection + "/pv2mechstack",
		AcceptanceRkey: "pv2mechstack",
		PinnedCID:      "bafyreipv2mechstack2",
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, accepted.Outcome)

	views, err := postgres.NewPostRepository(db).GetViewsByURIs(ctx, []string{uri}, "")
	require.NoError(t, err)
	view := views[uri]
	require.NotNil(t, view)
	require.NotNil(t, view.Stats)
	assert.Equal(t, 31, view.Stats.Upvotes)
	assert.Equal(t, 2, view.Stats.Downvotes)
	assert.Equal(t, 29, view.Stats.Score)
}
