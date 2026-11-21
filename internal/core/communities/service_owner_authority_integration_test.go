//go:build integration

package communities_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ownerAuthorityIntegrationCreator = "did:plc:troublemaker456"
	// RoleRecordKey("did:plc:troublemaker456")
	ownerAuthorityIntegrationCreatorKey = "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa"
)

// createOwnedCommunity creates a community as ownerAuthorityIntegrationCreator
// through the service over the real test PDS and a fresh database.
func createOwnedCommunity(t *testing.T, prefix string) (communities.Service, *sql.DB, *communities.Community) {
	t.Helper()
	service, _, _, db := newCommunityServiceWithDatabase(t)
	fixtures.User(t, db, testkit.UniqueIDWithPrefix(t, prefix)+".test", ownerAuthorityIntegrationCreator)

	name := testkit.UniqueIDWithPrefix(t, prefix)
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)
	community, err := service.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "Owned Community",
		Description:            "a community whose creator is its indexed owner",
		Visibility:             "public",
		CreatedByDID:           ownerAuthorityIntegrationCreator,
		AllowExternalDiscovery: true,
	})
	require.NoError(t, err)
	return service, db, community
}

// CreateCommunity indexes the creator as owner itself: the role index names
// the creator the moment the call returns, before any firehose event.
func TestService_CreateIndexesTheCreatorAsOwner(t *testing.T) {
	t.Parallel()
	_, db, community := createOwnedCommunity(t, "own")

	roster, err := communities.ReadRoster(context.Background(), postgres.NewCommunityRoleRepository(db), community.DID)
	require.NoError(t, err)
	require.NotNil(t, roster.Owner, "the created community must have an indexed owner on return")
	assert.Equal(t, "did:plc:troublemaker456", roster.Owner.SubjectDID)
	assert.Equal(t, "owner", roster.Owner.Role)
	assert.Equal(t, ownerAuthorityIntegrationCreatorKey, roster.Owner.RecordKey)
	assert.NotEmpty(t, roster.Owner.RecordCID, "the owner row carries the written record's CID")
	assert.Empty(t, roster.Moderators)
}

// A suspended owner keeps the owner row but cannot use it: an in-force
// instance ban or a stored PDS takedown refuses the owner's update, and the
// update succeeds again once the ban lapses or the restriction clears.
func TestService_UpdateRefusesASuspendedOwner(t *testing.T) {
	for _, test := range []struct {
		name    string
		suspend func(t *testing.T, db *sql.DB) (lift func())
	}{
		{
			name: "instance ban in force, then lapsed",
			suspend: func(t *testing.T, db *sql.DB) func() {
				moderator := accountbanfixture.New(db)
				expiresAt := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
				moderator.Ban(t, ownerAuthorityIntegrationCreator, &expiresAt)
				return func() { moderator.Lapse(t, ownerAuthorityIntegrationCreator) }
			},
		},
		{
			name: "stored takendown restriction, then cleared",
			suspend: func(t *testing.T, db *sql.DB) func() {
				accountstatusfixture.Set(t, db, ownerAuthorityIntegrationCreator, accountstatus.Takendown)
				return func() { accountstatusfixture.Clear(t, db, ownerAuthorityIntegrationCreator) }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, db, community := createOwnedCommunity(t, "sus")
			ctx := context.Background()
			roles := postgres.NewCommunityRoleRepository(db)
			before, err := roles.ListCommunityRoles(ctx, community.DID)
			require.NoError(t, err)

			lift := test.suspend(t, db)
			refused := "updated while suspended"
			_, err = service.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
				CommunityDID: community.DID, UpdatedByDID: ownerAuthorityIntegrationCreator, Description: &refused,
			})
			require.ErrorIs(t, err, communities.ErrUnauthorized, "a suspended owner's update must be refused")
			after, err := roles.ListCommunityRoles(ctx, community.DID)
			require.NoError(t, err)
			require.NotEmpty(t, before, "the creator is the indexed owner before suspension")
			assert.Equal(t, before, after, "suspension leaves the owner row as it is")

			lift()
			accepted := "updated after the suspension ended"
			_, err = service.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
				CommunityDID: community.DID, UpdatedByDID: ownerAuthorityIntegrationCreator, Description: &accepted,
			})
			require.NoError(t, err, "the owner's update succeeds once the suspension ends")
		})
	}
}
