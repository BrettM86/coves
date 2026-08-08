package post

import (
	"net/http"

	"Coves/internal/core/posts"
)

// RED STUB (task 5, cycle 1). Signature only — HandleGetStatus writes nothing,
// so every assertion in getstatus_integration_test.go fails on the response
// rather than on a missing symbol. The implementation is GREEN's.

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
}
