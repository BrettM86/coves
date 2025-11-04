package jetstream

import (
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// VoteEventConsumer consumes vote-related events from Jetstream
// Handles CREATE and DELETE operations for social.coves.feed.vote
type VoteEventConsumer struct {
	voteRepo    votes.Repository
	userService users.UserService
	db          *sql.DB // Direct DB access for atomic vote count updates
}

// NewVoteEventConsumer creates a new Jetstream consumer for vote events
func NewVoteEventConsumer(
	voteRepo votes.Repository,
	userService users.UserService,
	db *sql.DB,
) *VoteEventConsumer {
	return &VoteEventConsumer{
		voteRepo:    voteRepo,
		userService: userService,
		db:          db,
	}
}

// HandleEvent processes a Jetstream event for vote records
func (c *VoteEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for vote records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Handle vote record operations
	if commit.Collection == "social.coves.feed.vote" {
		switch commit.Operation {
		case "create":
			return c.createVote(ctx, event.Did, commit)
		case "delete":
			return c.deleteVote(ctx, event.Did, commit)
		}
	}

	// Silently ignore other operations and collections
	return nil
}

// createVote indexes a new vote from the firehose and updates post counts
func (c *VoteEventConsumer) createVote(ctx context.Context, repoDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("vote create event missing record data")
	}

	// Parse the vote record
	voteRecord, err := parseVoteRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse vote record: %w", err)
	}

	// SECURITY: Validate this is a legitimate vote event
	if err := c.validateVoteEvent(ctx, repoDID, voteRecord); err != nil {
		log.Printf("🚨 SECURITY: Rejecting vote event: %v", err)
		return err
	}

	// Build AT-URI for this vote
	// Format: at://voter_did/social.coves.feed.vote/rkey
	uri := fmt.Sprintf("at://%s/social.coves.feed.vote/%s", repoDID, commit.RKey)

	// Parse timestamp from record
	createdAt, err := time.Parse(time.RFC3339, voteRecord.CreatedAt)
	if err != nil {
		log.Printf("Warning: Failed to parse createdAt timestamp, using current time: %v", err)
		createdAt = time.Now()
	}

	// Build vote entity
	vote := &votes.Vote{
		URI:        uri,
		CID:        commit.CID,
		RKey:       commit.RKey,
		VoterDID:   repoDID, // Vote comes from user's repository
		SubjectURI: voteRecord.Subject.URI,
		SubjectCID: voteRecord.Subject.CID,
		Direction:  voteRecord.Direction,
		CreatedAt:  createdAt,
		IndexedAt:  time.Now(),
	}

	// Atomically: Index vote + Update post counts
	if err := c.indexVoteAndUpdateCounts(ctx, vote); err != nil {
		return fmt.Errorf("failed to index vote and update counts: %w", err)
	}

	log.Printf("✓ Indexed vote: %s (%s on %s)", uri, vote.Direction, vote.SubjectURI)
	return nil
}

// deleteVote soft-deletes a vote and updates post counts
func (c *VoteEventConsumer) deleteVote(ctx context.Context, repoDID string, commit *CommitEvent) error {
	// Build AT-URI for the vote being deleted
	uri := fmt.Sprintf("at://%s/social.coves.feed.vote/%s", repoDID, commit.RKey)

	// Get existing vote to know its direction (for decrementing the right counter)
	existingVote, err := c.voteRepo.GetByURI(ctx, uri)
	if err != nil {
		if err == votes.ErrVoteNotFound {
			// Idempotent: Vote already deleted or never existed
			log.Printf("Vote already deleted or not found: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to get existing vote: %w", err)
	}

	// Atomically: Soft-delete vote + Update post counts
	if err := c.deleteVoteAndUpdateCounts(ctx, existingVote); err != nil {
		return fmt.Errorf("failed to delete vote and update counts: %w", err)
	}

	log.Printf("✓ Deleted vote: %s (%s on %s)", uri, existingVote.Direction, existingVote.SubjectURI)
	return nil
}

// indexVoteAndUpdateCounts atomically indexes a vote and updates post vote counts
func (c *VoteEventConsumer) indexVoteAndUpdateCounts(ctx context.Context, vote *votes.Vote) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	// 1. Index the vote (idempotent with ON CONFLICT DO NOTHING)
	query := `
		INSERT INTO votes (
			uri, cid, rkey, voter_did,
			subject_uri, subject_cid, direction,
			created_at, indexed_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7,
			$8, NOW()
		)
		ON CONFLICT (uri) DO NOTHING
		RETURNING id
	`

	var voteID int64
	err = tx.QueryRowContext(
		ctx, query,
		vote.URI, vote.CID, vote.RKey, vote.VoterDID,
		vote.SubjectURI, vote.SubjectCID, vote.Direction,
		vote.CreatedAt,
	).Scan(&voteID)

	// If no rows returned, vote already exists (idempotent - OK for Jetstream replays)
	if err == sql.ErrNoRows {
		log.Printf("Vote already indexed: %s (idempotent)", vote.URI)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	if err != nil {
		return fmt.Errorf("failed to insert vote: %w", err)
	}

	// 2. Update post vote counts atomically
	// Increment upvote_count or downvote_count based on direction
	// Also update score (upvote_count - downvote_count)
	var updateQuery string
	if vote.Direction == "up" {
		updateQuery = `
			UPDATE posts
			SET upvote_count = upvote_count + 1,
			    score = upvote_count + 1 - downvote_count
			WHERE uri = $1 AND deleted_at IS NULL
		`
	} else { // "down"
		updateQuery = `
			UPDATE posts
			SET downvote_count = downvote_count + 1,
			    score = upvote_count - (downvote_count + 1)
			WHERE uri = $1 AND deleted_at IS NULL
		`
	}

	result, err := tx.ExecContext(ctx, updateQuery, vote.SubjectURI)
	if err != nil {
		return fmt.Errorf("failed to update post counts: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check update result: %w", err)
	}

	// If post doesn't exist or is deleted, that's OK (vote still indexed)
	// Future: We could validate post exists before indexing vote
	if rowsAffected == 0 {
		log.Printf("Warning: Post not found or deleted: %s (vote indexed anyway)", vote.SubjectURI)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// deleteVoteAndUpdateCounts atomically soft-deletes a vote and updates post vote counts
func (c *VoteEventConsumer) deleteVoteAndUpdateCounts(ctx context.Context, vote *votes.Vote) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	// 1. Soft-delete the vote (idempotent)
	deleteQuery := `
		UPDATE votes
		SET deleted_at = NOW()
		WHERE uri = $1 AND deleted_at IS NULL
	`

	result, err := tx.ExecContext(ctx, deleteQuery, vote.URI)
	if err != nil {
		return fmt.Errorf("failed to delete vote: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check delete result: %w", err)
	}

	// Idempotent: If no rows affected, vote already deleted
	if rowsAffected == 0 {
		log.Printf("Vote already deleted: %s (idempotent)", vote.URI)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	// 2. Decrement post vote counts atomically
	// Decrement upvote_count or downvote_count based on direction
	// Also update score (use GREATEST to prevent negative counts)
	var updateQuery string
	if vote.Direction == "up" {
		updateQuery = `
			UPDATE posts
			SET upvote_count = GREATEST(0, upvote_count - 1),
			    score = GREATEST(0, upvote_count - 1) - downvote_count
			WHERE uri = $1 AND deleted_at IS NULL
		`
	} else { // "down"
		updateQuery = `
			UPDATE posts
			SET downvote_count = GREATEST(0, downvote_count - 1),
			    score = upvote_count - GREATEST(0, downvote_count - 1)
			WHERE uri = $1 AND deleted_at IS NULL
		`
	}

	result, err = tx.ExecContext(ctx, updateQuery, vote.SubjectURI)
	if err != nil {
		return fmt.Errorf("failed to update post counts: %w", err)
	}

	rowsAffected, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check update result: %w", err)
	}

	// If post doesn't exist or is deleted, that's OK (vote still deleted)
	if rowsAffected == 0 {
		log.Printf("Warning: Post not found or deleted: %s (vote deleted anyway)", vote.SubjectURI)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// validateVoteEvent performs security validation on vote events
func (c *VoteEventConsumer) validateVoteEvent(ctx context.Context, repoDID string, vote *VoteRecordFromJetstream) error {
	// SECURITY: Votes MUST come from user repositories (repo owner = voter DID)
	// The repository owner (repoDID) IS the voter - votes are stored in user repos.
	//
	// We do NOT check if the user exists in AppView because:
	// 1. Vote events may arrive before user events in Jetstream (race condition)
	// 2. The vote came from the user's PDS repository (authenticated by PDS)
	// 3. The database FK constraint was removed to allow out-of-order indexing
	// 4. Orphaned votes (from never-indexed users) are harmless
	//
	// Security is maintained because:
	// - Vote must come from user's own PDS repository (verified by atProto)
	// - Communities cannot create votes in their repos (different collection)
	// - Fake DIDs will fail PDS authentication

	// Validate DID format (basic sanity check)
	if !strings.HasPrefix(repoDID, "did:") {
		return fmt.Errorf("invalid voter DID format: %s", repoDID)
	}

	// Validate vote direction
	if vote.Direction != "up" && vote.Direction != "down" {
		return fmt.Errorf("invalid vote direction: %s (must be 'up' or 'down')", vote.Direction)
	}

	// Validate subject has both URI and CID (strong reference)
	if vote.Subject.URI == "" || vote.Subject.CID == "" {
		return fmt.Errorf("invalid subject: must have both URI and CID (strong reference)")
	}

	return nil
}

// VoteRecordFromJetstream represents a vote record as received from Jetstream
type VoteRecordFromJetstream struct {
	Subject   StrongRefFromJetstream `json:"subject"`
	Direction string                 `json:"direction"`
	CreatedAt string                 `json:"createdAt"`
}

// StrongRefFromJetstream represents a strong reference (URI + CID)
type StrongRefFromJetstream struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// parseVoteRecord parses a vote record from Jetstream event data
func parseVoteRecord(record map[string]interface{}) (*VoteRecordFromJetstream, error) {
	// Extract subject (strong reference)
	subjectMap, ok := record["subject"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("missing or invalid subject field")
	}

	subjectURI, _ := subjectMap["uri"].(string)
	subjectCID, _ := subjectMap["cid"].(string)

	// Extract direction
	direction, _ := record["direction"].(string)

	// Extract createdAt
	createdAt, _ := record["createdAt"].(string)

	return &VoteRecordFromJetstream{
		Subject: StrongRefFromJetstream{
			URI: subjectURI,
			CID: subjectCID,
		},
		Direction: direction,
		CreatedAt: createdAt,
	}, nil
}
