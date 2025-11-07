package posts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
)

type postService struct {
	repo              Repository
	communityService  communities.Service
	aggregatorService aggregators.Service
	pdsURL            string
}

// NewPostService creates a new post service
// aggregatorService can be nil if aggregator support is not needed (e.g., in tests or minimal setups)
func NewPostService(
	repo Repository,
	communityService communities.Service,
	aggregatorService aggregators.Service, // Optional: can be nil
	pdsURL string,
) Service {
	return &postService{
		repo:              repo,
		communityService:  communityService,
		aggregatorService: aggregatorService,
		pdsURL:            pdsURL,
	}
}

// CreatePost creates a new post in a community
// Flow:
// 1. Validate input
// 2. Check if author is an aggregator (server-side validation using DID from JWT)
// 3. If aggregator: validate authorization and rate limits, skip membership checks
// 4. If user: resolve community and perform membership/ban validation
// 5. Build post record
// 6. Write to community's PDS repository
// 7. If aggregator: record post for rate limiting
// 8. Return URI/CID (AppView indexes asynchronously via Jetstream)
func (s *postService) CreatePost(ctx context.Context, req CreatePostRequest) (*CreatePostResponse, error) {
	// 1. SECURITY: Extract authenticated DID from context (set by JWT middleware)
	// Defense-in-depth: verify service layer receives correct DID even if handler is bypassed
	authenticatedDID := middleware.GetAuthenticatedDID(ctx)
	if authenticatedDID == "" {
		return nil, fmt.Errorf("no authenticated DID in context - authentication required")
	}

	// SECURITY: Verify request DID matches authenticated DID from JWT
	// This prevents DID spoofing where a malicious client or compromised handler
	// could provide a different DID than what was authenticated
	if authenticatedDID != req.AuthorDID {
		log.Printf("[SECURITY] DID mismatch: authenticated=%s, request=%s", authenticatedDID, req.AuthorDID)
		return nil, fmt.Errorf("authenticated DID does not match author DID")
	}

	// 2. Validate basic input
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}

	// 3. SECURITY: Check if the authenticated DID is a registered aggregator
	// This is server-side verification - we query the database to confirm
	// the DID from the JWT corresponds to a registered aggregator service
	// If aggregatorService is nil (tests or environments without aggregators), treat all posts as user posts
	isAggregator := false
	if s.aggregatorService != nil {
		var err error
		isAggregator, err = s.aggregatorService.IsAggregator(ctx, req.AuthorDID)
		if err != nil {
			return nil, fmt.Errorf("failed to check if author is aggregator: %w", err)
		}
	}

	// 4. Resolve community at-identifier (handle or DID) to DID
	// This accepts both formats per atProto best practices:
	// - Handles: !gardening.communities.coves.social
	// - DIDs: did:plc:abc123 or did:web:coves.social
	communityDID, err := s.communityService.ResolveCommunityIdentifier(ctx, req.Community)
	if err != nil {
		// Handle specific error types appropriately
		if communities.IsNotFound(err) {
			return nil, ErrCommunityNotFound
		}
		if communities.IsValidationError(err) {
			// Pass through validation errors (invalid format, etc.)
			return nil, NewValidationError("community", err.Error())
		}
		// Infrastructure failures (DB errors, network issues) should be internal errors
		// Don't leak internal details to client (e.g., "pq: connection refused")
		return nil, fmt.Errorf("failed to resolve community identifier: %w", err)
	}

	// 5. Fetch community from AppView (includes all metadata)
	community, err := s.communityService.GetByDID(ctx, communityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			return nil, ErrCommunityNotFound
		}
		return nil, fmt.Errorf("failed to fetch community: %w", err)
	}

	// 6. Apply validation based on actor type (aggregator vs user)
	if isAggregator {
		// AGGREGATOR VALIDATION FLOW
		// Following Bluesky's pattern: feed generators and labelers are authorized services
		log.Printf("[POST-CREATE] Aggregator detected: %s posting to community: %s", req.AuthorDID, communityDID)

		// Check authorization exists and is enabled, and verify rate limits
		if err := s.aggregatorService.ValidateAggregatorPost(ctx, req.AuthorDID, communityDID); err != nil {
			if aggregators.IsUnauthorized(err) {
				return nil, ErrNotAuthorized
			}
			if aggregators.IsRateLimited(err) {
				return nil, ErrRateLimitExceeded
			}
			return nil, fmt.Errorf("aggregator validation failed: %w", err)
		}

		// Aggregators skip membership checks and visibility restrictions
		// They are authorized services, not community members
	} else {
		// USER VALIDATION FLOW
		// Check community visibility (Alpha: public/unlisted only)
		// Beta will add membership checks for private communities
		if community.Visibility == "private" {
			return nil, ErrNotAuthorized
		}
	}

	// 7. Ensure community has fresh PDS credentials (token refresh if needed)
	community, err = s.communityService.EnsureFreshToken(ctx, community)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh community credentials: %w", err)
	}

	// 8. Build post record for PDS
	postRecord := PostRecord{
		Type:           "social.coves.community.post",
		Community:      communityDID,
		Author:         req.AuthorDID,
		Title:          req.Title,
		Content:        req.Content,
		Facets:         req.Facets,
		Embed:          req.Embed,
		Labels:         req.Labels,
		OriginalAuthor: req.OriginalAuthor,
		FederatedFrom:  req.FederatedFrom,
		Location:       req.Location,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	// 9. Write to community's PDS repository
	uri, cid, err := s.createPostOnPDS(ctx, community, postRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to write post to PDS: %w", err)
	}

	// 10. If aggregator, record post for rate limiting and statistics
	if isAggregator && s.aggregatorService != nil {
		if err := s.aggregatorService.RecordAggregatorPost(ctx, req.AuthorDID, communityDID, uri, cid); err != nil {
			// Log error but don't fail the request (post was already created on PDS)
			log.Printf("[POST-CREATE] Warning: failed to record aggregator post for rate limiting: %v", err)
		}
	}

	// 11. Return response (AppView will index via Jetstream consumer)
	log.Printf("[POST-CREATE] Author: %s (aggregator=%v), Community: %s, URI: %s",
		req.AuthorDID, isAggregator, communityDID, uri)

	return &CreatePostResponse{
		URI: uri,
		CID: cid,
	}, nil
}

// validateCreateRequest validates basic input requirements
func (s *postService) validateCreateRequest(req CreatePostRequest) error {
	// Global content limits (from lexicon)
	const (
		maxContentLength  = 100000 // 100k characters - matches social.coves.community.post lexicon
		maxTitleLength    = 3000   // 3k bytes
		maxTitleGraphemes = 300    // 300 graphemes (simplified check)
	)

	// Validate community required
	if req.Community == "" {
		return NewValidationError("community", "community is required")
	}

	// Validate author DID set by handler
	if req.AuthorDID == "" {
		return NewValidationError("authorDid", "authorDid must be set from authenticated user")
	}

	// Validate content length
	if req.Content != nil && len(*req.Content) > maxContentLength {
		return NewValidationError("content",
			fmt.Sprintf("content too long (max %d characters)", maxContentLength))
	}

	// Validate title length
	if req.Title != nil {
		if len(*req.Title) > maxTitleLength {
			return NewValidationError("title",
				fmt.Sprintf("title too long (max %d bytes)", maxTitleLength))
		}
		// Simplified grapheme check (actual implementation would need unicode library)
		// For Alpha, byte length check is sufficient
	}

	// Validate content labels are from known values
	if req.Labels != nil {
		validLabels := map[string]bool{
			"nsfw":     true,
			"spoiler":  true,
			"violence": true,
		}
		for _, label := range req.Labels.Values {
			if !validLabels[label.Val] {
				return NewValidationError("labels",
					fmt.Sprintf("unknown content label: %s (valid: nsfw, spoiler, violence)", label.Val))
			}
		}
	}

	return nil
}

// createPostOnPDS writes a post record to the community's PDS repository
// Uses com.atproto.repo.createRecord endpoint
func (s *postService) createPostOnPDS(
	ctx context.Context,
	community *communities.Community,
	record PostRecord,
) (uri, cid string, err error) {
	// Use community's PDS URL (not service default) for federated communities
	// Each community can be hosted on a different PDS instance
	pdsURL := community.PDSURL
	if pdsURL == "" {
		// Fallback to service default if community doesn't have a PDS URL
		// (shouldn't happen in practice, but safe default)
		pdsURL = s.pdsURL
	}

	// Build PDS endpoint URL
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.createRecord", pdsURL)

	// Build request payload
	// IMPORTANT: repo is set to community DID, not author DID
	// This writes the post to the community's repository
	payload := map[string]interface{}{
		"repo":       community.DID,                 // Community's repository
		"collection": "social.coves.community.post", // Collection type
		"record":     record,                        // The post record
		// "rkey" omitted - PDS will auto-generate TID
	}

	// Marshal payload
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal post payload: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", "", fmt.Errorf("failed to create PDS request: %w", err)
	}

	// Set headers (auth + content type)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+community.PDSAccessToken)

	// Extended timeout for write operations (30 seconds)
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("PDS request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Warning: failed to close response body: %v", closeErr)
		}
	}()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read PDS response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		// Sanitize error body for logging (prevent sensitive data leakage)
		bodyPreview := string(body)
		if len(bodyPreview) > 200 {
			bodyPreview = bodyPreview[:200] + "... (truncated)"
		}
		log.Printf("[POST-CREATE-ERROR] PDS Status: %d, Body: %s", resp.StatusCode, bodyPreview)

		// Return truncated error (defense in depth - handler will mask this further)
		return "", "", fmt.Errorf("PDS returned error %d: %s", resp.StatusCode, bodyPreview)
	}

	// Parse response
	var result struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", fmt.Errorf("failed to parse PDS response: %w", err)
	}

	return result.URI, result.CID, nil
}
