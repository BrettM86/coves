package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"Coves/internal/core/aggregators"
)

// AggregatorEventConsumer consumes aggregator-related events from Jetstream
// Following Bluesky's pattern: feed generators (app.bsky.feed.generator) and labelers (app.bsky.labeler.service)
type AggregatorEventConsumer struct {
	repo aggregators.Repository // Repository for aggregator operations
}

// NewAggregatorEventConsumer creates a new Jetstream consumer for aggregator events
func NewAggregatorEventConsumer(repo aggregators.Repository) *AggregatorEventConsumer {
	return &AggregatorEventConsumer{
		repo: repo,
	}
}

// HandleEvent processes a Jetstream event for aggregator records
// This is called by the main Jetstream consumer when it receives commit events
func (c *AggregatorEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for aggregator records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Route to appropriate handler based on collection
	// IMPORTANT: Collection names refer to RECORD TYPES in repositories
	// - social.coves.aggregator.service: Service declaration (in aggregator's own repo, rkey="self")
	// - social.coves.aggregator.authorization: Authorization (in community's repo, any rkey)
	switch commit.Collection {
	case "social.coves.aggregator.service":
		return c.handleServiceDeclaration(ctx, event.Did, commit)
	case "social.coves.aggregator.authorization":
		return c.handleAuthorization(ctx, event.Did, commit)
	default:
		// Not an aggregator-related collection
		return nil
	}
}

// handleServiceDeclaration processes aggregator service declaration events
// Service declarations are stored at: at://aggregator_did/social.coves.aggregator.service/self
func (c *AggregatorEventConsumer) handleServiceDeclaration(ctx context.Context, did string, commit *CommitEvent) error {
	switch commit.Operation {
	case "create", "update":
		// Both create and update are handled the same way (upsert)
		return c.upsertAggregator(ctx, did, commit)
	case "delete":
		return c.deleteAggregator(ctx, did)
	default:
		log.Printf("Unknown operation for aggregator service: %s", commit.Operation)
		return nil
	}
}

// handleAuthorization processes authorization record events
// Authorizations are stored at: at://community_did/social.coves.aggregator.authorization/{rkey}
func (c *AggregatorEventConsumer) handleAuthorization(ctx context.Context, communityDID string, commit *CommitEvent) error {
	switch commit.Operation {
	case "create", "update":
		// Both create and update are handled the same way (upsert)
		return c.upsertAuthorization(ctx, communityDID, commit)
	case "delete":
		return c.deleteAuthorization(ctx, communityDID, commit)
	default:
		log.Printf("Unknown operation for aggregator authorization: %s", commit.Operation)
		return nil
	}
}

// upsertAggregator indexes or updates an aggregator service declaration
func (c *AggregatorEventConsumer) upsertAggregator(ctx context.Context, did string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("aggregator service event missing record data")
	}

	// Verify rkey is "self" (canonical location for service declaration)
	// Following Bluesky's pattern: app.bsky.feed.generator and app.bsky.labeler.service use /self
	if commit.RKey != "self" {
		return fmt.Errorf("invalid aggregator service rkey: expected 'self', got '%s'", commit.RKey)
	}

	// Parse the service declaration record
	service, err := parseAggregatorService(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse aggregator service: %w", err)
	}

	// Validate DID matches repo DID (security check)
	if service.DID != "" && service.DID != did {
		return fmt.Errorf("service record DID (%s) does not match repo DID (%s)", service.DID, did)
	}

	// Build AT-URI for this record
	uri := fmt.Sprintf("at://%s/social.coves.aggregator.service/self", did)

	// Parse createdAt from service record
	var createdAt time.Time
	if service.CreatedAt != "" {
		createdAt, err = time.Parse(time.RFC3339, service.CreatedAt)
		if err != nil {
			createdAt = time.Now() // Fallback
			log.Printf("Warning: invalid createdAt format for aggregator %s: %v", did, err)
		}
	} else {
		createdAt = time.Now()
	}

	// Extract avatar CID from blob if present
	var avatarCID string
	if service.Avatar != nil {
		if cid, ok := extractBlobCID(service.Avatar); ok {
			avatarCID = cid
		}
	}

	// Build aggregator domain model
	agg := &aggregators.Aggregator{
		DID:           did,
		DisplayName:   service.DisplayName,
		Description:   service.Description,
		AvatarURL:     avatarCID, // Now contains the CID from blob
		MaintainerDID: service.MaintainerDID,
		SourceURL:     service.SourceURL,
		CreatedAt:     createdAt,
		IndexedAt:     time.Now(),
		RecordURI:     uri,
		RecordCID:     commit.CID,
	}

	// Handle config schema (JSONB)
	if service.ConfigSchema != nil {
		schemaBytes, err := json.Marshal(service.ConfigSchema)
		if err != nil {
			return fmt.Errorf("failed to marshal config schema: %w", err)
		}
		agg.ConfigSchema = schemaBytes
	}

	// Create or update in database
	if err := c.repo.CreateAggregator(ctx, agg); err != nil {
		return fmt.Errorf("failed to index aggregator: %w", err)
	}

	log.Printf("[AGGREGATOR-CONSUMER] Indexed service: %s (%s)", agg.DisplayName, did)
	return nil
}

// deleteAggregator removes an aggregator from the index
func (c *AggregatorEventConsumer) deleteAggregator(ctx context.Context, did string) error {
	// Delete from database (cascade deletes authorizations and posts via FK)
	if err := c.repo.DeleteAggregator(ctx, did); err != nil {
		// Log but don't fail if not found (idempotent delete)
		if aggregators.IsNotFound(err) {
			log.Printf("[AGGREGATOR-CONSUMER] Aggregator not found for deletion: %s (already deleted?)", did)
			return nil
		}
		return fmt.Errorf("failed to delete aggregator: %w", err)
	}

	log.Printf("[AGGREGATOR-CONSUMER] Deleted aggregator: %s", did)
	return nil
}

// upsertAuthorization indexes or updates an authorization record
func (c *AggregatorEventConsumer) upsertAuthorization(ctx context.Context, communityDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("authorization event missing record data")
	}

	// Parse the authorization record
	authRecord, err := parseAggregatorAuthorization(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse authorization: %w", err)
	}

	// Validate communityDid matches repo DID (security check)
	if authRecord.CommunityDid != "" && authRecord.CommunityDid != communityDID {
		return fmt.Errorf("authorization record communityDid (%s) does not match repo DID (%s)",
			authRecord.CommunityDid, communityDID)
	}

	// Build AT-URI for this record
	uri := fmt.Sprintf("at://%s/social.coves.aggregator.authorization/%s", communityDID, commit.RKey)

	// Parse createdAt from authorization record
	var createdAt time.Time
	if authRecord.CreatedAt != "" {
		createdAt, err = time.Parse(time.RFC3339, authRecord.CreatedAt)
		if err != nil {
			createdAt = time.Now() // Fallback
			log.Printf("Warning: invalid createdAt format for authorization %s: %v", uri, err)
		}
	} else {
		createdAt = time.Now()
	}

	// Parse disabledAt from authorization record (optional, for modlog/audit)
	var disabledAt *time.Time
	if authRecord.DisabledAt != "" {
		parsed, err := time.Parse(time.RFC3339, authRecord.DisabledAt)
		if err != nil {
			log.Printf("Warning: invalid disabledAt format for authorization %s: %v", uri, err)
		} else {
			disabledAt = &parsed
		}
	}

	// Build authorization domain model
	auth := &aggregators.Authorization{
		AggregatorDID: authRecord.Aggregator,
		CommunityDID:  communityDID,
		Enabled:       authRecord.Enabled,
		CreatedBy:     authRecord.CreatedBy,
		DisabledBy:    authRecord.DisabledBy,
		DisabledAt:    disabledAt,
		CreatedAt:     createdAt,
		IndexedAt:     time.Now(),
		RecordURI:     uri,
		RecordCID:     commit.CID,
	}

	// Handle config (JSONB)
	if authRecord.Config != nil {
		configBytes, err := json.Marshal(authRecord.Config)
		if err != nil {
			return fmt.Errorf("failed to marshal config: %w", err)
		}
		auth.Config = configBytes
	}

	// Create or update in database
	if err := c.repo.CreateAuthorization(ctx, auth); err != nil {
		return fmt.Errorf("failed to index authorization: %w", err)
	}

	log.Printf("[AGGREGATOR-CONSUMER] Indexed authorization: community=%s, aggregator=%s, enabled=%v",
		communityDID, authRecord.Aggregator, authRecord.Enabled)
	return nil
}

// deleteAuthorization removes an authorization from the index
func (c *AggregatorEventConsumer) deleteAuthorization(ctx context.Context, communityDID string, commit *CommitEvent) error {
	// Build AT-URI to find the authorization
	uri := fmt.Sprintf("at://%s/social.coves.aggregator.authorization/%s", communityDID, commit.RKey)

	// Delete from database
	if err := c.repo.DeleteAuthorizationByURI(ctx, uri); err != nil {
		// Log but don't fail if not found (idempotent delete)
		if aggregators.IsNotFound(err) {
			log.Printf("[AGGREGATOR-CONSUMER] Authorization not found for deletion: %s (already deleted?)", uri)
			return nil
		}
		return fmt.Errorf("failed to delete authorization: %w", err)
	}

	log.Printf("[AGGREGATOR-CONSUMER] Deleted authorization: %s", uri)
	return nil
}

// ===== Record Parsing Functions =====

// AggregatorServiceRecord represents the service declaration record structure
type AggregatorServiceRecord struct {
	Type          string                 `json:"$type"`
	DID           string                 `json:"did"` // DID of aggregator (must match repo DID)
	DisplayName   string                 `json:"displayName"`
	Description   string                 `json:"description,omitempty"`
	Avatar        map[string]interface{} `json:"avatar,omitempty"`       // Blob reference (CID will be extracted)
	ConfigSchema  map[string]interface{} `json:"configSchema,omitempty"` // JSON Schema
	MaintainerDID string                 `json:"maintainer,omitempty"`   // Fixed: was maintainerDid
	SourceURL     string                 `json:"sourceUrl,omitempty"`    // Fixed: was homepageUrl
	CreatedAt     string                 `json:"createdAt"`
}

// parseAggregatorService parses an aggregator service record
func parseAggregatorService(record interface{}) (*AggregatorServiceRecord, error) {
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}

	var service AggregatorServiceRecord
	if err := json.Unmarshal(recordBytes, &service); err != nil {
		return nil, fmt.Errorf("failed to unmarshal service record: %w", err)
	}

	// Validate required fields
	if service.DisplayName == "" {
		return nil, fmt.Errorf("displayName is required")
	}

	return &service, nil
}

// Note: extractBlobCID is defined in community_consumer.go and shared across consumers

// AggregatorAuthorizationRecord represents the authorization record structure
type AggregatorAuthorizationRecord struct {
	Config       map[string]interface{} `json:"config,omitempty"`
	Type         string                 `json:"$type"`
	Aggregator   string                 `json:"aggregatorDid"`
	CommunityDid string                 `json:"communityDid"`
	CreatedBy    string                 `json:"createdBy"`
	DisabledBy   string                 `json:"disabledBy,omitempty"`
	DisabledAt   string                 `json:"disabledAt,omitempty"`
	CreatedAt    string                 `json:"createdAt"`
	Enabled      bool                   `json:"enabled"`
}

// parseAggregatorAuthorization parses an aggregator authorization record
func parseAggregatorAuthorization(record interface{}) (*AggregatorAuthorizationRecord, error) {
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}

	var auth AggregatorAuthorizationRecord
	if err := json.Unmarshal(recordBytes, &auth); err != nil {
		return nil, fmt.Errorf("failed to unmarshal authorization record: %w", err)
	}

	// Validate required fields per lexicon
	if auth.Aggregator == "" {
		return nil, fmt.Errorf("aggregatorDid is required")
	}
	if auth.CommunityDid == "" {
		return nil, fmt.Errorf("communityDid is required")
	}
	if auth.CreatedAt == "" {
		return nil, fmt.Errorf("createdAt is required")
	}
	if auth.CreatedBy == "" {
		return nil, fmt.Errorf("createdBy is required")
	}

	return &auth, nil
}
