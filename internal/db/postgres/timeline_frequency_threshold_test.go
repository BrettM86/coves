//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Threshold-population tests for the level 1/2 frequency filter: which posts
// count toward N, how the line is rounded, and how ties and old posts fare.

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// setPostScore overwrites a seeded post's score.
func setPostScore(t *testing.T, db *sql.DB, uri string, score int) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `UPDATE posts SET score = $1 WHERE uri = $2`, score, uri)
	require.NoErrorf(t, err, "scoring post %s", uri)
}

// I5: every post tied at the line passes; positive posts below it do not.
func TestTimelineFrequency_TiesAtThresholdAllPass(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqtie")
	const community = "did:plc:freqtiecommunity"
	createTestCommunity(t, db, community, "c-freqtie.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, community, 1)

	now := time.Now().UTC()
	// N = 20, ceil(2.0) = 2: the rank-2 score is 18, shared by three posts.
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyRun("t", 1, 16, func(score int) time.Duration { return hours(20 + score) })...)
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyPost{rkey: "t20", score: 20, age: hours(2)},
		frequencyPost{rkey: "t18a", score: 18, age: hours(5)},
		frequencyPost{rkey: "t18b", score: 18, age: hours(8)},
		frequencyPost{rkey: "t18c", score: 18, age: hours(11)},
	)
	tie := func(rkey string) string { return frequencyPostURI(community, rkey) }

	// Score and recency agree, so every sort serves the same order.
	want := []string{tie("t20"), tie("t18a"), tie("t18b"), tie("t18c")}

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			assert.Equal(t, want, readWholeTimeline(t, repo, cast.viewer, read.sort, read.timeframe))
		})
	}
}

// I6a: posts older than 14 days are filtered against the window's line but
// do not count toward N; a 13-day-old post counts and a 15-day-old one does
// not, so the window is neither shorter than 13 days nor longer than 15.
func TestTimelineFrequency_OldPostsFilteredButNotCounted(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqold")
	const community = "did:plc:freqoldcommunity"
	createTestCommunity(t, db, community, "c-freqold.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, community, 1)

	now := time.Now().UTC()
	// Window (w1..w10 and n1): N = 11, ceil(1.1) = 2, line = 9.
	// A window under 13 days drops n1: N = 10, ceil(1.0) = 1, line = 10, and
	// w9 goes. A window over 15 days adds o30: N = 12, ceil(1.2) = 2,
	// line = 10, and w9 goes. Counting every old post: N = 15, line = 40.
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyRun("w", 1, 10, func(score int) time.Duration { return hours(2 * score) })...)
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyPost{rkey: "n1", score: 1, age: days(13)},
		frequencyPost{rkey: "o30", score: 30, age: days(15)},
		frequencyPost{rkey: "o50", score: 50, age: days(20)},
		frequencyPost{rkey: "o5", score: 5, age: days(21)},
		frequencyPost{rkey: "o40", score: 40, age: days(22)},
	)
	w9, w10 := frequencyPostURI(community, "w9"), frequencyPostURI(community, "w10")
	o30, o50, o40 := frequencyPostURI(community, "o30"), frequencyPostURI(community, "o50"), frequencyPostURI(community, "o40")

	want := map[string][]string{
		// Unweighted hot ranks 0.0369, 0.0329, 0.000644, 0.000466, 0.000386.
		"hot": {w9, w10, o30, o50, o40},
		"top": {o50, o40, o30, w10, w9},
		"new": {w9, w10, o30, o50, o40},
	}

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			assert.Equal(t, want[read.sort], readWholeTimeline(t, repo, cast.viewer, read.sort, read.timeframe))
		})
	}
}

// I6b: the line is taken over the fixed 14-day window, not Top's timeframe.
func TestTimelineFrequency_ThresholdWindowIgnoresTopTimeframe(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqtimeframe")
	const (
		community = "did:plc:freqtimeframecommunity"
		control   = "did:plc:freqtimeframecontrol"
	)
	createTestCommunity(t, db, community, "c-freqtimeframe.coves.social", cast.viewer)
	createTestCommunity(t, db, control, "c-freqtimeframectl.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, community, 1)
	subscribeAtLevel(t, db, cast.viewer, control, 3)

	now := time.Now().UTC()
	// 14-day window: N = 20, line = 19. Only s1 is inside the last day; a line
	// taken over the day alone would be 1 and pass it.
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyRun("s", 1, 20, func(score int) time.Duration {
			if score == 1 {
				return hours(3)
			}
			return hours(30 + 10*score)
		})...)
	seedFrequencyPosts(t, db, control, cast.thirdParty, now,
		frequencyPost{rkey: "ctrl", score: 1, age: hours(1)},
	)

	repo := NewTimelineRepository(db, "test-secret")
	assert.Equal(t,
		[]string{frequencyPostURI(control, "ctrl")},
		readWholeTimeline(t, repo, cast.viewer, "top", "day"))
}

// I7: a level 1/2 community with no visible window posts contributes nothing,
// even with older positive posts; a level-2 community whose only window post
// scores 0 contributes nothing.
func TestTimelineFrequency_NoVisibleWindowPostsContributesNothing(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqempty")
	const (
		deletedOnlyLevelOne = "did:plc:freqemptydeleted1"
		oldOnlyLevelTwo     = "did:plc:freqemptyold2"
		zeroOnlyLevelTwo    = "did:plc:freqemptyzero2"
		control             = "did:plc:freqemptycontrol"
	)
	createTestCommunity(t, db, deletedOnlyLevelOne, "c-freqemptydeleted1.coves.social", cast.viewer)
	createTestCommunity(t, db, oldOnlyLevelTwo, "c-freqemptyold2.coves.social", cast.viewer)
	createTestCommunity(t, db, zeroOnlyLevelTwo, "c-freqemptyzero2.coves.social", cast.viewer)
	createTestCommunity(t, db, control, "c-freqemptycontrol.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, deletedOnlyLevelOne, 1)
	subscribeAtLevel(t, db, cast.viewer, oldOnlyLevelTwo, 2)
	subscribeAtLevel(t, db, cast.viewer, zeroOnlyLevelTwo, 2)
	subscribeAtLevel(t, db, cast.viewer, control, 3)

	now := time.Now().UTC()
	// The only window post is deleted; counting it would give a line of 0
	// that both old posts clear.
	seedHotBoundDeletedPost(t, db, deletedOnlyLevelOne, cast.thirdParty, "edeleted", now.Add(-hours(2)))
	seedFrequencyPosts(t, db, deletedOnlyLevelOne, cast.thirdParty, now,
		frequencyPost{rkey: "e5", score: 5, age: days(20)},
		frequencyPost{rkey: "e50", score: 50, age: days(25)},
	)
	seedFrequencyPosts(t, db, oldOnlyLevelTwo, cast.thirdParty, now,
		frequencyPost{rkey: "f3", score: 3, age: days(21)},
		frequencyPost{rkey: "f30", score: 30, age: days(24)},
	)
	seedFrequencyPosts(t, db, zeroOnlyLevelTwo, cast.thirdParty, now,
		frequencyPost{rkey: "g0", score: 0, age: hours(3)},
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

// I8 (guard): a level-2 community's single visible window post is its best
// post and passes.
func TestTimelineFrequency_SingleWindowPostAppearsAtLevelTwo(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqsingle")
	const community = "did:plc:freqsinglecommunity"
	createTestCommunity(t, db, community, "c-freqsingle.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, community, 2)

	now := time.Now().UTC()
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyPost{rkey: "only", score: 4, age: hours(6)},
	)

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			assert.Equal(t,
				[]string{frequencyPostURI(community, "only")},
				readWholeTimeline(t, repo, cast.viewer, read.sort, read.timeframe))
		})
	}
}

// I9: the line is taken over the posts the viewer can see. Deleted, removed,
// pending-for-someone-else and blocked-author posts do not count; the
// viewer's own pending post does.
func TestTimelineFrequency_ThresholdCountsOnlyViewerVisiblePosts(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqvis")
	const community = "did:plc:freqviscommunity"
	createTestCommunity(t, db, community, "c-freqvis.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, community, 1)
	insertUserBlock(t, db, cast.viewer, cast.blocked)

	now := time.Now().UTC()
	// Visible to the viewer: N = 11, ceil(1.1) = 2, line = 25 (the viewer's
	// own pending post). Counting any one invisible post: N = 12, ceil = 2,
	// line = 30, which drops own25. Leaving own25 out of N: N = 10, ceil = 1,
	// line = 30, which drops it too.
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyRun("v", 1, 9, func(score int) time.Duration { return hours(30 + score) })...)
	seedFrequencyPosts(t, db, community, cast.thirdParty, now,
		frequencyPost{rkey: "tp30", score: 30, age: hours(5)},
	)
	own := seedHotBoundPost(t, db, community, cast.viewer, "own25", now.Add(-hours(3)), 25)
	seedHotBoundPendingAdmission(t, db, community, own)

	// Invisible to the viewer, each scored far above every visible post.
	seedHotBoundDeletedPost(t, db, community, cast.thirdParty, "xdeleted", now.Add(-hours(1)))
	setPostScore(t, db, frequencyPostURI(community, "xdeleted"), 1000)
	removed := seedHotBoundPost(t, db, community, cast.thirdParty, "xremoved", now.Add(-hours(2)), 900)
	seedModerationDecision(t, db, removed, "removal", community, true)
	pendingOther := seedHotBoundPost(t, db, community, cast.thirdParty, "xpending", now.Add(-hours(4)), 800)
	seedHotBoundPendingAdmission(t, db, community, pendingOther)
	seedHotBoundPost(t, db, community, cast.blocked, "xblocked", now.Add(-hours(6)), 700)

	own25, tp30 := frequencyPostURI(community, "own25"), frequencyPostURI(community, "tp30")
	want := map[string][]string{
		// Unweighted hot ranks 0.381, 0.239.
		"hot": {own25, tp30},
		"top": {tp30, own25},
		"new": {own25, tp30},
	}

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			assert.Equal(t, want[read.sort], readWholeTimeline(t, repo, cast.viewer, read.sort, read.timeframe))
		})
	}
}

// I14: N counts non-positive window posts, and the rank is rounded up.
func TestTimelineFrequency_ThresholdCountsNonPositiveAndRoundsUp(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	cast := seedBlockFilterCast(t, db, "freqround")
	const (
		mostlyNonPositive = "did:plc:freqroundnonpos"
		elevenPosts       = "did:plc:freqroundeleven"
	)
	createTestCommunity(t, db, mostlyNonPositive, "c-freqroundnonpos.coves.social", cast.viewer)
	createTestCommunity(t, db, elevenPosts, "c-freqroundeleven.coves.social", cast.viewer)
	subscribeAtLevel(t, db, cast.viewer, mostlyNonPositive, 1)
	subscribeAtLevel(t, db, cast.viewer, elevenPosts, 1)

	now := time.Now().UTC()
	// N = 20 (scores 2, 1, 0, -1..-17), ceil(2.0) = 2, line = 1. Counting
	// positive posts only: N = 2, ceil(0.2) = 1, line = 2.
	seedFrequencyPosts(t, db, mostlyNonPositive, cast.thirdParty, now,
		frequencyRun("pneg", -17, 0, func(score int) time.Duration { return hours(30 - score) })...)
	seedFrequencyPosts(t, db, mostlyNonPositive, cast.thirdParty, now,
		frequencyPost{rkey: "p2", score: 2, age: hours(2)},
		frequencyPost{rkey: "p1", score: 1, age: hours(4)},
	)
	// N = 11, ceil(1.1) = 2, line = 10. Floor would give rank 1, line 11.
	seedFrequencyPosts(t, db, elevenPosts, cast.thirdParty, now,
		frequencyRun("q", 1, 9, func(score int) time.Duration { return hours(40 + score) })...)
	seedFrequencyPosts(t, db, elevenPosts, cast.thirdParty, now,
		frequencyPost{rkey: "q11", score: 11, age: hours(3)},
		frequencyPost{rkey: "q10", score: 10, age: hours(5)},
	)

	p2, p1 := frequencyPostURI(mostlyNonPositive, "p2"), frequencyPostURI(mostlyNonPositive, "p1")
	q11, q10 := frequencyPostURI(elevenPosts, "q11"), frequencyPostURI(elevenPosts, "q10")
	want := map[string][]string{
		// Unweighted hot ranks at gravity 0.8.
		"hot": {q11, q10, p2, p1},
		"top": {q11, q10, p2, p1},
		"new": {p2, q11, p1, q10},
	}

	repo := NewTimelineRepository(db, "test-secret")
	for _, read := range timelineFrequencySorts {
		t.Run(read.sort, func(t *testing.T) {
			assert.Equal(t, want[read.sort], readWholeTimeline(t, repo, cast.viewer, read.sort, read.timeframe))
		})
	}
}
