package jetstream

import (
	"Coves/internal/core/communities"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// CommunityEventConsumer consumes community-related events from Jetstream
type CommunityEventConsumer struct {
	repo communities.Repository
}

// NewCommunityEventConsumer creates a new Jetstream consumer for community events
func NewCommunityEventConsumer(repo communities.Repository) *CommunityEventConsumer {
	return &CommunityEventConsumer{
		repo: repo,
	}
}

// HandleEvent processes a Jetstream event for community records
// This is called by the main Jetstream consumer when it receives commit events
func (c *CommunityEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for community records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Route to appropriate handler based on collection
	switch commit.Collection {
	case "social.coves.community.profile":
		return c.handleCommunityProfile(ctx, event.Did, commit)
	case "social.coves.community.subscribe":
		return c.handleSubscription(ctx, event.Did, commit)
	case "social.coves.community.unsubscribe":
		return c.handleUnsubscribe(ctx, event.Did, commit)
	default:
		// Not a community-related collection
		return nil
	}
}

// handleCommunityProfile processes community profile create/update/delete events
func (c *CommunityEventConsumer) handleCommunityProfile(ctx context.Context, did string, commit *CommitEvent) error {
	switch commit.Operation {
	case "create":
		return c.createCommunity(ctx, did, commit)
	case "update":
		return c.updateCommunity(ctx, did, commit)
	case "delete":
		return c.deleteCommunity(ctx, did)
	default:
		log.Printf("Unknown operation for community profile: %s", commit.Operation)
		return nil
	}
}

// createCommunity indexes a new community from the firehose
func (c *CommunityEventConsumer) createCommunity(ctx context.Context, did string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("community profile create event missing record data")
	}

	// Parse the community profile record
	profile, err := parseCommunityProfile(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse community profile: %w", err)
	}

	// Build AT-URI for this record
	// V2 Architecture (ONLY):
	//   - 'did' parameter IS the community DID (community owns its own repo)
	//   - rkey MUST be "self" for community profiles
	//   - URI: at://community_did/social.coves.community.profile/self

	// REJECT non-V2 communities (pre-production: no V1 compatibility)
	if commit.RKey != "self" {
		return fmt.Errorf("invalid community profile rkey: expected 'self', got '%s' (V1 communities not supported)", commit.RKey)
	}

	uri := fmt.Sprintf("at://%s/social.coves.community.profile/self", did)

	// V2: Community ALWAYS owns itself
	ownerDID := did

	// Create community entity
	community := &communities.Community{
		DID:                    did, // V2: Repository DID IS the community DID
		Handle:                 profile.Handle,
		Name:                   profile.Name,
		DisplayName:            profile.DisplayName,
		Description:            profile.Description,
		OwnerDID:               ownerDID, // V2: same as DID (self-owned)
		CreatedByDID:           profile.CreatedBy,
		HostedByDID:            profile.HostedBy,
		Visibility:             profile.Visibility,
		AllowExternalDiscovery: profile.Federation.AllowExternalDiscovery,
		ModerationType:         profile.ModerationType,
		ContentWarnings:        profile.ContentWarnings,
		MemberCount:            profile.MemberCount,
		SubscriberCount:        profile.SubscriberCount,
		FederatedFrom:          profile.FederatedFrom,
		FederatedID:            profile.FederatedID,
		CreatedAt:              profile.CreatedAt,
		UpdatedAt:              time.Now(),
		RecordURI:              uri,
		RecordCID:              commit.CID,
	}

	// Handle blobs (avatar/banner) if present
	if avatarCID, ok := extractBlobCID(profile.Avatar); ok {
		community.AvatarCID = avatarCID
	}
	if bannerCID, ok := extractBlobCID(profile.Banner); ok {
		community.BannerCID = bannerCID
	}

	// Handle description facets (rich text)
	if profile.DescriptionFacets != nil {
		facetsJSON, marshalErr := json.Marshal(profile.DescriptionFacets)
		if marshalErr == nil {
			community.DescriptionFacets = facetsJSON
		}
	}

	// Index in AppView database
	_, err = c.repo.Create(ctx, community)
	if err != nil {
		// Check if it already exists (idempotency)
		if communities.IsConflict(err) {
			log.Printf("Community already indexed: %s (%s)", community.Handle, community.DID)
			return nil
		}
		return fmt.Errorf("failed to index community: %w", err)
	}

	log.Printf("Indexed new community: %s (%s)", community.Handle, community.DID)
	return nil
}

// updateCommunity updates an existing community from the firehose
func (c *CommunityEventConsumer) updateCommunity(ctx context.Context, did string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("community profile update event missing record data")
	}

	// REJECT non-V2 communities (pre-production: no V1 compatibility)
	if commit.RKey != "self" {
		return fmt.Errorf("invalid community profile rkey: expected 'self', got '%s' (V1 communities not supported)", commit.RKey)
	}

	// Parse profile
	profile, err := parseCommunityProfile(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse community profile: %w", err)
	}

	// V2: Repository DID IS the community DID
	// Get existing community using the repo DID
	existing, err := c.repo.GetByDID(ctx, did)
	if err != nil {
		if communities.IsNotFound(err) {
			// Community doesn't exist yet - treat as create
			log.Printf("Community not found for update, creating: %s", did)
			return c.createCommunity(ctx, did, commit)
		}
		return fmt.Errorf("failed to get existing community: %w", err)
	}

	// Update fields
	existing.Handle = profile.Handle
	existing.Name = profile.Name
	existing.DisplayName = profile.DisplayName
	existing.Description = profile.Description
	existing.Visibility = profile.Visibility
	existing.AllowExternalDiscovery = profile.Federation.AllowExternalDiscovery
	existing.ModerationType = profile.ModerationType
	existing.ContentWarnings = profile.ContentWarnings
	existing.RecordCID = commit.CID

	// Update blobs
	if avatarCID, ok := extractBlobCID(profile.Avatar); ok {
		existing.AvatarCID = avatarCID
	}
	if bannerCID, ok := extractBlobCID(profile.Banner); ok {
		existing.BannerCID = bannerCID
	}

	// Update description facets
	if profile.DescriptionFacets != nil {
		facetsJSON, marshalErr := json.Marshal(profile.DescriptionFacets)
		if marshalErr == nil {
			existing.DescriptionFacets = facetsJSON
		}
	}

	// Save updates
	_, err = c.repo.Update(ctx, existing)
	if err != nil {
		return fmt.Errorf("failed to update community: %w", err)
	}

	log.Printf("Updated community: %s (%s)", existing.Handle, existing.DID)
	return nil
}

// deleteCommunity removes a community from the index
func (c *CommunityEventConsumer) deleteCommunity(ctx context.Context, did string) error {
	err := c.repo.Delete(ctx, did)
	if err != nil {
		if communities.IsNotFound(err) {
			log.Printf("Community already deleted: %s", did)
			return nil
		}
		return fmt.Errorf("failed to delete community: %w", err)
	}

	log.Printf("Deleted community: %s", did)
	return nil
}

// handleSubscription indexes a subscription event
func (c *CommunityEventConsumer) handleSubscription(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Operation != "create" {
		return nil // Subscriptions are only created, not updated
	}

	if commit.Record == nil {
		return fmt.Errorf("subscription event missing record data")
	}

	// Extract community DID from record
	communityDID, ok := commit.Record["community"].(string)
	if !ok {
		return fmt.Errorf("subscription record missing community field")
	}

	// Build AT-URI for subscription record
	uri := fmt.Sprintf("at://%s/social.coves.community.subscribe/%s", userDID, commit.RKey)

	// Create subscription
	subscription := &communities.Subscription{
		UserDID:      userDID,
		CommunityDID: communityDID,
		SubscribedAt: time.Now(),
		RecordURI:    uri,
		RecordCID:    commit.CID,
	}

	// Use transactional method to ensure subscription and count are atomically updated
	// This is idempotent - safe for Jetstream replays
	_, err := c.repo.SubscribeWithCount(ctx, subscription)
	if err != nil {
		return fmt.Errorf("failed to index subscription: %w", err)
	}

	log.Printf("Indexed subscription: %s -> %s", userDID, communityDID)
	return nil
}

// handleUnsubscribe removes a subscription
func (c *CommunityEventConsumer) handleUnsubscribe(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Operation != "delete" {
		return nil
	}

	// For unsubscribe, we need to extract the community DID from the record key or metadata
	// This might need adjustment based on actual Jetstream structure
	if commit.Record == nil {
		return fmt.Errorf("unsubscribe event missing record data")
	}

	communityDID, ok := commit.Record["community"].(string)
	if !ok {
		return fmt.Errorf("unsubscribe record missing community field")
	}

	// Use transactional method to ensure unsubscribe and count are atomically updated
	// This is idempotent - safe for Jetstream replays
	err := c.repo.UnsubscribeWithCount(ctx, userDID, communityDID)
	if err != nil {
		return fmt.Errorf("failed to remove subscription: %w", err)
	}

	log.Printf("Removed subscription: %s -> %s", userDID, communityDID)
	return nil
}

// Helper types and functions

type CommunityProfile struct {
	CreatedAt         time.Time              `json:"createdAt"`
	Avatar            map[string]interface{} `json:"avatar"`
	Banner            map[string]interface{} `json:"banner"`
	CreatedBy         string                 `json:"createdBy"`
	Visibility        string                 `json:"visibility"`
	AtprotoHandle     string                 `json:"atprotoHandle"`
	DisplayName       string                 `json:"displayName"`
	Name              string                 `json:"name"`
	Handle            string                 `json:"handle"`
	HostedBy          string                 `json:"hostedBy"`
	Description       string                 `json:"description"`
	FederatedID       string                 `json:"federatedId"`
	ModerationType    string                 `json:"moderationType"`
	FederatedFrom     string                 `json:"federatedFrom"`
	ContentWarnings   []string               `json:"contentWarnings"`
	DescriptionFacets []interface{}          `json:"descriptionFacets"`
	MemberCount       int                    `json:"memberCount"`
	SubscriberCount   int                    `json:"subscriberCount"`
	Federation        FederationConfig       `json:"federation"`
}

type FederationConfig struct {
	AllowExternalDiscovery bool `json:"allowExternalDiscovery"`
}

// parseCommunityProfile converts a raw record map to a CommunityProfile
func parseCommunityProfile(record map[string]interface{}) (*CommunityProfile, error) {
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}

	var profile CommunityProfile
	if err := json.Unmarshal(recordJSON, &profile); err != nil {
		return nil, fmt.Errorf("failed to unmarshal profile: %w", err)
	}

	return &profile, nil
}

// extractBlobCID extracts the CID from a blob reference
// Blob format: {"$type": "blob", "ref": {"$link": "cid"}, "mimeType": "...", "size": 123}
func extractBlobCID(blob map[string]interface{}) (string, bool) {
	if blob == nil {
		return "", false
	}

	// Check if it's a blob type
	blobType, ok := blob["$type"].(string)
	if !ok || blobType != "blob" {
		return "", false
	}

	// Extract ref
	ref, ok := blob["ref"].(map[string]interface{})
	if !ok {
		return "", false
	}

	// Extract $link (the CID)
	link, ok := ref["$link"].(string)
	if !ok {
		return "", false
	}

	return link, true
}
