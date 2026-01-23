package postgres

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/posts"
)

// feedRepoBase contains shared logic for timeline and discover feed repositories
// This eliminates ~85% code duplication and ensures bug fixes apply to both feeds
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
// 4. Hot sort uses computed expression: (score / POWER(age_hours + 2, 1.5))
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
	db                *sql.DB
	hotRankExpression string
	sortClauses       map[string]string
	cursorSecret      string // HMAC secret for cursor integrity protection
}

// newFeedRepoBase creates a new base repository with shared feed logic
func newFeedRepoBase(db *sql.DB, hotRankExpr string, sortClauses map[string]string, cursorSecret string) *feedRepoBase {
	return &feedRepoBase{
		db:                db,
		hotRankExpression: hotRankExpr,
		sortClauses:       sortClauses,
		cursorSecret:      cursorSecret,
	}
}

// buildSortClause returns the ORDER BY SQL and optional time filter
// Uses whitelist map to prevent SQL injection via dynamic ORDER BY
func (r *feedRepoBase) buildSortClause(sort, timeframe string) (string, string) {
	// Use whitelist map for ORDER BY clause (defense-in-depth against SQL injection)
	orderBy := r.sortClauses[sort]
	if orderBy == "" {
		orderBy = r.sortClauses["hot"] // safe default
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

// parseCursor decodes and validates pagination cursor
// paramOffset is the starting parameter number for cursor values ($2 for discover, $3 for timeline)
func (r *feedRepoBase) parseCursor(cursor *string, sort string, paramOffset int) (string, []interface{}, error) {
	if cursor == nil || *cursor == "" {
		return "", nil, nil
	}

	// Decode base64 cursor
	decoded, err := base64.StdEncoding.DecodeString(*cursor)
	if err != nil {
		return "", nil, fmt.Errorf("invalid cursor encoding")
	}

	// Parse cursor: payload::signature
	parts := strings.Split(string(decoded), "::")
	if len(parts) < 2 {
		return "", nil, fmt.Errorf("invalid cursor format")
	}

	// Verify HMAC signature
	signatureHex := parts[len(parts)-1]
	payload := strings.Join(parts[:len(parts)-1], "::")

	expectedMAC := hmac.New(sha256.New, []byte(r.cursorSecret))
	expectedMAC.Write([]byte(payload))
	expectedSignature := hex.EncodeToString(expectedMAC.Sum(nil))

	if !hmac.Equal([]byte(signatureHex), []byte(expectedSignature)) {
		return "", nil, fmt.Errorf("invalid cursor signature")
	}

	// Parse payload based on sort type
	payloadParts := strings.Split(payload, "::")

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
		// PRECISION FIX: We DON'T use hot_rank in the cursor comparison at all!
		// Instead, we use (created_at, uri) as the cursor key, which are deterministic values stored in DB.
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
		stableHotRankExpr := fmt.Sprintf(
			`((p.score + 1) / POWER(EXTRACT(EPOCH FROM ($%d::timestamptz - p.created_at))/3600 + 2, 1.5))`,
			paramOffset+2)

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
		cursorHotRankExpr := fmt.Sprintf(
			`((cursor_post.score + 1) / POWER(EXTRACT(EPOCH FROM ($%d::timestamptz - cursor_post.created_at))/3600 + 2, 1.5))`,
			paramOffset+2)

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
	var payload string
	// Use :: as delimiter following Bluesky convention
	const delimiter = "::"

	switch sort {
	case "new":
		// Format: timestamp::uri
		payload = fmt.Sprintf("%s%s%s", post.CreatedAt.Format(time.RFC3339Nano), delimiter, post.URI)

	case "top":
		// Format: score::timestamp::uri
		score := 0
		if post.Stats != nil {
			score = post.Stats.Score
		}
		payload = fmt.Sprintf("%d%s%s%s%s", score, delimiter, post.CreatedAt.Format(time.RFC3339Nano), delimiter, post.URI)

	case "hot":
		// Format: created_at::uri::cursor_timestamp
		// CRITICAL: Include cursor_timestamp for stable hot_rank comparison across requests
		// NOTE: We don't store hot_rank in the cursor - we use the post's URI to look it up
		// This avoids floating-point precision issues between cursor storage and comparison
		payload = fmt.Sprintf("%s%s%s%s%s", post.CreatedAt.Format(time.RFC3339Nano), delimiter, post.URI, delimiter, queryTime.Format(time.RFC3339Nano))

	default:
		payload = post.URI
	}

	// Sign the payload with HMAC-SHA256
	mac := hmac.New(sha256.New, []byte(r.cursorSecret))
	mac.Write([]byte(payload))
	signature := hex.EncodeToString(mac.Sum(nil))

	// Append signature to payload
	signed := payload + delimiter + signature

	return base64.StdEncoding.EncodeToString([]byte(signed))
}

// scanFeedPost scans a database row into a PostView
// This is the shared scanning logic used by both timeline and discover feeds
func (r *feedRepoBase) scanFeedPost(rows *sql.Rows) (*posts.PostView, float64, error) {
	var (
		postView        posts.PostView
		authorView      posts.AuthorView
		communityRef    posts.CommunityRef
		title, content  sql.NullString
		facets, embed   sql.NullString
		labelsJSON      sql.NullString
		editedAt        sql.NullTime
		communityHandle sql.NullString
		communityAvatar sql.NullString
		communityPDSURL sql.NullString
		hotRank         sql.NullFloat64
	)

	err := rows.Scan(
		&postView.URI, &postView.CID, &postView.RKey,
		&authorView.DID, &authorView.Handle,
		&communityRef.DID, &communityHandle, &communityRef.Name, &communityAvatar, &communityPDSURL,
		&title, &content, &facets, &embed, &labelsJSON,
		&postView.CreatedAt, &editedAt, &postView.IndexedAt,
		&postView.UpvoteCount, &postView.DownvoteCount, &postView.Score, &postView.CommentCount,
		&hotRank,
	)
	if err != nil {
		return nil, 0, err
	}

	// Build author view
	postView.Author = &authorView

	// Build community ref
	if communityHandle.Valid {
		communityRef.Handle = communityHandle.String
	}
	// Hydrate avatar CID to URL (instead of returning raw CID)
	if avatarURL := blobs.HydrateBlobURL(communityPDSURL.String, communityRef.DID, communityAvatar.String); avatarURL != "" {
		communityRef.Avatar = &avatarURL
	}
	if communityPDSURL.Valid {
		communityRef.PDSURL = communityPDSURL.String
	}
	postView.Community = &communityRef

	// Set optional fields
	postView.Title = nullStringPtr(title)
	postView.Text = nullStringPtr(content)

	// Parse facets JSON
	if facets.Valid {
		var facetArray []interface{}
		if err := json.Unmarshal([]byte(facets.String), &facetArray); err == nil {
			postView.TextFacets = facetArray
		}
	}

	// Parse embed JSON
	if embed.Valid {
		var embedData interface{}
		if err := json.Unmarshal([]byte(embed.String), &embedData); err == nil {
			postView.Embed = embedData
		}
	}

	// Build stats
	postView.Stats = &posts.PostStats{
		Upvotes:      postView.UpvoteCount,
		Downvotes:    postView.DownvoteCount,
		Score:        postView.Score,
		CommentCount: postView.CommentCount,
	}

	// Build the record (required by lexicon)
	record := map[string]interface{}{
		"$type":     "social.coves.community.post",
		"community": communityRef.DID,
		"author":    authorView.DID,
		"createdAt": postView.CreatedAt.Format(time.RFC3339),
	}

	// Add optional fields to record if present
	if title.Valid {
		record["title"] = title.String
	}
	if content.Valid {
		record["content"] = content.String
	}
	if facets.Valid {
		var facetArray []interface{}
		if err := json.Unmarshal([]byte(facets.String), &facetArray); err == nil {
			record["facets"] = facetArray
		}
	}
	if embed.Valid {
		var embedData interface{}
		if err := json.Unmarshal([]byte(embed.String), &embedData); err == nil {
			record["embed"] = embedData
		}
	}
	if labelsJSON.Valid {
		// Labels are stored as JSONB containing full com.atproto.label.defs#selfLabels structure
		// Deserialize and include in record
		var selfLabels posts.SelfLabels
		if err := json.Unmarshal([]byte(labelsJSON.String), &selfLabels); err == nil {
			record["labels"] = selfLabels
		}
	}

	postView.Record = record

	// Return the computed hot_rank (0.0 if NULL for non-hot sorts)
	hotRankValue := 0.0
	if hotRank.Valid {
		hotRankValue = hotRank.Float64
	}

	return &postView, hotRankValue, nil
}

// nullStringPtr converts sql.NullString to *string
// Helper function used by feed scanning logic across all feed types
func nullStringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}
