package integration

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"context"
	"testing"
	"time"
)

func TestUserIndexingFromJetstream(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Wire up dependencies
	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")

	ctx := context.Background()

	t.Run("Index new user from identity event", func(t *testing.T) {
		// Simulate an identity event from Jetstream
		event := jetstream.JetstreamEvent{
			Did:  "did:plc:jetstream123",
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "did:plc:jetstream123",
				Handle: "alice.jetstream.test",
				Seq:    12345,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		consumer := jetstream.NewUserEventConsumer(userService, resolver, "", "")

		// Handle the event
		err := consumer.HandleIdentityEventPublic(ctx, &event)
		if err != nil {
			t.Fatalf("failed to handle identity event: %v", err)
		}

		// Verify user was indexed
		user, err := userService.GetUserByDID(ctx, "did:plc:jetstream123")
		if err != nil {
			t.Fatalf("failed to get indexed user: %v", err)
		}

		if user.DID != "did:plc:jetstream123" {
			t.Errorf("expected DID did:plc:jetstream123, got %s", user.DID)
		}

		if user.Handle != "alice.jetstream.test" {
			t.Errorf("expected handle alice.jetstream.test, got %s", user.Handle)
		}
	})

	t.Run("Idempotent indexing - duplicate event", func(t *testing.T) {
		// Create a user first
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    "did:plc:duplicate123",
			Handle: "duplicate.test",
			PDSURL: "https://bsky.social",
		})
		if err != nil {
			t.Fatalf("failed to create initial user: %v", err)
		}

		// Simulate duplicate identity event
		event := jetstream.JetstreamEvent{
			Did:  "did:plc:duplicate123",
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "did:plc:duplicate123",
				Handle: "duplicate.test",
				Seq:    12346,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		consumer := jetstream.NewUserEventConsumer(userService, resolver, "", "")

		// Handle duplicate event - should not error
		err = consumer.HandleIdentityEventPublic(ctx, &event)
		if err != nil {
			t.Fatalf("duplicate event should be handled gracefully: %v", err)
		}

		// Verify still only one user
		user, err := userService.GetUserByDID(ctx, "did:plc:duplicate123")
		if err != nil {
			t.Fatalf("failed to get user: %v", err)
		}

		if user.Handle != "duplicate.test" {
			t.Errorf("expected handle duplicate.test, got %s", user.Handle)
		}
	})

	t.Run("Index multiple users", func(t *testing.T) {
		consumer := jetstream.NewUserEventConsumer(userService, resolver, "", "")

		users := []struct {
			did    string
			handle string
		}{
			{"did:plc:multi1", "user1.test"},
			{"did:plc:multi2", "user2.test"},
			{"did:plc:multi3", "user3.test"},
		}

		for _, u := range users {
			event := jetstream.JetstreamEvent{
				Did:  u.did,
				Kind: "identity",
				Identity: &jetstream.IdentityEvent{
					Did:    u.did,
					Handle: u.handle,
					Seq:    12345,
					Time:   time.Now().Format(time.RFC3339),
				},
			}

			err := consumer.HandleIdentityEventPublic(ctx, &event)
			if err != nil {
				t.Fatalf("failed to index user %s: %v", u.handle, err)
			}
		}

		// Verify all users indexed
		for _, u := range users {
			user, err := userService.GetUserByDID(ctx, u.did)
			if err != nil {
				t.Fatalf("user %s not found: %v", u.did, err)
			}

			if user.Handle != u.handle {
				t.Errorf("expected handle %s, got %s", u.handle, user.Handle)
			}
		}
	})

	t.Run("Skip invalid events", func(t *testing.T) {
		consumer := jetstream.NewUserEventConsumer(userService, resolver, "", "")

		// Missing DID
		invalidEvent1 := jetstream.JetstreamEvent{
			Did:  "",
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "",
				Handle: "invalid.test",
				Seq:    12345,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		err := consumer.HandleIdentityEventPublic(ctx, &invalidEvent1)
		if err == nil {
			t.Error("expected error for missing DID, got nil")
		}

		// Missing handle
		invalidEvent2 := jetstream.JetstreamEvent{
			Did:  "did:plc:invalid",
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "did:plc:invalid",
				Handle: "",
				Seq:    12345,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		err = consumer.HandleIdentityEventPublic(ctx, &invalidEvent2)
		if err == nil {
			t.Error("expected error for missing handle, got nil")
		}

		// Missing identity data
		invalidEvent3 := jetstream.JetstreamEvent{
			Did:      "did:plc:invalid2",
			Kind:     "identity",
			Identity: nil,
		}

		err = consumer.HandleIdentityEventPublic(ctx, &invalidEvent3)
		if err == nil {
			t.Error("expected error for nil identity data, got nil")
		}
	})

	t.Run("Handle change updates database and purges cache", func(t *testing.T) {
		testID := "handlechange"
		oldHandle := "old." + testID + ".test"
		newHandle := "new." + testID + ".test"
		did := "did:plc:" + testID

		// 1. Create user with old handle
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    did,
			Handle: oldHandle,
			PDSURL: "https://bsky.social",
		})
		if err != nil {
			t.Fatalf("failed to create initial user: %v", err)
		}

		// 2. Manually cache the identity (simulate a previous resolution)
		cache := identity.NewPostgresCache(db, 24*time.Hour)
		err = cache.Set(ctx, &identity.Identity{
			DID:        did,
			Handle:     oldHandle,
			PDSURL:     "https://bsky.social",
			Method:     identity.MethodDNS,
			ResolvedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("failed to cache identity: %v", err)
		}

		// 3. Verify cached (both handle and DID should be cached)
		cachedByHandle, err := cache.Get(ctx, oldHandle)
		if err != nil {
			t.Fatalf("expected old handle to be cached, got error: %v", err)
		}
		if cachedByHandle.DID != did {
			t.Errorf("expected cached DID %s, got %s", did, cachedByHandle.DID)
		}

		cachedByDID, err := cache.Get(ctx, did)
		if err != nil {
			t.Fatalf("expected DID to be cached, got error: %v", err)
		}
		if cachedByDID.Handle != oldHandle {
			t.Errorf("expected cached handle %s, got %s", oldHandle, cachedByDID.Handle)
		}

		// 4. Send identity event with NEW handle
		event := jetstream.JetstreamEvent{
			Did:  did,
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    did,
				Handle: newHandle,
				Seq:    99999,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		consumer := jetstream.NewUserEventConsumer(userService, resolver, "", "")
		err = consumer.HandleIdentityEventPublic(ctx, &event)
		if err != nil {
			t.Fatalf("failed to handle handle change event: %v", err)
		}

		// 5. Verify database updated
		user, err := userService.GetUserByDID(ctx, did)
		if err != nil {
			t.Fatalf("failed to get user by DID: %v", err)
		}
		if user.Handle != newHandle {
			t.Errorf("expected database to have new handle %s, got %s", newHandle, user.Handle)
		}

		// 6. Verify old handle purged from cache
		_, err = cache.Get(ctx, oldHandle)
		if err == nil {
			t.Error("expected old handle to be purged from cache, but it's still cached")
		}
		if _, isCacheMiss := err.(*identity.ErrCacheMiss); !isCacheMiss {
			t.Errorf("expected ErrCacheMiss for old handle, got: %v", err)
		}

		// 7. Verify DID cache purged
		_, err = cache.Get(ctx, did)
		if err == nil {
			t.Error("expected DID to be purged from cache, but it's still cached")
		}
		if _, isCacheMiss := err.(*identity.ErrCacheMiss); !isCacheMiss {
			t.Errorf("expected ErrCacheMiss for DID, got: %v", err)
		}

		// 8. Verify user can be found by new handle
		userByHandle, err := userService.GetUserByHandle(ctx, newHandle)
		if err != nil {
			t.Fatalf("failed to get user by new handle: %v", err)
		}
		if userByHandle.DID != did {
			t.Errorf("expected DID %s when looking up by new handle, got %s", did, userByHandle.DID)
		}
	})
}

func TestUserServiceIdempotency(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")
	ctx := context.Background()

	t.Run("CreateUser is idempotent for duplicate DID", func(t *testing.T) {
		req := users.CreateUserRequest{
			DID:    "did:plc:idempotent123",
			Handle: "idempotent.test",
			PDSURL: "https://bsky.social",
		}

		// First creation
		user1, err := userService.CreateUser(ctx, req)
		if err != nil {
			t.Fatalf("first creation failed: %v", err)
		}

		// Second creation with same DID - should return existing user, not error
		user2, err := userService.CreateUser(ctx, req)
		if err != nil {
			t.Fatalf("second creation should be idempotent: %v", err)
		}

		if user1.DID != user2.DID {
			t.Errorf("expected same DID, got %s and %s", user1.DID, user2.DID)
		}

		if user1.CreatedAt != user2.CreatedAt {
			t.Errorf("expected same user (same created_at), got different timestamps")
		}
	})

	t.Run("CreateUser fails for duplicate handle with different DID", func(t *testing.T) {
		// Create first user
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    "did:plc:handleconflict1",
			Handle: "conflicting.handle",
			PDSURL: "https://bsky.social",
		})
		if err != nil {
			t.Fatalf("first creation failed: %v", err)
		}

		// Try to create different user with same handle
		_, err = userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    "did:plc:handleconflict2",
			Handle: "conflicting.handle", // Same handle, different DID
			PDSURL: "https://bsky.social",
		})

		if err == nil {
			t.Fatal("expected error for duplicate handle, got nil")
		}

		if !contains(err.Error(), "handle already taken") {
			t.Errorf("expected 'handle already taken' error, got: %v", err)
		}
	})
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && anySubstring(s, substr)
}

func anySubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
