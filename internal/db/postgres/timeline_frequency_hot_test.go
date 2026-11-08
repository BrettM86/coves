//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/timeline"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hot ranks below are the neutral rank (sign(s)·ln(|s|+1)+1)/(ageHours+2)^g,
// worked out by hand at the original gravity g = 1.5; the asserted orders also
// hold at the current gravity 0.8; weighted = rank×m for rank ≥ 0 and rank÷m below zero,
// m = 1.5 at level 4 and 2.0 at level 5.

// readTimelinePages pages through the timeline at the given limit until the
// cursor runs out, failing past maxPages.
func readTimelinePages(t *testing.T, repo timeline.Repository, viewerDID, sort, timeframe string, limit, maxPages int) [][]string {
	t.Helper()

	var pages [][]string
	var cursor *string
	for page := 0; page < maxPages; page++ {
		feed, next, err := repo.GetTimeline(context.Background(), timeline.GetTimelineRequest{
			UserDID:   viewerDID,
			Sort:      sort,
			Timeframe: timeframe,
			Limit:     limit,
			Cursor:    cursor,
		})
		require.NoError(t, err)
		pages = append(pages, timelineURIs(feed))
		if next == nil {
			return pages
		}
		cursor = next
	}
	require.FailNowf(t, "timeline did not end", "still had a cursor after %d pages", maxPages)
	return nil
}

// I10: the weighted Hot rank moves a boosted post above an equal or slightly
// higher-ranked level-3 post, for positive and negative ranks.
func TestTimelineFrequency_HotWeightingOrdersBoostedPostsFirst(t *testing.T) {
	t.Parallel()

	// The boosted community's DID sorts lower, so on an exact unweighted tie
	// the uri DESC tiebreak puts the boosted post second.
	const (
		boostedCommunity = "did:plc:freqtiea"
		plainCommunity   = "did:plc:freqtieb"
	)
	cases := []struct {
		name         string
		boostedLevel int
		boosted      frequencyPost
		plain        frequencyPost
	}{
		{
			// Both rank 0.2496; weighted 0.4991 vs 0.2496.
			name:         "level 5 positive tie",
			boostedLevel: 5,
			boosted:      frequencyPost{rkey: "pair", score: 5, age: hours(3)},
			plain:        frequencyPost{rkey: "pair", score: 5, age: hours(3)},
		},
		{
			// Both rank -0.0345; weighted -0.0173 vs -0.0345.
			name:         "level 5 negative tie",
			boostedLevel: 5,
			boosted:      frequencyPost{rkey: "pair", score: -3, age: hours(3)},
			plain:        frequencyPost{rkey: "pair", score: -3, age: hours(3)},
		},
		{
			// Unweighted 0.1033 vs 0.1233 (×1.19 lower); weighted 0.1550.
			name:         "level 4 positive lower rank",
			boostedLevel: 4,
			boosted:      frequencyPost{rkey: "pair", score: 5, age: hours(7)},
			plain:        frequencyPost{rkey: "pair", score: 5, age: hours(6)},
		},
		{
			// Unweighted -0.0171 vs -0.0143 (×1.19 larger magnitude);
			// weighted -0.0114.
			name:         "level 4 negative larger magnitude",
			boostedLevel: 4,
			boosted:      frequencyPost{rkey: "pair", score: -3, age: hours(6)},
			plain:        frequencyPost{rkey: "pair", score: -3, age: hours(7)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			cast := seedBlockFilterCast(t, db, "freqtie")
			createTestCommunity(t, db, boostedCommunity, "c-freqtiea.coves.social", cast.viewer)
			createTestCommunity(t, db, plainCommunity, "c-freqtieb.coves.social", cast.viewer)
			subscribeAtLevel(t, db, cast.viewer, boostedCommunity, tc.boostedLevel)
			subscribeAtLevel(t, db, cast.viewer, plainCommunity, 3)

			now := time.Now().UTC()
			seedFrequencyPosts(t, db, boostedCommunity, cast.thirdParty, now, tc.boosted)
			seedFrequencyPosts(t, db, plainCommunity, cast.thirdParty, now, tc.plain)

			repo := NewTimelineRepository(db, "test-secret")
			assert.Equal(t, []string{
				frequencyPostURI(boostedCommunity, "pair"),
				frequencyPostURI(plainCommunity, "pair"),
			}, readWholeTimeline(t, repo, cast.viewer, "hot", ""))
		})
	}
}

// I11: with communities at levels 1, 3, 4 and 5, paging every sort with a
// cursor yields the hand-written order with no repeats or skips, and matches
// one large-limit read. A second user subscribes to every community at a
// different level, so each Hot page boundary (f1, f3, d2) has two subscription
// rows: the cursor post's multiplier must come from the viewer's row only.
func TestTimelineFrequency_MixedLevelsPageWithoutRepeatsOrSkips(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqpage")
	const (
		levelOne   = "did:plc:freqpagea"
		levelThree = "did:plc:freqpaged"
		levelFour  = "did:plc:freqpagef"
		levelFive  = "did:plc:freqpagev"
	)
	createTestCommunity(t, db, levelOne, "c-freqpagea.coves.social", cast.viewer)
	createTestCommunity(t, db, levelThree, "c-freqpaged.coves.social", cast.viewer)
	createTestCommunity(t, db, levelFour, "c-freqpagef.coves.social", cast.viewer)
	createTestCommunity(t, db, levelFive, "c-freqpagev.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, levelOne, 1)
	subscribeAtLevel(t, db, cast.viewer, levelThree, 3)
	subscribeAtLevel(t, db, cast.viewer, levelFour, 4)
	subscribeAtLevel(t, db, cast.viewer, levelFive, 5)
	const otherSubscriber = "did:plc:freqpageother"
	createTestUser(t, db, "freqpageother.test", otherSubscriber)
	subscribeAtLevel(t, db, otherSubscriber, levelOne, 5)
	subscribeAtLevel(t, db, otherSubscriber, levelThree, 1)
	subscribeAtLevel(t, db, otherSubscriber, levelFour, 2)
	subscribeAtLevel(t, db, otherSubscriber, levelFive, 3)

	now := time.Now().UTC()
	// Ten window posts scored 1..10: ceil(10×0.10) = 1, only a10 survives.
	seedFrequencyPosts(t, db, levelOne, cast.thirdParty, now,
		frequencyRun("a", 1, 10, func(score int) time.Duration { return hours(10 + score) })...)
	seedFrequencyPosts(t, db, levelThree, cast.thirdParty, now,
		frequencyPost{rkey: "d1", score: 12, age: hours(2)},
		frequencyPost{rkey: "d2", score: -5, age: hours(4)},
		frequencyPost{rkey: "d3", score: 0, age: hours(30)},
	)
	seedFrequencyPosts(t, db, levelFour, cast.thirdParty, now,
		frequencyPost{rkey: "f1", score: 20, age: hours(10)},
		frequencyPost{rkey: "f2", score: -4, age: hours(7)},
		frequencyPost{rkey: "f3", score: 2, age: hours(60)},
	)
	seedFrequencyPosts(t, db, levelFive, cast.thirdParty, now,
		frequencyPost{rkey: "v1", score: 3, age: hours(6)},
		frequencyPost{rkey: "v2", score: -20, age: hours(3)},
		frequencyPost{rkey: "v3", score: 50, age: hours(100)},
	)

	a10 := frequencyPostURI(levelOne, "a10")
	d1, d2, d3 := frequencyPostURI(levelThree, "d1"), frequencyPostURI(levelThree, "d2"), frequencyPostURI(levelThree, "d3")
	f1, f2, f3 := frequencyPostURI(levelFour, "f1"), frequencyPostURI(levelFour, "f2"), frequencyPostURI(levelFour, "f3")
	v1, v2, v3 := frequencyPostURI(levelFive, "v1"), frequencyPostURI(levelFive, "v2"), frequencyPostURI(levelFive, "v3")

	want := map[string][]string{
		// Weighted: d1 0.446, v1 0.211, f1 0.146, a10 0.033, v3 0.0096,
		// f3 0.0065, d3 0.0055, f2 -0.0150, d2 -0.0539, v2 -0.0914 (adjacent
		// pairs at least 16% apart). Unweighted puts d3 above v3 and f3.
		"hot": {d1, v1, f1, a10, v3, f3, d3, f2, d2, v2},
		"top": {v3, f1, d1, a10, v1, f3, d3, f2, d2, v2},
		"new": {d1, v2, d2, v1, f2, f1, a10, d3, f3, v3},
	}

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			pages := readTimelinePages(t, repo, cast.viewer, read.sort, read.timeframe, 3, 10)

			var concatenated []string
			seen := make(map[string]bool)
			for _, page := range pages {
				for _, uri := range page {
					assert.Falsef(t, seen[uri], "post %s repeated across pages", uri)
					seen[uri] = true
					concatenated = append(concatenated, uri)
				}
			}
			assert.GreaterOrEqual(t, len(pages), 3, "fixture must span at least three pages")
			assert.Equal(t, want[read.sort], concatenated, "paged order")
			assert.Equal(t, want[read.sort], readWholeTimeline(t, repo, cast.viewer, read.sort, read.timeframe),
				"single large-limit read")
		})
	}
}

// I12: when the viewer unsubscribes from the community of the hot cursor's
// last post, the next page still loads; the cursor boundary falls back to
// multiplier 1.0, so a level-4 post that ranked just below the cursor post
// now sits above the boundary and is not served.
func TestTimelineFrequency_HotCursorSurvivesUnsubscribingFromItsCommunity(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqcur")
	const (
		levelThree = "did:plc:freqcurd"
		levelFour  = "did:plc:freqcurf"
		levelFive  = "did:plc:freqcurv"
	)
	createTestCommunity(t, db, levelThree, "c-freqcurd.coves.social", cast.viewer)
	createTestCommunity(t, db, levelFour, "c-freqcurf.coves.social", cast.viewer)
	createTestCommunity(t, db, levelFive, "c-freqcurv.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, levelThree, 3)
	subscribeAtLevel(t, db, cast.viewer, levelFour, 4)
	subscribeAtLevel(t, db, cast.viewer, levelFive, 5)

	now := time.Now().UTC()
	// Neutral rank r = 0.2496 for both score-5 posts aged 3h.
	seedFrequencyPosts(t, db, levelThree, cast.thirdParty, now,
		frequencyPost{rkey: "d0", score: 100, age: hours(1)}, // 1.081
		frequencyPost{rkey: "d1", score: 2, age: hours(18)},  // 0.0235
	)
	seedFrequencyPosts(t, db, levelFour, cast.thirdParty, now,
		frequencyPost{rkey: "f1", score: 5, age: hours(3)},  // r×1.5 = 0.374
		frequencyPost{rkey: "f2", score: 2, age: hours(20)}, // 0.0203×1.5 = 0.0305
	)
	seedFrequencyPosts(t, db, levelFive, cast.thirdParty, now,
		frequencyPost{rkey: "v1", score: 5, age: hours(3)}, // r×2 = 0.499
	)

	repo := NewTimelineRepository(db, "test-secret")
	firstPage, cursor, err := repo.GetTimeline(context.Background(), timeline.GetTimelineRequest{
		UserDID: cast.viewer,
		Sort:    "hot",
		Limit:   2,
	})
	require.NoError(t, err)
	require.Equal(t, []string{frequencyPostURI(levelThree, "d0"), frequencyPostURI(levelFive, "v1")},
		timelineURIs(firstPage), "the first page must end on the level-5 post")
	require.NotNil(t, cursor)

	_, err = db.ExecContext(context.Background(),
		`DELETE FROM community_subscriptions WHERE user_did = $1 AND community_did = $2`,
		cast.viewer, levelFive)
	require.NoError(t, err, "unsubscribing from the cursor post's community")

	nextPage, _, err := repo.GetTimeline(context.Background(), timeline.GetTimelineRequest{
		UserDID: cast.viewer,
		Sort:    "hot",
		Limit:   10,
		Cursor:  cursor,
	})
	require.NoError(t, err, "a cursor whose community is no longer subscribed must still page")
	assert.Equal(t, []string{frequencyPostURI(levelFour, "f2"), frequencyPostURI(levelThree, "d1")},
		timelineURIs(nextPage),
		"boundary is r×1.0: f1 (r×1.5) stays above it; f2 outranks d1 only when weighted")
}

// I13 (guard): a level-1 subscription does not filter or reorder the
// community feed or Discover Hot.
func TestTimelineFrequency_CommunityFeedAndDiscoverIgnoreFrequency(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqother")
	// The only community in the database, with fewer than five posts, so
	// Discover has no normalization reference and no diversity choice; it is
	// not hosted, so it gets no hosted bonus.
	const community = "did:plc:freqothercommunity"
	createTestCommunity(t, db, community, "c-freqother.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, community, 1)

	now := time.Now().UTC()
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyPost{rkey: "o1", score: 10, age: hours(1)}, // 0.654
		frequencyPost{rkey: "o2", score: 1, age: hours(2)},  // 0.212
		frequencyPost{rkey: "o3", score: 0, age: hours(5)},  // 0.054
		frequencyPost{rkey: "o4", score: -3, age: hours(2)}, // -0.048
	)
	o := func(rkey string) string { return frequencyPostURI(community, rkey) }
	// The timeline would keep only o1 (ceil(4×0.10) = 1); these feeds keep all.
	want := []string{o("o1"), o("o2"), o("o3"), o("o4")}

	t.Run("community feed hot", func(t *testing.T) {
		repository := NewCommunityFeedRepository(db, "test-secret")
		var served []string
		var cursor *string
		for page := 0; page < 3; page++ {
			feed, next, err := repository.GetCommunityFeed(context.Background(), communityFeeds.GetCommunityFeedRequest{
				Community: community,
				ViewerDID: cast.viewer,
				Sort:      "hot",
				Limit:     2,
				Cursor:    cursor,
			})
			require.NoError(t, err)
			served = append(served, communityFeedURIs(feed)...)
			if next == nil {
				break
			}
			cursor = next
		}
		assert.Equal(t, want, served)
	})

	t.Run("discover hot", func(t *testing.T) {
		repository := NewDiscoverRepository(db, "test-secret")
		feed, _, err := repository.GetDiscover(context.Background(), discover.GetDiscoverRequest{
			ViewerDID: cast.viewer,
			Sort:      "hot",
			Limit:     10,
		})
		require.NoError(t, err)
		assert.Equal(t, want, discoverURIs(feed))
	})
}

// I15: the frequency filter applies to the posts inside the Hot candidate
// bound, not before it: the bound picks the two newest posts, which are below
// the level-1 threshold, so none are served.
func TestTimelineFrequency_FilterAppliesOutsideHotCandidateBound(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqbound")
	const community = "did:plc:freqboundcommunity"
	createTestCommunity(t, db, community, "c-freqbound.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, community, 1)

	now := time.Now().UTC()
	// Newest two scored 1, 2; older 3..20. Threshold ceil(20×0.10) = 2 keeps
	// only 19 and 20, which the bound of two never reaches.
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyRun("b", 1, 20, func(score int) time.Duration { return hours(score) })...)

	repository := NewTimelineRepository(db, "test-secret").(*postgresTimelineRepo)
	repository.hotCandidateLimit = 2

	feed, cursor, err := repository.GetTimeline(context.Background(), timeline.GetTimelineRequest{
		UserDID: cast.viewer,
		Sort:    "hot",
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Empty(t, timelineURIs(feed), "bounded candidates below the threshold are not served")
	assert.Nil(t, cursor)
}
