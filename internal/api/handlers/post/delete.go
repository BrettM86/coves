package post

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// DeleteHandler handles post deletion requests
type DeleteHandler struct {
	service posts.Service
}

// NewDeleteHandler creates a new handler for deleting posts
func NewDeleteHandler(service posts.Service) *DeleteHandler {
	return &DeleteHandler{
		service: service,
	}
}

// DeletePostInput matches the lexicon input schema for social.coves.community.post.delete
type DeletePostInput struct {
	URI string `json:"uri"`
}

// DeletePostOutput is empty per lexicon specification
type DeletePostOutput struct{}

// HandleDelete handles post deletion requests
// POST /xrpc/social.coves.community.post.delete
//
// Request body: { "uri": "at://..." }
// Response: {}
func (h *DeleteHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	// 1. Check method is POST
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Limit request body size to prevent DoS attacks (100KB should be plenty for delete requests)
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024)

	// 3. Parse JSON body into DeletePostInput
	var input DeletePostInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// 4. Get OAuth session from context (injected by auth middleware)
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// 5. Convert input to DeletePostRequest
	req := posts.DeletePostRequest{
		URI: input.URI,
	}

	// 6. Call service to delete post
	err := h.service.DeletePost(r.Context(), session, req)
	if err != nil {
		handleDeleteError(w, err)
		return
	}

	// 7. Return empty JSON object per lexicon specification
	output := DeletePostOutput{}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(output); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

// handleDeleteError maps delete-specific service errors to HTTP responses
func handleDeleteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, posts.ErrNotFound):
		writeError(w, http.StatusNotFound, "PostNotFound", "Post not found")

	case errors.Is(err, posts.ErrNotAuthorized):
		writeError(w, http.StatusForbidden, "NotAuthorized", "You are not authorized to delete this post")

	case errors.Is(err, posts.ErrCommunityNotFound):
		writeError(w, http.StatusNotFound, "CommunityNotFound", "Community not found")

	case posts.IsValidationError(err):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())

	default:
		// Don't leak internal error details to clients
		log.Printf("Unexpected error in post delete handler: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError",
			"An internal error occurred")
	}
}
