package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/posts"

	"github.com/lib/pq"
)

// legacyPostCollection is the DEPRECATED community-repo post record (§3.0). A
// post written since the author-owned flip is a posts.PostV2Collection record
// in the author's own repo; both are indexed into the same table, and the URI
// is what says which one a row came from.
const legacyPostCollection = "social.coves.community.post"

type postgresPostRepo struct {
	db *sql.DB
}

// postViewSelectColumns is the ordered SELECT list for hydrating a posts.PostView row.
// It MUST stay byte-aligned with the positional Scan in scanPostView. It is the single
// source of truth shared by GetViewsByURIs, GetByAuthor, and (via feedPostSelectClause)
// the community/timeline/discover feed queries, so the column order can only be defined
// in one place and the queries cannot drift apart and silently mis-scan.
//
// Displayed upvote/downvote counts fold in the bridge-asserted aggregates
// (upvote_count + bridged_upvote_count, etc.) so federated/bridged content shows the
// origin platform's votes. score is already stored inclusive of bridged aggregates, so
// it is selected as-is.
//
// author_handle is COALESCEd to the author DID because the users join is a LEFT
// join (a federated author with no users row must not vanish, PRD §5.3), so
// u.handle is NULL for an unindexed author; the comment read path does the same
// (comment_repo.go). a.status and a.acceptance_uri come from the admission LEFT
// join every display query splices in via visiblePostsJoin — they carry the
// per-community admission context an author's own view renders, and are NULL for
// legacy/bridged rows that hold no admission.
const postViewSelectColumns = `
		p.uri, p.cid, p.rkey,
		p.author_did, COALESCE(u.handle, p.author_did) as author_handle, u.display_name as author_display_name, u.avatar_cid as author_avatar, u.pds_url as author_pds_url,
		p.community_did, c.handle as community_handle, c.name as community_name, c.avatar_cid as community_avatar, c.pds_url as community_pds_url,
		p.title, p.content, p.content_facets, p.embed, p.content_labels,
		p.created_at, p.edited_at, p.indexed_at,
		p.upvote_count + p.bridged_upvote_count AS upvote_count, p.downvote_count + p.bridged_downvote_count AS downvote_count, p.score, p.comment_count,
		a.status AS admission_status, a.acceptance_uri AS admission_acceptance_uri`

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

		// Check for a foreign key violation. fk_community is the only FK left
		// on posts: migration 034 dropped fk_author (author-owned posts — a
		// federated author may have no users row, and that must not block
		// indexing).
		if strings.Contains(err.Error(), "violates foreign key constraint") &&
			strings.Contains(err.Error(), "fk_community") {
			return fmt.Errorf("community DID not found: %s", post.CommunityDID)
		}

		return fmt.Errorf("failed to insert post: %w", err)
	}

	return nil
}

// GetByURI retrieves a post by its AT-URI
// Used for E2E test verification and future GET endpoint
//
// KNOWN DEFECT (issue 2026-07-29-deleted-posts-still-served-by-getcomments.md): this is the one post read path with no
// `deleted_at IS NULL` predicate, so
// it serves a soft-deleted post in full — title, body, facets and all. It is not a dead
// path: comments.GetComments calls it to build the thread's post header and never inspects
// DeletedAt, so social.coves.community.comment.getComments still returns the whole of a
// withdrawn post to an anonymous caller. Same shape as the comment-thread hole found in
// task 12, one table over. (see TestPostRepo_SoftDelete)
func (r *postgresPostRepo) GetByURI(ctx context.Context, uri string) (*posts.Post, error) {
	query := `
		SELECT
			id, uri, cid, rkey, author_did, community_did,
			title, content, content_facets, embed, content_labels,
			created_at, edited_at, indexed_at, deleted_at,
			upvote_count + bridged_upvote_count AS upvote_count, downvote_count + bridged_downvote_count AS downvote_count, score, comment_count
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

	if errors.Is(err, sql.ErrNoRows) {
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

// GetViewsByURIs retrieves full post views for a set of canonical (DID-based) AT-URIs.
// Returns a map keyed by URI; URIs that are missing or soft-deleted are simply absent
// from the map (the caller emits notFoundPost markers for those).
// Row scanning goes through the shared scanPostView, whose Scan order is kept aligned
// with the single source of truth for the SELECT list, postViewSelectColumns.
// Backs the social.coves.community.post.get endpoint (feed hydration + permalinks).
func (r *postgresPostRepo) GetViewsByURIs(ctx context.Context, uris []string) (map[string]*posts.PostView, error) {
	result := make(map[string]*posts.PostView, len(uris))
	if len(uris) == 0 {
		return result, nil
	}

	// The URI set is bound through a single array parameter (= ANY($1)) rather
	// than an interpolated IN list, so the SQL stays fully parameterized and the
	// plan is cached regardless of batch size.
	//
	// post.get is the public permalink surface, so the visibility gate runs with
	// an ANONYMOUS viewer ($2 = ""): accepted posts (and legacy/bridged rows)
	// only. A pending/rejected/removed post is absent from the result, which the
	// service renders as notFoundPost — or, for a removal, upgrades to a
	// #removedPost tombstone from the admission row. An author's privileged view
	// of their own pending posts is served by actor.getPosts (GetByAuthor), which
	// threads a real viewer DID.
	visJoin, visWhere := visiblePostsJoin(2)
	query := `
		SELECT` + postViewSelectColumns + `
		FROM posts p
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did` + visJoin + `
		WHERE p.uri = ANY($1) AND p.deleted_at IS NULL AND ` + visWhere

	rows, err := r.db.QueryContext(ctx, query, pq.Array(uris), "")
	if err != nil {
		return nil, fmt.Errorf("failed to query posts by URIs: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Warn("failed to close rows", "error", err)
		}
	}()

	for rows.Next() {
		postView, err := scanPostView(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan post: %w", err)
		}
		result[postView.URI] = postView
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating posts results: %w", err)
	}

	return result, nil
}

// VisibleHeaderView returns the hydrated view of a single post IFF it is visible
// to viewerDID under the read-path predicate, and nil when it is hidden (or does
// not exist).
//
// It is the viewer-aware companion to GetViewsByURIs, which GetViewsByURIs itself
// cannot be because posts.Repository's signature is frozen at two arguments
// (three in-suite fakes implement it). getComments needs both halves the
// anonymous batch fetch cannot give it — a VIEWER (so an author reaches their own
// pending post's thread header) and the HYDRATED view (so the served header
// carries status/acceptanceUri, PRD §6.2). It is deliberately NOT on the
// Repository interface: the comment service reaches it by an optional type
// assertion, so a unit-test fake without it degrades gracefully rather than
// forcing every fake to grow a method.
//
// It runs the SAME predicate as every other display query — visiblePostsJoin,
// admission status + pinned-CID + collection fail-closed + author self-view — and
// the same deleted_at filter and scanPostView hydration, so the thread header can
// never diverge from post.get or the feeds on what is visible.
func (r *postgresPostRepo) VisibleHeaderView(ctx context.Context, uri, viewerDID string) (*posts.PostView, error) {
	visJoin, visWhere := visiblePostsJoin(2)
	query := `
		SELECT` + postViewSelectColumns + `
		FROM posts p
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did` + visJoin + `
		WHERE p.uri = $1 AND p.deleted_at IS NULL AND ` + visWhere

	rows, err := r.db.QueryContext(ctx, query, uri, viewerDID)
	if err != nil {
		return nil, fmt.Errorf("failed to query visible post header: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Warn("failed to close rows", "error", err)
		}
	}()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("error iterating visible post header: %w", err)
		}
		return nil, nil
	}

	postView, err := scanPostView(rows)
	if err != nil {
		return nil, fmt.Errorf("failed to scan visible post header: %w", err)
	}
	return postView, nil
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

	// The admission visibility gate, threading the viewer DID. A stranger (or the
	// anonymous public) sees the author's accepted posts only; the author
	// themselves ($viewer = ActorDID) additionally sees their own pending /
	// rejected / removed posts, which is how a profile renders per-community
	// status (PRD §6.2). This is the alternate-endpoint the feed gate is
	// worthless without: an author feed showing ungated content leaks exactly
	// what the community feed hides.
	visibilityParam := paramIndex
	visJoin, visWhere := visiblePostsJoin(visibilityParam)
	whereConditions = append(whereConditions, visWhere)
	args = append(args, req.ViewerDID)
	paramIndex++

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
		SELECT %s
		FROM posts p
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did%s
		WHERE %s
		ORDER BY p.created_at DESC, p.uri DESC
		LIMIT $%d
	`, postViewSelectColumns, visJoin, whereClause, paramIndex)

	// Execute query
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query author posts: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Warn("failed to close rows", "error", err)
		}
	}()

	// Scan results
	var postViews []*posts.PostView
	for rows.Next() {
		postView, err := scanPostView(rows)
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

// SoftDelete marks a post as deleted by setting deleted_at
// Called by Jetstream consumer after post is deleted from PDS
// Idempotent: Returns success if post already deleted or doesn't exist
func (r *postgresPostRepo) SoftDelete(ctx context.Context, uri string) error {
	query := `
		UPDATE posts
		SET deleted_at = NOW()
		WHERE uri = $1 AND deleted_at IS NULL
	`
	_, err := r.db.ExecContext(ctx, query, uri)
	if err != nil {
		return fmt.Errorf("failed to soft delete post: %w", err)
	}
	return nil
}

// scanPostView scans a database row into a PostView. Shared by GetByAuthor,
// GetViewsByURIs, and the feed repositories (via scanFeedPost), which all SELECT
// postViewSelectColumns; the Scan order below MUST stay byte-aligned with that column
// list. extraDest receives any columns a caller appends after postViewSelectColumns
// (the feed queries append hot_rank).
func scanPostView(rows *sql.Rows, extraDest ...interface{}) (*posts.PostView, error) {
	var (
		postView          posts.PostView
		authorView        posts.AuthorView
		communityRef      posts.CommunityRef
		title, content    sql.NullString
		facets, embed     sql.NullString
		labelsJSON        sql.NullString
		editedAt          sql.NullTime
		authorDisplayName sql.NullString
		authorAvatar      sql.NullString
		authorPDSURL      sql.NullString
		communityHandle   sql.NullString
		communityAvatar   sql.NullString
		communityPDSURL   sql.NullString
		admissionStatus   sql.NullString
		acceptanceURI     sql.NullString
	)

	dest := []interface{}{
		&postView.URI, &postView.CID, &postView.RKey,
		&authorView.DID, &authorView.Handle, &authorDisplayName, &authorAvatar, &authorPDSURL,
		&communityRef.DID, &communityHandle, &communityRef.Name, &communityAvatar, &communityPDSURL,
		&title, &content, &facets, &embed, &labelsJSON,
		&postView.CreatedAt, &editedAt, &postView.IndexedAt,
		&postView.UpvoteCount, &postView.DownvoteCount, &postView.Score, &postView.CommentCount,
		&admissionStatus, &acceptanceURI,
	}
	dest = append(dest, extraDest...)

	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}

	// Build author view with profile hydration (avatar_small preset, same as community avatars)
	if authorDisplayName.Valid && authorDisplayName.String != "" {
		authorView.DisplayName = &authorDisplayName.String
	}
	if avatarURL := blobs.HydrateImageURL(blobs.GetImageURLConfig(), authorPDSURL.String, authorView.DID, authorAvatar.String, "avatar_small"); avatarURL != "" {
		authorView.Avatar = &avatarURL
	}
	// CARRIED, not just used for the avatar above. A postv2 post's media lives
	// in the AUTHOR's repository, so the blob transform needs to know which
	// server holds it; this column has always been selected and always dropped
	// here, which would leave every author-owned post's images addressed to an
	// empty host.
	authorView.PDSURL = authorPDSURL.String
	postView.Author = &authorView

	// Build community ref
	if communityHandle.Valid {
		communityRef.Handle = communityHandle.String
	}
	// Hydrate avatar CID to URL using image proxy config (avatar_small preset for post views)
	if avatarURL := blobs.HydrateImageURL(blobs.GetImageURLConfig(), communityPDSURL.String, communityRef.DID, communityAvatar.String, "avatar_small"); avatarURL != "" {
		communityRef.Avatar = &avatarURL
	}
	if communityPDSURL.Valid {
		communityRef.PDSURL = communityPDSURL.String
	}
	postView.Community = &communityRef

	// Set optional fields
	if editedAt.Valid {
		postView.EditedAt = &editedAt.Time
	}

	// Per-community admission context (PRD §6.2). Present on any row the
	// visibility predicate returned that carries an admission decision — every
	// accepted post, and an author's own non-accepted posts on their profile.
	// Absent (NULL) for legacy/bridged rows, which omit it on the wire.
	if admissionStatus.Valid {
		postView.Status = admissionStatus.String
	}
	if acceptanceURI.Valid {
		postView.AcceptanceURI = acceptanceURI.String
	}

	// Parse facets JSON into local variable (will be added to record below)
	// Log errors but continue - malformed optional fields shouldn't break the response
	var facetArray []interface{}
	if facets.Valid {
		if err := json.Unmarshal([]byte(facets.String), &facetArray); err != nil {
			slog.Warn("failed to parse facets JSON",
				"post_uri", postView.URI,
				"error", err,
			)
		}
	}

	// Parse embed JSON
	// Log errors but continue - malformed optional fields shouldn't break the response
	if embed.Valid {
		var embedData interface{}
		if err := json.Unmarshal([]byte(embed.String), &embedData); err != nil {
			slog.Warn("failed to parse embed JSON",
				"post_uri", postView.URI,
				"error", err,
			)
		} else {
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

	// Build the record (required by lexicon).
	//
	// THE SHAPE FOLLOWS THE URI, because the two post collections are different
	// records rather than two spellings of one. A postv2 record lives in the
	// AUTHOR's repo and has NO author field — its removal is what makes
	// authorship unforgeable (§3.1) — so synthesising one here would hand every
	// reader back exactly the field the flip deleted, and a client that trusted
	// it would be trusting a value this AppView made up.
	collection := posts.CollectionOfPostURI(postView.URI)
	if collection == "" {
		collection = legacyPostCollection
	}
	record := map[string]interface{}{
		"$type":     collection,
		"community": communityRef.DID,
		"createdAt": postView.CreatedAt.Format(time.RFC3339),
	}
	if collection == legacyPostCollection {
		// The deprecated community-repo record DOES carry an author field, and
		// it is part of the record a client may verify against the repo.
		record["author"] = authorView.DID
	}

	// Add optional fields to record if present
	if title.Valid {
		record["title"] = title.String
	}
	if content.Valid {
		record["content"] = content.String
	}
	// Add facets to record if present
	if facetArray != nil {
		record["facets"] = facetArray
	}
	// Decode the stored embed a second time rather than aliasing postView.Embed.
	// The lexicon calls `record` the post record verbatim, and the handlers
	// hydrate postView.Embed in place into its #view shape (blob refs become
	// image-proxy URLs) — sharing one map would silently rewrite the record too,
	// leaving it claiming a record $type while carrying view-shaped fields.
	if embed.Valid {
		var recordEmbed interface{}
		if err := json.Unmarshal([]byte(embed.String), &recordEmbed); err != nil {
			// The same bytes decoded successfully a few lines above, so reaching
			// here means something stranger than malformed input. Logged rather
			// than dropped silently: the alternative is a post whose record is
			// missing its embed with nothing anywhere recording why.
			slog.Warn("failed to parse embed JSON for record",
				"post_uri", postView.URI,
				"error", err,
			)
		} else {
			record["embed"] = recordEmbed
		}
	}
	if labelsJSON.Valid {
		// Labels are stored as JSONB containing full com.atproto.label.defs#selfLabels structure
		var selfLabels posts.SelfLabels
		if err := json.Unmarshal([]byte(labelsJSON.String), &selfLabels); err != nil {
			slog.Warn("failed to parse labels JSON",
				"post_uri", postView.URI,
				"error", err,
			)
		} else {
			record["labels"] = selfLabels
		}
	}

	postView.Record = record

	return &postView, nil
}
