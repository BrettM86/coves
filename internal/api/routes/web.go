package routes

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
	"Coves/internal/web"
)

// RegisterWebRoutes registers all web page routes for the Coves frontend.
// This includes the landing page, account deletion flow, and static assets.
//
// turnstileSiteKey is the public Cloudflare Turnstile site key. May be empty;
// when empty, GET /m/turnstile.html returns 503 and the mobile signup flow
// fails closed at the captcha step (matching the requestSignupToken handler).
func RegisterWebRoutes(r chi.Router, oauthClient *oauth.OAuthClient, userService users.UserService, turnstileSiteKey string) {
	// Initialize templates
	templates, err := web.NewTemplates()
	if err != nil {
		panic("failed to load web templates: " + err.Error())
	}

	// Create handlers
	handlers := web.NewHandlers(templates, oauthClient, userService, turnstileSiteKey)

	// Landing page
	r.Get("/", handlers.LandingHandler)

	// Account deletion flow
	r.Get("/delete-account", handlers.DeleteAccountPageHandler)
	r.Post("/delete-account", handlers.DeleteAccountSubmitHandler)
	r.Get("/delete-account/success", handlers.DeleteAccountSuccessHandler)

	// Legal pages
	r.Get("/privacy", handlers.PrivacyHandler)

	// Safety pages
	r.Get("/safety/child-safety", handlers.ChildSafetyHandler)

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
