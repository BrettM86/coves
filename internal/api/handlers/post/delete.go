package post

import (
	"encoding/json"
	"log"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/posts"
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

// deleteErrorMapper narrows the package mapper for the delete path, which names
// the missing post and the refused action more precisely than the generic rules
// do; everything else it inherits.
//
// Note that deleting does NOT write to the caller's repo — posts live in the
// community's, and the delete authenticates with the community's service token.
// The service therefore strips the pds sentinels off a rejected community
// credential before it reaches here (see posts.communityCredentialFailure), so
// the inherited re-auth rule only ever fires on the caller's own session.
var deleteErrorMapper = errorMapper.With(
	xrpc.Sentinel(posts.ErrNotFound, http.StatusNotFound,
		"PostNotFound", "Post not found"),
	xrpc.Sentinel(posts.ErrNotAuthorized, http.StatusForbidden,
		"NotAuthorized", "You are not authorized to delete this post"),
)

// handleDeleteError maps delete-specific service errors to HTTP responses
func handleDeleteError(w http.ResponseWriter, err error) {
	deleteErrorMapper.Write(w, err)
}
