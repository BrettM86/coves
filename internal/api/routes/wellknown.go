package routes

import (
	"Coves/internal/api/handlers/wellknown"

	"github.com/go-chi/chi/v5"
)

// RegisterWellKnownRoutes registers RFC 8615 well-known URI endpoints
// These endpoints are used for service discovery and mobile app deep linking
//
// Spec: https://www.rfc-editor.org/rfc/rfc8615.html
func RegisterWellKnownRoutes(r chi.Router) {
	// iOS Universal Links configuration
	// Required for cryptographically-bound deep linking on iOS
	// Must be served at exact path /.well-known/apple-app-site-association
	// Content-Type: application/json (no redirects allowed)
	r.Get("/.well-known/apple-app-site-association", wellknown.HandleAppleAppSiteAssociation)

	// Android App Links configuration
	// Required for cryptographically-bound deep linking on Android
	// Must be served at exact path /.well-known/assetlinks.json
	// Content-Type: application/json (no redirects allowed)
	r.Get("/.well-known/assetlinks.json", wellknown.HandleAssetLinks)
}
