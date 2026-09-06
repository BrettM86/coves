package common

import (
	"context"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"
	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/require"
)

type voteFeedPost struct{ post *posts.PostView }

func (p voteFeedPost) GetPost() *posts.PostView { return p.post }

func TestPopulateViewerVoteState_UnavailableCachePreservesIndex(t *testing.T) {
	request := authenticatedRequest()
	request = request.WithContext(middleware.SetTestOAuthSession(request.Context(), &oauthlib.ClientSessionData{}))
	service := votes.NewServiceWithPDSFactory(nil, nil, nil, nil)
	direction, uri := "up", "at://did:plc:viewer/social.coves.feed.vote/old"
	selected := &posts.PostView{URI: "selected", Viewer: &posts.ViewerState{Vote: &direction, VoteURI: &uri}}
	unselected := &posts.PostView{URI: "unselected"}
	require.Nil(t, service.GetViewerVotesForSubjects("did:plc:viewer", []string{"selected", "unselected"}))
	PopulateViewerVoteState(context.Background(), request, service, []voteFeedPost{{selected}, {unselected}, {nil}})
	require.NotNil(t, selected.Viewer)
	require.Equal(t, &direction, selected.Viewer.Vote)
	require.Equal(t, &uri, selected.Viewer.VoteURI)
	require.Nil(t, unselected.Viewer)
}
