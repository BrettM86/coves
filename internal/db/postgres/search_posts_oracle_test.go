//go:build integration

package postgres

import (
	"context"
	"fmt"

	"Coves/internal/core/communityFeeds"
)

// The searchPosts oracle: a verbatim copy of (*postgresFeedRepo).SearchPosts
// (feed_repo.go) as it stood at 6cab19ce, when one query both selected and
// hydrated the matching posts. SearchPosts is being split into a rank query
// plus the shared PostHydrator; the equivalence test
// (search_posts_equivalence_test.go) compares it against this copy, page for
// page and cursor for cursor.
//
// It reads through a feedRepoBase built by newFeedOracle with the same cursor
// secret and account-restriction option as the repository under test, and it
// calls only helpers that stay in production code (buildSortClause,
// parseSearchCursor, buildSearchCursor, visiblePostsJoin, viewerBlockFilters)
// plus feedPostSelectClause and scanFeedPost, which live in feed_oracle_test.go.
// Do not "fix" it: a change here changes what
// equivalence means.
func oracleSearchPosts(ctx context.Context, r *feedRepoBase, req communityFeeds.SearchPostsRequest) ([]*communityFeeds.FeedViewPost, *string, error) {
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
	orderBy, timeFilter := r.buildSortClause(searchSort, req.Timeframe)

	cursorFilter, cursorValues, err := r.parseSearchCursor(req.Cursor, req, len(args)+1, rankExpression)
	if err != nil {
		return nil, nil, err
	}
	for _, value := range cursorValues {
		bind(value)
	}

	viewerParam := bind(req.ViewerDID)
	visJoin, visWhere := visiblePostsJoin(viewerParam, bind(r.authorityDID))

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
		postView, rank, err := r.scanFeedPost(rows)
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

		cursorString := r.buildSearchCursor(req, lastPost, lastRank)
		cursor = &cursorString
	}

	return feedPosts, cursor, nil
}
