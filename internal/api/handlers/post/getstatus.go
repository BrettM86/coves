package post

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// The identifier bounds this endpoint accepts, DERIVED from the specs rather
// than picked, because both directions of getting it wrong are silent.
//
// A postv2 URI is AUTHORITY-SCOPED — the author's DID is inside it — and a DID
// may legally run to 2048 bytes, the same fact that killed the readable rkey
// transform in PRD rev 2.2. A cap sized for the old community-repo URIs would
// refuse exactly the authors with long DIDs, on the one endpoint that can tell
// them why their post is not visible, and nothing else in the system would show
// a symptom: their posts index fine and every other endpoint serves them.
//
// So the URI bound is the sum of its legal parts rather than a round number:
// "at://" + a 2048-byte authority + "/" + a 317-byte NSID + "/" + a 512-byte
// record key. It is a pre-parse DoS bound only — ParseATURI below is what
// decides whether the thing is actually a URI.
const (
	maxAuthorityLength   = 2048 // DID Core's ceiling
	maxNSIDLength        = 317  // atProto NSID grammar
	maxRecordKeyLength   = 512  // atProto record-key grammar
	maxPostURILength     = len("at://") + maxAuthorityLength + 1 + maxNSIDLength + 1 + maxRecordKeyLength
	maxCommunityIDLength = maxAuthorityLength
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
	// THE CAP IS SIZED TO THE SPEC, NOT TO THE OLD URIs. A DID may legally run
	// to 2048 bytes — the same fact that killed the readable rkey transform in
	// PRD rev 2.2 — and an author-owned post URI is authority-scoped, so the
	// author's DID is INSIDE the URI this endpoint takes. A cap sized for the
	// old community-repo URIs would silently make long-DID authors unqueryable
	// and nothing else would notice: their posts index fine and every other
	// endpoint serves them.
	if len(postURI) > maxPostURILength {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "post URI exceeds maximum length")
		return
	}
	if len(communityDID) > maxCommunityIDLength {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "community DID exceeds maximum length")
		return
	}

	// Raising the cap must not become "accept anything long". The URI is PARSED,
	// so a non-at:// string comes back as the client bug it is rather than as a
	// silent not-found — which would tell a client with a typo that its post
	// does not exist.
	if _, err := syntax.ParseATURI(postURI); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"post must be a valid AT-URI")
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
	// NO-STORE, and not as a nicety. This endpoint exists to be POLLED for a
	// transition (§7), so a cached answer is the endpoint failing at its only
	// job: the client keeps being handed `pending` after the post was accepted
	// and stops asking. It is also unauthenticated and reports a moderation
	// decision, so an intermediary holding a copy is a disclosure surface that
	// outlives the request that created it.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(responseBytes); err != nil {
		log.Printf("ERROR: Failed to write getStatus response: %v", err)
	}
}
