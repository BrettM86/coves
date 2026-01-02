package postgres

import (
	"Coves/internal/core/timeline"
	"context"
	"database/sql"
	"fmt"
	"time"
)

type postgresTimelineRepo struct {
	*feedRepoBase
}

// sortClauses maps sort types to safe SQL ORDER BY clauses
// This whitelist prevents SQL injection via dynamic ORDER BY construction
// Note: Hot ranking uses (score + 1) to ensure new posts with 0 votes still appear
var timelineSortClauses = map[string]string{
	"hot": `((p.score + 1) / POWER(EXTRACT(EPOCH FROM (NOW() - p.created_at))/3600 + 2, 1.5)) DESC, p.created_at DESC, p.uri DESC`,
	"top": `p.score DESC, p.created_at DESC, p.uri DESC`,
	"new": `p.created_at DESC, p.uri DESC`,
}

// hotRankExpression is the SQL expression for computing the hot rank
// NOTE: Uses NOW() which means hot_rank changes over time - this is expected behavior
// Uses (score + 1) so new posts with 0 votes still get a positive rank
const timelineHotRankExpression = `((p.score + 1) / POWER(EXTRACT(EPOCH FROM (NOW() - p.created_at))/3600 + 2, 1.5))`

// NewTimelineRepository creates a new PostgreSQL timeline repository
func NewTimelineRepository(db *sql.DB, cursorSecret string) timeline.Repository {
	return &postgresTimelineRepo{
		feedRepoBase: newFeedRepoBase(db, timelineHotRankExpression, timelineSortClauses, cursorSecret),
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
	cursorFilter, cursorValues, err := r.feedRepoBase.parseCursor(req.Cursor, req.Sort, 3)
	if err != nil {
		return nil, nil, timeline.ErrInvalidCursor
	}

	// Build the main query
	// For hot sort, we need to compute and return the hot_rank for cursor building
	var selectClause string
	if req.Sort == "hot" {
		selectClause = fmt.Sprintf(`
		SELECT
			p.uri, p.cid, p.rkey,
			p.author_did, u.handle as author_handle,
			p.community_did, c.handle as community_handle, c.name as community_name, c.avatar_cid as community_avatar, c.pds_url as community_pds_url,
			p.title, p.content, p.content_facets, p.embed, p.content_labels,
			p.created_at, p.edited_at, p.indexed_at,
			p.upvote_count, p.downvote_count, p.score, p.comment_count,
			%s as hot_rank
		FROM posts p`, timelineHotRankExpression)
	} else {
		selectClause = `
		SELECT
			p.uri, p.cid, p.rkey,
			p.author_did, u.handle as author_handle,
			p.community_did, c.handle as community_handle, c.name as community_name, c.avatar_cid as community_avatar, c.pds_url as community_pds_url,
			p.title, p.content, p.content_facets, p.embed, p.content_labels,
			p.created_at, p.edited_at, p.indexed_at,
			p.upvote_count, p.downvote_count, p.score, p.comment_count,
			NULL::numeric as hot_rank
		FROM posts p`
	}

	// Join with community_subscriptions to get posts from subscribed communities
	query := fmt.Sprintf(`
		%s
		INNER JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did
		INNER JOIN community_subscriptions cs ON p.community_did = cs.community_did
		WHERE cs.user_did = $1
			AND p.deleted_at IS NULL
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, selectClause, timeFilter, cursorFilter, orderBy)

	// Prepare query arguments
	args := []interface{}{req.UserDID, req.Limit + 1} // +1 to check for next page
	args = append(args, cursorValues...)

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
		postView, hotRank, err := r.feedRepoBase.scanFeedPost(rows)
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
		cursorStr := r.feedRepoBase.buildCursor(lastPost, req.Sort, lastHotRank, queryTime)
		cursor = &cursorStr
	}

	return feedPosts, cursor, nil
}
