//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mentionEditRecipient struct {
	did, handle string
}

type mentionEditFixture struct {
	gate      notificationGateFixture
	consumer  *CommentEventConsumer
	post      notificationGatePost
	key, uri  string
	revision  string
	createdAt string
	parentURI string
	parentCID string
}

func newMentionEditFixture(t *testing.T) mentionEditFixture {
	t.Helper()
	gate := newNotificationGateFixture(t, bridgedTestNativePDS, bridgedTestNativePDS, false)
	return mentionEditFixture{
		gate: gate, consumer: gate.commentConsumer(), post: gate.posts[0],
		key: testkit.TID(), createdAt: activatedCommentNotificationTime(t, gate.db, context.Background()),
		parentURI: gate.posts[0].uri, parentCID: gate.posts[0].cid,
	}
}

func (fixture *mentionEditFixture) recipient(t *testing.T) mentionEditRecipient {
	t.Helper()
	return fixture.recipientOnPDS(t, bridgedTestNativePDS)
}

func (fixture *mentionEditFixture) recipientOnPDS(t *testing.T, pdsURL string) mentionEditRecipient {
	t.Helper()
	id := testkit.UniqueID(t)
	recipient := mentionEditRecipient{did: "did:plc:" + id + "mentioned", handle: id + "mentioned.test"}
	_, err := fixture.gate.db.ExecContext(context.Background(),
		`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
		recipient.did, recipient.handle, pdsURL)
	require.NoError(t, err, "fixture: the mention recipient must be indexed")
	return recipient
}

func (fixture *mentionEditFixture) postAuthor(t *testing.T) mentionEditRecipient {
	t.Helper()
	var handle string
	require.NoError(t, fixture.gate.db.QueryRowContext(context.Background(),
		`SELECT handle FROM users WHERE did = $1`, fixture.post.authorDID).Scan(&handle))
	return mentionEditRecipient{did: fixture.post.authorDID, handle: handle}
}

func (fixture *mentionEditFixture) record(t *testing.T, createdAt string, recipients ...mentionEditRecipient) map[string]interface{} {
	t.Helper()
	content := "A's edited comment"
	for _, recipient := range recipients {
		content += " @" + recipient.handle
	}
	record := revCommentRecord(content, fixture.post.uri, fixture.post.cid, fixture.parentURI, fixture.parentCID)
	record["createdAt"] = createdAt
	if len(recipients) > 0 {
		facets := make([]interface{}, 0, len(recipients))
		for _, recipient := range recipients {
			facets = append(facets, commentMentionFacet(t, content, recipient.handle, recipient.did))
		}
		record["facets"] = facets
	}
	return record
}

func (fixture *mentionEditFixture) create(t *testing.T, recipients ...mentionEditRecipient) {
	t.Helper()
	fixture.uri = "at://" + fixture.gate.commenterDID + "/" + CommentCollection + "/" + fixture.key
	fixture.revision = testkit.TID()
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		fixture.gate.commenterDID, CommentCollection, "create", fixture.key, fixture.revision,
		"bafyreimentioneditcreate", time.Now().UnixMicro(), fixture.record(t, fixture.createdAt, recipients...),
	)), "fixture: index the original comment before editing")
	require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM comments WHERE uri = $1`, fixture.uri))
	if len(recipients) > 0 {
		dids := make([]string, 0, len(recipients))
		for _, recipient := range recipients {
			dids = append(dids, recipient.did)
		}
		requireStoredMentionFacets(t, fixture.gate.db, fixture.uri, dids...)
	}
}

func (fixture *mentionEditFixture) update(t *testing.T, createdAt string, eventTime int64, recipients ...mentionEditRecipient) error {
	t.Helper()
	revision := testkit.TID()
	require.Less(t, fixture.revision, revision, "fixture: update revision must be newer")
	fixture.revision = revision
	return fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
		fixture.gate.commenterDID, CommentCollection, "update", fixture.key, revision,
		"bafyreimentioneditupdate", eventTime, fixture.record(t, createdAt, recipients...),
	))
}

func mentionEditRows(t *testing.T, fixture mentionEditFixture, recipient mentionEditRecipient, reason string) int {
	t.Helper()
	return countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND recipient_did = $2 AND reason = $3`, fixture.uri, recipient.did, reason)
}

// Hold only the comment row in the two concurrent-update guards. The fixture
// connection also observes pg_stat_activity while the other two pool connections
// run HandleEvent and any in-flight query.
func mentionEditRowTransaction(t *testing.T, ctx context.Context, db *sql.DB) (*sql.Tx, int) {
	t.Helper()
	connection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	transaction, err := connection.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackError := transaction.Rollback()
		require.True(t, rollbackError == nil || errors.Is(rollbackError, sql.ErrTxDone),
			"rolling back comment-row fixture transaction: %v", rollbackError)
	})
	var processID int
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&processID))
	return transaction, processID
}

func TestCommentConsumer_MentionEdit_AddRemoveAndReadd(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	postAuthor := fixture.postAuthor(t)
	kept := fixture.recipient(t)
	added := fixture.recipient(t)
	fixture.create(t, kept)
	require.Equal(t, 1, mentionEditRows(t, fixture, kept, "mention"), "fixture: B received the original mention")
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), kept, added))
	assert.Equal(t, 1, mentionEditRows(t, fixture, kept, "mention"), "keeping B must not duplicate B's mention")
	require.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"), "edit-added E must receive exactly one mention")
	var actorDID, recordCID, rootPostURI string
	var subjectURI sql.NullString
	var recordCreatedAt time.Time
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT actor_did, record_cid, subject_uri, root_post_uri, record_created_at
		FROM notifications WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, fixture.uri, added.did).
		Scan(&actorDID, &recordCID, &subjectURI, &rootPostURI, &recordCreatedAt))
	assert.Equal(t, fixture.gate.commenterDID, actorDID)
	assert.Equal(t, "bafyreimentioneditupdate", recordCID, "an edit mention points to the new CID")
	assert.False(t, subjectURI.Valid)
	assert.Equal(t, fixture.post.uri, rootPostURI)
	createdAt, err := time.Parse(time.RFC3339Nano, fixture.createdAt)
	require.NoError(t, err)
	assert.True(t, recordCreatedAt.Equal(createdAt))
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(2*time.Second).UnixMicro(), added))
	assert.Equal(t, 1, mentionEditRows(t, fixture, kept, "mention"), "removing B must retain its historical notification")
	assert.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"))
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(3*time.Second).UnixMicro(), added, kept))
	assert.Equal(t, 1, mentionEditRows(t, fixture, kept, "mention"), "re-adding B must not duplicate its existing row")
	assert.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"))
	assert.Equal(t, 1, mentionEditRows(t, fixture, postAuthor, "postReply"))
}

func TestCommentConsumer_MentionEdit_DiffUsesStoredFacets(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	blocked := fixture.recipient(t)
	added := fixture.recipient(t)
	blockURI := "at://" + blocked.did + "/" + CovesActorBlockCollection + "/" + testkit.TID()
	_, err := fixture.gate.db.ExecContext(context.Background(),
		`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
		blocked.did, fixture.gate.commenterDID, blockURI, "bafyreimentioneditblock")
	require.NoError(t, err)
	fixture.create(t, blocked)
	require.Zero(t, mentionEditRows(t, fixture, blocked, "mention"), "fixture: B is blocked but present in stored facets")
	_, err = fixture.gate.db.ExecContext(context.Background(), `DELETE FROM user_blocks WHERE record_uri = $1`, blockURI)
	require.NoError(t, err)
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), blocked, added))
	assert.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"), "newly added eligible E must be notified")
	assert.Zero(t, mentionEditRows(t, fixture, blocked, "mention"), "B was already in stored facets even though its original notification was blocked")
}

// The diff baseline is read under the comment row lock, so facets committed
// while the edit waits for that lock count as already mentioned.
func TestCommentConsumer_MentionEdit_DiffUsesLockedStoredFacets(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	concurrent := fixture.recipient(t)
	added := fixture.recipient(t)
	fixture.create(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	revision := testkit.TID()
	require.Less(t, fixture.revision, revision, "fixture: the edit must pass the revision gate")
	event := revCommitEvent(fixture.gate.commenterDID, CommentCollection, "update", fixture.key,
		revision, "bafyreimentioneditlocked", time.Now().Add(time.Second).UnixMicro(),
		fixture.record(t, fixture.createdAt, concurrent, added))
	concurrentContent := "A's comment @" + concurrent.handle
	concurrentFacets, err := json.Marshal([]interface{}{commentMentionFacet(t, concurrentContent, concurrent.handle, concurrent.did)})
	require.NoError(t, err)

	results := make(chan error, 1)
	started, finished := false, false
	t.Cleanup(func() {
		if started && !finished {
			commentErasureResult(t, ctx, results, "HandleEvent(update) after fixture rollback")
		}
	})
	transaction, processID := mentionEditRowTransaction(t, ctx, fixture.gate.db)
	_, err = transaction.ExecContext(ctx, `UPDATE comments SET content_facets = $1::jsonb WHERE uri = $2`, string(concurrentFacets), fixture.uri)
	require.NoError(t, err)
	started = true
	go func() { results <- fixture.consumer.HandleEvent(ctx, event) }()
	commentErasureBlockedByFixture(t, ctx, transaction, processID, "comments")
	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, results, "HandleEvent(update)")
	finished = true
	assert.Zero(t, mentionEditRows(t, fixture, concurrent, "mention"), "E was in the facets committed while the edit waited for the row lock")
	assert.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"), "newly added D must be notified")
}

func TestCommentConsumer_MentionEdit_TrustedBridgeRecipientExcluded(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	fixture.consumer = fixture.gate.commentConsumer(WithCommentBridgeTrust(NewBridgeTrust([]string{bridgedTestPDS})))
	bridged := fixture.recipientOnPDS(t, bridgedTestPDS)
	control := fixture.recipient(t)
	fixture.create(t)
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), bridged, control))
	assert.Zero(t, mentionEditRows(t, fixture, bridged, "mention"), "a recipient on a trusted bridge PDS must not be notified")
	assert.Equal(t, 1, mentionEditRows(t, fixture, control, "mention"), "the native control must be notified")
}

func TestCommentConsumer_MentionEdit_ReplyRecipientDeduplicated(t *testing.T) {
	t.Parallel()
	t.Run("top-level SQL NULL facets", func(t *testing.T) {
		t.Parallel()
		fixture := newMentionEditFixture(t)
		postAuthor := fixture.postAuthor(t)
		added := fixture.recipient(t)
		fixture.create(t)
		var facets sql.NullString
		require.NoError(t, fixture.gate.db.QueryRow(`SELECT content_facets FROM comments WHERE uri = $1`, fixture.uri).Scan(&facets))
		require.False(t, facets.Valid, "fixture: absent facets must be SQL NULL")
		require.Equal(t, 1, mentionEditRows(t, fixture, postAuthor, "postReply"))
		require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), postAuthor, added))
		assert.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, fixture.uri, postAuthor.did), "reply recipient B keeps only its postReply")
		assert.Equal(t, 1, mentionEditRows(t, fixture, postAuthor, "postReply"))
		assert.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"), "D must receive the edit-added mention")
	})
	t.Run("nested comment reply", func(t *testing.T) {
		t.Parallel()
		fixture := newMentionEditFixture(t)
		parent := fixture.recipient(t)
		added := fixture.recipient(t)
		parentKey := testkit.TID()
		fixture.parentURI = "at://" + parent.did + "/" + CommentCollection + "/" + parentKey
		fixture.parentCID = "bafyreimentioneditparent"
		parentRecord := revCommentRecord("C comments on B's post", fixture.post.uri, fixture.post.cid, fixture.post.uri, fixture.post.cid)
		parentRecord["createdAt"] = fixture.createdAt
		require.NoError(t, fixture.consumer.HandleEvent(context.Background(), revCommitEvent(
			parent.did, CommentCollection, "create", parentKey, testkit.TID(), fixture.parentCID,
			time.Now().UnixMicro(), parentRecord,
		)), "fixture: index C's parent comment")
		require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM comments WHERE uri = $1`, fixture.parentURI))
		fixture.create(t)
		require.Equal(t, 1, mentionEditRows(t, fixture, parent, "commentReply"))
		require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), parent, added))
		assert.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, fixture.uri, parent.did), "parent C keeps only its commentReply")
		assert.Equal(t, 1, mentionEditRows(t, fixture, parent, "commentReply"))
		assert.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"), "D must receive the edit-added mention")
	})
}

func TestCommentConsumer_MentionEdit_OldRecordUsesEditIndexTime(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	postAuthor := fixture.postAuthor(t)
	added := fixture.recipient(t)
	var now time.Time
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT now()`).Scan(&now))
	_, err := fixture.gate.db.Exec(`UPDATE notification_activation SET activated_at = $1`, now.Add(-40*24*time.Hour))
	require.NoError(t, err)
	fixture.createdAt = now.Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	fixture.create(t)
	require.Zero(t, mentionEditRows(t, fixture, postAuthor, "postReply"), "fixture: old reply is freshness-suppressed, though B is eligible")
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), added))
	assert.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"), "an edit to an old comment must notify newly mentioned D")
}

// The resolved reply recipient never gets a mention for the comment, even when
// its reply notification was suppressed at create.
func TestCommentConsumer_MentionEdit_SuppressedReplyRecipientNotMentioned(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	postAuthor := fixture.postAuthor(t)
	added := fixture.recipient(t)
	blockURI := "at://" + postAuthor.did + "/" + CovesActorBlockCollection + "/" + testkit.TID()
	_, err := fixture.gate.db.ExecContext(context.Background(),
		`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
		postAuthor.did, fixture.gate.commenterDID, blockURI, "bafyreimentioneditreplyblock")
	require.NoError(t, err)
	fixture.create(t)
	require.Zero(t, mentionEditRows(t, fixture, postAuthor, "postReply"), "fixture: B's block suppresses the create's postReply")
	_, err = fixture.gate.db.ExecContext(context.Background(), `DELETE FROM user_blocks WHERE record_uri = $1`, blockURI)
	require.NoError(t, err)
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), postAuthor, added))
	assert.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"), "D must receive the edit-added mention")
	assert.Zero(t, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, fixture.uri, postAuthor.did), "B is the resolved reply recipient even with no postReply row")
}

func TestCommentConsumer_MentionEdit_OldJetstreamTimestampSuppressesAddedMention(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	added := fixture.recipient(t)
	fixture.create(t)
	var oldTime, databaseNow time.Time
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT now(), now() - INTERVAL '8 days'`).Scan(&databaseNow, &oldTime))
	require.True(t, oldTime.Before(databaseNow.Add(-7*24*time.Hour)), "fixture: Jetstream time predates the freshness window")
	_, err := fixture.gate.db.Exec(`UPDATE comments SET indexed_at = $1 WHERE uri = $2`, oldTime.Add(-time.Hour), fixture.uri)
	require.NoError(t, err, "fixture: the old edit time must still beat the row's recency watermark")
	var storedIndexedAt time.Time
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT indexed_at FROM comments WHERE uri = $1`, fixture.uri).Scan(&storedIndexedAt))
	require.True(t, storedIndexedAt.Before(oldTime), "fixture: the edit must pass the recency guard")
	require.NoError(t, fixture.update(t, fixture.createdAt, oldTime.UnixMicro(), added))
	var cid, content, revision string
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT cid, content FROM comments WHERE uri = $1`, fixture.uri).Scan(&cid, &content))
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, fixture.uri).Scan(&revision))
	require.Equal(t, "bafyreimentioneditupdate", cid, "old edit must still replace the CID")
	require.Equal(t, "A's edited comment @"+added.handle, content, "old edit must still replace content")
	require.Equal(t, fixture.revision, revision, "old edit must still advance the rev")
	assert.Zero(t, mentionEditRows(t, fixture, added, "mention"), "an edit event eight days old must not notify its new mention")
}

func TestCommentConsumer_MentionEdit_ActivationUsesStoredCreatedAt(t *testing.T) {
	t.Parallel()
	t.Run("stored before activation", func(t *testing.T) {
		t.Parallel()
		fixture := newMentionEditFixture(t)
		added := fixture.recipient(t)
		var activation time.Time
		require.NoError(t, fixture.gate.db.QueryRow(`SELECT activated_at FROM notification_activation`).Scan(&activation))
		fixture.createdAt = activation.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		fixture.create(t)
		require.Zero(t, countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri))
		incomingCreatedAt := time.Now().UTC().Format(time.RFC3339Nano)
		require.NoError(t, fixture.update(t, incomingCreatedAt, time.Now().Add(time.Second).UnixMicro(), added))
		assert.Zero(t, mentionEditRows(t, fixture, added, "mention"), "the incoming createdAt cannot activate a pre-activation stored comment")
	})
	t.Run("stored after activation", func(t *testing.T) {
		t.Parallel()
		fixture := newMentionEditFixture(t)
		added := fixture.recipient(t)
		fixture.create(t)
		storedCreatedAt, err := time.Parse(time.RFC3339Nano, fixture.createdAt)
		require.NoError(t, err)
		require.NoError(t, fixture.update(t, storedCreatedAt.Add(-24*time.Hour).UTC().Format(time.RFC3339Nano), time.Now().Add(time.Second).UnixMicro(), added))
		require.Equal(t, 1, mentionEditRows(t, fixture, added, "mention"), "stored createdAt after activation must permit the edit mention")
		var recordCreatedAt time.Time
		require.NoError(t, fixture.gate.db.QueryRow(`SELECT record_created_at FROM notifications WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, fixture.uri, added.did).Scan(&recordCreatedAt))
		assert.True(t, recordCreatedAt.Equal(storedCreatedAt), "record_created_at must equal the stored timestamp")
	})
}

func TestCommentConsumer_MentionEdit_NonqualifyingUpdates(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"older revision", "older event time", "already deleted", "concurrent delete", "concurrent recency loss"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newMentionEditFixture(t)
			added := fixture.recipient(t)
			fixture.create(t)
			notificationCount := countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			baseTime := time.Now().Add(time.Second).UnixMicro()
			revision := testkit.TID()
			require.Less(t, fixture.revision, revision)
			contentBefore := "A's edited comment"
			event := revCommitEvent(fixture.gate.commenterDID, CommentCollection, "update", fixture.key,
				revision, "bafyreimentioneditguard", baseTime, fixture.record(t, fixture.createdAt, added))
			switch name {
			case "older revision":
				event.Commit.Rev = revA
				require.Less(t, event.Commit.Rev, fixture.revision)
				require.NoError(t, fixture.consumer.HandleEvent(ctx, event))
			case "older event time":
				event.TimeUS = time.Now().Add(-time.Hour).UnixMicro()
				require.NoError(t, fixture.consumer.HandleEvent(ctx, event))
			case "already deleted":
				require.NoError(t, fixture.consumer.HandleEvent(ctx, revCommitEvent(
					fixture.gate.commenterDID, CommentCollection, "delete", fixture.key,
					revision, "", baseTime, nil)))
				notificationCount = countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri)
				event.Commit.Rev = testkit.TID()
				require.Less(t, revision, event.Commit.Rev)
				require.NoError(t, fixture.consumer.HandleEvent(ctx, event))
			case "concurrent delete", "concurrent recency loss":
				results := make(chan error, 1)
				started := false
				finished := false
				t.Cleanup(func() {
					if started && !finished {
						commentErasureResult(t, ctx, results, "HandleEvent(update) after fixture rollback")
					}
				})
				transaction, processID := mentionEditRowTransaction(t, ctx, fixture.gate.db)
				if name == "concurrent delete" {
					_, err := transaction.ExecContext(ctx, `UPDATE comments SET deleted_at = now() WHERE uri = $1`, fixture.uri)
					require.NoError(t, err)
				} else {
					_, err := transaction.ExecContext(ctx, `UPDATE comments SET indexed_at = $1 WHERE uri = $2`, time.UnixMicro(baseTime).Add(time.Hour), fixture.uri)
					require.NoError(t, err)
				}
				started = true
				go func() { results <- fixture.consumer.HandleEvent(ctx, event) }()
				commentErasureBlockedByFixture(t, ctx, transaction, processID, "comments")
				require.NoError(t, transaction.Commit())
				commentErasureResult(t, ctx, results, "HandleEvent(update)")
				finished = true
				if name == "concurrent recency loss" {
					var content string
					require.NoError(t, fixture.gate.db.QueryRow(`SELECT content FROM comments WHERE uri = $1`, fixture.uri).Scan(&content))
					assert.Equal(t, contentBefore, content, "zero-row update preserves original content")
				}
				var storedRevision string
				require.NoError(t, fixture.gate.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, fixture.uri).Scan(&storedRevision))
				assert.Equal(t, fixture.revision, storedRevision, "a superseded update rolls back the incoming revision")
			default:
				t.Fatalf("unhandled nonqualifying update case %q", name)
			}
			assert.Equal(t, notificationCount, countRows(t, fixture.gate.db,
				`SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri),
				"nonqualifying update must not change the record's notification count")
			assert.Zero(t, mentionEditRows(t, fixture, added, "mention"), "nonqualifying update must not leak an eligible edit-added mention")
		})
	}
}

func TestCommentConsumer_MentionEdit_NotificationFailureRollsBack(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	added := fixture.recipient(t)
	fixture.create(t)
	var beforeContent, beforeRevision string
	var beforeFacets sql.NullString
	var beforeIndexedAt time.Time
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT content, content_facets, indexed_at FROM comments WHERE uri = $1`, fixture.uri).Scan(&beforeContent, &beforeFacets, &beforeIndexedAt))
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, fixture.uri).Scan(&beforeRevision))
	injected := errors.New("injected edit notification write failure")
	failing := &failingCommentNotificationRepository{delegate: postgres.NewNotificationRepository(fixture.gate.db), failure: injected}
	fixture.consumer = fixture.gate.commentConsumer(WithCommentNotifications(failing))
	err := fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), added)
	assert.ErrorIs(t, err, injected, "edit notification write failure must reach the caller")
	if assert.Len(t, failing.intents, 1, "eligible E mention must reach ApplyTx before the injected failure") {
		assert.Equal(t, added.did, failing.intents[0].RecipientDID)
		assert.Equal(t, notifications.ReasonMention, failing.intents[0].Reason)
	}
	var afterContent, afterRevision string
	var afterFacets sql.NullString
	var afterIndexedAt time.Time
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT content, content_facets, indexed_at FROM comments WHERE uri = $1`, fixture.uri).Scan(&afterContent, &afterFacets, &afterIndexedAt))
	require.NoError(t, fixture.gate.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, fixture.uri).Scan(&afterRevision))
	assert.Equal(t, beforeContent, afterContent, "failed edit must roll back content")
	assert.Equal(t, beforeFacets, afterFacets, "failed edit must roll back facets")
	assert.True(t, beforeIndexedAt.Equal(afterIndexedAt), "failed edit must roll back indexed_at")
	assert.Equal(t, beforeRevision, afterRevision, "failed edit must roll back its rev claim")
	assert.Zero(t, mentionEditRows(t, fixture, added, "mention"))
}

func TestCommentConsumer_MentionEdit_DeleteFirstWaitsBeforeContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	fixture := newCommentErasureFixture(t, ctx, db)
	priorEvent, priorURI := fixture.reply()
	require.NoError(t, fixture.consumer.HandleEvent(ctx, priorEvent))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1`, priorURI))
	mentionedID := testkit.UniqueID(t)
	mentioned := mentionEditRecipient{did: "did:plc:" + mentionedID + "mentioned", handle: mentionedID + "mentioned.test"}
	_, err := db.ExecContext(ctx, `INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`, mentioned.did, mentioned.handle, bridgedTestNativePDS)
	require.NoError(t, err)
	key := strings.TrimPrefix(priorURI, "at://"+fixture.actorDID+"/"+CommentCollection+"/")
	content := "A adds @" + mentioned.handle
	record := revCommentRecord(content, fixture.postURI, fixture.postCID, fixture.postURI, fixture.postCID)
	record["createdAt"] = fixture.createdAt
	record["facets"] = []interface{}{commentMentionFacet(t, content, mentioned.handle, mentioned.did)}
	event := revCommitEvent(fixture.actorDID, CommentCollection, "update", key,
		testkit.TID(), "bafyreimentionediterasure", time.Now().Add(time.Second).UnixMicro(), record)
	require.Less(t, priorEvent.Commit.Rev, event.Commit.Rev, "fixture: the edit must pass the revision gate")

	deleteResults := make(chan error, 1)
	consumerResults := make(chan error, 1)
	deleteStarted, consumerStarted := false, false
	deleteFinished, consumerFinished := false, false
	t.Cleanup(func() {
		if deleteStarted && !deleteFinished {
			commentErasureResult(t, ctx, deleteResults, "Delete(A) after fixture rollback")
		}
		if consumerStarted && !consumerFinished {
			commentErasureResult(t, ctx, consumerResults, "HandleEvent(edit A) after fixture rollback")
		}
	})
	transaction, fixtureProcessID := commentErasureLockTransaction(t, ctx, db, fixture.actorDID)
	deleteStarted = true
	go func() { deleteResults <- postgres.NewUserRepository(db).Delete(ctx, fixture.actorDID) }()
	deleteProcessID := commentErasureBlockedByFixture(t, ctx, transaction, fixtureProcessID, "DELETE FROM users")
	consumerStarted = true
	go func() { consumerResults <- fixture.consumer.HandleEvent(ctx, event) }()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var waitingBeforeContent bool
		err := transaction.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks waiter
			JOIN pg_locks holder ON holder.locktype = waiter.locktype
				AND holder.database = waiter.database AND holder.classid = waiter.classid
				AND holder.objid = waiter.objid AND holder.objsubid = waiter.objsubid
			WHERE holder.pid = $1 AND waiter.pid NOT IN ($1, $2)
				AND holder.locktype = 'advisory' AND holder.granted AND holder.mode = 'ExclusiveLock'
				AND NOT waiter.granted AND waiter.mode = 'ShareLock'
				AND NOT EXISTS (SELECT 1 FROM pg_locks other WHERE other.pid = waiter.pid
					AND NOT other.granted AND other.locktype IN ('tuple', 'transactionid'))
				AND NOT EXISTS (SELECT 1 FROM pg_locks content WHERE content.pid = waiter.pid
					AND content.relation IN ('comments'::regclass, 'posts'::regclass))
		)`, deleteProcessID, fixtureProcessID).Scan(&waitingBeforeContent)
		return waitingBeforeContent, err
	}, testkit.WithDescription("mention edit waits on erasure ShareLock before locking comments or posts"))
	require.NoError(t, transaction.Commit())
	commentErasureResult(t, ctx, deleteResults, "Delete(A)")
	deleteFinished = true
	commentErasureResult(t, ctx, consumerResults, "HandleEvent(edit A)")
	consumerFinished = true
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE actor_did = $1 AND reason = 'mention'`, fixture.actorDID), "delete-first edit must not notify anyone as erased A")
}

func TestCommentConsumer_MentionEdit_ErasedActor(t *testing.T) {
	t.Parallel()
	fixture := newMentionEditFixture(t)
	added := fixture.recipient(t)
	fixture.create(t)
	_, err := fixture.gate.db.Exec(`INSERT INTO deleted_accounts (did) VALUES ($1)`, fixture.gate.commenterDID)
	require.NoError(t, err, "fixture: retain the comment row and mark A erased")
	require.Equal(t, 1, countRows(t, fixture.gate.db, `SELECT count(*) FROM comments WHERE uri = $1`, fixture.uri))
	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().Add(time.Second).UnixMicro(), added))
	assert.Zero(t, mentionEditRows(t, fixture, added, "mention"), "erased A must not notify eligible E")
}
