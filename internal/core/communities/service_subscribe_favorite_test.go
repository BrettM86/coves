package communities_test

import (
	"context"
	"testing"

	"Coves/internal/atproto/pds"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The favorite flag on a subscription record. A supplied favorite (true or
// false) is written to the record; an omitted one (nil) leaves whatever the
// record already holds. Favorite and contentVisibility are independent: each
// supplied value is written and each omitted value keeps the listed one.

func boolPtr(v bool) *bool { return &v }

func favoriteSubscriptionValue(contentVisibility, favorite any) map[string]any {
	value := subscriptionValue(seededCommunityDID, "2026-01-02T03:04:05Z", contentVisibility)
	if favorite != nil {
		value["favorite"] = favorite
	}
	return value
}

func TestSubscribeToCommunity_FavoriteOnNewRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name         string
		favorite     *bool
		wantFavorite bool
	}{
		{name: "favorite true is written", favorite: boolPtr(true), wantFavorite: true},
		{name: "favorite omitted is not true", favorite: nil, wantFavorite: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, _, userPDS := writeForwardService(t)

			subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 4, tt.favorite)
			require.NoError(t, err)

			require.Len(t, userPDS.created, 1, "no record names this community, so exactly one is created")
			write := userPDS.created[0]
			assert.Equal(t, 4, write.record["contentVisibility"])
			if tt.wantFavorite {
				assert.Equal(t, true, write.record["favorite"], "the new subscription record must carry favorite: true")
			} else {
				favorite, present := write.record["favorite"]
				if present {
					assert.Equal(t, false, favorite, "an omitted favorite may only be written as false")
				}
			}

			require.NotNil(t, subscription)
			assert.Equal(t, tt.wantFavorite, subscription.Favorite)
		})
	}
}

func TestSubscribeToCommunity_SuppliedFavoriteRewritesExistingRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name                  string
		listedVisibility      any
		listedFavorite        any // nil = absent from the listed record
		contentVisibility     int // 0 = omitted
		favorite              *bool
		wantContentVisibility int
		wantFavorite          bool
	}{
		{
			name:             "favorite true on a record without favorite keeps listed contentVisibility",
			listedVisibility: float64(2), listedFavorite: nil,
			contentVisibility: 0, favorite: boolPtr(true),
			wantContentVisibility: 2, wantFavorite: true,
		},
		{
			name:             "favorite false on a favorite record",
			listedVisibility: float64(4), listedFavorite: true,
			contentVisibility: 0, favorite: boolPtr(false),
			wantContentVisibility: 4, wantFavorite: false,
		},
		{
			name:             "both supplied are both written",
			listedVisibility: float64(2), listedFavorite: false,
			contentVisibility: 5, favorite: boolPtr(true),
			wantContentVisibility: 5, wantFavorite: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, _, userPDS := writeForwardService(t)
			userPDS.records = []pds.RecordEntry{
				subscriptionEntry("3kaaaaaaaaaa2", "bafylisted",
					favoriteSubscriptionValue(tt.listedVisibility, tt.listedFavorite)),
			}

			subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, tt.contentVisibility, tt.favorite)
			require.NoError(t, err)

			require.Empty(t, userPDS.createAttempts, "the PDS already holds a subscription record for this community")
			require.Len(t, userPDS.put, 1, "a supplied favorite rewrites the matched record")
			write := userPDS.put[0]
			assert.Equal(t, "3kaaaaaaaaaa2", write.rkey)
			assert.Equal(t, "social.coves.community.subscription", write.record["$type"])
			assert.Equal(t, seededCommunityDID, write.record["subject"])
			assert.Equal(t, "2026-01-02T03:04:05Z", write.record["createdAt"])
			assert.EqualValues(t, tt.wantContentVisibility, write.record["contentVisibility"])
			assert.Equal(t, tt.wantFavorite, write.record["favorite"])

			require.NotNil(t, subscription)
			assert.True(t, subscription.Existing)
			assert.Equal(t, "bafyput3kaaaaaaaaaa2", subscription.RecordCID)
			assert.Equal(t, tt.wantContentVisibility, subscription.ContentVisibility)
			assert.Equal(t, tt.wantFavorite, subscription.Favorite)
		})
	}
}

func TestSubscribeToCommunity_OmittedFavoriteKeepsListedFavorite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("nothing supplied writes nothing and reports the listed favorite", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)
		userPDS.records = []pds.RecordEntry{
			subscriptionEntry("3kaaaaaaaaaa2", "bafylisted", favoriteSubscriptionValue(float64(2), true)),
		}

		subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 0, nil)
		require.NoError(t, err)

		require.Empty(t, userPDS.createAttempts)
		require.Empty(t, userPDS.putAttempts, "omitting both values never rewrites the existing record")
		require.NotNil(t, subscription)
		assert.True(t, subscription.Existing)
		assert.Equal(t, "bafylisted", subscription.RecordCID)
		assert.Equal(t, 2, subscription.ContentVisibility)
		assert.True(t, subscription.Favorite, "the listed record is a favorite")
	})

	t.Run("only contentVisibility supplied keeps the listed favorite in the rewrite", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)
		userPDS.records = []pds.RecordEntry{
			subscriptionEntry("3kaaaaaaaaaa2", "bafylisted", favoriteSubscriptionValue(float64(2), true)),
		}

		subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 5, nil)
		require.NoError(t, err)

		require.Empty(t, userPDS.createAttempts)
		require.Len(t, userPDS.put, 1)
		write := userPDS.put[0]
		assert.Equal(t, "3kaaaaaaaaaa2", write.rkey)
		assert.Equal(t, 5, write.record["contentVisibility"])
		assert.Equal(t, true, write.record["favorite"],
			"rewriting contentVisibility must not drop the listed favorite")

		require.NotNil(t, subscription)
		assert.Equal(t, 5, subscription.ContentVisibility)
		assert.True(t, subscription.Favorite)
	})
}

// A favorite-only rewrite carries over the listed contentVisibility the way the
// Jetstream consumer indexes it: a number is truncated and clamped to 1-5, a
// missing or non-number value is the default 3. Carrying over any other value
// would silently change the visibility the index already shows.
func TestSubscribeToCommunity_FavoriteOnlyRewriteCarriesIndexedVisibility(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name                  string
		listedVisibility      any // nil = absent from the listed record
		wantContentVisibility int
	}{
		{name: "listed 7 is indexed as 5", listedVisibility: float64(7), wantContentVisibility: 5},
		{name: "listed 4.5 is indexed as 4", listedVisibility: float64(4.5), wantContentVisibility: 4},
		{name: "listed non-number is indexed as 3", listedVisibility: "high", wantContentVisibility: 3},
		{name: "listed absent is indexed as 3", listedVisibility: nil, wantContentVisibility: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, _, userPDS := writeForwardService(t)
			userPDS.records = []pds.RecordEntry{
				subscriptionEntry("3kaaaaaaaaaa2", "bafylisted",
					favoriteSubscriptionValue(tt.listedVisibility, nil)),
			}

			subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 0, boolPtr(true))
			require.NoError(t, err)

			require.Empty(t, userPDS.createAttempts)
			require.Len(t, userPDS.put, 1, "a supplied favorite rewrites the matched record")
			write := userPDS.put[0]
			assert.Equal(t, "3kaaaaaaaaaa2", write.rkey)
			assert.EqualValues(t, tt.wantContentVisibility, write.record["contentVisibility"],
				"the rewrite must keep the contentVisibility the index shows for this record")
			assert.Equal(t, true, write.record["favorite"])

			require.NotNil(t, subscription)
			assert.Equal(t, tt.wantContentVisibility, subscription.ContentVisibility)
			assert.True(t, subscription.Favorite)
		})
	}
}
