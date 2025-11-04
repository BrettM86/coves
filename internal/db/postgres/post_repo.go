package postgres

import (
	"Coves/internal/core/posts"
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type postgresPostRepo struct {
	db *sql.DB
}

// NewPostRepository creates a new PostgreSQL post repository
func NewPostRepository(db *sql.DB) posts.Repository {
	return &postgresPostRepo{db: db}
}

// Create inserts a new post into the posts table
// Called by Jetstream consumer after post is created on PDS
func (r *postgresPostRepo) Create(ctx context.Context, post *posts.Post) error {
	// Serialize JSON fields for storage
	var facetsJSON, embedJSON sql.NullString

	if post.ContentFacets != nil {
		facetsJSON.String = *post.ContentFacets
		facetsJSON.Valid = true
	}

	if post.Embed != nil {
		embedJSON.String = *post.Embed
		embedJSON.Valid = true
	}

	// Store content labels as JSONB
	// post.ContentLabels contains com.atproto.label.defs#selfLabels JSON: {"values":[{"val":"nsfw","neg":false}]}
	// Store the full JSON blob to preserve the 'neg' field and future extensions
	var labelsJSON sql.NullString
	if post.ContentLabels != nil {
		labelsJSON.String = *post.ContentLabels
		labelsJSON.Valid = true
	}

	query := `
		INSERT INTO posts (
			uri, cid, rkey, author_did, community_did,
			title, content, content_facets, embed, content_labels,
			created_at, indexed_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, NOW()
		)
		RETURNING id, indexed_at
	`

	err := r.db.QueryRowContext(
		ctx, query,
		post.URI, post.CID, post.RKey, post.AuthorDID, post.CommunityDID,
		post.Title, post.Content, facetsJSON, embedJSON, labelsJSON,
		post.CreatedAt,
	).Scan(&post.ID, &post.IndexedAt)
	if err != nil {
		// Check for duplicate URI (post already indexed)
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "posts_uri_key") {
			return fmt.Errorf("post already indexed: %s", post.URI)
		}

		// Check for foreign key violations
		if strings.Contains(err.Error(), "violates foreign key constraint") {
			if strings.Contains(err.Error(), "fk_author") {
				return fmt.Errorf("author DID not found: %s", post.AuthorDID)
			}
			if strings.Contains(err.Error(), "fk_community") {
				return fmt.Errorf("community DID not found: %s", post.CommunityDID)
			}
		}

		return fmt.Errorf("failed to insert post: %w", err)
	}

	return nil
}

// GetByURI retrieves a post by its AT-URI
// Used for E2E test verification and future GET endpoint
func (r *postgresPostRepo) GetByURI(ctx context.Context, uri string) (*posts.Post, error) {
	query := `
		SELECT
			id, uri, cid, rkey, author_did, community_did,
			title, content, content_facets, embed, content_labels,
			created_at, edited_at, indexed_at, deleted_at,
			upvote_count, downvote_count, score, comment_count
		FROM posts
		WHERE uri = $1
	`

	var post posts.Post
	var facetsJSON, embedJSON, labelsJSON sql.NullString

	err := r.db.QueryRowContext(ctx, query, uri).Scan(
		&post.ID, &post.URI, &post.CID, &post.RKey,
		&post.AuthorDID, &post.CommunityDID,
		&post.Title, &post.Content, &facetsJSON, &embedJSON, &labelsJSON,
		&post.CreatedAt, &post.EditedAt, &post.IndexedAt, &post.DeletedAt,
		&post.UpvoteCount, &post.DownvoteCount, &post.Score, &post.CommentCount,
	)

	if err == sql.ErrNoRows {
		return nil, posts.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get post by URI: %w", err)
	}

	// Convert SQL types back to Go types
	if facetsJSON.Valid {
		post.ContentFacets = &facetsJSON.String
	}
	if embedJSON.Valid {
		post.Embed = &embedJSON.String
	}
	if labelsJSON.Valid {
		// Labels are stored as JSONB containing full com.atproto.label.defs#selfLabels structure
		post.ContentLabels = &labelsJSON.String
	}

	return &post, nil
}
