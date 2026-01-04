package actor

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
)

// GetCommentsHandler handles actor comment retrieval
type GetCommentsHandler struct {
	commentService comments.Service
	userService    users.UserService
	voteService    votes.Service
}

// NewGetCommentsHandler creates a new actor comments handler
func NewGetCommentsHandler(
	commentService comments.Service,
	userService users.UserService,
	voteService votes.Service,
) *GetCommentsHandler {
	return &GetCommentsHandler{
		commentService: commentService,
		userService:    userService,
		voteService:    voteService,
	}
}

// HandleGetComments retrieves comments by an actor (user)
// GET /xrpc/social.coves.actor.getComments?actor={did_or_handle}&community=...&limit=50&cursor=...
func (h *GetCommentsHandler) HandleGetComments(w http.ResponseWriter, r *http.Request) {
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

		// Check if it's an infrastructure failure during resolution
		// (database down, DNS failures, network errors, etc.)
		var resolutionFailed *resolutionFailedError
		if errors.As(err, &resolutionFailed) {
			log.Printf("ERROR: Actor resolution infrastructure failure: %v", err)
			writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to resolve actor identity")
			return
		}

		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	// Get viewer DID for populating viewer state (optional)
	viewerDID := middleware.GetUserDID(r)
	if viewerDID != "" {
		req.ViewerDID = &viewerDID
	}

	// Get actor comments from service
	response, err := h.commentService.GetActorComments(r.Context(), req)
	if err != nil {
		handleCommentServiceError(w, err)
		return
	}

	// Populate viewer vote state if authenticated
	h.populateViewerVoteState(r, response)

	// Pre-encode response to buffer before writing headers
	// This ensures we can return a proper error if encoding fails
	responseBytes, err := json.Marshal(response)
	if err != nil {
		log.Printf("ERROR: Failed to encode actor comments response: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to encode response")
		return
	}

	// Return comments
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(responseBytes); err != nil {
		log.Printf("ERROR: Failed to write actor comments response: %v", err)
	}
}

// parseRequest parses query parameters into GetActorCommentsRequest
func (h *GetCommentsHandler) parseRequest(r *http.Request) (*comments.GetActorCommentsRequest, error) {
	req := &comments.GetActorCommentsRequest{}

	// Required: actor (handle or DID)
	actor := r.URL.Query().Get("actor")
	if actor == "" {
		return nil, &validationError{field: "actor", message: "actor parameter is required"}
	}
	// Validate actor length to prevent DoS via massive strings
	// Max DID length is ~2048 chars (did:plc: is 8 + 24 base32 = 32, but did:web: can be longer)
	// Max handle length is 253 chars (DNS limit)
	const maxActorLength = 2048
	if len(actor) > maxActorLength {
		return nil, &validationError{field: "actor", message: "actor parameter exceeds maximum length"}
	}

	// Resolve actor to DID if it's a handle
	actorDID, err := h.resolveActor(r, actor)
	if err != nil {
		return nil, err
	}
	req.ActorDID = actorDID

	// Optional: community (handle or DID)
	req.Community = r.URL.Query().Get("community")

	// Optional: limit (default: 50, max: 100)
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil {
			return nil, &validationError{field: "limit", message: "limit must be a valid integer"}
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
func (h *GetCommentsHandler) resolveActor(r *http.Request, actor string) (string, error) {
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

// populateViewerVoteState enriches comment views with the authenticated user's vote state
func (h *GetCommentsHandler) populateViewerVoteState(r *http.Request, response *comments.GetActorCommentsResponse) {
	if h.voteService == nil || response == nil || len(response.Comments) == 0 {
		return
	}

	session := middleware.GetOAuthSession(r)
	if session == nil {
		return
	}

	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		return
	}

	// Ensure vote cache is populated from PDS
	if err := h.voteService.EnsureCachePopulated(r.Context(), session); err != nil {
		log.Printf("Warning: failed to populate vote cache for actor comments: %v", err)
		return
	}

	// Collect comment URIs to batch lookup
	commentURIs := make([]string, 0, len(response.Comments))
	for _, comment := range response.Comments {
		if comment != nil {
			commentURIs = append(commentURIs, comment.URI)
		}
	}

	// Get viewer votes for all comments
	viewerVotes := h.voteService.GetViewerVotesForSubjects(userDID, commentURIs)

	// Populate viewer state on each comment
	for _, comment := range response.Comments {
		if comment != nil {
			if vote, exists := viewerVotes[comment.URI]; exists {
				comment.Viewer = &comments.CommentViewerState{
					Vote:    &vote.Direction,
					VoteURI: &vote.URI,
				}
			}
		}
	}
}

// handleCommentServiceError maps service errors to HTTP responses
func handleCommentServiceError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}

	errStr := err.Error()

	// Check for validation errors
	if strings.Contains(errStr, "invalid request") {
		writeError(w, http.StatusBadRequest, "InvalidRequest", errStr)
		return
	}

	// Check for not found errors
	if comments.IsNotFound(err) || strings.Contains(errStr, "not found") {
		writeError(w, http.StatusNotFound, "NotFound", "Resource not found")
		return
	}

	// Check for authorization errors
	if errors.Is(err, comments.ErrNotAuthorized) {
		writeError(w, http.StatusForbidden, "NotAuthorized", "Not authorized")
		return
	}

	// Default to internal server error
	log.Printf("ERROR: Comment service error: %v", err)
	writeError(w, http.StatusInternalServerError, "InternalServerError", "An unexpected error occurred")
}

// validationError represents a validation error for a specific field
type validationError struct {
	field   string
	message string
}

func (e *validationError) Error() string {
	return e.message
}
