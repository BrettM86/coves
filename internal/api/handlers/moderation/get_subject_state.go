// Package moderation serves the social.coves.moderation.* XRPC endpoints.
package moderation

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// GetSubjectStateHandler serves social.coves.moderation.getSubjectState.
type GetSubjectStateHandler struct {
	service moderation.Service
}

// NewGetSubjectStateHandler builds the handler over the moderation service.
func NewGetSubjectStateHandler(service moderation.Service) *GetSubjectStateHandler {
	return &GetSubjectStateHandler{service: service}
}

// HandleGetSubjectState answers the query.
func (h *GetSubjectStateHandler) HandleGetSubjectState(w http.ResponseWriter, r *http.Request) {
	subject := r.URL.Query().Get("subject")
	if subject == "" {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidSubject", "subject is required")
		return
	}
	state, err := h.service.GetSubjectState(r.Context(), subject)
	if err != nil {
		writeSubjectStateError(w, err)
		return
	}
	if state == nil {
		writeSubjectStateError(w, nil)
		return
	}

	view := subjectStateView{
		Subject:     state.Subject,
		Version:     state.Version,
		Moderation:  moderationView{State: state.Moderation.State},
		RecordState: state.RecordState,
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
	xrpc.WriteJSON(w, http.StatusOK, struct {
		State subjectStateView `json:"state"`
	}{State: view})
}

type subjectStateView struct {
	Subject        string                 `json:"subject"`
	Version        string                 `json:"version"`
	Moderation     moderationView         `json:"moderation"`
	RecordState    moderation.RecordState `json:"recordState"`
	CurrentSubject *strongRefView         `json:"currentSubject,omitempty"`
	LocalRemoval   *actionRefView         `json:"localRemoval,omitempty"`
	LocalLabels    []localLabelView       `json:"localLabels,omitempty"`
}

type moderationView struct {
	State string `json:"state"`
}

type strongRefView struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

type actionRefView struct {
	ServiceDID string `json:"serviceDid"`
	ActionID   string `json:"actionId"`
}

type localLabelView struct {
	Value  string        `json:"value"`
	Action actionRefView `json:"action"`
}
