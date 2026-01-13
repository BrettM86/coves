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
func RegisterWebRoutes(r chi.Router, oauthClient *oauth.OAuthClient, userService users.UserService) {
	// Initialize templates
	templates, err := web.NewTemplates()
	if err != nil {
		panic("failed to load web templates: " + err.Error())
	}

	// Create handlers
	handlers := web.NewHandlers(templates, oauthClient, userService)

	// Landing page
	r.Get("/", handlers.LandingHandler)

	// Account deletion flow
	r.Get("/delete-account", handlers.DeleteAccountPageHandler)
	r.Post("/delete-account", handlers.DeleteAccountSubmitHandler)
	r.Get("/delete-account/success", handlers.DeleteAccountSuccessHandler)

	// Static files (images, etc.)
	r.Get("/static/*", func(w http.ResponseWriter, r *http.Request) {
		// Serve from project's static directory
		fs := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
		fs.ServeHTTP(w, r)
	})
}
