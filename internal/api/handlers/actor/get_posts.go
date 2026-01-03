package actor

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"Coves/internal/api/handlers/common"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
)

// GetPostsHandler handles actor post retrieval
type GetPostsHandler struct {
	postService    posts.Service
	userService    users.UserService
	voteService    votes.Service
	blueskyService blueskypost.Service
}

// NewGetPostsHandler creates a new actor posts handler
func NewGetPostsHandler(
	postService posts.Service,
	userService users.UserService,
	voteService votes.Service,
	blueskyService blueskypost.Service,
) *GetPostsHandler {
	if blueskyService == nil {
		log.Printf("[ACTOR-HANDLER] WARNING: blueskyService is nil - Bluesky post embeds will not be resolved")
	}
	return &GetPostsHandler{
		postService:    postService,
		userService:    userService,
		voteService:    voteService,
		blueskyService: blueskyService,
	}
}

// HandleGetPosts retrieves posts by an actor (user)
// GET /xrpc/social.coves.actor.getPosts?actor={did_or_handle}&filter=posts_with_replies&community=...&limit=50&cursor=...
func (h *GetPostsHandler) HandleGetPosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	req, err := h.parseRequest(r)
	if err != nil {
		// Check if it's an actor not found error (from handle resolution)
		var actorNotFound *actorNotFoundError
		if errors.As(err, &actorNotFound) {
			writeError(w, http.StatusNotFound, "ActorNotFound", "Actor not found")
			return
		}
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	// Get viewer DID for populating viewer state (optional)
	viewerDID := middleware.GetUserDID(r)
	req.ViewerDID = viewerDID

	// Get actor posts from service
	response, err := h.postService.GetAuthorPosts(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Populate viewer vote state if authenticated
	common.PopulateViewerVoteState(r.Context(), r, h.voteService, response.Feed)

	// Transform blob refs to URLs and resolve post embeds for all posts
	for _, feedPost := range response.Feed {
		if feedPost.Post != nil {
			posts.TransformBlobRefsToURLs(feedPost.Post)
			posts.TransformPostEmbeds(r.Context(), feedPost.Post, h.blueskyService)
		}
	}

	// Pre-encode response to buffer before writing headers
	// This ensures we can return a proper error if encoding fails
	responseBytes, err := json.Marshal(response)
	if err != nil {
		log.Printf("ERROR: Failed to encode actor posts response: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to encode response")
		return
	}

	// Return feed
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(responseBytes); err != nil {
		log.Printf("ERROR: Failed to write actor posts response: %v", err)
	}
}

// parseRequest parses query parameters into GetAuthorPostsRequest
func (h *GetPostsHandler) parseRequest(r *http.Request) (posts.GetAuthorPostsRequest, error) {
	req := posts.GetAuthorPostsRequest{}

	// Required: actor (handle or DID)
	actor := r.URL.Query().Get("actor")
	if actor == "" {
		return req, posts.NewValidationError("actor", "actor parameter is required")
	}
	// Validate actor length to prevent DoS via massive strings
	// Max DID length is ~2048 chars (did:plc: is 8 + 24 base32 = 32, but did:web: can be longer)
	// Max handle length is 253 chars (DNS limit)
	const maxActorLength = 2048
	if len(actor) > maxActorLength {
		return req, posts.NewValidationError("actor", "actor parameter exceeds maximum length")
	}

	// Resolve actor to DID if it's a handle
	actorDID, err := h.resolveActor(r, actor)
	if err != nil {
		return req, err
	}
	req.ActorDID = actorDID

	// Optional: filter (default: posts_with_replies)
	req.Filter = r.URL.Query().Get("filter")

	// Optional: community (handle or DID)
	req.Community = r.URL.Query().Get("community")

	// Optional: limit (default: 50, max: 100)
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil {
			return req, posts.NewValidationError("limit", "limit must be a valid integer")
		}
		req.Limit = limit
	}

	// Optional: cursor
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		req.Cursor = &cursor
	}

	return req, nil
}

// resolveActor converts an actor identifier (handle or DID) to a DID
func (h *GetPostsHandler) resolveActor(r *http.Request, actor string) (string, error) {
	// If it's already a DID, return it
	if strings.HasPrefix(actor, "did:") {
		return actor, nil
	}

	// It's a handle - resolve to DID using user service
	did, err := h.userService.ResolveHandleToDID(r.Context(), actor)
	if err != nil {
		// Check for context errors (timeouts, cancellation) - these are infrastructure errors
		if r.Context().Err() != nil {
			log.Printf("WARN: Handle resolution failed due to context error for %s: %v", actor, err)
			return "", &resolutionFailedError{actor: actor, cause: r.Context().Err()}
		}

		// Check for common "not found" patterns in error message
		errStr := err.Error()
		isNotFound := strings.Contains(errStr, "not found") ||
			strings.Contains(errStr, "no rows") ||
			strings.Contains(errStr, "unable to resolve")

		if isNotFound {
			return "", &actorNotFoundError{actor: actor}
		}

		// For other errors (network, database, DNS failures), return infrastructure error
		// This ensures users see "internal error" not "actor not found" for real problems
		log.Printf("WARN: Handle resolution infrastructure failure for %s: %v", actor, err)
		return "", &resolutionFailedError{actor: actor, cause: err}
	}

	return did, nil
}
