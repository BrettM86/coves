//go:build integration

package comments_test

import (
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postRemovalComment(t *testing.T, db *sql.DB, commenterDID, postURI, postCID, content string, createdAt time.Time) string {
	t.Helper()
	rkey := testkit.TID()
	uri := "at://" + commenterDID + "/" + moderation.CommentCollection + "/" + rkey
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, $8)
	`, uri, moderatedCommentCID, rkey, commenterDID, postURI, postCID, content, createdAt)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE posts SET comment_count = comment_count + 1 WHERE uri = $1`, postURI)
	require.NoError(t, err)
	return uri
}

func postRemovalActorComments(t *testing.T, service comments.Service, commenterDID, communityDID string, limit int, cursor *string) *comments.GetActorCommentsResponse {
	t.Helper()
	response, err := service.GetActorComments(t.Context(), &comments.GetActorCommentsRequest{
		ActorDID: commenterDID, Community: communityDID, Limit: limit, Cursor: cursor,
	})
	require.NoError(t, err)
	return response
}

func postRemovalFixture(t *testing.T) (*sql.DB, comments.Service, moderation.Service, moderation.StrongRef, string, string, string, string, string) {
	t.Helper()
	db, commentService, moderationService, postURI, _, authorDID := moderationThreadFixture(t)
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	communityDID := post.CommunityDID
	visibleURI := fixtures.Post(t, db, communityDID, authorDID, "unremoved companion thread", 0, time.Now())
	visiblePost, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), visibleURI)
	require.NoError(t, err)
	commenterName := testkit.UniqueIDWithPrefix(t, "rootcommenter")
	commenterDID := fixtures.DID(commenterName)
	fixtures.User(t, db, commenterName+".test", commenterDID)
	first := postRemovalComment(t, db, commenterDID, postURI, post.CID, "first original comment", time.Now().Add(-time.Minute))
	visible := postRemovalComment(t, db, commenterDID, visibleURI, visiblePost.CID, "visible companion comment", time.Now().Add(-2*time.Minute))
	second := postRemovalComment(t, db, commenterDID, postURI, post.CID, "second original comment", time.Now().Add(-3*time.Minute))
	return db, commentService, moderationService, moderation.StrongRef{URI: postURI, CID: post.CID}, communityDID, commenterDID, first, visible, second
}

func removeFixturePost(t *testing.T, service moderation.Service, subject moderation.StrongRef) *moderation.MutationResult {
	t.Helper()
	removed, err := service.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "rootadmina")), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-root", Reason: moderationTestReason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	return removed
}

// labelFixtureRoot applies an active NSFW label, which is a moderation decision
// but not a removal, to the root of comment, so read paths that exclude removed
// roots must still serve it.
func labelFixtureRoot(t *testing.T, db *sql.DB, service moderation.Service, comment string) {
	t.Helper()
	var root moderation.StrongRef
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT root_uri, root_cid FROM comments WHERE uri = $1`, comment).Scan(&root.URI, &root.CID))
	labelled, err := service.LabelContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "rootlabeler")), moderation.LabelContentRequest{
		Subject: root, LabelValue: moderation.LabelNSFW, ExpectedVersion: "v0", IdempotencyKey: "label-root",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, labelled.Outcome)
}

func TestPostRemovalGetCommentsHidesRootForEveryViewer(t *testing.T) {
	db, commentService, moderationService, subject, _, _, _, _, _ := postRemovalFixture(t)
	baseline, err := commentService.GetComments(t.Context(), &comments.GetCommentsRequest{PostURI: subject.URI, Sort: "new", Depth: 1, Limit: 10})
	require.NoError(t, err)
	require.Len(t, baseline.Comments, 3)
	var authorDID string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT author_did FROM posts WHERE uri = $1`, subject.URI).Scan(&authorDID))
	removeFixturePost(t, moderationService, subject)
	for _, viewer := range []struct {
		name string
		did  *string
	}{
		{name: "anonymous"},
		{name: "post author", did: &authorDID},
	} {
		t.Run(viewer.name, func(t *testing.T) {
			missingURI := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + testkit.TID()
			_, missingErr := commentService.GetComments(t.Context(), &comments.GetCommentsRequest{
				PostURI: missingURI, ViewerDID: viewer.did, Sort: "new", Depth: 1, Limit: 10,
			})
			require.ErrorIs(t, missingErr, comments.ErrRootNotFound)
			_, err := commentService.GetComments(t.Context(), &comments.GetCommentsRequest{
				PostURI: subject.URI, ViewerDID: viewer.did, Sort: "new", Depth: 1, Limit: 10,
			})
			require.ErrorIs(t, err, comments.ErrRootNotFound)
		})
	}
}

func TestPostRemovalActorCommentsFiltersBeforePagination(t *testing.T) {
	db, commentService, moderationService, subject, communityDID, commenterDID, first, visible, second := postRemovalFixture(t)
	baseline := postRemovalActorComments(t, commentService, commenterDID, "", 10, nil)
	require.Len(t, baseline.Comments, 3)
	require.Equal(t, []string{first, visible, second}, []string{baseline.Comments[0].URI, baseline.Comments[1].URI, baseline.Comments[2].URI})
	removeFixturePost(t, moderationService, subject)
	labelFixtureRoot(t, db, moderationService, visible)
	for _, filter := range []struct{ name, community string }{{"all communities", ""}, {"community filtered", communityDID}} {
		t.Run(filter.name, func(t *testing.T) {
			whole := postRemovalActorComments(t, commentService, commenterDID, filter.community, 10, nil)
			require.Len(t, whole.Comments, 1, "removed-root comments must be excluded before the page is selected")
			assert.Equal(t, visible, whole.Comments[0].URI)
			assert.Nil(t, whole.Cursor)
			page := postRemovalActorComments(t, commentService, commenterDID, filter.community, 1, nil)
			require.Len(t, page.Comments, 1, "page one must advance past the newer removed-root comment")
			assert.Equal(t, visible, page.Comments[0].URI)
			assert.Nil(t, page.Cursor, "the older removed-root comment must not leak a continuation cursor")
		})
	}
}

func TestPostRemovalRestoreServesIndexedThreadAndActorComments(t *testing.T) {
	db, commentService, moderationService, subject, _, commenterDID, first, visible, second := postRemovalFixture(t)
	before, err := commentService.GetComments(t.Context(), &comments.GetCommentsRequest{PostURI: subject.URI, Sort: "new", Depth: 1, Limit: 10})
	require.NoError(t, err)
	baselinePost, ok := before.Post.(*posts.PostView)
	require.True(t, ok)
	require.NotNil(t, baselinePost.Stats)
	baselineCommentCount := baselinePost.Stats.CommentCount
	removed := removeFixturePost(t, moderationService, subject)
	during := postRemovalComment(t, db, commenterDID, subject.URI, subject.CID, "indexed while root removed", time.Now())
	voterDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "rootvoter"))
	voteRkey := testkit.TID()
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'up', NOW())
	`, "at://"+voterDID+"/social.coves.interaction.vote/"+voteRkey, moderatedCommentCID, voteRkey, voterDID, first, moderatedCommentCID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE comments SET upvote_count = upvote_count + 1, score = score + 1 WHERE uri = $1`, first)
	require.NoError(t, err)
	restored, err := moderationService.RestoreContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "rootadminb")), moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: removed.State.Version,
		IdempotencyKey: "restore-root", Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	thread, err := commentService.GetComments(t.Context(), &comments.GetCommentsRequest{PostURI: subject.URI, Sort: "new", Depth: 1, Limit: 10})
	require.NoError(t, err)
	restoredPost, ok := thread.Post.(*posts.PostView)
	require.True(t, ok)
	require.NotNil(t, restoredPost.Stats)
	assert.Equal(t, baselineCommentCount+1, restoredPost.Stats.CommentCount,
		"a comment indexed during removal must contribute to the restored post's count")
	for _, uri := range []string{first, second, during} {
		view := moderationThreadComment(t, thread, uri).Comment
		require.NotNil(t, view.Record)
		if uri == first {
			assert.Equal(t, 1, view.Stats.Upvotes, "vote indexed during removal must be served")
		}
	}
	assert.Equal(t, "indexed while root removed", moderationThreadComment(t, thread, during).Comment.Record.(*comments.CommentRecord).Content)
	actor := postRemovalActorComments(t, commentService, commenterDID, "", 10, nil)
	require.Len(t, actor.Comments, 4)
	assert.Equal(t, []string{during, first, visible, second}, []string{actor.Comments[0].URI, actor.Comments[1].URI, actor.Comments[2].URI, actor.Comments[3].URI})
}

// The profile's commentCount must agree with the list actor.getComments
// serves: both exclude comments under an instance-removed root and comments
// that are themselves instance-removed, and both keep a comment whose root is
// only labelled.
func TestPostRemovalProfileCommentCountMatchesActorComments(t *testing.T) {
	db, commentService, moderationService, subject, _, commenterDID, _, visible, _ := postRemovalFixture(t)
	var visibleRootURI, visibleRootCID string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT root_uri, root_cid FROM comments WHERE uri = $1`, visible).Scan(&visibleRootURI, &visibleRootCID))
	directlyRemoved := postRemovalComment(t, db, commenterDID, visibleRootURI, visibleRootCID, "comment removed directly", time.Now().Add(-4*time.Minute))
	userRepo := postgres.NewUserRepository(db)
	baseline, err := userRepo.GetProfileStats(t.Context(), commenterDID)
	require.NoError(t, err)
	require.Equal(t, 4, baseline.CommentCount, "fixture: four comments before any removal")

	removeFixturePost(t, moderationService, subject)
	removedComment, err := moderationService.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "commentadmin")), moderation.RemoveContentRequest{
		Subject: moderation.StrongRef{URI: directlyRemoved, CID: moderatedCommentCID}, ExpectedVersion: "v0",
		IdempotencyKey: "remove-comment", Reason: moderationTestReason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removedComment.Outcome)
	labelFixtureRoot(t, db, moderationService, visible)

	listed := postRemovalActorComments(t, commentService, commenterDID, "", 50, nil)
	require.Len(t, listed.Comments, 1)
	assert.Equal(t, visible, listed.Comments[0].URI)
	stats, err := userRepo.GetProfileStats(t.Context(), commenterDID)
	require.NoError(t, err)
	assert.Equal(t, len(listed.Comments), stats.CommentCount,
		"profile commentCount must equal what actor.getComments lists: removed-root and removed comments are excluded from both")
}
