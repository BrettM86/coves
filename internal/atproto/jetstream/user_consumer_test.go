package jetstream

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockUserService is a test double for users.UserService
type mockUserService struct {
	users         map[string]*users.User
	updatedCalls  []users.UpdateProfileInput
	updatedDIDs   []string
	shouldFailGet bool
	getError      error
	updateError   error
}

func newMockUserService() *mockUserService {
	return &mockUserService{
		users:        make(map[string]*users.User),
		updatedCalls: []users.UpdateProfileInput{},
		updatedDIDs:  []string{},
	}
}

func (m *mockUserService) CreateUser(ctx context.Context, req users.CreateUserRequest) (*users.User, error) {
	return nil, nil
}

func (m *mockUserService) GetUserByDID(ctx context.Context, did string) (*users.User, error) {
	if m.shouldFailGet {
		return nil, m.getError
	}
	user, exists := m.users[did]
	if !exists {
		return nil, users.ErrUserNotFound
	}
	return user, nil
}

func (m *mockUserService) GetUserByHandle(ctx context.Context, handle string) (*users.User, error) {
	return nil, nil
}

func (m *mockUserService) UpdateHandle(ctx context.Context, did, newHandle string) (*users.User, error) {
	return nil, nil
}

func (m *mockUserService) ResolveHandleToDID(ctx context.Context, handle string) (string, error) {
	return "", nil
}

func (m *mockUserService) RegisterAccount(ctx context.Context, req users.RegisterAccountRequest) (*users.RegisterAccountResponse, error) {
	return nil, nil
}

func (m *mockUserService) RequestSignupToken(ctx context.Context, req users.RequestSignupTokenRequest) (*users.RequestSignupTokenResponse, error) {
	return nil, nil
}

func (m *mockUserService) IndexUser(ctx context.Context, did, handle, pdsURL string) error {
	m.users[did] = &users.User{DID: did, Handle: handle, PDSURL: pdsURL}
	return nil
}

func (m *mockUserService) GetProfile(ctx context.Context, did string) (*users.ProfileViewDetailed, error) {
	return nil, nil
}

func (m *mockUserService) UpdateProfile(ctx context.Context, did string, input users.UpdateProfileInput) (*users.User, error) {
	if m.updateError != nil {
		return nil, m.updateError
	}
	m.updatedCalls = append(m.updatedCalls, input)
	m.updatedDIDs = append(m.updatedDIDs, did)
	user := m.users[did]
	if user == nil {
		return nil, users.ErrUserNotFound
	}
	// Apply updates to mock user
	if input.DisplayName != nil {
		user.DisplayName = *input.DisplayName
	}
	if input.Bio != nil {
		user.Bio = *input.Bio
	}
	if input.AvatarCID != nil {
		user.AvatarCID = *input.AvatarCID
	}
	if input.BannerCID != nil {
		user.BannerCID = *input.BannerCID
	}
	return user, nil
}

func (m *mockUserService) DeleteAccount(ctx context.Context, did string) error {
	return nil
}

// mockIdentityResolverForUser is a test double for identity.Resolver
type mockIdentityResolverForUser struct {
	identities map[string]*identity.Identity
	resolveErr error
}

func (m *mockIdentityResolverForUser) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	if m.resolveErr != nil {
		return nil, m.resolveErr
	}
	return m.identities[identifier], nil
}

func (m *mockIdentityResolverForUser) ResolveHandle(ctx context.Context, handle string) (string, string, error) {
	return "", "", nil
}

func (m *mockIdentityResolverForUser) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	return nil, nil
}

func (m *mockIdentityResolverForUser) Purge(ctx context.Context, identifier string) error {
	return nil
}

func TestUserConsumer_HandleProfileCommit(t *testing.T) {
	t.Run("ignores commits for unknown collections", func(t *testing.T) {
		mockService := newMockUserService()
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		// Event with a non-profile collection (e.g., social.coves.post)
		event := &JetstreamEvent{
			Did:    "did:plc:testuser123",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.post", // Not CovesProfileCollection
				RKey:       "post123",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"text": "Hello world",
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("Expected no error for unknown collection, got: %v", err)
		}

		// Verify no UpdateProfile calls were made
		if len(mockService.updatedCalls) != 0 {
			t.Errorf("Expected 0 UpdateProfile calls, got %d", len(mockService.updatedCalls))
		}
	})

	t.Run("ignores commits for users not in database", func(t *testing.T) {
		mockService := newMockUserService()
		// Don't add any users - the user lookup will fail
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:unknownuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"displayName": "Unknown User",
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		// Should return nil (not an error) for users not in our database
		if err != nil {
			t.Errorf("Expected nil error for unknown user, got: %v", err)
		}

		// Verify no UpdateProfile calls were made
		if len(mockService.updatedCalls) != 0 {
			t.Errorf("Expected 0 UpdateProfile calls, got %d", len(mockService.updatedCalls))
		}
	})

	t.Run("indexes an unknown profile hosted by a trusted bridge", func(t *testing.T) {
		const did = "did:plc:bridgedprofile"
		const bridgePDS = "https://bridge.example"
		mockService := newMockUserService()
		mockResolver := &mockIdentityResolverForUser{identities: map[string]*identity.Identity{
			did: {DID: did, Handle: "alice.lemmy.bridge.example", PDSURL: bridgePDS},
		}}
		consumer := NewUserEventConsumer(mockService, mockResolver,
			WithUserBridgeTrust(NewBridgeTrust([]string{bridgePDS})))

		event := &JetstreamEvent{
			Did: did, Kind: "commit", TimeUS: time.Now().UnixMicro(),
			Commit: &CommitEvent{
				Operation: "create", Collection: CovesProfileCollection, RKey: "self", CID: "bafybridge",
				Record: map[string]interface{}{"displayName": "Alice from Lemmy"},
			},
		}

		if err := consumer.HandleEvent(context.Background(), event); err != nil {
			t.Fatalf("trusted bridge profile failed: %v", err)
		}
		indexed := mockService.users[did]
		if indexed == nil {
			t.Fatal("trusted bridge profile identity was not indexed")
		}
		if indexed.PDSURL != bridgePDS || indexed.DisplayName != "Alice from Lemmy" {
			t.Fatalf("unexpected indexed bridge profile: %+v", indexed)
		}
	})

	t.Run("extracts displayName from record", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:    "did:plc:testuser",
			Handle: "testuser.bsky.social",
			PDSURL: "https://bsky.social",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"displayName": "Test Display Name",
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if len(mockService.updatedCalls) != 1 {
			t.Fatalf("Expected 1 UpdateProfile call, got %d", len(mockService.updatedCalls))
		}

		call := mockService.updatedCalls[0]
		if call.DisplayName == nil || *call.DisplayName != "Test Display Name" {
			t.Errorf("Expected displayName 'Test Display Name', got %v", call.DisplayName)
		}
	})

	t.Run("extracts description (bio) from record", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:    "did:plc:testuser",
			Handle: "testuser.bsky.social",
			PDSURL: "https://bsky.social",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"description": "This is my bio",
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if len(mockService.updatedCalls) != 1 {
			t.Fatalf("Expected 1 UpdateProfile call, got %d", len(mockService.updatedCalls))
		}

		call := mockService.updatedCalls[0]
		if call.Bio == nil || *call.Bio != "This is my bio" {
			t.Errorf("Expected bio 'This is my bio', got %v", call.Bio)
		}
	})

	t.Run("extracts avatar CID from blob ref structure", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:    "did:plc:testuser",
			Handle: "testuser.bsky.social",
			PDSURL: "https://bsky.social",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"avatar": map[string]interface{}{
						"$type":    "blob",
						"ref":      map[string]interface{}{"$link": "bafkavatar123"},
						"mimeType": "image/jpeg",
						"size":     float64(12345),
					},
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if len(mockService.updatedCalls) != 1 {
			t.Fatalf("Expected 1 UpdateProfile call, got %d", len(mockService.updatedCalls))
		}

		call := mockService.updatedCalls[0]
		if call.AvatarCID == nil || *call.AvatarCID != "bafkavatar123" {
			t.Errorf("Expected avatar CID 'bafkavatar123', got %v", call.AvatarCID)
		}
	})

	t.Run("extracts banner CID from blob ref structure", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:    "did:plc:testuser",
			Handle: "testuser.bsky.social",
			PDSURL: "https://bsky.social",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"banner": map[string]interface{}{
						"$type":    "blob",
						"ref":      map[string]interface{}{"$link": "bafkbanner456"},
						"mimeType": "image/png",
						"size":     float64(54321),
					},
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if len(mockService.updatedCalls) != 1 {
			t.Fatalf("Expected 1 UpdateProfile call, got %d", len(mockService.updatedCalls))
		}

		call := mockService.updatedCalls[0]
		if call.BannerCID == nil || *call.BannerCID != "bafkbanner456" {
			t.Errorf("Expected banner CID 'bafkbanner456', got %v", call.BannerCID)
		}
	})

	t.Run("extracts all profile fields together", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:    "did:plc:testuser",
			Handle: "testuser.bsky.social",
			PDSURL: "https://bsky.social",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"displayName": "Full Profile User",
					"description": "A complete bio",
					"avatar": map[string]interface{}{
						"$type":    "blob",
						"ref":      map[string]interface{}{"$link": "bafkfullav123"},
						"mimeType": "image/jpeg",
						"size":     float64(10000),
					},
					"banner": map[string]interface{}{
						"$type":    "blob",
						"ref":      map[string]interface{}{"$link": "bafkfullbn456"},
						"mimeType": "image/png",
						"size":     float64(20000),
					},
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if len(mockService.updatedCalls) != 1 {
			t.Fatalf("Expected 1 UpdateProfile call, got %d", len(mockService.updatedCalls))
		}

		call := mockService.updatedCalls[0]
		if call.DisplayName == nil || *call.DisplayName != "Full Profile User" {
			t.Errorf("Expected displayName 'Full Profile User', got %v", call.DisplayName)
		}
		if call.Bio == nil || *call.Bio != "A complete bio" {
			t.Errorf("Expected bio 'A complete bio', got %v", call.Bio)
		}
		if call.AvatarCID == nil || *call.AvatarCID != "bafkfullav123" {
			t.Errorf("Expected avatar CID 'bafkfullav123', got %v", call.AvatarCID)
		}
		if call.BannerCID == nil || *call.BannerCID != "bafkfullbn456" {
			t.Errorf("Expected banner CID 'bafkfullbn456', got %v", call.BannerCID)
		}
	})

	t.Run("handles delete operation by clearing profile fields", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:         "did:plc:testuser",
			Handle:      "testuser.bsky.social",
			PDSURL:      "https://bsky.social",
			DisplayName: "Existing Name",
			Bio:         "Existing Bio",
			AvatarCID:   "existingavatar",
			BannerCID:   "existingbanner",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "delete",
				Collection: CovesProfileCollection,
				RKey:       "self",
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if len(mockService.updatedCalls) != 1 {
			t.Fatalf("Expected 1 UpdateProfile call, got %d", len(mockService.updatedCalls))
		}

		call := mockService.updatedCalls[0]
		// Delete should pass empty strings to clear fields
		if call.DisplayName == nil || *call.DisplayName != "" {
			t.Errorf("Expected empty displayName for delete, got %v", call.DisplayName)
		}
		if call.Bio == nil || *call.Bio != "" {
			t.Errorf("Expected empty bio for delete, got %v", call.Bio)
		}
		if call.AvatarCID == nil || *call.AvatarCID != "" {
			t.Errorf("Expected empty avatar CID for delete, got %v", call.AvatarCID)
		}
		if call.BannerCID == nil || *call.BannerCID != "" {
			t.Errorf("Expected empty banner CID for delete, got %v", call.BannerCID)
		}
	})

	t.Run("handles update operation same as create", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:         "did:plc:testuser",
			Handle:      "testuser.bsky.social",
			PDSURL:      "https://bsky.social",
			DisplayName: "Old Name",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev124",
				Operation:  "update", // Update operation instead of create
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy456",
				Record: map[string]interface{}{
					"displayName": "Updated Name",
					"description": "Updated bio",
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if len(mockService.updatedCalls) != 1 {
			t.Fatalf("Expected 1 UpdateProfile call, got %d", len(mockService.updatedCalls))
		}

		call := mockService.updatedCalls[0]
		if call.DisplayName == nil || *call.DisplayName != "Updated Name" {
			t.Errorf("Expected displayName 'Updated Name', got %v", call.DisplayName)
		}
		if call.Bio == nil || *call.Bio != "Updated bio" {
			t.Errorf("Expected bio 'Updated bio', got %v", call.Bio)
		}
	})

	t.Run("propagates database errors from GetUserByDID", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.shouldFailGet = true
		mockService.getError = errors.New("database connection error")
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"displayName": "Test User",
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Fatal("Expected error for database failure, got nil")
		}
		if !errors.Is(err, mockService.getError) && err.Error() != "failed to check if user exists: database connection error" {
			t.Errorf("Expected wrapped database error, got: %v", err)
		}
	})

	t.Run("handles nil commit gracefully", func(t *testing.T) {
		mockService := newMockUserService()
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: nil, // No commit data
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("Expected no error for nil commit, got: %v", err)
		}
	})

	t.Run("handles nil record in commit gracefully", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:    "did:plc:testuser",
			Handle: "testuser.bsky.social",
			PDSURL: "https://bsky.social",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record:     nil, // No record data
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("Expected no error for nil record, got: %v", err)
		}
	})

	t.Run("handles invalid blob structure gracefully", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:    "did:plc:testuser",
			Handle: "testuser.bsky.social",
			PDSURL: "https://bsky.social",
		}
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"displayName": "Test User",
					"avatar": map[string]interface{}{
						"$type": "not-a-blob", // Invalid type
					},
					"banner": "not-a-map", // Invalid structure
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if len(mockService.updatedCalls) != 1 {
			t.Fatalf("Expected 1 UpdateProfile call, got %d", len(mockService.updatedCalls))
		}

		call := mockService.updatedCalls[0]
		// displayName should be extracted
		if call.DisplayName == nil || *call.DisplayName != "Test User" {
			t.Errorf("Expected displayName 'Test User', got %v", call.DisplayName)
		}
		// Avatar and banner should be nil (not extracted due to invalid structure)
		if call.AvatarCID != nil {
			t.Errorf("Expected nil avatar CID for invalid blob, got %v", call.AvatarCID)
		}
		if call.BannerCID != nil {
			t.Errorf("Expected nil banner CID for invalid structure, got %v", call.BannerCID)
		}
	})
}

func TestUserConsumer_PropagatesUpdateProfileError(t *testing.T) {
	t.Run("propagates_database_errors_from_UpdateProfile", func(t *testing.T) {
		mockService := newMockUserService()
		mockService.users["did:plc:testuser"] = &users.User{
			DID:    "did:plc:testuser",
			Handle: "testuser.bsky.social",
			PDSURL: "https://bsky.social",
		}
		mockService.updateError = errors.New("database write error")
		mockResolver := &mockIdentityResolverForUser{}
		consumer := NewUserEventConsumer(mockService, mockResolver)
		ctx := context.Background()

		event := &JetstreamEvent{
			Did:    "did:plc:testuser",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafy123",
				Record: map[string]interface{}{
					"displayName": "Test User",
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Fatal("Expected error for UpdateProfile failure, got nil")
		}
		if !errors.Is(err, mockService.updateError) && err.Error() != "failed to update user profile: database write error" {
			t.Errorf("Expected wrapped database error, got: %v", err)
		}
	})
}

func TestExtractBlobCID(t *testing.T) {
	t.Run("extracts CID from valid blob structure", func(t *testing.T) {
		blob := map[string]interface{}{
			"$type":    "blob",
			"ref":      map[string]interface{}{"$link": "bafktest123"},
			"mimeType": "image/jpeg",
			"size":     float64(12345),
		}

		cid, ok := extractBlobCID(blob)
		if !ok {
			t.Fatal("Expected successful extraction")
		}
		if cid != "bafktest123" {
			t.Errorf("Expected CID 'bafktest123', got '%s'", cid)
		}
	})

	t.Run("returns false for nil blob", func(t *testing.T) {
		cid, ok := extractBlobCID(nil)
		if ok {
			t.Error("Expected false for nil blob")
		}
		if cid != "" {
			t.Errorf("Expected empty CID for nil blob, got '%s'", cid)
		}
	})

	t.Run("returns false for wrong $type", func(t *testing.T) {
		blob := map[string]interface{}{
			"$type": "image",
			"ref":   map[string]interface{}{"$link": "bafktest123"},
		}

		cid, ok := extractBlobCID(blob)
		if ok {
			t.Error("Expected false for wrong $type")
		}
		if cid != "" {
			t.Errorf("Expected empty CID for wrong type, got '%s'", cid)
		}
	})

	t.Run("returns false for missing $type", func(t *testing.T) {
		blob := map[string]interface{}{
			"ref": map[string]interface{}{"$link": "bafktest123"},
		}

		cid, ok := extractBlobCID(blob)
		if ok {
			t.Error("Expected false for missing $type")
		}
		if cid != "" {
			t.Errorf("Expected empty CID for missing type, got '%s'", cid)
		}
	})

	t.Run("returns false for missing ref", func(t *testing.T) {
		blob := map[string]interface{}{
			"$type": "blob",
		}

		cid, ok := extractBlobCID(blob)
		if ok {
			t.Error("Expected false for missing ref")
		}
		if cid != "" {
			t.Errorf("Expected empty CID for missing ref, got '%s'", cid)
		}
	})

	t.Run("returns false for missing $link", func(t *testing.T) {
		blob := map[string]interface{}{
			"$type": "blob",
			"ref":   map[string]interface{}{},
		}

		cid, ok := extractBlobCID(blob)
		if ok {
			t.Error("Expected false for missing $link")
		}
		if cid != "" {
			t.Errorf("Expected empty CID for missing link, got '%s'", cid)
		}
	})

	t.Run("returns false for non-map ref", func(t *testing.T) {
		blob := map[string]interface{}{
			"$type": "blob",
			"ref":   "not-a-map",
		}

		cid, ok := extractBlobCID(blob)
		if ok {
			t.Error("Expected false for non-map ref")
		}
		if cid != "" {
			t.Errorf("Expected empty CID for non-map ref, got '%s'", cid)
		}
	})
}

// TestUserConsumer_BlockEventsTheConsumerDeclinesToIndex covers the two block
// paths that write nothing and must still succeed.
//
// Both are invisible to the indexing tests in user_consumer_block_test.go,
// which assert on rows: "no row" is also what a broken consumer produces. Here
// the repository panics if it is touched at all, so a regression that started
// indexing these would fail loudly instead of quietly.
func TestUserConsumer_BlockEventsTheConsumerDeclinesToIndex(t *testing.T) {
	blockEvent := func(operation string) *JetstreamEvent {
		return &JetstreamEvent{
			Did: "did:plc:blockdecliner", Kind: "commit", TimeUS: time.Now().UnixMicro(),
			Commit: &CommitEvent{
				Operation: operation, Collection: CovesActorBlockCollection,
				RKey: "d1", CID: "bafydecline",
				Record: map[string]interface{}{
					"$type":     CovesActorBlockCollection,
					"subject":   "did:plc:blockdeclined",
					"createdAt": "2026-01-01T00:00:00Z",
				},
			},
		}
	}

	t.Run("no block repository configured", func(t *testing.T) {
		// WithUserBlockRepo is optional, and an AppView built without it (a
		// deployment that does not index blocks, or a wiring mistake) must drop
		// block events rather than nil-panic the whole users consumer on the
		// first block anybody performs.
		consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{})

		require.NoError(t, consumer.HandleEvent(context.Background(), blockEvent("create")))
	})

	t.Run("an update operation is ignored", func(t *testing.T) {
		// Block records have no mutable fields, so no client should ever emit
		// one — but a federated repo can put whatever it likes on the wire.
		// Treating an update as a create would let a remote repo rewrite an
		// existing block's subject; treating it as an error would dead-letter
		// junk that will never become valid.
		consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
			WithUserBlockRepo(failingUserBlockRepoStub{}))

		require.NoError(t, consumer.HandleEvent(context.Background(), blockEvent("update")))
	})
}

// fakeSessionHandleUpdater records the OAuth-session fan-out the identity path
// triggers, and can fail on demand.
type fakeSessionHandleUpdater struct {
	calls []struct{ did, handle string }
	err   error
}

func (f *fakeSessionHandleUpdater) UpdateHandleByDID(_ context.Context, did, newHandle string) (int64, error) {
	f.calls = append(f.calls, struct{ did, handle string }{did, newHandle})
	if f.err != nil {
		return 0, f.err
	}
	return int64(len(f.calls)), nil
}

// TestUserConsumer_IdentityEvent_SyncsSessionHandles covers the consumer's half
// of the handle-rename fan-out: that an identity event for a known user whose
// handle really changed reaches SessionHandleUpdater, and that nothing else
// does.
//
// The store's half — which sessions the fan-out touches — is
// TestPostgresOAuthStore_UpdateHandleByDID in internal/atproto/oauth. Together
// they replace tests/integration/oauth_session_handle_sync_test.go, which
// proved both against real Postgres in 370 lines and paid for a live websocket
// dial and a vacuous three-t.Log "E2E" test function to do it.
//
// Kept as a unit test rather than folded into the store's: the interesting
// cases here are the ones where the updater must NOT be called, and those are
// invisible when the collaborator is real.
func TestUserConsumer_IdentityEvent_SyncsSessionHandles(t *testing.T) {
	const did = "did:plc:identitysyncuser"

	identityEvent := func(handle string) *JetstreamEvent {
		return &JetstreamEvent{Kind: "identity", Did: did, Identity: &IdentityEvent{Did: did, Handle: handle}}
	}

	newConsumer := func(currentHandle string) (*UserEventConsumer, *fakeSessionHandleUpdater, *mockUserService) {
		service := newMockUserService()
		service.users[did] = &users.User{DID: did, Handle: currentHandle}
		updater := &fakeSessionHandleUpdater{}
		return NewUserEventConsumer(service, &mockIdentityResolverForUser{},
			WithSessionHandleUpdater(updater)), updater, service
	}

	t.Run("a real rename fans out to the sessions", func(t *testing.T) {
		consumer, updater, _ := newConsumer("old.example.com")

		require.NoError(t, consumer.HandleEvent(context.Background(), identityEvent("new.example.com")))

		require.Len(t, updater.calls, 1,
			"a handle change must reach the OAuth sessions, or every device the user is signed "+
				"in on keeps showing the old handle until its session expires")
		assert.Equal(t, did, updater.calls[0].did)
		assert.Equal(t, "new.example.com", updater.calls[0].handle,
			"the sessions must be given the NEW handle, not the one they already hold")
	})

	t.Run("an unchanged handle does not touch the sessions", func(t *testing.T) {
		// Identity events are re-emitted for reasons other than renames (a
		// rotation key change, a PDS migration), and they arrive on an
		// unfiltered stream. Writing every session row on each of them would be
		// a steady stream of pointless UPDATEs against a hot table.
		consumer, updater, _ := newConsumer("same.example.com")

		require.NoError(t, consumer.HandleEvent(context.Background(), identityEvent("same.example.com")))
		assert.Empty(t, updater.calls)
	})

	t.Run("an unknown user does not touch the sessions", func(t *testing.T) {
		// The consumer indexes only identities it has seen ("this prevents us
		// from indexing millions of Bluesky users we don't care about"), and the
		// session store cannot hold a session for one of them anyway.
		service := newMockUserService()
		updater := &fakeSessionHandleUpdater{}
		consumer := NewUserEventConsumer(service, &mockIdentityResolverForUser{},
			WithSessionHandleUpdater(updater))

		require.NoError(t, consumer.HandleEvent(context.Background(),
			&JetstreamEvent{Kind: "identity", Did: "did:plc:strangernobodyknows",
				Identity: &IdentityEvent{Did: "did:plc:strangernobodyknows", Handle: "stranger.example.com"}}))
		assert.Empty(t, updater.calls)
	})

	t.Run("a failed fan-out is logged, not returned", func(t *testing.T) {
		// PINS A DELIBERATE SILENT FAILURE, which is worth doing precisely
		// because silent failures are usually bugs and this one is a choice.
		//
		// The users row has already been updated and the identity cache already
		// purged by the time the fan-out runs. Returning the error here would
		// dead-letter the event and, on redrive, re-run those two steps for a
		// rename that has already been applied — to fix a stale string in a
		// session row that expires on its own. The cost of the failure is a user
		// seeing their old handle in one client; the cost of treating it as
		// fatal is a consumer that stops.
		consumer, updater, _ := newConsumer("old.example.com")
		updater.err = errors.New("the session store is unreachable")

		require.NoError(t, consumer.HandleEvent(context.Background(), identityEvent("new.example.com")),
			"a session-store failure must not fail the identity event: the handle change itself "+
				"has already been applied and a redrive would only repeat it")
		require.Len(t, updater.calls, 1, "the fan-out was attempted")
	})

	t.Run("a consumer with no updater configured does not panic", func(t *testing.T) {
		// The option is optional, and a nil interface value dereferenced here
		// would take down the whole users consumer on the first rename anybody
		// performed.
		service := newMockUserService()
		service.users[did] = &users.User{DID: did, Handle: "old.example.com"}
		consumer := NewUserEventConsumer(service, &mockIdentityResolverForUser{})

		require.NoError(t, consumer.HandleEvent(context.Background(), identityEvent("new.example.com")))
	})
}
