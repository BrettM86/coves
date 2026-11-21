package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
)

// SortGenerator is the built-in "new" or "top" feed generator. It ranks
// exactly what the old feeds rank for that sort — scope home is getTimeline
// (level 1-2 frequency thresholds, community blocks applied), scope community
// is getCommunity (community blocks not applied), scope all is getDiscover
// (community blocks applied, instance-removed communities excluded) — with no
// hot candidate bound, and returns post URIs only; the getFeed service hydrates them. Top
// keeps only posts inside the request's timeframe, as the old feeds do.
//
// Its cursor is the shared unsigned payload for its sort (buildCursorPayload /
// parseCursorPayload): created_at::uri for new, score::created_at::uri for
// top. The getFeed envelope signs it.
type SortGenerator struct {
	feedRepoBase
	sort string
}

// NewNewGenerator returns the new generator over db. Pass
// WithAccountRestrictionAuthority with the same authority as the old feeds.
func NewNewGenerator(db *sql.DB, options ...AccountRestrictionOption) *SortGenerator {
	return newSortGenerator(db, "new", options)
}

// NewTopGenerator returns the top generator over db. Pass
// WithAccountRestrictionAuthority with the same authority as the old feeds.
func NewTopGenerator(db *sql.DB, options ...AccountRestrictionOption) *SortGenerator {
	return newSortGenerator(db, "top", options)
}

func newSortGenerator(db *sql.DB, sort string, options []AccountRestrictionOption) *SortGenerator {
	return &SortGenerator{
		feedRepoBase: feedRepoBase{db: db, accountRestriction: newAccountRestriction(options)},
		sort:         sort,
	}
}

// Skeleton returns one page of post URIs for req.Scope in the generator's
// sort. As on the old feeds it reads Limit+1 rows, emits at most Limit, and
// returns a cursor only when the lookahead row exists, built from the last
// emitted post.
func (g *SortGenerator) Skeleton(ctx context.Context, req feedgen.SkeletonRequest) (feedgen.SkeletonPage, error) {
	var query string
	var args []any
	var err error
	switch req.Scope.Name {
	case feedgen.ScopeHome:
		if req.ViewerDID == "" {
			return feedgen.SkeletonPage{}, fmt.Errorf("%s skeleton: scope home requires a viewer", g.sort)
		}
		query, args, err = g.homeQuery(req)
	case feedgen.ScopeCommunity:
		query, args, err = g.communityQuery(req)
	case feedgen.ScopeAll:
		query, args, err = g.allQuery(req)
	default:
		return feedgen.SkeletonPage{}, fmt.Errorf("%s skeleton: scope %q: %w", g.sort, req.Scope.Name, feedgen.ErrUnsupportedFeedScope)
	}
	if err != nil {
		return feedgen.SkeletonPage{}, err
	}

	rows, err := g.db.QueryContext(ctx, query, args...)
	if err != nil {
		return feedgen.SkeletonPage{}, fmt.Errorf("%s skeleton: query %s: %w", g.sort, req.Scope.Name, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Warn("failed to close rows", "error", err)
		}
	}()

	var ranked []posts.PostView
	for rows.Next() {
		post := posts.PostView{Stats: &posts.PostStats{}}
		if err := rows.Scan(&post.URI, &post.CreatedAt, &post.Stats.Score); err != nil {
			return feedgen.SkeletonPage{}, fmt.Errorf("%s skeleton: scan: %w", g.sort, err)
		}
		ranked = append(ranked, post)
	}
	if err := rows.Err(); err != nil {
		return feedgen.SkeletonPage{}, fmt.Errorf("%s skeleton: iterate: %w", g.sort, err)
	}

	var page feedgen.SkeletonPage
	if len(ranked) > req.Limit && req.Limit > 0 {
		ranked = ranked[:req.Limit]
		page.Cursor = g.buildCursorPayload(&ranked[len(ranked)-1], g.sort, 0, time.Time{})
	}
	page.Items = make([]feedgen.SkeletonItem, 0, len(ranked))
	for _, post := range ranked {
		page.Items = append(page.Items, feedgen.SkeletonItem{URI: post.URI})
	}
	return page, nil
}

// parseSortCursor validates an incoming inner cursor and returns the shared
// cursor filter for the generator's sort and its values, bound from $3.
func (g *SortGenerator) parseSortCursor(cursor string) (string, []any, error) {
	if cursor == "" {
		return "", nil, nil
	}
	filter, values, err := g.parseCursorPayload(cursor, g.sort, 3)
	if err != nil {
		return "", nil, fmt.Errorf("%s skeleton: %w: %w", g.sort, feedgen.ErrInvalidCursor, err)
	}
	return filter, values, nil
}

// timeFilter is the old feeds' timeframe filter: top only.
func (g *SortGenerator) timeFilter(timeframe string) string {
	if g.sort != "top" {
		return ""
	}
	return g.buildTimeFilter(timeframe)
}

// homeQuery is the old getTimeline new/top query (oracleTimelineQuery) selecting only
// the post URI, created_at and score. $1 viewer, $2 limit+1, $3.. cursor,
// then the account-ban authority.
func (g *SortGenerator) homeQuery(req feedgen.SkeletonRequest) (string, []any, error) {
	cursorFilter, cursorValues, err := g.parseSortCursor(req.Cursor)
	if err != nil {
		return "", nil, err
	}
	orderBy := feedSortClauses[g.sort]

	authorityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(1, authorityParam)
	blockFilter := viewerBlockFilters(1, aggregateSurface)

	query := fmt.Sprintf(`
		%s
		SELECT p.uri, p.created_at, p.score
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
		timelineFrequencyFilter, g.timeFilter(req.Timeframe), cursorFilter, orderBy)

	args := []any{req.ViewerDID, req.Limit + 1}
	args = append(args, cursorValues...)
	args = append(args, g.authorityDID)
	return query, args, nil
}

// communityQuery is the old getCommunity new/top query (oracleCommunityFeedQuery) selecting
// only the post URI, created_at and score. $1 community, $2 limit+1, $3..
// cursor, then the viewer ("" for anonymous) and the account-ban authority.
// Community blocks do not apply to a community's own feed (explicitSurface).
func (g *SortGenerator) communityQuery(req feedgen.SkeletonRequest) (string, []any, error) {
	cursorFilter, cursorValues, err := g.parseSortCursor(req.Cursor)
	if err != nil {
		return "", nil, err
	}
	orderBy := feedSortClauses[g.sort]

	visibilityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(visibilityParam, visibilityParam+1)
	var viewerFilter string
	if req.ViewerDID != "" {
		viewerFilter = viewerBlockFilters(visibilityParam, explicitSurface)
	}

	query := fmt.Sprintf(`
		SELECT p.uri, p.created_at, p.score
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
	`, visJoin, visWhere, g.timeFilter(req.Timeframe), cursorFilter, viewerFilter, orderBy)

	args := []any{req.Scope.CommunityDID, req.Limit + 1}
	args = append(args, cursorValues...)
	args = append(args, req.ViewerDID, g.authorityDID)
	return query, args, nil
}

// allQuery is the old getDiscover new/top query (oracleDiscoverQuery) selecting only
// the post URI, created_at and score. $1 viewer ("" for anonymous), $2
// limit+1, $3.. cursor, then the account-ban authority. Every community is
// eligible except one the instance removed; the viewer's blocks apply on this
// aggregate surface.
func (g *SortGenerator) allQuery(req feedgen.SkeletonRequest) (string, []any, error) {
	cursorFilter, cursorValues, err := g.parseSortCursor(req.Cursor)
	if err != nil {
		return "", nil, err
	}
	orderBy := feedSortClauses[g.sort]

	authorityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(1, authorityParam)
	var viewerFilter string
	if req.ViewerDID != "" {
		viewerFilter = viewerBlockFilters(1, aggregateSurface)
	}

	query := fmt.Sprintf(`
		SELECT p.uri, p.created_at, p.score
		FROM posts p
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did%s
		WHERE p.deleted_at IS NULL
			AND %s
			AND NOT %s
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, visJoin, visWhere, communityRemovedSQL("p.community_did", fmt.Sprintf("$%d", authorityParam)),
		g.timeFilter(req.Timeframe), cursorFilter, viewerFilter, orderBy)

	args := []any{req.ViewerDID, req.Limit + 1}
	args = append(args, cursorValues...)
	args = append(args, g.authorityDID)
	return query, args, nil
}
