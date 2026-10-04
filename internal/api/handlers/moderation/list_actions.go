package moderation

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
)

// ListActionsHandler serves social.coves.moderation.listActions.
type ListActionsHandler struct {
	service moderation.Service
}

// NewListActionsHandler builds the handler over the moderation service.
func NewListActionsHandler(service moderation.Service) *ListActionsHandler {
	return &ListActionsHandler{service: service}
}

// HandleListActions answers the query.
func (h *ListActionsHandler) HandleListActions(w http.ResponseWriter, r *http.Request) {
	params, err := decodeActionListParams(r, false)
	if err != nil {
		writeActionListError(w, err)
		return
	}
	page, err := h.service.ListActions(r.Context(), params.ListActionsParams)
	if err == nil && page == nil {
		err = errNoActionPage
	}
	if err != nil {
		writeActionListError(w, err)
		return
	}
	xrpc.WriteJSON(w, http.StatusOK, page)
}

// errNoActionPage names a service that returned neither a page nor an error,
// so the 500 it causes is logged with a cause.
var errNoActionPage = errors.New("moderation service returned no page")

type actionListQueryField struct {
	name   string
	target *string
}

// decodeActionListParams reads the endpoint's parameters. Each may appear at
// most once and never empty; actionId is read only for the admin endpoint.
func decodeActionListParams(r *http.Request, admin bool) (moderation.ListAdminActionsParams, error) {
	values := r.URL.Query()
	var params moderation.ListAdminActionsParams
	var limit string
	fields := []actionListQueryField{
		{"limit", &limit},
		{"cursor", &params.Cursor},
		{"subject", &params.Subject},
		{"collection", &params.Collection},
		{"action", &params.Action},
		{"origin", &params.Origin},
		{"authority", &params.Authority},
		{"actor", &params.Actor},
		{"community", &params.Community},
		{"since", &params.Since},
		{"until", &params.Until},
	}
	if admin {
		fields = append(fields, actionListQueryField{"actionId", &params.ActionID})
	}
	for _, field := range fields {
		entries, present := values[field.name]
		if !present {
			continue
		}
		if len(entries) > 1 {
			return params, fmt.Errorf("%w: repeated %s", moderation.ErrInvalidRequest, field.name)
		}
		if entries[0] == "" {
			return params, fmt.Errorf("%w: empty %s", moderation.ErrInvalidRequest, field.name)
		}
		*field.target = entries[0]
	}
	if limit != "" {
		parsed, err := strconv.Atoi(limit)
		if err != nil {
			return params, fmt.Errorf("%w: limit must be an integer", moderation.ErrInvalidRequest)
		}
		params.Limit = &parsed
	}
	return params, nil
}
