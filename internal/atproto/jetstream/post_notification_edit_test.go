//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPostNotificationEdit_AcceptanceAddKeepRemoveThenDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 2)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 4)
	base := time.Now().UnixMicro()
	const (
		createCID = "bafyreiposteditacceptancecreate"
		addCID    = "bafyreiposteditacceptanceadd"
		removeCID = "bafyreiposteditacceptanceremove"
	)

	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		createCID, base, postMentionRecord(t, createdAt, handles[:1], recipients[:1]))))
	_, _, indexedCID, _, deletedAt := readPV2Post(t, db, uri)
	require.Equal(t, createCID, indexedCID)
	require.Nil(t, deletedAt)
	requirePostMentionFacets(t, db, uri, recipients[0])
	original := postMentionRows(t, db, uri)
	require.Len(t, original, 1, "fixture: B must receive a mention on create")
	require.Equal(t, recipients[0], original[0].recipient)
	require.Equal(t, createCID, original[0].cid)
	var storedCreatedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT created_at FROM posts WHERE uri = $1`, uri).Scan(&storedCreatedAt))

	// An edit's incoming createdAt must not replace the indexed post's original time.
	editCreatedAt := storedCreatedAt.Add(30 * time.Second).UTC().Format(time.RFC3339Nano)
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		addCID, base+1_000_000, postMentionRecord(t, editCreatedAt, handles, recipients))))
	_, _, indexedCID, _, _ = readPV2Post(t, db, uri)
	require.Equal(t, addCID, indexedCID, "fixture: the winning edit must reach the post row")
	requirePostMentionFacets(t, db, uri, recipients...)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, uri, recipients[1]),
		"the edit must add exactly one mention row for E")
	var editCID string
	var recordCreatedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT record_cid, record_created_at FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, uri, recipients[1]).
		Scan(&editCID, &recordCreatedAt))
	require.Equal(t, addCID, editCID)
	require.True(t, recordCreatedAt.Equal(storedCreatedAt),
		"E's mention must carry the stored post created_at, not the edit's incoming createdAt")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND id = $3 AND record_cid = $4`,
		uri, recipients[0], original[0].id, createCID), "keeping B must not re-notify B")
	require.Len(t, postMentionRows(t, db, uri), 2)

	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[2],
		removeCID, base+2_000_000, postMentionRecord(t, editCreatedAt, handles[1:], recipients[1:]))))
	_, _, indexedCID, _, _ = readPV2Post(t, db, uri)
	require.Equal(t, removeCID, indexedCID)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND id = $3 AND record_cid = $4`,
		uri, recipients[0], original[0].id, createCID), "removing B must retain B's original mention")
	require.Len(t, postMentionRows(t, db, uri), 2, "removing B must not retract or duplicate mentions")

	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "delete", key, revs[3],
		"", base+3_000_000, nil)))
	_, _, _, _, deletedAt = readPV2Post(t, db, uri)
	require.NotNil(t, deletedAt, "the winning delete must tombstone the post")
	require.Equal(t, 2, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri),
		"the post tombstone must keep both notification rows")
}

func TestPostNotificationEdit_OldStoredPostNotifiesOnFreshEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	now := time.Now().UTC().Truncate(time.Microsecond)
	createdAt := now.Add(-10 * 24 * time.Hour)
	_, err := db.ExecContext(ctx, `UPDATE notification_activation SET activated_at = $1`, now.Add(-11*24*time.Hour))
	require.NoError(t, err)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 2)
	create := pv2Record(pv2Community, "Old post", "No mentions yet")
	create["createdAt"] = createdAt.Format(time.RFC3339Nano)
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		"bafyreioldpostcreate", now.Add(-2*time.Second).UnixMicro(), create)))
	var storedCreatedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT created_at FROM posts WHERE uri = $1`, uri).Scan(&storedCreatedAt))
	require.True(t, storedCreatedAt.Equal(createdAt), "fixture: the stored post must be more than seven days old")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri))
	const editCID = "bafyreioldpostfreshedit"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		editCID, now.Add(time.Second).UnixMicro(), postMentionRecord(t, createdAt.Format(time.RFC3339Nano), handles, recipients))))
	_, _, indexedCID, _, _ := readPV2Post(t, db, uri)
	require.Equal(t, editCID, indexedCID, "fixture: the fresh edit must apply despite the old createdAt")
	requirePostMentionFacets(t, db, uri, recipients[0])
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`, uri, recipients[0], editCID),
		"an old post edited now must notify its newly mentioned recipient")
}

func TestPostNotificationEdit_OldEventAppliesContentWithoutMention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 2)
	create := pv2Record(pv2Community, "Old event post", "No mentions yet")
	create["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		"bafyreipostoldeventcreate", time.Now().UnixMicro(), create)))
	var oldTime, databaseNow time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT now(), now() - INTERVAL '8 days'`).Scan(&databaseNow, &oldTime))
	require.True(t, oldTime.Before(databaseNow.Add(-7*24*time.Hour)), "fixture: edit event is older than the freshness window")
	_, err := db.ExecContext(ctx, `UPDATE posts SET indexed_at = $1 WHERE uri = $2`, oldTime.Add(-time.Hour), uri)
	require.NoError(t, err)
	var storedIndexedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT indexed_at FROM posts WHERE uri = $1`, uri).Scan(&storedIndexedAt))
	require.True(t, storedIndexedAt.Before(oldTime), "fixture: old edit must pass the recency guard")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri))
	const editCID = "bafyreipostoldeventedit"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		editCID, oldTime.UnixMicro(), postMentionRecord(t, createdAt, handles, recipients))))
	row := readPostV2MechanismRow(t, db, uri)
	require.Equal(t, editCID, row.CID, "old event must still replace the post CID")
	require.Equal(t, "mentions @"+handles[0], row.Content, "old event must still replace the post content")
	require.Equal(t, revs[1], readPostV2MechanismRev(t, db, uri), "old edit must advance the revision")
	requirePostMentionFacets(t, db, uri, recipients[0])
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, uri, recipients[0]),
		"an edit event eight days old must not notify its new mention")
}

func TestPostNotificationEdit_ZeroEventTimeNotifiesAddedMention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 2)
	create := pv2Record(pv2Community, "Zero time post", "No mentions yet")
	create["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		"bafyreipostzerotimecreate", time.Now().UnixMicro(), create)))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri))
	const editCID = "bafyreipostzerotimeedit"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		editCID, 0, postMentionRecord(t, createdAt, handles, recipients))))
	require.Equal(t, editCID, readPostV2MechanismRow(t, db, uri).CID, "zero-time edit must apply")
	require.Equal(t, revs[1], readPostV2MechanismRev(t, db, uri))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`, uri, recipients[0], editCID),
		"missing Jetstream time falls back to index-time freshness, not the Unix epoch")
}

func TestPostNotificationEdit_StoredPreActivationPostStaysSilentDespiteNewIncomingTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	now := time.Now().UTC().Truncate(time.Microsecond)
	storedCreatedAt := now.Add(-2 * time.Hour)
	_, err := db.ExecContext(ctx, `UPDATE notification_activation SET activated_at = $1`, now.Add(-time.Hour))
	require.NoError(t, err)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 2)
	create := pv2Record(pv2Community, "Pre-activation post", "No mentions yet")
	create["createdAt"] = storedCreatedAt.Format(time.RFC3339Nano)
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		"bafyreipreactivationpost", now.UnixMicro(), create)))
	var stored time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT created_at FROM posts WHERE uri = $1`, uri).Scan(&stored))
	require.True(t, stored.Equal(storedCreatedAt), "fixture: post must predate activation")
	const editCID = "bafyreipreactivationedit"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		editCID, now.Add(time.Second).UnixMicro(), postMentionRecord(t, now.Format(time.RFC3339Nano), handles, recipients))))
	_, _, indexedCID, _, _ := readPV2Post(t, db, uri)
	require.Equal(t, editCID, indexedCID, "fixture: the edit must apply even though notification eligibility fails")
	requirePostMentionFacets(t, db, uri, recipients[0])
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri),
		"a newly stamped edit cannot activate a post created before notifications started")
}

func TestPostNotificationEdit_StoredActivatedPostNotifiesDespiteOldIncomingTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	storedCreatedAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 2)
	create := pv2Record(pv2Community, "Activated post", "No mentions yet")
	create["createdAt"] = storedCreatedAt
	base := time.Now().UnixMicro()
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		"bafyreiactivatedpost", base, create)))
	var stored, activation time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT created_at FROM posts WHERE uri = $1`, uri).Scan(&stored))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT activated_at FROM notification_activation`).Scan(&activation))
	require.True(t, stored.After(activation), "fixture: stored post creation must be after activation")
	const editCID = "bafyreiactivatedpostedit"
	beforeActivation := activation.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		editCID, base+1_000_000, postMentionRecord(t, beforeActivation, handles, recipients))))
	_, _, indexedCID, _, _ := readPV2Post(t, db, uri)
	require.Equal(t, editCID, indexedCID, "fixture: the edit with an old incoming createdAt must apply")
	requirePostMentionFacets(t, db, uri, recipients[0])
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`, uri, recipients[0], editCID),
		"activation must use the stored post created_at, not the incoming edit timestamp")
}

func TestPostNotificationEdit_PreviouslyIneligibleStoredFacetIsNotNew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	prefix := testkit.UniqueID(t)
	bHandle := prefix + "b.test"
	bDID := "did:plc:" + prefix + "b"
	eHandles, eRecipients := postMentionRecipients(t, db, 1)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 2)
	base := time.Now().UnixMicro()
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM users WHERE did = $1`, bDID),
		"fixture: B must be unindexed at creation")
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		"bafyreistoredunindexedb", base, postMentionRecord(t, createdAt, []string{bHandle}, []string{bDID}))))
	requirePostMentionFacets(t, db, uri, bDID)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri),
		"fixture: B must not receive a mention before being indexed")
	insertBridgedUserOnPDS(t, db, bDID, bHandle, bridgedTestNativePDS)
	const editCID = "bafyreiunindexedbedited"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		editCID, base+1_000_000, postMentionRecord(t, createdAt,
			[]string{bHandle, eHandles[0]}, []string{bDID, eRecipients[0]}))))
	_, _, indexedCID, _, _ := readPV2Post(t, db, uri)
	require.Equal(t, editCID, indexedCID, "fixture: the edit must apply after B is indexed")
	requirePostMentionFacets(t, db, uri, bDID, eRecipients[0])
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`, uri, eRecipients[0], editCID),
		"only newly mentioned E gets a mention on the edit")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, uri, bDID),
		"B was in stored facets already, despite having no row from the create")
}

func TestPostNotificationEdit_UpdateGateRequestsReadCommittedExplicitly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 2)
	base := time.Now().UnixMicro()
	create := pv2Record(pv2Community, "Isolation post", "No mentions yet")
	create["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		"bafyreiposteditisolationcreate", base, create)))
	var database string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database))
	_, err := db.ExecContext(ctx,
		"ALTER DATABASE "+pq.QuoteIdentifier(database)+" SET default_transaction_isolation = 'repeatable read'")
	require.NoError(t, err)
	db.SetMaxIdleConns(0) // New sessions must observe the changed database default.
	control, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	var defaultIsolation string
	require.NoError(t, control.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&defaultIsolation))
	require.NoError(t, control.Rollback())
	require.Equal(t, "repeatable read", defaultIsolation,
		"control: a transaction without isolation options must inherit the database default")
	const editCID = "bafyreiposteditisolationupdate"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		editCID, base+1_000_000, postMentionRecord(t, createdAt, handles, recipients))),
		"an edit's erasure gate needs an explicitly READ COMMITTED transaction")
	_, _, indexedCID, _, _ := readPV2Post(t, db, uri)
	require.Equal(t, editCID, indexedCID, "fixture: the edit must apply")
	requirePostMentionFacets(t, db, uri, recipients[0])
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`, uri, recipients[0], editCID),
		"the edit must notify even if a nil-options transaction would inherit REPEATABLE READ")
}

// A mention removed by one edit and re-added by a later one counts as added,
// but its recipient already holds a mention row, so it takes no budget slot.
func TestPostNotificationEdit_ReaddNearCapTakesNoBudgetSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 11)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, fixture, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revs := increasingTIDs(t, 3)
	base := time.Now().UnixMicro()
	const (
		createCID = "bafyreipostreaddcapcreate"
		removeCID = "bafyreipostreaddcapremove"
		readdCID  = "bafyreipostreaddcapreadd"
	)
	readded, fresh, overCap := recipients[8], recipients[9], recipients[10]

	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revs[0],
		createCID, base, postMentionRecord(t, createdAt, handles[:9], recipients[:9]))))
	require.Len(t, postMentionRows(t, db, uri), 9, "fixture: the create must notify all nine mentions")

	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[1],
		removeCID, base+1_000_000, postMentionRecord(t, createdAt, handles[:8], recipients[:8]))))
	_, _, indexedCID, _, _ := readPV2Post(t, db, uri)
	require.Equal(t, removeCID, indexedCID, "fixture: the removing edit must reach the post row")
	require.Len(t, postMentionRows(t, db, uri), 9, "removing a mention must keep its notification row")

	// Facet order puts the re-added recipient ahead of the fresh one, so a
	// re-add that consumed budget would take the last slot.
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revs[2],
		readdCID, base+2_000_000, postMentionRecord(t, createdAt, handles, recipients))))
	_, _, indexedCID, _, _ = readPV2Post(t, db, uri)
	require.Equal(t, readdCID, indexedCID, "fixture: the re-adding edit must reach the post row")
	requirePostMentionFacets(t, db, uri, readded, fresh, overCap)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`, uri, readded, createCID),
		"the re-added recipient keeps exactly its original mention row")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention' AND record_cid = $3`, uri, fresh, readdCID),
		"the newly mentioned recipient takes the tenth slot")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2`, uri, overCap),
		"a mention past the per-record cap gets no notification")
	require.Len(t, postMentionRows(t, db, uri), 10, "the record holds at most ten mention notifications")
}
