package postgres

import (
	"Coves/internal/core/comments"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
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
	if errors.Is(err, sql.ErrNoRows) {
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
// Preserves native vote counts and created_at timestamp.
//
// The consumer passes the INCOMING bridgedStats candidate in comment.Bridged* and
// comment.BridgedStatsAsOf (nil asOf => the record carried no applicable bridgedStats).
// The bridged columns are overwritten ATOMICALLY only when an incoming asOf is present
// and is newer-or-equal to the stored bridged_stats_as_of (NULL stored => first
// application). Doing the regression comparison inside this UPDATE (rather than
// read-check-write in the consumer) makes it race-free. score is always recomputed
// inclusive of native + whichever bridged counts win, so concurrent native votes are
// never clobbered. $10 is the incoming asOf; the stored asOf is read from the row.
func (r *postgresCommentRepo) Update(ctx context.Context, comment *comments.Comment) error {
	query := `
		UPDATE comments
		SET
			cid = $1,
			content = $2,
			content_facets = $3,
			embed = $4,
			content_labels = $5,
			langs = $6,
			bridged_upvote_count = CASE
				WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
				THEN $8 ELSE bridged_upvote_count END,
			bridged_downvote_count = CASE
				WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
				THEN $9 ELSE bridged_downvote_count END,
			bridged_stats_as_of = CASE
				WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
				THEN $10 ELSE bridged_stats_as_of END,
			score = upvote_count - downvote_count
				+ CASE
					WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
					THEN $8 ELSE bridged_upvote_count END
				- CASE
					WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
					THEN $9 ELSE bridged_downvote_count END
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
		comment.BridgedUpvoteCount,
		comment.BridgedDownvoteCount,
		comment.BridgedStatsAsOf,
	).Scan(
		&comment.ID,
		&comment.IndexedAt,
		&comment.CreatedAt,
		&comment.UpvoteCount,
		&comment.DownvoteCount,
		&comment.Score,
		&comment.ReplyCount,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return comments.ErrCommentNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to update comment: %w", err)
	}

	return nil
}

// GetByURI retrieves a comment by its AT-URI
//
// This is a DISPLAY read: like every other read path (ListByParent, ListByRoot,
// GetByURIsBatch, ...) it folds the bridge-asserted aggregates into the displayed
// upvote/downvote counts (upvote_count + bridged_upvote_count, etc.) so federated
// content shows the origin platform's votes. The Jetstream update path does NOT use
// this to run its regression guard — it issues a dedicated raw-columns query so it can
// reason about the separate native and bridged aggregates (see comment_consumer's
// updateComment); folding here would conflate them.
func (r *postgresCommentRepo) GetByURI(ctx context.Context, uri string) (*comments.Comment, error) {
	query := `
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at, deletion_reason, deleted_by,
			upvote_count + bridged_upvote_count AS upvote_count, downvote_count + bridged_downvote_count AS downvote_count, score, reply_count
		FROM comments
		WHERE uri = $1
	`

	var comment comments.Comment
	var langs pq.StringArray

	err := r.db.QueryRowContext(ctx, query, uri).Scan(
		&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
		&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
		&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
		&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
		&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
	)

	if errors.Is(err, sql.ErrNoRows) {
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
// Deprecated: Use SoftDeleteWithReason for new code to preserve thread structure
//
// KNOWN DEFECT (issue 2026-07-31-repo-minor-pins-batch.md, item 1): this sets deleted_at and blanks nothing,
// while every thread-shaped
// read below deliberately serves rows with deleted_at set. A comment removed through
// here keeps serving its full text, facets and embed to anonymous callers forever.
// No production caller remains (the consumer uses SoftDeleteWithReasonTx), but the
// method is still on the exported Repository interface.
// (see TestCommentRepo_DeleteLeavesTheTextReadable)
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

// SoftDeleteWithReason performs a soft delete that blanks content but preserves thread structure
// This allows deleted comments to appear as "[deleted]" placeholders in thread views
// Idempotent: Returns success if comment already deleted
// Validates that reason is a known deletion reason constant
func (r *postgresCommentRepo) SoftDeleteWithReason(ctx context.Context, uri, reason, deletedByDID string) error {
	// Validate deletion reason
	if reason != comments.DeletionReasonAuthor && reason != comments.DeletionReasonModerator {
		return fmt.Errorf("invalid deletion reason: %s", reason)
	}

	_, err := r.SoftDeleteWithReasonTx(ctx, nil, uri, reason, deletedByDID)
	return err
}

// SoftDeleteWithReasonTx performs a soft delete within an optional transaction
// If tx is nil, executes directly against the database
// Returns rows affected count for callers that need to check idempotency
// This method is used by both the repository and the Jetstream consumer
func (r *postgresCommentRepo) SoftDeleteWithReasonTx(ctx context.Context, tx *sql.Tx, uri, reason, deletedByDID string) (int64, error) {
	query := `
		UPDATE comments
		SET
			content = '',
			content_facets = NULL,
			embed = NULL,
			content_labels = NULL,
			deleted_at = NOW(),
			deletion_reason = $2,
			deleted_by = $3
		WHERE uri = $1 AND deleted_at IS NULL
	`

	var result sql.Result
	var err error

	if tx != nil {
		result, err = tx.ExecContext(ctx, query, uri, reason, deletedByDID)
	} else {
		result, err = r.db.ExecContext(ctx, query, uri, reason, deletedByDID)
	}

	if err != nil {
		return 0, fmt.Errorf("failed to soft delete comment: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to check delete result: %w", err)
	}

	return rowsAffected, nil
}

// ListByRoot retrieves all comments in a thread (flat), including deleted ones
// Used for fetching entire comment threads on posts
// Includes deleted comments to preserve thread structure (shown as "[deleted]" placeholders)
func (r *postgresCommentRepo) ListByRoot(ctx context.Context, rootURI string, limit, offset int) ([]*comments.Comment, error) {
	query := `
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at, deletion_reason, deleted_by,
			upvote_count + bridged_upvote_count AS upvote_count, downvote_count + bridged_downvote_count AS downvote_count, score, reply_count
		FROM comments
		WHERE root_uri = $1
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
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
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

// ListByParent retrieves direct replies to a post or comment, including deleted ones
// Used for building nested/threaded comment views
// Includes deleted comments to preserve thread structure (shown as "[deleted]" placeholders)
func (r *postgresCommentRepo) ListByParent(ctx context.Context, parentURI string, limit, offset int) ([]*comments.Comment, error) {
	query := `
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at, deletion_reason, deleted_by,
			upvote_count + bridged_upvote_count AS upvote_count, downvote_count + bridged_downvote_count AS downvote_count, score, reply_count
		FROM comments
		WHERE parent_uri = $1
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
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
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
// NOTE: Includes deleted comments since they're shown as "[deleted]" placeholders
func (r *postgresCommentRepo) CountByParent(ctx context.Context, parentURI string) (int, error) {
	query := `
		SELECT COUNT(*)
		FROM comments
		WHERE parent_uri = $1
	`

	var count int
	err := r.db.QueryRowContext(ctx, query, parentURI).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count comments by parent: %w", err)
	}

	return count, nil
}

// ListByCommenter retrieves all active comments by a specific user
// Used for user comment history - filters out deleted comments
func (r *postgresCommentRepo) ListByCommenter(ctx context.Context, commenterDID string, limit, offset int) ([]*comments.Comment, error) {
	query := `
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at, deletion_reason, deleted_by,
			upvote_count + bridged_upvote_count AS upvote_count, downvote_count + bridged_downvote_count AS downvote_count, score, reply_count
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
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
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

// ListByCommenterWithCursor retrieves comments by a user with cursor-based pagination
// Used for user profile comment history (social.coves.actor.getComments)
// Supports optional community filtering and returns next page cursor
// Uses chronological ordering (newest first) with composite key cursor for stable pagination
func (r *postgresCommentRepo) ListByCommenterWithCursor(ctx context.Context, req comments.ListByCommenterRequest) ([]*comments.Comment, *string, error) {
	// Parse cursor for pagination
	cursorFilter, cursorValues, err := r.parseCommenterCursor(req.Cursor)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid cursor: %w", err)
	}

	// Build community filter if provided
	// Parameter numbering: $1=commenterDID, $2=limit+1 (for pagination detection)
	// Cursor values (if present) use $3 and $4, then the community DID and the
	// viewer DID the visibility predicate binds.
	//
	// SECURITY (PRD §6.2 — "comment community filters" are in the must-convert
	// inventory): naming a community in this query is a claim about that
	// community's scope, so the roots it selects must be the posts that community
	// actually ADMITTED. Under author-owned posts anyone can write a postv2 naming
	// any community, and the comment consumer indexes a comment whatever its root
	// is — so a bare `community_did = $n` subquery lets an attacker comment on
	// their own never-accepted post and have the comment served back, in full,
	// inside a listing clients render as "this user's comments in <community>".
	// That is the removed write barrier being rebuilt one table over.
	//
	// The subquery therefore runs the SAME centralized predicate every other posts
	// read path runs (visiblePostsJoin) plus the soft-delete gate, with the
	// viewer's DID bound so the author keeps the carve-out over their own
	// pending/rejected/removed posts and nobody else gains it. Reusing the helper
	// rather than hand-rolling the status rule is the point: a second copy of the
	// predicate is a second thing to forget to update.
	var communityFilter string
	var communityValue []interface{}
	paramOffset := 2 + len(cursorValues) // Start after $1, $2, and any cursor params
	if req.CommunityDID != nil && *req.CommunityDID != "" {
		paramOffset++
		communityParam := paramOffset
		paramOffset++
		viewerParam := paramOffset

		visJoin, visWhere := visiblePostsJoin(viewerParam)
		communityFilter = fmt.Sprintf(`AND c.root_uri IN (
				SELECT p.uri
				FROM posts p%s
				WHERE p.community_did = $%d
					AND p.deleted_at IS NULL
					AND %s
			)`, visJoin, communityParam, visWhere)
		communityValue = append(communityValue, *req.CommunityDID, req.ViewerDID)
	}

	// Build complete query with JOINs and filters
	// LEFT JOIN prevents data loss when user record hasn't been indexed yet
	query := fmt.Sprintf(`
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			COALESCE(u.handle, c.commenter_did) as author_handle
		FROM comments c
		LEFT JOIN users u ON c.commenter_did = u.did
		WHERE c.commenter_did = $1
			AND c.deleted_at IS NULL
			%s
			%s
		ORDER BY c.created_at DESC, c.uri DESC
		LIMIT $2
	`, communityFilter, cursorFilter)

	// Prepare query arguments
	args := []interface{}{req.CommenterDID, req.Limit + 1} // +1 to detect next page
	args = append(args, cursorValues...)
	args = append(args, communityValue...)

	// Execute query
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query comments by commenter: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	// Scan results
	var result []*comments.Comment
	for rows.Next() {
		var comment comments.Comment
		var langs pq.StringArray
		var authorHandle string

		err := rows.Scan(
			&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
			&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
			&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
			&authorHandle,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		comment.CommenterHandle = authorHandle
		result = append(result, &comment)
	}

	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating comments: %w", err)
	}

	// Handle pagination cursor
	var nextCursor *string
	if len(result) > req.Limit && req.Limit > 0 {
		result = result[:req.Limit]
		lastComment := result[len(result)-1]
		cursorStr := r.buildCommenterCursor(lastComment)
		nextCursor = &cursorStr
	}

	return result, nextCursor, nil
}

// parseCommenterCursor decodes pagination cursor for commenter comments
// Cursor format: createdAt|uri (same as "new" sort for other comment queries)
//
// IMPORTANT: This function returns a filter string with hardcoded parameter numbers ($3, $4).
// The caller (ListByCommenterWithCursor) must ensure parameters are ordered as:
// $1=commenterDID, $2=limit+1, $3=createdAt, $4=uri, then community DID if present.
// If you modify the parameter order in the caller, you must update the filter here.
//
// KNOWN DEFECT (issue 2026-07-31-repo-minor-pins-batch.md, item 3): unlike parseCommentCursor below, none of
// these failures is wrapped in
// comments.ErrInvalidCursor, so a malformed cursor reaches
// actor/get_comments.go:handleCommentServiceError unclassifiable and answers 500
// instead of 400. (see TestCommentRepo_CommenterCursorErrorsAreNotClassifiable)
func (r *postgresCommentRepo) parseCommenterCursor(cursor *string) (string, []interface{}, error) {
	if cursor == nil || *cursor == "" {
		return "", nil, nil
	}

	// Validate cursor size to prevent DoS via massive base64 strings
	const maxCursorSize = 1024
	if len(*cursor) > maxCursorSize {
		return "", nil, fmt.Errorf("cursor too large: maximum %d bytes", maxCursorSize)
	}

	// Decode base64 cursor
	decoded, err := base64.URLEncoding.DecodeString(*cursor)
	if err != nil {
		return "", nil, fmt.Errorf("invalid cursor encoding")
	}

	// Parse cursor: createdAt|uri
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 2 {
		return "", nil, fmt.Errorf("invalid cursor format")
	}

	createdAt := parts[0]
	uri := parts[1]

	// Validate AT-URI format
	if !strings.HasPrefix(uri, "at://") {
		return "", nil, fmt.Errorf("invalid cursor URI")
	}

	filter := `AND (c.created_at < $3 OR (c.created_at = $3 AND c.uri < $4))`
	return filter, []interface{}{createdAt, uri}, nil
}

// buildCommenterCursor creates pagination cursor from last comment
// Uses createdAt|uri format for stable pagination
func (r *postgresCommentRepo) buildCommenterCursor(comment *comments.Comment) string {
	cursorStr := fmt.Sprintf("%s|%s",
		comment.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
		comment.URI)
	return base64.URLEncoding.EncodeToString([]byte(cursorStr))
}

// commentHotRankSQL builds the comment hot-rank SQL expression for a comments-table
// alias and a "now" expression.
//
// Without the age clamp, a comment more than two hours in the future makes the
// POWER base negative. PostgreSQL then errors with "a negative number raised to
// a non-integer power yields a complex result", aborting the whole thread query.
// GREATEST(age, 0) keeps the POWER base >= 2 and makes a future-dated comment rank
// exactly like a brand-new one instead of gaining a boost.
func commentHotRankSQL(alias, nowExpr string) string {
	return fmt.Sprintf(
		`LOG(GREATEST(2, %[1]s.score + 2)) / POWER(GREATEST(EXTRACT(EPOCH FROM (%[2]s - %[1]s.created_at))/3600, 0) + 2, 1.8)`,
		alias, nowExpr)
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
	viewerDID string,
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
	// LOG(GREATEST(2, score + 2)) / POWER(GREATEST(age_hours, 0) + 2, 1.8)
	//
	// This formula:
	// - Gives logarithmic weight to score (prevents high-score dominance)
	// - Decays over time with power 1.8 (faster than linear, slower than quadratic)
	// - Uses hours as time unit (3600 seconds)
	// - Clamps negative ages so future-dated comments cannot gain a ranking boost
	var selectClause string
	if sort == "hot" {
		selectClause = fmt.Sprintf(`
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			%s as hot_rank,
			COALESCE(u.handle, c.commenter_did) as author_handle
		FROM comments c`, commentHotRankSQL("c", "NOW()"))
	} else {
		selectClause = `
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			NULL::numeric as hot_rank,
			COALESCE(u.handle, c.commenter_did) as author_handle
		FROM comments c`
	}

	// Build optional viewer block filter (only when authenticated viewer is present)
	var viewerFilter string
	var viewerArgs []interface{}
	if viewerDID != "" {
		viewerParamIdx := 3 + len(cursorValues)
		viewerFilter = fmt.Sprintf("AND NOT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_did = $%d AND blocked_did = c.commenter_did)", viewerParamIdx)
		viewerArgs = append(viewerArgs, viewerDID)
	}

	// Build complete query with JOINs and filters
	// LEFT JOIN prevents data loss when user record hasn't been indexed yet (out-of-order Jetstream events)
	// Includes deleted comments to preserve thread structure (shown as "[deleted]" placeholders)
	query := fmt.Sprintf(`
		%s
		LEFT JOIN users u ON c.commenter_did = u.did
		WHERE c.parent_uri = $1
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, selectClause, timeFilter, cursorFilter, viewerFilter, orderBy)

	// Prepare query arguments
	args := []interface{}{parentURI, limit + 1} // +1 to detect next page
	args = append(args, cursorValues...)
	args = append(args, viewerArgs...)

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
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
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
		//
		// KNOWN DEFECT (issue 2026-07-31-comment-repo-unrecognised-sort-trio.md):
		// ListByParentWithHotRank only COMPUTES hot_rank when sort == "hot" exactly, so under
		// any other unrecognised value this leading key is NULL for every row and the ordering
		// collapses to score DESC — i.e. "top". ListByParentsBatch's own default arm does
		// compute the rank, so the two halves of one thread view disagree.
		// REACHABILITY: masked today — the sort allow-list in comment_service.go's
		// validate/list request path rejects any sort outside {hot, top, new} before the
		// repository is reached. Adding a fourth sort without updating both arms here
		// makes it live.
		// (see TestCommentRepo_UnknownSortRanksLikeTopNotLikeHot)
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
// All parse failures are wrapped with comments.ErrInvalidCursor so callers can
// surface them as client input errors (HTTP 400) instead of server faults.
func (r *postgresCommentRepo) parseCommentCursor(cursor *string, sort string) (string, []interface{}, error) {
	if cursor == nil || *cursor == "" {
		return "", nil, nil
	}

	// Validate cursor size to prevent DoS via massive base64 strings
	const maxCursorSize = 1024
	if len(*cursor) > maxCursorSize {
		return "", nil, fmt.Errorf("%w: cursor too large: maximum %d bytes", comments.ErrInvalidCursor, maxCursorSize)
	}

	// Decode base64 cursor
	decoded, err := base64.URLEncoding.DecodeString(*cursor)
	if err != nil {
		return "", nil, fmt.Errorf("%w: invalid base64 encoding", comments.ErrInvalidCursor)
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
			return "", nil, fmt.Errorf("%w: invalid cursor format for new sort", comments.ErrInvalidCursor)
		}

		createdAt := parts[0]
		uri := parts[1]

		// Validate AT-URI format
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("%w: invalid cursor URI", comments.ErrInvalidCursor)
		}

		filter := `AND (c.created_at < $3 OR (c.created_at = $3 AND c.uri < $4))`
		return filter, []interface{}{createdAt, uri}, nil

	case "top":
		// Cursor format: score|createdAt|uri
		if len(parts) != 3 {
			return "", nil, fmt.Errorf("%w: invalid cursor format for top sort", comments.ErrInvalidCursor)
		}

		scoreStr := parts[0]
		createdAt := parts[1]
		uri := parts[2]

		// Parse score as integer
		score := 0
		if _, err := fmt.Sscanf(scoreStr, "%d", &score); err != nil {
			return "", nil, fmt.Errorf("%w: invalid cursor score", comments.ErrInvalidCursor)
		}

		// Validate AT-URI format
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("%w: invalid cursor URI", comments.ErrInvalidCursor)
		}

		filter := `AND (c.score < $3 OR (c.score = $3 AND c.created_at < $4) OR (c.score = $3 AND c.created_at = $4 AND c.uri < $5))`
		return filter, []interface{}{score, createdAt, uri}, nil

	case "hot":
		// Cursor format: hotRank|score|createdAt|uri
		if len(parts) != 4 {
			return "", nil, fmt.Errorf("%w: invalid cursor format for hot sort", comments.ErrInvalidCursor)
		}

		hotRankStr := parts[0]
		scoreStr := parts[1]
		createdAt := parts[2]
		uri := parts[3]

		// Parse hot_rank as float
		hotRank := 0.0
		if _, err := fmt.Sscanf(hotRankStr, "%f", &hotRank); err != nil {
			return "", nil, fmt.Errorf("%w: invalid cursor hot rank", comments.ErrInvalidCursor)
		}

		// Parse score as integer
		score := 0
		if _, err := fmt.Sscanf(scoreStr, "%d", &score); err != nil {
			return "", nil, fmt.Errorf("%w: invalid cursor score", comments.ErrInvalidCursor)
		}

		// Validate AT-URI format
		if !strings.HasPrefix(uri, "at://") {
			return "", nil, fmt.Errorf("%w: invalid cursor URI", comments.ErrInvalidCursor)
		}

		// Use computed hot_rank expression in comparison
		hotRankExpr := commentHotRankSQL("c", "NOW()")
		filter := fmt.Sprintf(`AND ((%s < $3 OR (%s = $3 AND c.score < $4) OR (%s = $3 AND c.score = $4 AND c.created_at < $5) OR (%s = $3 AND c.score = $4 AND c.created_at = $5 AND c.uri < $6)) AND c.uri != $7)`,
			hotRankExpr, hotRankExpr, hotRankExpr, hotRankExpr)
		return filter, []interface{}{hotRank, score, createdAt, uri, uri}, nil

	default:
		// KNOWN DEFECT (issue 2026-07-31-comment-repo-unrecognised-sort-trio.md): an
		// unrecognised sort discards the cursor with no error, while buildCommentSortClause
		// happily produces an ORDER BY for the same value. Page two is therefore page one,
		// forever.
		// REACHABILITY: masked today — the sort allow-list in comment_service.go's
		// validate/list request path rejects any sort outside {hot, top, new} before the
		// repository is reached. Adding a fourth sort without updating both arms here
		// makes it live.
		// (see TestCommentRepo_UnknownSortCursorIsSilentlyDiscarded and
		// TestCommentRepo_UnknownSortCursorRepeatsPageOne)
		return "", nil, nil
	}
}

// KNOWN DEFECT (issue 2026-07-31-hot-comment-cursor-truncated-to-six-decimals.md): the
// %f verb below writes the hot rank to six decimal places, and that loses rows two ways.
//
// TOTAL LOSS, on old threads. At score 0, the rank from commentHotRankSQL crosses
// 1e-6 at ~46 days and rounds to "0.000000" at ~68 days; at higher scores,
// rounding to zero occurs around ~120 days (score 5) to ~194 days (score 100).
// Once the cursor reads "0.000000" the
// filter above asks for a rank strictly below zero, no row qualifies, and pagination
// returns page one and then stops, silently. (Not "within a week" — at seven days the
// rank is ~2.9e-5, about 29x above the floor.)
//
// PARTIAL LOSS, at ANY age. %f rounds to nearest, so the stored boundary is usually not
// the boundary row's true rank. When it rounds DOWN, every row whose true rank lies in
// [rounded, true) is excluded by the strict `<` even though it belongs on the next page
// — silently dropped from the thread at any age, not just old ones. When it rounds UP,
// rows in [true, rounded) are served a second time (the `c.uri != $7` guard excludes
// only the boundary row itself).
// (see TestCommentRepo_HotCursorLosesEveryRowAfterPageOne)
//
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

// GetByRootAndRkey retrieves a comment within a thread (root post) by its record key
// Used to resolve the parentRkey parameter of getComments (comment permalinks)
// Includes deleted comments so a deleted parent still renders as a "[deleted]" placeholder
// with its children preserved.
//
// viewerDID is optional — when non-empty, a comment whose author the viewer has blocked
// is filtered out and reported as ErrCommentNotFound, matching the block filtering
// applied by ListByParentWithHotRank and ListByParentsBatch.
//
// rkeys are TIDs, so a collision between two commenters within one post is astronomically
// unlikely. To stay deterministic if it happens, the earliest indexed comment wins
// (indexed_at ASC, id ASC) and a warning is logged.
func (r *postgresCommentRepo) GetByRootAndRkey(ctx context.Context, rootURI, rkey, viewerDID string) (*comments.Comment, error) {
	// Build optional viewer block filter (only when authenticated viewer is present)
	var viewerFilter string
	args := []interface{}{rootURI, rkey}
	if viewerDID != "" {
		viewerFilter = "AND NOT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_did = $3 AND blocked_did = c.commenter_did)"
		args = append(args, viewerDID)
	}

	// LEFT JOIN prevents data loss when user record hasn't been indexed yet
	// COALESCE falls back to DID when handle is NULL (user not yet in users table)
	// LIMIT 2 lets us detect (and log) rkey collisions without fetching everything
	query := fmt.Sprintf(`
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			COALESCE(u.handle, c.commenter_did) as author_handle
		FROM comments c
		LEFT JOIN users u ON c.commenter_did = u.did
		WHERE c.root_uri = $1 AND c.rkey = $2
			%s
		ORDER BY c.indexed_at ASC, c.id ASC
		LIMIT 2
	`, viewerFilter)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get comment by root and rkey: %w", err)
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
		var authorHandle string

		err := rows.Scan(
			&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
			&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
			&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
			&authorHandle,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		comment.CommenterHandle = authorHandle
		result = append(result, &comment)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating comments: %w", err)
	}

	if len(result) == 0 {
		return nil, comments.ErrCommentNotFound
	}

	if len(result) > 1 {
		log.Printf("WARN: rkey collision within thread %s: rkey %s matches %s and %s; using earliest indexed",
			rootURI, rkey, result[0].URI, result[1].URI)
	}

	return result[0], nil
}

// GetByURIsBatch retrieves multiple comments by their AT-URIs in a single query
// Returns map[uri]*Comment for efficient lookups without N+1 queries
// Includes deleted comments to preserve thread structure
func (r *postgresCommentRepo) GetByURIsBatch(ctx context.Context, uris []string) (map[string]*comments.Comment, error) {
	if len(uris) == 0 {
		return make(map[string]*comments.Comment), nil
	}

	// LEFT JOIN prevents data loss when user record hasn't been indexed yet (out-of-order Jetstream events)
	// COALESCE falls back to DID when handle is NULL (user not yet in users table)
	// Includes deleted comments to preserve thread structure (shown as "[deleted]" placeholders)
	query := `
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			COALESCE(u.handle, c.commenter_did) as author_handle
		FROM comments c
		LEFT JOIN users u ON c.commenter_did = u.did
		WHERE c.uri = ANY($1)
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
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
			&authorHandle,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		// KNOWN DEFECT (issue 2026-07-31-repo-minor-pins-batch.md, item 2): author_handle is scanned and then dropped
		// — GetByRootAndRkey and
		// ListByParentsBatch both assign it to comment.CommenterHandle here. Callers of this
		// method get the empty string and the LEFT JOIN is paid for nothing.
		// (see TestCommentRepo_GetByURIsBatchDropsTheAuthorHandle)
		result[comment.URI] = &comment
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating comments: %w", err)
	}

	return result, nil
}

// ListByParentsBatch retrieves direct replies to multiple parents in a single query
// Groups results by parent URI to prevent N+1 queries when loading nested replies
// Uses window functions to limit results per parent efficiently
func (r *postgresCommentRepo) ListByParentsBatch(
	ctx context.Context,
	parentURIs []string,
	sort string,
	limitPerParent int,
	viewerDID string,
) (map[string][]*comments.Comment, error) {
	if len(parentURIs) == 0 {
		return make(map[string][]*comments.Comment), nil
	}

	// Build ORDER BY clause based on sort type
	// windowOrderBy must inline expressions (can't use SELECT aliases in window functions)
	var windowOrderBy string
	var selectClause string
	switch sort {
	case "top":
		selectClause = `
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			NULL::numeric as hot_rank,
			COALESCE(u.handle, c.commenter_did) as author_handle`
		windowOrderBy = `c.score DESC, c.created_at DESC`
	case "new":
		selectClause = `
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			NULL::numeric as hot_rank,
			COALESCE(u.handle, c.commenter_did) as author_handle`
		windowOrderBy = `c.created_at DESC`
	default:
		// "hot", and deliberately any unrecognised sort — see the KNOWN DEFECT note in buildCommentSortClause
		selectClause = fmt.Sprintf(`
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			%s as hot_rank,
			COALESCE(u.handle, c.commenter_did) as author_handle`, commentHotRankSQL("c", "NOW()"))
		// CRITICAL: Must inline hot_rank formula - PostgreSQL doesn't allow SELECT aliases in window ORDER BY
		windowOrderBy = commentHotRankSQL("c", "NOW()") + ` DESC, c.score DESC, c.created_at DESC`
	}

	// Build optional viewer block filter (only when authenticated viewer is present)
	// Parameter index computed dynamically: $1=parentURIs, $2=limitPerParent, $3+=viewer
	var viewerFilter string
	var viewerArgs []interface{}
	if viewerDID != "" {
		viewerParamIdx := 3 // after $1=parentURIs and $2=limitPerParent
		viewerFilter = fmt.Sprintf("AND NOT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_did = $%d AND blocked_did = c.commenter_did)", viewerParamIdx)
		viewerArgs = append(viewerArgs, viewerDID)
	}

	// Use window function to limit results per parent
	// This is more efficient than LIMIT in a subquery per parent
	// LEFT JOIN prevents data loss when user record hasn't been indexed yet (out-of-order Jetstream events)
	// Includes deleted comments to preserve thread structure (shown as "[deleted]" placeholders)
	query := fmt.Sprintf(`
		WITH ranked_comments AS (
			SELECT
				%s,
				ROW_NUMBER() OVER (
					PARTITION BY c.parent_uri
					ORDER BY %s
				) as rn
			FROM comments c
			LEFT JOIN users u ON c.commenter_did = u.did
			WHERE c.parent_uri = ANY($1)
				%s
		)
		SELECT
			id, uri, cid, rkey, commenter_did,
			root_uri, root_cid, parent_uri, parent_cid,
			content, content_facets, embed, content_labels, langs,
			created_at, indexed_at, deleted_at, deletion_reason, deleted_by,
			upvote_count, downvote_count, score, reply_count,
			hot_rank, author_handle
		FROM ranked_comments
		WHERE rn <= $2
		ORDER BY parent_uri, rn
	`, selectClause, windowOrderBy, viewerFilter)

	queryArgs := []interface{}{pq.Array(parentURIs), limitPerParent}
	queryArgs = append(queryArgs, viewerArgs...)
	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to batch query comments by parents: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	// Group results by parent URI
	result := make(map[string][]*comments.Comment)
	for rows.Next() {
		var comment comments.Comment
		var langs pq.StringArray
		var hotRank sql.NullFloat64
		var authorHandle string

		err := rows.Scan(
			&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
			&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
			&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
			&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy,
			&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
			&hotRank, &authorHandle,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan comment: %w", err)
		}

		comment.Langs = langs
		comment.CommenterHandle = authorHandle

		// Group by parent URI
		result[comment.ParentURI] = append(result[comment.ParentURI], &comment)
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
		SELECT subject_uri, direction, uri
		FROM votes
		WHERE voter_did = $1 AND subject_uri = ANY($2) AND deleted_at IS NULL
	`

	rows, err := r.db.QueryContext(ctx, query, viewerDID, pq.Array(commentURIs))
	if err != nil {
		// If votes table doesn't exist yet, return empty map instead of error
		// This allows the API to work before votes indexing is fully implemented
		if strings.Contains(err.Error(), "does not exist") {
			log.Printf("WARN: Votes table does not exist, returning empty vote state for %d comments", len(commentURIs))
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
		var subjectURI, direction, uri string

		err := rows.Scan(&subjectURI, &direction, &uri)
		if err != nil {
			return nil, fmt.Errorf("failed to scan vote: %w", err)
		}

		// Store vote info as a simple map (can be enhanced later with proper Vote struct)
		result[subjectURI] = map[string]interface{}{
			"direction": direction,
			"uri":       uri,
		}
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating votes: %w", err)
	}

	return result, nil
}
