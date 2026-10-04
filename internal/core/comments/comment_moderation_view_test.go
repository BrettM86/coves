package comments

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const moderationViewInstanceDID = "did:web:coves.social"

func moderationViewFixture(t *testing.T) (Service, *mockCommentRepo, *GetCommentsRequest, *Comment, *Comment) {
	t.Helper()
	postURI := "at://did:plc:postauthor/social.coves.community.postv2/3k7a3dmb5bk2c"
	commentURI := "at://did:plc:commentauthor/social.coves.community.comment/3k7a3dmb5bk2d"
	commentRepo := newMockCommentRepo()
	postRepo := newMockPostRepo()
	postRepo.posts[postURI] = createTestPost(postURI, "did:plc:postauthor", "did:plc:community")
	userRepo := newMockUserRepo()
	userRepo.users["did:plc:commentauthor"] = createTestUser("did:plc:commentauthor", "author.test")
	comment := createTestComment(commentURI, "did:plc:commentauthor", "author.test", postURI, postURI, 1)
	comment.Content = "secret moderation-view content"
	comment.RKey = "3k7a3dmb5bk2d"
	comment.UpvoteCount, comment.DownvoteCount, comment.Score = 9, 3, 6
	facets := `[{"index":{"byteStart":0,"byteEnd":6},"features":[{"$type":"app.bsky.richtext.facet#tag","tag":"privateFacetMarker"}]}]`
	comment.ContentFacets = &facets
	embed := `{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"privateImageMarker"},"mimeType":"image/png","size":10},"alt":"privateEmbedMarker"}]}`
	comment.Embed = &embed
	reply := createTestComment("at://did:plc:replyauthor/social.coves.community.comment/3k7a3dmb5bk2e", "did:plc:replyauthor", "reply.test", postURI, commentURI, 0)
	reply.Content = "visible surviving reply"
	commentRepo.listByParentWithHotRankFunc = func(_ context.Context, parentURI, _, _ string, _ int, _ *string, _ string) ([]*Comment, *string, error) {
		if parentURI == postURI {
			return []*Comment{comment}, nil, nil
		}
		return nil, nil, nil
	}
	commentRepo.listByParentsBatchFunc = func(_ context.Context, parentURIs []string, _ string, _ int) (map[string][]*Comment, error) {
		return map[string][]*Comment{commentURI: {reply}}, nil
	}
	request := &GetCommentsRequest{PostURI: postURI, Sort: "new", Depth: 1, Limit: 10}
	return NewCommentService(commentRepo, userRepo, postRepo, newMockCommunityRepo(), nil, nil, nil), commentRepo, request, comment, reply
}

func TestCommentServiceGetCommentsOverlaysRemovalWithoutLeakingContent(t *testing.T) {
	service, repo, request, comment, reply := moderationViewFixture(t)
	repo.activeRemovalsByURI = map[string][]RemovalSource{
		comment.URI: {{AuthorityDID: moderationViewInstanceDID, ScopeKind: "instance"}},
	}
	viewerDID := "did:plc:viewer"
	request.ViewerDID = &viewerDID
	response, err := service.GetComments(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, response.Comments, 1)
	node := response.Comments[0]
	require.NotNil(t, node.Comment)
	view := node.Comment
	assert.True(t, view.IsDeleted)
	assert.Equal(t, stringPointer(DeletionReasonModerator), view.DeletionReason)
	assert.Equal(t, &ModerationView{
		State:   "removed",
		Sources: []ModerationSourceView{{AuthorityDID: moderationViewInstanceDID, Scope: ModerationScopeView{Kind: "instance"}}},
	}, view.Moderation)
	assert.Nil(t, view.Record)
	assert.Nil(t, view.Embed)
	assert.Nil(t, view.Viewer)
	assert.Nil(t, view.DeletedAt)
	require.NotNil(t, view.Author)
	assert.Equal(t, comment.CommenterDID, view.Author.DID)
	assert.Equal(t, "handle.invalid", view.Author.Handle)
	assert.Nil(t, view.Author.DisplayName)
	assert.Nil(t, view.Author.Avatar)
	require.NotNil(t, view.Stats)
	assert.Zero(t, view.Stats.Upvotes)
	assert.Zero(t, view.Stats.Downvotes)
	assert.Zero(t, view.Stats.Score)
	assert.Equal(t, 1, view.Stats.ReplyCount)
	require.Len(t, node.Replies, 1)
	assert.Equal(t, reply.URI, node.Replies[0].Comment.URI)
	assert.Equal(t, reply.Content, node.Replies[0].Comment.Record.(*CommentRecord).Content)
	assert.Nil(t, node.Replies[0].Comment.Moderation)

	serialized, err := json.Marshal(view)
	require.NoError(t, err)
	for _, secret := range []string{comment.Content, "privateFacetMarker", "privateImageMarker", "privateEmbedMarker"} {
		assert.NotContains(t, string(serialized), secret)
	}
	assert.JSONEq(t, `{"state":"removed","sources":[{"authorityDid":"did:web:coves.social","scope":{"kind":"instance"}}]}`, mustMarshalModerationView(t, view.Moderation))
}

func TestCommentServiceGetCommentsPreservesAuthorDeletionAlongsideRemoval(t *testing.T) {
	service, repo, request, comment, _ := moderationViewFixture(t)
	deletedAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	comment.DeletedAt = &deletedAt
	comment.DeletionReason = stringPointer(DeletionReasonAuthor)
	repo.activeRemovalsByURI = map[string][]RemovalSource{comment.URI: {{AuthorityDID: moderationViewInstanceDID, ScopeKind: "instance"}}}
	response, err := service.GetComments(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, response.Comments, 1)
	view := response.Comments[0].Comment
	assert.True(t, view.IsDeleted)
	assert.Equal(t, stringPointer(DeletionReasonAuthor), view.DeletionReason)
	require.NotNil(t, view.Moderation)
	assert.Equal(t, "removed", view.Moderation.State)
	assert.Equal(t, []ModerationSourceView{{AuthorityDID: moderationViewInstanceDID, Scope: ModerationScopeView{Kind: "instance"}}}, view.Moderation.Sources)
	assert.Nil(t, view.Record)
}

func TestCommentServiceGetCommentsOverlaysRemovalOnNestedReply(t *testing.T) {
	service, repo, request, comment, reply := moderationViewFixture(t)
	repo.activeRemovalsByURI = map[string][]RemovalSource{
		reply.URI: {{AuthorityDID: moderationViewInstanceDID, ScopeKind: "instance"}},
	}
	response, err := service.GetComments(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, response.Comments, 1)
	assert.Equal(t, comment.Content, response.Comments[0].Comment.Record.(*CommentRecord).Content)
	assert.Nil(t, response.Comments[0].Comment.Moderation)
	require.Len(t, response.Comments[0].Replies, 1)
	nested := response.Comments[0].Replies[0].Comment
	assert.True(t, nested.IsDeleted)
	assert.Nil(t, nested.Record)
	require.NotNil(t, nested.Moderation)
	assert.Equal(t, "removed", nested.Moderation.State)
}

func TestCommentServiceGetCommentsOmitsModerationForClearComment(t *testing.T) {
	service, _, request, comment, _ := moderationViewFixture(t)
	response, err := service.GetComments(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, response.Comments, 1)
	view := response.Comments[0].Comment
	assert.False(t, view.IsDeleted)
	assert.Nil(t, view.Moderation)
	require.IsType(t, &CommentRecord{}, view.Record)
	assert.Equal(t, comment.Content, view.Record.(*CommentRecord).Content)
	encoded, err := json.Marshal(view)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"moderation"`)
}

func TestCommentServiceGetCommentsFailsClosedOnRemovalLookupError(t *testing.T) {
	service, repo, request, _, _ := moderationViewFixture(t)
	lookupError := errors.New("removal lookup unavailable")
	repo.activeRemovalsErr = lookupError
	response, err := service.GetComments(t.Context(), request)
	require.ErrorIs(t, err, lookupError)
	assert.Nil(t, response, "unchecked content must never be returned")
}

// The removal lookup runs once per thread depth. A failure below the top level
// must fail the request too, or the nested replies would render unchecked.
func TestCommentServiceGetCommentsFailsClosedOnNestedRemovalLookupError(t *testing.T) {
	service, repo, request, _, reply := moderationViewFixture(t)
	lookupError := errors.New("nested removal lookup unavailable")
	repo.activeRemovalsErrFor = map[string]error{reply.URI: lookupError}
	response, err := service.GetComments(t.Context(), request)
	require.ErrorIs(t, err, lookupError)
	assert.Nil(t, response, "unchecked nested content must never be returned")
}

func stringPointer(value string) *string { return &value }

func mustMarshalModerationView(t *testing.T, view *ModerationView) string {
	t.Helper()
	encoded, err := json.Marshal(view)
	require.NoError(t, err)
	return string(encoded)
}
