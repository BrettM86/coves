package comments

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"encoding/json"
	"log"
	"net/http"
)

// UpdateCommentHandler handles comment update requests
type UpdateCommentHandler struct {
	service comments.Service
}

// NewUpdateCommentHandler creates a new handler for updating comments
func NewUpdateCommentHandler(service comments.Service) *UpdateCommentHandler {
	return &UpdateCommentHandler{
		service: service,
	}
}

// UpdateCommentInput matches the lexicon input schema for social.coves.community.comment.update
type UpdateCommentInput struct {
	URI     string        `json:"uri"`
	Content string        `json:"content"`
	Facets  []interface{} `json:"facets,omitempty"`
	Embed   interface{}   `json:"embed,omitempty"`
	Langs   []string      `json:"langs,omitempty"`
	Labels  interface{}   `json:"labels,omitempty"`
}

// UpdateCommentOutput matches the lexicon output schema
type UpdateCommentOutput struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// HandleUpdate handles comment update requests
// POST /xrpc/social.coves.community.comment.update
//
// Request body: { "uri": "at://...", "content": "..." }
// Response: { "uri": "at://...", "cid": "..." }
func (h *UpdateCommentHandler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	// 1. Check method is POST
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Limit request body size to prevent DoS attacks (100KB should be plenty for comments)
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024)

	// 3. Parse JSON body into UpdateCommentInput
	var input UpdateCommentInput
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

	// 5. Convert labels interface{} to *comments.SelfLabels if provided
	var labels *comments.SelfLabels
	if input.Labels != nil {
		labelsJSON, err := json.Marshal(input.Labels)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidLabels", "Invalid labels format")
			return
		}
		var selfLabels comments.SelfLabels
		if err := json.Unmarshal(labelsJSON, &selfLabels); err != nil {
			writeError(w, http.StatusBadRequest, "InvalidLabels", "Invalid labels structure")
			return
		}
		labels = &selfLabels
	}

	// 6. Convert input to UpdateCommentRequest
	req := comments.UpdateCommentRequest{
		URI:     input.URI,
		Content: input.Content,
		Facets:  input.Facets,
		Embed:   input.Embed,
		Langs:   input.Langs,
		Labels:  labels,
	}

	// 7. Call service to update comment
	response, err := h.service.UpdateComment(r.Context(), session, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// 8. Return JSON response with URI and CID
	output := UpdateCommentOutput{
		URI: response.URI,
		CID: response.CID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(output); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
