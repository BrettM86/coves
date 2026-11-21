package moderation

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// CommunityStateGetter reads a subject's community moderation state.
type CommunityStateGetter interface {
	GetCommunityState(ctx context.Context, req moderation.CommunityStateRequest) (*moderation.CommunityState, error)
}

// GetCommunityStateHandler serves social.coves.moderation.getCommunityState.
type GetCommunityStateHandler struct {
	service CommunityStateGetter
}

// NewGetCommunityStateHandler builds the handler over the service, which is
// required.
func NewGetCommunityStateHandler(service CommunityStateGetter) *GetCommunityStateHandler {
	if service == nil {
		panic("moderation.NewGetCommunityStateHandler: service is required")
	}
	return &GetCommunityStateHandler{service: service}
}

type communityStateOutput struct {
	Community string                 `json:"community"`
	Subject   string                 `json:"subject"`
	Kind      string                 `json:"kind"`
	Version   int64                  `json:"version"`
	Post      *communityPostState    `json:"post,omitempty"`
	Comment   *communityCommentState `json:"comment,omitempty"`
	Ban       *communityBanState     `json:"ban,omitempty"`
	Roster    *communityRosterState  `json:"roster,omitempty"`
}

type communityPostState struct {
	Status string `json:"status"`
}

type communityCommentState struct {
	Removed bool `json:"removed"`
}

type communityBanState struct {
	ExpiresAt *string `json:"expiresAt,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	RecordURI string  `json:"recordUri"`
}

type communityRosterMember struct {
	DID string `json:"did"`
}

type communityRosterState struct {
	Owner      *communityRosterMember  `json:"owner,omitempty"`
	Moderators []communityRosterMember `json:"moderators"`
}

// HandleGetCommunityState answers the query for the authenticated viewer. It
// must run behind InstanceAdminMiddleware.RequireAuthenticated, which sets the
// viewer DID.
func (h *GetCommunityStateHandler) HandleGetCommunityState(w http.ResponseWriter, r *http.Request) {
	viewerDID := middleware.GetUserDID(r)
	if viewerDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	query := r.URL.Query()
	state, err := h.service.GetCommunityState(r.Context(), moderation.CommunityStateRequest{
		ViewerDID:    viewerDID,
		Subject:      query.Get("subject"),
		CommunityDID: query.Get("community"),
	})
	if err != nil {
		writeCommunityStateError(w, err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, communityStateResponse(state))
}

func communityStateResponse(state *moderation.CommunityState) communityStateOutput {
	output := communityStateOutput{
		Community: state.CommunityDID,
		Subject:   state.Subject,
		Kind:      string(state.Kind),
		Version:   state.Version,
	}
	switch state.Kind {
	case moderation.CommunitySubjectPost:
		output.Post = &communityPostState{Status: state.AdmissionStatus}
	case moderation.CommunitySubjectComment:
		output.Comment = &communityCommentState{Removed: state.CommentRemoved}
	case moderation.CommunitySubjectAccount:
		if ban := state.Ban; ban != nil {
			banState := &communityBanState{Reason: ban.Reason, RecordURI: ban.URI()}
			if ban.ExpiresAt != nil {
				expiresAt := ban.ExpiresAt.UTC().Format(time.RFC3339Nano)
				banState.ExpiresAt = &expiresAt
			}
			output.Ban = banState
		}
	case moderation.CommunitySubjectRoster:
		roster := &communityRosterState{Moderators: []communityRosterMember{}}
		if state.Roster != nil {
			if state.Roster.Owner != nil {
				roster.Owner = &communityRosterMember{DID: state.Roster.Owner.SubjectDID}
			}
			for _, moderator := range state.Roster.Moderators {
				roster.Moderators = append(roster.Moderators, communityRosterMember{DID: moderator.SubjectDID})
			}
		}
		output.Roster = roster
	}
	return output
}

func writeCommunityStateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, moderation.ErrAuthRequired):
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
	case errors.Is(err, moderation.ErrForbidden):
		xrpc.WriteError(w, http.StatusForbidden, "Forbidden", "Not a moderator of the subject's community")
	case errors.Is(err, moderation.ErrInvalidRequest):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "A DID subject requires a valid community DID")
	case errors.Is(err, moderation.ErrInvalidSubject):
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidSubject", "Subject must be an indexed post or comment at-URI, or a DID")
	case errors.Is(err, context.Canceled):
		xrpc.WriteError(w, http.StatusBadRequest, "RequestCanceled", "Request was canceled")
	case errors.Is(err, moderation.ErrModerationUnavailable):
		slog.Error("community moderation state unavailable", "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation state temporarily unavailable")
	default:
		slog.Error("unexpected community moderation state failure", "error", err)
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
