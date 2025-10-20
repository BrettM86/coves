package jetstream

import (
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// PostEventConsumer consumes post-related events from Jetstream
// Currently handles only CREATE operations for social.coves.post.record
// UPDATE and DELETE handlers will be added when those features are implemented
type PostEventConsumer struct {
	postRepo      posts.Repository
	communityRepo communities.Repository
	userService   users.UserService
}

// NewPostEventConsumer creates a new Jetstream consumer for post events
func NewPostEventConsumer(
	postRepo posts.Repository,
	communityRepo communities.Repository,
	userService users.UserService,
) *PostEventConsumer {
	return &PostEventConsumer{
		postRepo:      postRepo,
		communityRepo: communityRepo,
		userService:   userService,
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
	if commit.Collection == "social.coves.post.record" && commit.Operation == "create" {
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
	// Format: at://community_did/social.coves.post.record/rkey
	uri := fmt.Sprintf("at://%s/social.coves.post.record/%s", repoDID, commit.RKey)

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

	if len(postRecord.ContentLabels) > 0 {
		labelsJSON, marshalErr := json.Marshal(postRecord.ContentLabels)
		if marshalErr == nil {
			labelsStr := string(labelsJSON)
			post.ContentLabels = &labelsStr
		}
	}

	// Index in AppView database (idempotent - safe for Jetstream replays)
	err = c.postRepo.Create(ctx, post)
	if err != nil {
		// Check if it already exists (idempotency)
		if posts.IsConflict(err) {
			log.Printf("Post already indexed: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to index post: %w", err)
	}

	log.Printf("✓ Indexed post: %s (author: %s, community: %s, rkey: %s)",
		uri, post.AuthorDID, post.CommunityDID, commit.RKey)
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
	//   - User creates post in their own repo (at://user_did/social.coves.post.record/xyz)
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
// Matches the structure written to PDS via social.coves.post.record
type PostRecordFromJetstream struct {
	OriginalAuthor interface{}            `json:"originalAuthor,omitempty"`
	FederatedFrom  interface{}            `json:"federatedFrom,omitempty"`
	Location       interface{}            `json:"location,omitempty"`
	Title          *string                `json:"title,omitempty"`
	Content        *string                `json:"content,omitempty"`
	Embed          map[string]interface{} `json:"embed,omitempty"`
	Type           string                 `json:"$type"`
	Community      string                 `json:"community"`
	Author         string                 `json:"author"`
	CreatedAt      string                 `json:"createdAt"`
	Facets         []interface{}          `json:"facets,omitempty"`
	ContentLabels  []string               `json:"contentLabels,omitempty"`
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
