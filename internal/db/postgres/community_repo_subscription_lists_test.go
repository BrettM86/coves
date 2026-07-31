//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"
)

// The two subscription listings, which were the last uncovered functions in
// community_repo_subscriptions.go.
//
// They are near-identical queries over the same table, differing only in which
// column they filter on — one answers "what does this user follow", the other
// "who follows this community" — and that symmetry is precisely why they are
// worth testing together: a copy-paste that left the wrong column in the WHERE
// clause returns plausible rows for the wrong question, and neither caller
// checks.
//
// Both order by subscribed_at DESC, which is what a client paginates against.
// The fixtures below use explicit, distinct timestamps rather than letting the
// database stamp them, because two subscriptions created in the same
// millisecond order arbitrarily and a pagination test built on that flakes.

// subscriptionFixture seeds two communities and three subscriptions across two
// users, so both listings have something to exclude as well as something to
// return.
type subscriptionFixture struct {
	repo      communities.Repository
	gardening *communities.Community
	woodwork  *communities.Community
	alice     string
	bob       string
	base      time.Time
}

func newSubscriptionFixture(t *testing.T) subscriptionFixture {
	t.Helper()
	ctx := context.Background()
	repo := NewCommunityRepository(testkit.DB(t))
	id := testkit.UniqueID(t)

	seedCommunity := func(name string) *communities.Community {
		community, err := repo.Create(ctx, &communities.Community{
			DID:          "did:plc:sub-" + name + id,
			Handle:       "c-" + name + "-" + id + ".coves.social",
			Name:         name + "-" + id,
			OwnerDID:     "did:web:coves.social",
			CreatedByDID: "did:plc:subcreator",
			HostedByDID:  "did:web:coves.social",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		})
		require.NoError(t, err)
		return community
	}

	fixture := subscriptionFixture{
		repo:      repo,
		gardening: seedCommunity("gardening"),
		woodwork:  seedCommunity("woodwork"),
		alice:     "did:plc:alice0000000000000" + id[:1],
		bob:       "did:plc:bob00000000000000" + id[:1],
		base:      time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
	}
	return fixture
}

func (f subscriptionFixture) subscribe(
	t *testing.T, userDID string, community *communities.Community, rkey string, at time.Time, visibility int,
) {
	t.Helper()
	_, err := f.repo.Subscribe(context.Background(), &communities.Subscription{
		UserDID:           userDID,
		CommunityDID:      community.DID,
		SubscribedAt:      at,
		RecordURI:         "at://" + userDID + "/social.coves.community.subscription/" + rkey,
		RecordCID:         "bafycid" + rkey,
		ContentVisibility: visibility,
	})
	require.NoError(t, err)
}

func TestCommunityRepo_ListSubscriptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("returns one user's subscriptions, newest first", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)
		fixture.subscribe(t, fixture.alice, fixture.gardening, "3kaaa", fixture.base, 3)
		fixture.subscribe(t, fixture.alice, fixture.woodwork, "3kbbb", fixture.base.Add(time.Hour), 5)
		fixture.subscribe(t, fixture.bob, fixture.gardening, "3kccc", fixture.base.Add(2*time.Hour), 1)

		got, err := fixture.repo.ListSubscriptions(ctx, fixture.alice, 10, 0)
		require.NoError(t, err)
		require.Len(t, got, 2, "bob's subscription must not appear in alice's list")
		assert.Equal(t, fixture.woodwork.DID, got[0].CommunityDID,
			"the most recent subscription comes first: this is the order a 'your communities' list "+
				"is rendered in, and it is what the offset paginates through")
		assert.Equal(t, fixture.gardening.DID, got[1].CommunityDID)
	})

	t.Run("carries the record identity and the content-visibility slider", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)
		fixture.subscribe(t, fixture.alice, fixture.gardening, "3kaaa", fixture.base, 4)

		got, err := fixture.repo.ListSubscriptions(ctx, fixture.alice, 10, 0)
		require.NoError(t, err)
		require.Len(t, got, 1)

		assert.Equal(t, 4, got[0].ContentVisibility,
			"the slider is per subscription and decides how much of the community's content reaches "+
				"the user's timeline; a listing that lost it would render every subscription at the "+
				"default")
		assert.Equal(t, "at://"+fixture.alice+"/social.coves.community.subscription/3kaaa", got[0].RecordURI,
			"the record URI is how an unsubscribe finds the record key to delete")
		assert.Equal(t, "bafycid3kaaa", got[0].RecordCID)
		assert.NotZero(t, got[0].ID)
		assert.WithinDuration(t, fixture.base, got[0].SubscribedAt, time.Second)
	})

	t.Run("paginates without skipping or repeating", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)
		fixture.subscribe(t, fixture.alice, fixture.gardening, "3kaaa", fixture.base, 3)
		fixture.subscribe(t, fixture.alice, fixture.woodwork, "3kbbb", fixture.base.Add(time.Hour), 3)

		first, err := fixture.repo.ListSubscriptions(ctx, fixture.alice, 1, 0)
		require.NoError(t, err)
		require.Len(t, first, 1)
		second, err := fixture.repo.ListSubscriptions(ctx, fixture.alice, 1, 1)
		require.NoError(t, err)
		require.Len(t, second, 1)

		assert.Equal(t, fixture.woodwork.DID, first[0].CommunityDID)
		assert.Equal(t, fixture.gardening.DID, second[0].CommunityDID)

		past, err := fixture.repo.ListSubscriptions(ctx, fixture.alice, 1, 2)
		require.NoError(t, err)
		assert.Empty(t, past)
	})

	t.Run("answers a user with no subscriptions with an empty slice", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)

		got, err := fixture.repo.ListSubscriptions(ctx, "did:plc:nobody000000000000", 10, 0)
		require.NoError(t, err)
		require.NotNil(t, got, "callers marshal this to JSON, where nil is null and empty is []")
		assert.Empty(t, got)
	})

	t.Run("forgets a subscription that was withdrawn", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)
		fixture.subscribe(t, fixture.alice, fixture.gardening, "3kaaa", fixture.base, 3)
		require.NoError(t, fixture.repo.Unsubscribe(ctx, fixture.alice, fixture.gardening.DID))

		got, err := fixture.repo.ListSubscriptions(ctx, fixture.alice, 10, 0)
		require.NoError(t, err)
		assert.Empty(t, got, "an unsubscribed community still listed is one the user cannot leave")
	})
}

func TestCommunityRepo_ListSubscribers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("returns one community's subscribers, newest first", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)
		fixture.subscribe(t, fixture.alice, fixture.gardening, "3kaaa", fixture.base, 3)
		fixture.subscribe(t, fixture.bob, fixture.gardening, "3kbbb", fixture.base.Add(time.Hour), 3)
		fixture.subscribe(t, fixture.alice, fixture.woodwork, "3kccc", fixture.base.Add(2*time.Hour), 3)

		got, err := fixture.repo.ListSubscribers(ctx, fixture.gardening.DID, 10, 0)
		require.NoError(t, err)
		require.Len(t, got, 2,
			"the woodwork subscription must not appear here. The two listings differ only in which "+
				"column they filter on, so this is the assertion that catches the copy-paste")
		assert.Equal(t, []string{fixture.bob, fixture.alice},
			[]string{got[0].UserDID, got[1].UserDID},
			"newest subscriber first")
	})

	t.Run("paginates", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)
		fixture.subscribe(t, fixture.alice, fixture.gardening, "3kaaa", fixture.base, 3)
		fixture.subscribe(t, fixture.bob, fixture.gardening, "3kbbb", fixture.base.Add(time.Hour), 3)

		first, err := fixture.repo.ListSubscribers(ctx, fixture.gardening.DID, 1, 0)
		require.NoError(t, err)
		require.Len(t, first, 1)
		assert.Equal(t, fixture.bob, first[0].UserDID)

		second, err := fixture.repo.ListSubscribers(ctx, fixture.gardening.DID, 1, 1)
		require.NoError(t, err)
		require.Len(t, second, 1)
		assert.Equal(t, fixture.alice, second[0].UserDID)
	})

	t.Run("answers a community nobody follows with an empty slice", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)

		got, err := fixture.repo.ListSubscribers(ctx, fixture.woodwork.DID, 10, 0)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("answers an unknown community with an empty slice rather than everybody", func(t *testing.T) {
		t.Parallel()
		fixture := newSubscriptionFixture(t)
		fixture.subscribe(t, fixture.alice, fixture.gardening, "3kaaa", fixture.base, 3)

		got, err := fixture.repo.ListSubscribers(ctx, "did:plc:nosuchcommunity0000", 10, 0)
		require.NoError(t, err)
		assert.Empty(t, got,
			"a filter that silently dropped out for an unknown DID would leak every subscriber in the "+
				"table to a caller naming a community that does not exist")
	})
}
