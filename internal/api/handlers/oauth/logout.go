package oauth

import (
	"log"
	"net/http"

	oauthCore "Coves/internal/core/oauth"
)

// LogoutHandler handles user logout
type LogoutHandler struct {
	sessionStore oauthCore.SessionStore
}

// NewLogoutHandler creates a new logout handler
func NewLogoutHandler(sessionStore oauthCore.SessionStore) *LogoutHandler {
	return &LogoutHandler{
		sessionStore: sessionStore,
	}
}

// HandleLogout logs out the current user
// POST /oauth/logout
func (h *LogoutHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get HTTP session
	cookieStore := GetCookieStore()
	httpSession, err := cookieStore.Get(r, sessionName)
	if err != nil || httpSession.IsNew {
		// No session to logout
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Get DID from session
	did, ok := httpSession.Values[sessionDID].(string)
	if !ok || did == "" {
		// No DID in session
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Delete OAuth session from database
	if err := h.sessionStore.DeleteSession(did); err != nil {
		log.Printf("Failed to delete OAuth session for DID %s: %v", did, err)
		// Continue with logout anyway
	}

	// Clear HTTP session cookie
	httpSession.Options.MaxAge = -1 // Delete cookie
	if err := httpSession.Save(r, w); err != nil {
		log.Printf("Failed to clear HTTP session: %v", err)
	}

	// Redirect to home
	http.Redirect(w, r, "/", http.StatusFound)
}

// GetCurrentUser returns the currently authenticated user's DID
// Helper function for other handlers
func GetCurrentUser(r *http.Request) (string, error) {
	cookieStore := GetCookieStore()
	httpSession, err := cookieStore.Get(r, sessionName)
	if err != nil || httpSession.IsNew {
		return "", err
	}

	did, ok := httpSession.Values[sessionDID].(string)
	if !ok || did == "" {
		return "", nil
	}

	return did, nil
}

// GetCurrentUserOrError returns the current user's DID or sends an error response
// Helper function for protected handlers
func GetCurrentUserOrError(w http.ResponseWriter, r *http.Request) (string, bool) {
	did, err := GetCurrentUser(r)
	if err != nil || did == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return "", false
	}

	return did, true
}
