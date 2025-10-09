package communities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"Coves/internal/atproto/did"
)

// Community handle validation regex (!name@instance)
var communityHandleRegex = regexp.MustCompile(`^![a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?@([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

type communityService struct {
	repo            Repository
	didGen          *did.Generator
	pdsURL          string // PDS URL for write-forward operations
	instanceDID     string // DID of this Coves instance
	pdsAccessToken  string // Access token for authenticating to PDS as the instance
}

// NewCommunityService creates a new community service
func NewCommunityService(repo Repository, didGen *did.Generator, pdsURL string, instanceDID string) Service {
	return &communityService{
		repo:        repo,
		didGen:      didGen,
		pdsURL:      pdsURL,
		instanceDID: instanceDID,
	}
}

// SetPDSAccessToken sets the PDS access token for authentication
// This should be called after creating a session for the Coves instance DID on the PDS
func (s *communityService) SetPDSAccessToken(token string) {
	s.pdsAccessToken = token
}

// CreateCommunity creates a new community via write-forward to PDS
// Flow: Service -> PDS (creates record) -> Firehose -> Consumer -> AppView DB
func (s *communityService) CreateCommunity(ctx context.Context, req CreateCommunityRequest) (*Community, error) {
	// Apply defaults before validation
	if req.Visibility == "" {
		req.Visibility = "public"
	}

	// Validate request
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}

	// Generate a unique DID for the community
	communityDID, err := s.didGen.GenerateCommunityDID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate community DID: %w", err)
	}

	// Build scoped handle: !{name}@{instance}
	instanceDomain := extractDomain(s.instanceDID)
	if instanceDomain == "" {
		instanceDomain = "coves.local" // Fallback for testing
	}
	handle := fmt.Sprintf("!%s@%s", req.Name, instanceDomain)

	// Validate the generated handle
	if err := s.ValidateHandle(handle); err != nil {
		return nil, fmt.Errorf("generated handle is invalid: %w", err)
	}

	// Build community profile record
	profile := map[string]interface{}{
		"$type":      "social.coves.community.profile",
		"did":        communityDID, // Unique identifier for this community
		"handle":     handle,
		"name":       req.Name,
		"visibility": req.Visibility,
		"owner":      s.instanceDID, // V1: instance owns the community
		"createdBy":  req.CreatedByDID,
		"hostedBy":   req.HostedByDID,
		"createdAt":  time.Now().Format(time.RFC3339),
		"federation": map[string]interface{}{
			"allowExternalDiscovery": req.AllowExternalDiscovery,
		},
	}

	// Add optional fields
	if req.DisplayName != "" {
		profile["displayName"] = req.DisplayName
	}
	if req.Description != "" {
		profile["description"] = req.Description
	}
	if len(req.Rules) > 0 {
		profile["rules"] = req.Rules
	}
	if len(req.Categories) > 0 {
		profile["categories"] = req.Categories
	}
	if req.Language != "" {
		profile["language"] = req.Language
	}

	// Initialize counts
	profile["memberCount"] = 0
	profile["subscriberCount"] = 0

	// TODO: Handle avatar and banner blobs
	// For now, we'll skip blob uploads. This would require:
	// 1. Upload blob to PDS via com.atproto.repo.uploadBlob
	// 2. Get blob ref (CID)
	// 3. Add to profile record

	// Write-forward to PDS: create the community profile record in the INSTANCE's repository
	// The instance owns all community records, community DID is just metadata in the record
	// Record will be at: at://INSTANCE_DID/social.coves.community.profile/COMMUNITY_RKEY
	recordURI, recordCID, err := s.createRecordOnPDS(ctx, s.instanceDID, "social.coves.community.profile", "", profile)
	if err != nil {
		return nil, fmt.Errorf("failed to create community on PDS: %w", err)
	}

	// Return a Community object representing what was created
	// Note: This won't be in AppView DB until the Jetstream consumer processes it
	community := &Community{
		DID:                    communityDID,
		Handle:                 handle,
		Name:                   req.Name,
		DisplayName:            req.DisplayName,
		Description:            req.Description,
		OwnerDID:               s.instanceDID,
		CreatedByDID:           req.CreatedByDID,
		HostedByDID:            req.HostedByDID,
		Visibility:             req.Visibility,
		AllowExternalDiscovery: req.AllowExternalDiscovery,
		MemberCount:            0,
		SubscriberCount:        0,
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
		RecordURI:              recordURI,
		RecordCID:              recordCID,
	}

	return community, nil
}

// GetCommunity retrieves a community from AppView DB
// identifier can be either a DID or handle
func (s *communityService) GetCommunity(ctx context.Context, identifier string) (*Community, error) {
	if identifier == "" {
		return nil, ErrInvalidInput
	}

	// Determine if identifier is DID or handle
	if strings.HasPrefix(identifier, "did:") {
		return s.repo.GetByDID(ctx, identifier)
	}

	if strings.HasPrefix(identifier, "!") {
		return s.repo.GetByHandle(ctx, identifier)
	}

	return nil, NewValidationError("identifier", "must be a DID or handle")
}

// UpdateCommunity updates a community via write-forward to PDS
func (s *communityService) UpdateCommunity(ctx context.Context, req UpdateCommunityRequest) (*Community, error) {
	if req.CommunityDID == "" {
		return nil, NewValidationError("communityDid", "required")
	}

	if req.UpdatedByDID == "" {
		return nil, NewValidationError("updatedByDid", "required")
	}

	// Get existing community
	existing, err := s.repo.GetByDID(ctx, req.CommunityDID)
	if err != nil {
		return nil, err
	}

	// Authorization: verify user is the creator
	// TODO(Communities-Auth): Add moderator check when moderation system is implemented
	if existing.CreatedByDID != req.UpdatedByDID {
		return nil, ErrUnauthorized
	}

	// Build updated profile record (start with existing)
	profile := map[string]interface{}{
		"$type":     "social.coves.community.profile",
		"handle":    existing.Handle,
		"name":      existing.Name,
		"owner":     existing.OwnerDID,
		"createdBy": existing.CreatedByDID,
		"hostedBy":  existing.HostedByDID,
		"createdAt": existing.CreatedAt.Format(time.RFC3339),
	}

	// Apply updates
	if req.DisplayName != nil {
		profile["displayName"] = *req.DisplayName
	} else {
		profile["displayName"] = existing.DisplayName
	}

	if req.Description != nil {
		profile["description"] = *req.Description
	} else {
		profile["description"] = existing.Description
	}

	if req.Visibility != nil {
		profile["visibility"] = *req.Visibility
	} else {
		profile["visibility"] = existing.Visibility
	}

	if req.AllowExternalDiscovery != nil {
		profile["federation"] = map[string]interface{}{
			"allowExternalDiscovery": *req.AllowExternalDiscovery,
		}
	} else {
		profile["federation"] = map[string]interface{}{
			"allowExternalDiscovery": existing.AllowExternalDiscovery,
		}
	}

	if req.ModerationType != nil {
		profile["moderationType"] = *req.ModerationType
	}

	if len(req.ContentWarnings) > 0 {
		profile["contentWarnings"] = req.ContentWarnings
	}

	// Preserve counts
	profile["memberCount"] = existing.MemberCount
	profile["subscriberCount"] = existing.SubscriberCount

	// Extract rkey from existing record URI (communities live in instance's repo)
	rkey := extractRKeyFromURI(existing.RecordURI)
	if rkey == "" {
		return nil, fmt.Errorf("invalid community record URI: %s", existing.RecordURI)
	}

	// Write-forward: update record on PDS using INSTANCE DID (communities are stored in instance repo)
	recordURI, recordCID, err := s.putRecordOnPDS(ctx, s.instanceDID, "social.coves.community.profile", rkey, profile)
	if err != nil {
		return nil, fmt.Errorf("failed to update community on PDS: %w", err)
	}

	// Return updated community representation
	// Actual AppView DB update happens via Jetstream consumer
	updated := *existing
	if req.DisplayName != nil {
		updated.DisplayName = *req.DisplayName
	}
	if req.Description != nil {
		updated.Description = *req.Description
	}
	if req.Visibility != nil {
		updated.Visibility = *req.Visibility
	}
	if req.AllowExternalDiscovery != nil {
		updated.AllowExternalDiscovery = *req.AllowExternalDiscovery
	}
	if req.ModerationType != nil {
		updated.ModerationType = *req.ModerationType
	}
	if len(req.ContentWarnings) > 0 {
		updated.ContentWarnings = req.ContentWarnings
	}
	updated.RecordURI = recordURI
	updated.RecordCID = recordCID
	updated.UpdatedAt = time.Now()

	return &updated, nil
}

// ListCommunities queries AppView DB for communities with filters
func (s *communityService) ListCommunities(ctx context.Context, req ListCommunitiesRequest) ([]*Community, int, error) {
	// Set defaults
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 50
	}

	return s.repo.List(ctx, req)
}

// SearchCommunities performs fuzzy search in AppView DB
func (s *communityService) SearchCommunities(ctx context.Context, req SearchCommunitiesRequest) ([]*Community, int, error) {
	if req.Query == "" {
		return nil, 0, NewValidationError("query", "search query is required")
	}

	// Set defaults
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 50
	}

	return s.repo.Search(ctx, req)
}

// SubscribeToCommunity creates a subscription via write-forward to PDS
func (s *communityService) SubscribeToCommunity(ctx context.Context, userDID, communityIdentifier string) (*Subscription, error) {
	if userDID == "" {
		return nil, NewValidationError("userDid", "required")
	}

	// Resolve community identifier to DID
	communityDID, err := s.ResolveCommunityIdentifier(ctx, communityIdentifier)
	if err != nil {
		return nil, err
	}

	// Verify community exists
	community, err := s.repo.GetByDID(ctx, communityDID)
	if err != nil {
		return nil, err
	}

	// Check visibility - can't subscribe to private communities without invitation (TODO)
	if community.Visibility == "private" {
		return nil, ErrUnauthorized
	}

	// Build subscription record
	subRecord := map[string]interface{}{
		"$type":     "social.coves.community.subscribe",
		"community": communityDID,
	}

	// Write-forward: create subscription record in user's repo
	recordURI, recordCID, err := s.createRecordOnPDS(ctx, userDID, "social.coves.community.subscribe", "", subRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to create subscription on PDS: %w", err)
	}

	// Return subscription representation
	subscription := &Subscription{
		UserDID:      userDID,
		CommunityDID: communityDID,
		SubscribedAt: time.Now(),
		RecordURI:    recordURI,
		RecordCID:    recordCID,
	}

	return subscription, nil
}

// UnsubscribeFromCommunity removes a subscription via PDS delete
func (s *communityService) UnsubscribeFromCommunity(ctx context.Context, userDID, communityIdentifier string) error {
	if userDID == "" {
		return NewValidationError("userDid", "required")
	}

	// Resolve community identifier
	communityDID, err := s.ResolveCommunityIdentifier(ctx, communityIdentifier)
	if err != nil {
		return err
	}

	// Get the subscription from AppView to find the record key
	subscription, err := s.repo.GetSubscription(ctx, userDID, communityDID)
	if err != nil {
		return err
	}

	// Extract rkey from record URI (at://did/collection/rkey)
	rkey := extractRKeyFromURI(subscription.RecordURI)
	if rkey == "" {
		return fmt.Errorf("invalid subscription record URI")
	}

	// Write-forward: delete record from PDS
	if err := s.deleteRecordOnPDS(ctx, userDID, "social.coves.community.subscribe", rkey); err != nil {
		return fmt.Errorf("failed to delete subscription on PDS: %w", err)
	}

	return nil
}

// GetUserSubscriptions queries AppView DB for user's subscriptions
func (s *communityService) GetUserSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*Subscription, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	return s.repo.ListSubscriptions(ctx, userDID, limit, offset)
}

// GetCommunitySubscribers queries AppView DB for community subscribers
func (s *communityService) GetCommunitySubscribers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*Subscription, error) {
	communityDID, err := s.ResolveCommunityIdentifier(ctx, communityIdentifier)
	if err != nil {
		return nil, err
	}

	if limit <= 0 || limit > 100 {
		limit = 50
	}

	return s.repo.ListSubscribers(ctx, communityDID, limit, offset)
}

// GetMembership retrieves membership info from AppView DB
func (s *communityService) GetMembership(ctx context.Context, userDID, communityIdentifier string) (*Membership, error) {
	communityDID, err := s.ResolveCommunityIdentifier(ctx, communityIdentifier)
	if err != nil {
		return nil, err
	}

	return s.repo.GetMembership(ctx, userDID, communityDID)
}

// ListCommunityMembers queries AppView DB for members
func (s *communityService) ListCommunityMembers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*Membership, error) {
	communityDID, err := s.ResolveCommunityIdentifier(ctx, communityIdentifier)
	if err != nil {
		return nil, err
	}

	if limit <= 0 || limit > 100 {
		limit = 50
	}

	return s.repo.ListMembers(ctx, communityDID, limit, offset)
}

// ValidateHandle checks if a community handle is valid
func (s *communityService) ValidateHandle(handle string) error {
	if handle == "" {
		return NewValidationError("handle", "required")
	}

	if !communityHandleRegex.MatchString(handle) {
		return ErrInvalidHandle
	}

	return nil
}

// ResolveCommunityIdentifier converts a handle or DID to a DID
func (s *communityService) ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) {
	if identifier == "" {
		return "", ErrInvalidInput
	}

	// If it's already a DID, return it
	if strings.HasPrefix(identifier, "did:") {
		return identifier, nil
	}

	// If it's a handle, look it up in AppView DB
	if strings.HasPrefix(identifier, "!") {
		community, err := s.repo.GetByHandle(ctx, identifier)
		if err != nil {
			return "", err
		}
		return community.DID, nil
	}

	return "", NewValidationError("identifier", "must be a DID or handle")
}

// Validation helpers

func (s *communityService) validateCreateRequest(req CreateCommunityRequest) error {
	if req.Name == "" {
		return NewValidationError("name", "required")
	}

	if len(req.Name) > 64 {
		return NewValidationError("name", "must be 64 characters or less")
	}

	// Name can only contain alphanumeric and hyphens
	nameRegex := regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,62}[a-zA-Z0-9])?$`)
	if !nameRegex.MatchString(req.Name) {
		return NewValidationError("name", "must contain only alphanumeric characters and hyphens")
	}

	if req.Description != "" && len(req.Description) > 3000 {
		return NewValidationError("description", "must be 3000 characters or less")
	}

	// Visibility should already be set with default in CreateCommunity
	if req.Visibility != "public" && req.Visibility != "unlisted" && req.Visibility != "private" {
		return ErrInvalidVisibility
	}

	if req.CreatedByDID == "" {
		return NewValidationError("createdByDid", "required")
	}

	if req.HostedByDID == "" {
		return NewValidationError("hostedByDid", "required")
	}

	return nil
}

// PDS write-forward helpers

func (s *communityService) createRecordOnPDS(ctx context.Context, repoDID, collection, rkey string, record map[string]interface{}) (string, string, error) {
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.createRecord", strings.TrimSuffix(s.pdsURL, "/"))

	payload := map[string]interface{}{
		"repo":       repoDID,
		"collection": collection,
		"record":     record,
	}

	if rkey != "" {
		payload["rkey"] = rkey
	}

	return s.callPDS(ctx, "POST", endpoint, payload)
}

func (s *communityService) putRecordOnPDS(ctx context.Context, repoDID, collection, rkey string, record map[string]interface{}) (string, string, error) {
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.putRecord", strings.TrimSuffix(s.pdsURL, "/"))

	payload := map[string]interface{}{
		"repo":       repoDID,
		"collection": collection,
		"rkey":       rkey,
		"record":     record,
	}

	return s.callPDS(ctx, "POST", endpoint, payload)
}

func (s *communityService) deleteRecordOnPDS(ctx context.Context, repoDID, collection, rkey string) error {
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.deleteRecord", strings.TrimSuffix(s.pdsURL, "/"))

	payload := map[string]interface{}{
		"repo":       repoDID,
		"collection": collection,
		"rkey":       rkey,
	}

	_, _, err := s.callPDS(ctx, "POST", endpoint, payload)
	return err
}

func (s *communityService) callPDS(ctx context.Context, method, endpoint string, payload map[string]interface{}) (string, string, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Add authentication if we have an access token
	if s.pdsAccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.pdsAccessToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to call PDS: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("PDS returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response to extract URI and CID
	var result struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// For delete operations, there might not be a response body
		if method == "POST" && strings.Contains(endpoint, "deleteRecord") {
			return "", "", nil
		}
		return "", "", fmt.Errorf("failed to parse PDS response: %w", err)
	}

	return result.URI, result.CID, nil
}

// Helper functions

func extractDomain(didOrURL string) string {
	// For did:web:example.com -> example.com
	if strings.HasPrefix(didOrURL, "did:web:") {
		parts := strings.Split(didOrURL, ":")
		if len(parts) >= 3 {
			return parts[2]
		}
	}

	// For URLs, extract domain
	if strings.Contains(didOrURL, "://") {
		parts := strings.Split(didOrURL, "://")
		if len(parts) >= 2 {
			domain := strings.Split(parts[1], "/")[0]
			domain = strings.Split(domain, ":")[0] // Remove port
			return domain
		}
	}

	return ""
}

func extractRKeyFromURI(uri string) string {
	// at://did/collection/rkey -> rkey
	parts := strings.Split(uri, "/")
	if len(parts) >= 4 {
		return parts[len(parts)-1]
	}
	return ""
}
