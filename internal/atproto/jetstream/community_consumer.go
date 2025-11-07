package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/communities"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/time/rate"
)

// CommunityEventConsumer consumes community-related events from Jetstream
type CommunityEventConsumer struct {
	repo             communities.Repository // Repository for community operations
	identityResolver interface {
		Resolve(context.Context, string) (*identity.Identity, error)
	} // For resolving handles from DIDs
	httpClient       *http.Client                     // Shared HTTP client with connection pooling
	didCache         *lru.Cache[string, cachedDIDDoc] // Bounded LRU cache for .well-known verification results
	wellKnownLimiter *rate.Limiter                    // Rate limiter for .well-known fetches
	instanceDID      string                           // DID of this Coves instance
	skipVerification bool                             // Skip did:web verification (for dev mode)
}

// cachedDIDDoc represents a cached verification result with expiration
type cachedDIDDoc struct {
	expiresAt time.Time // When this cache entry expires
	valid     bool      // Whether verification passed
}

// NewCommunityEventConsumer creates a new Jetstream consumer for community events
// instanceDID: The DID of this Coves instance (for hostedBy verification)
// skipVerification: Skip did:web verification (for dev mode)
// identityResolver: Optional resolver for resolving handles from DIDs (can be nil for tests)
func NewCommunityEventConsumer(repo communities.Repository, instanceDID string, skipVerification bool, identityResolver interface {
	Resolve(context.Context, string) (*identity.Identity, error)
},
) *CommunityEventConsumer {
	// Create bounded LRU cache for DID document verification results
	// Max 1000 entries to prevent unbounded memory growth (PR review feedback)
	// Each entry ~100 bytes → max ~100KB memory overhead
	cache, err := lru.New[string, cachedDIDDoc](1000)
	if err != nil {
		// This should never happen with a valid size, but handle gracefully
		log.Printf("WARNING: Failed to create DID cache, verification will be slower: %v", err)
		// Create minimal cache to avoid nil pointer
		cache, _ = lru.New[string, cachedDIDDoc](1)
	}

	return &CommunityEventConsumer{
		repo:             repo,
		identityResolver: identityResolver, // Optional - can be nil for tests
		instanceDID:      instanceDID,
		skipVerification: skipVerification,
		// Shared HTTP client with connection pooling for .well-known fetches
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		// Bounded LRU cache for .well-known verification results (max 1000 entries)
		// Automatically evicts least-recently-used entries when full
		didCache: cache,
		// Rate limiter: 10 requests per second, burst of 20
		// Prevents DoS via excessive .well-known fetches
		wellKnownLimiter: rate.NewLimiter(10, 20),
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
	// IMPORTANT: Collection names refer to RECORD TYPES in repositories, not XRPC procedures
	// - social.coves.community.profile: Community profile records (in community's own repo)
	// - social.coves.community.subscription: Subscription records (in user's repo)
	// - social.coves.community.block: Block records (in user's repo)
	//
	// XRPC procedures (social.coves.community.subscribe/unsubscribe) are just HTTP endpoints
	// that CREATE or DELETE records in these collections
	switch commit.Collection {
	case "social.coves.community.profile":
		return c.handleCommunityProfile(ctx, event.Did, commit)
	case "social.coves.community.subscription":
		// Handle both create (subscribe) and delete (unsubscribe) operations
		return c.handleSubscription(ctx, event.Did, commit)
	case "social.coves.community.block":
		// Handle both create (block) and delete (unblock) operations
		return c.handleBlock(ctx, event.Did, commit)
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

	// atProto Best Practice: Handles are NOT stored in records (they're mutable, resolved from DIDs)
	// If handle is missing from record (new atProto-compliant records), resolve it from PLC/DID
	if profile.Handle == "" {
		if c.identityResolver != nil {
			// Production: Resolve handle from PLC (source of truth)
			// NO FALLBACK - if PLC is down, we fail and backfill later
			// This prevents creating communities with incorrect handles in federated scenarios
			identity, err := c.identityResolver.Resolve(ctx, did)
			if err != nil {
				return fmt.Errorf("failed to resolve handle from PLC for %s: %w (no fallback - will retry during backfill)", did, err)
			}
			profile.Handle = identity.Handle
			log.Printf("✓ Resolved handle from PLC: %s (did=%s, method=%s)",
				profile.Handle, did, identity.Method)
		} else {
			// Test mode only: construct deterministically when no resolver available
			profile.Handle = constructHandleFromProfile(profile)
			log.Printf("✓ Constructed handle (test mode): %s (name=%s, hostedBy=%s)",
				profile.Handle, profile.Name, profile.HostedBy)
		}
	}

	// SECURITY: Verify hostedBy claim matches handle domain
	// This prevents malicious instances from claiming to host communities for domains they don't own
	if err := c.verifyHostedByClaim(ctx, profile.Handle, profile.HostedBy); err != nil {
		log.Printf("🚨 SECURITY: Rejecting community %s - hostedBy verification failed: %v", did, err)
		log.Printf("    Handle: %s, HostedBy: %s", profile.Handle, profile.HostedBy)
		return fmt.Errorf("hostedBy verification failed: %w", err)
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

	// atProto Best Practice: Handles are NOT stored in records (they're mutable, resolved from DIDs)
	// If handle is missing from record (new atProto-compliant records), resolve it from PLC/DID
	if profile.Handle == "" {
		if c.identityResolver != nil {
			// Production: Resolve handle from PLC (source of truth)
			// NO FALLBACK - if PLC is down, we fail and backfill later
			// This prevents creating communities with incorrect handles in federated scenarios
			identity, err := c.identityResolver.Resolve(ctx, did)
			if err != nil {
				return fmt.Errorf("failed to resolve handle from PLC for %s: %w (no fallback - will retry during backfill)", did, err)
			}
			profile.Handle = identity.Handle
			log.Printf("✓ Resolved handle from PLC: %s (did=%s, method=%s)",
				profile.Handle, did, identity.Method)
		} else {
			// Test mode only: construct deterministically when no resolver available
			profile.Handle = constructHandleFromProfile(profile)
			log.Printf("✓ Constructed handle (test mode): %s (name=%s, hostedBy=%s)",
				profile.Handle, profile.Name, profile.HostedBy)
		}
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

// verifyHostedByClaim verifies that the community's hostedBy claim matches the handle domain
// This prevents malicious instances from claiming to host communities for domains they don't own
func (c *CommunityEventConsumer) verifyHostedByClaim(ctx context.Context, handle, hostedByDID string) error {
	// Skip verification in dev mode
	if c.skipVerification {
		return nil
	}

	// Add 15 second overall timeout to prevent slow verification from blocking consumer (PR review feedback)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Verify hostedByDID is did:web format
	if !strings.HasPrefix(hostedByDID, "did:web:") {
		return fmt.Errorf("hostedByDID must use did:web method, got: %s", hostedByDID)
	}

	// Extract domain from did:web DID
	hostedByDomain := strings.TrimPrefix(hostedByDID, "did:web:")

	// Extract domain from community handle
	// Handle format examples:
	//   - "!gaming@coves.social" → domain: "coves.social"
	//   - "gaming.community.coves.social" → domain: "coves.social"
	handleDomain := extractDomainFromHandle(handle)
	if handleDomain == "" {
		return fmt.Errorf("failed to extract domain from handle: %s", handle)
	}

	// Verify handle domain matches hostedBy domain
	if handleDomain != hostedByDomain {
		return fmt.Errorf("handle domain (%s) doesn't match hostedBy domain (%s)", handleDomain, hostedByDomain)
	}

	// Optional: Verify DID document exists and is valid
	// This provides cryptographic proof of domain ownership
	if err := c.verifyDIDDocument(ctx, hostedByDID, hostedByDomain); err != nil {
		// Soft-fail: Log warning but don't reject the community
		// This allows operation during network issues or .well-known misconfiguration
		log.Printf("⚠️  WARNING: DID document verification failed for %s: %v", hostedByDomain, err)
		log.Printf("    Community will be indexed, but hostedBy claim cannot be cryptographically verified")
	}

	return nil
}

// verifyDIDDocument fetches and validates the DID document from .well-known/did.json
// This provides cryptographic proof that the instance controls the domain
// Results are cached with TTL and rate-limited to prevent DoS attacks
func (c *CommunityEventConsumer) verifyDIDDocument(ctx context.Context, did, domain string) error {
	// Skip verification in dev mode
	if c.skipVerification {
		return nil
	}

	// Check bounded LRU cache first (thread-safe, no locks needed)
	if cached, ok := c.didCache.Get(did); ok {
		// Check if cache entry is still valid (not expired)
		if time.Now().Before(cached.expiresAt) {
			if !cached.valid {
				return fmt.Errorf("cached verification failure for %s", did)
			}
			log.Printf("✓ DID document verification (cached): %s", domain)
			return nil
		}
		// Cache entry expired - remove it to free up space for fresh entries
		c.didCache.Remove(did)
	}

	// Rate limit .well-known fetches to prevent DoS
	if err := c.wellKnownLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limit exceeded for .well-known fetch: %w", err)
	}

	// Construct .well-known URL
	didDocURL := fmt.Sprintf("https://%s/.well-known/did.json", domain)

	// Create HTTP request with timeout
	req, err := http.NewRequestWithContext(ctx, "GET", didDocURL, nil)
	if err != nil {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Fetch DID document using shared HTTP client
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Cache the failure (shorter TTL for network errors)
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("failed to fetch DID document from %s: %w", didDocURL, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Failed to close response body: %v", closeErr)
		}
	}()

	// Verify HTTP status
	if resp.StatusCode != http.StatusOK {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("DID document returned HTTP %d from %s", resp.StatusCode, didDocURL)
	}

	// Parse DID document
	var didDoc struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&didDoc); err != nil {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("failed to parse DID document JSON: %w", err)
	}

	// Verify DID document ID matches claimed DID
	if didDoc.ID != did {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("DID document ID (%s) doesn't match claimed DID (%s)", didDoc.ID, did)
	}

	// Cache the success (1 hour TTL)
	c.cacheVerificationResult(did, true, 1*time.Hour)

	log.Printf("✓ DID document verified: %s", domain)
	return nil
}

// cacheVerificationResult stores a verification result in the bounded LRU cache with the given TTL
// The LRU cache is thread-safe and automatically evicts least-recently-used entries when full
func (c *CommunityEventConsumer) cacheVerificationResult(did string, valid bool, ttl time.Duration) {
	c.didCache.Add(did, cachedDIDDoc{
		valid:     valid,
		expiresAt: time.Now().Add(ttl),
	})
}

// extractDomainFromHandle extracts the registrable domain from a community handle
// Handles both formats:
//   - Bluesky-style: "!gaming@coves.social" → "coves.social"
//   - DNS-style: "gaming.community.coves.social" → "coves.social"
//
// Uses golang.org/x/net/publicsuffix to correctly handle multi-part TLDs:
//   - "gaming.community.coves.co.uk" → "coves.co.uk" (not "co.uk")
//   - "gaming.community.example.com.au" → "example.com.au" (not "com.au")
func extractDomainFromHandle(handle string) string {
	// Remove leading ! if present
	handle = strings.TrimPrefix(handle, "!")

	// Check for @-separated format (e.g., "gaming@coves.social")
	if strings.Contains(handle, "@") {
		parts := strings.Split(handle, "@")
		if len(parts) == 2 {
			domain := parts[1]
			// Validate and extract eTLD+1 from the @-domain part
			registrable, err := publicsuffix.EffectiveTLDPlusOne(domain)
			if err != nil {
				// If publicsuffix fails, fall back to returning the full domain part
				// This handles edge cases like localhost, IP addresses, etc.
				return domain
			}
			return registrable
		}
		return ""
	}

	// For DNS-style handles (e.g., "gaming.community.coves.social")
	// Extract the registrable domain (eTLD+1) using publicsuffix
	// This correctly handles multi-part TLDs like .co.uk, .com.au, etc.
	registrable, err := publicsuffix.EffectiveTLDPlusOne(handle)
	if err != nil {
		// If publicsuffix fails (e.g., invalid TLD, localhost, IP address)
		// fall back to naive extraction (last 2 parts)
		// This maintains backward compatibility for edge cases
		parts := strings.Split(handle, ".")
		if len(parts) < 2 {
			return "" // Invalid handle
		}
		return strings.Join(parts[len(parts)-2:], ".")
	}

	return registrable
}

// handleSubscription processes subscription create/delete events
// CREATE operation = user subscribed to community
// DELETE operation = user unsubscribed from community
func (c *CommunityEventConsumer) handleSubscription(ctx context.Context, userDID string, commit *CommitEvent) error {
	switch commit.Operation {
	case "create":
		return c.createSubscription(ctx, userDID, commit)
	case "delete":
		return c.deleteSubscription(ctx, userDID, commit)
	default:
		// Update operations shouldn't happen on subscriptions, but ignore gracefully
		log.Printf("Ignoring unexpected operation on subscription: %s (userDID=%s, rkey=%s)",
			commit.Operation, userDID, commit.RKey)
		return nil
	}
}

// createSubscription indexes a new subscription with retry logic
func (c *CommunityEventConsumer) createSubscription(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("subscription create event missing record data")
	}

	// Extract community DID from record's subject field (following atProto conventions)
	communityDID, ok := commit.Record["subject"].(string)
	if !ok {
		return fmt.Errorf("subscription record missing subject field")
	}

	// Extract contentVisibility with clamping and default value
	contentVisibility := extractContentVisibility(commit.Record)

	// Build AT-URI for subscription record
	// IMPORTANT: Collection is social.coves.community.subscription (record type), not the XRPC endpoint
	// The record lives in the USER's repository, but uses the communities namespace
	uri := fmt.Sprintf("at://%s/social.coves.community.subscription/%s", userDID, commit.RKey)

	// Create subscription entity
	// Parse createdAt from record to preserve chronological ordering during replays
	subscription := &communities.Subscription{
		UserDID:           userDID,
		CommunityDID:      communityDID,
		ContentVisibility: contentVisibility,
		SubscribedAt:      utils.ParseCreatedAt(commit.Record),
		RecordURI:         uri,
		RecordCID:         commit.CID,
	}

	// Use transactional method to ensure subscription and count are atomically updated
	// This is idempotent - safe for Jetstream replays
	_, err := c.repo.SubscribeWithCount(ctx, subscription)
	if err != nil {
		// If already exists, that's fine (idempotency)
		if communities.IsConflict(err) {
			log.Printf("Subscription already indexed: %s -> %s (visibility: %d)",
				userDID, communityDID, contentVisibility)
			return nil
		}
		return fmt.Errorf("failed to index subscription: %w", err)
	}

	log.Printf("✓ Indexed subscription: %s -> %s (visibility: %d)",
		userDID, communityDID, contentVisibility)
	return nil
}

// deleteSubscription removes a subscription from the index
// DELETE operations don't include record data, so we need to look up the subscription
// by its URI to find which community the user unsubscribed from
func (c *CommunityEventConsumer) deleteSubscription(ctx context.Context, userDID string, commit *CommitEvent) error {
	// Build AT-URI from the rkey
	uri := fmt.Sprintf("at://%s/social.coves.community.subscription/%s", userDID, commit.RKey)

	// Look up the subscription to get the community DID
	// (DELETE operations don't include record data in Jetstream)
	subscription, err := c.repo.GetSubscriptionByURI(ctx, uri)
	if err != nil {
		if communities.IsNotFound(err) {
			// Already deleted - this is fine (idempotency)
			log.Printf("Subscription already deleted: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to find subscription for deletion: %w", err)
	}

	// Use transactional method to ensure unsubscribe and count are atomically updated
	// This is idempotent - safe for Jetstream replays
	err = c.repo.UnsubscribeWithCount(ctx, userDID, subscription.CommunityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			log.Printf("Subscription already removed: %s -> %s", userDID, subscription.CommunityDID)
			return nil
		}
		return fmt.Errorf("failed to remove subscription: %w", err)
	}

	log.Printf("✓ Removed subscription: %s -> %s", userDID, subscription.CommunityDID)
	return nil
}

// handleBlock processes block create/delete events
// CREATE operation = user blocked a community
// DELETE operation = user unblocked a community
func (c *CommunityEventConsumer) handleBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	switch commit.Operation {
	case "create":
		return c.createBlock(ctx, userDID, commit)
	case "delete":
		return c.deleteBlock(ctx, userDID, commit)
	default:
		// Update operations shouldn't happen on blocks, but ignore gracefully
		log.Printf("Ignoring unexpected operation on block: %s (userDID=%s, rkey=%s)",
			commit.Operation, userDID, commit.RKey)
		return nil
	}
}

// createBlock indexes a new block
func (c *CommunityEventConsumer) createBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("block create event missing record data")
	}

	// Extract community DID from record's subject field (following atProto conventions)
	communityDID, ok := commit.Record["subject"].(string)
	if !ok {
		return fmt.Errorf("block record missing subject field")
	}

	// Build AT-URI for block record
	// The record lives in the USER's repository
	uri := fmt.Sprintf("at://%s/social.coves.community.block/%s", userDID, commit.RKey)

	// Create block entity
	// Parse createdAt from record to preserve chronological ordering during replays
	block := &communities.CommunityBlock{
		UserDID:      userDID,
		CommunityDID: communityDID,
		BlockedAt:    utils.ParseCreatedAt(commit.Record),
		RecordURI:    uri,
		RecordCID:    commit.CID,
	}

	// Index the block
	// This is idempotent - safe for Jetstream replays
	_, err := c.repo.BlockCommunity(ctx, block)
	if err != nil {
		// If already exists, that's fine (idempotency)
		if communities.IsConflict(err) {
			log.Printf("Block already indexed: %s -> %s", userDID, communityDID)
			return nil
		}
		return fmt.Errorf("failed to index block: %w", err)
	}

	log.Printf("✓ Indexed block: %s -> %s", userDID, communityDID)
	return nil
}

// deleteBlock removes a block from the index
// DELETE operations don't include record data, so we need to look up the block
// by its URI to find which community the user unblocked
func (c *CommunityEventConsumer) deleteBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	// Build AT-URI from the rkey
	uri := fmt.Sprintf("at://%s/social.coves.community.block/%s", userDID, commit.RKey)

	// Look up the block to get the community DID
	// (DELETE operations don't include record data in Jetstream)
	block, err := c.repo.GetBlockByURI(ctx, uri)
	if err != nil {
		if communities.IsNotFound(err) {
			// Already deleted - this is fine (idempotency)
			log.Printf("Block already deleted: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to find block for deletion: %w", err)
	}

	// Remove the block from the index
	err = c.repo.UnblockCommunity(ctx, userDID, block.CommunityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			log.Printf("Block already removed: %s -> %s", userDID, block.CommunityDID)
			return nil
		}
		return fmt.Errorf("failed to remove block: %w", err)
	}

	log.Printf("✓ Removed block: %s -> %s", userDID, block.CommunityDID)
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

// constructHandleFromProfile constructs a deterministic handle from profile data
// Format: {name}.community.{instanceDomain}
// Example: gaming.community.coves.social
// This is ONLY used in test mode (when identity resolver is nil)
// Production MUST resolve handles from PLC (source of truth)
// Returns empty string if hostedBy is not did:web format (caller will fail validation)
func constructHandleFromProfile(profile *CommunityProfile) string {
	if !strings.HasPrefix(profile.HostedBy, "did:web:") {
		// hostedBy must be did:web format for handle construction
		// Return empty to trigger validation error in repository
		return ""
	}
	instanceDomain := strings.TrimPrefix(profile.HostedBy, "did:web:")
	return fmt.Sprintf("%s.community.%s", profile.Name, instanceDomain)
}

// extractContentVisibility extracts contentVisibility from subscription record with clamping
// Returns default value of 3 if missing or invalid
func extractContentVisibility(record map[string]interface{}) int {
	const defaultVisibility = 3

	cv, ok := record["contentVisibility"]
	if !ok {
		// Field missing - use default
		return defaultVisibility
	}

	// JSON numbers decode as float64
	cvFloat, ok := cv.(float64)
	if !ok {
		// Try int (shouldn't happen but handle gracefully)
		if cvInt, isInt := cv.(int); isInt {
			return clampContentVisibility(cvInt)
		}
		log.Printf("WARNING: contentVisibility has unexpected type %T, using default", cv)
		return defaultVisibility
	}

	// Convert and clamp
	clamped := clampContentVisibility(int(cvFloat))
	if clamped != int(cvFloat) {
		log.Printf("WARNING: Clamped contentVisibility from %d to %d", int(cvFloat), clamped)
	}
	return clamped
}

// clampContentVisibility ensures value is within valid range (1-5)
func clampContentVisibility(value int) int {
	if value < 1 {
		return 1
	}
	if value > 5 {
		return 5
	}
	return value
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
