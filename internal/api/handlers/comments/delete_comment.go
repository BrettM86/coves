package comments

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"encoding/json"
	"log"
	"net/http"
)

// DeleteCommentHandler handles comment deletion requests
type DeleteCommentHandler struct {
	service comments.Service
}

// NewDeleteCommentHandler creates a new handler for deleting comments
func NewDeleteCommentHandler(service comments.Service) *DeleteCommentHandler {
	return &DeleteCommentHandler{
		service: service,
	}
}

// DeleteCommentInput matches the lexicon input schema for social.coves.community.comment.delete
type DeleteCommentInput struct {
	URI string `json:"uri"`
}

// DeleteCommentOutput is empty per lexicon specification
type DeleteCommentOutput struct{}

// HandleDelete handles comment deletion requests
// POST /xrpc/social.coves.community.comment.delete
//
// Request body: { "uri": "at://..." }
// Response: {}
func (h *DeleteCommentHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	// 1. Check method is POST
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Limit request body size to prevent DoS attacks (100KB should be plenty for comments)
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024)

	// 3. Parse JSON body into DeleteCommentInput
	var input DeleteCommentInput
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

	// 5. Convert input to DeleteCommentRequest
	req := comments.DeleteCommentRequest{
		URI: input.URI,
	}

	// 6. Call service to delete comment
	err := h.service.DeleteComment(r.Context(), session, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// 7. Return empty JSON object per lexicon specification
	output := DeleteCommentOutput{}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(output); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
