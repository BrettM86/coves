//go:build integration

package jetstream

import (
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A DID first indexed only as a post author has an author_hydration users
// row. Its own social.coves.actor.profile record arriving through the user
// consumer is the account's own record, so the row becomes an account row and
// the DID can no longer be indexed as a community. The promotion holds even
// when the profile event is older than the hydrated row and the recency guard
// skips the profile write; freshness still governs the profile fields.
func TestUserProfile_OwnProfileUpgradesHydratedAuthorToAccount(t *testing.T) {
	for _, tc := range []struct {
		name        string
		operation   string
		eventOffset time.Duration
		fresh       bool
	}{
		{name: "create", operation: "create", eventOffset: time.Minute, fresh: true},
		{name: "update", operation: "update", eventOffset: time.Minute, fresh: true},
		{name: "stale create", operation: "create", eventOffset: -time.Minute, fresh: false},
		{name: "stale update", operation: "update", eventOffset: -time.Minute, fresh: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx := t.Context()
			label := testkit.UniqueIDWithPrefix(t, "hydprof")
			did := fixtures.DID(label)

			userRepository := postgres.NewUserRepository(db)
			resolver := &mockIdentityResolverForUser{}
			service := users.NewUserService(userRepository, resolver, untrustedPDS, nil, "")
			require.NoError(t, service.IndexHydratedAuthor(ctx, did, label+".test", untrustedPDS),
				"fixture: post author hydration indexes the DID")
			hydrated, err := userRepository.GetByDID(ctx, did)
			require.NoError(t, err)
			require.Equal(t, users.IndexSourceAuthorHydration, hydrated.IndexSource, "fixture: the row is a hydrated author")

			consumer := NewUserEventConsumer(service, resolver)
			require.NoError(t, consumer.HandleEvent(ctx, revCommitEvent(
				did, CovesProfileCollection, tc.operation, "self", "", "bafyreihydratedprofile",
				hydrated.UpdatedAt.Add(tc.eventOffset).UnixMicro(),
				map[string]interface{}{"$type": CovesProfileCollection, "displayName": "Hydrated Author"},
			)))

			upgraded, err := userRepository.GetByDID(ctx, did)
			require.NoError(t, err)
			if tc.fresh {
				require.Equal(t, "Hydrated Author", upgraded.DisplayName, "fixture: the profile record was applied")
			} else {
				assert.NotEqual(t, "Hydrated Author", upgraded.DisplayName,
					"an event older than the row does not overwrite the profile fields")
			}
			assert.Equal(t, users.IndexSourceAccount, upgraded.IndexSource,
				"the account's own profile record makes the row an account row")

			communityRepository := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
			_, err = communityRepository.Create(ctx, &communities.Community{
				DID:          did,
				Handle:       "!" + label + "@coves.local",
				Name:         label,
				OwnerDID:     did,
				CreatedByDID: did,
				HostedByDID:  fixtures.InstanceDID(),
				Visibility:   "public",
				CreatedAt:    time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC),
				UpdatedAt:    time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC),
			})
			assert.ErrorIs(t, err, communities.ErrDIDHasAccountActivity,
				"an account DID cannot be indexed as a community")
		})
	}
}
