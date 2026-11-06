package routes

import (
	labelhandlers "Coves/internal/api/handlers/labels"
	"Coves/internal/core/labels"

	"github.com/go-chi/chi/v5"
)

// RegisterLabelRoutes registers the public com.atproto.label.queryLabels endpoint.
func RegisterLabelRoutes(r chi.Router, reader labels.Reader) {
	r.Method("GET", "/xrpc/com.atproto.label.queryLabels", labelhandlers.NewQueryLabelsHandler(reader))
}

// RegisterLabelStreamRoutes registers the public com.atproto.label.subscribeLabels endpoint.
func RegisterLabelStreamRoutes(r chi.Router, stream *labels.Stream) {
	r.Method("GET", "/xrpc/com.atproto.label.subscribeLabels", labelhandlers.NewSubscribeLabelsHandler(stream))
}
