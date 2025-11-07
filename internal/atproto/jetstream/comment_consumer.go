package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"Coves/internal/atproto/utils"
	"Coves/internal/core/comments"

	"github.com/lib/pq"
)

// Constants for comment validation and processing
const (
	// CommentCollection is the lexicon collection identifier for comments
	CommentCollection = "social.coves.feed.comment"

	// ATProtoScheme is the URI scheme for atProto AT-URIs
	ATProtoScheme = "at://"

	// MaxCommentContentBytes is the maximum allowed size for comment content
	// Per lexicon: max 3000 graphemes, ~30000 bytes
	MaxCommentContentBytes = 30000
)

// CommentEventConsumer consumes comment-related events from Jetstream
// Handles CREATE, UPDATE, and DELETE operations for social.coves.feed.comment
type CommentEventConsumer struct {
	commentRepo comments.Repository
	db          *sql.DB // Direct DB access for atomic count updates
}

// NewCommentEventConsumer creates a new Jetstream consumer for comment events
func NewCommentEventConsumer(
	commentRepo comments.Repository,
	db *sql.DB,
) *CommentEventConsumer {
	return &CommentEventConsumer{
		commentRepo: commentRepo,
		db:          db,
	}
}

// HandleEvent processes a Jetstream event for comment records
func (c *CommentEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for comment records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Handle comment record operations
	if commit.Collection == CommentCollection {
		switch commit.Operation {
		case "create":
			return c.createComment(ctx, event.Did, commit)
		case "update":
			return c.updateComment(ctx, event.Did, commit)
		case "delete":
			return c.deleteComment(ctx, event.Did, commit)
		}
	}

	// Silently ignore other operations and collections
	return nil
}

// createComment indexes a new comment from the firehose and updates parent counts
func (c *CommentEventConsumer) createComment(ctx context.Context, repoDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("comment create event missing record data")
	}

	// Parse the comment record
	commentRecord, err := parseCommentRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse comment record: %w", err)
	}

	// SECURITY: Validate this is a legitimate comment event
	if err := c.validateCommentEvent(ctx, repoDID, commentRecord); err != nil {
		log.Printf("🚨 SECURITY: Rejecting comment event: %v", err)
		return err
	}

	// Build AT-URI for this comment
	// Format: at://commenter_did/social.coves.feed.comment/rkey
	uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", repoDID, commit.RKey)

	// Parse timestamp from record
	createdAt, err := time.Parse(time.RFC3339, commentRecord.CreatedAt)
	if err != nil {
		log.Printf("Warning: Failed to parse createdAt timestamp, using current time: %v", err)
		createdAt = time.Now()
	}

	// Serialize optional JSON fields
	facetsJSON, embedJSON, labelsJSON := serializeOptionalFields(commentRecord)

	// Build comment entity
	comment := &comments.Comment{
		URI:           uri,
		CID:           commit.CID,
		RKey:          commit.RKey,
		CommenterDID:  repoDID, // Comment comes from user's repository
		RootURI:       commentRecord.Reply.Root.URI,
		RootCID:       commentRecord.Reply.Root.CID,
		ParentURI:     commentRecord.Reply.Parent.URI,
		ParentCID:     commentRecord.Reply.Parent.CID,
		Content:       commentRecord.Content,
		ContentFacets: facetsJSON,
		Embed:         embedJSON,
		ContentLabels: labelsJSON,
		Langs:         commentRecord.Langs,
		CreatedAt:     createdAt,
		IndexedAt:     time.Now(),
	}

	// Atomically: Index comment + Update parent counts
	if err := c.indexCommentAndUpdateCounts(ctx, comment); err != nil {
		return fmt.Errorf("failed to index comment and update counts: %w", err)
	}

	log.Printf("✓ Indexed comment: %s (on %s)", uri, comment.ParentURI)
	return nil
}

// updateComment updates an existing comment's content fields
func (c *CommentEventConsumer) updateComment(ctx context.Context, repoDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("comment update event missing record data")
	}

	// Parse the updated comment record
	commentRecord, err := parseCommentRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse comment record: %w", err)
	}

	// SECURITY: Validate this is a legitimate update
	if err := c.validateCommentEvent(ctx, repoDID, commentRecord); err != nil {
		log.Printf("🚨 SECURITY: Rejecting comment update: %v", err)
		return err
	}

	// Build AT-URI for the comment being updated
	uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", repoDID, commit.RKey)

	// Fetch existing comment to validate threading references are immutable
	existingComment, err := c.commentRepo.GetByURI(ctx, uri)
	if err != nil {
		if err == comments.ErrCommentNotFound {
			// Comment doesn't exist yet - might arrive out of order
			log.Printf("Warning: Update event for non-existent comment: %s (will be indexed on CREATE)", uri)
			return nil
		}
		return fmt.Errorf("failed to get existing comment for validation: %w", err)
	}

	// SECURITY: Threading references are IMMUTABLE after creation
	// Reject updates that attempt to change root/parent (prevents thread hijacking)
	if existingComment.RootURI != commentRecord.Reply.Root.URI ||
		existingComment.RootCID != commentRecord.Reply.Root.CID ||
		existingComment.ParentURI != commentRecord.Reply.Parent.URI ||
		existingComment.ParentCID != commentRecord.Reply.Parent.CID {
		log.Printf("🚨 SECURITY: Rejecting comment update - threading references are immutable: %s", uri)
		log.Printf("  Existing root: %s (CID: %s)", existingComment.RootURI, existingComment.RootCID)
		log.Printf("  Incoming root: %s (CID: %s)", commentRecord.Reply.Root.URI, commentRecord.Reply.Root.CID)
		log.Printf("  Existing parent: %s (CID: %s)", existingComment.ParentURI, existingComment.ParentCID)
		log.Printf("  Incoming parent: %s (CID: %s)", commentRecord.Reply.Parent.URI, commentRecord.Reply.Parent.CID)
		return fmt.Errorf("comment threading references cannot be changed after creation")
	}

	// Serialize optional JSON fields
	facetsJSON, embedJSON, labelsJSON := serializeOptionalFields(commentRecord)

	// Build comment update entity (preserves vote counts and created_at)
	comment := &comments.Comment{
		URI:           uri,
		CID:           commit.CID,
		Content:       commentRecord.Content,
		ContentFacets: facetsJSON,
		Embed:         embedJSON,
		ContentLabels: labelsJSON,
		Langs:         commentRecord.Langs,
	}

	// Update the comment in repository
	if err := c.commentRepo.Update(ctx, comment); err != nil {
		return fmt.Errorf("failed to update comment: %w", err)
	}

	log.Printf("✓ Updated comment: %s", uri)
	return nil
}

// deleteComment soft-deletes a comment and updates parent counts
func (c *CommentEventConsumer) deleteComment(ctx context.Context, repoDID string, commit *CommitEvent) error {
	// Build AT-URI for the comment being deleted
	uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", repoDID, commit.RKey)

	// Get existing comment to know its parent (for decrementing the right counter)
	existingComment, err := c.commentRepo.GetByURI(ctx, uri)
	if err != nil {
		if err == comments.ErrCommentNotFound {
			// Idempotent: Comment already deleted or never existed
			log.Printf("Comment already deleted or not found: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to get existing comment: %w", err)
	}

	// Atomically: Soft-delete comment + Update parent counts
	if err := c.deleteCommentAndUpdateCounts(ctx, existingComment); err != nil {
		return fmt.Errorf("failed to delete comment and update counts: %w", err)
	}

	log.Printf("✓ Deleted comment: %s", uri)
	return nil
}

// indexCommentAndUpdateCounts atomically indexes a comment and updates parent counts
func (c *CommentEventConsumer) indexCommentAndUpdateCounts(ctx context.Context, comment *comments.Comment) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	// 1. Check if comment exists and handle resurrection case
	// In atProto, deleted records' rkeys become available - users can recreate with same rkey
	// We must distinguish: idempotent replay (skip) vs resurrection (update + restore counts)
	var existingID int64
	var existingDeletedAt *time.Time
	checkQuery := `SELECT id, deleted_at FROM comments WHERE uri = $1`
	checkErr := tx.QueryRowContext(ctx, checkQuery, comment.URI).Scan(&existingID, &existingDeletedAt)

	var commentID int64

	if checkErr == nil {
		// Comment exists
		if existingDeletedAt == nil {
			// Not deleted - this is an idempotent replay, skip gracefully
			log.Printf("Comment already indexed: %s (idempotent replay)", comment.URI)
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("failed to commit transaction: %w", commitErr)
			}
			return nil
		}

		// Comment was soft-deleted, now being recreated (resurrection)
		// This is a NEW record with same rkey - update ALL fields including threading refs
		// User may have deleted old comment and created a new one on a different parent/root
		log.Printf("Resurrecting previously deleted comment: %s", comment.URI)
		commentID = existingID

		resurrectQuery := `
			UPDATE comments
			SET
				cid = $1,
				commenter_did = $2,
				root_uri = $3,
				root_cid = $4,
				parent_uri = $5,
				parent_cid = $6,
				content = $7,
				content_facets = $8,
				embed = $9,
				content_labels = $10,
				langs = $11,
				created_at = $12,
				indexed_at = $13,
				deleted_at = NULL,
				reply_count = 0
			WHERE id = $14
		`

		_, err = tx.ExecContext(
			ctx, resurrectQuery,
			comment.CID,
			comment.CommenterDID,
			comment.RootURI,
			comment.RootCID,
			comment.ParentURI,
			comment.ParentCID,
			comment.Content,
			comment.ContentFacets,
			comment.Embed,
			comment.ContentLabels,
			pq.Array(comment.Langs),
			comment.CreatedAt,
			time.Now(),
			commentID,
		)
		if err != nil {
			return fmt.Errorf("failed to resurrect comment: %w", err)
		}

	} else if checkErr == sql.ErrNoRows {
		// Comment doesn't exist - insert new comment
		insertQuery := `
			INSERT INTO comments (
				uri, cid, rkey, commenter_did,
				root_uri, root_cid, parent_uri, parent_cid,
				content, content_facets, embed, content_labels, langs,
				created_at, indexed_at
			) VALUES (
				$1, $2, $3, $4,
				$5, $6, $7, $8,
				$9, $10, $11, $12, $13,
				$14, $15
			)
			RETURNING id
		`

		err = tx.QueryRowContext(
			ctx, insertQuery,
			comment.URI, comment.CID, comment.RKey, comment.CommenterDID,
			comment.RootURI, comment.RootCID, comment.ParentURI, comment.ParentCID,
			comment.Content, comment.ContentFacets, comment.Embed, comment.ContentLabels, pq.Array(comment.Langs),
			comment.CreatedAt, time.Now(),
		).Scan(&commentID)
		if err != nil {
			return fmt.Errorf("failed to insert comment: %w", err)
		}

	} else {
		// Unexpected error checking for existing comment
		return fmt.Errorf("failed to check for existing comment: %w", checkErr)
	}

	// 1.5. Reconcile reply_count for this newly inserted comment
	// In case any replies arrived out-of-order before this parent was indexed
	reconcileQuery := `
		UPDATE comments
		SET reply_count = (
			SELECT COUNT(*)
			FROM comments c
			WHERE c.parent_uri = $1 AND c.deleted_at IS NULL
		)
		WHERE id = $2
	`
	_, reconcileErr := tx.ExecContext(ctx, reconcileQuery, comment.URI, commentID)
	if reconcileErr != nil {
		log.Printf("Warning: Failed to reconcile reply_count for %s: %v", comment.URI, reconcileErr)
		// Continue anyway - this is a best-effort reconciliation
	}

	// 2. Update parent counts atomically
	// Parent could be a post (increment comment_count) or a comment (increment reply_count)
	// Parse collection from parent URI to determine target table
	//
	// FIXME(P1): Post comment_count reconciliation not implemented
	// When a comment arrives before its parent post (common with cross-repo Jetstream ordering),
	// the post update below returns 0 rows and we only log a warning. Later, when the post
	// is indexed by the post consumer, there's NO reconciliation logic to count pre-existing
	// comments. This causes posts to have permanently stale comment_count values.
	//
	// FIX REQUIRED: Post consumer MUST implement the same reconciliation pattern as comments
	// (see lines 292-305 above). When indexing a new post, count any comments where parent_uri
	// matches the post URI and set comment_count accordingly.
	//
	// Test demonstrating issue: TestCommentConsumer_PostCountReconciliation_Limitation
	collection := utils.ExtractCollectionFromURI(comment.ParentURI)

	var updateQuery string
	switch collection {
	case "social.coves.community.post":
		// Comment on post - update posts.comment_count
		updateQuery = `
			UPDATE posts
			SET comment_count = comment_count + 1
			WHERE uri = $1 AND deleted_at IS NULL
		`

	case "social.coves.feed.comment":
		// Reply to comment - update comments.reply_count
		updateQuery = `
			UPDATE comments
			SET reply_count = reply_count + 1
			WHERE uri = $1 AND deleted_at IS NULL
		`

	default:
		// Unknown or unsupported parent collection
		// Comment is still indexed, we just don't update parent counts
		log.Printf("Comment parent has unsupported collection: %s (comment indexed, parent count not updated)", collection)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	result, err := tx.ExecContext(ctx, updateQuery, comment.ParentURI)
	if err != nil {
		return fmt.Errorf("failed to update parent count: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check update result: %w", err)
	}

	// If parent not found, that's OK (parent might not be indexed yet)
	if rowsAffected == 0 {
		log.Printf("Warning: Parent not found or deleted: %s (comment indexed anyway)", comment.ParentURI)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// deleteCommentAndUpdateCounts atomically soft-deletes a comment and updates parent counts
func (c *CommentEventConsumer) deleteCommentAndUpdateCounts(ctx context.Context, comment *comments.Comment) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	// 1. Soft-delete the comment (idempotent)
	deleteQuery := `
		UPDATE comments
		SET deleted_at = $2
		WHERE uri = $1 AND deleted_at IS NULL
	`

	result, err := tx.ExecContext(ctx, deleteQuery, comment.URI, time.Now())
	if err != nil {
		return fmt.Errorf("failed to delete comment: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check delete result: %w", err)
	}

	// Idempotent: If no rows affected, comment already deleted
	if rowsAffected == 0 {
		log.Printf("Comment already deleted: %s (idempotent)", comment.URI)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	// 2. Decrement parent counts atomically
	// Parent could be a post or comment - parse collection to determine target table
	collection := utils.ExtractCollectionFromURI(comment.ParentURI)

	var updateQuery string
	switch collection {
	case "social.coves.community.post":
		// Comment on post - decrement posts.comment_count
		updateQuery = `
			UPDATE posts
			SET comment_count = GREATEST(0, comment_count - 1)
			WHERE uri = $1 AND deleted_at IS NULL
		`

	case "social.coves.feed.comment":
		// Reply to comment - decrement comments.reply_count
		updateQuery = `
			UPDATE comments
			SET reply_count = GREATEST(0, reply_count - 1)
			WHERE uri = $1 AND deleted_at IS NULL
		`

	default:
		// Unknown or unsupported parent collection
		// Comment is still deleted, we just don't update parent counts
		log.Printf("Comment parent has unsupported collection: %s (comment deleted, parent count not updated)", collection)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	result, err = tx.ExecContext(ctx, updateQuery, comment.ParentURI)
	if err != nil {
		return fmt.Errorf("failed to update parent count: %w", err)
	}

	rowsAffected, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check update result: %w", err)
	}

	// If parent not found, that's OK (parent might be deleted)
	if rowsAffected == 0 {
		log.Printf("Warning: Parent not found or deleted: %s (comment deleted anyway)", comment.ParentURI)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// validateCommentEvent performs security validation on comment events
func (c *CommentEventConsumer) validateCommentEvent(ctx context.Context, repoDID string, comment *CommentRecordFromJetstream) error {
	// SECURITY: Comments MUST come from user repositories (repo owner = commenter DID)
	// The repository owner (repoDID) IS the commenter - comments are stored in user repos.
	//
	// We do NOT check if the user exists in AppView because:
	// 1. Comment events may arrive before user events in Jetstream (race condition)
	// 2. The comment came from the user's PDS repository (authenticated by PDS)
	// 3. The database FK constraint was removed to allow out-of-order indexing
	// 4. Orphaned comments (from never-indexed users) are harmless
	//
	// Security is maintained because:
	// - Comment must come from user's own PDS repository (verified by atProto)
	// - Fake DIDs will fail PDS authentication

	// Validate DID format (basic sanity check)
	if !strings.HasPrefix(repoDID, "did:") {
		return fmt.Errorf("invalid commenter DID format: %s", repoDID)
	}

	// Validate content is not empty (required per lexicon)
	if comment.Content == "" {
		return fmt.Errorf("comment content is required")
	}

	// Validate content length (defensive check - PDS should enforce this)
	// Per lexicon: max 3000 graphemes, ~30000 bytes
	// We check bytes as a simple defensive measure
	if len(comment.Content) > MaxCommentContentBytes {
		return fmt.Errorf("comment content exceeds maximum length (%d bytes): got %d bytes", MaxCommentContentBytes, len(comment.Content))
	}

	// Validate reply references exist
	if comment.Reply.Root.URI == "" || comment.Reply.Root.CID == "" {
		return fmt.Errorf("invalid root reference: must have both URI and CID")
	}

	if comment.Reply.Parent.URI == "" || comment.Reply.Parent.CID == "" {
		return fmt.Errorf("invalid parent reference: must have both URI and CID")
	}

	// Validate AT-URI structure for root and parent
	if err := validateATURI(comment.Reply.Root.URI); err != nil {
		return fmt.Errorf("invalid root URI: %w", err)
	}

	if err := validateATURI(comment.Reply.Parent.URI); err != nil {
		return fmt.Errorf("invalid parent URI: %w", err)
	}

	return nil
}

// validateATURI performs basic structure validation on AT-URIs
// Format: at://did:method:id/collection/rkey
// This is defensive validation - we trust PDS but catch obviously malformed URIs
func validateATURI(uri string) error {
	if !strings.HasPrefix(uri, ATProtoScheme) {
		return fmt.Errorf("must start with %s", ATProtoScheme)
	}

	// Remove at:// prefix and split by /
	withoutScheme := strings.TrimPrefix(uri, ATProtoScheme)
	parts := strings.Split(withoutScheme, "/")

	// Must have at least 3 parts: did, collection, rkey
	if len(parts) < 3 {
		return fmt.Errorf("invalid structure (expected at://did/collection/rkey)")
	}

	// First part should be a DID
	if !strings.HasPrefix(parts[0], "did:") {
		return fmt.Errorf("repository identifier must be a DID")
	}

	// Collection and rkey should not be empty
	if parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("collection and rkey cannot be empty")
	}

	return nil
}

// CommentRecordFromJetstream represents a comment record as received from Jetstream
// Matches social.coves.feed.comment lexicon
type CommentRecordFromJetstream struct {
	Labels    interface{}            `json:"labels,omitempty"`
	Embed     map[string]interface{} `json:"embed,omitempty"`
	Reply     ReplyRefFromJetstream  `json:"reply"`
	Type      string                 `json:"$type"`
	Content   string                 `json:"content"`
	CreatedAt string                 `json:"createdAt"`
	Facets    []interface{}          `json:"facets,omitempty"`
	Langs     []string               `json:"langs,omitempty"`
}

// ReplyRefFromJetstream represents the threading structure
type ReplyRefFromJetstream struct {
	Root   StrongRefFromJetstream `json:"root"`
	Parent StrongRefFromJetstream `json:"parent"`
}

// parseCommentRecord parses a comment record from Jetstream event data
func parseCommentRecord(record map[string]interface{}) (*CommentRecordFromJetstream, error) {
	// Marshal to JSON and back for proper type conversion
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}

	var comment CommentRecordFromJetstream
	if err := json.Unmarshal(recordJSON, &comment); err != nil {
		return nil, fmt.Errorf("failed to unmarshal comment record: %w", err)
	}

	// Validate required fields
	if comment.Content == "" {
		return nil, fmt.Errorf("comment record missing content field")
	}

	if comment.CreatedAt == "" {
		return nil, fmt.Errorf("comment record missing createdAt field")
	}

	return &comment, nil
}

// serializeOptionalFields serializes facets, embed, and labels from a comment record to JSON strings
// Returns nil pointers for empty/nil fields (DRY helper to avoid duplication)
func serializeOptionalFields(commentRecord *CommentRecordFromJetstream) (facetsJSON, embedJSON, labelsJSON *string) {
	// Serialize facets if present
	if len(commentRecord.Facets) > 0 {
		if facetsBytes, err := json.Marshal(commentRecord.Facets); err == nil {
			facetsStr := string(facetsBytes)
			facetsJSON = &facetsStr
		}
	}

	// Serialize embed if present
	if len(commentRecord.Embed) > 0 {
		if embedBytes, err := json.Marshal(commentRecord.Embed); err == nil {
			embedStr := string(embedBytes)
			embedJSON = &embedStr
		}
	}

	// Serialize labels if present
	if commentRecord.Labels != nil {
		if labelsBytes, err := json.Marshal(commentRecord.Labels); err == nil {
			labelsStr := string(labelsBytes)
			labelsJSON = &labelsStr
		}
	}

	return facetsJSON, embedJSON, labelsJSON
}
