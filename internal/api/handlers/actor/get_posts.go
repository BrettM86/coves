package actor

import (
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"strconv"

	"Coves/internal/api/handlers/common"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/moderation"
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
	authority      moderation.Authority
	instanceDID    string
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

// WithViewerModeration makes the handler report, in viewer.moderation, the
// moderation actions an instance admin viewer may take on each post.
func (h *GetPostsHandler) WithViewerModeration(authority moderation.Authority, instanceDID string) *GetPostsHandler {
	if authority == nil || instanceDID == "" {
		panic("actor.GetPostsHandler.WithViewerModeration: authority and instanceDID are required")
	}
	h.authority = authority
	h.instanceDID = instanceDID
	return h
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
		// More than one bridged user matches a name@host actor: 409 AmbiguousActor
		if errors.Is(err, users.ErrAmbiguousUser) {
			handleServiceError(w, err)
			return
		}

		// Check if it's an actor not found error (from handle resolution)
		var actorNotFound *actorNotFoundError
		if errors.As(err, &actorNotFound) {
			writeError(w, http.StatusNotFound, "ActorNotFound", "Actor not found")
			return
		}

		// An infrastructure failure during resolution (database down, DNS
		// failure) is a 500; its cause stays in the log, out of the response.
		var resolutionFailed *resolutionFailedError
		if errors.As(err, &resolutionFailed) {
			slog.Error("actor resolution infrastructure failure", "error", err)
			writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to resolve actor identity")
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

	// Populate viewer vote state and moderation actions if authenticated
	common.PopulateViewerVoteState(r.Context(), r, h.voteService, response.Feed)
	common.PopulateViewerModeration(r, h.authority, h.instanceDID, response.Feed)

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
	actorDID, err := resolveActorDID(r.Context(), h.userService, actor)
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
