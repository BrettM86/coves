package oauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// DPoP (Demonstrating Proof of Possession) - RFC 9449
// Binds access tokens to specific clients using cryptographic proofs

// GenerateDPoPKey generates a new ES256 (NIST P-256) keypair for DPoP
// Each OAuth session should have its own unique DPoP key
func GenerateDPoPKey() (jwk.Key, error) {
	// Generate ES256 private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ECDSA key: %w", err)
	}

	// Convert to JWK
	jwkKey, err := jwk.FromRaw(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create JWK from private key: %w", err)
	}

	// Set JWK parameters
	if err := jwkKey.Set(jwk.AlgorithmKey, jwa.ES256); err != nil {
		return nil, fmt.Errorf("failed to set algorithm: %w", err)
	}
	if err := jwkKey.Set(jwk.KeyUsageKey, "sig"); err != nil {
		return nil, fmt.Errorf("failed to set key usage: %w", err)
	}

	return jwkKey, nil
}

// CreateDPoPProof creates a DPoP proof JWT for HTTP requests
// Parameters:
//   - privateKey: The DPoP private key (ES256) as JWK
//   - method: HTTP method (e.g., "POST", "GET")
//   - uri: Full HTTP URI (e.g., "https://pds.example.com/xrpc/com.atproto.server.getSession")
//   - nonce: Optional server-provided nonce (empty on first request, use nonce from 401 response on retry)
//   - accessToken: Optional access token hash (required when using access token)
func CreateDPoPProof(privateKey jwk.Key, method, uri, nonce, accessToken string) (string, error) {
	// Get public key for JWK thumbprint
	pubKey, err := privateKey.PublicKey()
	if err != nil {
		return "", fmt.Errorf("failed to get public key: %w", err)
	}

	// Create JWT builder
	builder := jwt.NewBuilder().
		Claim("htm", method).            // HTTP method
		Claim("htu", uri).               // HTTP URI
		Claim("iat", time.Now().Unix()). // Issued at
		Claim("jti", generateJTI())      // Unique JWT ID

	// Add nonce if provided (required after first DPoP request)
	if nonce != "" {
		builder = builder.Claim("nonce", nonce)
	}

	// Add access token hash if provided (required when using access token)
	if accessToken != "" {
		ath := hashAccessToken(accessToken)
		builder = builder.Claim("ath", ath)
	}

	// Build the token
	token, err := builder.Build()
	if err != nil {
		return "", fmt.Errorf("failed to build JWT: %w", err)
	}

	// Serialize the token payload to JSON
	payloadBytes, err := json.Marshal(token)
	if err != nil {
		return "", fmt.Errorf("failed to marshal token: %w", err)
	}

	// Create headers with DPoP-specific fields
	// RFC 9449 requires the "jwk" header to contain the public key as a JSON object
	headers := jws.NewHeaders()
	if setErr := headers.Set(jws.AlgorithmKey, jwa.ES256); setErr != nil {
		return "", fmt.Errorf("failed to set algorithm: %w", setErr)
	}
	if setErr := headers.Set(jws.TypeKey, "dpop+jwt"); setErr != nil {
		return "", fmt.Errorf("failed to set type: %w", setErr)
	}
	// Set the public JWK directly - jwx library will handle serialization
	if setErr := headers.Set(jws.JWKKey, pubKey); setErr != nil {
		return "", fmt.Errorf("failed to set JWK: %w", setErr)
	}

	// Sign using jws.Sign to preserve custom headers
	// (jwt.Sign() overrides headers, so we use jws.Sign() directly)
	signed, err := jws.Sign(payloadBytes, jws.WithKey(jwa.ES256, privateKey, jws.WithProtectedHeaders(headers)))
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT: %w", err)
	}

	return string(signed), nil
}

// generateJTI generates a unique JWT ID for DPoP proofs
func generateJTI() string {
	// Generate 16 random bytes
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashAccessToken creates the 'ath' (access token hash) claim
// ath = base64url(SHA-256(access_token))
func hashAccessToken(accessToken string) string {
	hash := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// ParseJWKFromJSON parses a JWK from JSON bytes
func ParseJWKFromJSON(data []byte) (jwk.Key, error) {
	key, err := jwk.ParseKey(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JWK: %w", err)
	}
	return key, nil
}

// JWKToJSON converts a JWK to JSON bytes
func JWKToJSON(key jwk.Key) ([]byte, error) {
	data, err := json.Marshal(key)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JWK: %w", err)
	}
	return data, nil
}

// GetPublicJWKS creates a JWKS (JSON Web Key Set) response for the public key
// This is served at /oauth/jwks.json
func GetPublicJWKS(privateKey jwk.Key) (jwk.Set, error) {
	pubKey, err := privateKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get public key: %w", err)
	}

	// Create JWK Set
	set := jwk.NewSet()
	if err := set.AddKey(pubKey); err != nil {
		return nil, fmt.Errorf("failed to add key to set: %w", err)
	}

	return set, nil
}
