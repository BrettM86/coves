//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type postTombstoneFixture struct {
	db        *sql.DB
	post      pv2Fixture
	consumer  *PostEventConsumer
	key, uri  string
	revisions []string
	createdAt string
	handle    string
	recipient string
}

func newPostTombstoneFixture(t *testing.T) postTombstoneFixture {
	t.Helper()
	ctx := context.Background()
	db := testkit.DB(t)
	post := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := postMentionConsumer(db, post, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revisions := increasingTIDs(t, 3)
	const cid = "bafyreiposttombstonecreate"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revisions[1],
		cid, time.Now().UnixMicro(), postMentionRecord(t, createdAt, handles, recipients))))
	require.Equal(t, cid, readPostV2MechanismRow(t, db, uri).CID)
	require.Equal(t, revisions[1], readPostV2MechanismRev(t, db, uri))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE record_uri = $1 AND reason = 'mention' AND recipient_did = $2`, uri, recipients[0]),
		"fixture: create must leave a real mention row for the author delete")
	return postTombstoneFixture{
		db: db, post: post, consumer: consumer, key: key, uri: uri,
		revisions: revisions, createdAt: createdAt, handle: handles[0], recipient: recipients[0],
	}
}

func (fixture postTombstoneFixture) delete(revision string) error {
	return fixture.consumer.HandleEvent(context.Background(), pv2Event(pv2Author, "delete", fixture.key,
		revision, "", time.Now().UnixMicro(), nil))
}

type notificationRowSnapshot struct {
	id          int64
	recipient   string
	reason      string
	recordCID   sql.NullString
	subjectURI  sql.NullString
	rootPostURI string
	sortAt      time.Time
}

func notificationRowsForRecordOrSubject(t *testing.T, db *sql.DB, uri string) []notificationRowSnapshot {
	t.Helper()
	rows, err := db.Query(`SELECT id, recipient_did, reason, record_cid, subject_uri, root_post_uri, sort_at
		FROM notifications WHERE record_uri = $1 OR subject_uri = $1 ORDER BY id`, uri)
	require.NoError(t, err)
	defer rows.Close()
	var snapshots []notificationRowSnapshot
	for rows.Next() {
		var snapshot notificationRowSnapshot
		require.NoError(t, rows.Scan(&snapshot.id, &snapshot.recipient, &snapshot.reason,
			&snapshot.recordCID, &snapshot.subjectURI, &snapshot.rootPostURI, &snapshot.sortAt))
		snapshots = append(snapshots, snapshot)
	}
	require.NoError(t, rows.Err())
	return snapshots
}

func TestPostNotificationTombstone_KeepsDeletedPostsRecordRows(t *testing.T) {
	t.Parallel()
	fixture := newPostTombstoneFixture(t)
	ctx := context.Background()
	secondHandles, secondRecipients := postMentionRecipients(t, fixture.db, 1)
	secondRecord := postMentionRecord(t, fixture.createdAt,
		[]string{fixture.handle, secondHandles[0]}, []string{fixture.recipient, secondRecipients[0]})
	revisions := increasingTIDs(t, 2)
	require.NoError(t, fixture.consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", fixture.key,
		revisions[0], "bafyreiposttombstonetwomentions", time.Now().UnixMicro(), secondRecord)))
	// The edit only adds the second mention; the original row keeps its create CID.
	require.Len(t, postMentionRows(t, fixture.db, fixture.uri), 2,
		"fixture: the post must have two distinct mention recipients")
	otherKey := testkit.TID()
	otherURI := pv2URI(pv2Author, otherKey)
	require.NoError(t, fixture.consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", otherKey,
		testkit.TID(), "bafyreiposttombstoneother", time.Now().UnixMicro(),
		postMentionRecord(t, fixture.createdAt, []string{fixture.handle}, []string{fixture.recipient}))))
	otherRows := postMentionRows(t, fixture.db, otherURI)
	require.Len(t, otherRows, 1, "fixture: other post must also have a mention row")

	// These rows navigate to or group votes for the deleted post, but belong
	// to another record (or have no record_uri). Neither may be swept by URI.
	commentURI := "at://" + fixture.recipient + "/" + CommentCollection + "/" + testkit.TID()
	_, err := fixture.db.ExecContext(ctx, `INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at)
		VALUES ($1, 'commentReply', $2, $3, $4, $5, $5, NOW())`,
		pv2Author, commentURI, "bafyreiposttombstonecomment", fixture.recipient, fixture.uri)
	require.NoError(t, err)
	_, err = fixture.db.ExecContext(ctx, `INSERT INTO notifications
		(recipient_did, reason, subject_uri, root_post_uri)
		VALUES ($1, 'upvote', $2, $2)`, pv2Author, fixture.uri)
	require.NoError(t, err)
	var replyID, groupID int64
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT id FROM notifications WHERE record_uri = $1 AND reason = 'commentReply'`, commentURI).Scan(&replyID))
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT id FROM notifications WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
		pv2Author, fixture.uri).Scan(&groupID))
	before := notificationRowsForRecordOrSubject(t, fixture.db, fixture.uri)
	require.Len(t, before, 4, "fixture: two mentions, another record's reply and the subject's upvote group")
	require.NoError(t, fixture.delete(revisions[1]))
	_, _, _, _, deletedAt := readPV2Post(t, fixture.db, fixture.uri)
	require.NotNil(t, deletedAt, "winning author delete must soft-delete its post")
	require.Equal(t, revisions[1], readPostV2MechanismRev(t, fixture.db, fixture.uri))
	require.Equal(t, before, notificationRowsForRecordOrSubject(t, fixture.db, fixture.uri),
		"both mentions must keep their IDs, recipients, reasons, CIDs, roots and sort times")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE id = $1 AND record_uri = $2 AND root_post_uri = $3 AND subject_uri = $3 AND reason = 'commentReply'`,
		replyID, commentURI, fixture.uri), "another record's reply rooted at the post must survive")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE id = $1 AND reason = 'upvote' AND record_uri IS NULL AND subject_uri = $2`, groupID, fixture.uri),
		"the deleted post's upvote group belongs to its subject, not its record URI")
	require.Equal(t, otherRows, postMentionRows(t, fixture.db, otherURI),
		"a mention from a different post must retain its ID and CID")
}

func TestPostNotificationTombstone_StaleDeleteKeepsPostAndNotifications(t *testing.T) {
	t.Parallel()
	fixture := newPostTombstoneFixture(t)
	before := postMentionRows(t, fixture.db, fixture.uri)
	require.Less(t, fixture.revisions[0], fixture.revisions[1], "fixture: delete loses the rev gate")
	require.NoError(t, fixture.delete(fixture.revisions[0]))
	_, _, cid, _, deletedAt := readPV2Post(t, fixture.db, fixture.uri)
	require.Nil(t, deletedAt, "stale tombstone must leave the post active")
	require.Equal(t, "bafyreiposttombstonecreate", cid)
	require.Equal(t, before, postMentionRows(t, fixture.db, fixture.uri), "stale tombstone must preserve the mention row")
	require.Equal(t, fixture.revisions[1], readPostV2MechanismRev(t, fixture.db, fixture.uri))
}

func TestPostNotificationTombstone_AlreadySoftDeletedKeepsLeftoverMention(t *testing.T) {
	t.Parallel()
	fixture := newPostTombstoneFixture(t)
	_, err := fixture.db.Exec(`UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, fixture.uri)
	require.NoError(t, err)
	before := notificationRowsForRecordOrSubject(t, fixture.db, fixture.uri)
	require.Len(t, before, 1,
		"fixture: a previously soft-deleted post retains a leftover mention")
	require.NoError(t, fixture.delete(fixture.revisions[2]))
	_, _, _, _, deletedAt := readPV2Post(t, fixture.db, fixture.uri)
	require.NotNil(t, deletedAt)
	require.Equal(t, before, notificationRowsForRecordOrSubject(t, fixture.db, fixture.uri),
		"a zero-row tombstone must keep the leftover mention unchanged")
	require.Equal(t, fixture.revisions[2], readPostV2MechanismRev(t, fixture.db, fixture.uri))
}

func TestPostNotificationTombstone_AuthorDeleteMarkerRecordsOnlyPubliclyVisiblePosts(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		status        string
		mismatchedCID bool
		noAdmission   bool
		softDeleted   bool
		stale         bool
		seededMarker  bool
		adminRemoved  bool
		wantMarker    bool
		withdrawLater bool
	}{
		{name: "accepted then withdrawn", status: "accepted", wantMarker: true, withdrawLater: true},
		{name: "accepted with active server-admin removal", status: "accepted", adminRemoved: true, wantMarker: true},
		{name: "accepted already soft deleted", status: "accepted", softDeleted: true, wantMarker: true},
		{name: "pending", status: "pending"},
		{name: "rejected", status: "rejected"},
		{name: "pending reacceptance", status: "pending_reacceptance"},
		{name: "accepted with stale CID", status: "accepted", mismatchedCID: true},
		{name: "no admission", noAdmission: true},
		{name: "stale tombstone", status: "accepted", stale: true},
		{name: "existing author delete marker", status: "accepted", softDeleted: true, seededMarker: true, wantMarker: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fixture := newPostTombstoneFixture(t)
			ctx := context.Background()
			const postCID = "bafyreiposttombstonecreate"
			if testCase.noAdmission {
				_, err := fixture.db.ExecContext(ctx, `DELETE FROM community_post_admissions
					WHERE community_did = $1 AND post_uri = $2`, pv2Community, fixture.uri)
				require.NoError(t, err)
			} else {
				acceptedCID := sql.NullString{}
				acceptanceURI := sql.NullString{}
				acceptanceRKey := sql.NullString{}
				decisionCode := sql.NullString{}
				if testCase.status == "accepted" || testCase.status == "pending_reacceptance" {
					acceptedCID = sql.NullString{String: postCID, Valid: true}
					if testCase.mismatchedCID {
						acceptedCID.String = "bafyreiposttombstonedifferent"
					}
					acceptanceRKey = sql.NullString{String: testkit.TID(), Valid: true}
					acceptanceURI = sql.NullString{String: "at://" + pv2Community + "/social.coves.community.acceptance/" + acceptanceRKey.String, Valid: true}
				}
				if testCase.status == "rejected" {
					decisionCode = sql.NullString{String: "policy", Valid: true}
				}
				result, err := fixture.db.ExecContext(ctx, `UPDATE community_post_admissions
					SET status = $3, accepted_cid = $4, acceptance_uri = $5, acceptance_rkey = $6,
						decision_code = $7, evaluated_cid = $8
					WHERE community_did = $1 AND post_uri = $2`, pv2Community, fixture.uri,
					testCase.status, acceptedCID, acceptanceURI, acceptanceRKey, decisionCode, postCID)
				require.NoError(t, err)
				updated, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, updated, "fixture: the post's own-community admission must exist")
			}
			if testCase.softDeleted {
				_, err := fixture.db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, fixture.uri)
				require.NoError(t, err)
			}
			if testCase.adminRemoved {
				seedPostInstanceRemoval(t, fixture.db, fixture.uri)
			}
			recordedAt := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			if testCase.seededMarker {
				_, err := fixture.db.ExecContext(ctx, `INSERT INTO notification_public_post_withdrawals
					(post_uri, kind, recorded_at) VALUES ($1, 'authorDelete', $2)`, fixture.uri, recordedAt)
				require.NoError(t, err)
			}

			revision := fixture.revisions[2]
			if testCase.stale {
				revision = fixture.revisions[0]
			}
			require.NoError(t, fixture.delete(revision))
			if testCase.stale {
				require.Equal(t, fixture.revisions[1], readPostV2MechanismRev(t, fixture.db, fixture.uri))
				_, _, _, _, deletedAt := readPV2Post(t, fixture.db, fixture.uri)
				require.Nil(t, deletedAt, "stale tombstone must not delete the post")
			} else {
				require.Equal(t, revision, readPostV2MechanismRev(t, fixture.db, fixture.uri))
				_, _, _, _, deletedAt := readPV2Post(t, fixture.db, fixture.uri)
				require.NotNil(t, deletedAt, "winning tombstone must soft-delete the post")
			}
			markers := postAuthorDeleteMarkers(t, fixture.db, fixture.uri)
			if testCase.wantMarker {
				require.Len(t, markers, 1, "publicly visible post must retain one authorDelete marker")
				require.Equal(t, fixture.uri, markers[0].postURI)
				require.Equal(t, "authorDelete", markers[0].kind)
				require.False(t, markers[0].communityRev.Valid)
				if testCase.seededMarker {
					require.Equal(t, recordedAt, markers[0].recordedAt, "conflict must not restamp the existing marker")
				}
			} else {
				require.Empty(t, markers, "post hidden from anonymous viewers must have no authorDelete marker")
			}
			if testCase.withdrawLater {
				result, err := postgres.NewAdmissionRepository(fixture.db).ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
					CommunityDID: pv2Community, PostURI: fixture.uri,
					Watermark: posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpDelete},
				})
				require.NoError(t, err)
				require.Equal(t, posts.AdmissionStatusPending, result.Admission.Status)
				require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM community_post_admissions
					WHERE community_did = $1 AND post_uri = $2 AND status = 'pending' AND accepted_cid IS NULL`, pv2Community, fixture.uri))
				require.Equal(t, markers, postAuthorDeleteMarkers(t, fixture.db, fixture.uri),
					"later acceptance withdrawal must not erase the committed author-delete marker")
			}
		})
	}
}

// seedPostInstanceRemoval records an active instance-scope admin removal of
// postURI: the action row, then the decision it made active.
func seedPostInstanceRemoval(t *testing.T, db *sql.DB, postURI string) {
	t.Helper()
	actionID := "post-tombstone-removal-" + testkit.TID()
	_, err := db.Exec(`INSERT INTO moderation_actions
		(id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, action, origin, created_at)
		VALUES ($1, 'did:plc:posttombstonemoderator', 'did:plc:posttombstoneinstance', 'instance', $2, $3, 'remove', 'local', NOW())`,
		actionID, postURI, posts.PostV2Collection)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO moderation_decisions
		(authority_did, scope_kind, subject_uri, kind, active_action_id, active)
		VALUES ('did:plc:posttombstoneinstance', 'instance', $1, 'removal', $2, true)`, postURI, actionID)
	require.NoError(t, err)
}

type postAuthorDeleteMarker struct {
	postURI      string
	kind         string
	communityRev sql.NullString
	recordedAt   time.Time
}

func postAuthorDeleteMarkers(t *testing.T, db *sql.DB, postURI string) []postAuthorDeleteMarker {
	t.Helper()
	rows, err := db.Query(`SELECT post_uri, kind, community_rev, recorded_at
		FROM notification_public_post_withdrawals WHERE post_uri = $1 AND kind = 'authorDelete'`, postURI)
	require.NoError(t, err)
	defer rows.Close()
	var markers []postAuthorDeleteMarker
	for rows.Next() {
		var marker postAuthorDeleteMarker
		require.NoError(t, rows.Scan(&marker.postURI, &marker.kind, &marker.communityRev, &marker.recordedAt))
		marker.recordedAt = marker.recordedAt.UTC()
		markers = append(markers, marker)
	}
	require.NoError(t, rows.Err())
	return markers
}

func TestPostNotificationTombstone_NeverIndexedPostAdvancesRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	post := newPV2Fixture(t, db)
	consumer := postMentionConsumer(db, post, postgres.NewNotificationRepository(db))
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revision := testkit.TID()
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri), "fixture: post has never been indexed")
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "delete", key, revision, "", time.Now().UnixMicro(), nil)))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri))
	require.Equal(t, revision, readPostV2MechanismRev(t, db, uri), "delete-first tombstone must fence out stale creates")
}

// failingPostTombstoneNotificationRepository runs the real repository call at
// failurePoint, then returns failure, so the injected error arrives after the
// real statement has already touched the transaction.
type failingPostTombstoneNotificationRepository struct {
	notifications.Repository
	failurePoint string
	failure      error
}

const (
	postTombstoneFailAtErasureGate = "erasure"
	postTombstoneFailAtMarker      = "marker"
)

func (repository *failingPostTombstoneNotificationRepository) ErasureGateTx(ctx context.Context, tx *sql.Tx, did string) (bool, error) {
	erased, err := repository.Repository.ErasureGateTx(ctx, tx, did)
	if err != nil || repository.failurePoint != postTombstoneFailAtErasureGate {
		return erased, err
	}
	return false, repository.failure
}

func (repository *failingPostTombstoneNotificationRepository) RecordPostAuthorDeleteWithdrawalTx(ctx context.Context, tx *sql.Tx, uri string) error {
	if err := repository.Repository.RecordPostAuthorDeleteWithdrawalTx(ctx, tx, uri); err != nil {
		return err
	}
	if repository.failurePoint != postTombstoneFailAtMarker {
		return nil
	}
	return repository.failure
}

type recordingPostTombstoneIsolationRepository struct {
	notifications.Repository
	isolation string
}

func (repository *recordingPostTombstoneIsolationRepository) ErasureGateTx(ctx context.Context, tx *sql.Tx, did string) (bool, error) {
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&repository.isolation); err != nil {
		return false, err
	}
	return repository.Repository.ErasureGateTx(ctx, tx, did)
}

func TestPostNotificationTombstone_FailureRollsBackPostRowsMarkerAndRev(t *testing.T) {
	t.Parallel()
	for _, failurePoint := range []string{postTombstoneFailAtErasureGate, postTombstoneFailAtMarker} {
		t.Run(failurePoint, func(t *testing.T) {
			t.Parallel()
			fixture := newPostTombstoneFixture(t)
			ctx := context.Background()
			acceptanceKey := testkit.TID()
			result, err := fixture.db.ExecContext(ctx, `UPDATE community_post_admissions
				SET status = 'accepted', accepted_cid = $2, evaluated_cid = $2,
					acceptance_uri = $3, acceptance_rkey = $4
				WHERE post_uri = $1 AND community_did = $5`, fixture.uri, "bafyreiposttombstonecreate",
				"at://"+pv2Community+"/social.coves.community.acceptance/"+acceptanceKey, acceptanceKey, pv2Community)
			require.NoError(t, err)
			updated, err := result.RowsAffected()
			require.NoError(t, err)
			require.EqualValues(t, 1, updated, "fixture: public acceptance must match the post CID")
			before := notificationRowsForRecordOrSubject(t, fixture.db, fixture.uri)
			injectedError := errors.New("injected post tombstone " + failurePoint + " failure")
			fixture.consumer = postMentionConsumer(fixture.db, fixture.post, &failingPostTombstoneNotificationRepository{
				Repository:   postgres.NewNotificationRepository(fixture.db),
				failurePoint: failurePoint,
				failure:      injectedError,
			})
			err = fixture.delete(fixture.revisions[2])
			require.ErrorIs(t, err, injectedError, "a failure at the %s step must surface and roll back the entire tombstone", failurePoint)
			_, _, cid, _, deletedAt := readPV2Post(t, fixture.db, fixture.uri)
			require.Nil(t, deletedAt, "a failure at the %s step must leave the post active", failurePoint)
			require.Equal(t, "bafyreiposttombstonecreate", cid)
			require.Equal(t, before, notificationRowsForRecordOrSubject(t, fixture.db, fixture.uri),
				"a failure at the %s step must leave the post's notification rows unchanged", failurePoint)
			require.Equal(t, fixture.revisions[1], readPostV2MechanismRev(t, fixture.db, fixture.uri),
				"a failure at the %s step must roll back the revision gate", failurePoint)
			require.Empty(t, postAuthorDeleteMarkers(t, fixture.db, fixture.uri),
				"a failure at the %s step must roll back the author-delete marker", failurePoint)
		})
	}
}

func TestPostNotificationTombstone_RequestsReadCommittedExplicitly(t *testing.T) {
	t.Parallel()
	fixture := newPostTombstoneFixture(t)
	ctx := context.Background()
	var database string
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database))
	_, err := fixture.db.ExecContext(ctx,
		"ALTER DATABASE "+pq.QuoteIdentifier(database)+" SET default_transaction_isolation = 'repeatable read'")
	require.NoError(t, err)
	fixture.db.SetMaxIdleConns(0) // New sessions must see the changed database default.
	control, err := fixture.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	var defaultIsolation string
	require.NoError(t, control.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&defaultIsolation))
	require.NoError(t, control.Rollback())
	require.Equal(t, "repeatable read", defaultIsolation,
		"control: a transaction without explicit isolation inherits the database default")
	observed := &recordingPostTombstoneIsolationRepository{Repository: postgres.NewNotificationRepository(fixture.db)}
	fixture.consumer = postMentionConsumer(fixture.db, fixture.post, observed)
	require.NoError(t, fixture.delete(fixture.revisions[2]), "tombstone must use READ COMMITTED despite the database default")
	require.Equal(t, "read committed", observed.isolation, "the delete's real erasure gate must run in READ COMMITTED")
	_, _, _, _, deletedAt := readPV2Post(t, fixture.db, fixture.uri)
	require.NotNil(t, deletedAt)
	require.Equal(t, fixture.revisions[2], readPostV2MechanismRev(t, fixture.db, fixture.uri))
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri),
		"the delete must keep its notification rows under READ COMMITTED")
}
