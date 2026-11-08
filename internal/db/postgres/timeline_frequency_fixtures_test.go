//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/timeline"

	"github.com/stretchr/testify/require"
)

// Shared fixtures for the subscription-frequency timeline tests.

// subscribeAtLevel indexes a subscription with the given content_visibility
// (1..5). subscribeToCommunity covers the default level 3.
func subscribeAtLevel(t *testing.T, db *sql.DB, userDID, communityDID string, level int) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at, content_visibility)
		VALUES ($1, $2, NOW(), $3)
	`, userDID, communityDID, level)
	require.NoErrorf(t, err, "indexing the level-%d subscription", level)
}

// frequencyPost is one post to seed: its rkey, score and age relative to the
// test's reference time.
type frequencyPost struct {
	rkey  string
	score int
	age   time.Duration
}

// frequencyRun builds posts scored firstScore..lastScore, rkey prefix+score,
// each aged by age(score).
func frequencyRun(prefix string, firstScore, lastScore int, age func(score int) time.Duration) []frequencyPost {
	run := make([]frequencyPost, 0, lastScore-firstScore+1)
	for score := firstScore; score <= lastScore; score++ {
		run = append(run, frequencyPost{rkey: fmt.Sprintf("%s%d", prefix, score), score: score, age: age(score)})
	}
	return run
}

// seedFrequencyPosts seeds visible, scored posts created at now - age.
func seedFrequencyPosts(t *testing.T, db *sql.DB, communityDID, authorDID string, now time.Time, posts ...frequencyPost) {
	t.Helper()

	for _, post := range posts {
		seedHotBoundPost(t, db, communityDID, authorDID, post.rkey, now.Add(-post.age), post.score)
	}
}

// frequencyPostURI is the URI seedFilterablePost gives a post, written out so
// expected lists stay literal community + rkey pairs.
func frequencyPostURI(communityDID, rkey string) string {
	return "at://" + communityDID + "/social.coves.community.post/" + rkey
}

// timelineFrequencySorts is the sort loop every frequency behavior covers.
var timelineFrequencySorts = []struct {
	sort      string
	timeframe string
}{
	{sort: "hot"},
	{sort: "top", timeframe: "all"},
	{sort: "new"},
}

// readWholeTimeline reads one page large enough for the whole fixture and
// requires that there is no further page.
func readWholeTimeline(t *testing.T, repo timeline.Repository, viewerDID, sort, timeframe string) []string {
	t.Helper()

	feed, cursor, err := repo.GetTimeline(context.Background(), timeline.GetTimelineRequest{
		UserDID:   viewerDID,
		Sort:      sort,
		Timeframe: timeframe,
		Limit:     100,
	})
	require.NoError(t, err)
	require.Nil(t, cursor, "fixture must fit one page")
	return timelineURIs(feed)
}
