//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const roleAuthorizerInstanceDID = "did:web:test.local"

// insertRoleAuthorizerCommunity inserts a community created by createdBy and
// claiming hostedBy, storing a PDS refresh token only when credentials is set.
func insertRoleAuthorizerCommunity(t *testing.T, db *sql.DB, did, createdBy, hostedBy string, credentials bool) {
	t.Helper()
	var refreshToken []byte
	if credentials {
		refreshToken = []byte{1, 'r', 'e', 'f', 'r', 'e', 's', 'h'}
	}
	_, err := db.Exec(`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at, pds_refresh_token_encrypted)
		VALUES ($1, $2, 'Role Authorizer Community', $1, $3, $4, NOW(), $5)`,
		did, did[len("did:plc:"):]+".test", createdBy, hostedBy, refreshToken)
	require.NoError(t, err)
}

// The role authorizer over the real index: owner authority comes from the
// indexed owner row, never createdBy, and only in a community this instance
// stores credentials for, whatever hosted_by_did claims.
func TestRoleAuthorizer_OwnerAuthorityOverTheIndex(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	const (
		owner   = "did:plc:roleauthzowner"
		creator = "did:plc:roleauthzcreator"
	)
	for _, community := range []struct {
		did, hostedBy string
		credentials   bool
	}{
		{did: "did:plc:roleauthzhosted", hostedBy: roleAuthorizerInstanceDID, credentials: true},
		{did: "did:plc:roleauthznocreds", hostedBy: roleAuthorizerInstanceDID},
		{did: "did:plc:roleauthzclaimselse", hostedBy: "did:web:elsewhere.example", credentials: true},
		{did: "did:plc:roleauthzbridged", hostedBy: "did:web:elsewhere.example"},
	} {
		insertRoleAuthorizerCommunity(t, db, community.did, creator, community.hostedBy, community.credentials)
	}
	roles := NewCommunityRoleRepository(db)
	for _, community := range []string{"did:plc:roleauthzhosted", "did:plc:roleauthznocreds", "did:plc:roleauthzclaimselse", "did:plc:roleauthzbridged"} {
		require.NoError(t, roles.UpsertCommunityRole(ctx, communities.CommunityRole{
			CommunityDID: community, SubjectDID: owner, Role: communities.RoleOwner,
			RecordKey: "roleauthzownerkey", RecordCID: "bafyreiroleauthzowner",
			RecordCreatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		}))
	}
	hosted := NewHostedCommunityQuery(db)
	authorizer := communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted:        hosted,
		Removals:      NewCommunityRemovalReader(db, roleAuthorizerInstanceDID),
		Roles:         roles,
		AccountBans:   NewAccountBanReader(db, roleAuthorizerInstanceDID),
		AccountStatus: NewAccountStatusRepository(db),
		Admins:        moderation.NewAllowlistAuthority(nil),
		Now:           time.Now,
	})

	for _, test := range []struct {
		name       string
		community  string
		caller     string
		wantHosted bool
		authorized bool
	}{
		{name: "indexed owner of a hosted community", community: "did:plc:roleauthzhosted", caller: owner, wantHosted: true, authorized: true},
		{name: "creator with no role row", community: "did:plc:roleauthzhosted", caller: creator, wantHosted: true},
		{name: "hosted_by_did names this instance but no credentials are stored", community: "did:plc:roleauthznocreds", caller: owner},
		{name: "credentials stored while hosted_by_did names another instance", community: "did:plc:roleauthzclaimselse", caller: owner, wantHosted: true, authorized: true},
		{name: "bridged community with an indexed owner row", community: "did:plc:roleauthzbridged", caller: owner},
	} {
		t.Run(test.name, func(t *testing.T) {
			isHosted, err := hosted.IsHostedCommunity(ctx, test.community)
			require.NoError(t, err)
			assert.Equal(t, test.wantHosted, isHosted, "hosted means stored credentials")

			authorization, err := authorizer.Authorize(ctx, test.community, test.caller, communities.RoleOwner)
			if !test.authorized {
				require.ErrorIs(t, err, communities.ErrUnauthorized)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, communities.RoleAuthorization{Role: "owner"}, authorization)
		})
	}
}
