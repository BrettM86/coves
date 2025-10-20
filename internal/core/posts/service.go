package posts

import (
	"Coves/internal/core/communities"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

type postService struct {
	repo             Repository
	communityService communities.Service
	pdsURL           string
}

// NewPostService creates a new post service
func NewPostService(
	repo Repository,
	communityService communities.Service,
	pdsURL string,
) Service {
	return &postService{
		repo:             repo,
		communityService: communityService,
		pdsURL:           pdsURL,
	}
}

// CreatePost creates a new post in a community
// Flow:
// 1. Validate input
// 2. Resolve community at-identifier (handle or DID) to DID
// 3. Fetch community from AppView
// 4. Ensure community has fresh PDS credentials
// 5. Build post record
// 6. Write to community's PDS repository
// 7. Return URI/CID (AppView indexes asynchronously via Jetstream)
func (s *postService) CreatePost(ctx context.Context, req CreatePostRequest) (*CreatePostResponse, error) {
	// 1. Validate basic input
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}

	// 2. Resolve community at-identifier (handle or DID) to DID
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

	// 3. Fetch community from AppView (includes all metadata)
	community, err := s.communityService.GetByDID(ctx, communityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			return nil, ErrCommunityNotFound
		}
		return nil, fmt.Errorf("failed to fetch community: %w", err)
	}

	// 4. Check community visibility (Alpha: public/unlisted only)
	// Beta will add membership checks for private communities
	if community.Visibility == "private" {
		return nil, ErrNotAuthorized
	}

	// 5. Ensure community has fresh PDS credentials (token refresh if needed)
	community, err = s.communityService.EnsureFreshToken(ctx, community)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh community credentials: %w", err)
	}

	// 6. Build post record for PDS
	postRecord := PostRecord{
		Type:           "social.coves.post.record",
		Community:      communityDID,
		Author:         req.AuthorDID,
		Title:          req.Title,
		Content:        req.Content,
		Facets:         req.Facets,
		Embed:          req.Embed,
		ContentLabels:  req.ContentLabels,
		OriginalAuthor: req.OriginalAuthor,
		FederatedFrom:  req.FederatedFrom,
		Location:       req.Location,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	// 7. Write to community's PDS repository
	uri, cid, err := s.createPostOnPDS(ctx, community, postRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to write post to PDS: %w", err)
	}

	// 8. Return response (AppView will index via Jetstream consumer)
	log.Printf("[POST-CREATE] Author: %s, Community: %s, URI: %s", req.AuthorDID, communityDID, uri)

	return &CreatePostResponse{
		URI: uri,
		CID: cid,
	}, nil
}

// validateCreateRequest validates basic input requirements
func (s *postService) validateCreateRequest(req CreatePostRequest) error {
	// Global content limits (from lexicon)
	const (
		maxContentLength  = 50000 // 50k characters
		maxTitleLength    = 3000  // 3k bytes
		maxTitleGraphemes = 300   // 300 graphemes (simplified check)
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
	validLabels := map[string]bool{
		"nsfw":     true,
		"spoiler":  true,
		"violence": true,
	}
	for _, label := range req.ContentLabels {
		if !validLabels[label] {
			return NewValidationError("contentLabels",
				fmt.Sprintf("unknown content label: %s (valid: nsfw, spoiler, violence)", label))
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
		"repo":       community.DID,              // Community's repository
		"collection": "social.coves.post.record", // Collection type
		"record":     record,                     // The post record
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
