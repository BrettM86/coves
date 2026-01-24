package routes

import (
	"github.com/go-chi/chi/v5"

	imageproxyhandlers "Coves/internal/api/handlers/imageproxy"
)

// RegisterImageProxyRoutes registers image proxy endpoints on the router.
// The image proxy serves transformed images from AT Protocol PDSes.
//
// Route: GET /img/{preset}/plain/{did}/{cid}
//
// Parameters:
//   - preset: Image transformation preset (e.g., "avatar", "banner", "content_preview")
//   - did: DID of the user who owns the blob
//   - cid: Content identifier of the blob
//
// The endpoint supports ETag-based caching with If-None-Match headers.
func RegisterImageProxyRoutes(r chi.Router, handler *imageproxyhandlers.Handler) {
	r.Get("/img/{preset}/plain/{did}/{cid}", handler.HandleImage)
}
