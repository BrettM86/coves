//go:build integration

package postgres

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/internal/core/timeline"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hours(n int) time.Duration { return time.Duration(n) * time.Hour }

// I1: the weighted Hot rank multiplies a non-negative rank by the level's
// multiplier and divides a negative one by it, evaluated in Postgres.
func TestTimelineWeightedHotRankSQL_EvaluatesPerLevelAndSign(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	rankingTime := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	createdAt := rankingTime.Add(-6 * time.Hour)

	query := `SELECT ` + timelineWeightedHotRankSQL("p", "$3::timestamptz", "p.cv") + `
		FROM (VALUES ($1::integer, $2::timestamptz, $4::integer)) AS p(score, created_at, cv)`

	multipliers := []struct {
		level      int
		multiplier float64
	}{
		{level: 1, multiplier: 1.0},
		{level: 2, multiplier: 1.0},
		{level: 3, multiplier: 1.0},
		{level: 4, multiplier: 1.5},
		{level: 5, multiplier: 2.0},
	}
	for _, score := range []int{5, -3} {
		neutral := discover.DiscoverHotRank(score, createdAt, rankingTime, 1, 0)
		for _, level := range multipliers {
			t.Run(fmt.Sprintf("level %d score %d", level.level, score), func(t *testing.T) {
				want := neutral * level.multiplier
				if neutral < 0 {
					want = neutral / level.multiplier
				}

				var got float64
				err := db.QueryRowContext(context.Background(), query,
					score, createdAt, rankingTime, level.level).Scan(&got)
				require.NoError(t, err)

				relativeError := math.Abs(got-want) / math.Abs(want)
				assert.Truef(t, relativeError <= 1e-12,
					"weighted rank %0.17g, want %0.17g (relative error %g)", got, want, relativeError)
			})
		}
	}
}

// I2 (acceptance): levels 1 and 2 keep only the community's top 10% / 25% of
// window posts by score; level 3 is untouched; another user's subscriptions
// to the same communities do not leak into the viewer's levels. The other
// subscriber is at level 2 on A and E, where the viewer is at level 1: counting
// their subscription would double N and give each community a second line.
func TestTimelineFrequency_LevelsOneAndTwoKeepTopOfWindow(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqacc")
	const otherSubscriber = "did:plc:freqaccother"
	createTestUser(t, db, "freqaccother.test", otherSubscriber)

	const (
		communityA = "did:plc:freqacca"
		communityB = "did:plc:freqaccb"
		communityC = "did:plc:freqaccc"
		communityD = "did:plc:freqaccd"
		communityE = "did:plc:freqacce"
	)
	createTestCommunity(t, db, communityA, "c-freqacca.coves.social", cast.viewer)
	createTestCommunity(t, db, communityB, "c-freqaccb.coves.social", cast.viewer)
	createTestCommunity(t, db, communityC, "c-freqaccc.coves.social", cast.viewer)
	createTestCommunity(t, db, communityD, "c-freqaccd.coves.social", cast.viewer)
	createTestCommunity(t, db, communityE, "c-freqacce.coves.social", cast.viewer)

	subscribeAtLevel(t, db, cast.viewer, communityA, 1)
	subscribeAtLevel(t, db, cast.viewer, communityB, 1)
	subscribeAtLevel(t, db, cast.viewer, communityC, 2)
	subscribeAtLevel(t, db, cast.viewer, communityD, 3)
	subscribeAtLevel(t, db, cast.viewer, communityE, 1)
	subscribeAtLevel(t, db, otherSubscriber, communityA, 2)
	subscribeAtLevel(t, db, otherSubscriber, communityD, 1)
	subscribeAtLevel(t, db, otherSubscriber, communityE, 2)

	now := time.Now().UTC()
	seedFrequencyPosts(t, db, communityA, cast.thirdParty, now,
		frequencyRun("a", 1, 20, func(score int) time.Duration { return hours(2 * score) })...)
	seedFrequencyPosts(t, db, communityB, cast.thirdParty, now,
		frequencyRun("b", 101, 120, func(score int) time.Duration { return hours(2*(score-100) + 1) })...)
	seedFrequencyPosts(t, db, communityC, cast.thirdParty, now,
		frequencyRun("c", 1, 20, func(score int) time.Duration { return hours(50 + 3*score) })...)
	seedFrequencyPosts(t, db, communityD, cast.thirdParty, now,
		frequencyPost{rkey: "d1", score: 7, age: hours(5)},
		frequencyPost{rkey: "d2", score: 0, age: hours(10)},
		frequencyPost{rkey: "d3", score: -2, age: hours(20)},
		frequencyPost{rkey: "d4", score: 3, age: hours(60)},
		frequencyPost{rkey: "d5", score: 50, age: hours(200)},
	)
	// N = 4, ceil(0.4) = 1: only e30 passes. Counting the other subscriber's
	// rows makes N = 8 with every post twice; ranks 1 (level 1) and 2 (level 2)
	// are both copies of e30, so E gets either two lines, which repeats e30, or
	// none, which drops it, whatever order the tied copies sort in. On A the
	// same leak picks rank 4 or 10 of 40 by tie order, so A alone cannot pin it.
	seedFrequencyPosts(t, db, communityE, cast.thirdParty, now,
		frequencyPost{rkey: "e30", score: 30, age: hours(15)},
		frequencyPost{rkey: "e4", score: 4, age: hours(16)},
		frequencyPost{rkey: "e2", score: 2, age: hours(17)},
		frequencyPost{rkey: "e1", score: 1, age: hours(18)},
	)

	a19, a20 := frequencyPostURI(communityA, "a19"), frequencyPostURI(communityA, "a20")
	b119, b120 := frequencyPostURI(communityB, "b119"), frequencyPostURI(communityB, "b120")
	c16, c17, c18, c19, c20 := frequencyPostURI(communityC, "c16"), frequencyPostURI(communityC, "c17"),
		frequencyPostURI(communityC, "c18"), frequencyPostURI(communityC, "c19"), frequencyPostURI(communityC, "c20")
	d1, d2, d3, d4, d5 := frequencyPostURI(communityD, "d1"), frequencyPostURI(communityD, "d2"),
		frequencyPostURI(communityD, "d3"), frequencyPostURI(communityD, "d4"), frequencyPostURI(communityD, "d5")
	e30 := frequencyPostURI(communityE, "e30")

	want := map[string][]string{
		// Unweighted hot ranks at gravity 0.8.
		"hot": {d1, e30, b119, b120, a19, a20, d2, c16, c17, c18, c19, c20, d4, d5, d3},
		// Score DESC, ties (20, 19) broken by created_at DESC.
		"top": {b120, b119, d5, e30, a20, c20, a19, c19, c18, c17, c16, d1, d4, d2, d3},
		"new": {d1, d2, e30, d3, a19, b119, a20, b120, d4, c16, c17, c18, c19, c20, d5},
	}

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			assert.Equal(t, want[read.sort], readWholeTimeline(t, repo, cast.viewer, read.sort, read.timeframe))
		})
	}
}

// I3 (guard): a viewer subscribed only at level 3 gets today's feed, page by
// page, for mixed positive, zero and negative scores.
func TestTimelineFrequency_LevelThreeFeedUnchanged(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqguard")
	const community = "did:plc:freqguardcommunity"
	createTestCommunity(t, db, community, "c-freqguard.coves.social", cast.viewer)
	subscribeToCommunity(t, db, cast.viewer, community)

	now := time.Now().UTC()
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyPost{rkey: "g1", score: 10, age: hours(1)},
		frequencyPost{rkey: "g2", score: 0, age: hours(2)},
		frequencyPost{rkey: "g3", score: -5, age: hours(3)},
		frequencyPost{rkey: "g4", score: 3, age: hours(30)},
		frequencyPost{rkey: "g5", score: 100, age: hours(72)},
		frequencyPost{rkey: "g6", score: -1, age: hours(100)},
		frequencyPost{rkey: "g7", score: 1, age: hours(5)},
	)
	g := func(rkey string) string { return frequencyPostURI(community, rkey) }

	want := map[string][]string{
		// Unweighted hot ranks at gravity 0.8.
		"hot": {g("g1"), g("g7"), g("g2"), g("g5"), g("g4"), g("g6"), g("g3")},
		"top": {g("g5"), g("g1"), g("g4"), g("g7"), g("g2"), g("g6"), g("g3")},
		"new": {g("g1"), g("g2"), g("g3"), g("g7"), g("g4"), g("g5"), g("g6")},
	}

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			var pages [][]string
			var cursor *string
			for page := 0; page < 4; page++ {
				feed, next, err := repo.GetTimeline(context.Background(), timeline.GetTimelineRequest{
					UserDID:   cast.viewer,
					Sort:      read.sort,
					Timeframe: read.timeframe,
					Limit:     3,
					Cursor:    cursor,
				})
				require.NoError(t, err)
				pages = append(pages, timelineURIs(feed))
				if next == nil {
					break
				}
				cursor = next
			}

			expected := want[read.sort]
			assert.Equal(t, [][]string{expected[0:3], expected[3:6], expected[6:7]}, pages)
		})
	}
}

// I4: at levels 1 and 2 a post with score 0 or below never appears, even when
// the community's threshold is itself 0 or negative.
func TestTimelineFrequency_NonPositivePostsNeverAppearAtLevelsOneAndTwo(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqnonpos")

	const (
		zeroLineLevelOne     = "did:plc:freqnonposzero1"
		negativeLineLevelOne = "did:plc:freqnonposneg1"
		zeroLineLevelTwo     = "did:plc:freqnonposzero2"
		control              = "did:plc:freqnonposcontrol"
	)
	createTestCommunity(t, db, zeroLineLevelOne, "c-freqnonposzero1.coves.social", cast.viewer)
	createTestCommunity(t, db, negativeLineLevelOne, "c-freqnonposneg1.coves.social", cast.viewer)
	createTestCommunity(t, db, zeroLineLevelTwo, "c-freqnonposzero2.coves.social", cast.viewer)
	createTestCommunity(t, db, control, "c-freqnonposcontrol.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, zeroLineLevelOne, 1)
	subscribeAtLevel(t, db, cast.viewer, negativeLineLevelOne, 1)
	subscribeAtLevel(t, db, cast.viewer, zeroLineLevelTwo, 2)
	subscribeAtLevel(t, db, cast.viewer, control, 3)

	now := time.Now().UTC()
	// N = 3, ceil(0.3) = 1: the line is the best score, 0.
	seedFrequencyPosts(t, db, zeroLineLevelOne, cast.thirdParty, now,
		frequencyPost{rkey: "z1zero", score: 0, age: hours(2)},
		frequencyPost{rkey: "z1neg1", score: -1, age: hours(4)},
		frequencyPost{rkey: "z1neg2", score: -2, age: hours(6)},
	)
	// N = 2, ceil(0.2) = 1: the line is -1.
	seedFrequencyPosts(t, db, negativeLineLevelOne, cast.thirdParty, now,
		frequencyPost{rkey: "n1neg1", score: -1, age: hours(3)},
		frequencyPost{rkey: "n1neg3", score: -3, age: hours(5)},
	)
	// N = 4, ceil(1.0) = 1: the line is 0, shared by two posts.
	seedFrequencyPosts(t, db, zeroLineLevelTwo, cast.thirdParty, now,
		frequencyPost{rkey: "z2zeroa", score: 0, age: hours(7)},
		frequencyPost{rkey: "z2zerob", score: 0, age: hours(8)},
		frequencyPost{rkey: "z2neg1", score: -1, age: hours(9)},
		frequencyPost{rkey: "z2neg4", score: -4, age: hours(10)},
	)
	seedFrequencyPosts(t, db, control, cast.thirdParty, now,
		frequencyPost{rkey: "ctrl", score: 1, age: hours(1)},
	)

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			assert.Equal(t,
				[]string{frequencyPostURI(control, "ctrl")},
				readWholeTimeline(t, repo, cast.viewer, read.sort, read.timeframe))
		})
	}
}
