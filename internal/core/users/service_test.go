package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/identity"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockTurnstile is a test-only TurnstileVerifier that returns a preconfigured
// error. Mutates internal state on each call to support assertions about the
// last invocation. NOT safe for concurrent use — tests that exercise multiple
// goroutines should use stubTurnstile instead.
type mockTurnstile struct {
	err error
	// captured inputs from the last call
	lastToken    string
	lastRemoteIP string
	called       bool
}

func (m *mockTurnstile) Verify(_ context.Context, token, remoteIP string) error {
	m.called = true
	m.lastToken = token
	m.lastRemoteIP = remoteIP
	return m.err
}

// stubTurnstile is a concurrent-safe TurnstileVerifier with no state — always
// returns the configured error. Use this in tests that fan out across goroutines.
type stubTurnstile struct {
	err error
}

func (s stubTurnstile) Verify(_ context.Context, _ string, _ string) error {
	return s.err
}

// MockUserRepository is a mock implementation of UserRepository
type MockUserRepository struct {
	mock.Mock
}

func (m *MockUserRepository) Create(ctx context.Context, user *User) (*User, error) {
	args := m.Called(ctx, user)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*User), args.Error(1)
}

func (m *MockUserRepository) GetByDID(ctx context.Context, did string) (*User, error) {
	args := m.Called(ctx, did)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*User), args.Error(1)
}

func (m *MockUserRepository) GetByHandle(ctx context.Context, handle string) (*User, error) {
	args := m.Called(ctx, handle)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*User), args.Error(1)
}

func (m *MockUserRepository) UpdateHandle(ctx context.Context, did, newHandle string) (*User, error) {
	args := m.Called(ctx, did, newHandle)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*User), args.Error(1)
}

func (m *MockUserRepository) GetByDIDs(ctx context.Context, dids []string) (map[string]*User, error) {
	args := m.Called(ctx, dids)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(map[string]*User), args.Error(1)
}

func (m *MockUserRepository) GetProfileStats(ctx context.Context, did string) (*ProfileStats, error) {
	args := m.Called(ctx, did)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*ProfileStats), args.Error(1)
}

func (m *MockUserRepository) Delete(ctx context.Context, did string) error {
	args := m.Called(ctx, did)
	return args.Error(0)
}

func (m *MockUserRepository) UpdateProfile(ctx context.Context, did string, input UpdateProfileInput) (*User, error) {
	args := m.Called(ctx, did, input)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*User), args.Error(1)
}

// MockIdentityResolver is a mock implementation of identity.Resolver
type MockIdentityResolver struct {
	mock.Mock
}

func (m *MockIdentityResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	args := m.Called(ctx, identifier)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*identity.Identity), args.Error(1)
}

func (m *MockIdentityResolver) ResolveHandle(ctx context.Context, handle string) (string, string, error) {
	args := m.Called(ctx, handle)
	return args.String(0), args.String(1), args.Error(2)
}

func (m *MockIdentityResolver) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	args := m.Called(ctx, did)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*identity.DIDDocument), args.Error(1)
}

func (m *MockIdentityResolver) Purge(ctx context.Context, identifier string) error {
	args := m.Called(ctx, identifier)
	return args.Error(0)
}

// TestDeleteAccount_Success tests successful account deletion
func TestDeleteAccount_Success(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:testuser123"
	testHandle := "testuser.test"
	testUser := &User{
		DID:       testDID,
		Handle:    testHandle,
		PDSURL:    "https://test.pds",
		CreatedAt: time.Now(),
	}

	// Setup expectations
	mockRepo.On("GetByDID", mock.Anything, testDID).Return(testUser, nil)
	mockRepo.On("Delete", mock.Anything, testDID).Return(nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, testDID)
	assert.NoError(t, err)

	mockRepo.AssertExpectations(t)
}

// TestDeleteAccount_UserNotFound tests deletion of non-existent user
func TestDeleteAccount_UserNotFound(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:nonexistent"

	// Setup expectations
	mockRepo.On("GetByDID", mock.Anything, testDID).Return(nil, ErrUserNotFound)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, testDID)
	assert.ErrorIs(t, err, ErrUserNotFound)

	mockRepo.AssertExpectations(t)
	mockRepo.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything)
}

// TestDeleteAccount_EmptyDID tests deletion with empty DID
func TestDeleteAccount_EmptyDID(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, "")
	assert.Error(t, err)

	// Verify it's an InvalidDIDError
	var invalidDIDErr *InvalidDIDError
	assert.True(t, errors.As(err, &invalidDIDErr), "expected InvalidDIDError")
	assert.Contains(t, err.Error(), "DID is required")

	mockRepo.AssertNotCalled(t, "GetByDID", mock.Anything, mock.Anything)
	mockRepo.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything)
}

// TestDeleteAccount_WhitespaceDID tests deletion with whitespace-only DID
func TestDeleteAccount_WhitespaceDID(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, "   ")
	assert.Error(t, err)

	// Verify it's an InvalidDIDError
	var invalidDIDErr *InvalidDIDError
	assert.True(t, errors.As(err, &invalidDIDErr), "expected InvalidDIDError")
	assert.Contains(t, err.Error(), "DID is required")
}

// TestDeleteAccount_LeadingTrailingWhitespace tests that DIDs are trimmed
func TestDeleteAccount_LeadingTrailingWhitespace(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	// The input has whitespace but after trimming should be a valid DID
	inputDID := "  did:plc:whitespacetest  "
	trimmedDID := "did:plc:whitespacetest"

	testUser := &User{
		DID:       trimmedDID,
		Handle:    "whitespacetest.test",
		PDSURL:    "https://test.pds",
		CreatedAt: time.Now(),
	}

	// Expectations should use the trimmed DID
	mockRepo.On("GetByDID", mock.Anything, trimmedDID).Return(testUser, nil)
	mockRepo.On("Delete", mock.Anything, trimmedDID).Return(nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, inputDID)
	assert.NoError(t, err)

	mockRepo.AssertExpectations(t)
}

// TestDeleteAccount_InvalidDIDFormat tests deletion with invalid DID format
func TestDeleteAccount_InvalidDIDFormat(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, "invalid-did-format")
	assert.Error(t, err)

	// Verify it's an InvalidDIDError
	var invalidDIDErr *InvalidDIDError
	assert.True(t, errors.As(err, &invalidDIDErr), "expected InvalidDIDError")
	assert.Contains(t, err.Error(), "must start with 'did:'")
}

// TestDeleteAccount_RepoDeleteFails tests handling when repository delete fails
func TestDeleteAccount_RepoDeleteFails(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:testuser456"
	testUser := &User{
		DID:       testDID,
		Handle:    "testuser456.test",
		PDSURL:    "https://test.pds",
		CreatedAt: time.Now(),
	}

	// Setup expectations
	mockRepo.On("GetByDID", mock.Anything, testDID).Return(testUser, nil)
	mockRepo.On("Delete", mock.Anything, testDID).Return(errors.New("database error"))

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, testDID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to delete account")

	mockRepo.AssertExpectations(t)
}

// TestDeleteAccount_GetByDIDFails tests handling when GetByDID fails (non-NotFound error)
func TestDeleteAccount_GetByDIDFails(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:testuser789"

	// Setup expectations
	mockRepo.On("GetByDID", mock.Anything, testDID).Return(nil, errors.New("database connection error"))

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, testDID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get user for deletion")

	mockRepo.AssertExpectations(t)
	mockRepo.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything)
}

// TestDeleteAccount_ContextCancellation tests behavior with cancelled context
func TestDeleteAccount_ContextCancellation(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:testcontextcancel"

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Setup expectations - GetByDID should fail due to cancelled context
	mockRepo.On("GetByDID", mock.Anything, testDID).Return(nil, context.Canceled)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")

	err := service.DeleteAccount(ctx, testDID)
	assert.Error(t, err)

	mockRepo.AssertExpectations(t)
}

// TestDeleteAccount_PLCAndWebDID tests deletion works with both did:plc and did:web
func TestDeleteAccount_PLCAndWebDID(t *testing.T) {
	tests := []struct {
		name string
		did  string
	}{
		{
			name: "did:plc format",
			did:  "did:plc:abc123xyz",
		},
		{
			name: "did:web format",
			did:  "did:web:example.com",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockRepo := new(MockUserRepository)
			mockResolver := new(MockIdentityResolver)

			testUser := &User{
				DID:       tc.did,
				Handle:    "testuser.test",
				PDSURL:    "https://test.pds",
				CreatedAt: time.Now(),
			}

			mockRepo.On("GetByDID", mock.Anything, tc.did).Return(testUser, nil)
			mockRepo.On("Delete", mock.Anything, tc.did).Return(nil)

			service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
			ctx := context.Background()

			err := service.DeleteAccount(ctx, tc.did)
			assert.NoError(t, err)

			mockRepo.AssertExpectations(t)
		})
	}
}

// TestGetUserByDID tests retrieving a user by DID
func TestGetUserByDID(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:testuser"
	testUser := &User{
		DID:       testDID,
		Handle:    "testuser.test",
		PDSURL:    "https://test.pds",
		CreatedAt: time.Now(),
	}

	mockRepo.On("GetByDID", mock.Anything, testDID).Return(testUser, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	user, err := service.GetUserByDID(ctx, testDID)
	require.NoError(t, err)
	assert.Equal(t, testDID, user.DID)
	assert.Equal(t, "testuser.test", user.Handle)
}

// TestGetUserByDID_EmptyDID tests GetUserByDID with empty DID
func TestGetUserByDID_EmptyDID(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	_, err := service.GetUserByDID(ctx, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DID is required")
}

// TestGetUserByHandle tests retrieving a user by handle
func TestGetUserByHandle(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testHandle := "testuser.test"
	testUser := &User{
		DID:       "did:plc:testuser",
		Handle:    testHandle,
		PDSURL:    "https://test.pds",
		CreatedAt: time.Now(),
	}

	mockRepo.On("GetByHandle", mock.Anything, testHandle).Return(testUser, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	user, err := service.GetUserByHandle(ctx, testHandle)
	require.NoError(t, err)
	assert.Equal(t, testHandle, user.Handle)
}

// TestGetProfile tests retrieving a user's profile with stats
func TestGetProfile(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:profileuser"
	testUser := &User{
		DID:       testDID,
		Handle:    "profileuser.test",
		PDSURL:    "https://test.pds",
		CreatedAt: time.Now(),
	}
	testStats := &ProfileStats{
		PostCount:       10,
		CommentCount:    25,
		CommunityCount:  5,
		MembershipCount: 3,
		Reputation:      150,
	}

	mockRepo.On("GetByDID", mock.Anything, testDID).Return(testUser, nil)
	mockRepo.On("GetProfileStats", mock.Anything, testDID).Return(testStats, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	profile, err := service.GetProfile(ctx, testDID)
	require.NoError(t, err)
	assert.Equal(t, testDID, profile.DID)
	assert.Equal(t, 10, profile.Stats.PostCount)
	assert.Equal(t, 150, profile.Stats.Reputation)
}

// TestIndexUser tests indexing a new user
func TestIndexUser(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:newuser"
	testHandle := "newuser.test"
	testPDSURL := "https://test.pds"

	testUser := &User{
		DID:       testDID,
		Handle:    testHandle,
		PDSURL:    testPDSURL,
		CreatedAt: time.Now(),
	}

	mockRepo.On("Create", mock.Anything, mock.MatchedBy(func(u *User) bool {
		return u.DID == testDID && u.Handle == testHandle
	})).Return(testUser, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	err := service.IndexUser(ctx, testDID, testHandle, testPDSURL)
	assert.NoError(t, err)

	mockRepo.AssertExpectations(t)
}

// TestGetProfile_WithAvatarAndBanner tests that GetProfile transforms CIDs to URLs
func TestGetProfile_WithAvatarAndBanner(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:avataruser"
	testUser := &User{
		DID:         testDID,
		Handle:      "avataruser.test",
		PDSURL:      "https://test.pds",
		DisplayName: "Avatar User",
		Bio:         "Test bio for avatar user",
		AvatarCID:   "bafkreiabc123avatar",
		BannerCID:   "bafkreixyz789banner",
		CreatedAt:   time.Now(),
	}
	testStats := &ProfileStats{
		PostCount:       5,
		CommentCount:    10,
		CommunityCount:  2,
		MembershipCount: 1,
		Reputation:      50,
	}

	mockRepo.On("GetByDID", mock.Anything, testDID).Return(testUser, nil)
	mockRepo.On("GetProfileStats", mock.Anything, testDID).Return(testStats, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	profile, err := service.GetProfile(ctx, testDID)
	require.NoError(t, err)

	// Verify basic fields
	assert.Equal(t, testDID, profile.DID)
	assert.Equal(t, "avataruser.test", profile.Handle)
	assert.Equal(t, "Avatar User", profile.DisplayName)
	assert.Equal(t, "Test bio for avatar user", profile.Bio)

	// Verify CID-to-URL transformation (DID is URL-encoded in query params)
	expectedAvatarURL := "https://test.pds/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Aavataruser&cid=bafkreiabc123avatar"
	expectedBannerURL := "https://test.pds/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Aavataruser&cid=bafkreixyz789banner"
	assert.Equal(t, expectedAvatarURL, profile.Avatar)
	assert.Equal(t, expectedBannerURL, profile.Banner)

	mockRepo.AssertExpectations(t)
}

// TestGetProfile_WithAvatarOnly tests GetProfile with only avatar CID set
func TestGetProfile_WithAvatarOnly(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:avataronly"
	testUser := &User{
		DID:         testDID,
		Handle:      "avataronly.test",
		PDSURL:      "https://test.pds",
		DisplayName: "Avatar Only User",
		Bio:         "",
		AvatarCID:   "bafkreiavataronly",
		BannerCID:   "", // No banner
		CreatedAt:   time.Now(),
	}
	testStats := &ProfileStats{}

	mockRepo.On("GetByDID", mock.Anything, testDID).Return(testUser, nil)
	mockRepo.On("GetProfileStats", mock.Anything, testDID).Return(testStats, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	profile, err := service.GetProfile(ctx, testDID)
	require.NoError(t, err)

	// Avatar should be transformed to URL (DID is URL-encoded in query params)
	expectedAvatarURL := "https://test.pds/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Aavataronly&cid=bafkreiavataronly"
	assert.Equal(t, expectedAvatarURL, profile.Avatar)

	// Banner should be empty
	assert.Empty(t, profile.Banner)

	mockRepo.AssertExpectations(t)
}

// TestGetProfile_WithNoCIDsOrProfile tests GetProfile with no avatar/banner/display name/bio
func TestGetProfile_WithNoCIDsOrProfile(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:basicuser"
	testUser := &User{
		DID:         testDID,
		Handle:      "basicuser.test",
		PDSURL:      "https://test.pds",
		DisplayName: "",
		Bio:         "",
		AvatarCID:   "",
		BannerCID:   "",
		CreatedAt:   time.Now(),
	}
	testStats := &ProfileStats{}

	mockRepo.On("GetByDID", mock.Anything, testDID).Return(testUser, nil)
	mockRepo.On("GetProfileStats", mock.Anything, testDID).Return(testStats, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	profile, err := service.GetProfile(ctx, testDID)
	require.NoError(t, err)

	// All profile fields should be empty
	assert.Empty(t, profile.DisplayName)
	assert.Empty(t, profile.Bio)
	assert.Empty(t, profile.Avatar)
	assert.Empty(t, profile.Banner)

	mockRepo.AssertExpectations(t)
}

// TestGetProfile_WithEmptyPDSURL tests GetProfile does not create URLs when PDSURL is empty
func TestGetProfile_WithEmptyPDSURL(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:nopdsurl"
	testUser := &User{
		DID:         testDID,
		Handle:      "nopdsurl.test",
		PDSURL:      "", // No PDS URL
		DisplayName: "No PDS URL User",
		Bio:         "Test bio",
		AvatarCID:   "bafkreiavatarcid", // Has CID but no PDS URL
		BannerCID:   "bafkreibannercid",
		CreatedAt:   time.Now(),
	}
	testStats := &ProfileStats{}

	mockRepo.On("GetByDID", mock.Anything, testDID).Return(testUser, nil)
	mockRepo.On("GetProfileStats", mock.Anything, testDID).Return(testStats, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	profile, err := service.GetProfile(ctx, testDID)
	require.NoError(t, err)

	// Avatar and Banner should be empty since we can't construct URLs without PDS URL
	assert.Empty(t, profile.Avatar)
	assert.Empty(t, profile.Banner)

	// But display name and bio should still be set
	assert.Equal(t, "No PDS URL User", profile.DisplayName)
	assert.Equal(t, "Test bio", profile.Bio)

	mockRepo.AssertExpectations(t)
}

// TestUpdateProfile_Success tests successful profile update
func TestUpdateProfile_Success(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:updateuser"
	displayName := "Updated Name"
	bio := "Updated bio"
	avatarCID := "bafkreinewavatar"
	bannerCID := "bafkreinewbanner"

	updatedUser := &User{
		DID:         testDID,
		Handle:      "updateuser.test",
		PDSURL:      "https://test.pds",
		DisplayName: displayName,
		Bio:         bio,
		AvatarCID:   avatarCID,
		BannerCID:   bannerCID,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	input := UpdateProfileInput{
		DisplayName: &displayName,
		Bio:         &bio,
		AvatarCID:   &avatarCID,
		BannerCID:   &bannerCID,
	}
	mockRepo.On("UpdateProfile", mock.Anything, testDID, input).Return(updatedUser, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	user, err := service.UpdateProfile(ctx, testDID, input)
	require.NoError(t, err)

	assert.Equal(t, displayName, user.DisplayName)
	assert.Equal(t, bio, user.Bio)
	assert.Equal(t, avatarCID, user.AvatarCID)
	assert.Equal(t, bannerCID, user.BannerCID)

	mockRepo.AssertExpectations(t)
}

// TestUpdateProfile_PartialUpdate tests updating only some fields
func TestUpdateProfile_PartialUpdate(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:partialupdate"
	displayName := "Partial Update Name"
	// Other fields are nil (don't change)

	updatedUser := &User{
		DID:         testDID,
		Handle:      "partialupdate.test",
		PDSURL:      "https://test.pds",
		DisplayName: displayName,
		Bio:         "existing bio",
		AvatarCID:   "existingavatar",
		BannerCID:   "existingbanner",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// Only displayName is provided, others are nil
	input := UpdateProfileInput{
		DisplayName: &displayName,
	}
	mockRepo.On("UpdateProfile", mock.Anything, testDID, input).Return(updatedUser, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	user, err := service.UpdateProfile(ctx, testDID, input)
	require.NoError(t, err)

	assert.Equal(t, displayName, user.DisplayName)
	// Existing values should be preserved
	assert.Equal(t, "existing bio", user.Bio)
	assert.Equal(t, "existingavatar", user.AvatarCID)

	mockRepo.AssertExpectations(t)
}

// TestUpdateProfile_ClearFields tests clearing fields with empty strings
func TestUpdateProfile_ClearFields(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:clearfields"
	emptyDisplayName := ""
	emptyBio := ""

	updatedUser := &User{
		DID:         testDID,
		Handle:      "clearfields.test",
		PDSURL:      "https://test.pds",
		DisplayName: "",
		Bio:         "",
		AvatarCID:   "existingavatar",
		BannerCID:   "existingbanner",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	input := UpdateProfileInput{
		DisplayName: &emptyDisplayName,
		Bio:         &emptyBio,
	}
	mockRepo.On("UpdateProfile", mock.Anything, testDID, input).Return(updatedUser, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	user, err := service.UpdateProfile(ctx, testDID, input)
	require.NoError(t, err)

	assert.Empty(t, user.DisplayName)
	assert.Empty(t, user.Bio)

	mockRepo.AssertExpectations(t)
}

// TestUpdateProfile_RepoError tests UpdateProfile returns error on repo failure
func TestUpdateProfile_RepoError(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:erroruser"
	displayName := "Error User"

	input := UpdateProfileInput{
		DisplayName: &displayName,
	}
	mockRepo.On("UpdateProfile", mock.Anything, testDID, input).Return(nil, errors.New("database error"))

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	_, err := service.UpdateProfile(ctx, testDID, input)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "database error")

	mockRepo.AssertExpectations(t)
}

// TestUpdateProfile_UserNotFound tests UpdateProfile with non-existent user
func TestUpdateProfile_UserNotFound(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	testDID := "did:plc:notfound"
	displayName := "Not Found User"

	input := UpdateProfileInput{
		DisplayName: &displayName,
	}
	mockRepo.On("UpdateProfile", mock.Anything, testDID, input).Return(nil, ErrUserNotFound)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	_, err := service.UpdateProfile(ctx, testDID, input)
	assert.ErrorIs(t, err, ErrUserNotFound)

	mockRepo.AssertExpectations(t)
}

// TestUpdateProfile_EmptyDID tests UpdateProfile with empty DID
func TestUpdateProfile_EmptyDID(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	displayName := "Test Name"
	input := UpdateProfileInput{
		DisplayName: &displayName,
	}
	_, err := service.UpdateProfile(ctx, "", input)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DID is required")

	// Repo should not be called with empty DID
	mockRepo.AssertNotCalled(t, "UpdateProfile", mock.Anything, mock.Anything, mock.Anything)
}

// TestUpdateProfile_WhitespaceDID tests UpdateProfile with whitespace-only DID
func TestUpdateProfile_WhitespaceDID(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")
	ctx := context.Background()

	displayName := "Test Name"
	input := UpdateProfileInput{
		DisplayName: &displayName,
	}
	_, err := service.UpdateProfile(ctx, "   ", input)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DID is required")

	mockRepo.AssertNotCalled(t, "UpdateProfile", mock.Anything, mock.Anything, mock.Anything)
}

// startMockPDSAdmin returns an httptest server that behaves like a PDS
// com.atproto.server.createInviteCode admin endpoint. statusCode controls the HTTP
// response and, when 200, the returned code is "pds-invite-OK".
func startMockPDSAdmin(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/xrpc/com.atproto.server.createInviteCode", r.URL.Path)
		// Basic auth must be "admin:<password>"
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok, "basic auth required")
		assert.Equal(t, "admin", user)
		assert.NotEmpty(t, pass)
		// Body must be {"useCount":1}
		var body map[string]int
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, 1, body["useCount"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if statusCode == http.StatusOK {
			_, _ = w.Write([]byte(`{"code":"pds-invite-OK"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"mock"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRequestSignupToken_Success(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := &mockTurnstile{}
	pds := startMockPDSAdmin(t, http.StatusOK)

	service := NewUserService(mockRepo, nil, pds.URL, turnstile, "admin-pw")

	resp, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "tok",
		RemoteIP:       "1.2.3.4",
	})
	require.NoError(t, err)
	assert.Equal(t, "pds-invite-OK", resp.InviteCode)
	assert.True(t, turnstile.called)
	assert.Equal(t, "tok", turnstile.lastToken)
	assert.Equal(t, "1.2.3.4", turnstile.lastRemoteIP)
}

func TestRequestSignupToken_CaptchaFail(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := &mockTurnstile{err: &InvalidCaptchaError{Reason: "rejected"}}
	// PDS endpoint must NOT be called — fail fast if it is.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("PDS admin should not be called when captcha fails")
	}))
	defer srv.Close()

	service := NewUserService(mockRepo, nil, srv.URL, turnstile, "admin-pw")

	_, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "bad",
	})
	require.Error(t, err)
	var captchaErr *InvalidCaptchaError
	assert.True(t, errors.As(err, &captchaErr))
}

func TestRequestSignupToken_TurnstileNotConfigured(t *testing.T) {
	mockRepo := new(MockUserRepository)
	service := NewUserService(mockRepo, nil, "http://unused", nil, "admin-pw")

	_, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "tok",
	})
	require.Error(t, err)
	// Misconfiguration must surface as ErrSignupTokenDisabled (→ 503), NOT as a
	// captcha rejection (→ 403). Misconfig is an ops problem, not a user problem.
	assert.ErrorIs(t, err, ErrSignupTokenDisabled)
}

func TestRequestSignupToken_PDSAdmin401(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := &mockTurnstile{}
	pds := startMockPDSAdmin(t, http.StatusUnauthorized)

	service := NewUserService(mockRepo, nil, pds.URL, turnstile, "wrong-pw")

	_, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "tok",
	})
	require.Error(t, err)
	var mintErr *InviteMintError
	assert.True(t, errors.As(err, &mintErr))
	assert.Equal(t, http.StatusUnauthorized, mintErr.StatusCode())
}

func TestRequestSignupToken_PDSAdmin500(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := &mockTurnstile{}
	pds := startMockPDSAdmin(t, http.StatusInternalServerError)

	service := NewUserService(mockRepo, nil, pds.URL, turnstile, "admin-pw")

	_, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "tok",
	})
	require.Error(t, err)
	var mintErr *InviteMintError
	assert.True(t, errors.As(err, &mintErr))
	assert.Equal(t, http.StatusInternalServerError, mintErr.StatusCode())
}

func TestRequestSignupToken_EmptyAdminPassword(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := &mockTurnstile{}
	// Captcha verifier must NOT be called when the endpoint is disabled — we
	// don't want to burn a Turnstile token on a misconfigured server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("PDS admin should not be called when password is empty")
	}))
	defer srv.Close()

	service := NewUserService(mockRepo, nil, srv.URL, turnstile, "")

	_, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "tok",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSignupTokenDisabled)
	assert.False(t, turnstile.called, "turnstile must not run when endpoint is disabled")
}

// PDS returning HTTP 200 with non-JSON body is a real concern (proxies that
// serve a static 200 page on backend failure). Must wrap with the sentinel so
// ops can alert on "PDS is broken" rather than seeing a generic 500.
func TestRequestSignupToken_PDSAdminReturnsMalformedJSON(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := &mockTurnstile{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	t.Cleanup(srv.Close)

	service := NewUserService(mockRepo, nil, srv.URL, turnstile, "admin-pw")

	_, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "tok",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPDSAdminUnavailable),
		"malformed JSON must wrap ErrPDSAdminUnavailable; got %v", err)
}

// 200 OK with an empty code is a PDS misbehavior — surface it as InviteMintError
// so the handler returns 500, not 503. Pins down current behavior so refactors
// can't silently change it.
func TestRequestSignupToken_PDSAdminReturnsEmptyCode(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := &mockTurnstile{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":""}`))
	}))
	t.Cleanup(srv.Close)

	service := NewUserService(mockRepo, nil, srv.URL, turnstile, "admin-pw")

	_, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "tok",
	})
	require.Error(t, err)
	var mintErr *InviteMintError
	require.True(t, errors.As(err, &mintErr), "expected InviteMintError; got %v", err)
	assert.Equal(t, http.StatusOK, mintErr.StatusCode())
	assert.Contains(t, mintErr.Body(), "empty code")
}

// withPDSAdminClient replaces the client the service uses for PDS admin calls.
// Unexported and test-only: it exists so the transport-failure path can be
// exercised deterministically, not as a production knob.
func withPDSAdminClient(c *http.Client) UserServiceOption {
	return func(s *userService) { s.pdsAdminClient = c }
}

// Transport failure (PDS unreachable) must wrap ErrPDSAdminUnavailable so the
// handler maps to 503, not a bare 500.
func TestRequestSignupToken_PDSAdminTransportFailure(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := &mockTurnstile{}

	// The admin call fails in the transport, without a socket: see
	// failingTransport in turnstile_test.go for why this is injected rather than
	// dialled at an unreachable address.
	service := NewUserService(mockRepo, nil, "http://pds.invalid", turnstile, "admin-pw",
		withPDSAdminClient(unreachableClient()))

	_, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
		TurnstileToken: "tok",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPDSAdminUnavailable),
		"transport failure must wrap ErrPDSAdminUnavailable; got %v", err)
}

// Concurrent requests must each mint a distinct code — no caching, no reuse.
// Run under -race to also catch any shared-state mutation in the HTTP client
// path. Confirms the service is safe to call concurrently from many handlers.
func TestRequestSignupToken_ConcurrentRequestsMintDistinctCodes(t *testing.T) {
	mockRepo := new(MockUserRepository)
	turnstile := stubTurnstile{}

	var (
		counter   int64
		counterMu sync.Mutex
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counterMu.Lock()
		counter++
		n := counter
		counterMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"code":"invite-%d"}`, n)))
	}))
	t.Cleanup(srv.Close)

	service := NewUserService(mockRepo, nil, srv.URL, turnstile, "admin-pw")

	const N = 10
	codes := make([]string, N)
	errs := make([]error, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			resp, err := service.RequestSignupToken(context.Background(), RequestSignupTokenRequest{
				TurnstileToken: "tok",
			})
			if err != nil {
				errs[i] = err
				return
			}
			codes[i] = resp.InviteCode
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "goroutine %d failed", i)
	}

	seen := make(map[string]struct{}, N)
	for _, c := range codes {
		assert.NotEmpty(t, c)
		_, dup := seen[c]
		assert.Falsef(t, dup, "duplicate code %q — service must not cache/reuse", c)
		seen[c] = struct{}{}
	}
	assert.Len(t, seen, N)
}
