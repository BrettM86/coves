//go:build integration

package comments_test

import (
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/commentfeedfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chunk 14 R5: the community report queue hydrates reported comments by URI.
// GetCommentViewsByURIs serves a reply nested two levels under a community
// post, and has no entry for a URI with no indexed comment.
func TestCommentService_GetCommentViewsByURIs(t *testing.T) {
	db, service, _, postURI, topLevel, authorDID := moderationThreadFixture(t)
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	nestedURI := moderationThreadReply(t, db, authorDID, postURI, post.CID, topLevel, "a nested reply under review", time.Now())
	missingURI := "at://" + authorDID + "/" + moderation.CommentCollection + "/3kmissingreply1"

	views, blocked, err := service.GetCommentViewsByURIs(t.Context(), []string{nestedURI, missingURI}, "")

	require.NoError(t, err)
	assert.NotContains(t, views, missingURI, "a URI with no indexed comment has no view")
	assert.Empty(t, blocked, "no comment is blocked for a signed-out viewer")
	require.Contains(t, views, nestedURI, "the nested reply has a view")
	view := views[nestedURI]
	assert.Equal(t, nestedURI, view.URI)
	if assert.NotNil(t, view.Author, "the view names its author") {
		assert.Equal(t, authorDID, view.Author.DID)
	}
	encoded, err := json.Marshal(view.Record)
	require.NoError(t, err)
	var record struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(encoded, &record))
	assert.Equal(t, "a nested reply under review", record.Content, "the view serves the reply's text")
}

// A reported comment whose author is restricted on this instance (here a
// PDS-confirmed takedown) is served to the report queue as the same
// placeholder a thread shows, apart from the author.
func TestCommentService_GetCommentViewsByURIs_RestrictedAuthorPlaceholder(t *testing.T) {
	db, _, _, postURI, topLevel, _ := moderationThreadFixture(t)
	instanceDID := fixtures.InstanceDID()
	service := comments.NewCommentServiceWithPDSFactory(
		postgres.NewCommentRepository(db, postgres.WithAccountRestrictionAuthority(instanceDID)),
		postgres.NewUserRepository(db),
		postgres.NewPostRepository(db, postgres.WithAccountRestrictionAuthority(instanceDID)),
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed(), postgres.WithAccountRestrictionAuthority(instanceDID)),
		nil, nil, comments.WithInstanceDID(instanceDID), comments.WithFeedRegistry(commentfeedfixture.Registry(db)))
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	name := testkit.UniqueIDWithPrefix(t, "restricted")
	restrictedDID := fixtures.DID(name)
	fixtures.User(t, db, name+".test", restrictedDID)
	replyURI := moderationThreadReply(t, db, restrictedDID, postURI, post.CID, topLevel, "restricted author private text", time.Now())
	_, err = db.ExecContext(t.Context(), `INSERT INTO account_status_restrictions (did, status) VALUES ($1, 'takendown')`, restrictedDID)
	require.NoError(t, err)

	thread, err := service.GetComments(t.Context(), &comments.GetCommentsRequest{PostURI: postURI, Sort: "new", Depth: 1, Limit: 10})
	require.NoError(t, err)
	topNode := moderationThreadComment(t, thread, topLevel.URI)
	require.Len(t, topNode.Replies, 1, "the thread serves the restricted reply")
	threadView := topNode.Replies[0].Comment
	require.Equal(t, replyURI, threadView.URI)
	require.NotNil(t, threadView.Moderation, "the thread serves the restricted reply as a placeholder")

	views, _, err := service.GetCommentViewsByURIs(t.Context(), []string{replyURI}, "")

	require.NoError(t, err)
	require.Contains(t, views, replyURI)
	queueView := views[replyURI]
	encoded, err := json.Marshal(queueView)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "restricted author private text", "the queue never serves the restricted author's text")
	queueCopy, threadCopy := *queueView, *threadView
	queueCopy.Author, threadCopy.Author = nil, nil
	queueJSON, err := json.Marshal(queueCopy)
	require.NoError(t, err)
	threadJSON, err := json.Marshal(threadCopy)
	require.NoError(t, err)
	assert.JSONEq(t, string(threadJSON), string(queueJSON), "the queue placeholder matches the thread placeholder apart from the author")
}

// A reported comment whose author the viewer has blocked is served no
// content: GetCommentViewsByURIs gives it no view and marks it blocked, the
// way GetPosts serves a blocked author's post. A comment by an author the
// viewer has not blocked hydrates in the same call.
func TestCommentService_GetCommentViewsByURIs_ViewerBlockedAuthor(t *testing.T) {
	db, service, _, postURI, topLevel, authorDID := moderationThreadFixture(t)
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	blockedName := testkit.UniqueIDWithPrefix(t, "blockedauthor")
	blockedDID := fixtures.DID(blockedName)
	fixtures.User(t, db, blockedName+".test", blockedDID)
	moderatorName := testkit.UniqueIDWithPrefix(t, "blockingmod")
	moderatorDID := fixtures.DID(moderatorName)
	fixtures.User(t, db, moderatorName+".test", moderatorDID)
	blockedURI := moderationThreadReply(t, db, blockedDID, postURI, post.CID, topLevel, "blocked author private text", time.Now())
	visibleURI := moderationThreadReply(t, db, authorDID, postURI, post.CID, topLevel, "unblocked author text", time.Now())
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, 'bafyblock')`,
		moderatorDID, blockedDID, "at://"+moderatorDID+"/social.coves.actor.block/"+testkit.TID())
	require.NoError(t, err)

	views, blocked, err := service.GetCommentViewsByURIs(t.Context(), []string{blockedURI, visibleURI}, moderatorDID)

	require.NoError(t, err)
	encoded, err := json.Marshal(views)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "blocked author private text", "the blocked author's text is never served")
	assert.NotContains(t, views, blockedURI, "a comment by a blocked author has no view")
	assert.Equal(t, map[string]bool{blockedURI: true}, blocked, "the blocked author's comment, and only it, is marked blocked")
	require.Contains(t, views, visibleURI, "a comment by an unblocked author still hydrates")
	assert.Equal(t, visibleURI, views[visibleURI].URI)
}
