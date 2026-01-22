package aggregators

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// ptrTime returns a pointer to a time.Time (current time)
func ptrTime() *time.Time {
	t := time.Now()
	return &t
}

// ptrTimeOffset returns a pointer to a time.Time offset from now
func ptrTimeOffset(d time.Duration) *time.Time {
	t := time.Now().Add(d)
	return &t
}

// newTestAPIKeyService creates an APIKeyService with mock dependencies for testing.
// This helper ensures tests don't panic from nil checks added in constructor validation.
func newTestAPIKeyService(repo Repository) *APIKeyService {
	mockStore := &mockOAuthStore{}
	mockApp := &oauth.ClientApp{Store: mockStore}
	return NewAPIKeyService(repo, mockApp)
}

// mockRepository implements Repository interface for testing
type mockRepository struct {
	getAggregatorFunc                      func(ctx context.Context, did string) (*Aggregator, error)
	getByAPIKeyHashFunc                    func(ctx context.Context, keyHash string) (*Aggregator, error)
	getCredentialsByAPIKeyHashFunc         func(ctx context.Context, keyHash string) (*AggregatorCredentials, error)
	getAggregatorCredentialsFunc           func(ctx context.Context, did string) (*AggregatorCredentials, error)
	setAPIKeyFunc                          func(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *OAuthCredentials) error
	updateOAuthTokensFunc                  func(ctx context.Context, did, accessToken, refreshToken string, expiresAt time.Time) error
	updateOAuthNoncesFunc                  func(ctx context.Context, did, authServerNonce, pdsNonce string) error
	updateAPIKeyLastUsedFunc               func(ctx context.Context, did string) error
	revokeAPIKeyFunc                       func(ctx context.Context, did string) error
	listAggregatorsNeedingTokenRefreshFunc func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error)
}

func (m *mockRepository) GetAggregator(ctx context.Context, did string) (*Aggregator, error) {
	if m.getAggregatorFunc != nil {
		return m.getAggregatorFunc(ctx, did)
	}
	return &Aggregator{DID: did, DisplayName: "Test Aggregator"}, nil
}

func (m *mockRepository) GetByAPIKeyHash(ctx context.Context, keyHash string) (*Aggregator, error) {
	if m.getByAPIKeyHashFunc != nil {
		return m.getByAPIKeyHashFunc(ctx, keyHash)
	}
	return nil, ErrAggregatorNotFound
}

func (m *mockRepository) SetAPIKey(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *OAuthCredentials) error {
	if m.setAPIKeyFunc != nil {
		return m.setAPIKeyFunc(ctx, did, keyPrefix, keyHash, oauthCreds)
	}
	return nil
}

func (m *mockRepository) UpdateOAuthTokens(ctx context.Context, did, accessToken, refreshToken string, expiresAt time.Time) error {
	if m.updateOAuthTokensFunc != nil {
		return m.updateOAuthTokensFunc(ctx, did, accessToken, refreshToken, expiresAt)
	}
	return nil
}

func (m *mockRepository) UpdateOAuthNonces(ctx context.Context, did, authServerNonce, pdsNonce string) error {
	if m.updateOAuthNoncesFunc != nil {
		return m.updateOAuthNoncesFunc(ctx, did, authServerNonce, pdsNonce)
	}
	return nil
}

func (m *mockRepository) UpdateAPIKeyLastUsed(ctx context.Context, did string) error {
	if m.updateAPIKeyLastUsedFunc != nil {
		return m.updateAPIKeyLastUsedFunc(ctx, did)
	}
	return nil
}

func (m *mockRepository) RevokeAPIKey(ctx context.Context, did string) error {
	if m.revokeAPIKeyFunc != nil {
		return m.revokeAPIKeyFunc(ctx, did)
	}
	return nil
}

// Stub implementations for Repository interface methods not used in APIKeyService tests
func (m *mockRepository) CreateAggregator(ctx context.Context, aggregator *Aggregator) error {
	return nil
}

func (m *mockRepository) GetAggregatorsByDIDs(ctx context.Context, dids []string) ([]*Aggregator, error) {
	return nil, nil
}

func (m *mockRepository) UpdateAggregator(ctx context.Context, aggregator *Aggregator) error {
	return nil
}

func (m *mockRepository) DeleteAggregator(ctx context.Context, did string) error {
	return nil
}

func (m *mockRepository) ListAggregators(ctx context.Context, limit, offset int) ([]*Aggregator, error) {
	return nil, nil
}

func (m *mockRepository) IsAggregator(ctx context.Context, did string) (bool, error) {
	return false, nil
}

func (m *mockRepository) CreateAuthorization(ctx context.Context, auth *Authorization) error {
	return nil
}

func (m *mockRepository) GetAuthorization(ctx context.Context, aggregatorDID, communityDID string) (*Authorization, error) {
	return nil, nil
}

func (m *mockRepository) GetAuthorizationByURI(ctx context.Context, recordURI string) (*Authorization, error) {
	return nil, nil
}

func (m *mockRepository) UpdateAuthorization(ctx context.Context, auth *Authorization) error {
	return nil
}

func (m *mockRepository) DeleteAuthorization(ctx context.Context, aggregatorDID, communityDID string) error {
	return nil
}

func (m *mockRepository) DeleteAuthorizationByURI(ctx context.Context, recordURI string) error {
	return nil
}

func (m *mockRepository) ListAuthorizationsForAggregator(ctx context.Context, aggregatorDID string, enabledOnly bool, limit, offset int) ([]*Authorization, error) {
	return nil, nil
}

func (m *mockRepository) ListAuthorizationsForCommunity(ctx context.Context, communityDID string, enabledOnly bool, limit, offset int) ([]*Authorization, error) {
	return nil, nil
}

func (m *mockRepository) IsAuthorized(ctx context.Context, aggregatorDID, communityDID string) (bool, error) {
	return false, nil
}

func (m *mockRepository) RecordAggregatorPost(ctx context.Context, aggregatorDID, communityDID, postURI, postCID string) error {
	return nil
}

func (m *mockRepository) CountRecentPosts(ctx context.Context, aggregatorDID, communityDID string, since time.Time) (int, error) {
	return 0, nil
}

func (m *mockRepository) GetRecentPosts(ctx context.Context, aggregatorDID, communityDID string, since time.Time) ([]*AggregatorPost, error) {
	return nil, nil
}

func (m *mockRepository) GetAggregatorCredentials(ctx context.Context, did string) (*AggregatorCredentials, error) {
	if m.getAggregatorCredentialsFunc != nil {
		return m.getAggregatorCredentialsFunc(ctx, did)
	}
	return &AggregatorCredentials{DID: did}, nil
}

func (m *mockRepository) GetCredentialsByAPIKeyHash(ctx context.Context, keyHash string) (*AggregatorCredentials, error) {
	if m.getCredentialsByAPIKeyHashFunc != nil {
		return m.getCredentialsByAPIKeyHashFunc(ctx, keyHash)
	}
	return nil, ErrAggregatorNotFound
}

func (m *mockRepository) ListAggregatorsNeedingTokenRefresh(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
	if m.listAggregatorsNeedingTokenRefreshFunc != nil {
		return m.listAggregatorsNeedingTokenRefreshFunc(ctx, expiryBuffer)
	}
	return nil, nil
}

func TestHashAPIKey(t *testing.T) {
	plainKey := "ckapi_abcdef1234567890abcdef1234567890"

	// Hash the key
	hash := hashAPIKey(plainKey)

	// Verify it's a valid hex string
	if len(hash) != 64 {
		t.Errorf("Expected 64 character hash, got %d", len(hash))
	}

	// Verify it's consistent
	hash2 := hashAPIKey(plainKey)
	if hash != hash2 {
		t.Error("Hash function should be deterministic")
	}

	// Verify different keys produce different hashes
	differentKey := "ckapi_different1234567890abcdef12"
	differentHash := hashAPIKey(differentKey)
	if hash == differentHash {
		t.Error("Different keys should produce different hashes")
	}

	// Verify manually
	expectedHash := sha256.Sum256([]byte(plainKey))
	expectedHex := hex.EncodeToString(expectedHash[:])
	if hash != expectedHex {
		t.Errorf("Expected %s, got %s", expectedHex, hash)
	}
}

func TestAPIKeyConstants(t *testing.T) {
	// Verify the key prefix length assumption
	if len(APIKeyPrefix) != 6 {
		t.Errorf("Expected APIKeyPrefix to be 6 chars, got %d", len(APIKeyPrefix))
	}

	// Verify total length calculation
	// Random bytes are hex-encoded, so they double in length (32 bytes -> 64 chars)
	expectedLength := len(APIKeyPrefix) + (APIKeyRandomBytes * 2)
	if APIKeyTotalLength != expectedLength {
		t.Errorf("APIKeyTotalLength should be %d (prefix + hex-encoded random), got %d", expectedLength, APIKeyTotalLength)
	}

	// Verify expected values explicitly
	if APIKeyTotalLength != 70 {
		t.Errorf("APIKeyTotalLength should be 70 (6 prefix + 64 hex chars), got %d", APIKeyTotalLength)
	}
}

func TestValidateKey_FormatValidation(t *testing.T) {
	// We can't test the full ValidateKey without mocking, but we can verify
	// the format validation logic by checking the constants
	// 32 random bytes hex-encoded = 64 characters
	testKey := "ckapi_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if len(testKey) != APIKeyTotalLength {
		t.Errorf("Test key length mismatch: expected %d, got %d", APIKeyTotalLength, len(testKey))
	}

	// Test key should start with prefix
	if testKey[:6] != APIKeyPrefix {
		t.Errorf("Test key should start with %s", APIKeyPrefix)
	}

	// Verify key length is 70 characters
	if len(testKey) != 70 {
		t.Errorf("Test key should be 70 characters, got %d", len(testKey))
	}
}

// =============================================================================
// AggregatorCredentials Tests
// =============================================================================

func TestAggregatorCredentials_HasActiveAPIKey(t *testing.T) {
	tests := []struct {
		name       string
		creds      AggregatorCredentials
		wantActive bool
	}{
		{
			name:       "no key hash",
			creds:      AggregatorCredentials{},
			wantActive: false,
		},
		{
			name:       "has key hash, not revoked",
			creds:      AggregatorCredentials{APIKeyHash: "somehash"},
			wantActive: true,
		},
		{
			name: "has key hash, revoked",
			creds: AggregatorCredentials{
				APIKeyHash:      "somehash",
				APIKeyRevokedAt: ptrTime(),
			},
			wantActive: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.creds.HasActiveAPIKey()
			if got != tt.wantActive {
				t.Errorf("HasActiveAPIKey() = %v, want %v", got, tt.wantActive)
			}
		})
	}
}

func TestAggregatorCredentials_IsOAuthTokenExpired(t *testing.T) {
	tests := []struct {
		name        string
		creds       AggregatorCredentials
		wantExpired bool
	}{
		{
			name:        "nil expiry",
			creds:       AggregatorCredentials{},
			wantExpired: true,
		},
		{
			name: "expired in the past",
			creds: AggregatorCredentials{
				OAuthTokenExpiresAt: ptrTimeOffset(-1 * time.Hour),
			},
			wantExpired: true,
		},
		{
			name: "within 5 minute buffer (4 minutes remaining)",
			creds: AggregatorCredentials{
				OAuthTokenExpiresAt: ptrTimeOffset(4 * time.Minute),
			},
			wantExpired: true, // Should be expired because within buffer
		},
		{
			name: "exactly at 5 minute buffer",
			creds: AggregatorCredentials{
				OAuthTokenExpiresAt: ptrTimeOffset(5 * time.Minute),
			},
			wantExpired: true, // Edge case - at exactly buffer time
		},
		{
			name: "beyond 5 minute buffer (6 minutes remaining)",
			creds: AggregatorCredentials{
				OAuthTokenExpiresAt: ptrTimeOffset(6 * time.Minute),
			},
			wantExpired: false, // Should not be expired
		},
		{
			name: "well beyond buffer (1 hour remaining)",
			creds: AggregatorCredentials{
				OAuthTokenExpiresAt: ptrTimeOffset(1 * time.Hour),
			},
			wantExpired: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.creds.IsOAuthTokenExpired()
			if got != tt.wantExpired {
				t.Errorf("IsOAuthTokenExpired() = %v, want %v", got, tt.wantExpired)
			}
		})
	}
}

// =============================================================================
// ValidateKey Tests
// =============================================================================

func TestAPIKeyService_ValidateKey_InvalidFormat(t *testing.T) {
	repo := &mockRepository{}
	service := newTestAPIKeyService(repo)

	tests := []struct {
		name    string
		key     string
		wantErr error
	}{
		{
			name:    "empty key",
			key:     "",
			wantErr: ErrAPIKeyInvalid,
		},
		{
			name:    "too short",
			key:     "ckapi_short",
			wantErr: ErrAPIKeyInvalid,
		},
		{
			name:    "wrong prefix",
			key:     "wrong_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			wantErr: ErrAPIKeyInvalid,
		},
		{
			name:    "correct length but wrong prefix",
			key:     "badpfx0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd",
			wantErr: ErrAPIKeyInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := service.ValidateKey(context.Background(), tt.key)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidateKey() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAPIKeyService_ValidateKey_NotFound(t *testing.T) {
	repo := &mockRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, keyHash string) (*AggregatorCredentials, error) {
			return nil, ErrAggregatorNotFound
		},
	}
	service := newTestAPIKeyService(repo)

	// Valid format but key not in database
	validKey := "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, err := service.ValidateKey(context.Background(), validKey)
	if !errors.Is(err, ErrAPIKeyInvalid) {
		t.Errorf("ValidateKey() error = %v, want %v", err, ErrAPIKeyInvalid)
	}
}

func TestAPIKeyService_ValidateKey_Revoked(t *testing.T) {
	// The current implementation expects the repository to return ErrAPIKeyRevoked
	// when the API key has been revoked. This is done at the repository layer.
	repo := &mockRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, keyHash string) (*AggregatorCredentials, error) {
			// Repository returns error for revoked keys
			return nil, ErrAPIKeyRevoked
		},
	}
	service := newTestAPIKeyService(repo)

	validKey := "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, err := service.ValidateKey(context.Background(), validKey)
	if !errors.Is(err, ErrAPIKeyRevoked) {
		t.Errorf("ValidateKey() error = %v, want %v", err, ErrAPIKeyRevoked)
	}
}

func TestAPIKeyService_ValidateKey_Success(t *testing.T) {
	expectedDID := "did:plc:aggregator123"
	lastUsedChan := make(chan struct{})

	repo := &mockRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, keyHash string) (*AggregatorCredentials, error) {
			return &AggregatorCredentials{
				DID:          expectedDID,
				APIKeyHash:   keyHash,
				APIKeyPrefix: "ckapi_0123",
			}, nil
		},
		updateAPIKeyLastUsedFunc: func(ctx context.Context, did string) error {
			close(lastUsedChan)
			return nil
		},
	}
	service := newTestAPIKeyService(repo)

	validKey := "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	creds, err := service.ValidateKey(context.Background(), validKey)
	if err != nil {
		t.Fatalf("ValidateKey() unexpected error: %v", err)
	}

	if creds.DID != expectedDID {
		t.Errorf("ValidateKey() DID = %s, want %s", creds.DID, expectedDID)
	}

	// Wait for async update with timeout using channel-based synchronization
	select {
	case <-lastUsedChan:
		// Success - UpdateAPIKeyLastUsed was called
	case <-time.After(1 * time.Second):
		t.Error("Expected UpdateAPIKeyLastUsed to be called (timeout)")
	}
}

// =============================================================================
// GenerateKey Tests
// =============================================================================

func TestAPIKeyService_GenerateKey_AggregatorNotFound(t *testing.T) {
	repo := &mockRepository{
		getAggregatorFunc: func(ctx context.Context, did string) (*Aggregator, error) {
			return nil, ErrAggregatorNotFound
		},
	}
	service := newTestAPIKeyService(repo)

	did, _ := syntax.ParseDID("did:plc:test123")
	session := &oauth.ClientSessionData{
		AccountDID:  did,
		AccessToken: "test_token",
	}

	_, _, err := service.GenerateKey(context.Background(), "did:plc:test123", session)
	if err == nil {
		t.Error("GenerateKey() expected error, got nil")
	}
}

func TestAPIKeyService_GenerateKey_DIDMismatch(t *testing.T) {
	repo := &mockRepository{
		getAggregatorFunc: func(ctx context.Context, did string) (*Aggregator, error) {
			return &Aggregator{DID: did}, nil
		},
	}
	service := newTestAPIKeyService(repo)

	// Session DID doesn't match requested aggregator DID
	sessionDID, _ := syntax.ParseDID("did:plc:different")
	session := &oauth.ClientSessionData{
		AccountDID:  sessionDID,
		AccessToken: "test_token",
	}

	_, _, err := service.GenerateKey(context.Background(), "did:plc:aggregator123", session)
	if err == nil {
		t.Error("GenerateKey() expected DID mismatch error, got nil")
	}
	if !errors.Is(err, nil) && err.Error() == "" {
		// Just check there's an error for DID mismatch
	}
}

func TestAPIKeyService_GenerateKey_SetAPIKeyError(t *testing.T) {
	expectedError := errors.New("database error")
	repo := &mockRepository{
		getAggregatorFunc: func(ctx context.Context, did string) (*Aggregator, error) {
			return &Aggregator{DID: did, DisplayName: "Test"}, nil
		},
		setAPIKeyFunc: func(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *OAuthCredentials) error {
			return expectedError
		},
	}

	// Create a minimal mock OAuth store
	mockStore := &mockOAuthStore{}
	mockApp := &oauth.ClientApp{Store: mockStore}

	service := NewAPIKeyService(repo, mockApp)

	did, _ := syntax.ParseDID("did:plc:aggregator123")
	session := &oauth.ClientSessionData{
		AccountDID:  did,
		AccessToken: "test_token",
	}

	_, _, err := service.GenerateKey(context.Background(), "did:plc:aggregator123", session)
	if err == nil {
		t.Error("GenerateKey() expected error, got nil")
	}
}

func TestAPIKeyService_GenerateKey_Success(t *testing.T) {
	aggregatorDID := "did:plc:aggregator123"
	var storedKeyPrefix, storedKeyHash string
	var storedOAuthCreds *OAuthCredentials
	var savedSession *oauth.ClientSessionData

	repo := &mockRepository{
		getAggregatorFunc: func(ctx context.Context, did string) (*Aggregator, error) {
			if did != aggregatorDID {
				return nil, ErrAggregatorNotFound
			}
			return &Aggregator{
				DID:         did,
				DisplayName: "Test Aggregator",
			}, nil
		},
		setAPIKeyFunc: func(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *OAuthCredentials) error {
			storedKeyPrefix = keyPrefix
			storedKeyHash = keyHash
			storedOAuthCreds = oauthCreds
			return nil
		},
	}

	// Create mock OAuth store that tracks saved sessions
	mockStore := &mockOAuthStore{
		saveSessionFunc: func(ctx context.Context, session oauth.ClientSessionData) error {
			savedSession = &session
			return nil
		},
	}
	mockApp := &oauth.ClientApp{Store: mockStore}

	service := NewAPIKeyService(repo, mockApp)

	// Create OAuth session
	did, _ := syntax.ParseDID(aggregatorDID)
	session := &oauth.ClientSessionData{
		AccountDID:              did,
		SessionID:               "original_session",
		AccessToken:             "test_access_token",
		RefreshToken:            "test_refresh_token",
		HostURL:                 "https://pds.example.com",
		AuthServerURL:           "https://auth.example.com",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		DPoPPrivateKeyMultibase: "z1234567890",
		DPoPAuthServerNonce:     "auth_nonce_123",
		DPoPHostNonce:           "host_nonce_456",
	}

	plainKey, keyPrefix, err := service.GenerateKey(context.Background(), aggregatorDID, session)
	if err != nil {
		t.Fatalf("GenerateKey() unexpected error: %v", err)
	}

	// Verify key format
	if len(plainKey) != APIKeyTotalLength {
		t.Errorf("GenerateKey() plainKey length = %d, want %d", len(plainKey), APIKeyTotalLength)
	}
	if plainKey[:6] != APIKeyPrefix {
		t.Errorf("GenerateKey() plainKey prefix = %s, want %s", plainKey[:6], APIKeyPrefix)
	}

	// Verify key prefix is first 12 chars
	if keyPrefix != plainKey[:12] {
		t.Errorf("GenerateKey() keyPrefix = %s, want %s", keyPrefix, plainKey[:12])
	}

	// Verify hash was stored (SHA-256 produces 64 hex chars)
	if len(storedKeyHash) != 64 {
		t.Errorf("GenerateKey() stored hash length = %d, want 64", len(storedKeyHash))
	}

	// Verify hash matches the key
	expectedHash := hashAPIKey(plainKey)
	if storedKeyHash != expectedHash {
		t.Errorf("GenerateKey() stored hash doesn't match key hash")
	}

	// Verify stored key prefix matches returned prefix
	if storedKeyPrefix != keyPrefix {
		t.Errorf("GenerateKey() stored keyPrefix = %s, want %s", storedKeyPrefix, keyPrefix)
	}

	// Verify OAuth credentials were saved
	if storedOAuthCreds == nil {
		t.Fatal("GenerateKey() OAuth credentials not stored")
	}
	if storedOAuthCreds.AccessToken != session.AccessToken {
		t.Errorf("GenerateKey() stored AccessToken = %s, want %s", storedOAuthCreds.AccessToken, session.AccessToken)
	}
	if storedOAuthCreds.RefreshToken != session.RefreshToken {
		t.Errorf("GenerateKey() stored RefreshToken = %s, want %s", storedOAuthCreds.RefreshToken, session.RefreshToken)
	}
	if storedOAuthCreds.PDSURL != session.HostURL {
		t.Errorf("GenerateKey() stored PDSURL = %s, want %s", storedOAuthCreds.PDSURL, session.HostURL)
	}
	if storedOAuthCreds.AuthServerIss != session.AuthServerURL {
		t.Errorf("GenerateKey() stored AuthServerIss = %s, want %s", storedOAuthCreds.AuthServerIss, session.AuthServerURL)
	}
	if storedOAuthCreds.DPoPPrivateKeyMultibase != session.DPoPPrivateKeyMultibase {
		t.Errorf("GenerateKey() stored DPoPPrivateKeyMultibase mismatch")
	}
	if storedOAuthCreds.DPoPAuthServerNonce != session.DPoPAuthServerNonce {
		t.Errorf("GenerateKey() stored DPoPAuthServerNonce = %s, want %s", storedOAuthCreds.DPoPAuthServerNonce, session.DPoPAuthServerNonce)
	}
	if storedOAuthCreds.DPoPPDSNonce != session.DPoPHostNonce {
		t.Errorf("GenerateKey() stored DPoPPDSNonce = %s, want %s", storedOAuthCreds.DPoPPDSNonce, session.DPoPHostNonce)
	}

	// Verify session was saved to OAuth store
	if savedSession == nil {
		t.Fatal("GenerateKey() session not saved to OAuth store")
	}
	if savedSession.SessionID != DefaultSessionID {
		t.Errorf("GenerateKey() saved session ID = %s, want %s", savedSession.SessionID, DefaultSessionID)
	}
	if savedSession.AccessToken != session.AccessToken {
		t.Errorf("GenerateKey() saved session AccessToken mismatch")
	}
}

func TestAPIKeyService_GenerateKey_OAuthStoreSaveError(t *testing.T) {
	// Test that OAuth session save failure aborts key creation early
	// With the new ordering (OAuth session first, then API key), if OAuth save fails,
	// we abort immediately without creating an API key.
	aggregatorDID := "did:plc:aggregator123"
	setAPIKeyCalled := false

	repo := &mockRepository{
		getAggregatorFunc: func(ctx context.Context, did string) (*Aggregator, error) {
			return &Aggregator{DID: did, DisplayName: "Test"}, nil
		},
		setAPIKeyFunc: func(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *OAuthCredentials) error {
			setAPIKeyCalled = true
			return nil
		},
	}

	// Create mock OAuth store that fails on save
	mockStore := &mockOAuthStore{
		saveSessionFunc: func(ctx context.Context, session oauth.ClientSessionData) error {
			return errors.New("failed to save session")
		},
	}
	mockApp := &oauth.ClientApp{Store: mockStore}

	service := NewAPIKeyService(repo, mockApp)

	did, _ := syntax.ParseDID(aggregatorDID)
	session := &oauth.ClientSessionData{
		AccountDID:  did,
		AccessToken: "test_token",
	}

	_, _, err := service.GenerateKey(context.Background(), aggregatorDID, session)
	if err == nil {
		t.Error("GenerateKey() expected error when OAuth store save fails, got nil")
	}

	// Verify SetAPIKey was NOT called - we should abort before storing the key
	// This prevents the race condition where an API key exists but can't refresh tokens
	if setAPIKeyCalled {
		t.Error("GenerateKey() should NOT call SetAPIKey when OAuth session save fails")
	}
}

// mockOAuthStore implements oauth.ClientAuthStore for testing
type mockOAuthStore struct {
	getSessionFunc            func(ctx context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error)
	saveSessionFunc           func(ctx context.Context, session oauth.ClientSessionData) error
	deleteSessionFunc         func(ctx context.Context, did syntax.DID, sessionID string) error
	getAuthRequestInfoFunc    func(ctx context.Context, state string) (*oauth.AuthRequestData, error)
	saveAuthRequestInfoFunc   func(ctx context.Context, info oauth.AuthRequestData) error
	deleteAuthRequestInfoFunc func(ctx context.Context, state string) error
}

func (m *mockOAuthStore) GetSession(ctx context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error) {
	if m.getSessionFunc != nil {
		return m.getSessionFunc(ctx, did, sessionID)
	}
	return nil, errors.New("session not found")
}

func (m *mockOAuthStore) SaveSession(ctx context.Context, session oauth.ClientSessionData) error {
	if m.saveSessionFunc != nil {
		return m.saveSessionFunc(ctx, session)
	}
	return nil
}

func (m *mockOAuthStore) DeleteSession(ctx context.Context, did syntax.DID, sessionID string) error {
	if m.deleteSessionFunc != nil {
		return m.deleteSessionFunc(ctx, did, sessionID)
	}
	return nil
}

func (m *mockOAuthStore) GetAuthRequestInfo(ctx context.Context, state string) (*oauth.AuthRequestData, error) {
	if m.getAuthRequestInfoFunc != nil {
		return m.getAuthRequestInfoFunc(ctx, state)
	}
	return nil, errors.New("not found")
}

func (m *mockOAuthStore) SaveAuthRequestInfo(ctx context.Context, info oauth.AuthRequestData) error {
	if m.saveAuthRequestInfoFunc != nil {
		return m.saveAuthRequestInfoFunc(ctx, info)
	}
	return nil
}

func (m *mockOAuthStore) DeleteAuthRequestInfo(ctx context.Context, state string) error {
	if m.deleteAuthRequestInfoFunc != nil {
		return m.deleteAuthRequestInfoFunc(ctx, state)
	}
	return nil
}

// =============================================================================
// RevokeKey Tests
// =============================================================================

func TestAPIKeyService_RevokeKey_Success(t *testing.T) {
	revokeCalled := false
	revokedDID := ""

	repo := &mockRepository{
		revokeAPIKeyFunc: func(ctx context.Context, did string) error {
			revokeCalled = true
			revokedDID = did
			return nil
		},
	}
	service := newTestAPIKeyService(repo)

	err := service.RevokeKey(context.Background(), "did:plc:aggregator123")
	if err != nil {
		t.Fatalf("RevokeKey() unexpected error: %v", err)
	}

	if !revokeCalled {
		t.Error("Expected RevokeAPIKey to be called on repository")
	}
	if revokedDID != "did:plc:aggregator123" {
		t.Errorf("RevokeKey() called with DID = %s, want did:plc:aggregator123", revokedDID)
	}
}

func TestAPIKeyService_RevokeKey_Error(t *testing.T) {
	expectedError := errors.New("database error")
	repo := &mockRepository{
		revokeAPIKeyFunc: func(ctx context.Context, did string) error {
			return expectedError
		},
	}
	service := newTestAPIKeyService(repo)

	err := service.RevokeKey(context.Background(), "did:plc:aggregator123")
	if err == nil {
		t.Error("RevokeKey() expected error, got nil")
	}
}

// =============================================================================
// GetAPIKeyInfo Tests
// =============================================================================

func TestAPIKeyService_GetAPIKeyInfo_NoKey(t *testing.T) {
	repo := &mockRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*AggregatorCredentials, error) {
			return &AggregatorCredentials{
				DID:        did,
				APIKeyHash: "", // No key
			}, nil
		},
	}
	service := newTestAPIKeyService(repo)

	info, err := service.GetAPIKeyInfo(context.Background(), "did:plc:aggregator123")
	if err != nil {
		t.Fatalf("GetAPIKeyInfo() unexpected error: %v", err)
	}

	if info.HasKey {
		t.Error("GetAPIKeyInfo() HasKey = true, want false")
	}
}

func TestAPIKeyService_GetAPIKeyInfo_HasActiveKey(t *testing.T) {
	createdAt := time.Now().Add(-24 * time.Hour)
	lastUsed := time.Now().Add(-1 * time.Hour)

	repo := &mockRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*AggregatorCredentials, error) {
			return &AggregatorCredentials{
				DID:             did,
				APIKeyHash:      "somehash",
				APIKeyPrefix:    "ckapi_test12",
				APIKeyCreatedAt: &createdAt,
				APIKeyLastUsed:  &lastUsed,
			}, nil
		},
	}
	service := newTestAPIKeyService(repo)

	info, err := service.GetAPIKeyInfo(context.Background(), "did:plc:aggregator123")
	if err != nil {
		t.Fatalf("GetAPIKeyInfo() unexpected error: %v", err)
	}

	if !info.HasKey {
		t.Error("GetAPIKeyInfo() HasKey = false, want true")
	}
	if info.KeyPrefix != "ckapi_test12" {
		t.Errorf("GetAPIKeyInfo() KeyPrefix = %s, want ckapi_test12", info.KeyPrefix)
	}
	if info.IsRevoked {
		t.Error("GetAPIKeyInfo() IsRevoked = true, want false")
	}
	if info.CreatedAt == nil || !info.CreatedAt.Equal(createdAt) {
		t.Error("GetAPIKeyInfo() CreatedAt mismatch")
	}
	if info.LastUsedAt == nil || !info.LastUsedAt.Equal(lastUsed) {
		t.Error("GetAPIKeyInfo() LastUsedAt mismatch")
	}
}

func TestAPIKeyService_GetAPIKeyInfo_RevokedKey(t *testing.T) {
	revokedAt := time.Now().Add(-1 * time.Hour)

	repo := &mockRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*AggregatorCredentials, error) {
			return &AggregatorCredentials{
				DID:             did,
				APIKeyHash:      "somehash",
				APIKeyPrefix:    "ckapi_test12",
				APIKeyRevokedAt: &revokedAt,
			}, nil
		},
	}
	service := newTestAPIKeyService(repo)

	info, err := service.GetAPIKeyInfo(context.Background(), "did:plc:aggregator123")
	if err != nil {
		t.Fatalf("GetAPIKeyInfo() unexpected error: %v", err)
	}

	if !info.HasKey {
		t.Error("GetAPIKeyInfo() HasKey = false, want true (revoked keys still exist)")
	}
	if !info.IsRevoked {
		t.Error("GetAPIKeyInfo() IsRevoked = false, want true")
	}
	if info.RevokedAt == nil || !info.RevokedAt.Equal(revokedAt) {
		t.Error("GetAPIKeyInfo() RevokedAt mismatch")
	}
}

func TestAPIKeyService_GetAPIKeyInfo_NotFound(t *testing.T) {
	repo := &mockRepository{
		getAggregatorCredentialsFunc: func(ctx context.Context, did string) (*AggregatorCredentials, error) {
			return nil, ErrAggregatorNotFound
		},
	}
	service := newTestAPIKeyService(repo)

	_, err := service.GetAPIKeyInfo(context.Background(), "did:plc:nonexistent")
	if !errors.Is(err, ErrAggregatorNotFound) {
		t.Errorf("GetAPIKeyInfo() error = %v, want ErrAggregatorNotFound", err)
	}
}

// =============================================================================
// RefreshTokensIfNeeded Tests
// =============================================================================

func TestAPIKeyService_RefreshTokensIfNeeded_TokensStillValid(t *testing.T) {
	// Tokens expire in 1 hour - well beyond the 5 minute buffer
	expiresAt := time.Now().Add(1 * time.Hour)

	creds := &AggregatorCredentials{
		DID:                 "did:plc:aggregator123",
		OAuthTokenExpiresAt: &expiresAt,
	}

	repo := &mockRepository{}
	service := newTestAPIKeyService(repo)

	err := service.RefreshTokensIfNeeded(context.Background(), creds)
	if err != nil {
		t.Fatalf("RefreshTokensIfNeeded() unexpected error: %v", err)
	}

	// No refresh should have happened - we can't easily verify this without
	// more complex mocking, but the absence of error is the key indicator
}

func TestAPIKeyService_RefreshTokensIfNeeded_WithinBuffer(t *testing.T) {
	// Token expires in 4 minutes - within the 5 minute buffer, so needs refresh
	// This test verifies that when tokens are within the buffer, the service
	// attempts to refresh them.
	//
	// Note: Full integration testing of token refresh requires a real OAuth app.
	// This test is intentionally skipped as it would require extensive mocking
	// of the indigo OAuth library internals.
	t.Skip("RefreshTokensIfNeeded requires fully configured OAuth app - covered by integration tests")
}

func TestAPIKeyService_RefreshTokensIfNeeded_ExpiredNilTokens(t *testing.T) {
	// When OAuthTokenExpiresAt is nil, tokens need refresh
	// This should also attempt to refresh (and fail with nil OAuth app)
	t.Skip("RefreshTokensIfNeeded requires fully configured OAuth app - covered by integration tests")
}

// =============================================================================
// GetAccessToken Tests
// =============================================================================

func TestAPIKeyService_GetAccessToken_ValidAggregatorTokensNotExpired(t *testing.T) {
	// Tokens expire in 1 hour - well beyond the 5 minute buffer
	expiresAt := time.Now().Add(1 * time.Hour)
	expectedToken := "valid_access_token_123"

	creds := &AggregatorCredentials{
		DID:                 "did:plc:aggregator123",
		OAuthAccessToken:    expectedToken,
		OAuthTokenExpiresAt: &expiresAt,
	}

	repo := &mockRepository{}
	service := newTestAPIKeyService(repo)

	token, err := service.GetAccessToken(context.Background(), creds)
	if err != nil {
		t.Fatalf("GetAccessToken() unexpected error: %v", err)
	}

	if token != expectedToken {
		t.Errorf("GetAccessToken() = %s, want %s", token, expectedToken)
	}
}

func TestAPIKeyService_GetAccessToken_ExpiredTokens(t *testing.T) {
	// Tokens expired 1 hour ago - requires refresh
	// Since refresh requires a real OAuth app, this test verifies the error path
	expiresAt := time.Now().Add(-1 * time.Hour)

	creds := &AggregatorCredentials{
		DID:                 "did:plc:aggregator123",
		OAuthAccessToken:    "expired_token",
		OAuthRefreshToken:   "refresh_token",
		OAuthTokenExpiresAt: &expiresAt,
	}

	repo := &mockRepository{}
	// Service has nil OAuth app, so refresh will fail
	service := newTestAPIKeyService(repo)

	_, err := service.GetAccessToken(context.Background(), creds)
	if err == nil {
		t.Error("GetAccessToken() expected error when tokens are expired and no OAuth app configured, got nil")
	}
}

func TestAPIKeyService_GetAccessToken_NilExpiry(t *testing.T) {
	// Nil expiry means tokens need refresh
	creds := &AggregatorCredentials{
		DID:                 "did:plc:aggregator123",
		OAuthAccessToken:    "some_token",
		OAuthTokenExpiresAt: nil, // nil means needs refresh
	}

	repo := &mockRepository{}
	service := newTestAPIKeyService(repo)

	_, err := service.GetAccessToken(context.Background(), creds)
	if err == nil {
		t.Error("GetAccessToken() expected error when expiry is nil and no OAuth app configured, got nil")
	}
}

func TestAPIKeyService_GetAccessToken_WithinExpiryBuffer(t *testing.T) {
	// Tokens expire in 4 minutes - within the 5 minute buffer, so needs refresh
	expiresAt := time.Now().Add(4 * time.Minute)

	creds := &AggregatorCredentials{
		DID:                 "did:plc:aggregator123",
		OAuthAccessToken:    "soon_to_expire_token",
		OAuthRefreshToken:   "refresh_token",
		OAuthTokenExpiresAt: &expiresAt,
	}

	repo := &mockRepository{}
	service := newTestAPIKeyService(repo)

	// Should attempt refresh and fail since no OAuth app is configured
	_, err := service.GetAccessToken(context.Background(), creds)
	if err == nil {
		t.Error("GetAccessToken() expected error when tokens are within buffer and no OAuth app configured, got nil")
	}
}

func TestAPIKeyService_GetAccessToken_RevokedKey(t *testing.T) {
	// Test behavior when aggregator has a revoked key
	// The API key check happens in ValidateKey, but GetAccessToken should still work
	// if called directly with a valid aggregator (before revocation is detected)
	expiresAt := time.Now().Add(1 * time.Hour)
	revokedAt := time.Now().Add(-30 * time.Minute)
	expectedToken := "valid_access_token"

	creds := &AggregatorCredentials{
		DID:                 "did:plc:aggregator123",
		APIKeyRevokedAt:     &revokedAt, // Key is revoked
		OAuthAccessToken:    expectedToken,
		OAuthTokenExpiresAt: &expiresAt,
	}

	repo := &mockRepository{}
	service := newTestAPIKeyService(repo)

	// GetAccessToken doesn't check revocation - that's done at ValidateKey level
	// It just returns the token if valid
	token, err := service.GetAccessToken(context.Background(), creds)
	if err != nil {
		t.Fatalf("GetAccessToken() unexpected error: %v", err)
	}

	if token != expectedToken {
		t.Errorf("GetAccessToken() = %s, want %s", token, expectedToken)
	}
}

func TestAPIKeyService_FailureCounters_InitiallyZero(t *testing.T) {
	repo := &mockRepository{}
	service := newTestAPIKeyService(repo)

	if got := service.GetFailedLastUsedUpdates(); got != 0 {
		t.Errorf("GetFailedLastUsedUpdates() = %d, want 0", got)
	}

	if got := service.GetFailedNonceUpdates(); got != 0 {
		t.Errorf("GetFailedNonceUpdates() = %d, want 0", got)
	}
}

func TestAPIKeyService_FailedLastUsedUpdates_IncrementsOnError(t *testing.T) {
	// Create a valid API key
	plainKey := APIKeyPrefix + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	keyHash := hashAPIKey(plainKey)

	updateCalled := make(chan struct{}, 1)
	repo := &mockRepository{
		getCredentialsByAPIKeyHashFunc: func(ctx context.Context, hash string) (*AggregatorCredentials, error) {
			if hash == keyHash {
				return &AggregatorCredentials{
					DID:        "did:plc:aggregator123",
					APIKeyHash: keyHash,
				}, nil
			}
			return nil, ErrAPIKeyInvalid
		},
		updateAPIKeyLastUsedFunc: func(ctx context.Context, did string) error {
			defer func() { updateCalled <- struct{}{} }()
			return errors.New("database connection failed")
		},
	}

	service := newTestAPIKeyService(repo)

	// Initial count should be 0
	if got := service.GetFailedLastUsedUpdates(); got != 0 {
		t.Errorf("GetFailedLastUsedUpdates() initial = %d, want 0", got)
	}

	// Validate the key (triggers async last_used update)
	_, err := service.ValidateKey(context.Background(), plainKey)
	if err != nil {
		t.Fatalf("ValidateKey() unexpected error: %v", err)
	}

	// Wait for async update to complete
	select {
	case <-updateCalled:
		// Update was called
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for async UpdateAPIKeyLastUsed call")
	}

	// Give a moment for the counter to be incremented
	time.Sleep(10 * time.Millisecond)

	// Counter should now be 1
	if got := service.GetFailedLastUsedUpdates(); got != 1 {
		t.Errorf("GetFailedLastUsedUpdates() after failure = %d, want 1", got)
	}
}

// =============================================================================
// RefreshExpiringTokens Tests
// =============================================================================

func TestAPIKeyService_RefreshExpiringTokens_DatabaseError(t *testing.T) {
	expectedError := errors.New("database connection failed")
	repo := &mockRepository{
		listAggregatorsNeedingTokenRefreshFunc: func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
			return nil, expectedError
		},
	}
	service := newTestAPIKeyService(repo)

	refreshed, errs := service.RefreshExpiringTokens(context.Background(), 1*time.Hour)

	if refreshed != 0 {
		t.Errorf("RefreshExpiringTokens() refreshed = %d, want 0", refreshed)
	}
	if len(errs) != 1 {
		t.Fatalf("RefreshExpiringTokens() errors count = %d, want 1", len(errs))
	}
	if !errors.Is(errs[0], expectedError) {
		t.Errorf("RefreshExpiringTokens() error = %v, want %v", errs[0], expectedError)
	}
}

func TestAPIKeyService_RefreshExpiringTokens_EmptyList(t *testing.T) {
	repo := &mockRepository{
		listAggregatorsNeedingTokenRefreshFunc: func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
			return []*AggregatorCredentials{}, nil
		},
	}
	service := newTestAPIKeyService(repo)

	refreshed, errs := service.RefreshExpiringTokens(context.Background(), 1*time.Hour)

	if refreshed != 0 {
		t.Errorf("RefreshExpiringTokens() refreshed = %d, want 0", refreshed)
	}
	if len(errs) != 0 {
		t.Errorf("RefreshExpiringTokens() errors count = %d, want 0", len(errs))
	}
}

func TestAPIKeyService_RefreshExpiringTokens_NilList(t *testing.T) {
	repo := &mockRepository{
		listAggregatorsNeedingTokenRefreshFunc: func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
			return nil, nil
		},
	}
	service := newTestAPIKeyService(repo)

	refreshed, errs := service.RefreshExpiringTokens(context.Background(), 1*time.Hour)

	if refreshed != 0 {
		t.Errorf("RefreshExpiringTokens() refreshed = %d, want 0", refreshed)
	}
	if len(errs) != 0 {
		t.Errorf("RefreshExpiringTokens() errors count = %d, want 0", len(errs))
	}
}

func TestAPIKeyService_RefreshExpiringTokens_PassesCorrectExpiryBuffer(t *testing.T) {
	expectedBuffer := 2 * time.Hour
	var capturedBuffer time.Duration

	repo := &mockRepository{
		listAggregatorsNeedingTokenRefreshFunc: func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
			capturedBuffer = expiryBuffer
			return nil, nil
		},
	}
	service := newTestAPIKeyService(repo)

	service.RefreshExpiringTokens(context.Background(), expectedBuffer)

	if capturedBuffer != expectedBuffer {
		t.Errorf("RefreshExpiringTokens() passed expiryBuffer = %v, want %v", capturedBuffer, expectedBuffer)
	}
}

func TestAPIKeyService_RefreshExpiringTokens_TokensStillValid(t *testing.T) {
	// When tokens are still valid (not within refresh buffer), no refresh should happen
	// This tests the case where RefreshTokensIfNeeded returns early because tokens are valid
	expiresAt := time.Now().Add(1 * time.Hour) // Well beyond the 5 minute buffer

	repo := &mockRepository{
		listAggregatorsNeedingTokenRefreshFunc: func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
			return []*AggregatorCredentials{
				{
					DID:                 "did:plc:aggregator1",
					OAuthTokenExpiresAt: &expiresAt,
					OAuthAccessToken:    "valid_token",
					OAuthRefreshToken:   "refresh_token",
				},
			}, nil
		},
	}
	service := newTestAPIKeyService(repo)

	refreshed, errs := service.RefreshExpiringTokens(context.Background(), 1*time.Hour)

	// Tokens are valid, so RefreshTokensIfNeeded returns early without error
	// and counts as "refreshed" (even though no actual refresh was needed)
	if refreshed != 1 {
		t.Errorf("RefreshExpiringTokens() refreshed = %d, want 1", refreshed)
	}
	if len(errs) != 0 {
		t.Errorf("RefreshExpiringTokens() errors count = %d, want 0", len(errs))
	}
}

func TestAPIKeyService_RefreshExpiringTokens_TokensExpired_RefreshFails(t *testing.T) {
	// When tokens are expired and refresh fails (no OAuth app configured)
	expiresAt := time.Now().Add(-1 * time.Hour) // Already expired

	repo := &mockRepository{
		listAggregatorsNeedingTokenRefreshFunc: func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
			return []*AggregatorCredentials{
				{
					DID:                 "did:plc:aggregator1",
					OAuthTokenExpiresAt: &expiresAt,
					OAuthAccessToken:    "expired_token",
					OAuthRefreshToken:   "refresh_token",
				},
			}, nil
		},
	}
	service := newTestAPIKeyService(repo)

	refreshed, errs := service.RefreshExpiringTokens(context.Background(), 1*time.Hour)

	if refreshed != 0 {
		t.Errorf("RefreshExpiringTokens() refreshed = %d, want 0", refreshed)
	}
	if len(errs) != 1 {
		t.Errorf("RefreshExpiringTokens() errors count = %d, want 1", len(errs))
	}
}

func TestAPIKeyService_RefreshExpiringTokens_MixedResults(t *testing.T) {
	// Multiple aggregators: some with valid tokens, some with expired tokens
	validExpiry := time.Now().Add(1 * time.Hour)
	expiredExpiry := time.Now().Add(-1 * time.Hour)

	repo := &mockRepository{
		listAggregatorsNeedingTokenRefreshFunc: func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
			return []*AggregatorCredentials{
				{
					DID:                 "did:plc:valid1",
					OAuthTokenExpiresAt: &validExpiry,
					OAuthAccessToken:    "valid_token",
				},
				{
					DID:                 "did:plc:expired1",
					OAuthTokenExpiresAt: &expiredExpiry,
					OAuthAccessToken:    "expired_token",
					OAuthRefreshToken:   "refresh_token",
				},
				{
					DID:                 "did:plc:valid2",
					OAuthTokenExpiresAt: &validExpiry,
					OAuthAccessToken:    "valid_token2",
				},
			}, nil
		},
	}
	service := newTestAPIKeyService(repo)

	refreshed, errs := service.RefreshExpiringTokens(context.Background(), 1*time.Hour)

	// 2 valid tokens should count as refreshed, 1 expired token should fail
	if refreshed != 2 {
		t.Errorf("RefreshExpiringTokens() refreshed = %d, want 2", refreshed)
	}
	if len(errs) != 1 {
		t.Errorf("RefreshExpiringTokens() errors count = %d, want 1", len(errs))
	}
}

func TestAPIKeyService_RefreshExpiringTokens_ContextCancellation(t *testing.T) {
	// Test that context cancellation is respected
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	repo := &mockRepository{
		listAggregatorsNeedingTokenRefreshFunc: func(ctx context.Context, expiryBuffer time.Duration) ([]*AggregatorCredentials, error) {
			// Check if context is already cancelled
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				return []*AggregatorCredentials{
					{DID: "did:plc:test"},
				}, nil
			}
		},
	}
	service := newTestAPIKeyService(repo)

	refreshed, errs := service.RefreshExpiringTokens(ctx, 1*time.Hour)

	// Should fail due to context cancellation
	if refreshed != 0 {
		t.Errorf("RefreshExpiringTokens() refreshed = %d, want 0", refreshed)
	}
	if len(errs) != 1 {
		t.Errorf("RefreshExpiringTokens() errors count = %d, want 1", len(errs))
	}
}
