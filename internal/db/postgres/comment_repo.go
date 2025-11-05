package postgres

import (
	"Coves/internal/core/comments"
	"context"
	"database/sql"
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
