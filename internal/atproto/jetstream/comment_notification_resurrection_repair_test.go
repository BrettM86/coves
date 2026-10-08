//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type commentNotificationRepairFixture struct {
	commentAuthorDeleteFixture
	key, uri, firstParentURI, firstParentCID, firstCID, deleteRevision string
	secondPostURI, secondPostCID, secondParentURI, secondParentCID     string
	newParentDID, newParentHandle, mentionedDID, mentionedHandle       string
	otherRecordURI                                                     string
	before, otherRecordBefore                                          []notificationRowSnapshot
}

func newCommentNotificationRepairFixture(t *testing.T, topLevel, sameRoot bool) commentNotificationRepairFixture {
	t.Helper()
	f := commentNotificationRepairFixture{commentAuthorDeleteFixture: newCommentAuthorDeleteFixture(t)}
	f.newParentDID, f.newParentHandle = f.mentionRecipient(t)
	f.mentionedDID, f.mentionedHandle = f.mentionRecipient(t)
	f.firstParentURI, f.firstParentCID = f.postURI, f.postCID
	if !topLevel {
		f.firstParentURI, f.firstParentCID = f.parent(t, revTestAuthor)
	}
	f.secondPostURI, f.secondPostCID = f.postURI, f.postCID
	if !sameRoot && !topLevel {
		key := testkit.TID()
		f.secondPostURI = pv2URI(revTestAuthor, key)
		f.secondPostCID = "bafyrepairsecondpost"
		_, err := f.db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
			VALUES ($1, $2, $3, $4, $5, 'second root', NOW())`,
			f.secondPostURI, f.secondPostCID, key, revTestAuthor, revTestCommunity)
		require.NoError(t, err)
	}
	if topLevel {
		key := testkit.TID()
		f.secondPostURI = "at://" + revTestCommunity + "/social.coves.community.post/" + key
		f.secondPostCID = "bafyrepairlegacypost"
		_, err := f.db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
			VALUES ($1, $2, $3, $4, $5, 'legacy second root', NOW())`,
			f.secondPostURI, f.secondPostCID, key, f.newParentDID, revTestCommunity)
		require.NoError(t, err)
		f.secondParentURI, f.secondParentCID = f.secondPostURI, f.secondPostCID
	} else {
		key := testkit.TID()
		f.secondParentURI = "at://" + f.newParentDID + "/" + CommentCollection + "/" + key
		f.secondParentCID = "bafyrepairsecondparent"
		record := revCommentRecord("C's comment X2", f.secondPostURI, f.secondPostCID, f.secondPostURI, f.secondPostCID)
		record["createdAt"] = f.createdAt
		require.NoError(t, f.consumer.HandleEvent(context.Background(), revCommitEvent(
			f.newParentDID, CommentCollection, "create", key, testkit.TID(), f.secondParentCID,
			time.Now().UnixMicro(), record)), "index C's second parent")
		require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM comments WHERE uri = $1`, f.secondParentURI))
	}
	f.otherRecordURI = f.createReply(t, testkit.TID(), testkit.TID(), "bafyrepairotherrecord",
		f.record(t, f.postURI, f.postCID, f.firstParentURI, f.firstParentCID))
	f.otherRecordBefore = notificationRowsForRecordOrSubject(t, f.db, f.otherRecordURI)
	require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, f.otherRecordURI, f.newParentDID),
		"fixture: C holds a mention from another record")
	f.key = testkit.TID()
	f.firstCID = "bafyrepairoriginal"
	firstRevision := testkit.TID()
	f.uri = f.createReply(t, f.key, firstRevision, f.firstCID,
		f.record(t, f.postURI, f.postCID, f.firstParentURI, f.firstParentCID))
	f.before = notificationRowsForRecordOrSubject(t, f.db, f.uri)
	require.Len(t, f.before, 3, "fixture: B's reply plus D's and C's mentions")
	f.deleteRevision = testkit.TID()
	require.Less(t, firstRevision, f.deleteRevision)
	require.NoError(t, f.consumer.HandleEvent(context.Background(), revCommitEvent(
		revTestCommenter, CommentCollection, "delete", f.key, f.deleteRevision, "", time.Now().UnixMicro(), nil)))
	return f
}

func (f commentNotificationRepairFixture) record(t *testing.T, rootURI, rootCID, parentURI, parentCID string) map[string]interface{} {
	t.Helper()
	content := "A mentions @" + f.mentionedHandle + " @" + f.newParentHandle
	record := revCommentRecord(content, rootURI, rootCID, parentURI, parentCID)
	record["createdAt"] = f.createdAt
	record["facets"] = []interface{}{
		commentMentionFacet(t, content, f.mentionedHandle, f.mentionedDID),
		commentMentionFacet(t, content, f.newParentHandle, f.newParentDID),
	}
	return record
}

func (f commentNotificationRepairFixture) recreatedEvent(t *testing.T, parentURI, parentCID string) *JetstreamEvent {
	t.Helper()
	return f.recreatedEventForRecord(t, f.record(t, f.secondPostURI, f.secondPostCID, parentURI, parentCID))
}

func (f commentNotificationRepairFixture) recreatedEventForRecord(t *testing.T, record map[string]interface{}) *JetstreamEvent {
	t.Helper()
	revision := testkit.TID()
	require.Less(t, f.deleteRevision, revision)
	return revCommitEvent(revTestCommenter, CommentCollection, "create", f.key, revision,
		"bafyrepairrecreated", time.Now().UnixMicro(), record)
}

func repairSnapshotForRecipient(t *testing.T, rows []notificationRowSnapshot, recipient string) notificationRowSnapshot {
	t.Helper()
	for _, row := range rows {
		if row.recipient == recipient {
			return row
		}
	}
	t.Fatalf("fixture: no original notification for %s", recipient)
	return notificationRowSnapshot{}
}

func TestCommentConsumer_DifferentParentResurrectionRepairsKeptRows(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name                                                       string
		topLevel, sameRoot, erased, unsupported, missingLegacyPost bool
		backdated                                                  bool
	}{
		{name: "different root"},
		{name: "same root", sameRoot: true},
		{name: "erased author", erased: true},
		{name: "backdated createdAt", backdated: true},
		{name: "legacy top-level", topLevel: true},
		{name: "missing legacy top-level post", topLevel: true, missingLegacyPost: true},
		{name: "unsupported parent", unsupported: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			f := newCommentNotificationRepairFixture(t, scenario.topLevel, scenario.sameRoot)
			if scenario.missingLegacyPost {
				_, err := f.db.Exec(`DELETE FROM posts WHERE uri = $1`, f.secondPostURI)
				require.NoError(t, err)
				require.Zero(t, countRows(t, f.db, `SELECT count(*) FROM posts WHERE uri = $1`, f.secondPostURI),
					"fixture: the legacy post must be absent before Y is re-created")
			}
			if scenario.erased {
				_, err := f.db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, revTestCommenter)
				require.NoError(t, err)
			}
			parentURI, parentCID := f.secondParentURI, f.secondParentCID
			if scenario.unsupported {
				parentURI = "at://" + revTestAuthor + "/app.bsky.feed.post/" + testkit.TID()
				parentCID = "bafyrepairunsupported"
			}
			record := f.record(t, f.secondPostURI, f.secondPostCID, parentURI, parentCID)
			if scenario.backdated {
				// Older than the freshness window, so fan-out writes no reply for C.
				record["createdAt"] = time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
			}
			require.NoError(t, f.consumer.HandleEvent(context.Background(), f.recreatedEventForRecord(t, record)))
			require.Equal(t, f.otherRecordBefore, notificationRowsForRecordOrSubject(t, f.db, f.otherRecordURI),
				"the repair touches only the resurrected record's rows")
			if scenario.missingLegacyPost {
				var deletedAt sql.NullTime
				var storedParentURI, storedRootURI, storedCID string
				require.NoError(t, f.db.QueryRow(`SELECT deleted_at, parent_uri, root_uri, cid FROM comments WHERE uri = $1`, f.uri).
					Scan(&deletedAt, &storedParentURI, &storedRootURI, &storedCID))
				require.False(t, deletedAt.Valid, "Y must be resurrected despite the missing legacy post")
				require.Equal(t, f.secondPostURI, storedParentURI)
				require.Equal(t, f.secondPostURI, storedRootURI)
				require.Equal(t, "bafyrepairrecreated", storedCID)
			}
			rows := notificationRowsForRecordOrSubject(t, f.db, f.uri)
			require.Zero(t, countRows(t, f.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, f.uri, revTestAuthor),
				"B's reply to the old parent must be removed")
			originalD := repairSnapshotForRecipient(t, f.before, f.mentionedDID)
			originalD.rootPostURI = f.secondPostURI
			require.Contains(t, rows, originalD, "D keeps the original id, CID, subject and sort time at the new root")
			var cRows []notificationRowSnapshot
			for _, row := range rows {
				if row.recipient == f.newParentDID {
					cRows = append(cRows, row)
				}
			}
			if scenario.erased || scenario.backdated || scenario.unsupported || scenario.missingLegacyPost {
				originalC := repairSnapshotForRecipient(t, f.before, f.newParentDID)
				originalC.rootPostURI = f.secondPostURI
				require.ElementsMatch(t, []notificationRowSnapshot{originalD, originalC}, rows,
					"with no reply row written for C, B's old reply is removed and both mentions stay at the new root")
				if scenario.backdated {
					// A second deletion and a fresh same-parent re-create should replace
					// C's kept mention with one reply, not leave both rows behind.
					backdatedRevision := testkit.TID()
					require.Less(t, f.deleteRevision, backdatedRevision)
					// The first re-create's revision is stored by the consumer.
					var indexedRevision string
					require.NoError(t, f.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, f.uri).Scan(&indexedRevision))
					require.Less(t, indexedRevision, backdatedRevision)
					require.NoError(t, f.consumer.HandleEvent(context.Background(), revCommitEvent(
						revTestCommenter, CommentCollection, "delete", f.key, backdatedRevision, "", time.Now().UnixMicro(), nil)))
					freshRevision := testkit.TID()
					require.Less(t, backdatedRevision, freshRevision)
					fresh := f.record(t, f.secondPostURI, f.secondPostCID, f.secondParentURI, f.secondParentCID)
					fresh["createdAt"] = activatedCommentNotificationTime(t, f.db, context.Background())
					require.NoError(t, f.consumer.HandleEvent(context.Background(), revCommitEvent(
						revTestCommenter, CommentCollection, "create", f.key, freshRevision,
						"bafyrepairfreshsameparent", time.Now().UnixMicro(), fresh)))
					var reasons []string
					for _, row := range notificationRowsForRecordOrSubject(t, f.db, f.uri) {
						if row.recipient == f.newParentDID {
							reasons = append(reasons, row.reason)
						}
					}
					require.Equal(t, []string{"commentReply"}, reasons,
						"C must hold one reply, not both the kept mention and the fresh reply")
				}
				return
			}
			require.Len(t, cRows, 1, "C receives exactly one reply rather than a stale mention")
			reason := "commentReply"
			if scenario.topLevel {
				reason = "postReply"
			}
			require.Equal(t, reason, cRows[0].reason)
			require.Equal(t, sql.NullString{String: parentURI, Valid: true}, cRows[0].subjectURI)
			require.Equal(t, sql.NullString{String: "bafyrepairrecreated", Valid: true}, cRows[0].recordCID)
			require.Equal(t, f.secondPostURI, cRows[0].rootPostURI)
			require.NotZero(t, cRows[0].id)
			require.Len(t, rows, 2, "only D's kept mention and C's new reply remain")
		})
	}
}

func TestCommentConsumer_ResurrectionWithoutResolvedReplyGivesFormerReplyRecipientOneRow(t *testing.T) {
	t.Parallel()
	// Y replied directly to B's post P1. The re-create keeps parent P1 but names
	// another post as root, which resolves no reply recipient, and mentions B.
	f := newCommentNotificationRepairFixture(t, true, false)
	content := "A mentions @revauthor.test"
	record := revCommentRecord(content, f.secondPostURI, f.secondPostCID, f.postURI, f.postCID)
	record["createdAt"] = f.createdAt
	record["facets"] = []interface{}{commentMentionFacet(t, content, "revauthor.test", revTestAuthor)}
	require.NoError(t, f.consumer.HandleEvent(context.Background(), f.recreatedEventForRecord(t, record)))
	rows, err := f.db.Query(`SELECT reason FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, f.uri, revTestAuthor)
	require.NoError(t, err)
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		require.NoError(t, rows.Scan(&reason))
		reasons = append(reasons, reason)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"mention"}, reasons,
		"B's stale reply is removed when no reply resolves, so the record gives B one notification")
}

func TestCommentConsumer_ResurrectionUnderNonPostRootKeepsKeptRowsRoot(t *testing.T) {
	t.Parallel()
	f := newCommentNotificationRepairFixture(t, false, false)
	nonPostRoot := "at://" + revTestAuthor + "/app.bsky.feed.post/" + testkit.TID()
	record := f.record(t, nonPostRoot, "bafyrepairnonpostroot", f.secondParentURI, f.secondParentCID)
	require.NoError(t, f.consumer.HandleEvent(context.Background(), f.recreatedEventForRecord(t, record)))
	originalD := repairSnapshotForRecipient(t, f.before, f.mentionedDID)
	originalC := repairSnapshotForRecipient(t, f.before, f.newParentDID)
	require.ElementsMatch(t, []notificationRowSnapshot{originalD, originalC}, notificationRowsForRecordOrSubject(t, f.db, f.uri),
		"B's old reply is removed and the kept mentions keep their post root rather than the non-post root")
}

type failingCommentNotificationRepairRepository struct {
	notifications.Repository
	failure error
}

func (repository *failingCommentNotificationRepairRepository) RepairResurrectedCommentNotificationsTx(
	ctx context.Context, tx *sql.Tx, recordURI, replySubjectURI, rootPostURI string,
) error {
	if err := repository.Repository.RepairResurrectedCommentNotificationsTx(
		ctx, tx, recordURI, replySubjectURI, rootPostURI); err != nil {
		return err
	}
	return repository.failure
}

func TestCommentConsumer_DifferentParentResurrectionRepairFailureRollsBack(t *testing.T) {
	t.Parallel()
	f := newCommentNotificationRepairFixture(t, false, false)
	before := notificationRowsForRecordOrSubject(t, f.db, f.uri)
	var storedRevision string
	require.NoError(t, f.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, f.uri).Scan(&storedRevision))
	require.Equal(t, f.deleteRevision, storedRevision)
	injected := errors.New("injected resurrection repair failure")
	consumer := NewCommentEventConsumer(postgres.NewCommentRepository(f.db), f.db,
		WithCommentNotifications(&failingCommentNotificationRepairRepository{
			Repository: postgres.NewNotificationRepository(f.db), failure: injected,
		}))
	err := consumer.HandleEvent(context.Background(), f.recreatedEvent(t, f.secondParentURI, f.secondParentCID))
	require.ErrorIs(t, err, injected, "repair failure must abort the resurrection transaction")
	var deletedAt sql.NullTime
	var parentURI string
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at, parent_uri FROM comments WHERE uri = $1`, f.uri).
		Scan(&deletedAt, &parentURI))
	require.True(t, deletedAt.Valid, "Y must remain soft-deleted")
	require.Equal(t, f.firstParentURI, parentURI, "Y must retain its old parent")
	require.Equal(t, before, notificationRowsForRecordOrSubject(t, f.db, f.uri), "repair statements must roll back")
	require.NoError(t, f.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, f.uri).Scan(&storedRevision))
	require.Equal(t, f.deleteRevision, storedRevision, "failed repair must not advance the revision")
}
