package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
)

// HotGenerator is the built-in "hot" feed generator. It ranks exactly what the
// old hot feeds rank — scope home is getTimeline sort=hot (weighted rank,
// level 1-2 frequency thresholds, community blocks applied), scope community
// is getCommunity sort=hot (community blocks not applied) — with the same
// newest-hotCandidateLimit candidate bound, and returns post URIs only; the
// getFeed service hydrates them.
//
// Its cursor is the shared unsigned hot payload created_at::uri::query_time
// (buildCursorPayload / parseCursorPayload); the getFeed envelope signs it.
type HotGenerator struct {
	feedRepoBase
}

// NewHotGenerator returns the hot generator over db. Pass
// WithAccountRestrictionAuthority with the same authority as the old feeds.
func NewHotGenerator(db *sql.DB, options ...AccountRestrictionOption) *HotGenerator {
	return &HotGenerator{feedRepoBase: feedRepoBase{
		db:                 db,
		accountRestriction: newAccountRestriction(options),
	}}
}

// Skeleton returns one page of hot-ranked post URIs for req.Scope. As on the
// old feeds it reads Limit+1 rows, emits at most Limit, and returns a cursor
// only when the lookahead row exists, built from the last emitted post.
func (g *HotGenerator) Skeleton(ctx context.Context, req feedgen.SkeletonRequest) (feedgen.SkeletonPage, error) {
	queryTime := time.Now()

	var query string
	var args []any
	var err error
	switch req.Scope.Name {
	case feedgen.ScopeHome:
		if req.ViewerDID == "" {
			return feedgen.SkeletonPage{}, errors.New("hot skeleton: scope home requires a viewer")
		}
		query, args, err = g.homeQuery(req)
	case feedgen.ScopeCommunity:
		query, args, err = g.communityQuery(req)
	default:
		return feedgen.SkeletonPage{}, fmt.Errorf("hot skeleton: scope %q: %w", req.Scope.Name, feedgen.ErrUnsupportedFeedScope)
	}
	if err != nil {
		return feedgen.SkeletonPage{}, err
	}

	rows, err := g.db.QueryContext(ctx, query, args...)
	if err != nil {
		return feedgen.SkeletonPage{}, fmt.Errorf("hot skeleton: query %s: %w", req.Scope.Name, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Warn("failed to close rows", "error", err)
		}
	}()

	var ranked []posts.PostView
	for rows.Next() {
		var post posts.PostView
		if err := rows.Scan(&post.URI, &post.CreatedAt); err != nil {
			return feedgen.SkeletonPage{}, fmt.Errorf("hot skeleton: scan: %w", err)
		}
		ranked = append(ranked, post)
	}
	if err := rows.Err(); err != nil {
		return feedgen.SkeletonPage{}, fmt.Errorf("hot skeleton: iterate: %w", err)
	}

	var page feedgen.SkeletonPage
	if len(ranked) > req.Limit && req.Limit > 0 {
		ranked = ranked[:req.Limit]
		page.Cursor = g.buildCursorPayload(&ranked[len(ranked)-1], "hot", 0, queryTime)
	}
	page.Items = make([]feedgen.SkeletonItem, 0, len(ranked))
	for _, post := range ranked {
		page.Items = append(page.Items, feedgen.SkeletonItem{URI: post.URI})
	}
	return page, nil
}

// parseHotCursor validates an incoming inner cursor and returns the shared hot
// cursor filter and its values, bound from $3.
func (g *HotGenerator) parseHotCursor(cursor string) (string, []any, error) {
	if cursor == "" {
		return "", nil, nil
	}
	filter, values, err := g.parseCursorPayload(cursor, "hot", 3)
	if err != nil {
		return "", nil, fmt.Errorf("hot skeleton: %w: %w", feedgen.ErrInvalidCursor, err)
	}
	return filter, values, nil
}

// homeQuery is the old getTimeline hot query (oracleTimelineQuery) selecting only the
// post URI and created_at. $1 viewer, $2 limit+1, $3.. cursor, then the
// account-ban authority and the candidate limit.
func (g *HotGenerator) homeQuery(req feedgen.SkeletonRequest) (string, []any, error) {
	cursorFilter, cursorValues, err := g.parseHotCursor(req.Cursor)
	if err != nil {
		return "", nil, err
	}
	// The timeline compares weighted ranks over the shared cursor's values.
	if cursorFilter != "" {
		cursorFilter = timelineHotCursorFilter(3)
	}

	weightedHotRank := timelineWeightedHotRankSQL("p", "NOW()", "cs.content_visibility")
	orderBy := weightedHotRank + ` DESC, p.created_at DESC, p.uri DESC`

	authorityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(1, authorityParam)
	blockFilter := viewerBlockFilters(1, aggregateSurface)
	hotBoundFilter, hotBoundValues := g.hotCandidateBoundFilter(
		"hot",
		`p.community_did IN (SELECT community_did FROM community_subscriptions WHERE user_did = $1)`,
		1, authorityParam, blockFilter, authorityParam+1)

	query := fmt.Sprintf(`
		%s
		SELECT p.uri, p.created_at
		FROM posts p
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did
		INNER JOIN community_subscriptions cs ON p.community_did = cs.community_did%s
		LEFT JOIN frequency_thresholds ft ON ft.community_did = p.community_did
		WHERE cs.user_did = $1
			AND p.deleted_at IS NULL
			AND %s
			%s
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, timelineFrequencyThresholdsCTE(visJoin, visWhere, blockFilter), visJoin, visWhere, blockFilter,
		timelineFrequencyFilter, cursorFilter, hotBoundFilter, orderBy)

	args := []any{req.ViewerDID, req.Limit + 1}
	args = append(args, cursorValues...)
	args = append(args, g.authorityDID)
	args = append(args, hotBoundValues...)
	return query, args, nil
}

// communityQuery is the old getCommunity hot query (oracleCommunityFeedQuery) selecting only
// the post URI and created_at. $1 community, $2 limit+1, $3.. cursor, then the
// viewer ("" for anonymous), the account-ban authority and the candidate limit.
// Community blocks do not apply to a community's own feed (explicitSurface).
func (g *HotGenerator) communityQuery(req feedgen.SkeletonRequest) (string, []any, error) {
	cursorFilter, cursorValues, err := g.parseHotCursor(req.Cursor)
	if err != nil {
		return "", nil, err
	}

	visibilityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(visibilityParam, visibilityParam+1)
	var viewerFilter string
	if req.ViewerDID != "" {
		viewerFilter = viewerBlockFilters(visibilityParam, explicitSurface)
	}
	hotBoundFilter, hotBoundValues := g.hotCandidateBoundFilter(
		"hot", `p.community_did = $1`, visibilityParam, visibilityParam+1, viewerFilter, visibilityParam+2)

	query := fmt.Sprintf(`
		SELECT p.uri, p.created_at
		FROM posts p
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
	`, visJoin, visWhere, cursorFilter, viewerFilter, hotBoundFilter, feedSortClauses["hot"])

	args := []any{req.Scope.CommunityDID, req.Limit + 1}
	args = append(args, cursorValues...)
	args = append(args, req.ViewerDID, g.authorityDID)
	args = append(args, hotBoundValues...)
	return query, args, nil
}
