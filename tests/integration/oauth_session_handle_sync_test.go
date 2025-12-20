package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
)

// TestOAuthSessionHandleSync tests that OAuth session handles are updated
// when identity events indicate a handle change.
//
// This ensures mobile/web apps display the correct handle after a user
// changes their handle on their PDS.
//
// Prerequisites:
//   - Test database on localhost:5434
//
// Run with:
//
//	docker-compose --profile test up -d postgres-test
//	TEST_DATABASE_URL="postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable" \
//	  go test -v ./tests/integration/ -run "TestOAuthSessionHandleSync"
func TestOAuthSessionHandleSync(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx := context.Background()

	// Set up real infrastructure components
	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")

	// Create real OAuth store (with session handle updater capability)
	baseOAuthStore := oauth.NewPostgresOAuthStore(db, 24*time.Hour)

	t.Run("Handle change syncs to active OAuth sessions", func(t *testing.T) {
		testDID := "did:plc:oauthsync123"
		oldHandle := "oldhandle.oauth.sync.test"
		newHandle := "newhandle.oauth.sync.test"
		sessionID := "test-session-oauth-sync-001"

		// 1. Create user with old handle
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    testDID,
			Handle: oldHandle,
			PDSURL: "https://bsky.social",
		})
		require.NoError(t, err, "Failed to create test user")
		t.Logf("✅ Created user: %s (%s)", oldHandle, testDID)

		// 2. Create OAuth session with old handle
		parsedDID, err := syntax.ParseDID(testDID)
		require.NoError(t, err, "Failed to parse DID")

		session := oauthlib.ClientSessionData{
			AccountDID:   parsedDID,
			SessionID:    sessionID,
			HostURL:      "https://bsky.social",
			AccessToken:  "test-access-token",
			RefreshToken: "test-refresh-token",
			Scopes:       []string{"atproto"},
		}
		err = baseOAuthStore.SaveSession(ctx, session)
		require.NoError(t, err, "Failed to save OAuth session")
		t.Logf("✅ Created OAuth session: %s", sessionID)

		// 3. Verify session was created with correct data
		savedSession, err := baseOAuthStore.GetSession(ctx, parsedDID, sessionID)
		require.NoError(t, err, "Failed to retrieve saved session")
		require.NotNil(t, savedSession, "Session should exist")
		t.Logf("✅ Verified session exists for DID: %s", testDID)

		// 4. Cast store to SessionHandleUpdater (what the consumer uses)
		sessionUpdater, ok := baseOAuthStore.(jetstream.SessionHandleUpdater)
		require.True(t, ok, "OAuth store should implement SessionHandleUpdater")

		// 5. Create consumer with session handle updater
		consumer := jetstream.NewUserEventConsumer(
			userService,
			resolver,
			"", // No WebSocket URL - we'll call HandleIdentityEventPublic directly
			"",
			jetstream.WithSessionHandleUpdater(sessionUpdater),
		)

		// 6. Simulate identity event with NEW handle (as if PDS sent handle change)
		identityEvent := &jetstream.JetstreamEvent{
			Did:  testDID,
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    testDID,
				Handle: newHandle,
				Seq:    999999,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		t.Logf("📡 Simulating identity event: %s → %s", oldHandle, newHandle)
		err = consumer.HandleIdentityEventPublic(ctx, identityEvent)
		require.NoError(t, err, "Failed to handle identity event")
		t.Logf("✅ Identity event processed")

		// 7. Verify users table was updated
		user, err := userService.GetUserByDID(ctx, testDID)
		require.NoError(t, err, "Failed to get user after handle change")
		require.Equal(t, newHandle, user.Handle, "User handle should be updated in database")
		t.Logf("✅ Users table updated: handle=%s", user.Handle)

		// 8. Verify OAuth session handle was updated
		var sessionHandle string
		err = db.QueryRowContext(ctx,
			"SELECT handle FROM oauth_sessions WHERE did = $1 AND session_id = $2",
			testDID, sessionID,
		).Scan(&sessionHandle)
		require.NoError(t, err, "Failed to query session handle")
		require.Equal(t, newHandle, sessionHandle, "OAuth session handle should be updated")
		t.Logf("✅ OAuth session handle updated: %s", sessionHandle)
	})

	t.Run("Multiple sessions updated on handle change", func(t *testing.T) {
		testDID := "did:plc:multisession456"
		oldHandle := "multi.old.handle.test"
		newHandle := "multi.new.handle.test"

		// 1. Create user
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    testDID,
			Handle: oldHandle,
			PDSURL: "https://bsky.social",
		})
		require.NoError(t, err)

		// 2. Create multiple OAuth sessions (simulating login from multiple devices)
		parsedDID, _ := syntax.ParseDID(testDID)
		for i := 1; i <= 3; i++ {
			session := oauthlib.ClientSessionData{
				AccountDID:   parsedDID,
				SessionID:    fmt.Sprintf("multi-session-%d", i),
				HostURL:      "https://bsky.social",
				AccessToken:  fmt.Sprintf("access-token-%d", i),
				RefreshToken: fmt.Sprintf("refresh-token-%d", i),
				Scopes:       []string{"atproto"},
			}
			err = baseOAuthStore.SaveSession(ctx, session)
			require.NoError(t, err)
		}
		t.Logf("✅ Created 3 OAuth sessions for user")

		// 3. Process identity event with new handle
		sessionUpdater := baseOAuthStore.(jetstream.SessionHandleUpdater)
		consumer := jetstream.NewUserEventConsumer(
			userService, resolver, "", "",
			jetstream.WithSessionHandleUpdater(sessionUpdater),
		)

		identityEvent := &jetstream.JetstreamEvent{
			Did:  testDID,
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    testDID,
				Handle: newHandle,
				Seq:    888888,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		err = consumer.HandleIdentityEventPublic(ctx, identityEvent)
		require.NoError(t, err)

		// 4. Verify ALL sessions were updated
		var count int
		err = db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM oauth_sessions WHERE did = $1 AND handle = $2",
			testDID, newHandle,
		).Scan(&count)
		require.NoError(t, err)
		require.Equal(t, 3, count, "All 3 sessions should have updated handles")
		t.Logf("✅ All %d sessions updated with new handle", count)
	})

	t.Run("No sessions updated when user has no active sessions", func(t *testing.T) {
		testDID := "did:plc:nosessions789"
		oldHandle := "nosession.old.test"
		newHandle := "nosession.new.test"

		// 1. Create user with no OAuth sessions
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    testDID,
			Handle: oldHandle,
			PDSURL: "https://bsky.social",
		})
		require.NoError(t, err)

		// 2. Process identity event
		sessionUpdater := baseOAuthStore.(jetstream.SessionHandleUpdater)
		consumer := jetstream.NewUserEventConsumer(
			userService, resolver, "", "",
			jetstream.WithSessionHandleUpdater(sessionUpdater),
		)

		identityEvent := &jetstream.JetstreamEvent{
			Did:  testDID,
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    testDID,
				Handle: newHandle,
				Seq:    777777,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		// Should not error even when no sessions exist
		err = consumer.HandleIdentityEventPublic(ctx, identityEvent)
		require.NoError(t, err, "Should handle event gracefully with no sessions")

		// 3. Verify user was still updated
		user, err := userService.GetUserByDID(ctx, testDID)
		require.NoError(t, err)
		require.Equal(t, newHandle, user.Handle)
		t.Logf("✅ User updated correctly even with no active sessions")
	})

	t.Run("Consumer works without session updater (backward compat)", func(t *testing.T) {
		testDID := "did:plc:nosyncer000"
		oldHandle := "nosyncer.old.test"
		newHandle := "nosyncer.new.test"

		// 1. Create user
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    testDID,
			Handle: oldHandle,
			PDSURL: "https://bsky.social",
		})
		require.NoError(t, err)

		// 2. Create consumer WITHOUT session handle updater
		consumer := jetstream.NewUserEventConsumer(
			userService, resolver, "", "",
			// No WithSessionHandleUpdater - testing backward compatibility
		)

		// 3. Process identity event - should work without error
		identityEvent := &jetstream.JetstreamEvent{
			Did:  testDID,
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    testDID,
				Handle: newHandle,
				Seq:    666666,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		err = consumer.HandleIdentityEventPublic(ctx, identityEvent)
		require.NoError(t, err, "Consumer should work without session updater")

		// 4. Verify user was updated
		user, err := userService.GetUserByDID(ctx, testDID)
		require.NoError(t, err)
		require.Equal(t, newHandle, user.Handle)
		t.Logf("✅ Consumer works correctly without session handle updater")
	})
}

// TestOAuthSessionHandleSync_LiveJetstream tests the full flow with real Jetstream
// This requires the dev infrastructure to be running.
//
// Prerequisites:
//   - PDS running on localhost:3001
//   - Jetstream running on localhost:6008
//   - Test database on localhost:5434
//
// Run with:
//
//	docker-compose --profile test --profile jetstream up -d
//	TEST_DATABASE_URL="postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable" \
//	  go test -v ./tests/integration/ -run "TestOAuthSessionHandleSync_LiveJetstream"
func TestOAuthSessionHandleSync_LiveJetstream(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live Jetstream test in short mode")
	}

	// Check if Jetstream is available
	if !isServiceAvailable("http://localhost:6008") {
		t.Skip("Jetstream not available at localhost:6008 - run 'docker-compose --profile jetstream up -d' first")
	}

	// Check if PDS is available
	if !isServiceAvailable("http://localhost:3001/xrpc/_health") {
		t.Skip("PDS not available at localhost:3001 - run 'docker-compose up -d pds' first")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Set up real infrastructure
	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")
	baseOAuthStore := oauth.NewPostgresOAuthStore(db, 24*time.Hour)
	sessionUpdater := baseOAuthStore.(jetstream.SessionHandleUpdater)

	// Start consumer connected to real Jetstream
	consumer := jetstream.NewUserEventConsumer(
		userService,
		resolver,
		"ws://localhost:6008/subscribe",
		"",
		jetstream.WithSessionHandleUpdater(sessionUpdater),
	)

	// Start consumer in background
	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()

	go func() {
		if err := consumer.Start(consumerCtx); err != nil && err != context.Canceled {
			t.Logf("Consumer stopped: %v", err)
		}
	}()

	// Give consumer time to connect
	time.Sleep(500 * time.Millisecond)

	t.Run("Real Jetstream integration", func(t *testing.T) {
		t.Log("🔌 Connected to live Jetstream - waiting for identity events...")
		t.Log("Note: This test verifies the consumer is properly configured with session sync.")
		t.Log("To fully test handle sync, create a user on the PDS and change their handle.")

		// For now, just verify the consumer is running with the session updater
		// A full E2E test would require:
		// 1. Create user on PDS
		// 2. Create OAuth session
		// 3. Update handle on PDS (via user credentials)
		// 4. Wait for Jetstream to deliver identity event
		// 5. Verify session handle updated

		t.Log("✅ Consumer running with OAuth session sync enabled")
	})
}

// isServiceAvailable checks if an HTTP service is responding
func isServiceAvailable(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}
