package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"Coves/internal/core/timeline"
)

type postgresTimelineRepo struct {
	*feedRepoBase
}

// NewTimelineRepository creates a new PostgreSQL timeline repository
// Sorting (including the log-damped hot rank) is shared across all post feeds —
// see feedSortClauses and hotRankSQL in feed_repo_base.go
func NewTimelineRepository(db *sql.DB, cursorSecret string, options ...AccountRestrictionOption) timeline.Repository {
	return &postgresTimelineRepo{
		feedRepoBase: newFeedRepoBase(db, cursorSecret, options),
	}
}

// GetTimeline retrieves posts from all communities the user subscribes to
// Single query with JOINs for optimal performance
func (r *postgresTimelineRepo) GetTimeline(ctx context.Context, req timeline.GetTimelineRequest) ([]*timeline.FeedViewPost, *string, error) {
	// Capture query time for stable cursor generation (used for hot sort pagination)
	queryTime := time.Now()

	// Build ORDER BY clause based on sort type
	orderBy, timeFilter := r.buildSortClause(req.Sort, req.Timeframe)

	// Build cursor filter for pagination
	// Timeline uses $3+ for cursor params (after $1=userDID and $2=limit)
	cursorFilter, cursorValues, err := r.parseCursor(req.Cursor, req.Sort, 3)
	if err != nil {
		return nil, nil, timeline.ErrInvalidCursor
	}
	// parseCursor validated the hot cursor and returned its values; the
	// timeline compares weighted ranks, so it renders its own filter over them.
	if req.Sort == "hot" && cursorFilter != "" {
		cursorFilter = timelineHotCursorFilter(3)
	}

	// Build the main query (shared column list — see feedPostSelectClause)
	// For hot sort, we need to compute and return the hot_rank for cursor building
	var selectClause string
	if req.Sort == "hot" {
		weightedHotRank := timelineWeightedHotRankSQL("p", "NOW()", "cs.content_visibility")
		selectClause = feedPostSelectClause(weightedHotRank)
		orderBy = weightedHotRank + ` DESC, p.created_at DESC, p.uri DESC`
	} else {
		selectClause = feedPostSelectClause("NULL::numeric")
	}

	// The admission visibility gate reuses $1 (the subscriber's DID) as the
	// viewer — the same intentional reuse the block filter below makes — so a
	// pending post reaching the home feed is impossible, while the subscriber
	// still sees their own non-accepted posts in communities they subscribe to.
	// The account-ban authority is bound after the cursor values, and the hot
	// candidate limit after it.
	authorityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(1, authorityParam)
	blockFilter := viewerBlockFilters(1, aggregateSurface)

	hotBoundLimitParam := authorityParam + 1
	hotBoundFilter, hotBoundValues := r.hotCandidateBoundFilter(
		req.Sort,
		`p.community_did IN (SELECT community_did FROM community_subscriptions WHERE user_did = $1)`,
		1, authorityParam, blockFilter, hotBoundLimitParam)

	// Join with community_subscriptions to get posts from subscribed communities
	query := fmt.Sprintf(`
		%s
		%s
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did
		INNER JOIN community_subscriptions cs ON p.community_did = cs.community_did%s
		LEFT JOIN frequency_thresholds ft ON ft.community_did = p.community_did
		WHERE cs.user_did = $1
			AND p.deleted_at IS NULL
			AND %s
			-- Intentional $1 reuse: the viewer's DID (cs.user_did) is also the blocker for block filtering
			%s
			%s
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, timelineFrequencyThresholdsCTE(visJoin, visWhere, blockFilter), selectClause, visJoin, visWhere, blockFilter, timelineFrequencyFilter, timeFilter, cursorFilter, hotBoundFilter, orderBy)

	// Prepare query arguments
	args := []interface{}{req.UserDID, req.Limit + 1} // +1 to check for next page
	args = append(args, cursorValues...)
	args = append(args, r.authorityDID)
	args = append(args, hotBoundValues...)

	// Execute query
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query timeline: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			// Log close errors (non-fatal but worth noting)
			fmt.Printf("Warning: failed to close rows: %v\n", err)
		}
	}()

	// Scan results
	var feedPosts []*timeline.FeedViewPost
	var hotRanks []float64 // Store hot ranks for cursor building
	for rows.Next() {
		postView, hotRank, err := r.scanFeedPost(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to scan timeline post: %w", err)
		}
		feedPosts = append(feedPosts, &timeline.FeedViewPost{Post: postView})
		hotRanks = append(hotRanks, hotRank)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating timeline results: %w", err)
	}

	// Handle pagination cursor
	var cursor *string
	if len(feedPosts) > req.Limit && req.Limit > 0 {
		feedPosts = feedPosts[:req.Limit]
		hotRanks = hotRanks[:req.Limit]
		lastPost := feedPosts[len(feedPosts)-1].Post
		lastHotRank := hotRanks[len(hotRanks)-1]
		cursorStr := r.buildCursor(lastPost, req.Sort, lastHotRank, queryTime)
		cursor = &cursorStr
	}

	return feedPosts, cursor, nil
}

// Subscription frequency (community_subscriptions.content_visibility, 1..5,
// default 3) as the home timeline reads it. Levels 1 and 2 keep only a
// community's top 10% / 25% of posts by score; levels 4 and 5 boost the Hot
// rank. Level 3 is today's feed. Discover and community feeds ignore it.
const (
	timelineNeutralHotMultiplier   = 1.0
	timelineLevelFourHotMultiplier = 1.5
	timelineLevelFiveHotMultiplier = 2.0

	timelineLevelOneThresholdFraction = 0.10
	timelineLevelTwoThresholdFraction = 0.25

	// timelineThresholdWindow is the fixed span of recent posts a level-1/2
	// community's score line is computed from, whatever the sort or timeframe.
	timelineThresholdWindow = `INTERVAL '14 days'`
)

// timelineHotMultiplier is the Hot rank multiplier for a subscription level.
// Levels 1-3, and anything outside 4-5, are neutral.
func timelineHotMultiplier(contentVisibility int) float64 {
	switch contentVisibility {
	case 4:
		return timelineLevelFourHotMultiplier
	case 5:
		return timelineLevelFiveHotMultiplier
	default:
		return timelineNeutralHotMultiplier
	}
}

// timelineWeightedHotRankSQL renders the timeline's Hot rank: hotRankSQL scaled
// by the subscription level's multiplier m so that a boost always moves a post
// up: rank * m when the rank is non-negative, rank / m when it is negative.
// contentVisibilityExpr is the SQL for the level; NULL (no subscription row)
// takes the neutral multiplier. The live ORDER BY and both sides of the Hot
// cursor comparison use this one builder.
func timelineWeightedHotRankSQL(alias, nowExpr, contentVisibilityExpr string) string {
	rank := hotRankSQL(alias, nowExpr)
	multiplier := fmt.Sprintf(`(CASE %s WHEN 4 THEN %v::float8 WHEN 5 THEN %v::float8 ELSE %v::float8 END)`,
		contentVisibilityExpr,
		timelineHotMultiplier(4), timelineHotMultiplier(5), timelineNeutralHotMultiplier)
	return fmt.Sprintf(`(CASE WHEN %[1]s >= 0 THEN %[1]s * %[2]s ELSE %[1]s / %[2]s END)`, rank, multiplier)
}

// timelineFrequencyThresholdsCTE computes, for the viewer's ($1) level-1 and
// level-2 subscriptions only, each community's score line: the score of the
// post at rank ceil(N * fraction) in descending score order, over the N posts
// created within timelineThresholdWindow that pass the outer query's own
// visibility gate (visJoin, visWhere, blockFilter). A community with no such
// posts gets no row, so it contributes nothing at levels 1-2.
func timelineFrequencyThresholdsCTE(visJoin, visWhere, blockFilter string) string {
	return fmt.Sprintf(`WITH frequency_thresholds AS (
			SELECT ranked.community_did, ranked.score AS threshold
			FROM (
				SELECT p.community_did, p.score, fs.content_visibility,
					ROW_NUMBER() OVER (PARTITION BY p.community_did ORDER BY p.score DESC) AS position,
					COUNT(*) OVER (PARTITION BY p.community_did) AS window_count
				FROM community_subscriptions fs
				INNER JOIN posts p ON p.community_did = fs.community_did%s
				WHERE fs.user_did = $1
					AND fs.content_visibility IN (1, 2)
					AND p.deleted_at IS NULL
					AND p.created_at >= NOW() - %s
					AND %s
					%s
			) ranked
			WHERE ranked.position = CEIL(ranked.window_count *
				CASE ranked.content_visibility WHEN 1 THEN %v ELSE %v END)
		)`, visJoin, timelineThresholdWindow, visWhere, blockFilter,
		timelineLevelOneThresholdFraction, timelineLevelTwoThresholdFraction)
}

// timelineFrequencyFilter keeps every post at level 3 and above, and at levels
// 1-2 only a positive-score post at or above its community's threshold.
const timelineFrequencyFilter = `AND (cs.content_visibility >= 3 OR (p.score > 0 AND p.score >= ft.threshold))`

// timelineHotCursorFilter is the timeline's Hot cursor comparison: the shared
// one (parseCursorPayload) with both sides weighted. Its parameters are the
// shared hot cursor's values in the same order ($paramOffset created_at,
// +1 uri, +2 cursor timestamp). The cursor post's multiplier comes from the
// viewer's ($1) current subscription to its community; with no row (the viewer
// unsubscribed between pages) it is neutral.
func timelineHotCursorFilter(paramOffset int) string {
	cursorTimestampParam := fmt.Sprintf("$%d::timestamptz", paramOffset+2)
	stableRank := timelineWeightedHotRankSQL("p", cursorTimestampParam, "cs.content_visibility")
	cursorRank := fmt.Sprintf(`(SELECT %s FROM posts cursor_post
				LEFT JOIN community_subscriptions cursor_cs
					ON cursor_cs.community_did = cursor_post.community_did AND cursor_cs.user_did = $1
				WHERE cursor_post.uri = $%d)`,
		timelineWeightedHotRankSQL("cursor_post", cursorTimestampParam, "cursor_cs.content_visibility"), paramOffset+1)
	return fmt.Sprintf(`AND (
			%[1]s < %[2]s
			OR (%[1]s = %[2]s AND p.created_at < $%[3]d)
			OR (%[1]s = %[2]s AND p.created_at = $%[3]d AND p.uri < $%[4]d)
		)`, stableRank, cursorRank, paramOffset, paramOffset+1)
}
