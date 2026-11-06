package labels

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/labels"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
)

// NewQueryLabelsHandler serves com.atproto.label.queryLabels from reader.
func NewQueryLabelsHandler(reader labels.Reader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parameters := r.URL.Query()
		query := labels.Query{
			URIPatterns: parameters["uriPatterns"],
			Sources:     parameters["sources"],
			Cursor:      parameters.Get("cursor"),
			Limit:       labels.DefaultQueryLimit,
		}
		if len(query.URIPatterns) == 0 {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "uriPatterns is required")
			return
		}
		if err := query.ValidateValues(); err != nil {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
			return
		}
		if rawLimit := parameters.Get("limit"); rawLimit != "" {
			limit, err := strconv.Atoi(rawLimit)
			if err != nil || limit < 1 || limit > labels.MaxQueryLimit {
				xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "limit must be between 1 and 250")
				return
			}
			query.Limit = limit
		}

		page, err := reader.Query(r.Context(), query)
		if errors.Is(err, labels.ErrInvalidQuery) {
			xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
			return
		}
		if err != nil {
			slog.Error("queryLabels failed", slog.String("error", err.Error()))
			xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
			return
		}

		output := comatproto.LabelQueryLabels_Output{Labels: make([]*comatproto.LabelDefs_Label, 0, len(page.Labels))}
		for _, label := range page.Labels {
			lexicon := label.Lexicon()
			output.Labels = append(output.Labels, &lexicon)
		}
		if page.Cursor != "" {
			output.Cursor = &page.Cursor
		}
		xrpc.WriteJSON(w, http.StatusOK, output)
	})
}
