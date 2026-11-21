package moderation

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// CommunityPostActions runs community post removals and restores.
type CommunityPostActions interface {
	RemovePost(ctx context.Context, req moderation.RemovePostRequest) (*moderation.CommunityActionResult, error)
	RestorePost(ctx context.Context, req moderation.RestorePostRequest) (*moderation.CommunityActionResult, error)
}

// CommunityPostHandler serves social.coves.community.removePost and
// social.coves.community.restorePost.
type CommunityPostHandler struct {
	service CommunityPostActions
}

// NewCommunityPostHandler builds the handler over the service.
func NewCommunityPostHandler(service CommunityPostActions) *CommunityPostHandler {
	return &CommunityPostHandler{service: service}
}

// communityPostOutput is the output of both post procedures: the stand record
// (the removal for a remove, the acceptance for a restore), the post's
// community version after the action, the action written and whether the
// decision was already in place.
type communityPostOutput struct {
	URI            string `json:"uri"`
	Version        int64  `json:"version"`
	ActionID       string `json:"actionId,omitempty"`
	AlreadyApplied bool   `json:"alreadyApplied"`
}

// HandleRemovePost serves social.coves.community.removePost: the
// authenticated caller, a moderator of the post's community or an instance
// admin, removes the post from its community. It must run behind
// InstanceAdminMiddleware.RequireAuthenticated, which sets the caller DID.
func (h *CommunityPostHandler) HandleRemovePost(w http.ResponseWriter, r *http.Request) {
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
	result, err := h.service.RemovePost(r.Context(), moderation.RemovePostRequest{
		ActorDID: actorDID, PostURI: input.Subject, Code: input.Code, Reason: input.Reason,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	writeCommunityStandResult(w, "removePost", "post", result, err)
}

// HandleRestorePost serves social.coves.community.restorePost: the
// authenticated caller, a moderator of the post's community or an instance
// admin, restores a post the community removed. It must run behind
// InstanceAdminMiddleware.RequireAuthenticated.
func (h *CommunityPostHandler) HandleRestorePost(w http.ResponseWriter, r *http.Request) {
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
	result, err := h.service.RestorePost(r.Context(), moderation.RestorePostRequest{
		ActorDID: actorDID, PostURI: input.Subject,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	writeCommunityStandResult(w, "restorePost", "post", result, err)
}

// requireCommunityPostInput returns the authenticated caller, refusing a
// request without one or without expectedVersion. The comment procedures use
// it too.
func requireCommunityPostInput(w http.ResponseWriter, r *http.Request, expectedVersion *int64) (string, bool) {
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return "", false
	}
	if expectedVersion == nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "expectedVersion is required")
		return "", false
	}
	return actorDID, true
}

// writeCommunityStandResult writes a community post or comment action's
// output, or its error; subject ("post" or "comment") names what was acted on
// in error messages.
func writeCommunityStandResult(w http.ResponseWriter, procedure, subject string, result *moderation.CommunityActionResult, err error) {
	if err == nil && result == nil {
		err = errors.New("community " + subject + " action returned no result")
	}
	if err != nil {
		writeCommunityStandError(w, procedure, subject, err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, communityPostOutput{
		URI: result.RecordURI, Version: result.Version, ActionID: result.ActionID, AlreadyApplied: result.AlreadyApplied,
	})
}

func writeCommunityStandError(w http.ResponseWriter, procedure, subject string, err error) {
	switch {
	case errors.Is(err, moderation.ErrAuthRequired):
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
	case errors.Is(err, moderation.ErrForbidden):
		xrpc.WriteError(w, http.StatusForbidden, "Forbidden", "Not permitted to moderate this community's "+subject+"s")
	case errors.Is(err, moderation.ErrInvalidRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid subject, code, reason, expectedVersion or idempotencyKey")
	case errors.Is(err, moderation.ErrPostNotFound):
		xrpc.WriteError(w, http.StatusNotFound, "PostNotFound", "The post is not indexed in a community")
	case errors.Is(err, moderation.ErrCommentNotFound):
		xrpc.WriteError(w, http.StatusNotFound, "CommentNotFound", "The comment is not indexed under a post in a community")
	case errors.Is(err, moderation.ErrStateConflict):
		xrpc.WriteError(w, http.StatusConflict, "StateConflict", "The "+subject+"'s community state changed; reload and retry")
	case errors.Is(err, moderation.ErrIdempotencyConflict):
		xrpc.WriteError(w, http.StatusConflict, "IdempotencyConflict", "Idempotency key was used for a different request")
	case errors.Is(err, moderation.ErrRateLimited):
		xrpc.WriteError(w, http.StatusTooManyRequests, "RateLimitExceeded", "Too many removals; try again later")
	case errors.Is(err, context.Canceled):
		xrpc.WriteError(w, http.StatusBadRequest, "RequestCanceled", "Request was canceled")
	case errors.Is(err, moderation.ErrModerationUnavailable):
		slog.Error("community "+subject+" action unavailable", "procedure", procedure, "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation service temporarily unavailable")
	default:
		// DIDs, record URIs and database errors carry no credentials, so the
		// full error is logged; the response body stays generic.
		slog.Error("unexpected community "+subject+" action failure", "procedure", procedure, "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
