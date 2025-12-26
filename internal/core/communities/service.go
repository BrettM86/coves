package communities

import (
	"Coves/internal/atproto/utils"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Community handle validation regex (DNS-valid handle: name.community.instance.com)
// Matches standard DNS hostname format (RFC 1035)
var communityHandleRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// DNS label validation (RFC 1035: 1-63 chars, alphanumeric + hyphen, can't start/end with hyphen)
var dnsLabelRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// Domain validation (simplified - checks for valid DNS hostname structure)
var domainRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

type communityService struct {
	// Interfaces and pointers first (better alignment)
	repo        Repository
	provisioner *PDSAccountProvisioner

	// Token refresh concurrency control
	// Each community gets its own mutex to prevent concurrent refresh attempts
	refreshMutexes map[string]*sync.Mutex

	// Strings
	pdsURL         string
	instanceDID    string
	instanceDomain string
	pdsAccessToken string

	// Sync primitives last
	mapMutex sync.RWMutex // Protects refreshMutexes map itself
}

const (
	// Maximum recommended size for mutex cache (warning threshold, not hard limit)
	// At 10,000 entries × 16 bytes = ~160KB memory (negligible overhead)
	// Map can grow larger in production - even 100,000 entries = 1.6MB is acceptable
	maxMutexCacheSize = 10000
)

// NewCommunityService creates a new community service
func NewCommunityService(repo Repository, pdsURL, instanceDID, instanceDomain string, provisioner *PDSAccountProvisioner) Service {
	// SECURITY: Basic validation that did:web domain matches configured instanceDomain
	// This catches honest configuration mistakes but NOT malicious code modifications
	// Full verification (Phase 2) requires fetching DID document from domain
	// See: docs/PRD_BACKLOG.md - "did:web Domain Verification"
	if strings.HasPrefix(instanceDID, "did:web:") {
		didDomain := strings.TrimPrefix(instanceDID, "did:web:")
		if didDomain != instanceDomain {
			log.Printf("⚠️  SECURITY WARNING: Instance DID domain (%s) doesn't match configured domain (%s)",
				didDomain, instanceDomain)
			log.Printf("    This could indicate a configuration error or potential domain spoofing attempt")
			log.Printf("    Communities will be hosted by: %s", instanceDID)
		}
	}

	return &communityService{
		repo:           repo,
		pdsURL:         pdsURL,
		instanceDID:    instanceDID,
		instanceDomain: instanceDomain,
		provisioner:    provisioner,
		refreshMutexes: make(map[string]*sync.Mutex),
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

	// SECURITY: Auto-populate hostedByDID from instance configuration
	// Clients MUST NOT provide this field - it's derived from the instance receiving the request
	// This prevents malicious instances from claiming to host communities for domains they don't own
	req.HostedByDID = s.instanceDID

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
		"name":       req.Name, // Short name for !mentions (e.g., "gaming")
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
		Handle:                 pdsAccount.Handle, // atProto handle (e.g., gaming.community.coves.social)
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
// identifier can be:
//   - DID: did:plc:xxx
//   - Scoped handle: !name@instance
//   - At-identifier: @c-name.domain
//   - Canonical handle: c-name.domain
func (s *communityService) GetCommunity(ctx context.Context, identifier string) (*Community, error) {
	originalIdentifier := identifier
	identifier = strings.TrimSpace(identifier)

	if identifier == "" {
		return nil, ErrInvalidInput
	}

	// 1. DID format
	if strings.HasPrefix(identifier, "did:") {
		community, err := s.repo.GetByDID(ctx, identifier)
		if err != nil {
			return nil, fmt.Errorf("community not found for identifier %q: %w", originalIdentifier, err)
		}
		return community, nil
	}

	// 2. Scoped format: !name@instance
	if strings.HasPrefix(identifier, "!") {
		// Resolve scoped identifier to DID, then fetch
		did, err := s.resolveScopedIdentifier(ctx, identifier)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve identifier %q: %w", originalIdentifier, err)
		}
		community, err := s.repo.GetByDID(ctx, did)
		if err != nil {
			return nil, fmt.Errorf("community not found for identifier %q: %w", originalIdentifier, err)
		}
		return community, nil
	}

	// 3. At-identifier format: @handle (strip @ prefix)
	identifier = strings.TrimPrefix(identifier, "@")

	// 4. Canonical handle format: c-name.domain
	if strings.Contains(identifier, ".") {
		community, err := s.repo.GetByHandle(ctx, strings.ToLower(identifier))
		if err != nil {
			return nil, fmt.Errorf("community not found for identifier %q: %w", originalIdentifier, err)
		}
		return community, nil
	}

	return nil, NewValidationError("identifier", "must be a DID, handle, or scoped identifier (!name@instance)")
}

// GetByDID retrieves a community by its DID
// Exported for use by post service when validating community references
func (s *communityService) GetByDID(ctx context.Context, did string) (*Community, error) {
	if did == "" {
		return nil, ErrInvalidInput
	}

	if !strings.HasPrefix(did, "did:") {
		return nil, NewValidationError("did", "must be a valid DID")
	}

	return s.repo.GetByDID(ctx, did)
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

	// CRITICAL: Ensure fresh PDS access token before write operation
	// Community PDS tokens expire every ~2 hours and must be refreshed
	existing, err = s.EnsureFreshToken(ctx, existing)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure fresh credentials: %w", err)
	}

	// Authorization: verify user is the creator
	// TODO(Communities-Auth): Add moderator check when moderation system is implemented
	if existing.CreatedByDID != req.UpdatedByDID {
		return nil, ErrUnauthorized
	}

	// Build updated profile record (start with existing)
	profile := map[string]interface{}{
		"$type":     "social.coves.community.profile",
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

// getOrCreateRefreshMutex returns a mutex for the given community DID
// Thread-safe with read-lock fast path for existing entries
// SAFETY: Does NOT evict entries to avoid race condition where:
//  1. Thread A holds mutex for community-123
//  2. Thread B evicts community-123 from map
//  3. Thread C creates NEW mutex for community-123
//  4. Now two threads can refresh community-123 concurrently (mutex defeated!)
func (s *communityService) getOrCreateRefreshMutex(did string) *sync.Mutex {
	// Fast path: check if mutex already exists (read lock)
	s.mapMutex.RLock()
	mutex, exists := s.refreshMutexes[did]
	s.mapMutex.RUnlock()

	if exists {
		return mutex
	}

	// Slow path: create new mutex (write lock)
	s.mapMutex.Lock()
	defer s.mapMutex.Unlock()

	// Double-check after acquiring write lock (another goroutine might have created it)
	mutex, exists = s.refreshMutexes[did]
	if exists {
		return mutex
	}

	// Create new mutex
	mutex = &sync.Mutex{}
	s.refreshMutexes[did] = mutex

	// SAFETY: No eviction to prevent race condition
	// Map will grow beyond maxMutexCacheSize but this is safer than evicting in-use mutexes
	if len(s.refreshMutexes) > maxMutexCacheSize {
		memoryKB := len(s.refreshMutexes) * 16 / 1024
		log.Printf("[TOKEN-REFRESH] WARN: Mutex cache size (%d) exceeds recommended limit (%d) - this is safe but may indicate high community churn. Memory usage: ~%d KB",
			len(s.refreshMutexes), maxMutexCacheSize, memoryKB)
	}

	return mutex
}

// ensureFreshToken checks if a community's access token needs refresh and updates if needed
// Returns updated community with fresh credentials (or original if no refresh needed)
// Thread-safe: Uses per-community mutex to prevent concurrent refresh attempts
// EnsureFreshToken ensures the community's PDS access token is valid
// Exported for use by post service when writing posts to community repos
func (s *communityService) EnsureFreshToken(ctx context.Context, community *Community) (*Community, error) {
	// Get or create mutex for this specific community DID
	mutex := s.getOrCreateRefreshMutex(community.DID)

	// Lock for this specific community (allows other communities to refresh concurrently)
	mutex.Lock()
	defer mutex.Unlock()

	// Re-fetch community from DB (another goroutine might have already refreshed it)
	fresh, err := s.repo.GetByDID(ctx, community.DID)
	if err != nil {
		return nil, fmt.Errorf("failed to re-fetch community: %w", err)
	}

	// Check if token needs refresh (5-minute buffer before expiration)
	needsRefresh, err := NeedsRefresh(fresh.PDSAccessToken)
	if err != nil {
		log.Printf("[TOKEN-REFRESH] Community: %s, Event: token_parse_failed, Error: %v", fresh.DID, err)
		return nil, fmt.Errorf("failed to check token expiration: %w", err)
	}

	if !needsRefresh {
		// Token still valid, no refresh needed
		return fresh, nil
	}

	log.Printf("[TOKEN-REFRESH] Community: %s, Event: token_refresh_started, Message: Access token expiring soon", fresh.DID)

	// Attempt token refresh using refresh token
	newAccessToken, newRefreshToken, err := refreshPDSToken(ctx, fresh.PDSURL, fresh.PDSRefreshToken)
	if err != nil {
		// Check if refresh token expired (need password fallback)
		// Match both "ExpiredToken" and "Token has expired" error messages
		if strings.Contains(strings.ToLower(err.Error()), "expired") {
			log.Printf("[TOKEN-REFRESH] Community: %s, Event: refresh_token_expired, Message: Re-authenticating with password", fresh.DID)

			// Fallback: Re-authenticate with stored password
			newAccessToken, newRefreshToken, err = reauthenticateWithPassword(
				ctx,
				fresh.PDSURL,
				fresh.PDSEmail,
				fresh.PDSPassword, // Retrieved decrypted from DB
			)
			if err != nil {
				log.Printf("[TOKEN-REFRESH] Community: %s, Event: password_auth_failed, Error: %v", fresh.DID, err)
				return nil, fmt.Errorf("failed to re-authenticate community: %w", err)
			}

			log.Printf("[TOKEN-REFRESH] Community: %s, Event: password_fallback_success, Message: Re-authenticated after refresh token expiry", fresh.DID)
		} else {
			log.Printf("[TOKEN-REFRESH] Community: %s, Event: refresh_failed, Error: %v", fresh.DID, err)
			return nil, fmt.Errorf("failed to refresh token: %w", err)
		}
	}

	// CRITICAL: Update database with new tokens immediately
	// Refresh tokens are SINGLE-USE - old one is now invalid
	// Use retry logic to handle transient DB failures
	const maxRetries = 3
	var updateErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		updateErr = s.repo.UpdateCredentials(ctx, fresh.DID, newAccessToken, newRefreshToken)
		if updateErr == nil {
			break // Success
		}

		log.Printf("[TOKEN-REFRESH] Community: %s, Event: db_update_retry, Attempt: %d/%d, Error: %v",
			fresh.DID, attempt+1, maxRetries, updateErr)

		if attempt < maxRetries-1 {
			// Exponential backoff: 100ms, 200ms, 400ms
			backoff := time.Duration(1<<attempt) * 100 * time.Millisecond
			time.Sleep(backoff)
		}
	}

	if updateErr != nil {
		// CRITICAL: Community is now locked out - old refresh token invalid, new one not saved
		log.Printf("[TOKEN-REFRESH] CRITICAL: Community %s LOCKED OUT - failed to persist credentials after %d retries: %v",
			fresh.DID, maxRetries, updateErr)
		// TODO: Send alert to monitoring system (add in Beta)
		return nil, fmt.Errorf("failed to persist refreshed credentials after %d retries (COMMUNITY LOCKED OUT): %w",
			maxRetries, updateErr)
	}

	// Return updated community object with fresh tokens
	updatedCommunity := *fresh
	updatedCommunity.PDSAccessToken = newAccessToken
	updatedCommunity.PDSRefreshToken = newRefreshToken

	log.Printf("[TOKEN-REFRESH] Community: %s, Event: token_refreshed, Message: Access token refreshed successfully", fresh.DID)

	return &updatedCommunity, nil
}

// ListCommunities queries AppView DB for communities with filters
func (s *communityService) ListCommunities(ctx context.Context, req ListCommunitiesRequest) ([]*Community, error) {
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
func (s *communityService) SubscribeToCommunity(ctx context.Context, userDID, userAccessToken, communityIdentifier string, contentVisibility int) (*Subscription, error) {
	if userDID == "" {
		return nil, NewValidationError("userDid", "required")
	}
	if userAccessToken == "" {
		return nil, NewValidationError("userAccessToken", "required")
	}

	// Clamp contentVisibility to valid range (1-5), default to 3 if 0 or invalid
	if contentVisibility <= 0 || contentVisibility > 5 {
		contentVisibility = 3
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
	// CRITICAL: Collection is social.coves.community.subscription (RECORD TYPE), not social.coves.community.subscribe (XRPC procedure)
	// This record will be created in the USER's repository: at://user_did/social.coves.community.subscription/{tid}
	// Following atProto conventions, we use "subject" field to reference the community
	subRecord := map[string]interface{}{
		"$type":             "social.coves.community.subscription",
		"subject":           communityDID, // atProto convention: "subject" for entity references
		"createdAt":         time.Now().Format(time.RFC3339),
		"contentVisibility": contentVisibility,
	}

	// Write-forward: create subscription record in user's repo using their access token
	// The collection parameter refers to the record type in the repository
	recordURI, recordCID, err := s.createRecordOnPDSAs(ctx, userDID, "social.coves.community.subscription", "", subRecord, userAccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create subscription on PDS: %w", err)
	}

	// Return subscription representation
	subscription := &Subscription{
		UserDID:           userDID,
		CommunityDID:      communityDID,
		ContentVisibility: contentVisibility,
		SubscribedAt:      time.Now(),
		RecordURI:         recordURI,
		RecordCID:         recordCID,
	}

	return subscription, nil
}

// UnsubscribeFromCommunity removes a subscription via PDS delete
func (s *communityService) UnsubscribeFromCommunity(ctx context.Context, userDID, userAccessToken, communityIdentifier string) error {
	if userDID == "" {
		return NewValidationError("userDid", "required")
	}
	if userAccessToken == "" {
		return NewValidationError("userAccessToken", "required")
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
	rkey := utils.ExtractRKeyFromURI(subscription.RecordURI)
	if rkey == "" {
		return fmt.Errorf("invalid subscription record URI")
	}

	// Write-forward: delete record from PDS using user's access token
	// CRITICAL: Delete from social.coves.community.subscription (RECORD TYPE), not social.coves.community.unsubscribe
	if err := s.deleteRecordOnPDSAs(ctx, userDID, "social.coves.community.subscription", rkey, userAccessToken); err != nil {
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

// BlockCommunity blocks a community via write-forward to PDS
func (s *communityService) BlockCommunity(ctx context.Context, userDID, userAccessToken, communityIdentifier string) (*CommunityBlock, error) {
	if userDID == "" {
		return nil, NewValidationError("userDid", "required")
	}
	if userAccessToken == "" {
		return nil, NewValidationError("userAccessToken", "required")
	}

	// Resolve community identifier (also verifies community exists)
	communityDID, err := s.ResolveCommunityIdentifier(ctx, communityIdentifier)
	if err != nil {
		return nil, err
	}

	// Build block record
	// CRITICAL: Collection is social.coves.community.block (RECORD TYPE)
	// This record will be created in the USER's repository: at://user_did/social.coves.community.block/{tid}
	// Following atProto conventions and Bluesky's app.bsky.graph.block pattern
	blockRecord := map[string]interface{}{
		"$type":     "social.coves.community.block",
		"subject":   communityDID, // DID of community being blocked
		"createdAt": time.Now().Format(time.RFC3339),
	}

	// Write-forward: create block record in user's repo using their access token
	// Note: We don't check for existing blocks first because:
	// 1. The PDS may reject duplicates (depending on implementation)
	// 2. The repository layer handles idempotency with ON CONFLICT DO NOTHING
	// 3. This avoids a race condition where two concurrent requests both pass the check
	recordURI, recordCID, err := s.createRecordOnPDSAs(ctx, userDID, "social.coves.community.block", "", blockRecord, userAccessToken)
	if err != nil {
		// Check if this is a duplicate/conflict error from PDS
		// PDS should return 409 Conflict for duplicate records, but we also check common error messages
		// for compatibility with different PDS implementations
		errMsg := err.Error()
		isDuplicate := strings.Contains(errMsg, "status 409") || // HTTP 409 Conflict
			strings.Contains(errMsg, "duplicate") ||
			strings.Contains(errMsg, "already exists") ||
			strings.Contains(errMsg, "AlreadyExists")

		if isDuplicate {
			// Fetch and return existing block from our indexed view
			existingBlock, getErr := s.repo.GetBlock(ctx, userDID, communityDID)
			if getErr == nil {
				// Block exists in our index - return it
				return existingBlock, nil
			}
			// Only treat as "already exists" if the error is ErrBlockNotFound (race condition)
			// Any other error (DB outage, connection failure, etc.) should bubble up
			if errors.Is(getErr, ErrBlockNotFound) {
				// Race condition: PDS has the block but Jetstream hasn't indexed it yet
				// Return typed conflict error so handler can return 409 instead of 500
				// This is normal in eventually-consistent systems
				return nil, ErrBlockAlreadyExists
			}
			// Real datastore error - bubble it up so operators see the failure
			return nil, fmt.Errorf("PDS reported duplicate block but failed to fetch from index: %w", getErr)
		}
		return nil, fmt.Errorf("failed to create block on PDS: %w", err)
	}

	// Return block representation
	block := &CommunityBlock{
		UserDID:      userDID,
		CommunityDID: communityDID,
		BlockedAt:    time.Now(),
		RecordURI:    recordURI,
		RecordCID:    recordCID,
	}

	return block, nil
}

// UnblockCommunity removes a block via PDS delete
func (s *communityService) UnblockCommunity(ctx context.Context, userDID, userAccessToken, communityIdentifier string) error {
	if userDID == "" {
		return NewValidationError("userDid", "required")
	}
	if userAccessToken == "" {
		return NewValidationError("userAccessToken", "required")
	}

	// Resolve community identifier
	communityDID, err := s.ResolveCommunityIdentifier(ctx, communityIdentifier)
	if err != nil {
		return err
	}

	// Get the block from AppView to find the record key
	block, err := s.repo.GetBlock(ctx, userDID, communityDID)
	if err != nil {
		return err
	}

	// Extract rkey from record URI (at://did/collection/rkey)
	rkey := utils.ExtractRKeyFromURI(block.RecordURI)
	if rkey == "" {
		return fmt.Errorf("invalid block record URI")
	}

	// Write-forward: delete record from PDS using user's access token
	if err := s.deleteRecordOnPDSAs(ctx, userDID, "social.coves.community.block", rkey, userAccessToken); err != nil {
		return fmt.Errorf("failed to delete block on PDS: %w", err)
	}

	return nil
}

// GetBlockedCommunities queries AppView DB for user's blocks
func (s *communityService) GetBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*CommunityBlock, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	return s.repo.ListBlockedCommunities(ctx, userDID, limit, offset)
}

// IsBlocked checks if a user has blocked a community
func (s *communityService) IsBlocked(ctx context.Context, userDID, communityIdentifier string) (bool, error) {
	communityDID, err := s.ResolveCommunityIdentifier(ctx, communityIdentifier)
	if err != nil {
		return false, err
	}

	return s.repo.IsBlocked(ctx, userDID, communityDID)
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

// ResolveCommunityIdentifier converts a community identifier to a DID
// Following Bluesky's pattern with Coves extensions:
//
// Accepts (like Bluesky's at-identifier):
//  1. DID: did:plc:abc123 (pass through)
//  2. Canonical handle: gardening.community.coves.social (atProto standard)
//  3. At-identifier: @gardening.community.coves.social (strip @ prefix)
//
// Coves-specific extensions:
//  4. Scoped format: !gardening@coves.social (parse and resolve)
//
// Returns: DID string
func (s *communityService) ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) {
	identifier = strings.TrimSpace(identifier)

	if identifier == "" {
		return "", ErrInvalidInput
	}

	// 1. DID - verify it exists and return (Bluesky standard)
	if strings.HasPrefix(identifier, "did:") {
		_, err := s.repo.GetByDID(ctx, identifier)
		if err != nil {
			if IsNotFound(err) {
				return "", fmt.Errorf("community not found for DID %s: %w", identifier, err)
			}
			return "", fmt.Errorf("failed to verify community DID %s: %w", identifier, err)
		}
		return identifier, nil
	}

	// 2. Scoped format: !name@instance (Coves-specific)
	if strings.HasPrefix(identifier, "!") {
		return s.resolveScopedIdentifier(ctx, identifier)
	}

	// 3. At-identifier format: @handle (Bluesky standard - strip @ prefix)
	identifier = strings.TrimPrefix(identifier, "@")

	// 4. Canonical handle: name.community.instance.com (Bluesky standard)
	if strings.Contains(identifier, ".") {
		community, err := s.repo.GetByHandle(ctx, strings.ToLower(identifier))
		if err != nil {
			return "", fmt.Errorf("community not found for handle %s: %w", identifier, err)
		}
		return community.DID, nil
	}

	return "", NewValidationError("identifier", "must be a DID, handle, or scoped identifier (!name@instance)")
}

// resolveScopedIdentifier handles Coves-specific !name@instance format
// Formats accepted:
//
//	!gardening@coves.social  -> c-gardening.coves.social
func (s *communityService) resolveScopedIdentifier(ctx context.Context, scoped string) (string, error) {
	// Remove ! prefix
	scoped = strings.TrimPrefix(scoped, "!")

	var name string
	var instanceDomain string

	// Parse !name@instance
	if !strings.Contains(scoped, "@") {
		return "", NewValidationError("identifier", "scoped identifier must include @ symbol (!name@instance)")
	}

	parts := strings.SplitN(scoped, "@", 2)
	name = strings.TrimSpace(parts[0])
	instanceDomain = strings.TrimSpace(parts[1])

	// Validate name format
	if name == "" {
		return "", NewValidationError("identifier", "community name cannot be empty")
	}

	// Validate name is a valid DNS label (RFC 1035)
	// Must be 1-63 chars, alphanumeric + hyphen, can't start/end with hyphen
	if !isValidDNSLabel(name) {
		return "", NewValidationError("identifier", "community name must be valid DNS label (alphanumeric and hyphens only, 1-63 chars, cannot start or end with hyphen)")
	}

	// Validate instance domain format
	if !isValidDomain(instanceDomain) {
		return "", NewValidationError("identifier", "invalid instance domain format")
	}

	// Normalize domain to lowercase (DNS is case-insensitive)
	// This fixes the bug where !gardening@Coves.social would fail lookup
	instanceDomain = strings.ToLower(instanceDomain)

	// Validate the instance matches this server
	if !s.isLocalInstance(instanceDomain) {
		return "", NewValidationError("identifier",
			fmt.Sprintf("community is not hosted on this instance (expected @%s)", s.instanceDomain))
	}

	// Construct canonical handle: c-{name}.{instanceDomain}
	// Both name and instanceDomain are normalized to lowercase for consistent DB lookup
	canonicalHandle := fmt.Sprintf("c-%s.%s",
		strings.ToLower(name),
		instanceDomain) // Already normalized to lowercase above

	// Look up by canonical handle
	community, err := s.repo.GetByHandle(ctx, canonicalHandle)
	if err != nil {
		return "", fmt.Errorf("community not found for scoped identifier !%s@%s: %w", name, instanceDomain, err)
	}

	return community.DID, nil
}

// isLocalInstance checks if the provided domain matches this instance
func (s *communityService) isLocalInstance(domain string) bool {
	// Normalize both domains
	domain = strings.ToLower(strings.TrimSpace(domain))
	instanceDomain := strings.ToLower(s.instanceDomain)

	// Direct match
	return domain == instanceDomain
}

// Validation helpers

// isValidDNSLabel validates that a string is a valid DNS label per RFC 1035
// - 1-63 characters
// - Alphanumeric and hyphens only
// - Cannot start or end with hyphen
func isValidDNSLabel(label string) bool {
	return dnsLabelRegex.MatchString(label)
}

// isValidDomain validates that a string is a valid domain name
// Simplified validation - checks basic DNS hostname structure
func isValidDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	return domainRegex.MatchString(domain)
}

func (s *communityService) validateCreateRequest(req CreateCommunityRequest) error {
	if req.Name == "" {
		return NewValidationError("name", "required")
	}

	// DNS label limit: 63 characters per label
	// Community handle format: {name}.community.{instanceDomain}
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

	// hostedByDID is auto-populated by the service layer, no validation needed
	// The handler ensures clients cannot provide this field

	return nil
}

// PDS write-forward helpers

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

// deleteRecordOnPDSAs deletes a record with a specific access token (for user-scoped deletions)
func (s *communityService) deleteRecordOnPDSAs(ctx context.Context, repoDID, collection, rkey, accessToken string) error {
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.deleteRecord", strings.TrimSuffix(s.pdsURL, "/"))

	payload := map[string]interface{}{
		"repo":       repoDID,
		"collection": collection,
		"rkey":       rkey,
	}

	_, _, err := s.callPDSWithAuth(ctx, "POST", endpoint, payload, accessToken)
	return err
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
