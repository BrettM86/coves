package postgres

import (
	"Coves/internal/core/communityFeeds"
	"context"
	"database/sql"
	"fmt"
	"time"
)

type postgresFeedRepo struct {
	*feedRepoBase
}

// NewCommunityFeedRepository creates a new PostgreSQL feed repository
// Sorting (including the log-damped hot rank) is shared across all post feeds —
// see feedSortClauses and hotRankSQL in feed_repo_base.go
func NewCommunityFeedRepository(db *sql.DB, cursorSecret string) communityFeeds.Repository {
	return &postgresFeedRepo{
		feedRepoBase: newFeedRepoBase(db, cursorSecret),
	}
}

// GetCommunityFeed retrieves posts from a community with sorting and pagination
// Single query with JOINs for optimal performance
func (r *postgresFeedRepo) GetCommunityFeed(ctx context.Context, req communityFeeds.GetCommunityFeedRequest) ([]*communityFeeds.FeedViewPost, *string, error) {
	// Capture query time for stable cursor generation (used for hot sort pagination)
	queryTime := time.Now()

	// Build ORDER BY clause based on sort type
	orderBy, timeFilter := r.feedRepoBase.buildSortClause(req.Sort, req.Timeframe)

	// Build cursor filter for pagination
	// Community feed uses $3+ for cursor params (after $1=community and $2=limit)
	cursorFilter, cursorValues, err := r.feedRepoBase.parseCursor(req.Cursor, req.Sort, 3)
	if err != nil {
		return nil, nil, communityFeeds.ErrInvalidCursor
	}

	// Build the main query (shared column list — see feedPostSelectClause)
	// For hot sort, we need to compute and return the hot_rank for cursor building
	var selectClause string
	if req.Sort == "hot" {
		selectClause = feedPostSelectClause(feedHotRankExpression)
	} else {
		selectClause = feedPostSelectClause("NULL::numeric")
	}

	// The admission visibility gate always runs, so no read path can serve a
	// post its community has not admitted. Its viewer parameter ($visibilityParam)
	// carries the read's viewer DID — empty for the anonymous public, which sees
	// accepted content only.
	visibilityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(visibilityParam)

	// The viewer block filter reuses that same viewer parameter; it is only
	// meaningful for an authenticated viewer and absent for the public.
	//
	// explicitSurface: this is the community's OWN feed, an explicit request for
	// the place, so a community block (an aggregate-feed mute) does not apply
	// here while an author block still does. Pinned by
	// TestCommunityFeedRepo_CommunityBlockIsNotApplied.
	var viewerFilter string
	if req.ViewerDID != "" {
		viewerFilter = viewerBlockFilters(visibilityParam, explicitSurface)
	}

	query := fmt.Sprintf(`
		%s
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did%s
		WHERE p.community_did = $1
			AND p.deleted_at IS NULL
			AND %s
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, selectClause, visJoin, visWhere, timeFilter, cursorFilter, viewerFilter, orderBy)

	// Prepare query arguments. The viewer DID is bound once at $visibilityParam
	// and reused by the block filter above.
	args := []interface{}{req.Community, req.Limit + 1} // +1 to check for next page
	args = append(args, cursorValues...)
	args = append(args, req.ViewerDID)

	// Execute query
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query community feed: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			// Log close errors (non-fatal but worth noting)
			fmt.Printf("Warning: failed to close rows: %v\n", err)
		}
	}()

	// Scan results
	var feedPosts []*communityFeeds.FeedViewPost
	var hotRanks []float64 // Store hot ranks for cursor building
	for rows.Next() {
		postView, hotRank, err := r.feedRepoBase.scanFeedPost(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to scan feed post: %w", err)
		}
		feedPosts = append(feedPosts, &communityFeeds.FeedViewPost{Post: postView})
		hotRanks = append(hotRanks, hotRank)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating feed results: %w", err)
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

// SearchPosts is the social.coves.feed.searchPosts read path. It uses the shared
// admission predicate so search cannot reveal unaccepted posts, and guards empty
// and negation-only queries so they cannot accidentally match the whole corpus.
// Search cursors carry a result-set hash because a signed position is valid only
// for the query, community, sort, and applicable timeframe that minted it.
func (r *postgresFeedRepo) SearchPosts(ctx context.Context, req communityFeeds.SearchPostsRequest) ([]*communityFeeds.FeedViewPost, *string, error) {
	args := make([]interface{}, 0, 7)
	bind := func(value interface{}) int {
		args = append(args, value)
		return len(args)
	}

	queryParam := bind(req.Query)
	// Search reuses feedPostSelectClause's hot_rank slot for relevance rank.
	rankExpression := fmt.Sprintf(
		"ts_rank_cd(p.search_vector, websearch_to_tsquery('english', $%d), 1)",
		queryParam,
	)

	var communityFilter string
	if req.Community != "" {
		communityParam := bind(req.Community)
		communityFilter = fmt.Sprintf("AND p.community_did = $%d", communityParam)
	}

	searchSort := req.Sort
	if searchSort != "new" && searchSort != "top" {
		searchSort = "relevance"
	}
	orderBy, timeFilter := r.feedRepoBase.buildSortClause(searchSort, req.Timeframe)

	cursorFilter, cursorValues, err := r.feedRepoBase.parseSearchCursor(req.Cursor, req, len(args)+1, rankExpression)
	if err != nil {
		return nil, nil, err
	}
	for _, value := range cursorValues {
		bind(value)
	}

	viewerParam := bind(req.ViewerDID)
	visJoin, visWhere := visiblePostsJoin(viewerParam)

	var viewerFilter string
	if req.ViewerDID != "" {
		surface := aggregateSurface
		if req.Community != "" {
			surface = explicitSurface
		}
		viewerFilter = viewerBlockFilters(viewerParam, surface)
	}

	limitParam := bind(req.Limit + 1)
	query := fmt.Sprintf(`
		%s
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did%s
		WHERE p.deleted_at IS NULL
			AND %s
			AND p.search_vector @@ websearch_to_tsquery('english', $%d)
			AND querytree(websearch_to_tsquery('english', $%d)) NOT IN ('', 'T')
			%s
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $%d
	`, feedPostSelectClause(rankExpression), visJoin, visWhere, queryParam, queryParam,
		communityFilter, timeFilter, cursorFilter, viewerFilter, orderBy, limitParam)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query post search: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			fmt.Printf("Warning: failed to close rows: %v\n", err)
		}
	}()

	var feedPosts []*communityFeeds.FeedViewPost
	var ranks []float64
	for rows.Next() {
		postView, rank, err := r.feedRepoBase.scanFeedPost(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to scan post search result: %w", err)
		}
		feedPosts = append(feedPosts, &communityFeeds.FeedViewPost{Post: postView})
		ranks = append(ranks, rank)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating post search results: %w", err)
	}

	var cursor *string
	if len(feedPosts) > req.Limit && req.Limit > 0 {
		feedPosts = feedPosts[:req.Limit]
		ranks = ranks[:req.Limit]
		lastPost := feedPosts[len(feedPosts)-1].Post
		lastRank := ranks[len(ranks)-1]

		cursorString := r.feedRepoBase.buildSearchCursor(req, lastPost, lastRank)
		cursor = &cursorString
	}

	return feedPosts, cursor, nil
}
