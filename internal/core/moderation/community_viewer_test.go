package moderation_test

import (
	"context"
	"errors"
	"testing"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/moderation/moderationtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	communityViewerCommunityDID = "did:plc:viewerauthoritycommunity"
	communityViewerOwner        = "did:plc:viewerauthorityowner"
	communityViewerModerator    = "did:plc:viewerauthoritymod"
	communityViewerAdmin        = "did:plc:viewerauthorityadmin"
	communityViewerNoRole       = "did:plc:viewerauthoritynorole"
	communityViewerRemovedComm  = "did:plc:viewerauthorityremoved"
	communityViewerUnhostedComm = "did:plc:viewerauthorityunhosted"
	communityViewerBrokenComm   = "did:plc:viewerauthoritybroken"
)

// failingRoleAuthorizer fails every read of brokenCommunity and of any
// viewer's roles, and otherwise defers to RoleAuthorizer.
type failingRoleAuthorizer struct {
	*moderationtest.RoleAuthorizer
	brokenCommunity string
	err             error
}

func (a failingRoleAuthorizer) ViewerRole(ctx context.Context, communityDID, viewerDID string) (string, error) {
	if communityDID == a.brokenCommunity {
		return "", a.err
	}
	return a.RoleAuthorizer.ViewerRole(ctx, communityDID, viewerDID)
}

func (a failingRoleAuthorizer) RequireHostedCommunity(ctx context.Context, communityDID string) error {
	if communityDID == a.brokenCommunity {
		return a.err
	}
	return a.RoleAuthorizer.RequireHostedCommunity(ctx, communityDID)
}

func (a failingRoleAuthorizer) HasUsableRole(context.Context, string) (bool, error) {
	return false, a.err
}

// failingRoleIndex fails every read.
type failingRoleIndex struct{ err error }

func (i failingRoleIndex) ListCommunityRoles(context.Context, string) ([]communities.CommunityRole, error) {
	return nil, i.err
}

func (i failingRoleIndex) ListRolesBySubject(context.Context, string) ([]communities.CommunityRole, error) {
	return nil, i.err
}

func communityViewerAuthorizer() *moderationtest.RoleAuthorizer {
	return &moderationtest.RoleAuthorizer{
		Hosted: map[string]bool{
			communityViewerCommunityDID: true, communityViewerRemovedComm: true, communityViewerBrokenComm: true,
		},
		Removed: map[string]bool{communityViewerRemovedComm: true},
		Roles: map[string]map[string]string{
			communityViewerCommunityDID: {communityViewerOwner: communities.RoleOwner, communityViewerModerator: communities.RoleModerator},
			communityViewerRemovedComm:  {communityViewerModerator: communities.RoleModerator},
			communityViewerUnhostedComm: {communityViewerModerator: communities.RoleModerator},
		},
		Admins: map[string]bool{communityViewerAdmin: true},
	}
}

// TestCommunityViewerViewerAuthority pins the viewer's community authority:
// owner for the owner and for an instance admin in a hosted, not-removed
// community, moderator for a moderator, none otherwise, and a read failure
// returned, each decided in one authorizer read.
func TestCommunityViewerViewerAuthority(t *testing.T) {
	readFailure := errors.New("role index unavailable")
	for _, tc := range []struct {
		name      string
		community string
		viewer    string
		want      moderation.CommunityViewerAuthority
		wantErr   error
	}{
		{name: "owner", community: communityViewerCommunityDID, viewer: communityViewerOwner, want: moderation.CommunityAuthorityOwner},
		{name: "instance admin with no role", community: communityViewerCommunityDID, viewer: communityViewerAdmin, want: moderation.CommunityAuthorityOwner},
		{name: "moderator", community: communityViewerCommunityDID, viewer: communityViewerModerator, want: moderation.CommunityAuthorityModerator},
		{name: "viewer with no role", community: communityViewerCommunityDID, viewer: communityViewerNoRole, want: moderation.CommunityAuthorityNone},
		{name: "moderator of a removed community", community: communityViewerRemovedComm, viewer: communityViewerModerator, want: moderation.CommunityAuthorityNone},
		{name: "instance admin in a removed community", community: communityViewerRemovedComm, viewer: communityViewerAdmin, want: moderation.CommunityAuthorityNone},
		{name: "moderator of a community not hosted here", community: communityViewerUnhostedComm, viewer: communityViewerModerator, want: moderation.CommunityAuthorityNone},
		{name: "instance admin in a community not hosted here", community: communityViewerUnhostedComm, viewer: communityViewerAdmin, want: moderation.CommunityAuthorityNone},
		{name: "signed-out viewer", community: communityViewerCommunityDID, viewer: "", want: moderation.CommunityAuthorityNone},
		{name: "authorizer read failure", community: communityViewerBrokenComm, viewer: communityViewerModerator, wantErr: readFailure},
		{name: "authorizer read failure for an instance admin", community: communityViewerBrokenComm, viewer: communityViewerAdmin, wantErr: readFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := communityViewerAuthorizer()
			viewer := moderation.NewCommunityViewer(
				failingRoleAuthorizer{RoleAuthorizer: authorizer, brokenCommunity: communityViewerBrokenComm, err: readFailure},
				&moderationtest.RoleIndex{}, authorizer)

			got, err := viewer.ViewerAuthority(context.Background(), tc.community, tc.viewer)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr, "a read failure must reach the caller, not decay to no authority")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Zero(t, authorizer.Calls("Authorize"), "authority is read in one pass, not by procedure authorization")
			assert.LessOrEqual(t, authorizer.Calls("ViewerRole")+authorizer.Calls("RequireHostedCommunity"), 1)
		})
	}
}

// TestCommunityViewerViewerMayModerate pins which viewers may hold community
// authority anywhere: instance admins and holders of a usable role.
func TestCommunityViewerViewerMayModerate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		viewer string
		want   bool
	}{
		{name: "instance admin", viewer: communityViewerAdmin, want: true},
		{name: "moderator", viewer: communityViewerModerator, want: true},
		{name: "owner", viewer: communityViewerOwner, want: true},
		{name: "viewer with no role", viewer: communityViewerNoRole, want: false},
		{name: "signed-out viewer", viewer: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := communityViewerAuthorizer()
			viewer := moderation.NewCommunityViewer(authorizer, &moderationtest.RoleIndex{}, authorizer)

			got, err := viewer.ViewerMayModerate(context.Background(), tc.viewer)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("role read failure", func(t *testing.T) {
		readFailure := errors.New("role index unavailable")
		authorizer := communityViewerAuthorizer()
		viewer := moderation.NewCommunityViewer(failingRoleAuthorizer{RoleAuthorizer: authorizer, err: readFailure},
			&moderationtest.RoleIndex{}, authorizer)
		_, err := viewer.ViewerMayModerate(context.Background(), communityViewerNoRole)
		require.ErrorIs(t, err, readFailure)
	})
}

// TestCommunityViewerRosterHoldsRole pins the role-holder rule a community ban
// refuses on: an owner or moderator row in the effective roster.
func TestCommunityViewerRosterHoldsRole(t *testing.T) {
	index := &moderationtest.RoleIndex{Rows: map[string][]communities.CommunityRole{
		communityViewerCommunityDID: {
			{CommunityDID: communityViewerCommunityDID, SubjectDID: communityViewerOwner, Role: communities.RoleOwner, RecordKey: "owner"},
			{CommunityDID: communityViewerCommunityDID, SubjectDID: communityViewerModerator, Role: communities.RoleModerator, RecordKey: "3kabcmoderator"},
		},
	}}
	authorizer := communityViewerAuthorizer()
	roster, err := moderation.NewCommunityViewer(authorizer, index, authorizer).Roster(context.Background(), communityViewerCommunityDID)
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		subject string
		want    bool
	}{
		{name: "owner row", subject: communityViewerOwner, want: true},
		{name: "moderator row", subject: communityViewerModerator, want: true},
		{name: "no row", subject: communityViewerNoRole, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, moderation.RosterHoldsRole(roster, tc.subject))
		})
	}

	t.Run("role index read failure", func(t *testing.T) {
		readFailure := errors.New("role index unavailable")
		_, err := moderation.NewCommunityViewer(authorizer, failingRoleIndex{err: readFailure}, authorizer).
			Roster(context.Background(), communityViewerCommunityDID)
		require.ErrorIs(t, err, readFailure)
	})
}
