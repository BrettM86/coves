package oauth

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generateSealSecret generates a random 32-byte seal secret for testing
func generateSealSecret() []byte {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	return secret
}

func TestSealSession_RoundTrip(t *testing.T) {
	// Create client with seal secret
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	did := "did:plc:abc123"
	sessionID := "session-xyz"
	ttl := 1 * time.Hour

	// Seal the session
	token, err := client.SealSession(did, sessionID, ttl)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// Token should be base64url encoded
	_, err = base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err, "token should be valid base64url")

	// Unseal the session
	session, err := client.UnsealSession(token)
	require.NoError(t, err)
	require.NotNil(t, session)

	// Verify data
	assert.Equal(t, did, session.DID)
	assert.Equal(t, sessionID, session.SessionID)

	// Verify expiration is approximately correct (within 1 second)
	expectedExpiry := time.Now().Add(ttl).Unix()
	assert.InDelta(t, expectedExpiry, session.ExpiresAt, 1.0)
}

func TestSealSession_ExpirationValidation(t *testing.T) {
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	did := "did:plc:abc123"
	sessionID := "session-xyz"
	ttl := 2 * time.Second // Short TTL (must be >= 1 second due to Unix timestamp granularity)

	// Seal the session
	token, err := client.SealSession(did, sessionID, ttl)
	require.NoError(t, err)

	// Should work immediately
	session, err := client.UnsealSession(token)
	require.NoError(t, err)
	assert.Equal(t, did, session.DID)

	// Wait well past expiration
	time.Sleep(2500 * time.Millisecond)

	// Should fail after expiration
	session, err = client.UnsealSession(token)
	assert.Error(t, err)
	assert.Nil(t, session)
	assert.Contains(t, err.Error(), "token expired")
}

func TestSealSession_TamperedTokenDetection(t *testing.T) {
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	did := "did:plc:abc123"
	sessionID := "session-xyz"
	ttl := 1 * time.Hour

	// Seal the session
	token, err := client.SealSession(did, sessionID, ttl)
	require.NoError(t, err)

	// Tamper with the token by modifying one character
	tampered := token[:len(token)-5] + "XXXX" + token[len(token)-1:]

	// Should fail to unseal tampered token
	session, err := client.UnsealSession(tampered)
	assert.Error(t, err)
	assert.Nil(t, session)
	assert.Contains(t, err.Error(), "failed to decrypt token")
}

func TestSealSession_InvalidTokenFormats(t *testing.T) {
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	tests := []struct {
		name  string
		token string
	}{
		{
			name:  "empty token",
			token: "",
		},
		{
			name:  "invalid base64",
			token: "not-valid-base64!@#$",
		},
		{
			name:  "too short",
			token: base64.RawURLEncoding.EncodeToString([]byte("short")),
		},
		{
			name:  "random bytes",
			token: base64.RawURLEncoding.EncodeToString(make([]byte, 50)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session, err := client.UnsealSession(tt.token)
			assert.Error(t, err)
			assert.Nil(t, session)
		})
	}
}

func TestSealSession_DifferentSecrets(t *testing.T) {
	// Create two clients with different secrets
	client1 := &OAuthClient{
		SealSecret: generateSealSecret(),
	}
	client2 := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	did := "did:plc:abc123"
	sessionID := "session-xyz"
	ttl := 1 * time.Hour

	// Seal with client1
	token, err := client1.SealSession(did, sessionID, ttl)
	require.NoError(t, err)

	// Try to unseal with client2 (different secret)
	session, err := client2.UnsealSession(token)
	assert.Error(t, err)
	assert.Nil(t, session)
	assert.Contains(t, err.Error(), "failed to decrypt token")
}

func TestSealSession_NoSecretConfigured(t *testing.T) {
	client := &OAuthClient{
		SealSecret: nil,
	}

	did := "did:plc:abc123"
	sessionID := "session-xyz"
	ttl := 1 * time.Hour

	// Should fail to seal without secret
	token, err := client.SealSession(did, sessionID, ttl)
	assert.Error(t, err)
	assert.Empty(t, token)
	assert.Contains(t, err.Error(), "seal secret not configured")

	// Should fail to unseal without secret
	session, err := client.UnsealSession("dummy-token")
	assert.Error(t, err)
	assert.Nil(t, session)
	assert.Contains(t, err.Error(), "seal secret not configured")
}

func TestSealSession_MissingRequiredFields(t *testing.T) {
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	ttl := 1 * time.Hour

	tests := []struct {
		name      string
		did       string
		sessionID string
		errorMsg  string
	}{
		{
			name:      "missing DID",
			did:       "",
			sessionID: "session-123",
			errorMsg:  "DID is required",
		},
		{
			name:      "missing session ID",
			did:       "did:plc:abc123",
			sessionID: "",
			errorMsg:  "session ID is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := client.SealSession(tt.did, tt.sessionID, ttl)
			assert.Error(t, err)
			assert.Empty(t, token)
			assert.Contains(t, err.Error(), tt.errorMsg)
		})
	}
}

func TestSealSession_UniquenessPerCall(t *testing.T) {
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	did := "did:plc:abc123"
	sessionID := "session-xyz"
	ttl := 1 * time.Hour

	// Seal the same session twice
	token1, err := client.SealSession(did, sessionID, ttl)
	require.NoError(t, err)

	token2, err := client.SealSession(did, sessionID, ttl)
	require.NoError(t, err)

	// Tokens should be different (different nonces)
	assert.NotEqual(t, token1, token2, "tokens should be unique due to different nonces")

	// But both should unseal to the same session data
	session1, err := client.UnsealSession(token1)
	require.NoError(t, err)

	session2, err := client.UnsealSession(token2)
	require.NoError(t, err)

	assert.Equal(t, session1.DID, session2.DID)
	assert.Equal(t, session1.SessionID, session2.SessionID)
}

func TestSealSession_LongDIDAndSessionID(t *testing.T) {
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	// Test with very long DID and session ID
	did := "did:plc:" + strings.Repeat("a", 200)
	sessionID := "session-" + strings.Repeat("x", 200)
	ttl := 1 * time.Hour

	// Should work with long values
	token, err := client.SealSession(did, sessionID, ttl)
	require.NoError(t, err)

	session, err := client.UnsealSession(token)
	require.NoError(t, err)
	assert.Equal(t, did, session.DID)
	assert.Equal(t, sessionID, session.SessionID)
}

func TestSealSession_URLSafeEncoding(t *testing.T) {
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	did := "did:plc:abc123"
	sessionID := "session-xyz"
	ttl := 1 * time.Hour

	// Seal multiple times to get different nonces
	for i := 0; i < 100; i++ {
		token, err := client.SealSession(did, sessionID, ttl)
		require.NoError(t, err)

		// Token should not contain URL-unsafe characters
		assert.NotContains(t, token, "+", "token should not contain '+'")
		assert.NotContains(t, token, "/", "token should not contain '/'")
		assert.NotContains(t, token, "=", "token should not contain '='")

		// Should unseal successfully
		session, err := client.UnsealSession(token)
		require.NoError(t, err)
		assert.Equal(t, did, session.DID)
	}
}

func TestSealSession_ConcurrentAccess(t *testing.T) {
	client := &OAuthClient{
		SealSecret: generateSealSecret(),
	}

	did := "did:plc:abc123"
	sessionID := "session-xyz"
	ttl := 1 * time.Hour

	// Run concurrent seal/unseal operations
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				token, err := client.SealSession(did, sessionID, ttl)
				require.NoError(t, err)

				session, err := client.UnsealSession(token)
				require.NoError(t, err)
				assert.Equal(t, did, session.DID)
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}
