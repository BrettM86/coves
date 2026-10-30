package comments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/moderation"
	"Coves/internal/core/votes"

	core "Coves/internal/core/comments"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// viewerVotesCall is one recorded ViewerVotes call.
type viewerVotesCall struct {
	viewerDID   string
	subjectURIs []string
}

// recordingVoteService answers ViewerVotes from a configured map and records
// every call. The embedded nil votes.Service makes any write method panic: a
// read handler must never write.
type recordingVoteService struct {
	votes.Service
	result map[string]votes.ViewerVote
	calls  []viewerVotesCall
}

func (s *recordingVoteService) ViewerVotes(_ context.Context, viewerDID string, subjectURIs []string) (map[string]votes.ViewerVote, error) {
	s.calls = append(s.calls, viewerVotesCall{viewerDID: viewerDID, subjectURIs: append([]string(nil), subjectURIs...)})
	return s.result, nil
}

func TestGetComments_ResolvesViewerVotesOncePerResponse(t *testing.T) {
	const viewer = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
	const topLevelURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.comment/toplevel"
	const nestedReplyURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.comment/nested"
	const removedURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.comment/removed"
	const deletedURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.comment/deleted"
	const topLevelVoteURI = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.feed.vote/toplevel"
	const nestedReplyVoteURI = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.feed.vote/nested"
	const removedVoteURI = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.feed.vote/removed"

	response := &core.GetCommentsResponse{
		Post: map[string]string{"uri": testPostURI},
		Comments: []*core.ThreadViewComment{
			{Comment: &core.CommentView{URI: topLevelURI}, Replies: []*core.ThreadViewComment{
				nil,
				{Comment: &core.CommentView{URI: removedURI, Moderation: &core.ModerationView{State: "removed"}}, Replies: []*core.ThreadViewComment{
					{Comment: nil, Replies: []*core.ThreadViewComment{{Comment: &core.CommentView{URI: nestedReplyURI}}}},
				}},
			}},
			{Comment: &core.CommentView{URI: deletedURI, IsDeleted: true}},
		},
	}
	service := &recordingVoteService{result: map[string]votes.ViewerVote{
		topLevelURI:    {Direction: "up", URI: topLevelVoteURI},
		nestedReplyURI: {Direction: "down", URI: nestedReplyVoteURI},
		removedURI:     {Direction: "up", URI: removedVoteURI},
	}}
	request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.comment.getComments?post="+testPostURI, nil)
	request = request.WithContext(middleware.SetTestOAuthSession(middleware.SetTestUserDID(request.Context(), viewer),
		&oauthlib.ClientSessionData{AccountDID: syntax.DID(viewer)}))
	recorder := httptest.NewRecorder()

	NewGetCommentsHandler(&fakeCommentService{resp: response}, service, moderation.NewAllowlistAuthority(nil), "did:plc:testinstance").
		HandleGetComments(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	if assert.Len(t, service.calls, 1, "ViewerVotes calls per response") {
		assert.Equal(t, viewer, service.calls[0].viewerDID)
		assert.ElementsMatch(t, []string{topLevelURI, nestedReplyURI, removedURI, deletedURI}, service.calls[0].subjectURIs)
	}
	var got core.GetCommentsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	up, down := "up", "down"
	topLevelVote, nestedReplyVote, removedVote := topLevelVoteURI, nestedReplyVoteURI, removedVoteURI
	assert.Equal(t, &core.CommentViewerState{Vote: &up, VoteURI: &topLevelVote}, got.Comments[0].Comment.Viewer, "top-level comment")
	assert.Equal(t, &core.CommentViewerState{Vote: &up, VoteURI: &removedVote}, got.Comments[0].Replies[1].Comment.Viewer, "removed placeholder")
	assert.Equal(t, &core.CommentViewerState{Vote: &down, VoteURI: &nestedReplyVote}, got.Comments[0].Replies[1].Replies[0].Replies[0].Comment.Viewer, "nested reply")
	assert.Nil(t, got.Comments[1].Comment.Viewer, "deleted placeholder without a vote")
}
