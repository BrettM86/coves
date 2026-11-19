package routes

import (
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
	"Coves/internal/web"

	"github.com/go-chi/chi/v5"
)

// RegisterWebRoutes registers all web page routes for the Coves frontend.
// This includes the landing page, account deletion flow, and static assets.
//
// The account deletion page and the deletion submit run behind
// auth.OptionalAuth, so both see only a session that still exists server-side;
// a submit without one is sent back to the page with its cookie cleared, and a
// session store outage is a 503. publicURL is the browser-facing origin the submit's Origin
// header must match.
//
// turnstileSiteKey is the public Cloudflare Turnstile site key. May be empty;
// when empty, GET /m/turnstile.html returns 503 and the mobile signup flow
// fails closed at the captcha step (matching the requestSignupToken handler).
func RegisterWebRoutes(r chi.Router, oauthClient *oauth.OAuthClient, userService users.UserService, auth *middleware.OAuthAuthMiddleware, publicURL, turnstileSiteKey string) {
	// Initialize templates
	templates, err := web.NewTemplates()
	if err != nil {
		panic("failed to load web templates: " + err.Error())
	}

	// Create handlers
	handlers := web.NewHandlers(templates, oauthClient, userService, publicURL, turnstileSiteKey)

	// Landing page
	r.Get("/", handlers.LandingHandler)
	r.Head("/", handlers.LandingHandler)

	// Account deletion flow
	r.With(auth.OptionalAuth).Get("/delete-account", handlers.DeleteAccountPageHandler)
	r.With(auth.OptionalAuth).Head("/delete-account", handlers.DeleteAccountPageHandler)
	r.With(auth.OptionalAuth).Post("/delete-account", handlers.DeleteAccountSubmitHandler)
	r.Get("/delete-account/success", handlers.DeleteAccountSuccessHandler)
	r.Head("/delete-account/success", handlers.DeleteAccountSuccessHandler)

	// Legal pages
	r.Get("/privacy", handlers.PrivacyHandler)
	r.Head("/privacy", handlers.PrivacyHandler)

	// Safety pages
	r.Get("/safety/child-safety", handlers.ChildSafetyHandler)
	r.Head("/safety/child-safety", handlers.ChildSafetyHandler)

	// Mobile Turnstile widget host page. Loaded by the Flutter WebView during
	// signup; success callback posts the token back via the "Turnstile" JS
	// channel. Origin is coves.social, which matches the Cloudflare dashboard
	// hostname binding for the site key.
	r.Get("/m/turnstile.html", handlers.TurnstileHandler)

	// Static files (images, etc.)
	r.Get("/static/*", func(w http.ResponseWriter, r *http.Request) {
		// Serve from project's static directory
		fs := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
		fs.ServeHTTP(w, r)
	})
}
