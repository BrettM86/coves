package postgres

import (
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

type postgresFeedRepo struct {
	db *sql.DB
}

// sortClauses maps sort types to safe SQL ORDER BY clauses
// This whitelist prevents SQL injection via dynamic ORDER BY construction
var sortClauses = map[string]string{
	"hot": `(p.score / POWER(EXTRACT(EPOCH FROM (NOW() - p.created_at))/3600 + 2, 1.5)) DESC, p.created_at DESC, p.uri DESC`,
	"top": `p.score DESC, p.created_at DESC, p.uri DESC`,
	"new": `p.created_at DESC, p.uri DESC`,
}

// hotRankExpression is the SQL expression for computing the hot rank
// NOTE: Uses NOW() which means hot_rank changes over time - this is expected behavior
// for hot sorting (posts naturally age out). Slight time drift between cursor creation
// and usage may cause minor reordering but won't drop posts entirely (unlike using raw score).
const hotRankExpression = `(p.score / POWER(EXTRACT(EPOCH FROM (NOW() - p.created_at))/3600 + 2, 1.5))`

// NewCommunityFeedRepository creates a new PostgreSQL feed repository
func NewCommunityFeedRepository(db *sql.DB) communityFeeds.Repository {
	return &postgresFeedRepo{db: db}
}

// GetCommunityFeed retrieves posts from a community with sorting and pagination
// Single query with JOINs for optimal performance
func (r *postgresFeedRepo) GetCommunityFeed(ctx context.Context, req communityFeeds.GetCommunityFeedRequest) ([]*communityFeeds.FeedViewPost, *string, error) {
	// Build ORDER BY clause based on sort type
	orderBy, timeFilter := r.buildSortClause(req.Sort, req.Timeframe)

	// Build cursor filter for pagination
	cursorFilter, cursorValues, err := r.parseCursor(req.Cursor, req.Sort)
	if err != nil {
		return nil, nil, communityFeeds.ErrInvalidCursor
	}

	// Build the main query
	// For hot sort, we need to compute and return the hot_rank for cursor building
	var selectClause string
	if req.Sort == "hot" {
		selectClause = fmt.Sprintf(`
		SELECT
			p.uri, p.cid, p.rkey,
			p.author_did, u.handle as author_handle,
			p.community_did, c.name as community_name, c.avatar_cid as community_avatar,
			p.title, p.content, p.content_facets, p.embed, p.content_labels,
			p.created_at, p.edited_at, p.indexed_at,
			p.upvote_count, p.downvote_count, p.score, p.comment_count,
			%s as hot_rank
		FROM posts p`, hotRankExpression)
	} else {
		selectClause = `
		SELECT
			p.uri, p.cid, p.rkey,
			p.author_did, u.handle as author_handle,
			p.community_did, c.name as community_name, c.avatar_cid as community_avatar,
			p.title, p.content, p.content_facets, p.embed, p.content_labels,
			p.created_at, p.edited_at, p.indexed_at,
			p.upvote_count, p.downvote_count, p.score, p.comment_count,
			NULL::numeric as hot_rank
		FROM posts p`
	}

	query := fmt.Sprintf(`
		%s
		INNER JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did
		WHERE p.community_did = $1
			AND p.deleted_at IS NULL
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, selectClause, timeFilter, cursorFilter, orderBy)

	// Prepare query arguments
	args := []interface{}{req.Community, req.Limit + 1} // +1 to check for next page
	args = append(args, cursorValues...)

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
		feedPost, hotRank, err := r.scanFeedViewPost(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to scan feed post: %w", err)
		}
		feedPosts = append(feedPosts, feedPost)
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
		cursorStr := r.buildCursor(lastPost, req.Sort, lastHotRank)
		cursor = &cursorStr
	}

	return feedPosts, cursor, nil
}

// buildSortClause returns the ORDER BY SQL and optional time filter
func (r *postgresFeedRepo) buildSortClause(sort, timeframe string) (string, string) {
	// Use whitelist map for ORDER BY clause (defense-in-depth against SQL injection)
	orderBy := sortClauses[sort]
	if orderBy == "" {
		orderBy = sortClauses["hot"] // safe default
	}

	// Add time filter for "top" sort
	var timeFilter string
	if sort == "top" {
		timeFilter = r.buildTimeFilter(timeframe)
	}

	return orderBy, timeFilter
}

// buildTimeFilter returns SQL filter for timeframe
func (r *postgresFeedRepo) buildTimeFilter(timeframe string) string {
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

// parseCursor decodes pagination cursor
func (r *postgresFeedRepo) parseCursor(cursor *string, sort string) (string, []interface{}, error) {
	if cursor == nil || *cursor == "" {
		return "", nil, nil
	}

	// Decode base64 cursor
	decoded, err := base64.StdEncoding.DecodeString(*cursor)
	if err != nil {
		return "", nil, fmt.Errorf("invalid cursor encoding")
	}

	// Parse cursor based on sort type using :: delimiter (Bluesky convention)
	parts := strings.Split(string(decoded), "::")

	switch sort {
	case "new":
		// Cursor format: timestamp::uri
		if len(parts) != 2 {
			return "", nil, fmt.Errorf("invalid cursor format")
		}

		createdAt := parts[0]
		uri := parts[1]

		// Validate timestamp format
		if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return "", nil, fmt.Errorf("invalid cursor timestamp")
		}

		// Validate URI format (must be AT-URI)
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("invalid cursor URI")
		}

		filter := `AND (p.created_at < $3 OR (p.created_at = $3 AND p.uri < $4))`
		return filter, []interface{}{createdAt, uri}, nil

	case "top":
		// Cursor format: score::timestamp::uri
		if len(parts) != 3 {
			return "", nil, fmt.Errorf("invalid cursor format for %s sort", sort)
		}

		scoreStr := parts[0]
		createdAt := parts[1]
		uri := parts[2]

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

		filter := `AND (p.score < $3 OR (p.score = $3 AND p.created_at < $4) OR (p.score = $3 AND p.created_at = $4 AND p.uri < $5))`
		return filter, []interface{}{score, createdAt, uri}, nil

	case "hot":
		// Cursor format: hot_rank::timestamp::uri
		// CRITICAL: Must use computed hot_rank, not raw score, to prevent pagination bugs
		if len(parts) != 3 {
			return "", nil, fmt.Errorf("invalid cursor format for hot sort")
		}

		hotRankStr := parts[0]
		createdAt := parts[1]
		uri := parts[2]

		// Validate hot_rank is numeric (float)
		hotRank := 0.0
		if _, err := fmt.Sscanf(hotRankStr, "%f", &hotRank); err != nil {
			return "", nil, fmt.Errorf("invalid cursor hot rank")
		}

		// Validate timestamp format
		if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return "", nil, fmt.Errorf("invalid cursor timestamp")
		}

		// Validate URI format (must be AT-URI)
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("invalid cursor URI")
		}

		// CRITICAL: Compare against the computed hot_rank expression, not p.score
		// This prevents dropping posts with higher raw scores but lower hot ranks
		//
		// NOTE: We exclude the exact cursor post by URI to handle time drift in hot_rank
		// (hot_rank changes with NOW(), so the same post may have different ranks over time)
		filter := fmt.Sprintf(`AND ((%s < $3 OR (%s = $3 AND p.created_at < $4) OR (%s = $3 AND p.created_at = $4 AND p.uri < $5)) AND p.uri != $6)`,
			hotRankExpression, hotRankExpression, hotRankExpression)
		return filter, []interface{}{hotRank, createdAt, uri, uri}, nil

	default:
		return "", nil, nil
	}
}

// buildCursor creates pagination cursor from last post
func (r *postgresFeedRepo) buildCursor(post *posts.PostView, sort string, hotRank float64) string {
	var cursorStr string
	// Use :: as delimiter following Bluesky convention
	// Safe because :: doesn't appear in ISO timestamps or AT-URIs
	const delimiter = "::"

	switch sort {
	case "new":
		// Format: timestamp::uri (following Bluesky pattern)
		cursorStr = fmt.Sprintf("%s%s%s", post.CreatedAt.Format(time.RFC3339Nano), delimiter, post.URI)

	case "top":
		// Format: score::timestamp::uri
		score := 0
		if post.Stats != nil {
			score = post.Stats.Score
		}
		cursorStr = fmt.Sprintf("%d%s%s%s%s", score, delimiter, post.CreatedAt.Format(time.RFC3339Nano), delimiter, post.URI)

	case "hot":
		// Format: hot_rank::timestamp::uri
		// CRITICAL: Use computed hot_rank with full precision to prevent pagination bugs
		// Using 'g' format with -1 precision gives us full float64 precision without trailing zeros
		// This prevents posts being dropped when hot ranks differ by <1e-6
		hotRankStr := strconv.FormatFloat(hotRank, 'g', -1, 64)
		cursorStr = fmt.Sprintf("%s%s%s%s%s", hotRankStr, delimiter, post.CreatedAt.Format(time.RFC3339Nano), delimiter, post.URI)

	default:
		cursorStr = post.URI
	}

	return base64.StdEncoding.EncodeToString([]byte(cursorStr))
}

// scanFeedViewPost scans a row into FeedViewPost
// Alpha: No viewer state - basic community feed only
func (r *postgresFeedRepo) scanFeedViewPost(rows *sql.Rows) (*communityFeeds.FeedViewPost, float64, error) {
	var (
		postView        posts.PostView
		authorView      posts.AuthorView
		communityRef    posts.CommunityRef
		title, content  sql.NullString
		facets, embed   sql.NullString
		labels          pq.StringArray
		editedAt        sql.NullTime
		communityAvatar sql.NullString
		hotRank         sql.NullFloat64
	)

	err := rows.Scan(
		&postView.URI, &postView.CID, &postView.RKey,
		&authorView.DID, &authorView.Handle,
		&communityRef.DID, &communityRef.Name, &communityAvatar,
		&title, &content, &facets, &embed, &labels,
		&postView.CreatedAt, &editedAt, &postView.IndexedAt,
		&postView.UpvoteCount, &postView.DownvoteCount, &postView.Score, &postView.CommentCount,
		&hotRank,
	)
	if err != nil {
		return nil, 0, err
	}

	// Build author view (no display_name or avatar in users table yet)
	postView.Author = &authorView

	// Build community ref
	communityRef.Avatar = nullStringPtr(communityAvatar)
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

	// Alpha: No viewer state for basic feed
	// TODO(feed-generator): Implement viewer state (saved, voted, blocked) in feed generator skeleton

	// Build the record (required by lexicon - social.coves.post.record structure)
	record := map[string]interface{}{
		"$type":     "social.coves.post.record",
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
	if len(labels) > 0 {
		record["contentLabels"] = labels
	}

	postView.Record = record

	// Wrap in FeedViewPost
	feedPost := &communityFeeds.FeedViewPost{
		Post: &postView,
		// Reason: nil, // TODO(feed-generator): Implement pinned posts
		// Reply: nil,  // TODO(feed-generator): Implement reply context
	}

	// Return the computed hot_rank (0.0 if NULL for non-hot sorts)
	hotRankValue := 0.0
	if hotRank.Valid {
		hotRankValue = hotRank.Float64
	}

	return feedPost, hotRankValue, nil
}

// Helper function to convert sql.NullString to *string
func nullStringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}
