package jetstream

import (
	"context"
	"testing"

	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subscriptionIndexRepo records the subscription the consumer hands to the
// index. The embedded interface is nil, so any repository call not modelled
// here panics rather than silently passing.
type subscriptionIndexRepo struct {
	communities.Repository
	byURI   map[string]*communities.Subscription
	indexed []communities.Subscription
}

func (r *subscriptionIndexRepo) GetSubscriptionByURI(_ context.Context, uri string) (*communities.Subscription, error) {
	if sub, ok := r.byURI[uri]; ok {
		cp := *sub
		return &cp, nil
	}
	return nil, communities.ErrSubscriptionNotFound
}

func (r *subscriptionIndexRepo) SubscribeWithCount(_ context.Context, sub *communities.Subscription) (*communities.Subscription, error) {
	r.indexed = append(r.indexed, *sub)
	cp := *sub
	r.byURI[sub.RecordURI] = &cp
	return sub, nil
}

func TestCommunityConsumer_SubscriptionIndexesFavorite(t *testing.T) {
	t.Parallel()

	const userDID = "did:plc:favoritesubscriber"
	const communityDID = "did:plc:favoritecommunity"

	record := func(favorite any) map[string]interface{} {
		value := map[string]interface{}{
			"$type":             "social.coves.community.subscription",
			"subject":           communityDID,
			"createdAt":         "2026-01-02T03:04:05Z",
			"contentVisibility": float64(4),
		}
		if favorite != nil {
			value["favorite"] = favorite
		}
		return value
	}

	tests := []struct {
		name         string
		operation    string
		favorite     any // nil = absent from the record
		wantFavorite bool
	}{
		{name: "create with favorite true", operation: "create", favorite: true, wantFavorite: true},
		{name: "create without favorite", operation: "create", favorite: nil, wantFavorite: false},
		{name: "create with non-boolean favorite", operation: "create", favorite: "yes", wantFavorite: false},
		{name: "update with favorite true", operation: "update", favorite: true, wantFavorite: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &subscriptionIndexRepo{byURI: map[string]*communities.Subscription{}}
			consumer := NewCommunityEventConsumer(repo, "did:web:test.local", true, nil)

			require.NoError(t, consumer.HandleEvent(context.Background(), taxonomyEvent(
				userDID, "social.coves.community.subscription", tt.operation, "3kfavorite22", record(tt.favorite))))

			require.Len(t, repo.indexed, 1)
			assert.Equal(t, communityDID, repo.indexed[0].CommunityDID)
			assert.Equal(t, 4, repo.indexed[0].ContentVisibility)
			assert.Equal(t, tt.wantFavorite, repo.indexed[0].Favorite)
		})
	}

	t.Run("update flipping favorite to false indexes false", func(t *testing.T) {
		t.Parallel()
		repo := &subscriptionIndexRepo{byURI: map[string]*communities.Subscription{}}
		consumer := NewCommunityEventConsumer(repo, "did:web:test.local", true, nil)
		ctx := context.Background()

		require.NoError(t, consumer.HandleEvent(ctx, taxonomyEvent(
			userDID, "social.coves.community.subscription", "create", "3kfavorite22", record(true))))
		require.NoError(t, consumer.HandleEvent(ctx, taxonomyEvent(
			userDID, "social.coves.community.subscription", "update", "3kfavorite22", record(false))))

		require.Len(t, repo.indexed, 2)
		assert.True(t, repo.indexed[0].Favorite, "the create indexed favorite true")
		assert.False(t, repo.indexed[1].Favorite, "the update must index favorite false")
	})
}
