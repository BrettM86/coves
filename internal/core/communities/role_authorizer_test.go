package communities_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authzHostedCommunity    = "did:plc:authzhostedcommunity"
	authzBridgedCommunity   = "did:plc:authzbridgedcommunity"
	authzRemovedCommunity   = "did:plc:authzremovedcommunity"
	authzCompetingCommunity = "did:plc:authzcompetingcommunity"

	authzOwner          = "did:plc:authzowner"
	authzCreator        = "did:plc:authzcreator" // the community's createdBy; holds no role row
	authzModerator      = "did:plc:authzmoderator"
	authzAdmin          = "did:plc:authzadmin"
	authzAdminModerator = "did:plc:authzadminmoderator"
	authzOlderOwner     = "did:plc:authzolderowner"
	authzNewerOwner     = "did:plc:authznewerowner"
	authzRemovedOnly    = "did:plc:authzremovedonly"
	authzBridgedOnly    = "did:plc:authzbridgedonly"
)

var errRoleIndexUnavailable = errors.New("role index unavailable")

// failingRoleIndex is a role index whose reads fail.
type failingRoleIndex struct{ *fakeRoleIndex }

func (failingRoleIndex) ListCommunityRoles(context.Context, string) ([]communities.CommunityRole, error) {
	return nil, errRoleIndexUnavailable
}

func (failingRoleIndex) ListRolesBySubject(context.Context, string) ([]communities.CommunityRole, error) {
	return nil, errRoleIndexUnavailable
}

// authorizerRulesFixture: a hosted community with owner, moderator and an
// admin who is also a moderator; a bridged community and a removed hosted
// community each with an owner row for authzOwner; a hosted community with two
// competing owner records; and subjects whose only roles are in the bridged or
// the removed community.
func authorizerRulesFixture() *roleAuthorizationFixture {
	older := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	f := newRoleAuthorizationFixture(
		ownerRow(authzHostedCommunity, authzOwner, older),
		moderatorRow(authzHostedCommunity, authzModerator, older),
		moderatorRow(authzHostedCommunity, authzAdminModerator, older),
		ownerRow(authzBridgedCommunity, authzOwner, older),
		ownerRow(authzBridgedCommunity, authzBridgedOnly, older),
		ownerRow(authzRemovedCommunity, authzOwner, older),
		ownerRow(authzRemovedCommunity, authzRemovedOnly, older),
		ownerRow(authzCompetingCommunity, authzOlderOwner, older),
		ownerRow(authzCompetingCommunity, authzNewerOwner, newer),
	)
	f.hosted[authzHostedCommunity] = true
	f.hosted[authzRemovedCommunity] = true
	f.hosted[authzCompetingCommunity] = true
	f.removals[authzRemovedCommunity] = true
	f.admins[authzAdmin] = true
	f.admins[authzAdminModerator] = true
	return f
}

func permanentBan() *accountbans.Ban { return &accountbans.Ban{ActionID: "ban-permanent"} }

func lapsedBan() *accountbans.Ban {
	expired := time.Date(2026, 11, 12, 11, 59, 59, 0, time.UTC) // one second before the fixture's now
	return &accountbans.Ban{ActionID: "ban-lapsed", ExpiresAt: &expired}
}

func TestRoleAuthorizer_Authorize(t *testing.T) {
	for _, test := range []struct {
		name      string
		setup     func(*roleAuthorizationFixture)
		community string
		caller    string
		required  string
		want      communities.RoleAuthorization
		wantErr   error
	}{
		{
			name: "indexed owner is authorized as owner", community: authzHostedCommunity, caller: authzOwner,
			required: communities.RoleOwner, want: communities.RoleAuthorization{Role: "owner"},
		},
		{
			name: "owner satisfies moderator", community: authzHostedCommunity, caller: authzOwner,
			required: communities.RoleModerator, want: communities.RoleAuthorization{Role: "owner"},
		},
		{
			name: "createdBy with no role row is refused", community: authzHostedCommunity, caller: authzCreator,
			required: communities.RoleOwner, wantErr: communities.ErrUnauthorized,
		},
		{
			name: "moderator is authorized as moderator", community: authzHostedCommunity, caller: authzModerator,
			required: communities.RoleModerator, want: communities.RoleAuthorization{Role: "moderator"},
		},
		{
			name: "moderator is refused owner", community: authzHostedCommunity, caller: authzModerator,
			required: communities.RoleOwner, wantErr: communities.ErrUnauthorized,
		},
		{
			name: "instance admin with no role acts for a hosted community", community: authzHostedCommunity, caller: authzAdmin,
			required: communities.RoleOwner, want: communities.RoleAuthorization{InstanceAdmin: true},
		},
		{
			name: "instance admin holding a satisfying role is authorized by the role", community: authzHostedCommunity,
			caller: authzAdminModerator, required: communities.RoleModerator,
			want: communities.RoleAuthorization{Role: "moderator"},
		},
		{
			name: "instance admin is refused in a community not hosted here", community: authzBridgedCommunity, caller: authzAdmin,
			required: communities.RoleOwner, wantErr: communities.ErrUnauthorized,
		},
		{
			name: "owner row in a community not hosted here is refused", community: authzBridgedCommunity, caller: authzOwner,
			required: communities.RoleOwner, wantErr: communities.ErrUnauthorized,
		},
		{
			name: "owner with an in-force instance ban is suspended", community: authzHostedCommunity, caller: authzOwner,
			setup:    func(f *roleAuthorizationFixture) { f.bans[authzOwner] = permanentBan() },
			required: communities.RoleOwner, wantErr: communities.ErrUnauthorized,
		},
		{
			name: "owner whose instance ban has lapsed is authorized", community: authzHostedCommunity, caller: authzOwner,
			setup:    func(f *roleAuthorizationFixture) { f.bans[authzOwner] = lapsedBan() },
			required: communities.RoleOwner, want: communities.RoleAuthorization{Role: "owner"},
		},
		{
			name: "owner whose PDS reports takendown is suspended", community: authzHostedCommunity, caller: authzOwner,
			setup:    func(f *roleAuthorizationFixture) { f.statuses[authzOwner] = accountstatus.Takendown },
			required: communities.RoleOwner, wantErr: communities.ErrUnauthorized,
		},
		{
			name: "moderator whose PDS reports suspended is suspended", community: authzHostedCommunity, caller: authzModerator,
			setup:    func(f *roleAuthorizationFixture) { f.statuses[authzModerator] = accountstatus.Suspended },
			required: communities.RoleModerator, wantErr: communities.ErrUnauthorized,
		},
		{
			name: "owner of a removed community is refused as removed", community: authzRemovedCommunity, caller: authzOwner,
			required: communities.RoleOwner, wantErr: communities.ErrCommunityRemoved,
		},
		{
			name: "instance admin in a removed community is refused as removed", community: authzRemovedCommunity, caller: authzAdmin,
			required: communities.RoleOwner, wantErr: communities.ErrCommunityRemoved,
		},
		{
			name: "older competing owner is refused owner", community: authzCompetingCommunity, caller: authzOlderOwner,
			required: communities.RoleOwner, wantErr: communities.ErrUnauthorized,
		},
		{
			name: "older competing owner is authorized as moderator", community: authzCompetingCommunity, caller: authzOlderOwner,
			required: communities.RoleModerator, want: communities.RoleAuthorization{Role: "moderator"},
		},
		{
			name: "newer competing owner is the owner", community: authzCompetingCommunity, caller: authzNewerOwner,
			required: communities.RoleOwner, want: communities.RoleAuthorization{Role: "owner"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := authorizerRulesFixture()
			if test.setup != nil {
				test.setup(f)
			}
			got, err := f.authorizer().Authorize(context.Background(), test.community, test.caller, test.required)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestRoleAuthorizer_DependencyReadErrorIsNotARefusal(t *testing.T) {
	f := authorizerRulesFixture()
	authorizer := communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted: f.hosted, Removals: f.removals, Roles: failingRoleIndex{f.roles},
		AccountBans: f.bans, AccountStatus: f.statuses, Admins: f.admins,
		Now: func() time.Time { return f.now },
	})

	_, err := authorizer.Authorize(context.Background(), authzHostedCommunity, authzOwner, communities.RoleOwner)
	require.ErrorIs(t, err, errRoleIndexUnavailable)
	assert.NotErrorIs(t, err, communities.ErrUnauthorized)

	_, err = authorizer.ViewerRole(context.Background(), authzHostedCommunity, authzOwner)
	require.ErrorIs(t, err, errRoleIndexUnavailable)

	_, err = authorizer.HasUsableRole(context.Background(), authzOwner)
	require.ErrorIs(t, err, errRoleIndexUnavailable)
}

func TestRoleAuthorizer_ViewerRole(t *testing.T) {
	for _, test := range []struct {
		name      string
		setup     func(*roleAuthorizationFixture)
		community string
		viewer    string
		want      string
	}{
		{name: "owner", community: authzHostedCommunity, viewer: authzOwner, want: "owner"},
		{name: "moderator", community: authzHostedCommunity, viewer: authzModerator, want: "moderator"},
		{name: "createdBy with no role row", community: authzHostedCommunity, viewer: authzCreator, want: ""},
		{name: "admin status is not a role", community: authzHostedCommunity, viewer: authzAdmin, want: ""},
		{name: "role in a community not hosted here", community: authzBridgedCommunity, viewer: authzOwner, want: ""},
		{name: "role in a removed community", community: authzRemovedCommunity, viewer: authzOwner, want: ""},
		{
			name: "owner with an in-force ban", community: authzHostedCommunity, viewer: authzOwner, want: "",
			setup: func(f *roleAuthorizationFixture) { f.bans[authzOwner] = permanentBan() },
		},
		{
			name: "owner whose ban has lapsed", community: authzHostedCommunity, viewer: authzOwner, want: "owner",
			setup: func(f *roleAuthorizationFixture) { f.bans[authzOwner] = lapsedBan() },
		},
		{
			name: "takendown moderator", community: authzHostedCommunity, viewer: authzModerator, want: "",
			setup: func(f *roleAuthorizationFixture) { f.statuses[authzModerator] = accountstatus.Takendown },
		},
		{name: "older competing owner reads as moderator", community: authzCompetingCommunity, viewer: authzOlderOwner, want: "moderator"},
		{name: "newer competing owner reads as owner", community: authzCompetingCommunity, viewer: authzNewerOwner, want: "owner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := authorizerRulesFixture()
			if test.setup != nil {
				test.setup(f)
			}
			got, err := f.authorizer().ViewerRole(context.Background(), test.community, test.viewer)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestRoleAuthorizer_HasUsableRole(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(*roleAuthorizationFixture)
		viewer string
		want   bool
	}{
		{name: "moderator of a hosted community", viewer: authzModerator, want: true},
		{name: "roles in removed and bridged communities plus one usable", viewer: authzOwner, want: true},
		{name: "roles only in a removed community", viewer: authzRemovedOnly, want: false},
		{name: "roles only in a community not hosted here", viewer: authzBridgedOnly, want: false},
		{name: "admin with no role", viewer: authzAdmin, want: false},
		{
			name: "owner with an in-force ban", viewer: authzOwner, want: false,
			setup: func(f *roleAuthorizationFixture) { f.bans[authzOwner] = permanentBan() },
		},
		{
			name: "suspended moderator", viewer: authzModerator, want: false,
			setup: func(f *roleAuthorizationFixture) { f.statuses[authzModerator] = accountstatus.Suspended },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := authorizerRulesFixture()
			if test.setup != nil {
				test.setup(f)
			}
			got, err := f.authorizer().HasUsableRole(context.Background(), test.viewer)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}
