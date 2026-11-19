package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
)

// Handlers provides HTTP handlers for the Coves web interface.
// This includes the landing page, static files, and account management.
type Handlers struct {
	templates        *Templates
	oauthClient      *oauth.OAuthClient
	userService      users.UserService
	publicURL        string
	turnstileSiteKey string
}

// NewHandlers creates a new Handlers instance with the provided dependencies.
// turnstileSiteKey is the public Cloudflare Turnstile site key injected into the
// /m/turnstile.html page. May be empty — in which case TurnstileHandler returns 503.
// publicURL is the browser-facing origin that POST /delete-account requires
// in its Origin header when one is sent.
func NewHandlers(templates *Templates, oauthClient *oauth.OAuthClient, userService users.UserService, publicURL, turnstileSiteKey string) *Handlers {
	return &Handlers{
		templates:        templates,
		oauthClient:      oauthClient,
		userService:      userService,
		publicURL:        publicURL,
		turnstileSiteKey: turnstileSiteKey,
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
		Title:        "Coves - Community-Driven Forums on atProto",
		Description:  "Coves is a forum-like social app built on the AT Protocol. Join communities, share content, and own your data.",
		AppStoreURL:  "https://apps.apple.com/app/coves-social/id6758530907",
		PlayStoreURL: "https://play.google.com/store/apps/details?id=social.coves",
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
	// CSRFToken is the session-bound token the deletion form posts back.
	// Empty when no CSRF key is available, in which case submits are refused.
	CSRFToken string
}

// deleteAccountCSRFKeyLabel separates the delete-account CSRF key from every
// other use of the seal secret.
const deleteAccountCSRFKeyLabel = "coves web delete-account csrf v1"

// deleteAccountCSRFToken returns the CSRF token bound to one OAuth session:
// base64url(HMAC-SHA256(key, DID + "\x00" + sessionID)), where key is
// HMAC-SHA256(SealSecret, deleteAccountCSRFKeyLabel). It returns false when no
// seal secret is configured, so callers fail closed.
func (h *Handlers) deleteAccountCSRFToken(did, sessionID string) (string, bool) {
	if h.oauthClient == nil || len(h.oauthClient.SealSecret) == 0 {
		return "", false
	}
	keyMAC := hmac.New(sha256.New, h.oauthClient.SealSecret)
	keyMAC.Write([]byte(deleteAccountCSRFKeyLabel))
	tokenMAC := hmac.New(sha256.New, keyMAC.Sum(nil))
	tokenMAC.Write([]byte(did + "\x00" + sessionID))
	return base64.RawURLEncoding.EncodeToString(tokenMAC.Sum(nil)), true
}

// isSameOriginRequest reports whether a state-changing browser request comes
// from the configured public origin. A Sec-Fetch-Site of cross-site is
// refused; an Origin header, when present, must match publicURL's scheme and
// host (including port). A missing Origin header is allowed. An unparsable
// public URL refuses every request that carries an Origin header.
func (h *Handlers) isSameOriginRequest(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	public, err := url.Parse(h.publicURL)
	if err != nil || public.Scheme == "" || public.Host == "" {
		return false
	}
	requestOrigin, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(requestOrigin.Scheme, public.Scheme) && strings.EqualFold(requestOrigin.Host, public.Host)
}

// DeleteAccountPageHandler renders the delete account page.
// GET /delete-account, behind auth.OptionalAuth: the signed-in view is shown
// only when the middleware found a live server-side session.
func (h *Handlers) DeleteAccountPageHandler(w http.ResponseWriter, r *http.Request) {
	data := DeleteAccountPageData{
		LoggedIn: false,
	}

	did := middleware.GetUserDID(r)
	session := middleware.GetOAuthSession(r)
	if did != "" && session != nil {
		user, err := h.userService.GetUserByDID(r.Context(), did)
		if err == nil && user != nil {
			data.LoggedIn = true
			data.Handle = user.Handle
			data.DID = user.DID
			if token, ok := h.deleteAccountCSRFToken(did, session.SessionID); ok {
				data.CSRFToken = token
			} else {
				slog.Error("delete account: CSRF key unavailable; deletion form will be refused", "did", did)
			}
		} else {
			slog.Warn("delete account: failed to get user by DID",
				"did", did, "error", err)
		}
	}

	if err := h.templates.Render(w, "delete_account.html", data); err != nil {
		slog.Error("failed to render delete account template", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// DeleteAccountSubmitHandler processes the account deletion request.
// POST /delete-account, behind auth.OptionalAuth. Without a live server-side
// session (logged out in another tab, expired) the stale cookie is cleared and
// the browser is sent back to the page. The request must be same-origin and
// carry the CSRF token rendered for the caller's session.
func (h *Handlers) DeleteAccountSubmitHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	did := middleware.GetUserDID(r)
	session := middleware.GetOAuthSession(r)
	if did == "" || session == nil {
		slog.Warn("delete account submit: no authenticated session")
		h.clearSessionCookie(w)
		http.Redirect(w, r, "/delete-account", http.StatusFound)
		return
	}

	if !h.isSameOriginRequest(r) {
		slog.Warn("delete account submit: cross-origin request refused", "did", did)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Parse form to check the CSRF token and confirmation checkbox. The
	// legitimate payload is two small fields, so cap the body far below
	// net/http's 10MB form default before ParseForm reads it.
	r.Body = http.MaxBytesReader(w, r.Body, int64(reqbody.LimitTiny))
	if err := r.ParseForm(); err != nil { // coves:allow-raw-body-decode: form endpoint, not JSON — capped by the MaxBytesReader on the line above
		slog.Error("delete account submit: failed to parse form", "error", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	expectedToken, ok := h.deleteAccountCSRFToken(did, session.SessionID)
	if !ok || !hmac.Equal([]byte(r.PostFormValue("csrf_token")), []byte(expectedToken)) {
		slog.Warn("delete account submit: missing or invalid CSRF token", "did", did)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Verify confirmation checkbox was checked
	if r.FormValue("confirm") != "true" {
		slog.Warn("delete account submit: confirmation not checked", "did", did)
		http.Redirect(w, r, "/delete-account", http.StatusFound)
		return
	}

	// Delete the user's account
	if err := h.userService.DeleteAccount(ctx, did); err != nil {
		slog.Error("delete account submit: failed to delete account",
			"did", did, "error", err)
		http.Error(w, "Failed to delete account", http.StatusInternalServerError)
		return
	}

	slog.Info("account deleted successfully via web", "did", did)

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

// ChildSafetyHandler handles GET /safety/child-safety requests and renders the child safety standards page.
func (h *Handlers) ChildSafetyHandler(w http.ResponseWriter, r *http.Request) {
	if err := h.templates.Render(w, "child-safety.html", nil); err != nil {
		slog.Error("failed to render child safety standards template", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// TurnstilePageData holds data injected into the Turnstile widget page.
type TurnstilePageData struct {
	// SiteKey is the public Cloudflare Turnstile site key. Embedded into the
	// rendered HTML server-side so the mobile binary doesn't need to ship it.
	SiteKey string
}

// TurnstileHandler renders the Cloudflare Turnstile widget host page for the
// mobile WebView. The mobile signup flow loads this URL, the user solves the
// widget, and the success callback posts the token back to Flutter via the
// "Turnstile" JS channel.
//
// Symmetric with the requestSignupToken endpoint: if TURNSTILE_SITE_KEY is
// missing the handler returns 503 rather than rendering a broken widget.
// The signup flow then fails closed at the captcha step.
func (h *Handlers) TurnstileHandler(w http.ResponseWriter, r *http.Request) {
	if h.turnstileSiteKey == "" {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "Turnstile not configured", http.StatusServiceUnavailable)
		return
	}

	// Lock the page to Cloudflare's challenge origin. The inline script and
	// style are tiny and stable; using nonces would add complexity without
	// meaningful gain on a self-contained page with no user input.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src 'unsafe-inline' https://challenges.cloudflare.com; "+
			"style-src 'unsafe-inline'; "+
			"frame-src https://challenges.cloudflare.com; "+
			"connect-src https://challenges.cloudflare.com",
	)
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The site key is public; cache briefly so the WebView doesn't re-fetch
	// on every signup retry, but stay short so a key rotation propagates fast.
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")

	if err := h.templates.Render(w, "turnstile.html", TurnstilePageData{SiteKey: h.turnstileSiteKey}); err != nil {
		slog.Error("failed to render turnstile template", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}
