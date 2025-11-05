package postgres

import (
	"Coves/internal/core/comments"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"strings"

	"github.com/lib/pq"
)

type postgresCommentRepo struct {
	db *sql.DB
}

// NewCommentRepository creates a new PostgreSQL comment repository
func NewCommentRepository(db *sql.DB) comments.Repository {
	return &postgresCommentRepo{db: db}
}

// Create inserts a new comment into the comments table
// Called by Jetstream consumer after comment is created on PDS
// Idempotent: Returns success if comment already exists (for Jetstream replays)
func (r *postgresCommentRepo) Create(ctx context.Context, comment *comments.Comment) error {
	query := `
		INSERT INTO comments (
			uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11, $12, $13,
			$14, NOW()
		)
		ON CONFLICT (uri) DO NOTHING
		RETURNING id, indexed_at
	`

	err := r.db.QueryRowContext(
		ctx, query,
		comment.URI, comment.CID, comment.RKey, comment.CommenterDID,
		comment.RootURI, comment.RootCID, comment.ParentURI, comment.ParentCID,
		comment.Content, comment.ContentFacets, comment.Embed, comment.ContentLabels, pq.Array(comment.Langs),
		comment.CreatedAt,
	).Scan(&comment.ID, &comment.IndexedAt)

	// ON CONFLICT DO NOTHING returns no rows if duplicate - this is OK (idempotent)
	if err == sql.ErrNoRows {
		return nil // Comment already exists, no error for idempotency
	}

	if err != nil {
		// Check for unique constraint violation
		if strings.Contains(err.Error(), "duplicate key") {
			return comments.ErrCommentAlreadyExists
		}

		return fmt.Errorf("failed to insert comment: %w", err)
	}

	return nil
}

// Update modifies an existing comment's content fields
// Called by Jetstream consumer after comment is updated on PDS
// Preserves vote counts and created_at timestamp
func (r *postgresCommentRepo) Update(ctx context.Context, comment *comments.Comment) error {
	query := `
		UPDATE comments
		SET
			cid = $1,
			content = $2,
			content_facets = $3,
			embed = $4,
			content_labels = $5,
			langs = $6
		WHERE uri = $7 AND deleted_at IS NULL
		RETURNING id, indexed_at, created_at, upvote_count, downvote_count, score, reply_count
	`

	err := r.db.QueryRowContext(
		ctx, query,
		comment.CID,
		comment.Content,
		comment.ContentFacets,
		comment.Embed,
		comment.ContentLabels,
		pq.Array(comment.Langs),
		comment.URI,
	).Scan(
		&comment.ID,
		&comment.IndexedAt,
		&comment.CreatedAt,
		&comment.UpvoteCount,
		&comment.DownvoteCount,
		&comment.Score,
		&comment.ReplyCount,
	)

	if err == sql.ErrNoRows {
		return comments.ErrCommentNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to update comment: %w", err)
	}

	return nil
}

// GetByURI retrieves a comment by its AT-URI
// Used by Jetstream consumer for UPDATE/DELETE operations
func (r *postgresCommentRepo) GetByURI(ctx context.Context, uri string) (*comments.Comment, error) {
	query := `
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at,
			upvote_count, downvote_count, score, reply_count
		FROM comments
		WHERE uri = $1
	`

	var comment comments.Comment
	var langs pq.StringArray

	err := r.db.QueryRowContext(ctx, query, uri).Scan(
		&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
		&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
		&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
		&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt,
		&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
	)

	if err == sql.ErrNoRows {
		return nil, comments.ErrCommentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get comment by URI: %w", err)
	}

	comment.Langs = langs

	return &comment, nil
}

// Delete soft-deletes a comment (sets deleted_at)
// Called by Jetstream consumer after comment is deleted from PDS
// Idempotent: Returns success if comment already deleted
func (r *postgresCommentRepo) Delete(ctx context.Context, uri string) error {
	query := `
		UPDATE comments
		SET deleted_at = NOW()
		WHERE uri = $1 AND deleted_at IS NULL
	`

	result, err := r.db.ExecContext(ctx, query, uri)
	if err != nil {
		return fmt.Errorf("failed to delete comment: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check delete result: %w", err)
	}

	// Idempotent: If no rows affected, comment already deleted (OK for Jetstream replays)
	if rowsAffected == 0 {
		return nil
	}

	return nil
}

// ListByRoot retrieves all active comments in a thread (flat)
// Used for fetching entire comment threads on posts
func (r *postgresCommentRepo) ListByRoot(ctx context.Context, rootURI string, limit, offset int) ([]*comments.Comment, error) {
	query := `
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at,
			upvote_count, downvote_count, score, reply_count
		FROM comments
		WHERE root_uri = $1 AND deleted_at IS NULL
		ORDER BY created_at ASC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, rootURI, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list comments by root: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	var result []*comments.Comment
	for rows.Next() {
		var comment comments.Comment
		var langs pq.StringArray

		err := rows.Scan(
			&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
			&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
			&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		result = append(result, &comment)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating comments: %w", err)
	}

	return result, nil
}

// ListByParent retrieves direct replies to a post or comment
// Used for building nested/threaded comment views
func (r *postgresCommentRepo) ListByParent(ctx context.Context, parentURI string, limit, offset int) ([]*comments.Comment, error) {
	query := `
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at,
			upvote_count, downvote_count, score, reply_count
		FROM comments
		WHERE parent_uri = $1 AND deleted_at IS NULL
		ORDER BY created_at ASC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, parentURI, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list comments by parent: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	var result []*comments.Comment
	for rows.Next() {
		var comment comments.Comment
		var langs pq.StringArray

		err := rows.Scan(
			&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
			&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
			&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		result = append(result, &comment)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating comments: %w", err)
	}

	return result, nil
}

// CountByParent counts direct replies to a post or comment
// Used for showing reply counts in threading UI
func (r *postgresCommentRepo) CountByParent(ctx context.Context, parentURI string) (int, error) {
	query := `
		SELECT COUNT(*)
		FROM comments
		WHERE parent_uri = $1 AND deleted_at IS NULL
	`

	var count int
	err := r.db.QueryRowContext(ctx, query, parentURI).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count comments by parent: %w", err)
	}

	return count, nil
}

// ListByCommenter retrieves all active comments by a specific user
// Future: Used for user comment history
func (r *postgresCommentRepo) ListByCommenter(ctx context.Context, commenterDID string, limit, offset int) ([]*comments.Comment, error) {
	query := `
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at,
			upvote_count, downvote_count, score, reply_count
		FROM comments
		WHERE commenter_did = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, commenterDID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list comments by commenter: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	var result []*comments.Comment
	for rows.Next() {
		var comment comments.Comment
		var langs pq.StringArray

		err := rows.Scan(
			&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
			&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
			&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		result = append(result, &comment)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating comments: %w", err)
	}

	return result, nil
}
// ListByParentWithHotRank retrieves direct replies to a post or comment with sorting and pagination
// Supports three sort modes: hot (Lemmy algorithm), top (by score + timeframe), and new (by created_at)
// Uses cursor-based pagination with composite keys for consistent ordering
// Hydrates author info (handle, display_name, avatar) via JOIN with users table
func (r *postgresCommentRepo) ListByParentWithHotRank(
	ctx context.Context,
	parentURI string,
	sort string,
	timeframe string,
	limit int,
	cursor *string,
) ([]*comments.Comment, *string, error) {
	// Build ORDER BY clause and time filter based on sort type
	orderBy, timeFilter := r.buildCommentSortClause(sort, timeframe)

	// Parse cursor for pagination
	cursorFilter, cursorValues, err := r.parseCommentCursor(cursor, sort)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid cursor: %w", err)
	}

	// Build SELECT clause - compute hot_rank for "hot" sort
	// Hot rank formula (Lemmy algorithm):
	// log(greatest(2, score + 2)) / power(((EXTRACT(EPOCH FROM (NOW() - created_at)) / 3600) + 2), 1.8)
	//
	// This formula:
	// - Gives logarithmic weight to score (prevents high-score dominance)
	// - Decays over time with power 1.8 (faster than linear, slower than quadratic)
	// - Uses hours as time unit (3600 seconds)
	// - Adds constants to prevent division by zero and ensure positive values
	var selectClause string
	if sort == "hot" {
		selectClause = `
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at,
			c.upvote_count, c.downvote_count, c.score, c.reply_count,
			log(greatest(2, c.score + 2)) / power(((EXTRACT(EPOCH FROM (NOW() - c.created_at)) / 3600) + 2), 1.8) as hot_rank,
			u.handle as author_handle
		FROM comments c`
	} else {
		selectClause = `
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at,
			c.upvote_count, c.downvote_count, c.score, c.reply_count,
			NULL::numeric as hot_rank,
			u.handle as author_handle
		FROM comments c`
	}

	// Build complete query with JOINs and filters
	query := fmt.Sprintf(`
		%s
		INNER JOIN users u ON c.commenter_did = u.did
		WHERE c.parent_uri = $1 AND c.deleted_at IS NULL
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, selectClause, timeFilter, cursorFilter, orderBy)

	// Prepare query arguments
	args := []interface{}{parentURI, limit + 1} // +1 to detect next page
	args = append(args, cursorValues...)

	// Execute query
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query comments with hot rank: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	// Scan results
	var result []*comments.Comment
	var hotRanks []float64
	for rows.Next() {
		var comment comments.Comment
		var langs pq.StringArray
		var hotRank sql.NullFloat64
		var authorHandle string

		err := rows.Scan(
			&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
			&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
			&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
			&hotRank, &authorHandle,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		comment.CommenterHandle = authorHandle

		// Store hot_rank for cursor building
		hotRankValue := 0.0
		if hotRank.Valid {
			hotRankValue = hotRank.Float64
		}
		hotRanks = append(hotRanks, hotRankValue)

		result = append(result, &comment)
	}

	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating comments: %w", err)
	}

	// Handle pagination cursor
	var nextCursor *string
	if len(result) > limit && limit > 0 {
		result = result[:limit]
		hotRanks = hotRanks[:limit]
		lastComment := result[len(result)-1]
		lastHotRank := hotRanks[len(hotRanks)-1]
		cursorStr := r.buildCommentCursor(lastComment, sort, lastHotRank)
		nextCursor = &cursorStr
	}

	return result, nextCursor, nil
}

// buildCommentSortClause returns the ORDER BY SQL and optional time filter
func (r *postgresCommentRepo) buildCommentSortClause(sort, timeframe string) (string, string) {
	var orderBy string
	switch sort {
	case "hot":
		// Hot rank DESC, then score DESC as tiebreaker, then created_at DESC, then uri DESC
		orderBy = `hot_rank DESC, c.score DESC, c.created_at DESC, c.uri DESC`
	case "top":
		// Score DESC, then created_at DESC, then uri DESC
		orderBy = `c.score DESC, c.created_at DESC, c.uri DESC`
	case "new":
		// Created at DESC, then uri DESC
		orderBy = `c.created_at DESC, c.uri DESC`
	default:
		// Default to hot
		orderBy = `hot_rank DESC, c.score DESC, c.created_at DESC, c.uri DESC`
	}

	// Add time filter for "top" sort
	var timeFilter string
	if sort == "top" {
		timeFilter = r.buildCommentTimeFilter(timeframe)
	}

	return orderBy, timeFilter
}

// buildCommentTimeFilter returns SQL filter for timeframe
func (r *postgresCommentRepo) buildCommentTimeFilter(timeframe string) string {
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
		interval = "7 days"
	case "month":
		interval = "30 days"
	case "year":
		interval = "1 year"
	default:
		return ""
	}

	return fmt.Sprintf("AND c.created_at >= NOW() - INTERVAL '%s'", interval)
}

// parseCommentCursor decodes pagination cursor for comments
func (r *postgresCommentRepo) parseCommentCursor(cursor *string, sort string) (string, []interface{}, error) {
	if cursor == nil || *cursor == "" {
		return "", nil, nil
	}

	// Decode base64 cursor
	decoded, err := base64.URLEncoding.DecodeString(*cursor)
	if err != nil {
		return "", nil, fmt.Errorf("invalid cursor encoding")
	}

	// Parse cursor based on sort type using | delimiter
	// Format: hotRank|score|createdAt|uri (for hot)
	//         score|createdAt|uri (for top)
	//         createdAt|uri (for new)
	parts := strings.Split(string(decoded), "|")

	switch sort {
	case "new":
		// Cursor format: createdAt|uri
		if len(parts) != 2 {
			return "", nil, fmt.Errorf("invalid cursor format for new sort")
		}

		createdAt := parts[0]
		uri := parts[1]

		// Validate AT-URI format
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("invalid cursor URI")
		}

		filter := `AND (c.created_at < $3 OR (c.created_at = $3 AND c.uri < $4))`
		return filter, []interface{}{createdAt, uri}, nil

	case "top":
		// Cursor format: score|createdAt|uri
		if len(parts) != 3 {
			return "", nil, fmt.Errorf("invalid cursor format for top sort")
		}

		scoreStr := parts[0]
		createdAt := parts[1]
		uri := parts[2]

		// Parse score as integer
		score := 0
		if _, err := fmt.Sscanf(scoreStr, "%d", &score); err != nil {
			return "", nil, fmt.Errorf("invalid cursor score")
		}

		// Validate AT-URI format
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("invalid cursor URI")
		}

		filter := `AND (c.score < $3 OR (c.score = $3 AND c.created_at < $4) OR (c.score = $3 AND c.created_at = $4 AND c.uri < $5))`
		return filter, []interface{}{score, createdAt, uri}, nil

	case "hot":
		// Cursor format: hotRank|score|createdAt|uri
		if len(parts) != 4 {
			return "", nil, fmt.Errorf("invalid cursor format for hot sort")
		}

		hotRankStr := parts[0]
		scoreStr := parts[1]
		createdAt := parts[2]
		uri := parts[3]

		// Parse hot_rank as float
		hotRank := 0.0
		if _, err := fmt.Sscanf(hotRankStr, "%f", &hotRank); err != nil {
			return "", nil, fmt.Errorf("invalid cursor hot rank")
		}

		// Parse score as integer
		score := 0
		if _, err := fmt.Sscanf(scoreStr, "%d", &score); err != nil {
			return "", nil, fmt.Errorf("invalid cursor score")
		}

		// Validate AT-URI format
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("invalid cursor URI")
		}

		// Use computed hot_rank expression in comparison
		hotRankExpr := `log(greatest(2, c.score + 2)) / power(((EXTRACT(EPOCH FROM (NOW() - c.created_at)) / 3600) + 2), 1.8)`
		filter := fmt.Sprintf(`AND ((%s < $3 OR (%s = $3 AND c.score < $4) OR (%s = $3 AND c.score = $4 AND c.created_at < $5) OR (%s = $3 AND c.score = $4 AND c.created_at = $5 AND c.uri < $6)) AND c.uri != $7)`,
			hotRankExpr, hotRankExpr, hotRankExpr, hotRankExpr)
		return filter, []interface{}{hotRank, score, createdAt, uri, uri}, nil

	default:
		return "", nil, nil
	}
}

// buildCommentCursor creates pagination cursor from last comment
func (r *postgresCommentRepo) buildCommentCursor(comment *comments.Comment, sort string, hotRank float64) string {
	var cursorStr string
	const delimiter = "|"

	switch sort {
	case "new":
		// Format: createdAt|uri
		cursorStr = fmt.Sprintf("%s%s%s",
			comment.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
			delimiter,
			comment.URI)

	case "top":
		// Format: score|createdAt|uri
		cursorStr = fmt.Sprintf("%d%s%s%s%s",
			comment.Score,
			delimiter,
			comment.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
			delimiter,
			comment.URI)

	case "hot":
		// Format: hotRank|score|createdAt|uri
		cursorStr = fmt.Sprintf("%f%s%d%s%s%s%s",
			hotRank,
			delimiter,
			comment.Score,
			delimiter,
			comment.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
			delimiter,
			comment.URI)

	default:
		cursorStr = comment.URI
	}

	return base64.URLEncoding.EncodeToString([]byte(cursorStr))
}

// GetByURIsBatch retrieves multiple comments by their AT-URIs in a single query
// Returns map[uri]*Comment for efficient lookups without N+1 queries
func (r *postgresCommentRepo) GetByURIsBatch(ctx context.Context, uris []string) (map[string]*comments.Comment, error) {
	if len(uris) == 0 {
		return make(map[string]*comments.Comment), nil
	}

	query := `
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at,
			c.upvote_count, c.downvote_count, c.score, c.reply_count,
			u.handle as author_handle
		FROM comments c
		INNER JOIN users u ON c.commenter_did = u.did
		WHERE c.uri = ANY($1) AND c.deleted_at IS NULL
	`

	rows, err := r.db.QueryContext(ctx, query, pq.Array(uris))
	if err != nil {
		return nil, fmt.Errorf("failed to batch get comments by URIs: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	result := make(map[string]*comments.Comment)
	for rows.Next() {
		var comment comments.Comment
		var langs pq.StringArray
		var authorHandle string

		err := rows.Scan(
			&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
			&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
			&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
			&authorHandle,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		result[comment.URI] = &comment
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating comments: %w", err)
	}

	return result, nil
}

// GetVoteStateForComments retrieves the viewer's votes on a batch of comments
// Returns map[commentURI]*Vote for efficient lookups
// Note: This implementation is prepared for when the votes table indexing is implemented
// Currently returns an empty map as votes may not be fully indexed yet
func (r *postgresCommentRepo) GetVoteStateForComments(ctx context.Context, viewerDID string, commentURIs []string) (map[string]interface{}, error) {
	if len(commentURIs) == 0 || viewerDID == "" {
		return make(map[string]interface{}), nil
	}

	// Query votes table for viewer's votes on these comments
	// Note: This assumes votes table exists and is being indexed
	// If votes table doesn't exist yet, this query will fail gracefully
	query := `
		SELECT subject_uri, direction, uri, cid, created_at
		FROM votes
		WHERE voter_did = $1 AND subject_uri = ANY($2) AND deleted_at IS NULL
	`

	rows, err := r.db.QueryContext(ctx, query, viewerDID, pq.Array(commentURIs))
	if err != nil {
		// If votes table doesn't exist yet, return empty map instead of error
		// This allows the API to work before votes indexing is fully implemented
		if strings.Contains(err.Error(), "does not exist") {
			return make(map[string]interface{}), nil
		}
		return nil, fmt.Errorf("failed to get vote state for comments: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	// Build result map with vote information
	result := make(map[string]interface{})
	for rows.Next() {
		var subjectURI, direction, uri, cid string
		var createdAt sql.NullTime

		err := rows.Scan(&subjectURI, &direction, &uri, &cid, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan vote: %w", err)
		}

		// Store vote info as a simple map (can be enhanced later with proper Vote struct)
		result[subjectURI] = map[string]interface{}{
			"direction": direction,
			"uri":       uri,
			"cid":       cid,
			"createdAt": createdAt.Time,
		}
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating votes: %w", err)
	}

	return result, nil
}
