package oauth

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// ClientMetadata represents OAuth 2.0 client metadata (RFC 7591)
// Served at /oauth/client-metadata.json
type ClientMetadata struct {
	ClientID                    string   `json:"client_id"`
	ClientName                  string   `json:"client_name"`
	ClientURI                   string   `json:"client_uri"`
	RedirectURIs                []string `json:"redirect_uris"`
	GrantTypes                  []string `json:"grant_types"`
	ResponseTypes               []string `json:"response_types"`
	Scope                       string   `json:"scope"`
	TokenEndpointAuthMethod     string   `json:"token_endpoint_auth_method"`
	TokenEndpointAuthSigningAlg string   `json:"token_endpoint_auth_signing_alg"`
	DpopBoundAccessTokens       bool     `json:"dpop_bound_access_tokens"`
	ApplicationType             string   `json:"application_type"`
	JwksURI                     string   `json:"jwks_uri,omitempty"` // Only in production
}

// HandleClientMetadata serves the OAuth client metadata
// GET /oauth/client-metadata.json
func HandleClientMetadata(w http.ResponseWriter, r *http.Request) {
	appviewURL := getAppViewURL()

	// Determine client ID based on environment
	clientID := getClientID(appviewURL)
	jwksURI := ""

	// Only include JWKS URI in production (not for loopback clients)
	if !strings.HasPrefix(appviewURL, "http://localhost") && !strings.HasPrefix(appviewURL, "http://127.0.0.1") {
		jwksURI = appviewURL + "/oauth/jwks.json"
	}

	metadata := ClientMetadata{
		ClientID:                    clientID,
		ClientName:                  "Coves",
		ClientURI:                   appviewURL,
		RedirectURIs:                []string{appviewURL + "/oauth/callback"},
		GrantTypes:                  []string{"authorization_code", "refresh_token"},
		ResponseTypes:               []string{"code"},
		Scope:                       "atproto transition:generic",
		TokenEndpointAuthMethod:     "private_key_jwt",
		TokenEndpointAuthSigningAlg: "ES256",
		DpopBoundAccessTokens:       true,
		ApplicationType:             "web",
		JwksURI:                     jwksURI,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(metadata)
}

// getAppViewURL returns the public URL of the AppView
func getAppViewURL() string {
	url := os.Getenv("APPVIEW_PUBLIC_URL")
	if url == "" {
		// Default to localhost for development
		url = "http://localhost:8081"
	}
	return strings.TrimSuffix(url, "/")
}

// getClientID returns the OAuth client ID based on environment
// For localhost development, use loopback client identifier
// For production, use HTTPS URL to client metadata
func getClientID(appviewURL string) string {
	// Development: use loopback client (http://localhost?...)
	if strings.HasPrefix(appviewURL, "http://localhost") || strings.HasPrefix(appviewURL, "http://127.0.0.1") {
		return "http://localhost?redirect_uri=" + appviewURL + "/oauth/callback&scope=atproto%20transition:generic"
	}

	// Production: use HTTPS URL to client metadata
	return appviewURL + "/oauth/client-metadata.json"
}
