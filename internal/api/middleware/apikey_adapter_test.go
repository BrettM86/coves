package middleware

import (
	"Coves/internal/core/aggregators"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// minimalMockOAuthStore implements oauth.SessionStore for testing.
// This is a minimal implementation that just returns errors, used for tests
// that don't actually need OAuth functionality.
type minimalMockOAuthStore struct{}

func (m *minimalMockOAuthStore) GetSession(ctx context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error) {
	return nil, errors.New("session not found")
}

func (m *minimalMockOAuthStore) SaveSession(ctx context.Context, sess oauth.ClientSessionData) error {
	return nil
}

func (m *minimalMockOAuthStore) DeleteSession(ctx context.Context, did syntax.DID, sessionID string) error {
	return nil
}

func (m *minimalMockOAuthStore) GetAuthRequestInfo(ctx context.Context, state string) (*oauth.AuthRequestData, error) {
	return nil, errors.New("not found")
}

func (m *minimalMockOAuthStore) SaveAuthRequestInfo(ctx context.Context, info oauth.AuthRequestData) error {
	return nil
}

func (m *minimalMockOAuthStore) DeleteAuthRequestInfo(ctx context.Context, state string) error {
	return nil
}

// newTestAPIKeyService creates an APIKeyService with mock dependencies for testing.
// This helper ensures tests don't panic from nil checks added in constructor validation.
func newTestAPIKeyService(repo aggregators.Repository) *aggregators.APIKeyService {
	mockStore := &minimalMockOAuthStore{}
	mockApp := &oauth.ClientApp{Store: mockStore}
	return aggregators.NewAPIKeyService(repo, mockApp)
}

// mockAPIKeyServiceRepository implements aggregators.Repository for testing
type mockAPIKeyServiceRepository struct {
	getAggregatorFunc              func(ctx context.Context, did string) (*aggregators.Aggregator, error)
	getByAPIKeyHashFunc            func(ctx context.Context, keyHash string) (*aggregators.Aggregator, error)
	getCredentialsByAPIKeyHashFunc func(ctx context.Context, keyHash string) (*aggregators.AggregatorCredentials, error)
	getAggregatorCredentialsFunc   func(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error)
	setAPIKeyFunc                  func(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *aggregators.OAuthCredentials) error
	updateOAuthTokensFunc          func(ctx context.Context, did, accessToken, refreshToken string, expiresAt time.Time) error
	updateOAuthNoncesFunc          func(ctx context.Context, did, authServerNonce, pdsNonce string) error
	updateAPIKeyLastUsedFunc       func(ctx context.Context, did string) error
	revokeAPIKeyFunc               func(ctx context.Context, did string) error
}

func (m *mockAPIKeyServiceRepository) GetAggregator(ctx context.Context, did string) (*aggregators.Aggregator, error) {
	if m.getAggregatorFunc != nil {
		return m.getAggregatorFunc(ctx, did)
	}
	return &aggregators.Aggregator{DID: did, DisplayName: "Test Aggregator"}, nil
}

func (m *mockAPIKeyServiceRepository) GetByAPIKeyHash(ctx context.Context, keyHash string) (*aggregators.Aggregator, error) {
	if m.getByAPIKeyHashFunc != nil {
		return m.getByAPIKeyHashFunc(ctx, keyHash)
	}
	return nil, aggregators.ErrAggregatorNotFound
}

func (m *mockAPIKeyServiceRepository) SetAPIKey(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *aggregators.OAuthCredentials) error {
	if m.setAPIKeyFunc != nil {
		return m.setAPIKeyFunc(ctx, did, keyPrefix, keyHash, oauthCreds)
	}
	return nil
}

func (m *mockAPIKeyServiceRepository) UpdateOAuthTokens(ctx context.Context, did, accessToken, refreshToken string, expiresAt time.Time) error {
	if m.updateOAuthTokensFunc != nil {
		return m.updateOAuthTokensFunc(ctx, did, accessToken, refreshToken, expiresAt)
	}
	return nil
}

func (m *mockAPIKeyServiceRepository) UpdateOAuthNonces(ctx context.Context, did, authServerNonce, pdsNonce string) error {
	if m.updateOAuthNoncesFunc != nil {
		return m.updateOAuthNoncesFunc(ctx, did, authServerNonce, pdsNonce)
	}
	return nil
}

func (m *mockAPIKeyServiceRepository) UpdateAPIKeyLastUsed(ctx context.Context, did string) error {
	if m.updateAPIKeyLastUsedFunc != nil {
		return m.updateAPIKeyLastUsedFunc(ctx, did)
	}
	return nil
}

func (m *mockAPIKeyServiceRepository) RevokeAPIKey(ctx context.Context, did string) error {
	if m.revokeAPIKeyFunc != nil {
		return m.revokeAPIKeyFunc(ctx, did)
	}
	return nil
}

// Stub implementations for Repository interface methods not used in APIKeyService tests
func (m *mockAPIKeyServiceRepository) CreateAggregator(ctx context.Context, aggregator *aggregators.Aggregator) error {
	return nil
}

func (m *mockAPIKeyServiceRepository) GetAggregatorsByDIDs(ctx context.Context, dids []string) ([]*aggregators.Aggregator, error) {
	return nil, nil
}

func (m *mockAPIKeyServiceRepository) UpdateAggregator(ctx context.Context, aggregator *aggregators.Aggregator) error {
	return nil
}

func (m *mockAPIKeyServiceRepository) DeleteAggregator(ctx context.Context, did string) error {
	return nil
}

func (m *mockAPIKeyServiceRepository) ListAggregators(ctx context.Context, limit, offset int) ([]*aggregators.Aggregator, error) {
	return nil, nil
}

func (m *mockAPIKeyServiceRepository) IsAggregator(ctx context.Context, did string) (bool, error) {
	return false, nil
}

func (m *mockAPIKeyServiceRepository) CreateAuthorization(ctx context.Context, auth *aggregators.Authorization) error {
	return nil
}

func (m *mockAPIKeyServiceRepository) GetAuthorization(ctx context.Context, aggregatorDID, communityDID string) (*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAPIKeyServiceRepository) GetAuthorizationByURI(ctx context.Context, recordURI string) (*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAPIKeyServiceRepository) UpdateAuthorization(ctx context.Context, auth *aggregators.Authorization) error {
	return nil
}

func (m *mockAPIKeyServiceRepository) DeleteAuthorization(ctx context.Context, aggregatorDID, communityDID string) error {
	return nil
}

func (m *mockAPIKeyServiceRepository) DeleteAuthorizationByURI(ctx context.Context, recordURI string) error {
	return nil
}

func (m *mockAPIKeyServiceRepository) ListAuthorizationsForAggregator(ctx context.Context, aggregatorDID string, enabledOnly bool, limit, offset int) ([]*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAPIKeyServiceRepository) ListAuthorizationsForCommunity(ctx context.Context, communityDID string, enabledOnly bool, limit, offset int) ([]*aggregators.Authorization, error) {
	return nil, nil
}

func (m *mockAPIKeyServiceRepository) IsAuthorized(ctx context.Context, aggregatorDID, communityDID string) (bool, error) {
	return false, nil
}

func (m *mockAPIKeyServiceRepository) RecordAggregatorPost(ctx context.Context, aggregatorDID, communityDID, postURI, postCID string) error {
	return nil
}

func (m *mockAPIKeyServiceRepository) CountRecentPosts(ctx context.Context, aggregatorDID, communityDID string, since time.Time) (int, error) {
	return 0, nil
}

func (m *mockAPIKeyServiceRepository) GetRecentPosts(ctx context.Context, aggregatorDID, communityDID string, since time.Time) ([]*aggregators.AggregatorPost, error) {
	return nil, nil
}

func (m *mockAPIKeyServiceRepository) GetAggregatorCredentials(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error) {
	if m.getAggregatorCredentialsFunc != nil {
		return m.getAggregatorCredentialsFunc(ctx, did)
	}
	return &aggregators.AggregatorCredentials{DID: did}, nil
}

func (m *mockAPIKeyServiceRepository) GetCredentialsByAPIKeyHash(ctx context.Context, keyHash string) (*aggregators.AggregatorCredentials, error) {
	if m.getCredentialsByAPIKeyHashFunc != nil {
		return m.getCredentialsByAPIKeyHashFunc(ctx, keyHash)
	}
	return nil, aggregators.ErrAggregatorNotFound
}

// =============================================================================
// ValidateKey Delegation Tests
// =============================================================================

func TestAPIKeyValidatorAdapter_ValidateKey_DelegatesToService(t *testing.T) {
	expectedDID := "did:plc:aggregator123"

	repo := &mockAPIKeyServiceRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, keyHash string) (*aggregators.AggregatorCredentials, error) {
			return &aggregators.AggregatorCredentials{
				DID:          expectedDID,
				APIKeyHash:   keyHash,
				APIKeyPrefix: "ckapi_0123",
			}, nil
		},
		updateAPIKeyLastUsedFunc: func(ctx context.Context, did string) error {
			return nil
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	validKey := "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	did, err := adapter.ValidateKey(context.Background(), validKey)
	if err != nil {
		t.Fatalf("ValidateKey() unexpected error: %v", err)
	}

	if did != expectedDID {
		t.Errorf("ValidateKey() = %s, want %s", did, expectedDID)
	}
}

func TestAPIKeyValidatorAdapter_ValidateKey_InvalidKey(t *testing.T) {
	repo := &mockAPIKeyServiceRepository{}
	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	// Test various invalid key formats
	tests := []struct {
		name string
		key  string
	}{
		{"empty key", ""},
		{"too short", "ckapi_short"},
		{"wrong prefix", "wrong_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adapter.ValidateKey(context.Background(), tt.key)
			if err == nil {
				t.Error("ValidateKey() expected error, got nil")
			}
			if !errors.Is(err, aggregators.ErrAPIKeyInvalid) {
				t.Errorf("ValidateKey() error = %v, want %v", err, aggregators.ErrAPIKeyInvalid)
			}
		})
	}
}

func TestAPIKeyValidatorAdapter_ValidateKey_NotFound(t *testing.T) {
	repo := &mockAPIKeyServiceRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, keyHash string) (*aggregators.AggregatorCredentials, error) {
			return nil, aggregators.ErrAggregatorNotFound
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	validKey := "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, err := adapter.ValidateKey(context.Background(), validKey)
	if err == nil {
		t.Error("ValidateKey() expected error, got nil")
	}
	// Should return ErrAPIKeyInvalid when key not found
	if !errors.Is(err, aggregators.ErrAPIKeyInvalid) {
		t.Errorf("ValidateKey() error = %v, want %v", err, aggregators.ErrAPIKeyInvalid)
	}
}

func TestAPIKeyValidatorAdapter_ValidateKey_Revoked(t *testing.T) {
	repo := &mockAPIKeyServiceRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, keyHash string) (*aggregators.AggregatorCredentials, error) {
			return nil, aggregators.ErrAPIKeyRevoked
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	validKey := "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, err := adapter.ValidateKey(context.Background(), validKey)
	if err == nil {
		t.Error("ValidateKey() expected error, got nil")
	}
	if !errors.Is(err, aggregators.ErrAPIKeyRevoked) {
		t.Errorf("ValidateKey() error = %v, want %v", err, aggregators.ErrAPIKeyRevoked)
	}
}

func TestAPIKeyValidatorAdapter_ValidateKey_RepositoryError(t *testing.T) {
	expectedError := errors.New("database connection failed")

	repo := &mockAPIKeyServiceRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, keyHash string) (*aggregators.AggregatorCredentials, error) {
			return nil, expectedError
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	validKey := "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, err := adapter.ValidateKey(context.Background(), validKey)
	if err == nil {
		t.Error("ValidateKey() expected error, got nil")
	}
}

// =============================================================================
// RefreshTokensIfNeeded Delegation Tests
// =============================================================================

func TestAPIKeyValidatorAdapter_RefreshTokensIfNeeded_DelegatesToService(t *testing.T) {
	// Tokens expire in 1 hour - well beyond the 5 minute buffer, so no refresh needed
	expiresAt := time.Now().Add(1 * time.Hour)
	aggregatorDID := "did:plc:aggregator123"

	repo := &mockAPIKeyServiceRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error) {
			return &aggregators.AggregatorCredentials{
				DID:                 did,
				APIKeyHash:          "somehash",
				OAuthTokenExpiresAt: &expiresAt,
			}, nil
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	err := adapter.RefreshTokensIfNeeded(context.Background(), aggregatorDID)
	if err != nil {
		t.Fatalf("RefreshTokensIfNeeded() unexpected error: %v", err)
	}
}

func TestAPIKeyValidatorAdapter_RefreshTokensIfNeeded_AggregatorNotFound(t *testing.T) {
	repo := &mockAPIKeyServiceRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error) {
			return nil, aggregators.ErrAggregatorNotFound
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	err := adapter.RefreshTokensIfNeeded(context.Background(), "did:plc:nonexistent")
	if err == nil {
		t.Error("RefreshTokensIfNeeded() expected error, got nil")
	}
	if !errors.Is(err, aggregators.ErrAggregatorNotFound) {
		t.Errorf("RefreshTokensIfNeeded() error = %v, want %v", err, aggregators.ErrAggregatorNotFound)
	}
}

func TestAPIKeyValidatorAdapter_RefreshTokensIfNeeded_NoAPIKey(t *testing.T) {
	aggregatorDID := "did:plc:aggregator123"

	repo := &mockAPIKeyServiceRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error) {
			return &aggregators.AggregatorCredentials{
				DID:        did,
				APIKeyHash: "", // No API key
			}, nil
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	// Should return ErrAPIKeyInvalid when no API key exists
	err := adapter.RefreshTokensIfNeeded(context.Background(), aggregatorDID)
	if !errors.Is(err, aggregators.ErrAPIKeyInvalid) {
		t.Errorf("RefreshTokensIfNeeded() error = %v, want %v", err, aggregators.ErrAPIKeyInvalid)
	}
}

func TestAPIKeyValidatorAdapter_RefreshTokensIfNeeded_RevokedAPIKey(t *testing.T) {
	aggregatorDID := "did:plc:aggregator123"
	revokedAt := time.Now().Add(-1 * time.Hour)

	repo := &mockAPIKeyServiceRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error) {
			return &aggregators.AggregatorCredentials{
				DID:             did,
				APIKeyHash:      "somehash",
				APIKeyRevokedAt: &revokedAt, // Key is revoked
			}, nil
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	// Should return ErrAPIKeyRevoked when API key is revoked
	err := adapter.RefreshTokensIfNeeded(context.Background(), aggregatorDID)
	if !errors.Is(err, aggregators.ErrAPIKeyRevoked) {
		t.Errorf("RefreshTokensIfNeeded() error = %v, want %v", err, aggregators.ErrAPIKeyRevoked)
	}
}

func TestAPIKeyValidatorAdapter_RefreshTokensIfNeeded_RepositoryError(t *testing.T) {
	expectedError := errors.New("database connection failed")

	repo := &mockAPIKeyServiceRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error) {
			return nil, expectedError
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	err := adapter.RefreshTokensIfNeeded(context.Background(), "did:plc:aggregator123")
	if err == nil {
		t.Error("RefreshTokensIfNeeded() expected error, got nil")
	}
}

// =============================================================================
// GetAPIKeyInfo Delegation Tests (via service)
// =============================================================================

func TestAPIKeyValidatorAdapter_GetAggregator_DelegatesToService(t *testing.T) {
	expectedDID := "did:plc:aggregator123"
	expectedDisplayName := "Test Aggregator"

	repo := &mockAPIKeyServiceRepository{
		getAggregatorFunc: func(ctx context.Context, did string) (*aggregators.Aggregator, error) {
			return &aggregators.Aggregator{
				DID:         expectedDID,
				DisplayName: expectedDisplayName,
			}, nil
		},
	}

	service := newTestAPIKeyService(repo)

	// Test that GetAggregator is properly delegated
	aggregator, err := service.GetAggregator(context.Background(), expectedDID)
	if err != nil {
		t.Fatalf("GetAggregator() unexpected error: %v", err)
	}

	if aggregator.DID != expectedDID {
		t.Errorf("GetAggregator() DID = %s, want %s", aggregator.DID, expectedDID)
	}
	if aggregator.DisplayName != expectedDisplayName {
		t.Errorf("GetAggregator() DisplayName = %s, want %s", aggregator.DisplayName, expectedDisplayName)
	}
}

func TestAPIKeyValidatorAdapter_GetAggregator_NotFound(t *testing.T) {
	repo := &mockAPIKeyServiceRepository{
		getAggregatorFunc: func(ctx context.Context, did string) (*aggregators.Aggregator, error) {
			return nil, aggregators.ErrAggregatorNotFound
		},
	}

	service := newTestAPIKeyService(repo)

	_, err := service.GetAggregator(context.Background(), "did:plc:nonexistent")
	if !errors.Is(err, aggregators.ErrAggregatorNotFound) {
		t.Errorf("GetAggregator() error = %v, want %v", err, aggregators.ErrAggregatorNotFound)
	}
}

func TestAPIKeyValidatorAdapter_GetAggregator_RepositoryError(t *testing.T) {
	expectedError := errors.New("database error")

	repo := &mockAPIKeyServiceRepository{
		getAggregatorFunc: func(ctx context.Context, did string) (*aggregators.Aggregator, error) {
			return nil, expectedError
		},
	}

	service := newTestAPIKeyService(repo)

	_, err := service.GetAggregator(context.Background(), "did:plc:aggregator123")
	if err == nil {
		t.Error("GetAggregator() expected error, got nil")
	}
}

// =============================================================================
// Constructor and nil handling tests
// =============================================================================

func TestNewAPIKeyValidatorAdapter(t *testing.T) {
	repo := &mockAPIKeyServiceRepository{}
	service := newTestAPIKeyService(repo)

	adapter := NewAPIKeyValidatorAdapter(service)
	if adapter == nil {
		t.Fatal("NewAPIKeyValidatorAdapter() returned nil")
	}
}

// =============================================================================
// Integration-style test: Full validation flow
// =============================================================================

func TestAPIKeyValidatorAdapter_FullValidationFlow(t *testing.T) {
	// This test verifies the complete flow:
	// 1. Validate API key
	// 2. Check if tokens need refresh
	// 3. Return aggregator DID

	aggregatorDID := "did:plc:aggregator123"
	expiresAt := time.Now().Add(1 * time.Hour)
	validationCount := 0

	repo := &mockAPIKeyServiceRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, keyHash string) (*aggregators.AggregatorCredentials, error) {
			validationCount++
			return &aggregators.AggregatorCredentials{
				DID:                 aggregatorDID,
				APIKeyHash:          keyHash,
				APIKeyPrefix:        "ckapi_0123",
				OAuthTokenExpiresAt: &expiresAt,
			}, nil
		},
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error) {
			return &aggregators.AggregatorCredentials{
				DID:                 did,
				APIKeyHash:          "somehash",
				OAuthTokenExpiresAt: &expiresAt,
			}, nil
		},
		updateAPIKeyLastUsedFunc: func(ctx context.Context, did string) error {
			return nil
		},
	}

	service := newTestAPIKeyService(repo)
	adapter := NewAPIKeyValidatorAdapter(service)

	// Step 1: Validate the key
	validKey := "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	did, err := adapter.ValidateKey(context.Background(), validKey)
	if err != nil {
		t.Fatalf("ValidateKey() unexpected error: %v", err)
	}
	if did != aggregatorDID {
		t.Errorf("ValidateKey() DID = %s, want %s", did, aggregatorDID)
	}

	// Step 2: Check/refresh tokens (should succeed without refresh since tokens are valid)
	err = adapter.RefreshTokensIfNeeded(context.Background(), did)
	if err != nil {
		t.Errorf("RefreshTokensIfNeeded() unexpected error: %v", err)
	}

	// Verify validation was called
	if validationCount != 1 {
		t.Errorf("Expected 1 validation call, got %d", validationCount)
	}
}

// Ensure we don't have unused import
var _ = oauth.ClientApp{}
