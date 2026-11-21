package moderation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	coremoderation "Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// usableRoles is a communities.RoleAuthorizer answering HasUsableRole from a
// fixed set of viewer DIDs.
type usableRoles map[string]bool

func (usableRoles) Authorize(context.Context, string, string, string) (communities.RoleAuthorization, error) {
	return communities.RoleAuthorization{}, communities.ErrUnauthorized
}

func (usableRoles) ViewerRole(context.Context, string, string) (string, error) { return "", nil }

func (u usableRoles) HasUsableRole(_ context.Context, viewerDID string) (bool, error) {
	return u[viewerDID], nil
}

func (usableRoles) RequireHostedCommunity(context.Context, string) error { return nil }

func TestGetViewerAuthority_ReportsCommunityRole(t *testing.T) {
	const (
		moderatorDID = "did:plc:viewerauthoritymoderator"
		memberDID    = "did:plc:viewerauthoritymember"
	)
	handler := NewGetViewerAuthorityHandler(coremoderation.NewAllowlistAuthority(nil),
		WithCommunityRoles(usableRoles{moderatorDID: true}))
	for _, test := range []struct {
		name     string
		viewer   string
		wantBody string
	}{
		{name: "community moderator", viewer: moderatorDID, wantBody: `{"instanceAdmin":false,"hasCommunityRole":true}`},
		{name: "viewer with no community role", viewer: memberDID, wantBody: `{"instanceAdmin":false,"hasCommunityRole":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getViewerAuthority", nil)
			request = request.WithContext(middleware.SetTestUserDID(request.Context(), test.viewer))
			w := httptest.NewRecorder()

			handler.HandleGetViewerAuthority(w, request)

			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.JSONEq(t, test.wantBody, w.Body.String())
		})
	}
}
