package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/posts"
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

// GetByAuthor retrieves posts by author with filtering and pagination
// Supports filter options: posts_with_replies (default), posts_no_replies, posts_with_media
// Uses cursor-based pagination with created_at + uri for stable ordering
// Returns []*PostView, next cursor, and error
func (r *postgresPostRepo) GetByAuthor(ctx context.Context, req posts.GetAuthorPostsRequest) ([]*posts.PostView, *string, error) {
	// Build WHERE clauses based on filters
	whereConditions := []string{
		"p.author_did = $1",
		"p.deleted_at IS NULL",
	}
	args := []interface{}{req.ActorDID}
	paramIndex := 2

	// Optional community filter
	if req.Community != "" {
		whereConditions = append(whereConditions, fmt.Sprintf("p.community_did = $%d", paramIndex))
		args = append(args, req.Community)
		paramIndex++
	}

	// Filter by post type
	// Design note: Coves architecture separates posts from comments (unlike Bluesky where
	// posts can be replies to other posts). The posts_no_replies filter exists for API
	// compatibility with Bluesky's getAuthorFeed, but is intentionally a no-op in Coves
	// since all Coves posts are top-level (comments are stored in a separate table).
	switch req.Filter {
	case posts.FilterPostsWithMedia:
		whereConditions = append(whereConditions, "p.embed IS NOT NULL")
	case posts.FilterPostsNoReplies:
		// No-op: All Coves posts are top-level; comments are in the comments table.
		// This filter exists for Bluesky API compatibility.
	case posts.FilterPostsWithReplies, "":
		// Default: return all posts (no additional filter needed)
	}

	// Build cursor filter for pagination
	cursorFilter, cursorArgs, cursorErr := r.parseAuthorPostsCursor(req.Cursor, paramIndex)
	if cursorErr != nil {
		return nil, nil, cursorErr
	}
	if cursorFilter != "" {
		whereConditions = append(whereConditions, cursorFilter)
		args = append(args, cursorArgs...)
		paramIndex += len(cursorArgs)
	}

	// Add limit to args
	limit := req.Limit
	if limit <= 0 {
		limit = 50 // default
	}
	if limit > 100 {
		limit = 100 // max
	}
	args = append(args, limit+1) // +1 to check for next page

	whereClause := strings.Join(whereConditions, " AND ")

	query := fmt.Sprintf(`
		SELECT
			p.uri, p.cid, p.rkey,
			p.author_did, u.handle as author_handle,
			p.community_did, c.handle as community_handle, c.name as community_name, c.avatar_cid as community_avatar, c.pds_url as community_pds_url,
			p.title, p.content, p.content_facets, p.embed, p.content_labels,
			p.created_at, p.edited_at, p.indexed_at,
			p.upvote_count, p.downvote_count, p.score, p.comment_count
		FROM posts p
		INNER JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did
		WHERE %s
		ORDER BY p.created_at DESC, p.uri DESC
		LIMIT $%d
	`, whereClause, paramIndex)

	// Execute query
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query author posts: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("WARN: failed to close rows: %v", err)
		}
	}()

	// Scan results
	var postViews []*posts.PostView
	for rows.Next() {
		postView, err := r.scanAuthorPost(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to scan author post: %w", err)
		}
		postViews = append(postViews, postView)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating author posts results: %w", err)
	}

	// Handle pagination cursor
	var cursor *string
	if len(postViews) > limit && limit > 0 {
		postViews = postViews[:limit]
		lastPost := postViews[len(postViews)-1]
		cursorStr := r.buildAuthorPostsCursor(lastPost)
		cursor = &cursorStr
	}

	return postViews, cursor, nil
}

// parseAuthorPostsCursor decodes pagination cursor for author posts
// Cursor format: base64(created_at|uri)
// Uses simple | delimiter since this is an internal cursor (not signed like feed cursors)
// Returns filter clause, arguments, and error. Error is returned for malformed cursors
// to provide clear feedback rather than silently returning the first page.
func (r *postgresPostRepo) parseAuthorPostsCursor(cursor *string, paramOffset int) (string, []interface{}, error) {
	if cursor == nil || *cursor == "" {
		return "", nil, nil
	}

	// Validate cursor size to prevent DoS via massive base64 strings
	const maxCursorSize = 512
	if len(*cursor) > maxCursorSize {
		return "", nil, fmt.Errorf("%w: cursor exceeds maximum length", posts.ErrInvalidCursor)
	}

	// Decode base64 cursor
	decoded, err := base64.URLEncoding.DecodeString(*cursor)
	if err != nil {
		return "", nil, fmt.Errorf("%w: invalid base64 encoding", posts.ErrInvalidCursor)
	}

	// Parse cursor: created_at|uri
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 2 {
		return "", nil, fmt.Errorf("%w: malformed cursor format", posts.ErrInvalidCursor)
	}

	createdAt := parts[0]
	uri := parts[1]

	// Validate timestamp format
	if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return "", nil, fmt.Errorf("%w: invalid timestamp in cursor", posts.ErrInvalidCursor)
	}

	// Validate URI format (must be AT-URI)
	if !strings.HasPrefix(uri, "at://") {
		return "", nil, fmt.Errorf("%w: invalid URI format in cursor", posts.ErrInvalidCursor)
	}

	// Use composite key comparison for stable cursor pagination
	// (created_at, uri) < (cursor_created_at, cursor_uri)
	filter := fmt.Sprintf("(p.created_at < $%d OR (p.created_at = $%d AND p.uri < $%d))",
		paramOffset, paramOffset, paramOffset+1)
	return filter, []interface{}{createdAt, uri}, nil
}

// buildAuthorPostsCursor creates pagination cursor from last post
// Cursor format: base64(created_at|uri)
func (r *postgresPostRepo) buildAuthorPostsCursor(post *posts.PostView) string {
	cursorStr := fmt.Sprintf("%s|%s", post.CreatedAt.Format(time.RFC3339Nano), post.URI)
	return base64.URLEncoding.EncodeToString([]byte(cursorStr))
}

// scanAuthorPost scans a database row into a PostView for author posts query
func (r *postgresPostRepo) scanAuthorPost(rows *sql.Rows) (*posts.PostView, error) {
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
	)

	err := rows.Scan(
		&postView.URI, &postView.CID, &postView.RKey,
		&authorView.DID, &authorView.Handle,
		&communityRef.DID, &communityHandle, &communityRef.Name, &communityAvatar, &communityPDSURL,
		&title, &content, &facets, &embed, &labelsJSON,
		&postView.CreatedAt, &editedAt, &postView.IndexedAt,
		&postView.UpvoteCount, &postView.DownvoteCount, &postView.Score, &postView.CommentCount,
	)
	if err != nil {
		return nil, err
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
	if title.Valid {
		postView.Title = &title.String
	}
	if content.Valid {
		postView.Text = &content.String
	}
	if editedAt.Valid {
		postView.EditedAt = &editedAt.Time
	}

	// Parse facets JSON
	if facets.Valid {
		var facetArray []interface{}
		if err := json.Unmarshal([]byte(facets.String), &facetArray); err != nil {
			return nil, fmt.Errorf("failed to parse facets JSON for post %s: %w", postView.URI, err)
		}
		postView.TextFacets = facetArray
	}

	// Parse embed JSON
	if embed.Valid {
		var embedData interface{}
		if err := json.Unmarshal([]byte(embed.String), &embedData); err != nil {
			return nil, fmt.Errorf("failed to parse embed JSON for post %s: %w", postView.URI, err)
		}
		postView.Embed = embedData
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
	// Reuse already-parsed facets and embed from PostView to avoid double parsing
	if facets.Valid {
		record["facets"] = postView.TextFacets
	}
	if embed.Valid {
		record["embed"] = postView.Embed
	}
	if labelsJSON.Valid {
		// Labels are stored as JSONB containing full com.atproto.label.defs#selfLabels structure
		var selfLabels posts.SelfLabels
		if err := json.Unmarshal([]byte(labelsJSON.String), &selfLabels); err != nil {
			return nil, fmt.Errorf("failed to parse labels JSON for post %s: %w", postView.URI, err)
		}
		record["labels"] = selfLabels
	}

	postView.Record = record

	return &postView, nil
}
