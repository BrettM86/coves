package post

import (
	"encoding/json"
	"log"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/posts"
)

// UpdateHandler serves social.coves.community.post.update.
type UpdateHandler struct {
	service posts.Service
}

// NewUpdateHandler creates a new handler for editing posts.
func NewUpdateHandler(service posts.Service) *UpdateHandler {
	return &UpdateHandler{service: service}
}

// HandleUpdate handles POST /xrpc/social.coves.community.post.update
//
// Request body: the lexicon's input schema (uri plus the mutable fields).
// Response: { "uri": "at://...", "cid": "..." }
func (h *UpdateHandler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// The same 1MB ceiling the create path uses: an edit carries the same
	// content and embeds a create does, so a tighter bound here would refuse
	// edits to posts this service accepted.
	r.Body = http.MaxBytesReader(w, r.Body, 1*1024*1024)

	var req posts.UpdatePostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if err.Error() == "http: request body too large" {
			writeError(w, http.StatusRequestEntityTooLarge, "RequestTooLarge",
				"Request body too large (max 1MB)")
			return
		}
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// The session is both the authorization and the credential: the record is
	// signed by its author, and the author is who the session says it is. There
	// is no aggregator path here — an aggregator syndicates, it does not edit.
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	response, err := h.service.UpdatePost(r.Context(), session, req)
	if err != nil {
		handleUpdateError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode post update response: %v", err)
	}
}

// updateErrorMapper narrows the package mapper for the edit path, naming the
// missing post and the refused action the way the update lexicon's errors do;
// everything else it inherits.
var updateErrorMapper = errorMapper.With(
	xrpc.Sentinel(posts.ErrNotFound, http.StatusNotFound,
		"PostNotFound", "Post not found"),
	xrpc.Sentinel(posts.ErrNotAuthorized, http.StatusForbidden,
		"NotAuthorized", "You are not authorized to edit this post"),
)

// handleUpdateError maps edit-specific service errors to HTTP responses.
func handleUpdateError(w http.ResponseWriter, err error) {
	updateErrorMapper.Write(w, err)
}
