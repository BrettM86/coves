package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// SealedSession represents the data sealed in a mobile session token
type SealedSession struct {
	DID       string `json:"did"` // User's DID
	SessionID string `json:"sid"` // Session identifier
	ExpiresAt int64  `json:"exp"` // Unix timestamp when token expires
}

// SealSession creates an encrypted token containing session information.
// The token is encrypted using AES-256-GCM and encoded as base64url.
//
// Token format: base64url(nonce || ciphertext || tag)
// - nonce: 12 bytes (GCM standard nonce size)
// - ciphertext: encrypted JSON payload
// - tag: 16 bytes (GCM authentication tag)
//
// The sealed token can be safely given to mobile clients and used as
// a reference to the server-side session without exposing sensitive data.
func (c *OAuthClient) SealSession(did, sessionID string, ttl time.Duration) (string, error) {
	if len(c.SealSecret) == 0 {
		return "", fmt.Errorf("seal secret not configured")
	}

	if did == "" {
		return "", fmt.Errorf("DID is required")
	}

	if sessionID == "" {
		return "", fmt.Errorf("session ID is required")
	}

	// Create the session data
	expiresAt := time.Now().Add(ttl).Unix()
	session := SealedSession{
		DID:       did,
		SessionID: sessionID,
		ExpiresAt: expiresAt,
	}

	// Marshal to JSON
	plaintext, err := json.Marshal(session)
	if err != nil {
		return "", fmt.Errorf("failed to marshal session: %w", err)
	}

	// Create AES cipher
	block, err := aes.NewCipher(c.SealSecret)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	// Generate random nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Encrypt and authenticate
	// GCM.Seal appends the ciphertext and tag to the nonce
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)

	// Encode as base64url (no padding)
	token := base64.RawURLEncoding.EncodeToString(ciphertext)

	return token, nil
}

// UnsealSession decrypts and validates a sealed session token.
// Returns the session information if the token is valid and not expired.
func (c *OAuthClient) UnsealSession(token string) (*SealedSession, error) {
	if len(c.SealSecret) == 0 {
		return nil, fmt.Errorf("seal secret not configured")
	}

	if token == "" {
		return nil, fmt.Errorf("token is required")
	}

	// Decode from base64url
	ciphertext, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid token encoding: %w", err)
	}

	// Create AES cipher
	block, err := aes.NewCipher(c.SealSecret)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Verify minimum size (nonce + tag)
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("invalid token: too short")
	}

	// Extract nonce and ciphertext
	nonce := ciphertext[:nonceSize]
	ciphertextData := ciphertext[nonceSize:]

	// Decrypt and authenticate
	plaintext, err := gcm.Open(nil, nonce, ciphertextData, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt token: %w", err)
	}

	// Unmarshal JSON
	var session SealedSession
	if err := json.Unmarshal(plaintext, &session); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	// Validate required fields
	if session.DID == "" {
		return nil, fmt.Errorf("invalid session: missing DID")
	}

	if session.SessionID == "" {
		return nil, fmt.Errorf("invalid session: missing session ID")
	}

	// Check expiration
	now := time.Now().Unix()
	if session.ExpiresAt <= now {
		return nil, fmt.Errorf("token expired at %v", time.Unix(session.ExpiresAt, 0))
	}

	return &session, nil
}
