package comments

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"encoding/json"
	"log"
	"net/http"
)

// CreateCommentHandler handles comment creation requests
type CreateCommentHandler struct {
	service comments.Service
}

// NewCreateCommentHandler creates a new handler for creating comments
func NewCreateCommentHandler(service comments.Service) *CreateCommentHandler {
	return &CreateCommentHandler{
		service: service,
	}
}

// CreateCommentInput matches the lexicon input schema for social.coves.community.comment.create
type CreateCommentInput struct {
	Reply struct {
		Root struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		} `json:"root"`
		Parent struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		} `json:"parent"`
	} `json:"reply"`
	Content string        `json:"content"`
	Facets  []interface{} `json:"facets,omitempty"`
	Embed   interface{}   `json:"embed,omitempty"`
	Langs   []string      `json:"langs,omitempty"`
	Labels  interface{}   `json:"labels,omitempty"`
}

// CreateCommentOutput matches the lexicon output schema
type CreateCommentOutput struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// HandleCreate handles comment creation requests
// POST /xrpc/social.coves.community.comment.create
//
// Request body: { "reply": { "root": {...}, "parent": {...} }, "content": "..." }
// Response: { "uri": "at://...", "cid": "..." }
func (h *CreateCommentHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	// 1. Check method is POST
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Limit request body size to prevent DoS attacks (100KB should be plenty for comments)
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024)

	// 3. Parse JSON body into CreateCommentInput
	var input CreateCommentInput
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

	// 6. Convert input to CreateCommentRequest
	req := comments.CreateCommentRequest{
		Reply: comments.ReplyRef{
			Root: comments.StrongRef{
				URI: input.Reply.Root.URI,
				CID: input.Reply.Root.CID,
			},
			Parent: comments.StrongRef{
				URI: input.Reply.Parent.URI,
				CID: input.Reply.Parent.CID,
			},
		},
		Content: input.Content,
		Facets:  input.Facets,
		Embed:   input.Embed,
		Langs:   input.Langs,
		Labels:  labels,
	}

	// 7. Call service to create comment
	response, err := h.service.CreateComment(r.Context(), session, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// 8. Return JSON response with URI and CID
	output := CreateCommentOutput{
		URI: response.URI,
		CID: response.CID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(output); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
