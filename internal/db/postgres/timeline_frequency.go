package postgres

import "fmt"

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
