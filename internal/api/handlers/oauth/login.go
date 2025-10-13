package oauth

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/oauth"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"

	oauthCore "Coves/internal/core/oauth"
)

// LoginHandler handles OAuth login flow initiation
type LoginHandler struct {
	identityResolver identity.Resolver
	sessionStore     oauthCore.SessionStore
}

// NewLoginHandler creates a new login handler
func NewLoginHandler(identityResolver identity.Resolver, sessionStore oauthCore.SessionStore) *LoginHandler {
	return &LoginHandler{
		identityResolver: identityResolver,
		sessionStore:     sessionStore,
	}
}

// HandleLogin initiates the OAuth login flow
// POST /oauth/login
// Body: { "handle": "alice.bsky.social" }
func (h *LoginHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Handle    string `json:"handle"`
		ReturnURL string `json:"returnUrl,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Normalize handle
	handle := strings.TrimSpace(strings.ToLower(req.Handle))
	handle = strings.TrimPrefix(handle, "@")

	// Validate handle format
	if handle == "" || !strings.Contains(handle, ".") {
		http.Error(w, "Invalid handle format", http.StatusBadRequest)
		return
	}

	// Resolve handle to DID and PDS
	resolved, err := h.identityResolver.Resolve(r.Context(), handle)
	if err != nil {
		log.Printf("Failed to resolve handle %s: %v", handle, err)
		http.Error(w, "Unable to find that account", http.StatusBadRequest)
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

	// Discover auth server from PDS
	pdsURL := resolved.PDSURL
	authServerIss, err := client.ResolvePDSAuthServer(r.Context(), pdsURL)
	if err != nil {
		log.Printf("Failed to resolve auth server for PDS %s: %v", pdsURL, err)
		http.Error(w, "Failed to discover authorization server", http.StatusInternalServerError)
		return
	}

	// Fetch auth server metadata
	authMeta, err := client.FetchAuthServerMetadata(r.Context(), authServerIss)
	if err != nil {
		log.Printf("Failed to fetch auth server metadata: %v", err)
		http.Error(w, "Failed to fetch authorization server metadata", http.StatusInternalServerError)
		return
	}

	// Generate DPoP key for this session
	dpopKey, err := oauth.GenerateDPoPKey()
	if err != nil {
		log.Printf("Failed to generate DPoP key: %v", err)
		http.Error(w, "Failed to generate session key", http.StatusInternalServerError)
		return
	}

	// Send PAR request
	parResp, err := client.SendPARRequest(r.Context(), authMeta, handle, "atproto transition:generic", dpopKey)
	if err != nil {
		log.Printf("Failed to send PAR request: %v", err)
		http.Error(w, "Failed to initiate authorization", http.StatusInternalServerError)
		return
	}

	// Serialize DPoP key to JSON
	dpopKeyJSON, err := oauth.JWKToJSON(dpopKey)
	if err != nil {
		log.Printf("Failed to serialize DPoP key: %v", err)
		http.Error(w, "Failed to store session key", http.StatusInternalServerError)
		return
	}

	// Save OAuth request state to database
	oauthReq := &oauthCore.OAuthRequest{
		State:               parResp.State,
		DID:                 resolved.DID,
		Handle:              handle,
		PDSURL:              pdsURL,
		PKCEVerifier:        parResp.PKCEVerifier,
		DPoPPrivateJWK:      string(dpopKeyJSON),
		DPoPAuthServerNonce: parResp.DpopAuthserverNonce,
		AuthServerIss:       authServerIss,
		ReturnURL:           req.ReturnURL,
	}

	if saveErr := h.sessionStore.SaveRequest(oauthReq); saveErr != nil {
		log.Printf("Failed to save OAuth request: %v", saveErr)
		http.Error(w, "Failed to save authorization state", http.StatusInternalServerError)
		return
	}

	// Build authorization URL
	authURL, err := url.Parse(authMeta.AuthorizationEndpoint)
	if err != nil {
		log.Printf("Invalid authorization endpoint: %v", err)
		http.Error(w, "Invalid authorization endpoint", http.StatusInternalServerError)
		return
	}

	query := authURL.Query()
	query.Set("client_id", clientID)
	query.Set("request_uri", parResp.RequestURI)
	authURL.RawQuery = query.Encode()

	// Return authorization URL to client
	resp := map[string]string{
		"authorizationUrl": authURL.String(),
		"state":            parResp.State,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
