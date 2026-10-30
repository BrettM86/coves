package common

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type voteFeedPost struct{ post *posts.PostView }

func (p voteFeedPost) GetPost() *posts.PostView { return p.post }

// viewerVotesCall is one recorded ViewerVotes call.
type viewerVotesCall struct {
	viewerDID   string
	subjectURIs []string
}

// recordingVoteService answers ViewerVotes from a configured map or error and
// records every call. The embedded nil votes.Service makes any write method
// panic: display must never write.
type recordingVoteService struct {
	votes.Service
	result map[string]votes.ViewerVote
	err    error
	calls  []viewerVotesCall
}

func (s *recordingVoteService) ViewerVotes(_ context.Context, viewerDID string, subjectURIs []string) (map[string]votes.ViewerVote, error) {
	s.calls = append(s.calls, viewerVotesCall{viewerDID: viewerDID, subjectURIs: append([]string(nil), subjectURIs...)})
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

// signedInVoteRequest is a request carrying what OptionalAuth sets for the
// signed-in did:plc:viewer: the DID and the OAuth session.
func signedInVoteRequest() *http.Request {
	request := authenticatedRequest()
	return request.WithContext(middleware.SetTestOAuthSession(request.Context(),
		&oauthlib.ClientSessionData{AccountDID: syntax.DID("did:plc:viewer")}))
}

const (
	upvotedSubjectURI          = "at://did:plc:author/social.coves.community.postv2/upvoted"
	downvotedSubjectURI        = "at://did:plc:author/social.coves.community.postv2/downvoted"
	unvotedSubjectURI          = "at://did:plc:author/social.coves.community.postv2/unvoted"
	unvotedModeratedSubjectURI = "at://did:plc:author/social.coves.community.postv2/unvotedmoderated"
	upvoteRecordURI            = "at://did:plc:viewer/social.coves.feed.vote/upvote"
	downvoteRecordURI          = "at://did:plc:viewer/social.coves.feed.vote/downvote"
)

// resolvedViewerVotes is the resolver answer for the voted subjects above.
func resolvedViewerVotes() map[string]votes.ViewerVote {
	return map[string]votes.ViewerVote{
		upvotedSubjectURI:   {Direction: "up", URI: upvoteRecordURI},
		downvotedSubjectURI: {Direction: "down", URI: downvoteRecordURI},
	}
}

func stringPointer(value string) *string { return &value }

func TestPopulateViewerVoteState_AppliesResolvedVotes(t *testing.T) {
	allSubjects := []string{upvotedSubjectURI, downvotedSubjectURI, unvotedSubjectURI, unvotedModeratedSubjectURI}
	for _, testCase := range []struct {
		name          string
		request       *http.Request
		resolverError error
		wantCalls     int
		wantUpvoted   *posts.ViewerState
		wantDownvoted *posts.ViewerState
	}{
		{
			name:          "signed-in viewer gets resolved votes",
			request:       signedInVoteRequest(),
			wantCalls:     1,
			wantUpvoted:   &posts.ViewerState{Vote: stringPointer("up"), VoteURI: stringPointer(upvoteRecordURI)},
			wantDownvoted: &posts.ViewerState{Vote: stringPointer("down"), VoteURI: stringPointer(downvoteRecordURI), Moderation: []string{"remove", "label"}},
		},
		{
			name:          "viewer DID without an OAuth session gets resolved votes",
			request:       authenticatedRequest(),
			wantCalls:     1,
			wantUpvoted:   &posts.ViewerState{Vote: stringPointer("up"), VoteURI: stringPointer(upvoteRecordURI)},
			wantDownvoted: &posts.ViewerState{Vote: stringPointer("down"), VoteURI: stringPointer(downvoteRecordURI), Moderation: []string{"remove", "label"}},
		},
		{
			name:          "resolver failure leaves vote state out of the whole page",
			request:       signedInVoteRequest(),
			resolverError: errors.New("index query failed"),
			wantCalls:     1,
			wantUpvoted:   nil,
			wantDownvoted: &posts.ViewerState{Moderation: []string{"remove", "label"}},
		},
		{
			name:          "anonymous request is not resolved",
			request:       httptest.NewRequest(http.MethodGet, "/", nil),
			wantCalls:     0,
			wantUpvoted:   nil,
			wantDownvoted: &posts.ViewerState{Moderation: []string{"remove", "label"}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &recordingVoteService{result: resolvedViewerVotes(), err: testCase.resolverError}
			upvoted := &posts.PostView{URI: upvotedSubjectURI}
			downvoted := &posts.PostView{URI: downvotedSubjectURI, Viewer: &posts.ViewerState{Moderation: []string{"remove", "label"}}}
			unvoted := &posts.PostView{URI: unvotedSubjectURI}
			unvotedModerated := &posts.PostView{URI: unvotedModeratedSubjectURI, Viewer: &posts.ViewerState{Moderation: []string{"remove"}}}

			PopulateViewerVoteState(testCase.request.Context(), testCase.request, service,
				[]voteFeedPost{{upvoted}, {downvoted}, {nil}, {unvoted}, {unvotedModerated}})

			assert.Len(t, service.calls, testCase.wantCalls, "ViewerVotes calls per page")
			if testCase.wantCalls == 1 && len(service.calls) == 1 {
				require.Equal(t, "did:plc:viewer", service.calls[0].viewerDID)
				require.ElementsMatch(t, allSubjects, service.calls[0].subjectURIs)
			}
			require.Equal(t, testCase.wantUpvoted, upvoted.Viewer, "upvoted post")
			require.Equal(t, testCase.wantDownvoted, downvoted.Viewer, "downvoted post")
			require.Nil(t, unvoted.Viewer, "post without a vote")
			require.Equal(t, &posts.ViewerState{Moderation: []string{"remove"}}, unvotedModerated.Viewer, "post without a vote keeps its other viewer fields")
		})
	}
}

func TestPopulateCommentViewerVoteState_AppliesResolvedVotes(t *testing.T) {
	allSubjects := []string{upvotedSubjectURI, downvotedSubjectURI, unvotedSubjectURI, unvotedModeratedSubjectURI}
	for _, testCase := range []struct {
		name          string
		request       *http.Request
		resolverError error
		wantCalls     int
		wantUpvoted   *comments.CommentViewerState
		wantDownvoted *comments.CommentViewerState
	}{
		{
			name:          "signed-in viewer gets resolved votes",
			request:       signedInVoteRequest(),
			wantCalls:     1,
			wantUpvoted:   &comments.CommentViewerState{Vote: stringPointer("up"), VoteURI: stringPointer(upvoteRecordURI)},
			wantDownvoted: &comments.CommentViewerState{Vote: stringPointer("down"), VoteURI: stringPointer(downvoteRecordURI), Moderation: []string{"remove"}},
		},
		{
			name:          "viewer DID without an OAuth session gets resolved votes",
			request:       authenticatedRequest(),
			wantCalls:     1,
			wantUpvoted:   &comments.CommentViewerState{Vote: stringPointer("up"), VoteURI: stringPointer(upvoteRecordURI)},
			wantDownvoted: &comments.CommentViewerState{Vote: stringPointer("down"), VoteURI: stringPointer(downvoteRecordURI), Moderation: []string{"remove"}},
		},
		{
			name:          "resolver failure leaves vote state out of the whole response",
			request:       signedInVoteRequest(),
			resolverError: errors.New("index query failed"),
			wantCalls:     1,
			wantUpvoted:   nil,
			wantDownvoted: &comments.CommentViewerState{Moderation: []string{"remove"}},
		},
		{
			name:          "anonymous request is not resolved",
			request:       httptest.NewRequest(http.MethodGet, "/", nil),
			wantCalls:     0,
			wantUpvoted:   nil,
			wantDownvoted: &comments.CommentViewerState{Moderation: []string{"remove"}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &recordingVoteService{result: resolvedViewerVotes(), err: testCase.resolverError}
			upvoted := &comments.CommentView{URI: upvotedSubjectURI}
			downvoted := &comments.CommentView{URI: downvotedSubjectURI, Viewer: &comments.CommentViewerState{Moderation: []string{"remove"}}}
			unvoted := &comments.CommentView{URI: unvotedSubjectURI}
			unvotedModerated := &comments.CommentView{URI: unvotedModeratedSubjectURI, Viewer: &comments.CommentViewerState{Moderation: []string{"remove"}}}

			PopulateCommentViewerVoteState(testCase.request, service,
				[]*comments.CommentView{upvoted, downvoted, nil, unvoted, unvotedModerated})

			assert.Len(t, service.calls, testCase.wantCalls, "ViewerVotes calls per response")
			if testCase.wantCalls == 1 && len(service.calls) == 1 {
				require.Equal(t, "did:plc:viewer", service.calls[0].viewerDID)
				require.ElementsMatch(t, allSubjects, service.calls[0].subjectURIs)
			}
			require.Equal(t, testCase.wantUpvoted, upvoted.Viewer, "upvoted comment")
			require.Equal(t, testCase.wantDownvoted, downvoted.Viewer, "downvoted comment")
			require.Nil(t, unvoted.Viewer, "comment without a vote")
			require.Equal(t, &comments.CommentViewerState{Moderation: []string{"remove"}}, unvotedModerated.Viewer, "comment without a vote keeps its other viewer fields")
		})
	}
}

// A comment the viewer has not voted on, whose viewer state carries nothing
// else, is serialized without a viewer object.
func TestPopulateCommentViewerVoteState_OmitsEmptyViewerState(t *testing.T) {
	service := &recordingVoteService{result: resolvedViewerVotes()}
	unvoted := &comments.CommentView{URI: unvotedSubjectURI, Viewer: &comments.CommentViewerState{}}

	PopulateCommentViewerVoteState(signedInVoteRequest(), service, []*comments.CommentView{unvoted})

	require.Nil(t, unvoted.Viewer)
}
