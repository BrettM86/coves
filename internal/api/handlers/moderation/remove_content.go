package moderation

import (
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// RemoveContentHandler serves social.coves.moderation.removeContent.
type RemoveContentHandler struct {
	service moderation.Service
}

// NewRemoveContentHandler builds the removeContent handler.
func NewRemoveContentHandler(service moderation.Service) *RemoveContentHandler {
	return &RemoveContentHandler{service: service}
}

// HandleRemoveContent handles POST /xrpc/social.coves.moderation.removeContent.
func (h *RemoveContentHandler) HandleRemoveContent(w http.ResponseWriter, r *http.Request) {
	if !requireMutationPOSTJSON(w, r) {
		return
	}
	var input struct {
		Subject         strongRefView `json:"subject"`
		ExpectedVersion string        `json:"expectedVersion"`
		IdempotencyKey  string        `json:"idempotencyKey"`
		Reason          string        `json:"reason"`
		PrivateNote     string        `json:"privateNote"`
	}
	if !decodeMutationRequest(w, r, &input) {
		return
	}
	actorDID := middleware.GetUserDID(r)
	if actorDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	result, err := h.service.RemoveContent(r.Context(), actorDID, moderation.RemoveContentRequest{
		Subject:         moderation.StrongRef{URI: input.Subject.URI, CID: input.Subject.CID},
		ExpectedVersion: input.ExpectedVersion, IdempotencyKey: input.IdempotencyKey,
		Reason: input.Reason, PrivateNote: input.PrivateNote,
	})
	writeMutationResult(w, "removeContent", result, err)
}

func requireMutationPOSTJSON(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		xrpc.WriteError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Content-Type must be application/json")
		return false
	}
	return true
}

func decodeMutationRequest(w http.ResponseWriter, r *http.Request, input any) bool {
	if err := reqbody.DecodeJSON(w, r, reqbody.LimitMedium, input); err != nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return false
	}
	return true
}

func writeMutationResult(w http.ResponseWriter, operation string, result *moderation.MutationResult, err error) {
	if err != nil || result == nil {
		writeMutationError(w, operation, err)
		return
	}
	state := result.State
	view := subjectStateView{
		Subject: state.Subject, Version: state.Version,
		Moderation: newModerationView(state.Moderation), RecordState: state.RecordState,
	}
	if state.CurrentSubject != nil {
		view.CurrentSubject = &strongRefView{URI: state.CurrentSubject.URI, CID: state.CurrentSubject.CID}
	}
	if state.LocalRemoval != nil {
		view.LocalRemoval = &actionRefView{ServiceDID: state.LocalRemoval.ServiceDID, ActionID: state.LocalRemoval.ActionID}
	}
	for _, label := range state.LocalLabels {
		view.LocalLabels = append(view.LocalLabels, localLabelView{
			Value:  label.Value,
			Action: actionRefView{ServiceDID: label.Action.ServiceDID, ActionID: label.Action.ActionID},
		})
	}
	response := struct {
		Outcome string                      `json:"outcome"`
		State   subjectStateView            `json:"state"`
		Action  *moderation.AdminActionView `json:"action,omitempty"`
	}{Outcome: result.Outcome, State: view}
	if result.Action != nil {
		projection := moderation.NewAdminActionView(*result.Action)
		response.Action = &projection
	}
	xrpc.WriteJSON(w, http.StatusOK, response)
}

func writeMutationError(w http.ResponseWriter, operation string, err error) {
	for _, entry := range []struct {
		cause error
		code  string
	}{
		{moderation.ErrInvalidRequest, "InvalidRequest"},
		{moderation.ErrInvalidSubject, "InvalidSubject"},
		{moderation.ErrSubjectNotFound, "SubjectNotFound"},
		{moderation.ErrDecisionNotFound, "DecisionNotFound"},
		{moderation.ErrInvalidDecision, "InvalidDecision"},
		{moderation.ErrContentChanged, "ContentChanged"},
		{moderation.ErrStateConflict, "StateConflict"},
		{moderation.ErrIdempotencyConflict, "IdempotencyConflict"},
		{moderation.ErrUnsupportedReason, "UnsupportedReason"},
		{moderation.ErrUnsupportedLabel, "UnsupportedLabel"},
	} {
		if errors.Is(err, entry.cause) {
			// Rule errors carry only fixed text and configured limits, such as
			// "unsupported subject collection" or the live-key limit, never request
			// payloads, so the detail is safe to show the caller.
			xrpc.WriteError(w, http.StatusBadRequest, entry.code, strings.TrimPrefix(err.Error(), "moderation: "))
			return
		}
	}
	if errors.Is(err, moderation.ErrModerationUnavailable) {
		// Subject AT-URIs, DIDs and database errors carry no credentials or
		// private notes, so the full error is logged; the response stays generic.
		slog.Error("moderation mutation unavailable", "operation", operation, "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ModerationUnavailable", "Moderation service temporarily unavailable")
		return
	}
	slog.Error("unexpected moderation mutation failure", "operation", operation, "error", err)
	xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
}
