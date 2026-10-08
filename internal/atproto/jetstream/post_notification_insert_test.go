//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func postMentionConsumer(db *sql.DB, fixture pv2Fixture, repository notifications.Repository, options ...PostEventConsumerOption) *PostEventConsumer {
	options = append([]PostEventConsumerOption{
		WithAdmissions(fixture.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostNotifications(repository),
	}, options...)
	return NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		fixture.users, db, options...,
	)
}

func postMentionRecord(t *testing.T, createdAt string, handles, recipients []string) map[string]interface{} {
	t.Helper()
	require.Len(t, handles, len(recipients))
	content := "mentions @" + strings.Join(handles, " @")
	record := pv2Record(pv2Community, "Post mentioning users", content)
	record["createdAt"] = createdAt
	facets := make([]interface{}, len(recipients))
	for index := range recipients {
		facets[index] = commentMentionFacet(t, content, handles[index], recipients[index])
	}
	record["facets"] = facets
	return record
}

func postMentionRecipients(t *testing.T, db *sql.DB, count int) ([]string, []string) {
	t.Helper()
	prefix := testkit.UniqueID(t)
	handles := make([]string, count)
	recipients := make([]string, count)
	for index := range recipients {
		handles[index] = fmt.Sprintf("%sperson%02d.test", prefix, index)
		recipients[index] = fmt.Sprintf("did:plc:%sperson%02d", prefix, index)
		insertBridgedUserOnPDS(t, db, recipients[index], handles[index], bridgedTestNativePDS)
	}
	return handles, recipients
}

type postMentionRow struct {
	id        int64
	recipient string
	cid       string
}

func postMentionRows(t *testing.T, db *sql.DB, uri string) []postMentionRow {
	t.Helper()
	rows, err := db.Query(`SELECT id, recipient_did, record_cid FROM notifications
		WHERE record_uri = $1 AND reason = 'mention' ORDER BY id`, uri)
	require.NoError(t, err)
	defer rows.Close()
	var result []postMentionRow
	for rows.Next() {
		var row postMentionRow
		require.NoError(t, rows.Scan(&row.id, &row.recipient, &row.cid))
		result = append(result, row)
	}
	require.NoError(t, rows.Err())
	return result
}

func requirePostMentionFacets(t *testing.T, db *sql.DB, uri string, recipients ...string) {
	t.Helper()
	var facets sql.NullString
	require.NoError(t, db.QueryRow(`SELECT content_facets FROM posts WHERE uri = $1`, uri).Scan(&facets))
	require.True(t, facets.Valid, "fixture: post mention facets must be indexed")
	for _, recipient := range recipients {
		require.Contains(t, facets.String, recipient, "fixture: mention must survive post indexing")
	}
}

func TestPostNotificationInsert_PendingPostMentionsBeforeAcceptance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	const cid = "bafyreiinsertpending"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, testkit.TID(), cid,
		time.Now().UnixMicro(), postMentionRecord(t, createdAt, handles, recipients))))
	_, _, indexedCID, _, deletedAt := readPV2Post(t, db, uri)
	require.Equal(t, cid, indexedCID)
	require.Nil(t, deletedAt)
	requirePostMentionFacets(t, db, uri, recipients...)
	admission, err := fixture.admissions.Get(ctx, pv2Community, uri)
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionStatusPending, admission.Status, "fixture: no acceptance has arrived")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri),
		"a pending post must notify its mentioned recipient")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1
		AND recipient_did = $2 AND reason = 'mention' AND actor_did = $3 AND record_cid = $4
		AND subject_uri IS NULL AND root_post_uri = $1`, uri, recipients[0], pv2Author, cid))
}

func TestPostNotificationInsert_DirectFetchFirstAndJetstreamReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newRealRepoFixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 2)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	postRecord := postMentionRecord(t, createdAt, handles, recipients)
	postRecord["community"] = accCommunity
	record := fixture.author.CreateRecord(t, PostV2Collection, postRecord)
	consumer := newRealPostMentionConsumer(db, fixture, postgres.NewNotificationRepository(db), nil)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, record.URI),
		"fixture: direct fetch must be the first indexing path")
	require.NoError(t, consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, record.URI, record.CID,
		testkit.TID(), time.Now().UnixMicro())))
	_, communityDID, indexedCID, _, deletedAt := readPV2Post(t, db, record.URI)
	require.Equal(t, accCommunity, communityDID)
	require.Equal(t, record.CID, indexedCID)
	require.Nil(t, deletedAt)
	requirePostMentionFacets(t, db, record.URI, recipients...)
	before := postMentionRows(t, db, record.URI)
	require.Len(t, before, 2, "direct-fetch insertion must notify both mentioned users")
	for index, row := range before {
		require.Equal(t, recipients[index], row.recipient)
		require.Equal(t, record.CID, row.cid)
	}
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(fixture.author.DID, "create", record.RKey,
		testkit.TID(), record.CID, time.Now().UnixMicro(), postRecord)))
	_, _, replayCID, _, _ := readPV2Post(t, db, record.URI)
	require.Equal(t, record.CID, replayCID)
	require.Equal(t, before, postMentionRows(t, db, record.URI),
		"Jetstream delivery after direct fetch must preserve mention row IDs and CIDs")
}

func TestPostNotificationInsert_DirectFetchTwelveMentionsKeepsFirstTenOnJetstreamReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newRealRepoFixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 12)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	postRecord := postMentionRecord(t, createdAt, handles, recipients)
	postRecord["community"] = accCommunity
	record := fixture.author.CreateRecord(t, PostV2Collection, postRecord)
	consumer := newRealPostMentionConsumer(db, fixture, postgres.NewNotificationRepository(db), nil)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, record.URI),
		"fixture: the post is not indexed before acceptance triggers direct fetch")
	require.NoError(t, consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, record.URI, record.CID,
		testkit.TID(), time.Now().UnixMicro())))
	_, communityDID, indexedCID, _, deletedAt := readPV2Post(t, db, record.URI)
	require.Equal(t, accCommunity, communityDID)
	require.Equal(t, record.CID, indexedCID, "acceptance must index the fetched post before Jetstream arrives")
	require.Nil(t, deletedAt)
	requirePostMentionFacets(t, db, record.URI, recipients...)

	before := postMentionRows(t, db, record.URI)
	require.Len(t, before, 10, "direct fetch must notify at most ten of the twelve mentioned users")
	for index, row := range before {
		require.Equal(t, recipients[index], row.recipient, "the first ten facets must win in order")
		require.Equal(t, record.CID, row.cid)
	}
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(fixture.author.DID, "create", record.RKey,
		testkit.TID(), record.CID, time.Now().UnixMicro(), postRecord)))
	require.Equal(t, before, postMentionRows(t, db, record.URI),
		"Jetstream replay must preserve exactly the original ten mention rows and their IDs")
}

func newRealPostMentionConsumer(db *sql.DB, fixture *realRepoFixture, repository notifications.Repository, fetcher PostRecordFetcher) *PostEventConsumer {
	if fetcher == nil {
		fetcher = NewDirectPostFetcher(pinnedResolver(fixture.author.DID, fixture.pds.URL()), PrivatePostFetcherOptions(true)...)
	}
	return NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		newMockUserService(), db,
		WithAdmissions(fixture.admissions), WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostRecordFetcher(fetcher), WithPostNotifications(repository),
	)
}

func TestPostNotificationInsert_RacingDirectFetchConflictKeepsFirstTen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newRealRepoFixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 12)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	postRecord := postMentionRecord(t, createdAt, handles, recipients)
	postRecord["community"] = accCommunity
	record := fixture.author.CreateRecord(t, PostV2Collection, postRecord)
	var consumer *PostEventConsumer
	consumer = newRealPostMentionConsumer(db, fixture, postgres.NewNotificationRepository(db), &racingFetcher{
		inner: NewDirectPostFetcher(pinnedResolver(fixture.author.DID, fixture.pds.URL()), PrivatePostFetcherOptions(true)...),
		before: func() {
			require.NoError(t, consumer.HandleEvent(ctx, pv2Event(fixture.author.DID, "create", record.RKey,
				testkit.TID(), record.CID, time.Now().UnixMicro(), postRecord)))
			_, _, indexedCID, _, _ := readPV2Post(t, db, record.URI)
			require.Equal(t, record.CID, indexedCID, "fixture: Jetstream must win the insert race")
		},
	})
	require.NoError(t, consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, record.URI, record.CID,
		testkit.TID(), time.Now().UnixMicro())))
	_, _, indexedCID, _, deletedAt := readPV2Post(t, db, record.URI)
	require.Equal(t, record.CID, indexedCID)
	require.Nil(t, deletedAt)
	requirePostMentionFacets(t, db, record.URI, recipients...)
	rows := postMentionRows(t, db, record.URI)
	require.Len(t, rows, 10, "the insert winner must notify exactly the first ten, with no duplicate on fetch conflict")
	for index, row := range rows {
		require.Equal(t, recipients[index], row.recipient, "facet order determines the ten recipients")
		require.Equal(t, record.CID, row.cid)
	}
}

func TestPostNotificationInsert_ConflictNeverCallsNotificationRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	recordMap := postMentionRecord(t, createdAt, handles, recipients)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	const originalCID = "bafyreiinsertconflictoriginal"
	firstConsumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	revs := increasingTIDs(t, 2)
	require.NoError(t, firstConsumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		originalCID, time.Now().UnixMicro(), recordMap)))
	_, _, indexedCID, _, _ := readPV2Post(t, db, uri)
	require.Equal(t, originalCID, indexedCID)
	requirePostMentionFacets(t, db, uri, recipients...)
	// Seed an existing mention independently of the feature under development:
	// the conflict must preserve a real row even while fan-out is still absent.
	_, err := db.ExecContext(ctx, `INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, root_post_uri, record_created_at)
		VALUES ($1, 'mention', $2, $3, $4, $2, NOW()) ON CONFLICT DO NOTHING`, recipients[0], uri, originalCID, pv2Author)
	require.NoError(t, err)
	before := postMentionRows(t, db, uri)
	require.Len(t, before, 1)
	injectedError := errors.New("notification repository reached on insert conflict")
	failing := &failingCommentNotificationRepository{delegate: postgres.NewNotificationRepository(db), failure: injectedError}
	consumer := postMentionConsumer(db, fixture, failing)
	parsed, err := parseAuthorPostRecord(recordMap)
	require.NoError(t, err)
	facetsJSON, err := json.Marshal(recordMap["facets"])
	require.NoError(t, err)
	applied, err := consumer.insertAuthorPost(ctx, authorPostInsert{
		uri: uri, authorDID: pv2Author, record: parsed,
		commit: &CommitEvent{Operation: "create", Collection: PostV2Collection,
			RKey: key, Rev: revs[1], CID: "bafyreiinsertconflictnewer"},
		timeUS: time.Now().Add(time.Second).UnixMicro(),
		facets: sql.NullString{String: string(facetsJSON), Valid: true},
	})
	require.NoError(t, err)
	require.False(t, applied, "ON CONFLICT must report no post insertion")
	_, _, indexedCID, _, _ = readPV2Post(t, db, uri)
	require.Equal(t, originalCID, indexedCID)
	require.Equal(t, before, postMentionRows(t, db, uri), "the conflicting insert must preserve existing mention rows")
	require.Empty(t, failing.intents, "notification ApplyTx must not run for an insert conflict")
}

func TestPostNotificationInsert_StaleRevNeverCallsNotificationRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 2)
	won, err := tryAdvanceRecordRev(ctx, db, uri, revs[1])
	require.NoError(t, err)
	require.True(t, won, "fixture: newer rev must be recorded without a post row")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
	failing := &failingCommentNotificationRepository{
		delegate: postgres.NewNotificationRepository(db), failure: errors.New("stale event reached ApplyTx"),
	}
	consumer := postMentionConsumer(db, fixture, failing)
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		"bafyreistaleinsert", time.Now().UnixMicro(), postMentionRecord(t, createdAt, handles, recipients))))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri), "stale create must not index")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri))
	require.Empty(t, failing.intents, "stale create must never call notification ApplyTx")
}

func TestPostNotificationInsert_TwelveMentionsKeepFirstTen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 12)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	const cid = "bafyreipostmentioncap"
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, testkit.TID(), cid,
		time.Now().UnixMicro(), postMentionRecord(t, createdAt, handles, recipients))))
	_, _, indexedCID, _, deletedAt := readPV2Post(t, db, uri)
	require.Equal(t, cid, indexedCID)
	require.Nil(t, deletedAt)
	requirePostMentionFacets(t, db, uri, recipients...)
	rows := postMentionRows(t, db, uri)
	require.Len(t, rows, 10, "one post creates at most ten mentions")
	for index, row := range rows {
		require.Equal(t, recipients[index], row.recipient, "first ten facets win")
		require.Equal(t, cid, row.cid)
	}
	require.Equal(t, 10, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri),
		"post create must write only mention notifications")
}

func TestPostNotificationInsert_ApplyFailureRollsBackJetstreamPostRevAndAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	injectedError := errors.New("injected post mention write failure")
	failing := &failingCommentNotificationRepository{delegate: postgres.NewNotificationRepository(db), failure: injectedError}
	consumer := postMentionConsumer(db, fixture, failing)
	err := consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, testkit.TID(),
		"bafyreipostrollback", time.Now().UnixMicro(), postMentionRecord(t, createdAt, handles, recipients)))
	require.ErrorIs(t, err, injectedError, "notification failure must fail the entire post insert")
	require.Len(t, failing.intents, 1, "fixture: mention fan-out must have reached ApplyTx")
	require.Equal(t, recipients[0], failing.intents[0].RecipientDID)
	require.Equal(t, notifications.ReasonMention, failing.intents[0].Reason)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri), "failed insert must roll back")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM jetstream_record_revs WHERE record_uri = $1`, uri),
		"failed insert must not advance the rev")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM community_post_admissions WHERE post_uri = $1`, uri),
		"failed insert must not seed an admission")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri))
}

func TestPostNotificationInsert_ApplyFailureRollsBackDirectFetch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newRealRepoFixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	postRecord := postMentionRecord(t, createdAt, handles, recipients)
	postRecord["community"] = accCommunity
	record := fixture.author.CreateRecord(t, PostV2Collection, postRecord)
	injectedError := errors.New("injected fetched post mention write failure")
	failing := &failingCommentNotificationRepository{delegate: postgres.NewNotificationRepository(db), failure: injectedError}
	consumer := newRealPostMentionConsumer(db, fixture, failing, nil)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, record.URI),
		"fixture: post must be fetched rather than already indexed")
	err := consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, record.URI, record.CID,
		testkit.TID(), time.Now().UnixMicro()))
	require.ErrorIs(t, err, injectedError, "notification failure must fail direct-fetch insertion")
	require.Len(t, failing.intents, 1, "fixture: fetched post's mention must reach ApplyTx")
	require.Equal(t, recipients[0], failing.intents[0].RecipientDID)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, record.URI),
		"fetched post insert must roll back with notification failure")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, record.URI))
}

func TestPostNotificationInsert_LegacyPostMentionIsDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	const collection = "social.coves.community.post"
	key := testkit.TID()
	uri := "at://" + pv2Community + "/" + collection + "/" + key
	record := postMentionRecord(t, createdAt, handles, recipients)
	record["$type"] = collection
	record["author"] = pv2Author
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(pv2Community, collection, "create", key,
		testkit.TID(), "bafyreilegacymention", time.Now().UnixMicro(), record)))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"fixture: retired legacy collection must remain unindexed")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri),
		"retired legacy collection must never fan out mentions")
}
