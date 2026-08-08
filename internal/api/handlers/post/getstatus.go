package post

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"Coves/internal/core/posts"
)

// GetStatusHandler serves social.coves.community.post.getStatus: one
// community's decision about one post (docs/PRD_AUTHOR_OWNED_POSTS.md §3.4).
//
// UNAUTHENTICATED, deliberately, and the trade is recorded rather than hidden.
// The caller with the strongest need is an author on a DIFFERENT server whose
// post is pending on this one (§7): they have no account here, so there is no
// session to require, and service-auth is Beta scope. The cost is that a
// rejected post's status is mildly disclosed to anyone who can name its URI —
// accepted by the owner in PRD rev 2.7. That the route carries no auth
// middleware is declared in internal/api/routes/registration_test.go, which is
// the only place the whole HTTP surface is enumerated.
type GetStatusHandler struct {
	service posts.StatusService
}

// NewGetStatusHandler creates a new getStatus handler.
func NewGetStatusHandler(service posts.StatusService) *GetStatusHandler {
	return &GetStatusHandler{service: service}
}

// HandleGetStatus handles
// GET /xrpc/social.coves.community.post.getStatus?post=at://...&community=did:...
func (h *GetStatusHandler) HandleGetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	postURI := r.URL.Query().Get("post")
	communityDID := r.URL.Query().Get("community")

	// Both halves are refused rather than defaulted. A post carries independent
	// decisions from several communities (§2), so an incomplete subject is not
	// an under-specified question with an obvious answer — it is a different
	// question, and answering about whichever row turned up first would report
	// one community's verdict as another's.
	if postURI == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "post parameter is required")
		return
	}
	if communityDID == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community parameter is required")
		return
	}
	if len(postURI) > maxURILength {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "post URI exceeds maximum length")
		return
	}
	if len(communityDID) > maxURILength {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community DID exceeds maximum length")
		return
	}

	status, err := h.service.GetStatus(r.Context(), posts.GetStatusRequest{
		PostURI:      postURI,
		CommunityDID: communityDID,
	})
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Built field by field so an absent optional field is ABSENT from the JSON
	// rather than present and null. The distinction is the client's: a caller
	// polling for the accepted transition reads `"decisionCode": null` as a
	// decision that was made, when in fact none exists.
	body := map[string]interface{}{"status": string(status.Status)}
	if status.DecisionCode != nil {
		body["decisionCode"] = *status.DecisionCode
	}
	if status.DecisionAt != nil {
		body["decisionAt"] = status.DecisionAt.UTC().Format(time.RFC3339)
	}
	if status.AcceptanceURI != nil {
		body["acceptanceUri"] = *status.AcceptanceURI
	}

	// Pre-encoded so an encoding failure still yields a proper error response
	// rather than a 200 with a truncated body (mirrors post.get).
	responseBytes, err := json.Marshal(body)
	if err != nil {
		log.Printf("ERROR: Failed to encode getStatus response: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "Failed to encode response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(responseBytes); err != nil {
		log.Printf("ERROR: Failed to write getStatus response: %v", err)
	}
}
