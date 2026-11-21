package moderation

import (
	"context"
	"net/http"

	"Coves/internal/core/moderation"
)

// CommunityCommentActions runs community comment removals and restores.
type CommunityCommentActions interface {
	RemoveComment(ctx context.Context, req moderation.RemoveCommentRequest) (*moderation.CommunityActionResult, error)
	RestoreComment(ctx context.Context, req moderation.RestoreCommentRequest) (*moderation.CommunityActionResult, error)
}

// CommunityCommentHandler serves social.coves.community.removeComment and
// social.coves.community.restoreComment. Both answer with the post
// procedures' output: the removal record's URI, the comment's community
// version after the action, the action written and whether the decision was
// already in place.
type CommunityCommentHandler struct {
	service CommunityCommentActions
}

// NewCommunityCommentHandler builds the handler over the service.
func NewCommunityCommentHandler(service CommunityCommentActions) *CommunityCommentHandler {
	return &CommunityCommentHandler{service: service}
}

// HandleRemoveComment serves social.coves.community.removeComment: the
// authenticated caller, a moderator of the comment's community or an instance
// admin, removes the comment from its community. It must run behind
// InstanceAdminMiddleware.RequireAuthenticated, which sets the caller DID.
func (h *CommunityCommentHandler) HandleRemoveComment(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Subject         string `json:"subject"`
		Code            string `json:"code"`
		Reason          string `json:"reason"`
		ExpectedVersion *int64 `json:"expectedVersion"`
		IdempotencyKey  string `json:"idempotencyKey"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID, ok := requireCommunityPostInput(w, r, input.ExpectedVersion)
	if !ok {
		return
	}
	result, err := h.service.RemoveComment(r.Context(), moderation.RemoveCommentRequest{
		ActorDID: actorDID, CommentURI: input.Subject, Code: input.Code, Reason: input.Reason,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	writeCommunityStandResult(w, "removeComment", "comment", result, err)
}

// HandleRestoreComment serves social.coves.community.restoreComment: the
// authenticated caller, a moderator of the comment's community or an instance
// admin, restores a comment the community removed. It must run behind
// InstanceAdminMiddleware.RequireAuthenticated.
func (h *CommunityCommentHandler) HandleRestoreComment(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Subject         string `json:"subject"`
		ExpectedVersion *int64 `json:"expectedVersion"`
		IdempotencyKey  string `json:"idempotencyKey"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID, ok := requireCommunityPostInput(w, r, input.ExpectedVersion)
	if !ok {
		return
	}
	result, err := h.service.RestoreComment(r.Context(), moderation.RestoreCommentRequest{
		ActorDID: actorDID, CommentURI: input.Subject,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	writeCommunityStandResult(w, "restoreComment", "comment", result, err)
}
