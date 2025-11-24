package post

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"
)

// CreateHandler handles post creation requests
type CreateHandler struct {
	service posts.Service
}

// NewCreateHandler creates a new create handler
func NewCreateHandler(service posts.Service) *CreateHandler {
	return &CreateHandler{
		service: service,
	}
}

// HandleCreate handles POST /xrpc/social.coves.community.post.create
// Creates a new post in a community's repository
func (h *CreateHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	// 1. Check HTTP method
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Limit request body size to prevent DoS attacks
	// 1MB allows for large content + embeds while preventing abuse
	r.Body = http.MaxBytesReader(w, r.Body, 1*1024*1024)

	// 3. Parse request body
	var req posts.CreatePostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Check if error is due to body size limit
		if err.Error() == "http: request body too large" {
			writeError(w, http.StatusRequestEntityTooLarge, "RequestTooLarge",
				"Request body too large (max 1MB)")
			return
		}
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// 4. Extract authenticated user DID from request context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// 5. Validate required fields
	if req.Community == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community is required")
		return
	}

	// 5b. Basic format validation for better UX (fail fast on obviously invalid input)
	// Valid formats accepted by resolver:
	//   - DID: did:plc:xyz, did:web:example.com
	//   - Scoped handle: !name@instance
	//   - Canonical handle: name.community.instance
	//   - @-prefixed handle: @name.community.instance
	//
	// We only reject obviously invalid formats here (no prefix, no dots, no @ for !)
	// The service layer (ResolveCommunityIdentifier) does comprehensive validation

	// Scoped handles must include @ symbol
	if strings.HasPrefix(req.Community, "!") && !strings.Contains(req.Community, "@") {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"scoped handle must include @ symbol (!name@instance)")
		return
	}

	// 6. SECURITY: Reject client-provided authorDid
	// This prevents users from impersonating other users
	if req.AuthorDID != "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"authorDid must not be provided - derived from authenticated user")
		return
	}

	// 7. Set author from authenticated user context
	req.AuthorDID = userDID

	// 8. Call service to create post (write-forward to PDS)
	// Note: Service layer will resolve community at-identifier (handle or DID) to DID
	response, err := h.service.CreatePost(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// 9. Return success response matching lexicon output
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		log.Printf("Failed to encode post creation response: %v", err)
	}
}
