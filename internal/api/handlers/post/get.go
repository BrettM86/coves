package post

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"Coves/internal/api/handlers/common"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"
)

// maxURILength caps each URI to prevent abuse via oversized query params.
// The batch size limit is shared with the service layer via posts.MaxGetPostsURIs.
const maxURILength = 512

// GetHandler handles batch post retrieval by AT-URI.
// Implements social.coves.community.post.get (feed hydration + permalink/cold-load).
type GetHandler struct {
	service        posts.Service
	voteService    votes.Service
	blueskyService blueskypost.Service
}

// NewGetHandler creates a new get handler.
// voteService is used to populate the viewer's vote state (may be nil).
// blueskyService is used to resolve embedded Bluesky posts (may be nil).
func NewGetHandler(service posts.Service, voteService votes.Service, blueskyService blueskypost.Service) *GetHandler {
	return &GetHandler{
		service:        service,
		voteService:    voteService,
		blueskyService: blueskyService,
	}
}

// HandleGet handles GET /xrpc/social.coves.community.post.get?uris=at://...&uris=at://...
// Batch-fetches post views by AT-URI for feed hydration and permalink rendering.
// Posts are returned in the same order as the input URIs; missing or deleted posts
// are returned as notFoundPost markers ({uri, notFound: true}).
func (h *GetHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse and validate the repeated `uris` query parameter
	uris := r.URL.Query()["uris"]
	if len(uris) == 0 {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "uris parameter is required")
		return
	}
	if len(uris) > posts.MaxGetPostsURIs {
		writeError(w, http.StatusBadRequest, "InvalidRequest", fmt.Sprintf("too many URIs (max %d)", posts.MaxGetPostsURIs))
		return
	}
	for _, uri := range uris {
		if len(uri) > maxURILength {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "URI exceeds maximum length")
			return
		}
	}

	// Optional viewer DID (set by OptionalAuth) for viewer-specific state
	viewerDID := middleware.GetUserDID(r)

	results, err := h.service.GetPosts(r.Context(), posts.GetPostsRequest{
		URIs:      uris,
		ViewerDID: viewerDID,
	})
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Enrich found posts with the authenticated viewer's vote state (no-op if unauthenticated)
	common.PopulateViewerVoteState(r.Context(), r, h.voteService, results)

	// Transform blob refs to URLs and resolve embedded Bluesky posts for found posts
	for _, res := range results {
		if res.Post != nil {
			posts.TransformBlobRefsToURLs(res.Post)
			posts.TransformPostEmbeds(r.Context(), res.Post, h.blueskyService)
		}
	}

	// Build the union output array in request order. Every slot must be exactly one
	// union member (postView, blockedPost, or notFoundPost); a result with no member
	// set is an internal bug and must not be emitted as a null array entry, which would
	// violate the lexicon's union.
	out := make([]interface{}, len(results))
	for i, res := range results {
		member, ok := res.Member()
		if !ok {
			log.Printf("ERROR: getPosts result %d has no union member set", i)
			writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to assemble response")
			return
		}
		out[i] = member
	}

	// Pre-encode to a buffer so an encoding failure still yields a proper error response
	responseBytes, err := json.Marshal(map[string]interface{}{"posts": out})
	if err != nil {
		log.Printf("ERROR: Failed to encode getPosts response: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to encode response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(responseBytes); err != nil {
		log.Printf("ERROR: Failed to write getPosts response: %v", err)
	}
}
