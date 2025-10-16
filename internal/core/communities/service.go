package communities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Community handle validation regex (DNS-valid handle: name.communities.instance.com)
// Matches standard DNS hostname format (RFC 1035)
var communityHandleRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

type communityService struct {
	repo           Repository
	provisioner    *PDSAccountProvisioner
	pdsURL         string
	instanceDID    string
	instanceDomain string
	pdsAccessToken string
}

// NewCommunityService creates a new community service
func NewCommunityService(repo Repository, pdsURL, instanceDID, instanceDomain string, provisioner *PDSAccountProvisioner) Service {
	return &communityService{
		repo:           repo,
		pdsURL:         pdsURL,
		instanceDID:    instanceDID,
		instanceDomain: instanceDomain,
		provisioner:    provisioner,
	}
}

// SetPDSAccessToken sets the PDS access token for authentication
// This should be called after creating a session for the Coves instance DID on the PDS
func (s *communityService) SetPDSAccessToken(token string) {
	s.pdsAccessToken = token
}

// CreateCommunity creates a new community via write-forward to PDS
// V2 Flow:
// 1. Service creates PDS account for community (PDS generates signing keypair)
// 2. Service writes community profile to COMMUNITY's own repository
// 3. Firehose emits event
// 4. Consumer indexes to AppView DB
//
// V2 Architecture:
// - Community owns its own repository (at://community_did/social.coves.community.profile/self)
// - PDS manages the signing keypair (we never see it)
// - We store PDS credentials to act on behalf of the community
// - Community can migrate to other instances (future V2.1 with rotation keys)
func (s *communityService) CreateCommunity(ctx context.Context, req CreateCommunityRequest) (*Community, error) {
	// Apply defaults before validation
	if req.Visibility == "" {
		req.Visibility = "public"
	}

	// Validate request
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}

	// V2: Provision a real PDS account for this community
	// This calls com.atproto.server.createAccount internally
	// The PDS will:
	//   1. Generate a signing keypair (stored in PDS, we never see it)
	//   2. Create a DID (did:plc:xxx)
	//   3. Return credentials (DID, tokens)
	pdsAccount, err := s.provisioner.ProvisionCommunityAccount(ctx, req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to provision PDS account for community: %w", err)
	}

	// Validate the atProto handle
	if validateErr := s.ValidateHandle(pdsAccount.Handle); validateErr != nil {
		return nil, fmt.Errorf("generated atProto handle is invalid: %w", validateErr)
	}

	// Build community profile record
	profile := map[string]interface{}{
		"$type":      "social.coves.community.profile",
		"handle":     pdsAccount.Handle, // atProto handle (e.g., gaming.communities.coves.social)
		"name":       req.Name,          // Short name for !mentions (e.g., "gaming")
		"visibility": req.Visibility,
		"hostedBy":   s.instanceDID, // V2: Instance hosts, community owns
		"createdBy":  req.CreatedByDID,
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

	// V2: Write to COMMUNITY's own repository (not instance repo!)
	// Repository: at://COMMUNITY_DID/social.coves.community.profile/self
	// Authenticate using community's access token
	recordURI, recordCID, err := s.createRecordOnPDSAs(
		ctx,
		pdsAccount.DID, // repo = community's DID (community owns its repo!)
		"social.coves.community.profile",
		"self", // canonical rkey for profile
		profile,
		pdsAccount.AccessToken, // authenticate as the community
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create community profile record: %w", err)
	}

	// Build Community object with PDS credentials AND cryptographic keys
	community := &Community{
		DID:                    pdsAccount.DID,    // Community's DID (owns the repo!)
		Handle:                 pdsAccount.Handle, // atProto handle (e.g., gaming.communities.coves.social)
		Name:                   req.Name,
		DisplayName:            req.DisplayName,
		Description:            req.Description,
		OwnerDID:               pdsAccount.DID, // V2: Community owns itself
		CreatedByDID:           req.CreatedByDID,
		HostedByDID:            req.HostedByDID,
		PDSEmail:               pdsAccount.Email,
		PDSPassword:            pdsAccount.Password,
		PDSAccessToken:         pdsAccount.AccessToken,
		PDSRefreshToken:        pdsAccount.RefreshToken,
		PDSURL:                 pdsAccount.PDSURL,
		Visibility:             req.Visibility,
		AllowExternalDiscovery: req.AllowExternalDiscovery,
		MemberCount:            0,
		SubscriberCount:        0,
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
		RecordURI:              recordURI,
		RecordCID:              recordCID,
		// V2: Cryptographic keys for portability (will be encrypted by repository)
		RotationKeyPEM: pdsAccount.RotationKeyPEM, // CRITICAL: Enables DID migration
		SigningKeyPEM:  pdsAccount.SigningKeyPEM,  // For atproto operations
	}

	// CRITICAL: Persist PDS credentials immediately to database
	// The Jetstream consumer will eventually index the community profile from the firehose,
	// but it won't have the PDS credentials. We must store them now so we can:
	// 1. Update the community profile later (using its own credentials)
	// 2. Re-authenticate if access tokens expire
	_, err = s.repo.Create(ctx, community)
	if err != nil {
		return nil, fmt.Errorf("failed to persist community with credentials: %w", err)
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

	// Preserve moderation settings (even if empty)
	// These fields are optional but should not be erased on update
	if req.ModerationType != nil {
		profile["moderationType"] = *req.ModerationType
	} else if existing.ModerationType != "" {
		profile["moderationType"] = existing.ModerationType
	}

	if len(req.ContentWarnings) > 0 {
		profile["contentWarnings"] = req.ContentWarnings
	} else if len(existing.ContentWarnings) > 0 {
		profile["contentWarnings"] = existing.ContentWarnings
	}

	// Preserve counts
	profile["memberCount"] = existing.MemberCount
	profile["subscriberCount"] = existing.SubscriberCount

	// V2: Community profiles always use "self" as rkey
	// (No need to extract from URI - it's always "self" for V2 communities)

	// V2 CRITICAL FIX: Write-forward using COMMUNITY's own DID and credentials
	// Repository: at://COMMUNITY_DID/social.coves.community.profile/self
	// Authenticate as the community (not as instance!)
	if existing.PDSAccessToken == "" {
		return nil, fmt.Errorf("community %s missing PDS credentials - cannot update", existing.DID)
	}

	recordURI, recordCID, err := s.putRecordOnPDSAs(
		ctx,
		existing.DID, // repo = community's own DID (V2!)
		"social.coves.community.profile",
		"self", // V2: always "self"
		profile,
		existing.PDSAccessToken, // authenticate as the community
	)
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

	// DNS label limit: 63 characters per label
	// Community handle format: {name}.communities.{instanceDomain}
	// The first label is just req.Name, so it must be <= 63 chars
	if len(req.Name) > 63 {
		return NewValidationError("name", "must be 63 characters or less (DNS label limit)")
	}

	// Name can only contain alphanumeric and hyphens
	// Must start and end with alphanumeric (not hyphen)
	nameRegex := regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
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

// createRecordOnPDSAs creates a record with a specific access token (for V2 community auth)
func (s *communityService) createRecordOnPDSAs(ctx context.Context, repoDID, collection, rkey string, record map[string]interface{}, accessToken string) (string, string, error) {
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.createRecord", strings.TrimSuffix(s.pdsURL, "/"))

	payload := map[string]interface{}{
		"repo":       repoDID,
		"collection": collection,
		"record":     record,
	}

	if rkey != "" {
		payload["rkey"] = rkey
	}

	return s.callPDSWithAuth(ctx, "POST", endpoint, payload, accessToken)
}

// putRecordOnPDSAs updates a record with a specific access token (for V2 community auth)
func (s *communityService) putRecordOnPDSAs(ctx context.Context, repoDID, collection, rkey string, record map[string]interface{}, accessToken string) (string, string, error) {
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.putRecord", strings.TrimSuffix(s.pdsURL, "/"))

	payload := map[string]interface{}{
		"repo":       repoDID,
		"collection": collection,
		"rkey":       rkey,
		"record":     record,
	}

	return s.callPDSWithAuth(ctx, "POST", endpoint, payload, accessToken)
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
	// Use instance's access token
	return s.callPDSWithAuth(ctx, method, endpoint, payload, s.pdsAccessToken)
}

// callPDSWithAuth makes a PDS call with a specific access token (V2: for community authentication)
func (s *communityService) callPDSWithAuth(ctx context.Context, method, endpoint string, payload map[string]interface{}, accessToken string) (string, string, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Add authentication with provided access token
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	// Dynamic timeout based on operation type
	// Write operations (createAccount, createRecord, putRecord) are slower due to:
	// - Keypair generation
	// - DID PLC registration
	// - Database writes on PDS
	timeout := 10 * time.Second // Default for read operations
	if strings.Contains(endpoint, "createAccount") ||
		strings.Contains(endpoint, "createRecord") ||
		strings.Contains(endpoint, "putRecord") {
		timeout = 30 * time.Second // Extended timeout for write operations
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to call PDS: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Failed to close response body: %v", closeErr)
		}
	}()

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

func extractRKeyFromURI(uri string) string {
	// at://did/collection/rkey -> rkey
	parts := strings.Split(uri, "/")
	if len(parts) >= 4 {
		return parts[len(parts)-1]
	}
	return ""
}
