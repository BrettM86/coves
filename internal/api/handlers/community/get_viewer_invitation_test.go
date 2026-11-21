package community

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// viewerInvitations is a communities.InvitationReader over a fixed map of
// community DID -> invitee DID -> invitation. Following the reader's
// contract, PendingInvitation returns an invitation only while it is open
// and not lapsed at the now it is given.
type viewerInvitations map[string]map[string]communities.Invitation

func (viewerInvitations) ListPendingInvitationsForCommunity(context.Context, string, time.Time) ([]communities.Invitation, error) {
	return nil, nil
}

func (viewerInvitations) ListPendingInvitationsForInvitee(context.Context, string, time.Time) ([]communities.Invitation, error) {
	return nil, nil
}

func (v viewerInvitations) PendingInvitation(_ context.Context, communityDID, inviteeDID string, now time.Time) (*communities.Invitation, error) {
	invitation, ok := v[communityDID][inviteeDID]
	if !ok || invitation.State != communities.InvitationOpen || !now.Before(invitation.ExpiresAt) {
		return nil, nil
	}
	return &invitation, nil
}

func TestGetHandler_ServesTheViewersPendingInvitation(t *testing.T) {
	const (
		communityDID     = "did:plc:viewerinvitecommunity"
		inviteeDID       = "did:plc:viewerinvitee"
		lapsedInviteeDID = "did:plc:viewerlapsedinvitee"
		otherViewerDID   = "did:plc:viewerbystander"
	)
	invitations := viewerInvitations{communityDID: {
		inviteeDID: {
			ID: "invitation-1", CommunityDID: communityDID, InviteeDID: inviteeDID,
			Role: communities.RoleModerator, InviterDID: "did:plc:viewerinviteowner",
			State:     communities.InvitationOpen,
			CreatedAt: time.Date(2099, 1, 1, 10, 0, 0, 0, time.UTC),
			ExpiresAt: time.Date(2099, 1, 15, 10, 0, 0, 0, time.UTC),
		},
		lapsedInviteeDID: {
			ID: "invitation-2", CommunityDID: communityDID, InviteeDID: lapsedInviteeDID,
			Role: communities.RoleOwner, InviterDID: "did:plc:viewerinviteowner",
			State:     communities.InvitationOpen,
			CreatedAt: time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC),
			ExpiresAt: time.Date(2020, 1, 15, 10, 0, 0, 0, time.UTC),
		},
	}}
	for _, test := range []struct {
		name   string
		viewer string
		// wantInvitation is the viewer.invitation JSON, or empty for none.
		wantInvitation string
	}{
		{
			name: "invitee", viewer: inviteeDID,
			wantInvitation: `{"id":"invitation-1","role":"moderator","expiresAt":"2099-01-15T10:00:00Z"}`,
		},
		{name: "another viewer", viewer: otherViewerDID},
		{name: "invitee whose invitation lapsed", viewer: lapsedInviteeDID},
		{name: "signed out", viewer: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewGetHandler(&getTestService{
				mockCommunityService: &mockCommunityService{},
				get: func(context.Context, string) (*communities.Community, error) {
					return &communities.Community{DID: communityDID, Handle: "c-viewerinvite.coves.social", Name: "viewerinvite"}, nil
				},
			}, getTestRepo(), WithViewerInvitation(invitations))
			request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.get?community="+communityDID, nil)
			if test.viewer != "" {
				request = request.WithContext(middleware.SetTestUserDID(request.Context(), test.viewer))
			}
			w := httptest.NewRecorder()

			handler.HandleGet(w, request)

			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			if test.wantInvitation == "" {
				assert.NotContains(t, w.Body.String(), `"invitation"`, "no pending invitation is served to this caller")
				return
			}
			var view struct {
				Viewer struct {
					Invitation json.RawMessage `json:"invitation"`
				} `json:"viewer"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &view))
			require.NotEmpty(t, view.Viewer.Invitation, "viewer.invitation missing: %s", w.Body.String())
			assert.JSONEq(t, test.wantInvitation, string(view.Viewer.Invitation))
		})
	}
}
