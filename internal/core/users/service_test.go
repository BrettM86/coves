package users

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/identity"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
	ctx := context.Background()

	err := service.DeleteAccount(ctx, inputDID)
	assert.NoError(t, err)

	mockRepo.AssertExpectations(t)
}

// TestDeleteAccount_InvalidDIDFormat tests deletion with invalid DID format
func TestDeleteAccount_InvalidDIDFormat(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")

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

			service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
	ctx := context.Background()

	profile, err := service.GetProfile(ctx, testDID)
	require.NoError(t, err)

	// Verify basic fields
	assert.Equal(t, testDID, profile.DID)
	assert.Equal(t, "avataruser.test", profile.Handle)
	assert.Equal(t, "Avatar User", profile.DisplayName)
	assert.Equal(t, "Test bio for avatar user", profile.Bio)

	// Verify CID-to-URL transformation
	expectedAvatarURL := "https://test.pds/xrpc/com.atproto.sync.getBlob?did=did:plc:avataruser&cid=bafkreiabc123avatar"
	expectedBannerURL := "https://test.pds/xrpc/com.atproto.sync.getBlob?did=did:plc:avataruser&cid=bafkreixyz789banner"
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
	ctx := context.Background()

	profile, err := service.GetProfile(ctx, testDID)
	require.NoError(t, err)

	// Avatar should be transformed to URL
	expectedAvatarURL := "https://test.pds/xrpc/com.atproto.sync.getBlob?did=did:plc:avataronly&cid=bafkreiavataronly"
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
	ctx := context.Background()

	_, err := service.UpdateProfile(ctx, testDID, input)
	assert.ErrorIs(t, err, ErrUserNotFound)

	mockRepo.AssertExpectations(t)
}

// TestUpdateProfile_EmptyDID tests UpdateProfile with empty DID
func TestUpdateProfile_EmptyDID(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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

	service := NewUserService(mockRepo, mockResolver, "https://default.pds")
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
