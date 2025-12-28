package aggregators

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	// APIKeyPrefix is the prefix for all Coves API keys
	APIKeyPrefix = "ckapi_"
	// APIKeyRandomBytes is the number of random bytes in the key (32 bytes = 256 bits)
	APIKeyRandomBytes = 32
	// APIKeyTotalLength is the total length of the API key including prefix
	// 6 (prefix "ckapi_") + 64 (32 bytes hex-encoded) = 70
	APIKeyTotalLength = 70
	// TokenRefreshBuffer is how long before expiry we should refresh tokens
	TokenRefreshBuffer = 5 * time.Minute
	// DefaultSessionID is used for API key sessions since aggregators have a single session
	DefaultSessionID = "apikey"
)

// APIKeyService handles API key generation, validation, and OAuth token management
// for aggregator authentication.
type APIKeyService struct {
	repo     Repository
	oauthApp *oauth.ClientApp // For resuming sessions and refreshing tokens

	// failedLastUsedUpdates tracks the number of failed API key last_used timestamp updates.
	// This counter provides visibility into persistent DB issues that would otherwise be hidden
	// since the update is done asynchronously. Use GetFailedLastUsedUpdates() to read.
	failedLastUsedUpdates atomic.Int64

	// failedNonceUpdates tracks the number of failed OAuth nonce updates.
	// Nonce failures may indicate DB issues and could lead to DPoP replay protection issues.
	// Use GetFailedNonceUpdates() to read.
	failedNonceUpdates atomic.Int64
}

// NewAPIKeyService creates a new API key service.
// Panics if repo or oauthApp are nil, as these are required dependencies.
func NewAPIKeyService(repo Repository, oauthApp *oauth.ClientApp) *APIKeyService {
	if repo == nil {
		panic("aggregators.NewAPIKeyService: repo cannot be nil")
	}
	if oauthApp == nil {
		panic("aggregators.NewAPIKeyService: oauthApp cannot be nil")
	}
	return &APIKeyService{
		repo:     repo,
		oauthApp: oauthApp,
	}
}

// GenerateKey creates a new API key for an aggregator.
// The aggregator must have completed OAuth authentication first.
// Returns the plain-text key (only shown once) and the key prefix for reference.
func (s *APIKeyService) GenerateKey(ctx context.Context, aggregatorDID string, oauthSession *oauth.ClientSessionData) (plainKey string, keyPrefix string, err error) {
	// Validate aggregator exists
	aggregator, err := s.repo.GetAggregator(ctx, aggregatorDID)
	if err != nil {
		return "", "", fmt.Errorf("failed to get aggregator: %w", err)
	}

	// Validate OAuth session matches the aggregator
	if oauthSession.AccountDID.String() != aggregatorDID {
		return "", "", ErrOAuthSessionMismatch
	}

	// Generate random key
	randomBytes := make([]byte, APIKeyRandomBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate random key: %w", err)
	}
	randomHex := hex.EncodeToString(randomBytes)
	plainKey = APIKeyPrefix + randomHex

	// Create key prefix (first 12 chars including prefix for identification)
	keyPrefix = plainKey[:12]

	// Hash the key for storage (SHA-256)
	keyHash := hashAPIKey(plainKey)

	// Extract OAuth credentials from session
	// Note: ClientSessionData doesn't store token expiry from the OAuth response.
	// We use a 1-hour default which matches typical OAuth access token lifetimes.
	// Token refresh happens proactively before expiry via RefreshTokensIfNeeded.
	tokenExpiry := time.Now().Add(1 * time.Hour)
	oauthCreds := &OAuthCredentials{
		AccessToken:             oauthSession.AccessToken,
		RefreshToken:            oauthSession.RefreshToken,
		TokenExpiresAt:          tokenExpiry,
		PDSURL:                  oauthSession.HostURL,
		AuthServerIss:           oauthSession.AuthServerURL,
		AuthServerTokenEndpoint: oauthSession.AuthServerTokenEndpoint,
		DPoPPrivateKeyMultibase: oauthSession.DPoPPrivateKeyMultibase,
		DPoPAuthServerNonce:     oauthSession.DPoPAuthServerNonce,
		DPoPPDSNonce:            oauthSession.DPoPHostNonce,
	}

	// Validate OAuth credentials before proceeding
	if err := oauthCreds.Validate(); err != nil {
		return "", "", fmt.Errorf("invalid OAuth credentials: %w", err)
	}

	// Store the OAuth session in the store FIRST (before API key)
	// This prevents a race condition where the API key exists but can't refresh tokens.
	// Order: OAuth session → API key (if session fails, no dangling API key)
	apiKeySession := *oauthSession // Copy session data
	apiKeySession.SessionID = DefaultSessionID
	if err := s.oauthApp.Store.SaveSession(ctx, apiKeySession); err != nil {
		slog.Error("failed to store OAuth session for API key - aborting key creation",
			"did", aggregatorDID,
			"error", err,
		)
		return "", "", fmt.Errorf("failed to store OAuth session for token refresh: %w", err)
	}

	// Now store key hash and OAuth credentials in aggregators table
	// If this fails, we have an orphaned OAuth session, but that's less problematic
	// than having an API key that can't refresh tokens.
	if err := s.repo.SetAPIKey(ctx, aggregatorDID, keyPrefix, keyHash, oauthCreds); err != nil {
		// Best effort cleanup of the OAuth session we just stored
		if deleteErr := s.oauthApp.Store.DeleteSession(ctx, oauthSession.AccountDID, DefaultSessionID); deleteErr != nil {
			slog.Warn("failed to cleanup OAuth session after API key storage failure",
				"did", aggregatorDID,
				"error", deleteErr,
			)
		}
		return "", "", fmt.Errorf("failed to store API key: %w", err)
	}

	slog.Info("API key generated for aggregator",
		"did", aggregatorDID,
		"display_name", aggregator.DisplayName,
		"key_prefix", keyPrefix,
	)

	return plainKey, keyPrefix, nil
}

// ValidateKey validates an API key and returns the associated aggregator credentials.
// Returns ErrAPIKeyInvalid if the key is not found or revoked.
func (s *APIKeyService) ValidateKey(ctx context.Context, plainKey string) (*AggregatorCredentials, error) {
	// Validate key format - log invalid attempts for security monitoring
	if len(plainKey) != APIKeyTotalLength || plainKey[:6] != APIKeyPrefix {
		// Log for security monitoring (potential brute-force detection)
		// Don't log the full key, just metadata about the attempt
		slog.Warn("[SECURITY] invalid API key format attempt",
			"key_length", len(plainKey),
			"has_valid_prefix", len(plainKey) >= 6 && plainKey[:6] == APIKeyPrefix,
		)
		return nil, ErrAPIKeyInvalid
	}

	// Hash the provided key
	keyHash := hashAPIKey(plainKey)

	// Look up aggregator credentials by hash
	creds, err := s.repo.GetCredentialsByAPIKeyHash(ctx, keyHash)
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrAPIKeyInvalid
		}
		// Check for revoked API key (returned by repo when api_key_revoked_at is set)
		if errors.Is(err, ErrAPIKeyRevoked) {
			slog.Warn("revoked API key used",
				"key_hash_prefix", keyHash[:8],
			)
			return nil, ErrAPIKeyRevoked
		}
		return nil, fmt.Errorf("failed to lookup API key: %w", err)
	}

	// Update last used timestamp (async, don't block on error)
	// Use a bounded timeout to prevent goroutine accumulation if DB is slow/down
	// Extract trace info from context before spawning goroutine for log correlation
	aggregatorDID := creds.DID // capture for goroutine
	go func() {
		updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if updateErr := s.repo.UpdateAPIKeyLastUsed(updateCtx, aggregatorDID); updateErr != nil {
			// Increment failure counter for monitoring visibility
			failCount := s.failedLastUsedUpdates.Add(1)
			slog.Error("failed to update API key last used",
				"did", aggregatorDID,
				"error", updateErr,
				"total_failures", failCount,
			)
		}
	}()

	return creds, nil
}

// RefreshTokensIfNeeded checks if the OAuth tokens are expired or expiring soon,
// and refreshes them if necessary.
func (s *APIKeyService) RefreshTokensIfNeeded(ctx context.Context, creds *AggregatorCredentials) error {
	// Check if tokens need refresh
	if creds.OAuthTokenExpiresAt != nil {
		if time.Until(*creds.OAuthTokenExpiresAt) > TokenRefreshBuffer {
			// Tokens still valid
			return nil
		}
	}

	// Need to refresh tokens
	slog.Info("refreshing OAuth tokens for aggregator",
		"did", creds.DID,
		"expires_at", creds.OAuthTokenExpiresAt,
	)

	// Parse DID
	did, err := syntax.ParseDID(creds.DID)
	if err != nil {
		return fmt.Errorf("failed to parse aggregator DID: %w", err)
	}

	// Resume the OAuth session from the store
	// The session was stored when the aggregator created their API key
	session, err := s.oauthApp.ResumeSession(ctx, did, DefaultSessionID)
	if err != nil {
		slog.Error("failed to resume OAuth session for token refresh",
			"did", creds.DID,
			"error", err,
		)
		return fmt.Errorf("failed to resume session: %w", err)
	}

	// Refresh tokens using indigo's OAuth library
	newAccessToken, err := session.RefreshTokens(ctx)
	if err != nil {
		slog.Error("failed to refresh OAuth tokens",
			"did", creds.DID,
			"error", err,
		)
		return fmt.Errorf("failed to refresh tokens: %w", err)
	}

	// Note: ClientSessionData doesn't store token expiry from the OAuth response.
	// We use a 1-hour default which matches typical OAuth access token lifetimes.
	newExpiry := time.Now().Add(1 * time.Hour)

	// Update tokens in database
	if err := s.repo.UpdateOAuthTokens(ctx, creds.DID, newAccessToken, session.Data.RefreshToken, newExpiry); err != nil {
		return fmt.Errorf("failed to update tokens: %w", err)
	}

	// Update nonces in our database as a secondary copy for visibility/backup.
	// The authoritative nonces are in indigo's OAuth store (via SaveSession above).
	// Session resumption uses s.oauthApp.ResumeSession which reads from indigo's store,
	// so this failure is non-critical - hence warning level, not error.
	if err := s.repo.UpdateOAuthNonces(ctx, creds.DID, session.Data.DPoPAuthServerNonce, session.Data.DPoPHostNonce); err != nil {
		failCount := s.failedNonceUpdates.Add(1)
		slog.Warn("failed to update OAuth nonces in aggregators table",
			"did", creds.DID,
			"error", err,
			"total_failures", failCount,
		)
	}

	// Update credentials in memory
	creds.OAuthAccessToken = newAccessToken
	creds.OAuthRefreshToken = session.Data.RefreshToken
	creds.OAuthTokenExpiresAt = &newExpiry
	creds.OAuthDPoPAuthServerNonce = session.Data.DPoPAuthServerNonce
	creds.OAuthDPoPPDSNonce = session.Data.DPoPHostNonce

	slog.Info("OAuth tokens refreshed for aggregator",
		"did", creds.DID,
		"new_expires_at", newExpiry,
	)

	return nil
}

// GetAccessToken returns a valid access token for the aggregator,
// refreshing if necessary.
func (s *APIKeyService) GetAccessToken(ctx context.Context, creds *AggregatorCredentials) (string, error) {
	// Ensure tokens are fresh
	if err := s.RefreshTokensIfNeeded(ctx, creds); err != nil {
		return "", fmt.Errorf("failed to ensure fresh tokens: %w", err)
	}

	return creds.OAuthAccessToken, nil
}

// RevokeKey revokes an API key for an aggregator.
// After revocation, the aggregator must complete OAuth flow again to get a new key.
func (s *APIKeyService) RevokeKey(ctx context.Context, aggregatorDID string) error {
	if err := s.repo.RevokeAPIKey(ctx, aggregatorDID); err != nil {
		return fmt.Errorf("failed to revoke API key: %w", err)
	}

	slog.Info("API key revoked for aggregator",
		"did", aggregatorDID,
	)

	return nil
}

// GetAggregator retrieves the public aggregator information by DID.
// For credential/authentication data, use GetAggregatorCredentials instead.
func (s *APIKeyService) GetAggregator(ctx context.Context, aggregatorDID string) (*Aggregator, error) {
	return s.repo.GetAggregator(ctx, aggregatorDID)
}

// GetAggregatorCredentials retrieves credentials for an aggregator by DID.
func (s *APIKeyService) GetAggregatorCredentials(ctx context.Context, aggregatorDID string) (*AggregatorCredentials, error) {
	return s.repo.GetAggregatorCredentials(ctx, aggregatorDID)
}

// GetAPIKeyInfo returns information about an aggregator's API key (without the actual key).
func (s *APIKeyService) GetAPIKeyInfo(ctx context.Context, aggregatorDID string) (*APIKeyInfo, error) {
	creds, err := s.repo.GetAggregatorCredentials(ctx, aggregatorDID)
	if err != nil {
		return nil, err
	}

	if creds.APIKeyHash == "" {
		return &APIKeyInfo{
			HasKey: false,
		}, nil
	}

	return &APIKeyInfo{
		HasKey:     true,
		KeyPrefix:  creds.APIKeyPrefix,
		CreatedAt:  creds.APIKeyCreatedAt,
		LastUsedAt: creds.APIKeyLastUsed,
		IsRevoked:  creds.APIKeyRevokedAt != nil,
		RevokedAt:  creds.APIKeyRevokedAt,
	}, nil
}

// APIKeyInfo contains non-sensitive information about an API key
type APIKeyInfo struct {
	HasKey     bool
	KeyPrefix  string
	CreatedAt  *time.Time
	LastUsedAt *time.Time
	IsRevoked  bool
	RevokedAt  *time.Time
}

// hashAPIKey creates a SHA-256 hash of the API key for storage
func hashAPIKey(plainKey string) string {
	hash := sha256.Sum256([]byte(plainKey))
	return hex.EncodeToString(hash[:])
}

// GetFailedLastUsedUpdates returns the count of failed API key last_used timestamp updates.
// This is useful for monitoring and alerting on persistent database issues.
func (s *APIKeyService) GetFailedLastUsedUpdates() int64 {
	return s.failedLastUsedUpdates.Load()
}

// GetFailedNonceUpdates returns the count of failed OAuth nonce updates.
// This is useful for monitoring and alerting on persistent database issues
// that could affect DPoP replay protection.
func (s *APIKeyService) GetFailedNonceUpdates() int64 {
	return s.failedNonceUpdates.Load()
}
