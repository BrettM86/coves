package postgres

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
)

// feedHotRankExpression is the live hot-rank expression shared by the discover,
// timeline, and community feed queries (both the hot_rank SELECT column and the
// ORDER BY). It uses NOW(), so a post's rank decays between requests — expected
// for hot sorting. parseCursor rebuilds the same formula with the cursor's
// pinned timestamp so pagination stays stable; the two MUST stay in sync, which
// is why both go through hotRankSQL.
var feedHotRankExpression = hotRankSQL("p", "NOW()")

const cursorDelimiter = "::"

// feedSortClauses whitelists ORDER BY clauses for all post feeds
// (discover, timeline, community, search), preventing SQL injection via dynamic ORDER BY
var feedSortClauses = map[string]string{
	"hot":       feedHotRankExpression + ` DESC, p.created_at DESC, p.uri DESC`,
	"relevance": `hot_rank DESC, p.created_at DESC, p.uri DESC`,
	"top":       `p.score DESC, p.created_at DESC, p.uri DESC`,
	"new":       `p.created_at DESC, p.uri DESC`,
}

// feedRepoBase contains shared logic for the discover, timeline, and community
// feed repositories
// This eliminates ~85% code duplication and ensures bug fixes apply to all feeds
//
// DATABASE INDEXES REQUIRED:
// The feed queries rely on these indexes (created in migration 011_create_posts_table.sql):
//
// 1. idx_posts_community_created ON posts(community_did, created_at DESC) WHERE deleted_at IS NULL
//   - Used by: Both timeline and discover for "new" sort
//   - Covers: Community filtering + chronological ordering + soft delete filter
//
// 2. idx_posts_community_score ON posts(community_did, score DESC, created_at DESC) WHERE deleted_at IS NULL
//   - Used by: Both timeline and discover for "top" sort
//   - Covers: Community filtering + score ordering + tie-breaking + soft delete filter
//
// 3. idx_subscriptions_user_community ON community_subscriptions(user_did, community_did)
//   - Used by: Timeline feed (JOIN with subscriptions)
//   - Covers: User subscription lookup
//
// 4. Hot sort uses computed expression: see hotRankSQL below
//   - Cannot be indexed directly (computed at query time)
//   - Uses idx_posts_community_created for base ordering
//   - Performance: ~10-20ms for timeline, ~8-15ms for discover (acceptable for alpha)
//
// PERFORMANCE NOTES:
// - All queries use single execution (no N+1)
// - JOINs are minimal (3 for timeline, 2 for discover)
// - Partial indexes (WHERE deleted_at IS NULL) eliminate soft-deleted posts efficiently
// - Cursor pagination is stable (no offset drift)
// - Limit+1 pattern checks for next page without extra query
type feedRepoBase struct {
	db           *sql.DB
	cursorSecret string // HMAC secret for cursor integrity protection
}

// hotRankSQL builds the hot-rank SQL expression for a posts-table alias and a
// "now" expression (NOW() for live sorting, a $n::timestamptz parameter for
// stable cursor comparison across pages).
//
// Formula: (SIGN(score) * LN(ABS(score) + 1) + 1) / (age_hours + 2)^1.5
//
// The numerator is log-dampened so vote counts from different-sized populations
// (e.g. bridged Lemmy communities with thousands of voters) can't dominate the
// feed for days: going from 0 to 10 votes counts as much as going from 10 to
// ~120. A 0-vote post still gets a numerator of 1 so brand-new organic posts
// surface, and SIGN/ABS keep the expression defined and monotonic for negative
// scores (LN of a non-positive value errors in PostgreSQL).
//
// GREATEST(age, 0) clamps negative age — created_at after the "now" expression,
// from posts created between paginated requests or federated records with
// future timestamps. Clamping the age (not the POWER base) means a future-dated
// post ranks exactly like a brand-new one instead of gaining a boost, and the
// POWER base stays >= 2 so it can never error on a negative base.
func hotRankSQL(alias, nowExpr string) string {
	return fmt.Sprintf(
		`((SIGN(%[1]s.score) * LN(ABS(%[1]s.score) + 1) + 1) / POWER(GREATEST(EXTRACT(EPOCH FROM (%[2]s - %[1]s.created_at))/3600, 0) + 2, 1.5))`,
		alias, nowExpr)
}

// newFeedRepoBase creates a new base repository with shared feed logic
func newFeedRepoBase(db *sql.DB, cursorSecret string) *feedRepoBase {
	return &feedRepoBase{
		db:           db,
		cursorSecret: cursorSecret,
	}
}

// buildSortClause returns the ORDER BY SQL and optional time filter
// Uses whitelist map to prevent SQL injection via dynamic ORDER BY
func (r *feedRepoBase) buildSortClause(sort, timeframe string) (string, string) {
	// Use whitelist map for ORDER BY clause (defense-in-depth against SQL injection)
	orderBy := feedSortClauses[sort]
	if orderBy == "" {
		orderBy = feedSortClauses["hot"] // safe default
	}

	// Add time filter for "top" sort
	var timeFilter string
	if sort == "top" {
		timeFilter = r.buildTimeFilter(timeframe)
	}

	return orderBy, timeFilter
}

// buildTimeFilter returns SQL filter for timeframe
func (r *feedRepoBase) buildTimeFilter(timeframe string) string {
	if timeframe == "" || timeframe == "all" {
		return ""
	}

	var interval string
	switch timeframe {
	case "hour":
		interval = "1 hour"
	case "day":
		interval = "1 day"
	case "week":
		interval = "1 week"
	case "month":
		interval = "1 month"
	case "year":
		interval = "1 year"
	default:
		return ""
	}

	return fmt.Sprintf("AND p.created_at > NOW() - INTERVAL '%s'", interval)
}

func (r *feedRepoBase) signCursorPayload(payload string) string {
	mac := hmac.New(sha256.New, []byte(r.cursorSecret))
	mac.Write([]byte(payload))
	signature := hex.EncodeToString(mac.Sum(nil))
	signed := payload + cursorDelimiter + signature
	return base64.StdEncoding.EncodeToString([]byte(signed))
}

func (r *feedRepoBase) verifyCursorPayload(cursor string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return "", fmt.Errorf("invalid cursor encoding: %w", err)
	}

	signed := string(decoded)
	signatureSeparator := strings.LastIndex(signed, cursorDelimiter)
	if signatureSeparator <= 0 || signatureSeparator+len(cursorDelimiter) == len(signed) {
		return "", fmt.Errorf("invalid cursor format")
	}

	payload := signed[:signatureSeparator]
	signature := signed[signatureSeparator+len(cursorDelimiter):]
	expectedMAC := hmac.New(sha256.New, []byte(r.cursorSecret))
	expectedMAC.Write([]byte(payload))
	expectedSignature := hex.EncodeToString(expectedMAC.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		return "", fmt.Errorf("invalid cursor signature")
	}

	return payload, nil
}

// parseCursor decodes and validates pagination cursor
// paramOffset is the starting parameter number for cursor values
// ($2 for discover, $3 for timeline and community feed)
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

func (r *feedRepoBase) parseCursorPayload(payload, sort string, paramOffset int) (string, []interface{}, error) {
	payloadParts := strings.Split(payload, cursorDelimiter)

	switch sort {
	case "new":
		// Cursor format: timestamp::uri
		if len(payloadParts) != 2 {
			return "", nil, fmt.Errorf("invalid cursor format")
		}

		createdAt := payloadParts[0]
		uri := payloadParts[1]

		// Validate timestamp format
		if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return "", nil, fmt.Errorf("invalid cursor timestamp")
		}

		// Validate URI format (must be AT-URI)
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("invalid cursor URI")
		}

		filter := fmt.Sprintf(`AND (p.created_at < $%d OR (p.created_at = $%d AND p.uri < $%d))`,
			paramOffset, paramOffset, paramOffset+1)
		return filter, []interface{}{createdAt, uri}, nil

	case "top":
		// Cursor format: score::timestamp::uri
		if len(payloadParts) != 3 {
			return "", nil, fmt.Errorf("invalid cursor format for %s sort", sort)
		}

		scoreStr := payloadParts[0]
		createdAt := payloadParts[1]
		uri := payloadParts[2]

		// Validate score is numeric
		score := 0
		if _, err := fmt.Sscanf(scoreStr, "%d", &score); err != nil {
			return "", nil, fmt.Errorf("invalid cursor score")
		}

		// Validate timestamp format
		if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return "", nil, fmt.Errorf("invalid cursor timestamp")
		}

		// Validate URI format (must be AT-URI)
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("invalid cursor URI")
		}

		filter := fmt.Sprintf(`AND (p.score < $%d OR (p.score = $%d AND p.created_at < $%d) OR (p.score = $%d AND p.created_at = $%d AND p.uri < $%d))`,
			paramOffset, paramOffset, paramOffset+1, paramOffset, paramOffset+1, paramOffset+2)
		return filter, []interface{}{score, createdAt, uri}, nil

	case "hot":
		// Cursor format: created_at::uri::cursor_timestamp
		// CRITICAL: cursor_timestamp is when the cursor was created, used for stable hot_rank comparison
		// This prevents pagination bugs caused by hot_rank drift when NOW() changes between requests
		//
		// PRECISION FIX: We DON'T serialize the hot_rank value into the cursor!
		// Instead, the cursor stores (created_at, uri) — deterministic values from the DB —
		// and the comparison recomputes the cursor post's rank via subquery using the
		// exact same SQL expression as the main query, so both sides of the float
		// comparison are computed identically.
		// The hot sort ORDER BY is: hot_rank DESC, created_at DESC, uri DESC
		// For posts with the same hot_rank, created_at and uri provide stable ordering.
		//
		// This works because:
		// 1. Posts with very different hot_ranks will be separated by created_at anyway
		// 2. Posts with similar hot_ranks (same score, close creation times) will be ordered by created_at, uri
		// 3. The cursor_timestamp ensures hot_rank is computed consistently across pages
		if len(payloadParts) != 3 {
			return "", nil, fmt.Errorf("invalid cursor format for hot sort")
		}

		createdAt := payloadParts[0]
		uri := payloadParts[1]
		cursorTimestamp := payloadParts[2]

		// Validate created_at timestamp format
		if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return "", nil, fmt.Errorf("invalid cursor created_at timestamp")
		}

		// Validate URI format (must be AT-URI)
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("invalid cursor URI")
		}

		// Validate cursor timestamp format
		if _, err := time.Parse(time.RFC3339Nano, cursorTimestamp); err != nil {
			return "", nil, fmt.Errorf("invalid cursor timestamp")
		}

		// CRITICAL: Use cursor_timestamp instead of NOW() for stable hot_rank comparison
		// This ensures posts don't drift across page boundaries due to time passing
		//
		// Both expressions come from hotRankSQL — the same builder used for the
		// live ORDER BY — so the ranking formula can never diverge between
		// sorting and pagination
		cursorTimestampParam := fmt.Sprintf("$%d::timestamptz", paramOffset+2)
		stableHotRankExpr := hotRankSQL("p", cursorTimestampParam)

		// Filter by cursor position in the hot-sorted result set
		// The ORDER BY is: hot_rank DESC, created_at DESC, uri DESC
		// We need posts that come AFTER the cursor position in this ordering.
		//
		// A post comes after the cursor if ANY of:
		// 1. It has a lower hot_rank (hot_rank DESC means lower values come later)
		// 2. Same hot_rank AND lower created_at
		// 3. Same hot_rank AND same created_at AND lower uri
		//
		// To avoid floating-point comparison issues with hot_rank, we use a subquery
		// to get the cursor post's hot_rank and compare using the SAME expression
		cursorHotRankExpr := hotRankSQL("cursor_post", cursorTimestampParam)

		// Use a subquery to find the cursor post and compare hot_ranks using identical expressions
		// This ensures floating-point values are computed the same way on both sides
		filter := fmt.Sprintf(`AND (
			%s < (SELECT %s FROM posts cursor_post WHERE cursor_post.uri = $%d)
			OR (%s = (SELECT %s FROM posts cursor_post WHERE cursor_post.uri = $%d) AND p.created_at < $%d)
			OR (%s = (SELECT %s FROM posts cursor_post WHERE cursor_post.uri = $%d) AND p.created_at = $%d AND p.uri < $%d)
		)`,
			stableHotRankExpr, cursorHotRankExpr, paramOffset+1,
			stableHotRankExpr, cursorHotRankExpr, paramOffset+1, paramOffset,
			stableHotRankExpr, cursorHotRankExpr, paramOffset+1, paramOffset, paramOffset+1)
		return filter, []interface{}{createdAt, uri, cursorTimestamp}, nil

	default:
		return "", nil, nil
	}
}

// buildCursor creates HMAC-signed pagination cursor from last post
// SECURITY: Cursor is signed with HMAC-SHA256 to prevent manipulation
// queryTime is the timestamp when the query was executed, used for stable hot_rank comparison
func (r *feedRepoBase) buildCursor(post *posts.PostView, sort string, hotRank float64, queryTime time.Time) string {
	return r.signCursorPayload(r.buildCursorPayload(post, sort, hotRank, queryTime))
}

func (r *feedRepoBase) buildCursorPayload(post *posts.PostView, sort string, hotRank float64, queryTime time.Time) string {
	var payload string

	switch sort {
	case "new":
		// Format: timestamp::uri
		payload = fmt.Sprintf("%s%s%s", post.CreatedAt.Format(time.RFC3339Nano), cursorDelimiter, post.URI)

	case "top":
		// Format: score::timestamp::uri
		score := 0
		if post.Stats != nil {
			score = post.Stats.Score
		}
		payload = fmt.Sprintf("%d%s%s%s%s", score, cursorDelimiter, post.CreatedAt.Format(time.RFC3339Nano), cursorDelimiter, post.URI)

	case "hot":
		// Format: created_at::uri::cursor_timestamp
		// CRITICAL: Include cursor_timestamp for stable hot_rank comparison across requests
		// NOTE: We don't store hot_rank in the cursor - we use the post's URI to look it up
		// This avoids floating-point precision issues between cursor storage and comparison
		payload = fmt.Sprintf("%s%s%s%s%s", post.CreatedAt.Format(time.RFC3339Nano), cursorDelimiter, post.URI, cursorDelimiter, queryTime.Format(time.RFC3339Nano))

	default:
		payload = post.URI
	}

	return payload
}

func (r *feedRepoBase) parseSearchCursor(cursor *string, req communityFeeds.SearchPostsRequest, paramOffset int, rankExpression string) (string, []interface{}, error) {
	if cursor == nil || *cursor == "" {
		return "", nil, nil
	}

	payload, err := r.verifyCursorPayload(*cursor)
	if err != nil {
		return "", nil, fmt.Errorf("invalid search cursor signature: %w", communityFeeds.ErrInvalidCursor)
	}

	envelope := strings.SplitN(payload, cursorDelimiter, 3)
	if len(envelope) != 3 || envelope[0] != "search" {
		return "", nil, fmt.Errorf("invalid search cursor envelope: %w", communityFeeds.ErrInvalidCursor)
	}
	if envelope[1] != postSearchScopeHash(req) {
		return "", nil, fmt.Errorf("search cursor result set mismatch: %w", communityFeeds.ErrInvalidCursor)
	}

	if req.Sort == "new" || req.Sort == "top" {
		filter, values, err := r.parseCursorPayload(envelope[2], req.Sort, paramOffset)
		if err != nil {
			return "", nil, fmt.Errorf("invalid search cursor position: %w", communityFeeds.ErrInvalidCursor)
		}
		return filter, values, nil
	}

	return parseRelevanceCursorPayload(envelope[2], paramOffset, rankExpression)
}

func parseRelevanceCursorPayload(payload string, paramOffset int, rankExpression string) (string, []interface{}, error) {
	parts := strings.Split(payload, cursorDelimiter)
	if len(parts) != 3 {
		return "", nil, fmt.Errorf("invalid relevance cursor format: %w", communityFeeds.ErrInvalidCursor)
	}

	rank, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return "", nil, fmt.Errorf("invalid relevance cursor rank: %w", communityFeeds.ErrInvalidCursor)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return "", nil, fmt.Errorf("invalid relevance cursor timestamp: %w", communityFeeds.ErrInvalidCursor)
	}
	if !strings.HasPrefix(parts[2], "at://") {
		return "", nil, fmt.Errorf("invalid relevance cursor URI: %w", communityFeeds.ErrInvalidCursor)
	}

	// ts_rank_cd returns float4; lib/pq's extra_float_digits=2 round-trips that
	// value exactly, and ::real restores the identical float4 for safe equality.
	filter := fmt.Sprintf(`AND (
			%s < $%d::real
			OR (%s = $%d::real AND p.created_at < $%d)
			OR (%s = $%d::real AND p.created_at = $%d AND p.uri < $%d)
		)`, rankExpression, paramOffset,
		rankExpression, paramOffset, paramOffset+1,
		rankExpression, paramOffset, paramOffset+1, paramOffset+2)
	return filter, []interface{}{rank, createdAt, parts[2]}, nil
}

func (r *feedRepoBase) buildSearchCursor(req communityFeeds.SearchPostsRequest, post *posts.PostView, rank float64) string {
	var innerPayload string
	if req.Sort == "new" || req.Sort == "top" {
		innerPayload = r.buildCursorPayload(post, req.Sort, rank, time.Now())
	} else {
		innerPayload = buildRelevanceCursorPayload(post, rank)
	}

	payload := strings.Join([]string{"search", postSearchScopeHash(req), innerPayload}, cursorDelimiter)
	return r.signCursorPayload(payload)
}

func buildRelevanceCursorPayload(post *posts.PostView, rank float64) string {
	return strings.Join([]string{
		strconv.FormatFloat(rank, 'g', -1, 64),
		post.CreatedAt.Format(time.RFC3339Nano),
		post.URI,
	}, cursorDelimiter)
}

func postSearchScopeHash(req communityFeeds.SearchPostsRequest) string {
	timeframe := ""
	if req.Sort == "top" {
		timeframe = req.Timeframe
	}

	hash := sha256.New()
	for _, value := range []string{req.Query, req.Community, req.Sort, timeframe} {
		hash.Write([]byte(strconv.Itoa(len(value))))
		hash.Write([]byte{':'})
		hash.Write([]byte(value))
	}
	sum := hash.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// feedPostSelectClause builds the SELECT ... FROM posts p clause shared by the
// community, timeline, and discover feed queries. It is postViewSelectColumns (the
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
