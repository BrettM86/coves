package community

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/moderation/moderationtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetHandler_ServesCommunityViewerModeration proves community.get serves
// viewer.communityModeration from the viewer's community authority: owner
// level for the owner and for an instance admin acting for the community,
// moderator level for a moderator, nothing otherwise. Which actions apply is
// owned by the moderation rule tests.
func TestGetHandler_ServesCommunityViewerModeration(t *testing.T) {
	const (
		communityDID = "did:plc:getcommunitymoderation"
		ownerDID     = "did:plc:getcommunityowner"
		adminDID     = "did:plc:getcommunityadmin"
		moderatorDID = "did:plc:getcommunitymoderator"
		noRoleDID    = "did:plc:getcommunitynorole"
	)
	viewer := moderationtest.CommunityViewer{Authority: map[string]map[string]moderation.CommunityViewerAuthority{communityDID: {
		ownerDID:     moderation.CommunityAuthorityOwner,
		adminDID:     moderation.CommunityAuthorityOwner,
		moderatorDID: moderation.CommunityAuthorityModerator,
	}}}
	ownerActions := []any{"ban-account", "unban-account", "grant-role", "remove-role", "transfer-ownership"}
	for _, test := range []struct {
		name   string
		viewer string
		want   []any // nil: viewer.communityModeration absent
	}{
		{name: "owner", viewer: ownerDID, want: ownerActions},
		{name: "instance admin", viewer: adminDID, want: ownerActions},
		{name: "moderator", viewer: moderatorDID, want: []any{"ban-account", "unban-account"}},
		{name: "signed-in viewer with no role", viewer: noRoleDID},
		{name: "signed out", viewer: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewGetHandler(&getTestService{
				mockCommunityService: &mockCommunityService{},
				get: func(context.Context, string) (*communities.Community, error) {
					return &communities.Community{DID: communityDID, Handle: "c-moderation.coves.social", Name: "moderation"}, nil
				},
			}, getTestRepo(), WithCommunityViewerModeration(viewer))
			request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.get?community="+communityDID, nil)
			if test.viewer != "" {
				request = request.WithContext(middleware.SetTestUserDID(request.Context(), test.viewer))
			}
			recorder := httptest.NewRecorder()

			handler.HandleGet(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code, "body: %s", recorder.Body.String())
			if test.want == nil {
				assert.NotContains(t, recorder.Body.String(), `"communityModeration"`)
				return
			}
			var view map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &view))
			viewerState, ok := view["viewer"].(map[string]any)
			require.True(t, ok, "viewer state missing: %s", recorder.Body.String())
			assert.Equal(t, test.want, viewerState["communityModeration"])
		})
	}
}
