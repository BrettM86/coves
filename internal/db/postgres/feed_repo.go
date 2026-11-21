package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
)

type postgresFeedRepo struct {
	*feedRepoBase
}

// NewCommunityFeedRepository creates a new PostgreSQL feed repository
// Sorting (including the log-damped hot rank) is shared across all post feeds —
// see feedSortClauses and hotRankSQL in feed_repo_base.go. Posts of accounts
// restricted by the WithAccountRestrictionAuthority option's instance are
// hidden from every viewer but their author.
func NewCommunityFeedRepository(db *sql.DB, cursorSecret string, options ...AccountRestrictionOption) communityFeeds.Repository {
	return &postgresFeedRepo{
		feedRepoBase: newFeedRepoBase(db, cursorSecret, options),
	}
}

// SearchPosts is the social.coves.feed.searchPosts read path. It uses the shared
// admission predicate so search cannot reveal unaccepted posts, and guards empty
// and negation-only queries so they cannot accidentally match the whole corpus.
// A rank query applies every filter and the ordering and selects only the
// columns ordering and the cursor need; the page's posts are then hydrated by
// the shared PostHydrator (scope all, or scope community when the search is
// limited to one), which returns them in rank order.
// Search cursors carry a result-set hash because a signed position is valid only
// for the query, community, sort, and applicable timeframe that minted it.
func (r *postgresFeedRepo) SearchPosts(ctx context.Context, req communityFeeds.SearchPostsRequest) ([]*communityFeeds.FeedViewPost, *string, error) {
	args := make([]interface{}, 0, 7)
	bind := func(value interface{}) int {
		args = append(args, value)
		return len(args)
	}

	queryParam := bind(req.Query)
	// The relevance rank is selected as hot_rank, the alias the relevance
	// ORDER BY in feedSortClauses reads.
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

	scope := feedgen.Scope{Name: feedgen.ScopeAll}
	surface := aggregateSurface
	if req.Community != "" {
		scope = feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: req.Community}
		surface = explicitSurface
	}
	var viewerFilter string
	if req.ViewerDID != "" {
		viewerFilter = viewerBlockFilters(viewerParam, surface)
	}

	limitParam := bind(req.Limit + 1)
	query := fmt.Sprintf(`
		SELECT p.uri, p.created_at, p.score, %s AS hot_rank
		FROM posts p
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
	`, rankExpression, visJoin, visWhere, queryParam, queryParam,
		communityFilter, timeFilter, cursorFilter, viewerFilter, orderBy, limitParam)

	// The rank query and the hydration read one snapshot, so a post that
	// becomes invisible between them cannot shorten (or empty) the page.
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to begin post search transaction: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.Warn("failed to roll back post search transaction", "error", err)
		}
	}()

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query post search: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Warn("failed to close post search rank rows", "error", err)
		}
	}()

	type searchRankRow struct {
		uri       string
		createdAt time.Time
		score     int
		rank      float64
	}
	var ranked []searchRankRow
	for rows.Next() {
		var row searchRankRow
		var rank sql.NullFloat64
		if err := rows.Scan(&row.uri, &row.createdAt, &row.score, &rank); err != nil {
			return nil, nil, fmt.Errorf("failed to scan post search rank: %w", err)
		}
		row.rank = rank.Float64
		ranked = append(ranked, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating post search results: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("failed to close post search rank rows: %w", err)
	}
	if len(ranked) == 0 {
		return nil, nil, nil
	}

	var cursor *string
	if len(ranked) > req.Limit && req.Limit > 0 {
		ranked = ranked[:req.Limit]
		last := ranked[len(ranked)-1]
		cursorString := r.buildSearchCursor(req, &posts.PostView{
			URI:       last.uri,
			CreatedAt: last.createdAt,
			Stats:     &posts.PostStats{Score: last.score},
		}, last.rank)
		cursor = &cursorString
	}

	uris := make([]string, len(ranked))
	for i, row := range ranked {
		uris[i] = row.uri
	}
	hydrator := NewPostHydrator(tx, WithAccountRestrictionAuthority(r.authorityDID))
	views, err := hydrator.HydratePosts(ctx, feedgen.HydrateRequest{
		ViewerDID: req.ViewerDID,
		Scope:     scope,
		URIs:      uris,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to hydrate post search results: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("failed to commit post search transaction: %w", err)
	}

	feedPosts := make([]*communityFeeds.FeedViewPost, len(views))
	for i, view := range views {
		feedPosts[i] = &communityFeeds.FeedViewPost{Post: view}
	}
	return feedPosts, cursor, nil
}
