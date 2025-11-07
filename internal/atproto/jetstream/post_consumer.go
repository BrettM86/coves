package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
)

// PostEventConsumer consumes post-related events from Jetstream
// Currently handles only CREATE operations for social.coves.community.post
// UPDATE and DELETE handlers will be added when those features are implemented
type PostEventConsumer struct {
	postRepo      posts.Repository
	communityRepo communities.Repository
	userService   users.UserService
	db            *sql.DB // Direct DB access for atomic count reconciliation
}

// NewPostEventConsumer creates a new Jetstream consumer for post events
func NewPostEventConsumer(
	postRepo posts.Repository,
	communityRepo communities.Repository,
	userService users.UserService,
	db *sql.DB,
) *PostEventConsumer {
	return &PostEventConsumer{
		postRepo:      postRepo,
		communityRepo: communityRepo,
		userService:   userService,
		db:            db,
	}
}

// HandleEvent processes a Jetstream event for post records
// Currently only handles CREATE operations - UPDATE/DELETE deferred until those features exist
func (c *PostEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for post records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Only handle post record creation for now
	// UPDATE and DELETE will be added when we implement those features
	if commit.Collection == "social.coves.community.post" && commit.Operation == "create" {
		return c.createPost(ctx, event.Did, commit)
	}

	// Silently ignore other operations (update, delete) and other collections
	return nil
}

// createPost indexes a new post from the firehose
func (c *PostEventConsumer) createPost(ctx context.Context, repoDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("post create event missing record data")
	}

	// Parse the post record
	postRecord, err := parsePostRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse post record: %w", err)
	}

	// SECURITY: Validate this is a legitimate post event
	if err := c.validatePostEvent(ctx, repoDID, postRecord); err != nil {
		log.Printf("🚨 SECURITY: Rejecting post event: %v", err)
		return err
	}

	// Build AT-URI for this post
	// Format: at://community_did/social.coves.community.post/rkey
	uri := fmt.Sprintf("at://%s/social.coves.community.post/%s", repoDID, commit.RKey)

	// Parse timestamp from record
	createdAt, err := time.Parse(time.RFC3339, postRecord.CreatedAt)
	if err != nil {
		// Fallback to current time if parsing fails
		log.Printf("Warning: Failed to parse createdAt timestamp, using current time: %v", err)
		createdAt = time.Now()
	}

	// Build post entity
	post := &posts.Post{
		URI:          uri,
		CID:          commit.CID,
		RKey:         commit.RKey,
		AuthorDID:    postRecord.Author,
		CommunityDID: postRecord.Community,
		Title:        postRecord.Title,
		Content:      postRecord.Content,
		CreatedAt:    createdAt,
		IndexedAt:    time.Now(),
		// Stats remain at 0 (no votes yet)
		UpvoteCount:   0,
		DownvoteCount: 0,
		Score:         0,
		CommentCount:  0,
	}

	// Serialize JSON fields (facets, embed, labels)
	if postRecord.Facets != nil {
		facetsJSON, marshalErr := json.Marshal(postRecord.Facets)
		if marshalErr == nil {
			facetsStr := string(facetsJSON)
			post.ContentFacets = &facetsStr
		}
	}

	if postRecord.Embed != nil {
		embedJSON, marshalErr := json.Marshal(postRecord.Embed)
		if marshalErr == nil {
			embedStr := string(embedJSON)
			post.Embed = &embedStr
		}
	}

	if postRecord.Labels != nil {
		labelsJSON, marshalErr := json.Marshal(postRecord.Labels)
		if marshalErr == nil {
			labelsStr := string(labelsJSON)
			post.ContentLabels = &labelsStr
		}
	}

	// Atomically: Index post + Reconcile comment count for out-of-order arrivals
	if err := c.indexPostAndReconcileCounts(ctx, post); err != nil {
		return fmt.Errorf("failed to index post and reconcile counts: %w", err)
	}

	log.Printf("✓ Indexed post: %s (author: %s, community: %s, rkey: %s)",
		uri, post.AuthorDID, post.CommunityDID, commit.RKey)
	return nil
}

// indexPostAndReconcileCounts atomically indexes a post and reconciles comment counts
// This fixes the race condition where comments arrive before their parent post
func (c *PostEventConsumer) indexPostAndReconcileCounts(ctx context.Context, post *posts.Post) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	// 1. Insert the post (idempotent with RETURNING clause)
	var facetsJSON, embedJSON, labelsJSON sql.NullString

	if post.ContentFacets != nil {
		facetsJSON.String = *post.ContentFacets
		facetsJSON.Valid = true
	}

	if post.Embed != nil {
		embedJSON.String = *post.Embed
		embedJSON.Valid = true
	}

	if post.ContentLabels != nil {
		labelsJSON.String = *post.ContentLabels
		labelsJSON.Valid = true
	}

	insertQuery := `
		INSERT INTO posts (
			uri, cid, rkey, author_did, community_did,
			title, content, content_facets, embed, content_labels,
			created_at, indexed_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, NOW()
		)
		ON CONFLICT (uri) DO NOTHING
		RETURNING id
	`

	var postID int64
	insertErr := tx.QueryRowContext(
		ctx, insertQuery,
		post.URI, post.CID, post.RKey, post.AuthorDID, post.CommunityDID,
		post.Title, post.Content, facetsJSON, embedJSON, labelsJSON,
		post.CreatedAt,
	).Scan(&postID)

	// If no rows returned, post already exists (idempotent - OK for Jetstream replays)
	if insertErr == sql.ErrNoRows {
		log.Printf("Post already indexed: %s (idempotent)", post.URI)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	if insertErr != nil {
		return fmt.Errorf("failed to insert post: %w", insertErr)
	}

	// 2. Reconcile comment_count for this newly inserted post
	// In case any comments arrived out-of-order before this post was indexed
	// This is the CRITICAL FIX for the race condition identified in the PR review
	reconcileQuery := `
		UPDATE posts
		SET comment_count = (
			SELECT COUNT(*)
			FROM comments c
			WHERE c.parent_uri = $1 AND c.deleted_at IS NULL
		)
		WHERE id = $2
	`
	_, reconcileErr := tx.ExecContext(ctx, reconcileQuery, post.URI, postID)
	if reconcileErr != nil {
		log.Printf("Warning: Failed to reconcile comment_count for %s: %v", post.URI, reconcileErr)
		// Continue anyway - this is a best-effort reconciliation
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// validatePostEvent performs security validation on post events
// This prevents malicious actors from indexing fake posts
func (c *PostEventConsumer) validatePostEvent(ctx context.Context, repoDID string, post *PostRecordFromJetstream) error {
	// CRITICAL SECURITY CHECK:
	// Posts MUST come from community repositories, not user repositories
	// This prevents users from creating posts that appear to be from communities they don't control
	//
	// Example attack prevented:
	//   - User creates post in their own repo (at://user_did/social.coves.community.post/xyz)
	//   - Claims it's for community X (community field = community_did)
	//   - Without this check, fake post would be indexed
	//
	// With this check:
	//   - We verify event.Did (repo owner) == post.community (claimed community)
	//   - Reject if mismatch
	if repoDID != post.Community {
		return fmt.Errorf("repository DID (%s) doesn't match community DID (%s) - posts must come from community repos",
			repoDID, post.Community)
	}

	// CRITICAL: Verify community exists in AppView
	// Posts MUST reference valid communities (enforced by FK constraint)
	// If community isn't indexed yet, we must reject the post
	// Jetstream will replay events, so the post will be indexed once community is ready
	_, err := c.communityRepo.GetByDID(ctx, post.Community)
	if err != nil {
		if communities.IsNotFound(err) {
			// Reject - community must be indexed before posts
			// This maintains referential integrity and prevents orphaned posts
			return fmt.Errorf("community not found: %s - cannot index post before community", post.Community)
		}
		// Database error or other issue
		return fmt.Errorf("failed to verify community exists: %w", err)
	}

	// CRITICAL: Verify author exists in AppView
	// Every post MUST have a valid author (enforced by FK constraint)
	// Even though posts live in community repos, they belong to specific authors
	// If author isn't indexed yet, we must reject the post
	_, err = c.userService.GetUserByDID(ctx, post.Author)
	if err != nil {
		// Check if it's a "not found" error using string matching
		// (users package doesn't export IsNotFound)
		if err.Error() == "user not found" || strings.Contains(err.Error(), "not found") {
			// Reject - author must be indexed before posts
			// This maintains referential integrity and prevents orphaned posts
			return fmt.Errorf("author not found: %s - cannot index post before author", post.Author)
		}
		// Database error or other issue
		return fmt.Errorf("failed to verify author exists: %w", err)
	}

	return nil
}

// PostRecordFromJetstream represents a post record as received from Jetstream
// Matches the structure written to PDS via social.coves.community.post
type PostRecordFromJetstream struct {
	OriginalAuthor interface{}            `json:"originalAuthor,omitempty"`
	FederatedFrom  interface{}            `json:"federatedFrom,omitempty"`
	Location       interface{}            `json:"location,omitempty"`
	Title          *string                `json:"title,omitempty"`
	Content        *string                `json:"content,omitempty"`
	Embed          map[string]interface{} `json:"embed,omitempty"`
	Labels         *posts.SelfLabels      `json:"labels,omitempty"`
	Type           string                 `json:"$type"`
	Community      string                 `json:"community"`
	Author         string                 `json:"author"`
	CreatedAt      string                 `json:"createdAt"`
	Facets         []interface{}          `json:"facets,omitempty"`
}

// parsePostRecord converts a raw Jetstream record map to a PostRecordFromJetstream
func parsePostRecord(record map[string]interface{}) (*PostRecordFromJetstream, error) {
	// Marshal to JSON and back to ensure proper type conversion
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}

	var post PostRecordFromJetstream
	if err := json.Unmarshal(recordJSON, &post); err != nil {
		return nil, fmt.Errorf("failed to unmarshal post record: %w", err)
	}

	// Validate required fields
	if post.Community == "" {
		return nil, fmt.Errorf("post record missing community field")
	}
	if post.Author == "" {
		return nil, fmt.Errorf("post record missing author field")
	}
	if post.CreatedAt == "" {
		return nil, fmt.Errorf("post record missing createdAt field")
	}

	return &post, nil
}
