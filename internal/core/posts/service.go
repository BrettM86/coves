package posts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/pds"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/blobs"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/communities"
	"Coves/internal/core/richtext"
	"Coves/internal/core/unfurl"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

type postService struct {
	repo              Repository
	communityService  communities.Service
	aggregatorService aggregators.Service
	blobService       blobs.Service
	unfurlService     unfurl.Service
	blueskyService    blueskypost.Service
	blockChecker      BlockChecker
	admission         *AdmissionPolicy
	pdsURL            string

	// The author-owned write path (§4.2). authorRepos opens the AUTHOR's repo
	// under the author's own credentials; admissions and acceptor are the
	// local-community fast path — the row the post is seeded into and the
	// engine that settles it. See postv2.go.
	authorRepos AuthorRepoFactory
	admissions  AdmissionRepository
	acceptor    SubmissionAcceptor
}

// PostServiceOption configures optional postService dependencies. Options keep the
// constructor stable as new optional collaborators are added.
type PostServiceOption func(*postService)

// WithBlockChecker enables viewer block enforcement on GetPosts. When set and a viewer
// DID is present on the request, posts authored by users the viewer has blocked are
// returned as BlockedPost union members instead of full views, keeping permalink/
// cold-load reads consistent with feed/timeline block filtering.
func WithBlockChecker(checker BlockChecker) PostServiceOption {
	return func(s *postService) { s.blockChecker = checker }
}

// NewPostService creates a new post service
// aggregatorService, blobService, unfurlService, and blueskyService can be nil if not needed (e.g., in tests or minimal setups)
func NewPostService(
	repo Repository,
	communityService communities.Service,
	aggregatorService aggregators.Service, // Optional: can be nil
	blobService blobs.Service, // Optional: can be nil
	unfurlService unfurl.Service, // Optional: can be nil
	blueskyService blueskypost.Service, // Optional: can be nil
	pdsURL string,
	opts ...PostServiceOption,
) Service {
	s := &postService{
		repo:              repo,
		communityService:  communityService,
		aggregatorService: aggregatorService,
		blobService:       blobService,
		unfurlService:     unfurlService,
		blueskyService:    blueskyService,
		pdsURL:            pdsURL,
	}
	for _, opt := range opts {
		opt(s)
	}
	// The admission policy is mandatory, and a missing or partial one panics
	// here rather than defaulting to no-ops: a post service whose ban check and
	// quota silently do not exist is a wiring bug, not a configuration.
	mustCompleteAdmissionPolicy(s.admission)
	return s
}

// CreatePost creates a new post in a community
// Flow:
//  1. Validate input (and normalize embed/facet URIs)
//  2. Verify the authenticated DID matches the request's author DID
//  3. Classify the actor: trusted aggregator, registered aggregator, or user
//  4. Admission: one decision over community existence, visibility, ban,
//     aggregator authorization, dedupe and the per-author quota (admitPost)
//  5. Ensure the community has fresh PDS credentials (token refresh)
//  6. Build the post record
//  7. Validate and enhance external embeds (thumb validation, unfurl, blobs)
//  8. Write to community's PDS repository
//  9. If aggregator: record post for rate limiting
//  10. Return URI/CID (AppView indexes asynchronously via Jetstream)
//
// Admission runs BEFORE the token refresh, the blob uploads and the PDS write,
// so a refused submission costs a few lookups rather than an upload — and,
// more to the point, leaves no record in a community that refused it. Every
// failure AFTER admission (steps 5-8) must release the ledger reservation the
// admission took, or the failure costs the author a quota slot and refuses
// their retry as a duplicate.
func (s *postService) CreatePost(ctx context.Context, session *oauth.ClientSessionData, req CreatePostRequest) (*CreatePostResponse, error) {
	// RED STUB SEAM (task 6): the session is the author's credential and is
	// consumed by the author-repo write the GREEN cycle installs below. It is
	// accepted here so the contract compiles against the flipped signature
	// while the body still write-forwards to the community's repo.
	_ = session

	// 1. Validate basic input (before DID checks to give clear validation errors)
	if err := s.validateCreateRequest(&req); err != nil {
		return nil, err
	}

	// 1b. Normalize the fields the lexicon declares as `format: uri` before any
	// of them reach the record. Runs here, ahead of the community and PDS work,
	// so an unrecoverable URI fails fast without burning a DB lookup or an
	// unfurl fetch. Mutates req in place; the record built below reads these
	// same values, and the unfurl step then works from the encoded URI,
	// which dereferences identically.
	if err := normalizeEmbedURIs(req.Embed); err != nil {
		return nil, err
	}
	if err := richtext.NormalizeLinkURIs(req.Facets); err != nil {
		return nil, NewValidationErrorFrom("facets", err)
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

	// 3. Determine actor type: trusted aggregator, other aggregator, or regular user.
	// The allowlist is read through the shared helper rather than inline, so
	// this path and the acceptance engine's decider cannot drift into disagreeing
	// about who is trusted — see TrustedAggregatorDIDs.
	isTrustedAggregator := TrustedAggregatorDIDs()[req.AuthorDID]

	// Check if this is a non-trusted aggregator (requires database lookup)
	var isOtherAggregator bool
	if !isTrustedAggregator && s.aggregatorService != nil {
		isAggregator, err := s.aggregatorService.IsAggregator(ctx, req.AuthorDID)
		if err != nil {
			log.Printf("[POST-CREATE] Warning: failed to check if DID is aggregator: %v", err)
			// Don't fail the request - treat as regular user if check fails
			isOtherAggregator = false
		} else {
			isOtherAggregator = isAggregator
		}
	}

	// The classification the admission decision is made against. It is resolved
	// HERE, at the call site, rather than inside admitPost: "who is trusted"
	// comes out of the process environment, and a decision function that read it
	// itself would hide the most consequential input to a security decision from
	// the place that makes it.
	actor := ActorUser
	switch {
	case isTrustedAggregator:
		log.Printf("[POST-CREATE] Trusted aggregator detected: %s posting to community: %s", req.AuthorDID, req.Community)
		actor = ActorTrustedAggregator
	case isOtherAggregator:
		actor = ActorRegisteredAggregator
	}

	// 4. ADMISSION: the single decision over community existence, visibility,
	// ban, aggregator authorization, dedupe and the per-author quota (§4.1, §8).
	//
	// The fingerprint is taken from the record as the CLIENT sent it, before
	// unfurl enhancement rewrites the embed: two submissions of the same content
	// must hash the same, and an enriched embed varies with whatever the remote
	// page served at the time. The thumbnail URL rides along as submitted; the
	// client-typed community identifier and the per-attempt timestamp are
	// excluded inside submissionFingerprint (see its doc comment).
	decision, err := admitPost(ctx, s.admissionDeps(), AdmissionRequest{
		Actor:       actor,
		AuthorDID:   req.AuthorDID,
		Community:   req.Community,
		Fingerprint: submissionFingerprint(postRecordFor(req, req.Community, ""), req.ThumbnailURL),
	})
	if err != nil {
		return nil, err
	}
	if !decision.Admitted() {
		log.Printf("[POST-CREATE] Refused: author=%s, community=%s, actor=%s, code=%s",
			req.AuthorDID, req.Community, actor, decision.Code)
		return nil, refusalError(decision)
	}

	community := decision.Community
	communityDID := community.DID

	// From here on the submission holds a ledger row. Every path that fails
	// before the record exists has to give it back, or a transient failure
	// permanently costs the author a quota slot AND blocks them from retrying
	// the same content until the dedupe window rolls.
	releaseOnFailure := func() {
		if decision.Reservation != nil {
			releaseReservation(ctx, s.admission.Ledger, *decision.Reservation)
		}
	}

	// 5. Ensure community has fresh PDS credentials (token refresh if needed)
	community, err = s.communityService.EnsureFreshToken(ctx, community)
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("failed to refresh community credentials: %w", err)
	}

	// 6. Build post record for PDS
	postRecord := postRecordFor(req, communityDID, time.Now().UTC().Format(time.RFC3339))

	// 7. Validate and enhance external embeds
	if err := s.enhanceExternalEmbed(ctx, &postRecord, req, community, actor == ActorTrustedAggregator); err != nil {
		releaseOnFailure()
		return nil, err
	}

	// 8. Write to community's PDS repository
	//
	// A failure here is the case the reservation was designed around: the row
	// went in before the write precisely so two concurrent identical submissions
	// would collide on the unique key, and the cost of that ordering is that a
	// write which never happened owes the author their slot back. Without it, a
	// PDS hiccup would consume a quota slot AND refuse the retry as a duplicate,
	// turning a transient outage into a per-author lockout that outlives it.
	uri, cid, err := s.createPostOnPDS(ctx, community, postRecord)
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("failed to write post to PDS: %w", err)
	}

	// 9. Record aggregator post for rate limiting (non-Kagi aggregators only)
	// Kagi is exempted from rate limiting via env var (temporary)
	if isOtherAggregator && s.aggregatorService != nil {
		if recordErr := s.aggregatorService.RecordAggregatorPost(ctx, req.AuthorDID, communityDID, uri, cid); recordErr != nil {
			// Log but don't fail - post was already created successfully
			log.Printf("[POST-CREATE] Warning: failed to record aggregator post for rate limiting: %v", recordErr)
		}
	}

	// 10. Return response (AppView will index via Jetstream consumer)
	log.Printf("[POST-CREATE] Author: %s (trustedKagi=%v, otherAggregator=%v), Community: %s, URI: %s",
		req.AuthorDID, isTrustedAggregator, isOtherAggregator, communityDID, uri)

	return &CreatePostResponse{
		URI: uri,
		CID: cid,
	}, nil
}

// UpdatePost edits a post in place in the author's repository.
//
// RED STUB (task 6): see interfaces.go for the contract and
// service_writeflip_test.go for the pinned journey.
func (s *postService) UpdatePost(ctx context.Context, session *oauth.ClientSessionData, req UpdatePostRequest) (*UpdatePostResponse, error) {
	_, _, _ = ctx, session, req
	return nil, ErrNotFound
}

// postRecordFor builds the record a request describes, stamped with the given
// community identifier and creation time.
//
// It is shared by the submission fingerprint and the record actually written,
// so that the thing dedupe hashes and the thing the community's repo receives
// cannot drift into describing different posts. The two callers differ in
// exactly the two arguments: the fingerprint is taken before the community
// identifier has been resolved and with no timestamp at all (createdAt is
// stamped per attempt, so including it would make every retry look new).
func postRecordFor(req CreatePostRequest, community, createdAt string) PostRecord {
	return PostRecord{
		Type:           postCollection,
		Community:      community,
		Author:         req.AuthorDID,
		Title:          req.Title,
		Content:        req.Content,
		Facets:         req.Facets,
		Embed:          req.Embed, // Start with user-provided embed
		Labels:         req.Labels,
		OriginalAuthor: req.OriginalAuthor,
		FederatedFrom:  req.FederatedFrom,
		Location:       req.Location,
		CreatedAt:      createdAt,
	}
}

// enhanceExternalEmbed applies the external-embed handling that has to happen
// against a live network: Bluesky URL conversion, client thumb validation, and
// unfurl enrichment with its blob uploads.
//
// It is a method rather than inline steps because every failure inside it now
// happens with a submission reservation already on the ledger, and a caller
// that has one error return to handle can give the reservation back in one
// place instead of at each of the four validation exits.
//
// trusted marks a trusted aggregator, which supplies its own metadata and is
// unfurled only for a thumbnail it did not provide.
func (s *postService) enhanceExternalEmbed(ctx context.Context, postRecord *PostRecord, req CreatePostRequest, community *communities.Community, trusted bool) error {
	if postRecord.Embed != nil {
		embedType, typeOk := postRecord.Embed["$type"].(string)
		if typeOk && embedType == "social.coves.embed.external" {
			if external, extOk := postRecord.Embed["external"].(map[string]interface{}); extOk {
				// Check if this is a Bluesky post URL and convert to post embed
				if !s.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord) {
					// Not a Bluesky URL or conversion failed - continue with normal external embed processing
					// SECURITY: Validate thumb field (must be blob, not URL string)
					// This validation happens BEFORE unfurl to catch client errors early
					if existingThumb := external["thumb"]; existingThumb != nil {
						if thumbStr, isString := existingThumb.(string); isString {
							return NewValidationError("thumb",
								fmt.Sprintf("thumb must be a blob reference (with $type, ref, mimeType, size), not URL string: %s", thumbStr))
						}

						// Validate blob structure if provided
						if thumbMap, isMap := existingThumb.(map[string]interface{}); isMap {
							// Check for $type field
							if thumbType, ok := thumbMap["$type"].(string); !ok || thumbType != "blob" {
								return NewValidationError("thumb",
									fmt.Sprintf("thumb must have $type: blob (got: %v)", thumbType))
							}
							// Check for required blob fields
							if _, hasRef := thumbMap["ref"]; !hasRef {
								return NewValidationError("thumb", "thumb blob missing required 'ref' field")
							}
							if _, hasMimeType := thumbMap["mimeType"]; !hasMimeType {
								return NewValidationError("thumb", "thumb blob missing required 'mimeType' field")
							}
							log.Printf("[POST-CREATE] Client provided valid thumbnail blob")
						} else {
							return NewValidationError("thumb",
								fmt.Sprintf("thumb must be a blob object, got: %T", existingThumb))
						}
					}

					// TRUSTED AGGREGATOR: Allow Kagi aggregator to provide thumbnail URLs directly
					// This bypasses unfurl for more accurate RSS-sourced thumbnails
					if req.ThumbnailURL != nil && *req.ThumbnailURL != "" && trusted {
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
					// For trusted aggregators: only unfurl for thumbnail if they didn't provide one
					// For regular users: full unfurl for all metadata
					needsThumbnailUnfurl := trusted && external["thumb"] == nil && (req.ThumbnailURL == nil || *req.ThumbnailURL == "")
					needsFullUnfurl := !trusted

					if needsThumbnailUnfurl || needsFullUnfurl {
						if uri, ok := external["uri"].(string); ok && uri != "" {
							// Check if we support unfurling this URL
							if s.unfurlService != nil && s.unfurlService.IsSupported(uri) {
								log.Printf("[POST-CREATE] Unfurling URL: %s (thumbnailOnly=%v)", uri, needsThumbnailUnfurl)

								// Unfurl with timeout (non-fatal if it fails)
								unfurlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
								defer cancel()

								result, err := s.unfurlService.UnfurlURL(unfurlCtx, uri)
								if err != nil {
									// Log but don't fail - user can still post with manual metadata
									log.Printf("[POST-CREATE] Warning: Failed to unfurl URL %s: %v", uri, err)
								} else {
									// For regular users: enhance embed with fetched metadata
									// For trusted aggregators: skip metadata, they provide their own
									if needsFullUnfurl {
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
									}

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

									if needsFullUnfurl {
										log.Printf("[POST-CREATE] Successfully enhanced embed with unfurl data (provider: %s, type: %s)",
											result.Provider, result.Type)
									} else {
										log.Printf("[POST-CREATE] Fetched thumbnail via unfurl for trusted aggregator")
									}
								}
							}
						}
					}
				}
			}
		}
	}

	return nil
}

// validateCreateRequest validates basic input requirements
func (s *postService) validateCreateRequest(req *CreatePostRequest) error {
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

	// Validate facets structurally against the content they annotate.
	// Catches out-of-range byte slices at the API boundary instead of
	// persisting a record whose annotations slice outside the content.
	contentByteLen := 0
	if req.Content != nil {
		contentByteLen = len(*req.Content)
	}
	if err := richtext.ValidateFacets(req.Facets, contentByteLen); err != nil {
		return NewValidationError("facets", err.Error())
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

	// Validate the embed (if provided) matches a known lexicon union member.
	// Catches malformed embeds at the API boundary instead of silently
	// persisting an unrenderable record to the PDS.
	if err := validateEmbed(req.Embed); err != nil {
		return err
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

// tryConvertBlueskyURLToPostEmbed attempts to convert a Bluesky URL in an external embed to a post embed.
// Returns true if the conversion was successful and the postRecord was modified.
// Returns false if the URL is not a Bluesky URL or if conversion failed (caller should continue with external embed).
//
// A strongRef is an AT Protocol reference containing both URI (at://did/collection/rkey) and CID
// (content identifier hash). This function resolves the Bluesky URL to obtain both values,
// enabling rich embedded quote posts instead of plain external links.
func (s *postService) tryConvertBlueskyURLToPostEmbed(ctx context.Context, external map[string]interface{}, postRecord *PostRecord) bool {
	// 1. Check if blueskyService is available
	if s.blueskyService == nil {
		log.Printf("[POST-CREATE] BlueskyService unavailable, keeping as external embed")
		return false
	}

	// 2. Extract and validate URL
	url, ok := external["uri"].(string)
	if !ok || url == "" {
		return false
	}

	// 3. Check if it's a Bluesky URL
	if !s.blueskyService.IsBlueskyURL(url) {
		return false
	}

	// 4. Parse URL to AT-URI (resolves handle to DID if needed)
	atURI, err := s.blueskyService.ParseBlueskyURL(ctx, url)
	if err != nil {
		// Differentiate between timeout and other errors
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[POST-CREATE] WARN: Bluesky URL parse timed out, keeping as external embed: %s", url)
		} else {
			log.Printf("[POST-CREATE] Failed to parse Bluesky URL %s: %v", url, err)
		}
		return false // Fall back to external embed
	}

	// 5. Resolve post to get CID
	result, err := s.blueskyService.ResolvePost(ctx, atURI)
	if err != nil {
		// Differentiate error types for better debugging
		if errors.Is(err, blueskypost.ErrCircuitOpen) {
			log.Printf("[POST-CREATE] WARN: Bluesky circuit breaker OPEN, keeping as external embed: %s", atURI)
		} else {
			log.Printf("[POST-CREATE] Failed to resolve Bluesky post %s: %v", atURI, err)
		}
		return false // Fall back to external embed
	}

	if result == nil {
		log.Printf("[POST-CREATE] ERROR: ResolvePost returned nil result for %s", atURI)
		return false
	}

	// 6. Handle unavailable posts - keep as external embed since we can't get a valid CID
	if result.Unavailable {
		log.Printf("[POST-CREATE] Bluesky post unavailable, keeping as external embed: %s (reason: %s)", atURI, result.Message)
		return false
	}

	// 7. Validate we have both URI and CID
	if result.URI == "" || result.CID == "" {
		log.Printf("[POST-CREATE] ERROR: Bluesky post missing URI or CID (internal bug): uri=%q, cid=%q", result.URI, result.CID)
		return false
	}

	// 8. Convert embed to social.coves.embed.post with strongRef
	postRecord.Embed = map[string]interface{}{
		"$type": "social.coves.embed.post",
		"post": map[string]interface{}{
			"uri": result.URI,
			"cid": result.CID,
		},
	}

	log.Printf("[POST-CREATE] Converted Bluesky URL to post embed: %s (cid: %s)", result.URI, result.CID)
	return true
}

// GetAuthorPosts retrieves posts by a specific author with optional filtering
// Supports filtering by: posts_with_replies, posts_no_replies, posts_with_media
// Optionally filter to a specific community
func (s *postService) GetAuthorPosts(ctx context.Context, req GetAuthorPostsRequest) (*GetAuthorPostsResponse, error) {
	// 1. Validate request
	if err := s.validateGetAuthorPostsRequest(&req); err != nil {
		return nil, err
	}

	// 2. If community is provided, resolve it to DID
	if req.Community != "" {
		communityDID, err := s.communityService.ResolveCommunityIdentifier(ctx, req.Community)
		if err != nil {
			if communities.IsNotFound(err) {
				return nil, ErrCommunityNotFound
			}
			if communities.IsValidationError(err) {
				return nil, NewValidationError("community", err.Error())
			}
			return nil, fmt.Errorf("failed to resolve community identifier: %w", err)
		}
		req.Community = communityDID
	}

	// 3. Fetch posts from repository
	postViews, cursor, err := s.repo.GetByAuthor(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get author posts: %w", err)
	}

	// 4. Wrap PostViews in FeedViewPost
	feed := make([]*FeedViewPost, len(postViews))
	for i, postView := range postViews {
		feed[i] = &FeedViewPost{
			Post: postView,
		}
	}

	// 5. Return response
	return &GetAuthorPostsResponse{
		Feed:   feed,
		Cursor: cursor,
	}, nil
}

// Bounds for the social.coves.community.post.get endpoint.
const (
	postCollection = "social.coves.community.post"
	// MaxGetPostsURIs is the maximum number of URIs accepted by a single
	// social.coves.community.post.get request (matches the lexicon maxLength).
	// Exported so the handler layer reuses the same bound (single source of truth).
	MaxGetPostsURIs = 25
)

// GetPosts batch-fetches post views by AT-URI for feed hydration and permalink
// (cold-load) rendering. Implements social.coves.community.post.get.
//
// URIs must be canonical DID-based AT-URIs (at://<community-did>/social.coves.community.post/<rkey>).
// Handle-based authorities are rejected: handles are mutable, so a handle-based URI
// would break (or, if the handle is later reassigned, mis-resolve to the wrong community)
// after a rename. Resolving a human-readable handle to a DID is the caller's job, done
// once at the edge. DIDs are permanent, so DID-based URIs stay valid forever.
//
// Flow:
//  1. Validate the URI count (1..25) and that every URI is a well-formed DID-based URI.
//     A malformed or handle-based URI is a client error -> InvalidRequest, not a silent miss.
//  2. Batch fetch views for the (deduped) URIs.
//  3. Assemble results in request order; valid-but-absent URIs become notFoundPost.
//
// Viewer state (vote) and embed/blob transforms are applied by the handler layer.
func (s *postService) GetPosts(ctx context.Context, req GetPostsRequest) ([]*PostResult, error) {
	// 1. Validate batch size
	if len(req.URIs) == 0 {
		return nil, NewValidationError("uris", "at least one URI is required")
	}
	if len(req.URIs) > MaxGetPostsURIs {
		return nil, NewValidationError("uris", fmt.Sprintf("too many URIs (max %d)", MaxGetPostsURIs))
	}

	// Validate every URI up front and dedup for the batch fetch. A malformed or
	// handle-based URI fails the whole request with a clear error rather than silently
	// degrading to notFound, which would hide client bugs.
	uniqueSet := make(map[string]struct{}, len(req.URIs))
	for _, uri := range req.URIs {
		if err := validatePostURI(uri); err != nil {
			return nil, err
		}
		uniqueSet[uri] = struct{}{}
	}

	// 2. Batch fetch the (deduped) URIs
	unique := make([]string, 0, len(uniqueSet))
	for uri := range uniqueSet {
		unique = append(unique, uri)
	}
	views, err := s.repo.GetViewsByURIs(ctx, unique)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch post views: %w", err)
	}

	// 3. Assemble results in request order; valid-but-absent URIs become notFoundPost
	results := make([]*PostResult, len(req.URIs))
	for i, uri := range req.URIs {
		if view := views[uri]; view != nil {
			results[i] = foundResult(view)
		} else {
			results[i] = notFoundResult(uri)
		}
	}

	// 4. Enforce viewer block visibility: posts authored by someone the viewer has
	// blocked become blockedPost markers, consistent with feed/timeline filtering.
	// Only runs for an authenticated viewer when a block checker is wired.
	if req.ViewerDID != "" && s.blockChecker != nil {
		if err := s.applyViewerBlocks(ctx, req.ViewerDID, results); err != nil {
			return nil, err
		}
	}

	return results, nil
}

// applyViewerBlocks rewrites found posts whose author the viewer has blocked into
// blockedPost results (blockedBy "author"). It batches the block lookup over the unique
// author DIDs in the result set. On lookup failure it returns an error rather than
// emitting the posts: the endpoint fails closed so it can never surface content the
// viewer's block list would have hidden.
func (s *postService) applyViewerBlocks(ctx context.Context, viewerDID string, results []*PostResult) error {
	// Collect the unique author DIDs of the found posts.
	seen := make(map[string]struct{}, len(results))
	authorDIDs := make([]string, 0, len(results))
	for _, r := range results {
		if r.Post == nil || r.Post.Author == nil {
			continue
		}
		did := r.Post.Author.DID
		if did == "" {
			continue
		}
		if _, ok := seen[did]; ok {
			continue
		}
		seen[did] = struct{}{}
		authorDIDs = append(authorDIDs, did)
	}
	if len(authorDIDs) == 0 {
		return nil
	}

	blocked, err := s.blockChecker.AreBlocked(ctx, viewerDID, authorDIDs)
	if err != nil {
		return fmt.Errorf("failed to check viewer blocks: %w", err)
	}

	for i, r := range results {
		if r.Post == nil || r.Post.Author == nil {
			continue
		}
		if blocked[r.Post.Author.DID] {
			results[i] = blockedByAuthorResult(r.Post.URI, r.Post.Author.DID)
		}
	}
	return nil
}

// parsePostURIParts splits a post AT-URI into its authority and rkey, validating the
// scheme, structure, collection, and that both authority and rkey are present. The
// authority may be a DID or a handle; callers enforce the DID requirement (if any) via
// requireDIDAuthority. field names the request parameter for error attribution (e.g.
// "uri" or "uris"). This is the single source of truth for post-URI structure rules,
// shared by validatePostURI (get) and parsePostURI (delete). Pure (no I/O), so unit-testable.
func parsePostURIParts(uri, field string) (authority string, rkey string, err error) {
	if !strings.HasPrefix(uri, "at://") {
		return "", "", NewValidationError(field, "invalid AT-URI: must start with at://")
	}
	parts := strings.Split(strings.TrimPrefix(uri, "at://"), "/")
	if len(parts) != 3 {
		return "", "", NewValidationError(field, "invalid post URI format: expected at://authority/"+postCollection+"/rkey")
	}
	authority, collection, rkey := parts[0], parts[1], parts[2]
	if authority == "" {
		return "", "", NewValidationError(field, "invalid post URI: missing authority")
	}
	// EITHER post collection is a well-formed post URI. A post now lives in the
	// author's repo under social.coves.community.postv2 (§3.1), while every post
	// written before the flip is still at the deprecated community-repo NSID, and
	// a reader has to be able to name both — refusing postv2 here made the new
	// records unfetchable by the endpoint that hydrates every feed.
	//
	// What the authority MEANS differs between them — the community for the old
	// collection, the author for the new — so a caller that goes on to use it as
	// one or the other must narrow this itself. parsePostURI does, because it
	// writes to the repo the authority names.
	if collection != postCollection && collection != PostV2Collection {
		return "", "", NewValidationError(field, fmt.Sprintf("invalid collection in URI: expected %s or %s, got %s",
			postCollection, PostV2Collection, collection))
	}
	if rkey == "" {
		return "", "", NewValidationError(field, "invalid post URI: missing rkey")
	}
	return authority, rkey, nil
}

// CollectionOfPostURI returns the collection segment of an at:// record URI, or
// "" when the URI is not shaped like one.
//
// It is exported because the two post collections are now indexed into one
// table, so every layer that renders or narrows a post has to ask the same
// question of the same URI — the repository decides which record shape to
// build from it, and this path decides which writes it will accept. A second
// spelling of the split is a second place for the two to disagree.
func CollectionOfPostURI(uri string) string {
	parts := strings.Split(strings.TrimPrefix(uri, "at://"), "/")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// requireDIDAuthority enforces that a parsed post-URI authority is a DID (not a handle).
// Handles are mutable, so a handle-based URI would break after a community rename, or
// mis-resolve if the handle is later reassigned. field names the request parameter for errors.
func requireDIDAuthority(authority, field string) error {
	if !strings.HasPrefix(authority, "did:") {
		return NewValidationError(field, fmt.Sprintf("post URI authority must be a DID, got handle %q (resolve the community handle to its DID before calling)", authority))
	}
	if err := validateDIDFormat(authority); err != nil {
		return NewValidationError(field, fmt.Sprintf("invalid community DID in URI: %s", err.Error()))
	}
	return nil
}

// validatePostURI verifies that uri is a well-formed, canonical (DID-based) post AT-URI:
//
//	at://<community-did>/social.coves.community.post/<rkey>
//
// Used by GetPosts; callers must resolve handles to DIDs before calling.
func validatePostURI(uri string) error {
	authority, _, err := parsePostURIParts(uri, "uris")
	if err != nil {
		return err
	}
	return requireDIDAuthority(authority, "uris")
}

// validateGetAuthorPostsRequest validates the GetAuthorPosts request
func (s *postService) validateGetAuthorPostsRequest(req *GetAuthorPostsRequest) error {
	// Validate actor DID is set
	if req.ActorDID == "" {
		return NewValidationError("actor", "actor is required")
	}

	// Validate DID format - AT Protocol supports did:plc and did:web
	if err := validateDIDFormat(req.ActorDID); err != nil {
		return NewValidationError("actor", err.Error())
	}

	// Validate and set defaults for filter
	// Legacy snake_case values are normalized so pre-rename clients keep working
	req.Filter = strings.ReplaceAll(req.Filter, "_", "-")
	validFilters := map[string]bool{
		FilterPostsWithReplies: true,
		FilterPostsNoReplies:   true,
		FilterPostsWithMedia:   true,
	}
	if req.Filter == "" {
		req.Filter = FilterPostsWithReplies // Default
	}
	if !validFilters[req.Filter] {
		return NewValidationError("filter", "filter must be one of: posts-with-replies, posts-no-replies, posts-with-media")
	}

	// Validate and set defaults for limit
	if req.Limit <= 0 {
		req.Limit = 50 // Default
	}
	if req.Limit > 100 {
		req.Limit = 100 // Max
	}

	return nil
}

// validateDIDFormat validates that a string is a properly formatted DID
// Supports did:plc: (24 char base32 identifier) and did:web: (domain-based)
func validateDIDFormat(did string) error {
	const maxDIDLength = 2048

	if len(did) > maxDIDLength {
		return fmt.Errorf("DID exceeds maximum length")
	}

	switch {
	case strings.HasPrefix(did, "did:plc:"):
		// did:plc: format - identifier is 24 lowercase alphanumeric chars
		identifier := strings.TrimPrefix(did, "did:plc:")
		if len(identifier) == 0 {
			return fmt.Errorf("invalid did:plc format: missing identifier")
		}
		// Base32 uses lowercase a-z and 2-7
		for _, c := range identifier {
			if !((c >= 'a' && c <= 'z') || (c >= '2' && c <= '7')) {
				return fmt.Errorf("invalid did:plc format: identifier contains invalid characters")
			}
		}
		return nil

	case strings.HasPrefix(did, "did:web:"):
		// did:web: format - domain-based identifier
		domain := strings.TrimPrefix(did, "did:web:")
		if len(domain) == 0 {
			return fmt.Errorf("invalid did:web format: missing domain")
		}
		// Basic domain validation - must contain at least one dot or be localhost
		if !strings.Contains(domain, ".") && domain != "localhost" {
			return fmt.Errorf("invalid did:web format: invalid domain")
		}
		return nil

	default:
		return fmt.Errorf("unsupported DID method: must be did:plc or did:web")
	}
}

// communityCredentialFailure reports that the *community's* stored PDS
// credentials were rejected, deliberately severing the pds sentinel from the
// chain with %v rather than %w.
//
// Posts live in the community's repo, so deletes authenticate with the
// community's service token, not the caller's OAuth session. If that token were
// allowed to surface pds.ErrUnauthorized, the API boundary would read it as the
// caller's session being dead and answer 401 — telling a user with a perfectly
// healthy session to sign in again over a server-side credential problem they
// cannot fix, and hiding a real outage from 5xx alerting. Unclassified is the
// correct answer here: it becomes a logged 500.
func communityCredentialFailure(operation, communityDID string, err error) error {
	return fmt.Errorf("community PDS credentials rejected during %s for %s: %v",
		operation, communityDID, err)
}

// DeletePost deletes a post from the community's PDS repository
// SECURITY: Only the post author can delete their own posts
// Flow:
// 1. Validate session and URI format
// 2. Extract community DID and rkey from URI
// 3. Fetch community from AppView
// 4. Ensure fresh PDS credentials
// 5. Fetch post record from community's PDS to get author field
// 6. SECURITY: Verify author matches session.AccountDID
// 7. Delete record from community's PDS using community credentials
func (s *postService) DeletePost(ctx context.Context, session *oauth.ClientSessionData, req DeletePostRequest) error {
	// 1. Validate session
	if session == nil {
		return NewValidationError("session", "OAuth session required")
	}
	userDID := session.AccountDID.String()

	// 2. Validate URI format: at://community_did/social.coves.community.post/rkey
	if err := s.validateDeleteRequest(&req); err != nil {
		return err
	}

	// 3. Extract community DID and rkey from URI
	communityDID, rkey, err := s.parsePostURI(req.URI)
	if err != nil {
		return err
	}

	// 4. Fetch community from AppView
	community, err := s.communityService.GetByDID(ctx, communityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			return ErrCommunityNotFound
		}
		return fmt.Errorf("failed to fetch community: %w", err)
	}

	// 5. Ensure community has fresh PDS credentials
	community, err = s.communityService.EnsureFreshToken(ctx, community)
	if err != nil {
		return fmt.Errorf("failed to refresh community credentials: %w", err)
	}

	// 6. Create PDS client for community repository
	pdsClient, err := pds.NewFromAccessToken(community.PDSURL, community.DID, community.PDSAccessToken)
	if err != nil {
		return fmt.Errorf("failed to create PDS client: %w", err)
	}

	// 7. Fetch post record from PDS to verify author
	record, err := pdsClient.GetRecord(ctx, "social.coves.community.post", rkey)
	if err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			// Post already deleted or never existed - idempotent success
			log.Printf("[POST-DELETE] Post not found on PDS (already deleted?): %s", req.URI)
			return nil
		}
		if pds.IsAuthError(err) {
			return communityCredentialFailure("fetch post", community.DID, err)
		}
		return fmt.Errorf("failed to fetch post from PDS: %w", err)
	}

	// 8. SECURITY: Verify the requesting user is the post author
	// The author field in the record must match the authenticated user's DID
	postAuthor, ok := record.Value["author"].(string)
	if !ok || postAuthor == "" {
		return fmt.Errorf("post record missing author field: %s", req.URI)
	}

	if postAuthor != userDID {
		log.Printf("[SECURITY] Post delete authorization failed: user=%s, author=%s, uri=%s",
			userDID, postAuthor, req.URI)
		return ErrNotAuthorized
	}

	// 9. Delete record from community's PDS
	if err := pdsClient.DeleteRecord(ctx, "social.coves.community.post", rkey); err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			// Already deleted - idempotent success
			log.Printf("[POST-DELETE] Post already deleted from PDS: %s", req.URI)
			return nil
		}
		if pds.IsAuthError(err) {
			return communityCredentialFailure("delete post", community.DID, err)
		}
		return fmt.Errorf("failed to delete post from PDS: %w", err)
	}

	// 10. Log success (AppView will update via Jetstream consumer)
	log.Printf("[POST-DELETE] Successfully deleted post: uri=%s, author=%s, community=%s",
		req.URI, userDID, communityDID)

	return nil
}

// validateDeleteRequest validates the delete post request
func (s *postService) validateDeleteRequest(req *DeletePostRequest) error {
	if req.URI == "" {
		return NewValidationError("uri", "post URI is required")
	}

	// Basic URI format check
	if !strings.HasPrefix(req.URI, "at://") {
		return NewValidationError("uri", "invalid AT-URI format: must start with at://")
	}

	return nil
}

// parsePostURI extracts community DID and rkey from a post URI
// Format: at://community_did/social.coves.community.post/rkey
// Returns community DID, rkey, and error
func (s *postService) parsePostURI(uri string) (communityDID string, rkey string, err error) {
	// Structure + DID-authority validation is shared with the get path (single source of truth).
	communityDID, rkey, err = parsePostURIParts(uri, "uri")
	if err != nil {
		return "", "", err
	}

	// NARROWED to the community-repo collection, which the shared splitter
	// deliberately is not. This path treats the authority as the COMMUNITY and
	// goes on to open that repo and delete from it — so handed an author-repo
	// postv2 URI it would authenticate as the AUTHOR's DID and try to delete a
	// record there. The write path moves to postv2 in task 6; until it does,
	// refusing is the only correct answer.
	if collection := CollectionOfPostURI(uri); collection != postCollection {
		return "", "", NewValidationError("uri", fmt.Sprintf(
			"deleting a post is only supported for %s URIs, got %s", postCollection, collection))
	}
	if err := requireDIDAuthority(communityDID, "uri"); err != nil {
		return "", "", err
	}

	// Defense-in-depth: verify rkey extraction is consistent with the utils helper.
	if extractedRkey := utils.ExtractRKeyFromURI(uri); extractedRkey != rkey {
		return "", "", NewValidationError("uri", "URI parsing inconsistency")
	}

	return communityDID, rkey, nil
}
