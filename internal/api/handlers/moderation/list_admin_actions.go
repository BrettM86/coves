package moderation

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// ListAdminActionsHandler serves social.coves.moderation.listAdminActions.
type ListAdminActionsHandler struct {
	service moderation.Service
}

// NewListAdminActionsHandler builds the handler over the moderation service.
func NewListAdminActionsHandler(service moderation.Service) *ListAdminActionsHandler {
	return &ListAdminActionsHandler{service: service}
}

// HandleListAdminActions answers the query.
func (h *ListAdminActionsHandler) HandleListAdminActions(w http.ResponseWriter, r *http.Request) {
	params, err := decodeActionListParams(r, true)
	if err != nil {
		writeActionListError(w, err)
		return
	}
	page, err := h.service.ListAdminActions(r.Context(), params)
	if err == nil && page == nil {
		err = errNoActionPage
	}
	if err != nil {
		writeActionListError(w, err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, page)
}
