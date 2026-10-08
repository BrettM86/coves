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

type commentResurrectionRootFixture struct {
	db             *sql.DB
	consumer       *CommentEventConsumer
	commentKey     string
	commentURI     string
	childReplyURI  string
	firstPostURI   string
	secondPostURI  string
	secondPostCID  string
	createdAt      string
	deleteRevision string
	firstCount     int
	secondCount    int
}

func newCommentResurrectionRootFixture(t *testing.T) commentResurrectionRootFixture {
	t.Helper()
	ctx := context.Background()
	db := testkit.DB(t)
	_, firstPostURI, firstPostCID := setupRevFixtures(t, db)
	// The post consumer opens a pending admission. Make P1 public while
	// leaving P2 without an admission to exercise visibility-independent repair.
	acceptanceKey := testkit.TID()
	result, err := db.ExecContext(ctx, `UPDATE community_post_admissions
		SET status = 'accepted', accepted_cid = $2, evaluated_cid = $2,
			acceptance_uri = $3, acceptance_rkey = $4
		WHERE post_uri = $1`, firstPostURI, firstPostCID,
		"at://"+revTestCommunity+"/social.coves.community.acceptance/"+acceptanceKey, acceptanceKey)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, updated, "fixture: post consumer must open P1's admission")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM community_post_admissions
		WHERE post_uri = $1 AND status = 'accepted'`, firstPostURI), "fixture: first post must be publicly admitted")
	secondPostKey := testkit.TID()
	secondPostURI := pv2URI(revTestAuthor, secondPostKey)
	const secondPostCID = "bafyreiresurrectionsecondpost"
	_, err = db.ExecContext(ctx, `INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, 'unadmitted target', NOW())`,
		secondPostURI, secondPostCID, secondPostKey, revTestAuthor, revTestCommunity)
	require.NoError(t, err)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM community_post_admissions WHERE post_uri = $1`, secondPostURI),
		"fixture: second post exists but is not publicly admitted")
	// Both the comment author and voter are indexed native users; the real vote
	// consumer creates the group from an eligible vote on the indexed comment.
	insertBridgedUserOnPDS(t, db, revTestCommenter, "revcommenter.test", bridgedTestNativePDS)
	insertBridgedUserOnPDS(t, db, revTestVoter, "revvoter.test", bridgedTestNativePDS)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	consumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db,
		WithCommentNotifications(postgres.NewNotificationRepository(db)))
	commentKey := testkit.TID()
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentKey
	createRevision := testkit.TID()
	record := revCommentRecord("B comments on the public first post", firstPostURI, firstPostCID, firstPostURI, firstPostCID)
	record["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(revTestCommenter, CommentCollection,
		"create", commentKey, createRevision, "bafyreiresurrectionrootoriginal", time.Now().UnixMicro(), record)))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM comments WHERE uri = $1 AND deleted_at IS NULL`, commentURI))
	voteConsumer := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db,
		WithVoteNotifications(postgres.NewNotificationRepository(db)))
	deliverGroupVote(t, voteConsumer, revTestVoter, commentURI, "up", createdAt)
	require.Equal(t, 1, groupCount(t, db, revTestCommenter, commentURI), "fixture: real vote must create B's group")
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2 AND root_post_uri = $3 AND record_uri IS NULL`,
		revTestCommenter, commentURI, firstPostURI), "fixture: group must point at the first post")
	// The voter's reply Y to X notifies B with subject X, the same subject as B's group.
	childReplyKey := testkit.TID()
	childReplyURI := "at://" + revTestVoter + "/" + CommentCollection + "/" + childReplyKey
	childRecord := revCommentRecord("voter replies to X", firstPostURI, firstPostCID, commentURI, "bafyreiresurrectionrootoriginal")
	childRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(revTestVoter, CommentCollection,
		"create", childReplyKey, testkit.TID(), "bafyreiresurrectionrootchild", time.Now().UnixMicro(), childRecord)))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
		WHERE reason = 'commentReply' AND recipient_did = $1 AND subject_uri = $2 AND record_uri = $3 AND root_post_uri = $4`,
		revTestCommenter, commentURI, childReplyURI, firstPostURI), "fixture: Y must notify B under the first post")
	deleteRevision := testkit.TID()
	require.Less(t, createRevision, deleteRevision)
	require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(revTestCommenter, CommentCollection,
		"delete", commentKey, deleteRevision, "", time.Now().UnixMicro(), nil)))
	require.Equal(t, 1, groupCount(t, db, revTestCommenter, commentURI), "author delete must preserve the group")
	var firstCount, secondCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT comment_count FROM posts WHERE uri = $1`, firstPostURI).Scan(&firstCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT comment_count FROM posts WHERE uri = $1`, secondPostURI).Scan(&secondCount))
	require.Equal(t, 2, firstCount, "author delete retains X's placeholder in the old parent's count, beside Y")
	require.Zero(t, secondCount)
	return commentResurrectionRootFixture{db: db, consumer: consumer, commentKey: commentKey,
		commentURI: commentURI, childReplyURI: childReplyURI, firstPostURI: firstPostURI,
		secondPostURI: secondPostURI, secondPostCID: secondPostCID, createdAt: createdAt,
		deleteRevision: deleteRevision, firstCount: firstCount, secondCount: secondCount}
}

func (fixture commentResurrectionRootFixture) recreateEvent(t *testing.T, parentURI, parentCID string) *JetstreamEvent {
	t.Helper()
	return fixture.recreateEventUnderRoot(t, fixture.secondPostURI, fixture.secondPostCID, parentURI, parentCID)
}

func (fixture commentResurrectionRootFixture) recreateEventUnderRoot(t *testing.T, rootURI, rootCID, parentURI, parentCID string) *JetstreamEvent {
	t.Helper()
	revision := testkit.TID()
	require.Less(t, fixture.deleteRevision, revision)
	record := revCommentRecord("B re-creates the comment on another root", rootURI, rootCID, parentURI, parentCID)
	record["createdAt"] = fixture.createdAt
	return revCommitEvent(revTestCommenter, CommentCollection, "create", fixture.commentKey, revision,
		"bafyreiresurrectionrootrecreated", time.Now().UnixMicro(), record)
}

func (fixture commentResurrectionRootFixture) assertGroupRoot(t *testing.T, rootURI string) {
	t.Helper()
	require.Equal(t, 1, groupCount(t, fixture.db, revTestCommenter, fixture.commentURI),
		"exactly one upvote group must survive for B and X")
	var storedRoot string
	require.NoError(t, fixture.db.QueryRowContext(context.Background(), `SELECT root_post_uri FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2 AND record_uri IS NULL`,
		revTestCommenter, fixture.commentURI).Scan(&storedRoot))
	require.Equal(t, rootURI, storedRoot, "group navigation must follow the comment's root")
}

func (fixture commentResurrectionRootFixture) assertFailedRecreateRolledBack(t *testing.T) {
	t.Helper()
	var deletedAt sql.NullTime
	var rootURI, parentURI, storedRevision string
	require.NoError(t, fixture.db.QueryRow(`SELECT deleted_at, root_uri, parent_uri FROM comments WHERE uri = $1`,
		fixture.commentURI).Scan(&deletedAt, &rootURI, &parentURI))
	require.True(t, deletedAt.Valid, "failed resurrection leaves X soft-deleted")
	require.Equal(t, fixture.firstPostURI, rootURI)
	require.Equal(t, fixture.firstPostURI, parentURI)
	fixture.assertGroupRoot(t, fixture.firstPostURI)
	var firstCount, secondCount int
	require.NoError(t, fixture.db.QueryRow(`SELECT comment_count FROM posts WHERE uri = $1`, fixture.firstPostURI).Scan(&firstCount))
	require.NoError(t, fixture.db.QueryRow(`SELECT comment_count FROM posts WHERE uri = $1`, fixture.secondPostURI).Scan(&secondCount))
	require.Equal(t, fixture.firstCount, firstCount, "failed resurrection must not change the old parent's count")
	require.Equal(t, fixture.secondCount, secondCount, "failed resurrection must not change the new parent's count")
	require.NoError(t, fixture.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`,
		fixture.commentURI).Scan(&storedRevision))
	require.Equal(t, fixture.deleteRevision, storedRevision, "failed resurrection must not advance the tombstone rev")
}

func TestCommentConsumer_DifferentRootResurrectionRepointsUpvoteGroupOnUnadmittedPost(t *testing.T) {
	t.Parallel()
	fixture := newCommentResurrectionRootFixture(t)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(),
		fixture.recreateEvent(t, fixture.secondPostURI, fixture.secondPostCID)))
	var rootURI string
	var deletedAt sql.NullTime
	require.NoError(t, fixture.db.QueryRow(`SELECT root_uri, deleted_at FROM comments WHERE uri = $1`,
		fixture.commentURI).Scan(&rootURI, &deletedAt))
	require.False(t, deletedAt.Valid, "X must be resurrected")
	require.Equal(t, fixture.secondPostURI, rootURI)
	fixture.assertGroupRoot(t, fixture.secondPostURI)
}

func TestCommentConsumer_DifferentRootResurrectionWithUnsupportedParentRepointsUpvoteGroup(t *testing.T) {
	t.Parallel()
	fixture := newCommentResurrectionRootFixture(t)
	parentURI := "at://" + revTestAuthor + "/app.bsky.feed.post/" + testkit.TID()
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(),
		fixture.recreateEvent(t, parentURI, "bafyreiresurrectionunsupportedparent")),
		"unsupported parent collection must still index the re-created comment")
	var rootURI, storedParentURI string
	var deletedAt sql.NullTime
	require.NoError(t, fixture.db.QueryRow(`SELECT root_uri, parent_uri, deleted_at FROM comments WHERE uri = $1`,
		fixture.commentURI).Scan(&rootURI, &storedParentURI, &deletedAt))
	require.False(t, deletedAt.Valid)
	require.Equal(t, fixture.secondPostURI, rootURI)
	require.Equal(t, parentURI, storedParentURI)
	fixture.assertGroupRoot(t, fixture.secondPostURI)
}

// A comment's root is checked only for AT-URI shape, so a resurrection can name
// a non-post record as its root. root_post_uri must name a post, so the group
// then keeps its old root.
func TestCommentConsumer_DifferentRootResurrectionRepointsUpvoteGroupOnlyOntoAPost(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name     string
		rootURI  func(fixture commentResurrectionRootFixture) string
		wantRoot func(fixture commentResurrectionRootFixture) string
	}{
		{
			name:     "postv2 root is applied",
			rootURI:  func(fixture commentResurrectionRootFixture) string { return fixture.secondPostURI },
			wantRoot: func(fixture commentResurrectionRootFixture) string { return fixture.secondPostURI },
		},
		{
			name: "comment root keeps the old root",
			rootURI: func(commentResurrectionRootFixture) string {
				return "at://" + revTestAuthor + "/" + CommentCollection + "/" + testkit.TID()
			},
			wantRoot: func(fixture commentResurrectionRootFixture) string { return fixture.firstPostURI },
		},
		{
			name: "app.bsky.feed.post root keeps the old root",
			rootURI: func(commentResurrectionRootFixture) string {
				return "at://" + revTestAuthor + "/app.bsky.feed.post/" + testkit.TID()
			},
			wantRoot: func(fixture commentResurrectionRootFixture) string { return fixture.firstPostURI },
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCommentResurrectionRootFixture(t)
			rootURI := scenario.rootURI(fixture)
			require.NoError(t, fixture.consumer.HandleEvent(context.Background(), fixture.recreateEventUnderRoot(t,
				rootURI, "bafyreiresurrectionrootguard", fixture.secondPostURI, fixture.secondPostCID)))
			var storedRootURI string
			var deletedAt sql.NullTime
			require.NoError(t, fixture.db.QueryRow(`SELECT root_uri, deleted_at FROM comments WHERE uri = $1`,
				fixture.commentURI).Scan(&storedRootURI, &deletedAt))
			require.False(t, deletedAt.Valid, "X must be resurrected")
			require.Equal(t, rootURI, storedRootURI, "the comment itself records the root it names")
			fixture.assertGroupRoot(t, scenario.wantRoot(fixture))
		})
	}
}

func TestCommentConsumer_DifferentRootResurrectionRepointsOnlyTheAuthorsUpvoteGroup(t *testing.T) {
	t.Parallel()
	fixture := newCommentResurrectionRootFixture(t)
	// Votes on X only group under X's author; a group on X for anyone else must
	// keep its root when B's resurrection repoints B's own group.
	_, err := fixture.db.Exec(`INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri)
		VALUES ($1, 'upvote', $2, $3)`, revTestVoter, fixture.commentURI, fixture.firstPostURI)
	require.NoError(t, err)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(),
		fixture.recreateEvent(t, fixture.secondPostURI, fixture.secondPostCID)))
	fixture.assertGroupRoot(t, fixture.secondPostURI)
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2 AND root_post_uri = $3`,
		revTestVoter, fixture.commentURI, fixture.firstPostURI), "another recipient's group on X must keep its root")
}

func TestCommentConsumer_DifferentRootResurrectionKeepsChildReplyRoot(t *testing.T) {
	t.Parallel()
	fixture := newCommentResurrectionRootFixture(t)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(),
		fixture.recreateEvent(t, fixture.secondPostURI, fixture.secondPostCID)))
	fixture.assertGroupRoot(t, fixture.secondPostURI)
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM notifications
		WHERE reason = 'commentReply' AND recipient_did = $1 AND subject_uri = $2 AND record_uri = $3 AND root_post_uri = $4`,
		revTestCommenter, fixture.commentURI, fixture.childReplyURI, fixture.firstPostURI),
		"Y's reply notification to B has subject X but must keep Y's own root")
}

type failingCommentUpvoteRootRepository struct {
	notifications.Repository
	failure error
}

// ReplaceUpvoteGroupRootTx applies the real replacement before failing, so only
// the consumer's rollback can restore the group root.
func (repository *failingCommentUpvoteRootRepository) ReplaceUpvoteGroupRootTx(ctx context.Context, tx *sql.Tx, recipientDID, subjectURI, rootPostURI string) error {
	if err := repository.Repository.ReplaceUpvoteGroupRootTx(ctx, tx, recipientDID, subjectURI, rootPostURI); err != nil {
		return err
	}
	return repository.failure
}

func TestCommentConsumer_DifferentRootResurrectionGroupWriteFailureRollsBack(t *testing.T) {
	t.Parallel()
	fixture := newCommentResurrectionRootFixture(t)
	injectedError := errors.New("injected upvote group root replacement failure")
	repository := &failingCommentUpvoteRootRepository{
		Repository: postgres.NewNotificationRepository(fixture.db), failure: injectedError,
	}
	consumer := NewCommentEventConsumer(postgres.NewCommentRepository(fixture.db), fixture.db,
		WithCommentNotifications(repository))
	err := consumer.HandleEvent(context.Background(), fixture.recreateEvent(t, fixture.secondPostURI, fixture.secondPostCID))
	require.ErrorIs(t, err, injectedError, "resurrection must propagate a failed group-root replacement")
	fixture.assertFailedRecreateRolledBack(t)
}

func TestCommentConsumer_DifferentRootResurrectionNotificationFailureRollsBack(t *testing.T) {
	t.Parallel()
	fixture := newCommentResurrectionRootFixture(t)
	injectedError := errors.New("injected different-root resurrection notification failure")
	repository := &failingCommentNotificationRepository{
		delegate: postgres.NewNotificationRepository(fixture.db), failure: injectedError,
	}
	consumer := NewCommentEventConsumer(postgres.NewCommentRepository(fixture.db), fixture.db,
		WithCommentNotifications(repository))
	err := consumer.HandleEvent(context.Background(), fixture.recreateEvent(t, fixture.secondPostURI, fixture.secondPostCID))
	require.ErrorIs(t, err, injectedError, "notification failure must roll back the resurrection")
	require.Len(t, repository.intents, 1, "re-creation on a post must attempt a postReply notification")
	fixture.assertFailedRecreateRolledBack(t)
}
