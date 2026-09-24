//go:build integration

package comments_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	moderatedCommentCID  = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	moderatedImageCID    = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	moderationTestReason = "social.coves.moderation.defs#reasonSpam"
)

func moderationThreadFixture(t *testing.T) (*sql.DB, comments.Service, moderation.Service, string, moderation.StrongRef, string) {
	t.Helper()
	db := testkit.DB(t)
	postRepository := postgres.NewPostRepository(db)
	commentRepository := postgres.NewCommentRepository(db)
	service := setupCommentService(db)
	moderationService := moderation.NewService(
		moderation.NewRepositorySubjectReader(postRepository, commentRepository),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
	name := testkit.UniqueIDWithPrefix(t, "threadauthor")
	authorDID := fixtures.DID(name)
	fixtures.User(t, db, name+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "threadcommunity")
	communityDID, err := fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)
	postURI := fixtures.Post(t, db, communityDID, authorDID, "moderation thread fixture", 0, time.Now())
	post, err := postRepository.GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	rkey := testkit.TID()
	subject := moderation.StrongRef{URI: "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey, CID: moderatedCommentCID}
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid,
		                      content, content_facets, embed, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, $8::jsonb, $9::jsonb, NOW())
	`, subject.URI, subject.CID, rkey, authorDID, postURI, post.CID,
		"private original moderation text", `[{"index":{"byteStart":0,"byteEnd":7},"features":[{"$type":"app.bsky.richtext.facet#tag","tag":"privateFacetOnly"}]}]`,
		fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":10},"alt":"privateEmbedOnly"}]}`, moderatedImageCID))
	require.NoError(t, err)
	return db, service, moderationService, postURI, subject, authorDID
}

func moderationThreadReply(t *testing.T, db *sql.DB, authorDID, postURI, rootCID string, parent moderation.StrongRef, content string, createdAt time.Time) string {
	t.Helper()
	rkey := testkit.TID()
	uri := "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, uri, moderatedCommentCID, rkey, authorDID, postURI, rootCID, parent.URI, parent.CID, content, createdAt)
	require.NoError(t, err)
	result, err := db.ExecContext(t.Context(), `UPDATE comments SET reply_count = reply_count + 1 WHERE uri = $1`, parent.URI)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	return uri
}

func moderationThreadComment(t *testing.T, response *comments.GetCommentsResponse, uri string) *comments.ThreadViewComment {
	t.Helper()
	for _, node := range response.Comments {
		if node.Comment.URI == uri {
			return node
		}
	}
	t.Fatalf("comment %s missing from thread", uri)
	return nil
}

func TestModerationCommentThreadPlaceholderPaginatesAndRestoresIndexedActivity(t *testing.T) {
	db, commentService, moderationService, postURI, subject, authorDID := moderationThreadFixture(t)
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	replyURIs := make(map[string]string)
	for i := 0; i < 3; i++ {
		content := fmt.Sprintf("visible reply %d", i)
		replyURIs[moderationThreadReply(t, db, authorDID, postURI, post.CID, subject, content, time.Now().Add(-time.Duration(4-i)*time.Minute))] = content
	}
	threadRequest := &comments.GetCommentsRequest{PostURI: postURI, Sort: "new", Depth: 1, Limit: 10}
	before, err := commentService.GetComments(t.Context(), threadRequest)
	require.NoError(t, err)
	require.Len(t, before.Comments, 1)
	initialView := moderationThreadComment(t, before, subject.URI).Comment
	require.IsType(t, &comments.CommentRecord{}, initialView.Record)
	assert.Equal(t, "private original moderation text", initialView.Record.(*comments.CommentRecord).Content)
	assert.NotNil(t, initialView.Embed)
	initialJSON, err := json.Marshal(initialView)
	require.NoError(t, err)
	assert.Contains(t, string(initialJSON), "privateFacetOnly", "facet must be served before removal for the leak check to be meaningful")
	assert.Contains(t, string(initialJSON), moderatedImageCID, "embed must be served before removal for the leak check to be meaningful")

	adminA := fixtures.DID(testkit.UniqueIDWithPrefix(t, "threadadmina"))
	removed, err := moderationService.RemoveContent(t.Context(), adminA, moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "thread-remove", Reason: moderationTestReason,
	})
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.NotNil(t, removed.Action)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	var originalActionRow string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(a)::text FROM moderation_actions a WHERE id = $1`, removed.Action.ID).Scan(&originalActionRow))

	whileRemoved, err := commentService.GetComments(t.Context(), threadRequest)
	require.NoError(t, err)
	node := moderationThreadComment(t, whileRemoved, subject.URI)
	view := node.Comment
	assert.True(t, view.IsDeleted)
	require.NotNil(t, view.DeletionReason)
	assert.Equal(t, "moderator", *view.DeletionReason)
	require.NotNil(t, view.Moderation)
	assert.Equal(t, "removed", view.Moderation.State)
	assert.Equal(t, []comments.ModerationSourceView{{AuthorityDID: fixtures.InstanceDID(), Scope: comments.ModerationScopeView{Kind: "instance"}}}, view.Moderation.Sources)
	assert.Nil(t, view.Record)
	assert.Nil(t, view.Embed)
	assert.Nil(t, view.Viewer)
	assert.Nil(t, view.DeletedAt)
	require.NotNil(t, view.Author)
	assert.Equal(t, authorDID, view.Author.DID)
	assert.Equal(t, "handle.invalid", view.Author.Handle)
	require.NotNil(t, view.Stats)
	assert.Zero(t, view.Stats.Upvotes)
	assert.Zero(t, view.Stats.Downvotes)
	assert.Zero(t, view.Stats.Score)
	assert.Equal(t, 3, view.Stats.ReplyCount)
	require.Len(t, node.Replies, 3)
	for _, child := range node.Replies {
		assert.Equal(t, replyURIs[child.Comment.URI], child.Comment.Record.(*comments.CommentRecord).Content)
		assert.Nil(t, child.Comment.Moderation)
	}
	serialized, err := json.Marshal(whileRemoved)
	require.NoError(t, err)
	for _, secret := range []string{"private original moderation text", "privateFacetOnly", moderatedImageCID, "privateEmbedOnly"} {
		assert.NotContains(t, string(serialized), secret, "removed comment data must not escape into the served response")
	}
	var indexedContent, indexedFacets, indexedEmbed string
	var deletedAt sql.NullTime
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT content, content_facets::text, embed::text, deleted_at FROM comments WHERE uri = $1`, subject.URI).Scan(&indexedContent, &indexedFacets, &indexedEmbed, &deletedAt))
	assert.Equal(t, "private original moderation text", indexedContent, "moderator removal must remain an overlay")
	assert.Contains(t, indexedFacets, "privateFacetOnly")
	assert.Contains(t, indexedEmbed, moderatedImageCID)
	assert.False(t, deletedAt.Valid)

	// The subtree cursor paginates direct replies, retaining the removed parent on every page.
	firstPage, err := commentService.GetComments(t.Context(), &comments.GetCommentsRequest{
		PostURI: postURI, ParentRkey: strings.TrimPrefix(subject.URI, "at://"+authorDID+"/"+moderation.CommentCollection+"/"),
		Sort: "new", Depth: 1, Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, firstPage.Comments, 1)
	assert.True(t, firstPage.Comments[0].Comment.IsDeleted)
	assert.Equal(t, "removed", firstPage.Comments[0].Comment.Moderation.State)
	require.Len(t, firstPage.Comments[0].Replies, 2)
	require.NotNil(t, firstPage.Cursor)
	assert.True(t, firstPage.Comments[0].HasMore)
	secondPage, err := commentService.GetComments(t.Context(), &comments.GetCommentsRequest{
		PostURI: postURI, ParentRkey: strings.TrimPrefix(subject.URI, "at://"+authorDID+"/"+moderation.CommentCollection+"/"),
		Sort: "new", Depth: 1, Limit: 2, Cursor: firstPage.Cursor,
	})
	require.NoError(t, err)
	require.Len(t, secondPage.Comments, 1)
	assert.True(t, secondPage.Comments[0].Comment.IsDeleted)
	assert.Equal(t, "removed", secondPage.Comments[0].Comment.Moderation.State)
	require.Len(t, secondPage.Comments[0].Replies, 1)
	assert.Nil(t, secondPage.Cursor)
	seen := make(map[string]bool)
	for _, page := range []*comments.GetCommentsResponse{firstPage, secondPage} {
		for _, child := range page.Comments[0].Replies {
			assert.False(t, seen[child.Comment.URI], "a reply must not appear on both pages")
			seen[child.Comment.URI] = true
			assert.Equal(t, replyURIs[child.Comment.URI], child.Comment.Record.(*comments.CommentRecord).Content)
		}
	}
	assert.Len(t, seen, 3)

	voterDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "threadvoter"))
	voteRkey := testkit.TID()
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'up', NOW())
	`, "at://"+voterDID+"/social.coves.interaction.vote/"+voteRkey, moderatedCommentCID, voteRkey, voterDID, subject.URI, subject.CID)
	require.NoError(t, err)
	result, err := db.ExecContext(t.Context(), `UPDATE comments SET upvote_count = upvote_count + 1, score = score + 1 WHERE uri = $1`, subject.URI)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	newReply := moderationThreadReply(t, db, authorDID, postURI, post.CID, subject, "reply indexed during removal", time.Now())
	stillRemoved, err := commentService.GetComments(t.Context(), threadRequest)
	require.NoError(t, err)
	assert.Zero(t, moderationThreadComment(t, stillRemoved, subject.URI).Comment.Stats.Upvotes)
	assert.Equal(t, 4, moderationThreadComment(t, stillRemoved, subject.URI).Comment.Stats.ReplyCount)

	adminB := fixtures.DID(testkit.UniqueIDWithPrefix(t, "threadadminb"))
	restored, err := moderationService.RestoreContent(t.Context(), adminB, moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: removed.State.Version,
		IdempotencyKey: "thread-restore", Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	require.NoError(t, err)
	require.NotNil(t, restored)
	require.NotNil(t, restored.Action)
	assert.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	assert.Equal(t, removed.Action.ID, restored.Action.ReversesActionID)
	var reversedID, restoredActor, originalActionAfter string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT reverses_action_id, actor_did FROM moderation_actions WHERE id = $1`, restored.Action.ID).Scan(&reversedID, &restoredActor))
	assert.Equal(t, removed.Action.ID, reversedID)
	assert.Equal(t, adminB, restoredActor)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(a)::text FROM moderation_actions a WHERE id = $1`, removed.Action.ID).Scan(&originalActionAfter))
	assert.Equal(t, originalActionRow, originalActionAfter, "restoring must not rewrite the original action")

	after, err := commentService.GetComments(t.Context(), threadRequest)
	require.NoError(t, err)
	node = moderationThreadComment(t, after, subject.URI)
	view = node.Comment
	assert.False(t, view.IsDeleted)
	assert.Nil(t, view.Moderation)
	require.IsType(t, &comments.CommentRecord{}, view.Record)
	assert.Equal(t, "private original moderation text", view.Record.(*comments.CommentRecord).Content)
	assert.NotNil(t, view.Embed)
	restoredJSON, err := json.Marshal(view)
	require.NoError(t, err)
	assert.Contains(t, string(restoredJSON), "privateFacetOnly")
	assert.Contains(t, string(restoredJSON), moderatedImageCID)
	assert.Equal(t, 1, view.Stats.Upvotes)
	assert.Equal(t, 1, view.Stats.Score)
	assert.Equal(t, 4, view.Stats.ReplyCount)
	require.Len(t, node.Replies, 4)
	replyFound := false
	for _, child := range node.Replies {
		if child.Comment.URI == newReply {
			replyFound = true
			assert.Equal(t, "reply indexed during removal", child.Comment.Record.(*comments.CommentRecord).Content)
		}
	}
	assert.True(t, replyFound, "a reply indexed during removal must be served after restore")
}

func TestModerationAuthorDeletedCommentRetainsAuthorPlaceholderAfterRemoval(t *testing.T) {
	db, commentService, moderationService, postURI, subject, authorDID := moderationThreadFixture(t)
	_, err := db.ExecContext(t.Context(), `
		UPDATE comments SET deleted_at = NOW(), deletion_reason = 'author', deleted_by = $1 WHERE uri = $2
	`, authorDID, subject.URI)
	require.NoError(t, err)
	indexedCID := subject.CID
	subject.CID = moderatedImageCID // Q5/R7: the author-deleted record accepts a different supplied CID.
	removed, err := moderationService.RemoveContent(t.Context(), fixtures.DID("authordeletedadmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "author-deleted-remove", Reason: moderationTestReason,
	})
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.NotNil(t, removed.Action)
	assert.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	assert.Equal(t, indexedCID, removed.Action.ObservedCID)
	response, err := commentService.GetComments(t.Context(), &comments.GetCommentsRequest{PostURI: postURI, Sort: "new", Depth: 1, Limit: 10})
	require.NoError(t, err)
	require.Len(t, response.Comments, 1)
	view := response.Comments[0].Comment
	assert.True(t, view.IsDeleted)
	require.NotNil(t, view.DeletionReason)
	assert.Equal(t, "author", *view.DeletionReason)
	require.NotNil(t, view.Moderation)
	assert.Equal(t, "removed", view.Moderation.State)
	assert.Equal(t, []comments.ModerationSourceView{{AuthorityDID: fixtures.InstanceDID(), Scope: comments.ModerationScopeView{Kind: "instance"}}}, view.Moderation.Sources)
	assert.Nil(t, view.Record)
	state, err := moderationService.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	assert.Equal(t, moderation.RecordStateDeleted, state.RecordState)
	assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
	require.NotNil(t, state.LocalRemoval)
	assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)
}

func TestModerationActorCommentsOmitsRemovedAndAuthorDeletedAcrossPages(t *testing.T) {
	db, commentService, moderationService, postURI, first, authorDID := moderationThreadFixture(t)
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	secondURI := moderationThreadReply(t, db, authorDID, postURI, post.CID, first, "removed from profile", time.Now().Add(-time.Minute))
	thirdURI := moderationThreadReply(t, db, authorDID, postURI, post.CID, first, "deleted by author", time.Now().Add(-2*time.Minute))
	fourthURI := moderationThreadReply(t, db, authorDID, postURI, post.CID, first, "restored to profile", time.Now().Add(-3*time.Minute))

	list := func(limit int, cursor *string) *comments.GetActorCommentsResponse {
		t.Helper()
		response, err := commentService.GetActorComments(t.Context(), &comments.GetActorCommentsRequest{
			ActorDID: authorDID, Limit: limit, Cursor: cursor,
		})
		require.NoError(t, err)
		return response
	}
	before := list(10, nil)
	require.Len(t, before.Comments, 4, "all four comments must be visible before moderation")
	assert.Equal(t, []string{first.URI, secondURI, thirdURI, fourthURI}, []string{
		before.Comments[0].URI, before.Comments[1].URI, before.Comments[2].URI, before.Comments[3].URI,
	})

	adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "actoradmin"))
	second := moderation.StrongRef{URI: secondURI, CID: moderatedCommentCID}
	removed, err := moderationService.RemoveContent(t.Context(), adminDID, moderation.RemoveContentRequest{
		Subject: second, ExpectedVersion: "v0", IdempotencyKey: "actor-remove-second", Reason: moderationTestReason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	_, err = db.ExecContext(t.Context(), `
		UPDATE comments SET deleted_at = NOW(), deletion_reason = 'author', deleted_by = $1 WHERE uri = $2
	`, authorDID, thirdURI)
	require.NoError(t, err)
	fourth := moderation.StrongRef{URI: fourthURI, CID: moderatedCommentCID}
	removedFourth, err := moderationService.RemoveContent(t.Context(), adminDID, moderation.RemoveContentRequest{
		Subject: fourth, ExpectedVersion: "v0", IdempotencyKey: "actor-remove-fourth", Reason: moderationTestReason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removedFourth.Outcome)
	restored, err := moderationService.RestoreContent(t.Context(), adminDID, moderation.RestoreContentRequest{
		ActionID: removedFourth.Action.ID, ReviewedSubject: &fourth, ExpectedVersion: removedFourth.State.Version,
		IdempotencyKey: "actor-restore-fourth", Reason: moderationTestReason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)

	whole := list(10, nil)
	var wholeURIs []string
	for _, comment := range whole.Comments {
		wholeURIs = append(wholeURIs, comment.URI)
	}
	assert.Equal(t, []string{first.URI, fourthURI}, wholeURIs, "removed and author-deleted comments must be absent from the actor profile")
	assert.Nil(t, whole.Cursor)

	var cursor *string
	var visited []string
	for pageNumber := 0; pageNumber < 3; pageNumber++ {
		page := list(1, cursor)
		require.Len(t, page.Comments, 1, "each profile page must contain a visible comment")
		uri := page.Comments[0].URI
		assert.NotEqual(t, secondURI, uri)
		assert.NotEqual(t, thirdURI, uri)
		assert.NotContains(t, visited, uri, "a visible comment must not appear on two pages")
		visited = append(visited, uri)
		if page.Cursor == nil {
			break
		}
		cursor = page.Cursor
	}
	assert.Equal(t, []string{first.URI, fourthURI}, visited, "cursor traversal must match the unpaged profile without gaps or duplicates")
}
