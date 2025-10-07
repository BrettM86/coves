package oauth

import (
	"encoding/json"
	"net/http"

	"Coves/internal/atproto/oauth"

	"github.com/lestrrat-go/jwx/v2/jwk"
)

// HandleJWKS serves the JSON Web Key Set (JWKS) containing the public key
// GET /oauth/jwks.json
func HandleJWKS(w http.ResponseWriter, r *http.Request) {
	// Get private key from environment (supports base64 encoding)
	privateJWK, err := GetEnvBase64OrPlain("OAUTH_PRIVATE_JWK")
	if err != nil {
		http.Error(w, "OAuth configuration error", http.StatusInternalServerError)
		return
	}
	if privateJWK == "" {
		http.Error(w, "OAuth not configured", http.StatusInternalServerError)
		return
	}

	// Parse private key
	privateKey, err := oauth.ParseJWKFromJSON([]byte(privateJWK))
	if err != nil {
		http.Error(w, "Failed to parse private key", http.StatusInternalServerError)
		return
	}

	// Get public key
	publicKey, err := privateKey.PublicKey()
	if err != nil {
		http.Error(w, "Failed to get public key", http.StatusInternalServerError)
		return
	}

	// Create JWKS
	jwks := jwk.NewSet()
	if err := jwks.AddKey(publicKey); err != nil {
		http.Error(w, "Failed to create JWKS", http.StatusInternalServerError)
		return
	}

	// Serve JWKS
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(jwks)
}
