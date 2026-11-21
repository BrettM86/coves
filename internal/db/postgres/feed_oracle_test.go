//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/require"
)

// The post feed oracle: a verbatim copy of the SQL, cursor handling and row
// scan of the three repository reads the getFeed aliases replace, as they
// stood at 45e8da33:
//
//   - GetTimeline (timeline_repo.go): hot (weighted rank, level 1-2 frequency
//     thresholds, hot candidate bound), new and top with its timeframe;
//   - GetCommunityFeed (feed_repo.go): hot (candidate bound), new, top;
//   - GetDiscover New/Top (discover_repo.go). Discover Hot is not here: it
//     stays in production as the discover-hot generator.
//
// It is the reference the equivalence and cost tests compare getFeed against,
// so it must keep compiling and behaving exactly the same after those methods
// are deleted. Apart from the signed-cursor wrappers parseCursor and
// buildCursor and the row helpers feedPostSelectClause and scanFeedPost below
// (also used by the searchPosts oracle), which only tests use, it calls
// only helpers that stay in production code: the feedRepoBase builders
// (newFeedRepoBase, buildSortClause, verifyCursorPayload, parseCursorPayload,
// signCursorPayload, buildCursorPayload, hotCandidateBoundFilter),
// feedHotRankExpression,
// visiblePostsJoin, viewerBlockFilters, communityRemovedSQL and the timeline
// frequency helpers
// (timelineWeightedHotRankSQL, timelineFrequencyThresholdsCTE,
// timelineFrequencyFilter, timelineHotCursorFilter). Everything that lived
// only inside the three methods is copied here. Do not "fix" it: a change here
// changes what equivalence means.
//
// The request is what the old repository received, after its service had
// validated and defaulted it (sort "" -> hot, limit <= 0 -> 15, top without a
// timeframe -> day; getCommunity's identifier already resolved to a DID).

// oracleFeedSurface names one of the old feed reads.
type oracleFeedSurface string

const (
	oracleTimeline  oracleFeedSurface = "timeline"
	oracleCommunity oracleFeedSurface = "community"
	oracleDiscover  oracleFeedSurface = "discover"
)

// oracleFeedRequest mirrors the old repository request fields. ViewerDID is
// the timeline's subscriber (required), or the community feed's and
// Discover's viewer ("" for anonymous). Community is the community feed's
// community DID.
type oracleFeedRequest struct {
	ViewerDID string
	Community string
	Sort      string
	Timeframe string
	Limit     int
	Cursor    string
}

// newFeedOracle returns the shared base the oracle reads through, configured
// as the old repositories were: the cursor secret and the account-restriction
// option. Lower its hotCandidateLimit to match a lowered generator bound.
func newFeedOracle(db *sql.DB, cursorSecret string, options ...AccountRestrictionOption) *feedRepoBase {
	return newFeedRepoBase(db, cursorSecret, options)
}

// errOracleInvalidCursor is what the old repositories returned for a cursor
// they rejected (timeline/communityFeeds/discover ErrInvalidCursor).
var errOracleInvalidCursor = fmt.Errorf("oracle: %w", feedgen.ErrInvalidCursor)

// parseCursor decodes and validates the old repositories' signed pagination
// cursor. paramOffset is the starting parameter number for cursor values
// ($2 for discover, $3 for timeline and community feed).
func (r *feedRepoBase) parseCursor(cursor *string, sort string, paramOffset int) (string, []interface{}, error) {
	if cursor == nil || *cursor == "" {
		return "", nil, nil
	}

	payload, err := r.verifyCursorPayload(*cursor)
	if err != nil {
		return "", nil, err
	}
	return r.parseCursorPayload(payload, sort, paramOffset)
}

// buildCursor creates the old repositories' HMAC-signed pagination cursor from
// the last post. queryTime is when the query ran, used for a stable hot_rank
// comparison.
func (r *feedRepoBase) buildCursor(post *posts.PostView, sort string, hotRank float64, queryTime time.Time) string {
	return r.signCursorPayload(r.buildCursorPayload(post, sort, hotRank, queryTime))
}

// oracleCursor is the old repositories' *string cursor.
func oracleCursor(cursor string) *string {
	if cursor == "" {
		return nil
	}
	return &cursor
}

// oracleTimelineQuery builds the SQL and arguments of today's GetTimeline.
func oracleTimelineQuery(r *feedRepoBase, req oracleFeedRequest) (string, []any, error) {
	// Build ORDER BY clause based on sort type
	orderBy, timeFilter := r.buildSortClause(req.Sort, req.Timeframe)

	// Build cursor filter for pagination
	// Timeline uses $3+ for cursor params (after $1=userDID and $2=limit)
	cursorFilter, cursorValues, err := r.parseCursor(oracleCursor(req.Cursor), req.Sort, 3)
	if err != nil {
		return "", nil, errOracleInvalidCursor
	}
	// parseCursor validated the hot cursor and returned its values; the
	// timeline compares weighted ranks, so it renders its own filter over them.
	if req.Sort == "hot" && cursorFilter != "" {
		cursorFilter = timelineHotCursorFilter(3)
	}

	var selectClause string
	if req.Sort == "hot" {
		weightedHotRank := timelineWeightedHotRankSQL("p", "NOW()", "cs.content_visibility")
		selectClause = feedPostSelectClause(weightedHotRank)
		orderBy = weightedHotRank + ` DESC, p.created_at DESC, p.uri DESC`
	} else {
		selectClause = feedPostSelectClause("NULL::numeric")
	}

	authorityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(1, authorityParam)
	blockFilter := viewerBlockFilters(1, aggregateSurface)

	hotBoundLimitParam := authorityParam + 1
	hotBoundFilter, hotBoundValues := r.hotCandidateBoundFilter(
		req.Sort,
		`p.community_did IN (SELECT community_did FROM community_subscriptions WHERE user_did = $1)`,
		1, authorityParam, blockFilter, hotBoundLimitParam)

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

	args := []any{req.ViewerDID, req.Limit + 1} // +1 to check for next page
	args = append(args, cursorValues...)
	args = append(args, r.authorityDID)
	args = append(args, hotBoundValues...)
	return query, args, nil
}

// oracleCommunityFeedQuery builds the SQL and arguments of today's
// GetCommunityFeed.
func oracleCommunityFeedQuery(r *feedRepoBase, req oracleFeedRequest) (string, []any, error) {
	orderBy, timeFilter := r.buildSortClause(req.Sort, req.Timeframe)

	// Community feed uses $3+ for cursor params (after $1=community and $2=limit)
	cursorFilter, cursorValues, err := r.parseCursor(oracleCursor(req.Cursor), req.Sort, 3)
	if err != nil {
		return "", nil, errOracleInvalidCursor
	}

	var selectClause string
	if req.Sort == "hot" {
		selectClause = feedPostSelectClause(feedHotRankExpression)
	} else {
		selectClause = feedPostSelectClause("NULL::numeric")
	}

	visibilityParam := 3 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(visibilityParam, visibilityParam+1)

	// explicitSurface: the community's own feed, so a community block does not
	// apply while an author block still does.
	var viewerFilter string
	if req.ViewerDID != "" {
		viewerFilter = viewerBlockFilters(visibilityParam, explicitSurface)
	}

	hotBoundFilter, hotBoundValues := r.hotCandidateBoundFilter(
		req.Sort, `p.community_did = $1`, visibilityParam, visibilityParam+1, viewerFilter, visibilityParam+2)

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
			%s
		ORDER BY %s
		LIMIT $2
	`, selectClause, visJoin, visWhere, timeFilter, cursorFilter, viewerFilter, hotBoundFilter, orderBy)

	args := []any{req.Community, req.Limit + 1} // +1 to check for next page
	args = append(args, cursorValues...)
	args = append(args, req.ViewerDID, r.authorityDID)
	args = append(args, hotBoundValues...)
	return query, args, nil
}

// oracleDiscoverQuery builds the SQL and arguments of today's GetDiscover for
// sort new or top. Sort hot is the discover-hot generator, not this oracle.
func oracleDiscoverQuery(r *feedRepoBase, req oracleFeedRequest) (string, []any, error) {
	if req.Sort == "hot" {
		return "", nil, fmt.Errorf("oracle: Discover Hot is the discover-hot generator, not an oracle query")
	}

	orderBy, timeFilter := r.buildSortClause(req.Sort, req.Timeframe)

	// Discover uses $2+ for cursor params (after $1=limit)
	cursorFilter, cursorValues, err := r.parseCursor(oracleCursor(req.Cursor), req.Sort, 2)
	if err != nil {
		return "", nil, errOracleInvalidCursor
	}

	var selectClause string
	if req.Sort == "hot" {
		selectClause = feedPostSelectClause(feedHotRankExpression)
	} else {
		selectClause = feedPostSelectClause("NULL::numeric")
	}

	visibilityParam := 2 + len(cursorValues)
	visJoin, visWhere := visiblePostsJoin(visibilityParam, visibilityParam+1)

	var viewerFilter string
	if req.ViewerDID != "" {
		viewerFilter = viewerBlockFilters(visibilityParam, aggregateSurface)
	}

	query := fmt.Sprintf(`
		%s
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did%s
		WHERE p.deleted_at IS NULL
			AND %s
			AND NOT %s
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $1
	`, selectClause, visJoin, visWhere, communityRemovedSQL("p.community_did", fmt.Sprintf("$%d", visibilityParam+1)),
		timeFilter, cursorFilter, viewerFilter, orderBy)

	args := []any{req.Limit + 1} // +1 to check for next page
	args = append(args, cursorValues...)
	args = append(args, req.ViewerDID, r.authorityDID)
	return query, args, nil
}

// oracleFeedQuery dispatches to the surface's query builder.
func oracleFeedQuery(r *feedRepoBase, surface oracleFeedSurface, req oracleFeedRequest) (string, []any, error) {
	switch surface {
	case oracleTimeline:
		return oracleTimelineQuery(r, req)
	case oracleCommunity:
		return oracleCommunityFeedQuery(r, req)
	case oracleDiscover:
		return oracleDiscoverQuery(r, req)
	default:
		return "", nil, fmt.Errorf("oracle: unknown surface %q", surface)
	}
}

// oracleFeedPage runs one page of the old read: the query, the row scan and
// the signed next cursor (built from the last emitted post only when the
// lookahead row exists; "" at the end), exactly as the three methods did.
func oracleFeedPage(ctx context.Context, r *feedRepoBase, surface oracleFeedSurface, req oracleFeedRequest) ([]*posts.PostView, string, error) {
	// Capture query time for stable cursor generation (used for hot sort pagination)
	queryTime := time.Now()

	query, args, err := oracleFeedQuery(r, surface, req)
	if err != nil {
		return nil, "", err
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("oracle: query %s: %w", surface, err)
	}
	defer func() { _ = rows.Close() }()

	var feedPosts []*posts.PostView
	var hotRanks []float64
	for rows.Next() {
		postView, hotRank, err := r.scanFeedPost(rows)
		if err != nil {
			return nil, "", fmt.Errorf("oracle: scan %s: %w", surface, err)
		}
		feedPosts = append(feedPosts, postView)
		hotRanks = append(hotRanks, hotRank)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("oracle: iterate %s: %w", surface, err)
	}

	var cursor string
	if len(feedPosts) > req.Limit && req.Limit > 0 {
		feedPosts = feedPosts[:req.Limit]
		hotRanks = hotRanks[:req.Limit]
		cursor = r.buildCursor(feedPosts[len(feedPosts)-1], req.Sort, hotRanks[len(hotRanks)-1], queryTime)
	}
	return feedPosts, cursor, nil
}

// oracleFeedPageResult is one oracle page: its posts and the cursor it
// returned.
type oracleFeedPageResult struct {
	Posts  []*posts.PostView
	Cursor string
}

// URIs is the page's post URIs, in order.
func (p oracleFeedPageResult) URIs() []string {
	uris := make([]string, 0, len(p.Posts))
	for _, post := range p.Posts {
		uris = append(uris, post.URI)
	}
	return uris
}

// readOracleFeedPages pages the old read with its own cursors, starting from
// req (its Cursor included), until the cursor runs out.
func readOracleFeedPages(t *testing.T, r *feedRepoBase, surface oracleFeedSurface, req oracleFeedRequest) []oracleFeedPageResult {
	t.Helper()

	const maxPages = 50
	var pages []oracleFeedPageResult
	for range maxPages {
		views, next, err := oracleFeedPage(t.Context(), r, surface, req)
		require.NoErrorf(t, err, "oracle %s page %d", surface, len(pages)+1)
		pages = append(pages, oracleFeedPageResult{Posts: views, Cursor: next})
		if next == "" {
			return pages
		}
		req.Cursor = next
	}
	require.FailNowf(t, "oracle feed did not end", "%s still had a cursor after %d pages", surface, maxPages)
	return nil
}

// oraclePagedURIs concatenates the URIs of every oracle page.
func oraclePagedURIs(pages []oracleFeedPageResult) []string {
	var uris []string
	for _, page := range pages {
		uris = append(uris, page.URIs()...)
	}
	return uris
}

// feedPostSelectClause builds the SELECT ... FROM posts p clause of the oracle
// queries (the old community, timeline, discover and search reads). It is postViewSelectColumns (the
// single source of truth for PostView hydration, see post_repo.go) plus a hot_rank
// column: pass feedHotRankExpression for hot sort, the search relevance rank,
// or "NULL::numeric" for other sorts.
func feedPostSelectClause(hotRankExpr string) string {
	return `
		SELECT` + postViewSelectColumns + `,
			` + hotRankExpr + ` as hot_rank
		FROM posts p`
}

// scanFeedPost scans a database row into a PostView plus its computed hot_rank.
// Rows MUST be selected via feedPostSelectClause; all PostView hydration is delegated
// to scanPostView so feed and post queries can never drift apart.
func (r *feedRepoBase) scanFeedPost(rows *sql.Rows) (*posts.PostView, float64, error) {
	var hotRank sql.NullFloat64
	postView, err := scanPostView(rows, &hotRank)
	if err != nil {
		return nil, 0, err
	}

	// hot_rank is NULL for ordinary non-hot feeds; search reuses it for relevance.
	hotRankValue := 0.0
	if hotRank.Valid {
		hotRankValue = hotRank.Float64
	}

	return postView, hotRankValue, nil
}
