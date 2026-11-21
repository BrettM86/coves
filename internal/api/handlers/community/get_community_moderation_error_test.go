package community

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/require"
)

// failingCommunityViewer fails every read.
type failingCommunityViewer struct{ err error }

func (v failingCommunityViewer) ViewerMayModerate(context.Context, string) (bool, error) {
	return false, v.err
}

func (v failingCommunityViewer) ViewerAuthority(context.Context, string, string) (moderation.CommunityViewerAuthority, error) {
	return moderation.CommunityAuthorityNone, v.err
}

func (v failingCommunityViewer) Roster(context.Context, string) (communities.Roster, error) {
	return communities.Roster{}, v.err
}

// TestGetHandler_CommunityModerationReadFailureIsAServerError proves a failed
// community authority read fails community.get, as its sibling viewer reads
// do, rather than serving the community without the viewer's actions.
func TestGetHandler_CommunityModerationReadFailureIsAServerError(t *testing.T) {
	const communityDID = "did:plc:getcommunitymoderationfailure"
	handler := NewGetHandler(&getTestService{
		mockCommunityService: &mockCommunityService{},
		get: func(context.Context, string) (*communities.Community, error) {
			return &communities.Community{DID: communityDID, Handle: "c-failure.coves.social", Name: "failure"}, nil
		},
	}, getTestRepo(), WithCommunityViewerModeration(failingCommunityViewer{err: errors.New("role index unavailable")}))
	request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.get?community="+communityDID, nil)
	request = request.WithContext(middleware.SetTestUserDID(request.Context(), "did:plc:getcommunityfailureviewer"))
	recorder := httptest.NewRecorder()

	handler.HandleGet(recorder, request)

	require.Equal(t, http.StatusInternalServerError, recorder.Code, "body: %s", recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "role index unavailable", "the internal error must not leak")
}
