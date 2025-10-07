package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCE (Proof Key for Code Exchange) - RFC 7636
// Prevents authorization code interception attacks

// PKCEChallenge contains the code verifier and challenge for PKCE
type PKCEChallenge struct {
	Verifier  string // Random string (43-128 characters)
	Challenge string // Base64URL(SHA256(verifier))
	Method    string // Always "S256" for atProto
}

// GeneratePKCEChallenge generates a new PKCE code verifier and challenge
// Uses S256 method (SHA-256 hash) as required by atProto OAuth
func GeneratePKCEChallenge() (*PKCEChallenge, error) {
	// Generate 32 random bytes (will be 43 chars when base64url encoded)
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Base64URL encode (no padding)
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	// Create SHA-256 hash of verifier
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	return &PKCEChallenge{
		Verifier:  verifier,
		Challenge: challenge,
		Method:    "S256",
	}, nil
}

// GenerateState generates a random state parameter for CSRF protection
// State is used to prevent CSRF attacks in the OAuth flow
func GenerateState() (string, error) {
	// Generate 32 random bytes
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", fmt.Errorf("failed to generate random state: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(stateBytes), nil
}
