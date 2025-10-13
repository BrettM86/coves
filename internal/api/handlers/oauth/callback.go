package oauth

import (
	"Coves/internal/atproto/oauth"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	oauthCore "Coves/internal/core/oauth"
)

const (
	sessionName = "coves_session"
	sessionDID  = "did"
)

// CallbackHandler handles OAuth callback
type CallbackHandler struct {
	sessionStore oauthCore.SessionStore
}

// NewCallbackHandler creates a new callback handler
func NewCallbackHandler(sessionStore oauthCore.SessionStore) *CallbackHandler {
	return &CallbackHandler{
		sessionStore: sessionStore,
	}
}

// HandleCallback processes the OAuth callback
// GET /oauth/callback?code=...&state=...&iss=...
func (h *CallbackHandler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	// Extract query parameters
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	iss := r.URL.Query().Get("iss")
	errorParam := r.URL.Query().Get("error")
	errorDesc := r.URL.Query().Get("error_description")

	// Check for authorization errors
	if errorParam != "" {
		log.Printf("OAuth error: %s - %s", errorParam, errorDesc)
		http.Error(w, "Authorization failed", http.StatusBadRequest)
		return
	}

	// Validate required parameters
	if code == "" || state == "" || iss == "" {
		http.Error(w, "Missing required OAuth parameters", http.StatusBadRequest)
		return
	}

	// Retrieve and delete OAuth request atomically to prevent replay attacks
	oauthReq, err := h.sessionStore.GetAndDeleteRequest(state)
	if err != nil {
		log.Printf("Failed to retrieve OAuth request for state %s: %v", state, err)
		http.Error(w, "Invalid or expired authorization request", http.StatusBadRequest)
		return
	}

	// Verify issuer matches
	if iss != oauthReq.AuthServerIss {
		log.Printf("Issuer mismatch: expected %s, got %s", oauthReq.AuthServerIss, iss)
		http.Error(w, "Authorization server mismatch", http.StatusBadRequest)
		return
	}

	// Get OAuth client configuration (supports base64 encoding)
	privateJWK, err := GetEnvBase64OrPlain("OAUTH_PRIVATE_JWK")
	if err != nil {
		log.Printf("Failed to load OAuth private key: %v", err)
		http.Error(w, "OAuth configuration error", http.StatusInternalServerError)
		return
	}
	if privateJWK == "" {
		http.Error(w, "OAuth not configured", http.StatusInternalServerError)
		return
	}

	privateKey, err := oauth.ParseJWKFromJSON([]byte(privateJWK))
	if err != nil {
		log.Printf("Failed to parse OAuth private key: %v", err)
		http.Error(w, "OAuth configuration error", http.StatusInternalServerError)
		return
	}

	appviewURL := getAppViewURL()
	clientID := getClientID(appviewURL)
	redirectURI := appviewURL + "/oauth/callback"

	// Create OAuth client
	client := oauth.NewClient(clientID, privateKey, redirectURI)

	// Parse DPoP key from OAuth request
	dpopKey, err := oauth.ParseJWKFromJSON([]byte(oauthReq.DPoPPrivateJWK))
	if err != nil {
		log.Printf("Failed to parse DPoP key: %v", err)
		http.Error(w, "Failed to restore session key", http.StatusInternalServerError)
		return
	}

	// Exchange authorization code for tokens
	tokenResp, err := client.InitialTokenRequest(
		r.Context(),
		code,
		oauthReq.AuthServerIss,
		oauthReq.PKCEVerifier,
		oauthReq.DPoPAuthServerNonce,
		dpopKey,
	)
	if err != nil {
		log.Printf("Failed to exchange code for tokens: %v", err)
		http.Error(w, "Failed to obtain access tokens", http.StatusInternalServerError)
		return
	}

	// Verify token type is DPoP
	if tokenResp.TokenType != "DPoP" {
		log.Printf("Expected DPoP token type, got: %s", tokenResp.TokenType)
		http.Error(w, "Invalid token type", http.StatusInternalServerError)
		return
	}

	// Verify subject (DID) matches
	if tokenResp.Sub != oauthReq.DID {
		log.Printf("DID mismatch: expected %s, got %s", oauthReq.DID, tokenResp.Sub)
		http.Error(w, "Identity verification failed", http.StatusBadRequest)
		return
	}

	// Calculate token expiration
	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	// Serialize DPoP key for storage
	dpopKeyJSON, err := oauth.JWKToJSON(dpopKey)
	if err != nil {
		log.Printf("Failed to serialize DPoP key: %v", err)
		http.Error(w, "Failed to store session", http.StatusInternalServerError)
		return
	}

	// Save OAuth session to database
	session := &oauthCore.OAuthSession{
		DID:                 oauthReq.DID,
		Handle:              oauthReq.Handle,
		PDSURL:              oauthReq.PDSURL,
		AccessToken:         tokenResp.AccessToken,
		RefreshToken:        tokenResp.RefreshToken,
		DPoPPrivateJWK:      string(dpopKeyJSON),
		DPoPAuthServerNonce: tokenResp.DpopAuthserverNonce,
		DPoPPDSNonce:        "", // Will be populated on first PDS request
		AuthServerIss:       oauthReq.AuthServerIss,
		ExpiresAt:           expiresAt,
	}

	if saveErr := h.sessionStore.SaveSession(session); saveErr != nil {
		log.Printf("Failed to save OAuth session: %v", saveErr)
		http.Error(w, "Failed to save session", http.StatusInternalServerError)
		return
	}

	// Note: OAuth request already deleted atomically in GetAndDeleteRequest above

	// Create HTTP session cookie
	cookieStore := GetCookieStore()
	httpSession, err := cookieStore.Get(r, sessionName)
	if err != nil {
		log.Printf("Failed to get cookie session: %v", err)
		// Try to create a new session anyway
		httpSession, err = cookieStore.New(r, sessionName)
		if err != nil {
			log.Printf("Failed to create new session: %v", err)
			http.Error(w, "Failed to create session", http.StatusInternalServerError)
			return
		}
	}

	httpSession.Values[sessionDID] = oauthReq.DID
	httpSession.Options.MaxAge = SessionMaxAge
	httpSession.Options.HttpOnly = true
	httpSession.Options.Secure = !isDevelopment() // HTTPS only in production
	httpSession.Options.SameSite = http.SameSiteLaxMode

	if err := httpSession.Save(r, w); err != nil {
		log.Printf("Failed to save HTTP session: %v", err)
		http.Error(w, "Failed to create session", http.StatusInternalServerError)
		return
	}

	// Determine redirect URL
	returnURL := oauthReq.ReturnURL
	if returnURL == "" {
		returnURL = "/"
	}

	// Redirect user back to application
	http.Redirect(w, r, returnURL, http.StatusFound)
}

// isDevelopment checks if we're running in development mode
func isDevelopment() bool {
	// Explicitly check for localhost/127.0.0.1 on any port
	appviewURL := os.Getenv("APPVIEW_PUBLIC_URL")
	return appviewURL == "" ||
		strings.HasPrefix(appviewURL, "http://localhost:") ||
		strings.HasPrefix(appviewURL, "http://localhost/") ||
		strings.HasPrefix(appviewURL, "http://127.0.0.1:") ||
		strings.HasPrefix(appviewURL, "http://127.0.0.1/")
}
