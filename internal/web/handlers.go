package web

import (
	"log"
	"log/slog"
	"net/http"

	"Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
)

// Handlers provides HTTP handlers for the Coves web interface.
// This includes the landing page, static files, and account management.
type Handlers struct {
	templates   *Templates
	oauthClient *oauth.OAuthClient
	userService users.UserService
}

// NewHandlers creates a new Handlers instance with the provided dependencies.
func NewHandlers(templates *Templates, oauthClient *oauth.OAuthClient, userService users.UserService) *Handlers {
	return &Handlers{
		templates:   templates,
		oauthClient: oauthClient,
		userService: userService,
	}
}

// LandingPageData holds data for the landing page template.
type LandingPageData struct {
	// Title is the page title
	Title string
	// Description is the meta description for SEO
	Description string
	// AppStoreURL is the URL for the iOS App Store listing
	AppStoreURL string
	// PlayStoreURL is the URL for the Google Play Store listing
	PlayStoreURL string
}

// LandingHandler handles GET / requests and renders the landing page.
func (h *Handlers) LandingHandler(w http.ResponseWriter, r *http.Request) {
	// Only handle exact root path - let other routes handle their own paths
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := LandingPageData{
		Title:       "Coves - Community-Driven Forums on atProto",
		Description: "Coves is a forum-like social app built on the AT Protocol. Join communities, share content, and own your data.",
		// App store URLs - update these when apps are published
		AppStoreURL:  "https://apps.apple.com/app/coves",
		PlayStoreURL: "https://play.google.com/store/apps/details?id=social.coves.app",
	}

	if err := h.templates.Render(w, "landing.html", data); err != nil {
		log.Printf("Failed to render landing page: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// DeleteAccountPageData contains the data for the delete account template
type DeleteAccountPageData struct {
	LoggedIn bool
	Handle   string
	DID      string
}

// DeleteAccountPageHandler renders the delete account page
// GET /delete-account
func (h *Handlers) DeleteAccountPageHandler(w http.ResponseWriter, r *http.Request) {
	data := DeleteAccountPageData{
		LoggedIn: false,
	}

	// Check for session cookie
	cookie, err := r.Cookie("coves_session")
	if err == nil && cookie.Value != "" {
		// Try to unseal the session
		sealed, err := h.oauthClient.UnsealSession(cookie.Value)
		if err == nil && sealed != nil {
			// Session is valid, get user info
			user, err := h.userService.GetUserByDID(r.Context(), sealed.DID)
			if err == nil && user != nil {
				data.LoggedIn = true
				data.Handle = user.Handle
				data.DID = user.DID
			} else {
				slog.Warn("delete account: failed to get user by DID",
					"did", sealed.DID, "error", err)
			}
		} else {
			slog.Debug("delete account: invalid or expired session", "error", err)
		}
	}

	if err := h.templates.Render(w, "delete_account.html", data); err != nil {
		slog.Error("failed to render delete account template", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// DeleteAccountSubmitHandler processes the account deletion request
// POST /delete-account
func (h *Handlers) DeleteAccountSubmitHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Verify session
	cookie, err := r.Cookie("coves_session")
	if err != nil || cookie.Value == "" {
		slog.Warn("delete account submit: no session cookie")
		http.Redirect(w, r, "/delete-account", http.StatusFound)
		return
	}

	// Unseal the session
	sealed, err := h.oauthClient.UnsealSession(cookie.Value)
	if err != nil || sealed == nil {
		slog.Warn("delete account submit: invalid session", "error", err)
		h.clearSessionCookie(w)
		http.Redirect(w, r, "/delete-account", http.StatusFound)
		return
	}

	// Parse form to check confirmation checkbox
	if err := r.ParseForm(); err != nil {
		slog.Error("delete account submit: failed to parse form", "error", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Verify confirmation checkbox was checked
	if r.FormValue("confirm") != "true" {
		slog.Warn("delete account submit: confirmation not checked", "did", sealed.DID)
		http.Redirect(w, r, "/delete-account", http.StatusFound)
		return
	}

	// Delete the user's account
	err = h.userService.DeleteAccount(ctx, sealed.DID)
	if err != nil {
		slog.Error("delete account submit: failed to delete account",
			"did", sealed.DID, "error", err)
		http.Error(w, "Failed to delete account", http.StatusInternalServerError)
		return
	}

	slog.Info("account deleted successfully via web", "did", sealed.DID)

	// Clear the session cookie
	h.clearSessionCookie(w)

	// Redirect to success page
	http.Redirect(w, r, "/delete-account/success", http.StatusFound)
}

// DeleteAccountSuccessHandler renders the deletion success page
// GET /delete-account/success
func (h *Handlers) DeleteAccountSuccessHandler(w http.ResponseWriter, r *http.Request) {
	if err := h.templates.Render(w, "delete_success.html", nil); err != nil {
		slog.Error("failed to render delete success template", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// clearSessionCookie clears the session cookie
func (h *Handlers) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   "coves_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}

// PrivacyHandler handles GET /privacy requests and renders the privacy policy page.
func (h *Handlers) PrivacyHandler(w http.ResponseWriter, r *http.Request) {
	if err := h.templates.Render(w, "privacy.html", nil); err != nil {
		slog.Error("failed to render privacy policy template", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}
