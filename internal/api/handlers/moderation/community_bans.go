package moderation

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// CommunityBanActions runs community bans, unbans and ban listings.
type CommunityBanActions interface {
	BanCommunityAccount(ctx context.Context, req moderation.BanCommunityAccountRequest) (*moderation.CommunityActionResult, error)
	UnbanCommunityAccount(ctx context.Context, req moderation.UnbanCommunityAccountRequest) (*moderation.CommunityActionResult, error)
	ListCommunityBans(ctx context.Context, req moderation.ListCommunityBansRequest) (*moderation.CommunityBanPage, error)
}

// CommunityBanHandler serves social.coves.community.banAccount, unbanAccount
// and listBans.
type CommunityBanHandler struct {
	service CommunityBanActions
}

// NewCommunityBanHandler builds the handler over the service.
func NewCommunityBanHandler(service CommunityBanActions) *CommunityBanHandler {
	return &CommunityBanHandler{service: service}
}

// HandleBanAccount serves social.coves.community.banAccount: the
// authenticated caller, a moderator of the community or an instance admin,
// bans an account that holds no role there. It must run behind
// InstanceAdminMiddleware.RequireAuthenticated, which sets the caller DID.
func (h *CommunityBanHandler) HandleBanAccount(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Community       string     `json:"community"`
		Subject         string     `json:"subject"`
		Reason          string     `json:"reason"`
		ExpiresAt       *time.Time `json:"expiresAt"`
		ExpectedVersion *int64     `json:"expectedVersion"`
		IdempotencyKey  string     `json:"idempotencyKey"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID, ok := requireCommunityPostInput(w, r, input.ExpectedVersion)
	if !ok {
		return
	}
	result, err := h.service.BanCommunityAccount(r.Context(), moderation.BanCommunityAccountRequest{
		ActorDID: actorDID, CommunityDID: input.Community, SubjectDID: input.Subject,
		Reason: input.Reason, ExpiresAt: input.ExpiresAt,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	writeCommunityBanResult(w, "banAccount", result, err)
}

// HandleUnbanAccount serves social.coves.community.unbanAccount: the
// authenticated caller, a moderator of the community or an instance admin,
// lifts an account's ban. It must run behind
// InstanceAdminMiddleware.RequireAuthenticated.
func (h *CommunityBanHandler) HandleUnbanAccount(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Community       string `json:"community"`
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
	result, err := h.service.UnbanCommunityAccount(r.Context(), moderation.UnbanCommunityAccountRequest{
		ActorDID: actorDID, CommunityDID: input.Community, SubjectDID: input.Subject,
		ExpectedVersion: *input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
	})
	writeCommunityBanResult(w, "unbanAccount", result, err)
}

// communityBanView is one ban in the listBans output.
type communityBanView struct {
	Subject   string     `json:"subject"`
	URI       string     `json:"uri"`
	CID       string     `json:"cid"`
	Reason    string     `json:"reason,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	IndexedAt time.Time  `json:"indexedAt"`
	Active    bool       `json:"active"`
}

// HandleListBans serves social.coves.community.listBans: the community's
// standing bans, in force and lapsed, for the authenticated caller, a
// moderator of the community or an instance admin.
func (h *CommunityBanHandler) HandleListBans(w http.ResponseWriter, r *http.Request) {
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	query := r.URL.Query()
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "limit must be an integer")
			return
		}
		limit = parsed
	}
	page, err := h.service.ListCommunityBans(r.Context(), moderation.ListCommunityBansRequest{
		ActorDID: actorDID, CommunityDID: query.Get("community"), Limit: limit, Cursor: query.Get("cursor"),
	})
	if err == nil && page == nil {
		err = errors.New("community ban list returned no page")
	}
	if err != nil {
		writeCommunityBanError(w, "listBans", err)
		return
	}
	output := struct {
		Bans   []communityBanView `json:"bans"`
		Cursor string             `json:"cursor,omitempty"`
	}{Bans: make([]communityBanView, 0, len(page.Bans)), Cursor: page.Cursor}
	for _, ban := range page.Bans {
		output.Bans = append(output.Bans, communityBanView{
			Subject: ban.SubjectDID, URI: ban.URI, CID: ban.CID, Reason: ban.Reason,
			ExpiresAt: utcTime(ban.ExpiresAt), CreatedAt: utcTime(ban.CreatedAt),
			IndexedAt: ban.IndexedAt.UTC(), Active: ban.Active,
		})
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

func utcTime(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	utc := at.UTC()
	return &utc
}

// writeCommunityBanResult writes a ban or unban's output, or its error.
func writeCommunityBanResult(w http.ResponseWriter, procedure string, result *moderation.CommunityActionResult, err error) {
	if err == nil && result == nil {
		err = errors.New("community ban action returned no result")
	}
	if err != nil {
		writeCommunityBanError(w, procedure, err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, communityPostOutput{
		URI: result.RecordURI, Version: result.Version, ActionID: result.ActionID, AlreadyApplied: result.AlreadyApplied,
	})
}

func writeCommunityBanError(w http.ResponseWriter, procedure string, err error) {
	switch {
	case errors.Is(err, moderation.ErrAuthRequired):
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
	case errors.Is(err, moderation.ErrForbidden):
		xrpc.WriteError(w, http.StatusForbidden, "Forbidden", "Not permitted to manage this community's bans")
	case errors.Is(err, moderation.ErrInvalidRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid community, subject, reason, expiresAt, expectedVersion, idempotencyKey, limit or cursor")
	case errors.Is(err, moderation.ErrSubjectHoldsRole):
		xrpc.WriteError(w, http.StatusConflict, "SubjectHoldsRole", "The account holds a role in this community; remove the role before banning it")
	case errors.Is(err, moderation.ErrStateConflict):
		xrpc.WriteError(w, http.StatusConflict, "StateConflict", "The account's community ban changed; reload and retry")
	case errors.Is(err, moderation.ErrIdempotencyConflict):
		xrpc.WriteError(w, http.StatusConflict, "IdempotencyConflict", "Idempotency key was used for a different request")
	case errors.Is(err, moderation.ErrRateLimited):
		xrpc.WriteError(w, http.StatusTooManyRequests, "RateLimitExceeded", "Too many bans; try again later")
	case errors.Is(err, context.Canceled):
		xrpc.WriteError(w, http.StatusBadRequest, "RequestCanceled", "Request was canceled")
	case errors.Is(err, moderation.ErrModerationUnavailable):
		slog.Error("community ban action unavailable", "procedure", procedure, "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation service temporarily unavailable")
	default:
		// DIDs, record URIs and database errors carry no credentials, so the
		// full error is logged; the response body stays generic.
		slog.Error("unexpected community ban action failure", "procedure", procedure, "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
