package community

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// viewerRoles is a communities.RoleAuthorizer answering ViewerRole from a
// fixed map of community DID -> viewer DID -> role.
type viewerRoles map[string]map[string]string

func (viewerRoles) Authorize(context.Context, string, string, string) (communities.RoleAuthorization, error) {
	return communities.RoleAuthorization{}, communities.ErrUnauthorized
}

func (v viewerRoles) ViewerRole(_ context.Context, communityDID, viewerDID string) (string, error) {
	return v[communityDID][viewerDID], nil
}

func (viewerRoles) HasUsableRole(context.Context, string) (bool, error) { return false, nil }

func (viewerRoles) RequireHostedCommunity(context.Context, string) error { return nil }

func TestGetHandler_ServesTheViewersRole(t *testing.T) {
	const (
		communityDID = "did:plc:viewerrolecommunity"
		moderatorDID = "did:plc:viewerrolemoderator"
	)
	roles := viewerRoles{communityDID: {moderatorDID: "moderator"}}
	for _, test := range []struct {
		name       string
		viewer     string
		wantViewer bool
		wantRole   any
	}{
		{name: "moderator viewer", viewer: moderatorDID, wantViewer: true, wantRole: "moderator"},
		{name: "signed out", viewer: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewGetHandler(&getTestService{
				mockCommunityService: &mockCommunityService{},
				get: func(context.Context, string) (*communities.Community, error) {
					return &communities.Community{DID: communityDID, Handle: "c-viewerrole.coves.social", Name: "viewerrole"}, nil
				},
			}, getTestRepo(), WithViewerRole(roles))
			request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.get?community="+communityDID, nil)
			if test.viewer != "" {
				request = request.WithContext(middleware.SetTestUserDID(request.Context(), test.viewer))
			}
			w := httptest.NewRecorder()

			handler.HandleGet(w, request)

			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			var view map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &view))
			if !test.wantViewer {
				assert.NotContains(t, w.Body.String(), `"role"`, "a signed-out request carries no role")
				return
			}
			viewer, ok := view["viewer"].(map[string]any)
			require.True(t, ok, "viewer state missing: %s", w.Body.String())
			assert.Equal(t, test.wantRole, viewer["role"])
		})
	}
}
