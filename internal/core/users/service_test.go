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
