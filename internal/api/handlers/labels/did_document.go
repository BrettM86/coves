// Package labels serves the labeler identity and label query endpoints.
package labels

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"Coves/internal/core/labels"
)

// NewDIDDocumentHandler serves GET /.well-known/did.json for the instance DID.
// The document comes from the Publisher, so the #atproto_label key it lists is
// the key the Publisher signs with.
func NewDIDDocumentHandler(p *labels.Publisher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := json.Marshal(p.DIDDocument())
		if err != nil {
			slog.Error("encoding the instance DID document failed", slog.String("error", err.Error()))
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}
