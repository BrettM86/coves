package posts

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/core/unfurl"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

type postService struct {
	repo              Repository
	communityService  communities.Service
	aggregatorService aggregators.Service
	blobService       blobs.Service
	unfurlService     unfurl.Service
	pdsURL            string
}

// NewPostService creates a new post service
// aggregatorService, blobService, and unfurlService can be nil if not needed (e.g., in tests or minimal setups)
func NewPostService(
	repo Repository,
	communityService communities.Service,
	aggregatorService aggregators.Service, // Optional: can be nil
	blobService blobs.Service, // Optional: can be nil
	unfurlService unfurl.Service, // Optional: can be nil
	pdsURL string,
) Service {
	return &postService{
		repo:              repo,
		communityService:  communityService,
		aggregatorService: aggregatorService,
		blobService:       blobService,
		unfurlService:     unfurlService,
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
	// 1. Validate basic input (before DID checks to give clear validation errors)
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}

	// 2. SECURITY: Extract authenticated DID from context (set by JWT middleware)
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

	// 3. Determine actor type: Kagi aggregator, other aggregator, or regular user
	kagiAggregatorDID := os.Getenv("KAGI_AGGREGATOR_DID")
	isTrustedKagi := kagiAggregatorDID != "" && req.AuthorDID == kagiAggregatorDID

	// Check if this is a non-Kagi aggregator (requires database lookup)
	var isOtherAggregator bool
	var err error
	if !isTrustedKagi && s.aggregatorService != nil {
		isOtherAggregator, err = s.aggregatorService.IsAggregator(ctx, req.AuthorDID)
		if err != nil {
			log.Printf("[POST-CREATE] Warning: failed to check if DID is aggregator: %v", err)
			// Don't fail the request - treat as regular user if check fails
			isOtherAggregator = false
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

	// 5. AUTHORIZATION: For non-Kagi aggregators, validate authorization and rate limits
	// Kagi is exempted from database checks via env var (temporary until XRPC endpoint is ready)
	if isOtherAggregator && s.aggregatorService != nil {
		if err := s.aggregatorService.ValidateAggregatorPost(ctx, req.AuthorDID, communityDID); err != nil {
			log.Printf("[POST-CREATE] Aggregator authorization failed: %s -> %s: %v", req.AuthorDID, communityDID, err)
			return nil, fmt.Errorf("aggregator not authorized: %w", err)
		}
		log.Printf("[POST-CREATE] Aggregator authorized: %s -> %s", req.AuthorDID, communityDID)
	}

	// 6. Fetch community from AppView (includes all metadata)
	community, err := s.communityService.GetByDID(ctx, communityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			return nil, ErrCommunityNotFound
		}
		return nil, fmt.Errorf("failed to fetch community: %w", err)
	}

	// 7. Apply validation based on actor type (aggregator vs user)
	if isTrustedKagi {
		// TRUSTED AGGREGATOR VALIDATION FLOW
		// Kagi aggregator is authorized via KAGI_AGGREGATOR_DID env var (temporary)
		// TODO: Replace with proper XRPC aggregator authorization endpoint
		log.Printf("[POST-CREATE] Trusted Kagi aggregator detected: %s posting to community: %s", req.AuthorDID, communityDID)
		// Aggregators skip membership checks and visibility restrictions
		// They are authorized services, not community members
	} else if isOtherAggregator {
		// OTHER AGGREGATOR VALIDATION FLOW
		// Authorization and rate limits already validated above via ValidateAggregatorPost
		log.Printf("[POST-CREATE] Authorized aggregator detected: %s posting to community: %s", req.AuthorDID, communityDID)
	} else {
		// USER VALIDATION FLOW
		// Check community visibility (Alpha: public/unlisted only)
		// Beta will add membership checks for private communities
		if community.Visibility == "private" {
			return nil, ErrNotAuthorized
		}
	}

	// 8. Ensure community has fresh PDS credentials (token refresh if needed)
	community, err = s.communityService.EnsureFreshToken(ctx, community)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh community credentials: %w", err)
	}

	// 9. Build post record for PDS
	postRecord := PostRecord{
		Type:           "social.coves.community.post",
		Community:      communityDID,
		Author:         req.AuthorDID,
		Title:          req.Title,
		Content:        req.Content,
		Facets:         req.Facets,
		Embed:          req.Embed, // Start with user-provided embed
		Labels:         req.Labels,
		OriginalAuthor: req.OriginalAuthor,
		FederatedFrom:  req.FederatedFrom,
		Location:       req.Location,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	// 10. Validate and enhance external embeds
	if postRecord.Embed != nil {
		if embedType, ok := postRecord.Embed["$type"].(string); ok && embedType == "social.coves.embed.external" {
			if external, ok := postRecord.Embed["external"].(map[string]interface{}); ok {
				// SECURITY: Validate thumb field (must be blob, not URL string)
				// This validation happens BEFORE unfurl to catch client errors early
				if existingThumb := external["thumb"]; existingThumb != nil {
					if thumbStr, isString := existingThumb.(string); isString {
						return nil, NewValidationError("thumb",
							fmt.Sprintf("thumb must be a blob reference (with $type, ref, mimeType, size), not URL string: %s", thumbStr))
					}

					// Validate blob structure if provided
					if thumbMap, isMap := existingThumb.(map[string]interface{}); isMap {
						// Check for $type field
						if thumbType, ok := thumbMap["$type"].(string); !ok || thumbType != "blob" {
							return nil, NewValidationError("thumb",
								fmt.Sprintf("thumb must have $type: blob (got: %v)", thumbType))
						}
						// Check for required blob fields
						if _, hasRef := thumbMap["ref"]; !hasRef {
							return nil, NewValidationError("thumb", "thumb blob missing required 'ref' field")
						}
						if _, hasMimeType := thumbMap["mimeType"]; !hasMimeType {
							return nil, NewValidationError("thumb", "thumb blob missing required 'mimeType' field")
						}
						log.Printf("[POST-CREATE] Client provided valid thumbnail blob")
					} else {
						return nil, NewValidationError("thumb",
							fmt.Sprintf("thumb must be a blob object, got: %T", existingThumb))
					}
				}

				// TRUSTED AGGREGATOR: Allow Kagi aggregator to provide thumbnail URLs directly
				// This bypasses unfurl for more accurate RSS-sourced thumbnails
				if req.ThumbnailURL != nil && *req.ThumbnailURL != "" && isTrustedKagi {
					log.Printf("[AGGREGATOR-THUMB] Trusted aggregator provided thumbnail: %s", *req.ThumbnailURL)

					if s.blobService != nil {
						blobCtx, blobCancel := context.WithTimeout(ctx, 15*time.Second)
						defer blobCancel()

						blob, blobErr := s.blobService.UploadBlobFromURL(blobCtx, community, *req.ThumbnailURL)
						if blobErr != nil {
							log.Printf("[AGGREGATOR-THUMB] Failed to upload thumbnail: %v", blobErr)
							// No fallback - aggregators only use RSS feed thumbnails
						} else {
							external["thumb"] = blob
							log.Printf("[AGGREGATOR-THUMB] Successfully uploaded thumbnail from trusted aggregator")
						}
					}
				}

				// Unfurl enhancement (optional, only if URL is supported)
				// Skip unfurl for trusted aggregators - they provide their own metadata
				if !isTrustedKagi {
					if uri, ok := external["uri"].(string); ok && uri != "" {
						// Check if we support unfurling this URL
						if s.unfurlService != nil && s.unfurlService.IsSupported(uri) {
							log.Printf("[POST-CREATE] Unfurling URL: %s", uri)

							// Unfurl with timeout (non-fatal if it fails)
							unfurlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
							defer cancel()

							result, err := s.unfurlService.UnfurlURL(unfurlCtx, uri)
							if err != nil {
								// Log but don't fail - user can still post with manual metadata
								log.Printf("[POST-CREATE] Warning: Failed to unfurl URL %s: %v", uri, err)
							} else {
								// Enhance embed with fetched metadata (only if client didn't provide)
								// Note: We respect client-provided values, even empty strings
								// If client sends title="", we assume they want no title
								if external["title"] == nil {
									external["title"] = result.Title
								}
								if external["description"] == nil {
									external["description"] = result.Description
								}
								// Always set metadata fields (provider, domain, type)
								external["embedType"] = result.Type
								external["provider"] = result.Provider
								external["domain"] = result.Domain

								// Upload thumbnail from unfurl if client didn't provide one
								// (Thumb validation already happened above)
								if external["thumb"] == nil {
									if result.ThumbnailURL != "" && s.blobService != nil {
										blobCtx, blobCancel := context.WithTimeout(ctx, 15*time.Second)
										defer blobCancel()

										blob, blobErr := s.blobService.UploadBlobFromURL(blobCtx, community, result.ThumbnailURL)
										if blobErr != nil {
											log.Printf("[POST-CREATE] Warning: Failed to upload thumbnail for %s: %v", uri, blobErr)
										} else {
											external["thumb"] = blob
											log.Printf("[POST-CREATE] Uploaded thumbnail blob for %s", uri)
										}
									}
								}

								log.Printf("[POST-CREATE] Successfully enhanced embed with unfurl data (provider: %s, type: %s)",
									result.Provider, result.Type)
							}
						}
					}
				}
			}
		}
	}

	// 11. Write to community's PDS repository
	uri, cid, err := s.createPostOnPDS(ctx, community, postRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to write post to PDS: %w", err)
	}

	// 12. Record aggregator post for rate limiting (non-Kagi aggregators only)
	// Kagi is exempted from rate limiting via env var (temporary)
	if isOtherAggregator && s.aggregatorService != nil {
		if recordErr := s.aggregatorService.RecordAggregatorPost(ctx, req.AuthorDID, communityDID, uri, cid); recordErr != nil {
			// Log but don't fail - post was already created successfully
			log.Printf("[POST-CREATE] Warning: failed to record aggregator post for rate limiting: %v", recordErr)
		}
	}

	// 13. Return response (AppView will index via Jetstream consumer)
	log.Printf("[POST-CREATE] Author: %s (trustedKagi=%v, otherAggregator=%v), Community: %s, URI: %s",
		req.AuthorDID, isTrustedKagi, isOtherAggregator, communityDID, uri)

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
