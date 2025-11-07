package postgres

import (
	"Coves/internal/core/discover"
	"context"
	"database/sql"
	"fmt"
)

type postgresDiscoverRepo struct {
	*feedRepoBase
}

// sortClauses maps sort types to safe SQL ORDER BY clauses
var discoverSortClauses = map[string]string{
	"hot": `(p.score / POWER(EXTRACT(EPOCH FROM (NOW() - p.created_at))/3600 + 2, 1.5)) DESC, p.created_at DESC, p.uri DESC`,
	"top": `p.score DESC, p.created_at DESC, p.uri DESC`,
	"new": `p.created_at DESC, p.uri DESC`,
}

// hotRankExpression for discover feed
const discoverHotRankExpression = `(p.score / POWER(EXTRACT(EPOCH FROM (NOW() - p.created_at))/3600 + 2, 1.5))`

// NewDiscoverRepository creates a new PostgreSQL discover repository
func NewDiscoverRepository(db *sql.DB, cursorSecret string) discover.Repository {
	return &postgresDiscoverRepo{
		feedRepoBase: newFeedRepoBase(db, discoverHotRankExpression, discoverSortClauses, cursorSecret),
	}
}

// GetDiscover retrieves posts from ALL communities (public feed)
func (r *postgresDiscoverRepo) GetDiscover(ctx context.Context, req discover.GetDiscoverRequest) ([]*discover.FeedViewPost, *string, error) {
	// Build ORDER BY clause based on sort type
	orderBy, timeFilter := r.buildSortClause(req.Sort, req.Timeframe)

	// Build cursor filter for pagination
	// Discover uses $2+ for cursor params (after $1=limit)
	cursorFilter, cursorValues, err := r.feedRepoBase.parseCursor(req.Cursor, req.Sort, 2)
	if err != nil {
		return nil, nil, discover.ErrInvalidCursor
	}

	// Build the main query
	var selectClause string
	if req.Sort == "hot" {
		selectClause = fmt.Sprintf(`
		SELECT
			p.uri, p.cid, p.rkey,
			p.author_did, u.handle as author_handle,
			p.community_did, c.handle as community_handle, c.name as community_name, c.avatar_cid as community_avatar,
			p.title, p.content, p.content_facets, p.embed, p.content_labels,
			p.created_at, p.edited_at, p.indexed_at,
			p.upvote_count, p.downvote_count, p.score, p.comment_count,
			%s as hot_rank
		FROM posts p`, discoverHotRankExpression)
	} else {
		selectClause = `
		SELECT
			p.uri, p.cid, p.rkey,
			p.author_did, u.handle as author_handle,
			p.community_did, c.handle as community_handle, c.name as community_name, c.avatar_cid as community_avatar,
			p.title, p.content, p.content_facets, p.embed, p.content_labels,
			p.created_at, p.edited_at, p.indexed_at,
			p.upvote_count, p.downvote_count, p.score, p.comment_count,
			NULL::numeric as hot_rank
		FROM posts p`
	}

	// No subscription filter - show ALL posts from ALL communities
	query := fmt.Sprintf(`
		%s
		INNER JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did
		WHERE p.deleted_at IS NULL
			%s
			%s
		ORDER BY %s
		LIMIT $1
	`, selectClause, timeFilter, cursorFilter, orderBy)

	// Prepare query arguments
	args := []interface{}{req.Limit + 1} // +1 to check for next page
	args = append(args, cursorValues...)

	// Execute query
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query discover feed: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			fmt.Printf("Warning: failed to close rows: %v\n", err)
		}
	}()

	// Scan results
	var feedPosts []*discover.FeedViewPost
	var hotRanks []float64
	for rows.Next() {
		postView, hotRank, err := r.feedRepoBase.scanFeedPost(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to scan discover post: %w", err)
		}
		feedPosts = append(feedPosts, &discover.FeedViewPost{Post: postView})
		hotRanks = append(hotRanks, hotRank)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating discover results: %w", err)
	}

	// Handle pagination cursor
	var cursor *string
	if len(feedPosts) > req.Limit && req.Limit > 0 {
		feedPosts = feedPosts[:req.Limit]
		hotRanks = hotRanks[:req.Limit]
		lastPost := feedPosts[len(feedPosts)-1].Post
		lastHotRank := hotRanks[len(hotRanks)-1]
		cursorStr := r.feedRepoBase.buildCursor(lastPost, req.Sort, lastHotRank)
		cursor = &cursorStr
	}

	return feedPosts, cursor, nil
}
