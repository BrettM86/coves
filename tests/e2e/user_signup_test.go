package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// TestE2E_UserSignup tests the full user signup flow:
// Third-party client → social.coves.actor.signup XRPC → PDS account creation → Jetstream → AppView indexing
//
// This tests the same code path that a third-party client or UI would use.
//
// Prerequisites:
//   - AppView running on localhost:8081
//   - PDS running on localhost:3001
//   - Jetstream running on localhost:6008 (consuming from PDS)
//   - Test database on localhost:5434
//
// Run with:
//   make e2e-up  # Start infrastructure
//   go run ./cmd/server &  # Start AppView
//   go test ./tests/integration -run TestE2E_UserSignup -v
func TestE2E_UserSignup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Check if AppView is available
	if !isAppViewAvailable(t) {
		t.Skip("AppView not available at localhost:8081 - run 'go run ./cmd/server' first")
	}

	// Check if PDS is available
	if !isPDSAvailable(t) {
		t.Skip("PDS not available at localhost:3001 - run 'make e2e-up' first")
	}

	// Check if Jetstream is available
	if !isJetstreamAvailable(t) {
		t.Skip("Jetstream not available at localhost:6008 - run 'make e2e-up' first")
	}

	db := setupTestDB(t)
	defer db.Close()

	// Set up services
	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")

	// Start Jetstream consumer
	consumer := jetstream.NewUserEventConsumer(
		userService,
		resolver,
		"ws://localhost:6008/subscribe",
		"", // No PDS filter
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start consumer in background
	consumerDone := make(chan error, 1)
	go func() {
		consumerDone <- consumer.Start(ctx)
	}()

	// Give Jetstream consumer a moment to connect (no need to wait long)
	t.Log("Waiting for Jetstream consumer to connect...")
	time.Sleep(500 * time.Millisecond)

	// Test 1: Create account on PDS
	t.Run("Create account on PDS and verify indexing", func(t *testing.T) {
		handle := fmt.Sprintf("alice-%d.local.coves.dev", time.Now().Unix())
		email := fmt.Sprintf("alice-%d@test.com", time.Now().Unix())

		t.Logf("Creating account: %s", handle)

		// Create account via UserService (what UI would call)
		did, err := createPDSAccount(t, userService, handle, email, "test1234")
		if err != nil {
			t.Fatalf("Failed to create PDS account: %v", err)
		}

		t.Logf("Account created with DID: %s", did)

		// Wait for Jetstream to process and AppView to index (with retry)
		t.Log("Waiting for Jetstream → AppView indexing...")
		var user *users.User
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			user, err = userService.GetUserByDID(ctx, did)
			if err == nil {
				break // Successfully indexed!
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("User not indexed in AppView after 10s: %v", err)
		}

		if user.Handle != handle {
			t.Errorf("Expected handle %s, got %s", handle, user.Handle)
		}

		if user.DID != did {
			t.Errorf("Expected DID %s, got %s", did, user.DID)
		}

		t.Logf("✅ User successfully indexed: %s → %s", handle, did)
	})

	// Test 2: Idempotency
	t.Run("Idempotent indexing on duplicate events", func(t *testing.T) {
		handle := fmt.Sprintf("bob-%d.local.coves.dev", time.Now().Unix())
		email := fmt.Sprintf("bob-%d@test.com", time.Now().Unix())

		// Create account via UserService
		did, err := createPDSAccount(t, userService, handle, email, "test1234")
		if err != nil {
			t.Fatalf("Failed to create PDS account: %v", err)
		}

		// Wait for indexing (with retry)
		var user1 *users.User
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			user1, err = userService.GetUserByDID(ctx, did)
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("User not indexed after 10s: %v", err)
		}

		// Manually trigger duplicate indexing (simulates Jetstream replay)
		_, err = userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    did,
			Handle: handle,
			PDSURL: "http://localhost:3001",
		})
		if err != nil {
			t.Fatalf("Idempotent CreateUser should not error: %v", err)
		}

		// Verify still only one user
		user2, err := userService.GetUserByDID(ctx, did)
		if err != nil {
			t.Fatalf("Failed to get user after duplicate: %v", err)
		}

		if user1.CreatedAt != user2.CreatedAt {
			t.Errorf("Duplicate indexing created new user (timestamps differ)")
		}

		t.Logf("✅ Idempotency verified: duplicate events handled gracefully")
	})

	// Test 3: Multiple users
	t.Run("Index multiple users concurrently", func(t *testing.T) {
		const numUsers = 3
		dids := make([]string, numUsers)

		for i := 0; i < numUsers; i++ {
			handle := fmt.Sprintf("user%d-%d.local.coves.dev", i, time.Now().Unix())
			email := fmt.Sprintf("user%d-%d@test.com", i, time.Now().Unix())

			did, err := createPDSAccount(t, userService, handle, email, "test1234")
			if err != nil {
				t.Fatalf("Failed to create account %d: %v", i, err)
			}
			dids[i] = did
			t.Logf("Created user %d: %s", i, did)

			// Small delay between creations
			time.Sleep(500 * time.Millisecond)
		}

		// Verify all indexed (with retry for each user)
		t.Log("Waiting for all users to be indexed...")
		for i, did := range dids {
			var user *users.User
			var err error
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				user, err = userService.GetUserByDID(ctx, did)
				if err == nil {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if err != nil {
				t.Errorf("User %d not indexed after 15s: %v", i, err)
				continue
			}
			t.Logf("✅ User %d indexed: %s", i, user.Handle)
		}
	})

	// Clean shutdown
	cancel()
	select {
	case err := <-consumerDone:
		if err != nil && err != context.Canceled {
			t.Logf("Consumer stopped with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Log("Consumer shutdown timeout")
	}
}

// generateInviteCode generates a single-use invite code via PDS admin API
func generateInviteCode(t *testing.T) (string, error) {
	payload := map[string]int{
		"useCount": 1,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest(
		"POST",
		"http://localhost:3001/xrpc/com.atproto.server.createInviteCode",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// PDS admin authentication
	req.SetBasicAuth("admin", "admin")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to create invite code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errorResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errorResp)
		return "", fmt.Errorf("PDS admin API returned status %d: %v", resp.StatusCode, errorResp)
	}

	var result struct {
		Code string `json:"code"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	t.Logf("Generated invite code: %s", result.Code)
	return result.Code, nil
}

// createPDSAccount creates an account via the coves.user.signup XRPC endpoint
// This is the same code path that a third-party client or UI would use
func createPDSAccount(t *testing.T, userService users.UserService, handle, email, password string) (string, error) {
	// Generate fresh invite code for each account
	inviteCode, err := generateInviteCode(t)
	if err != nil {
		return "", fmt.Errorf("failed to generate invite code: %w", err)
	}

	// Call our XRPC endpoint (what a third-party client would call)
	payload := map[string]string{
		"handle":     handle,
		"email":      email,
		"password":   password,
		"inviteCode": inviteCode,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := http.Post(
		"http://localhost:8081/xrpc/social.coves.actor.signup",
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return "", fmt.Errorf("failed to call signup endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errorResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errorResp)
		return "", fmt.Errorf("signup endpoint returned status %d: %v", resp.StatusCode, errorResp)
	}

	var result struct {
		DID         string `json:"did"`
		Handle      string `json:"handle"`
		AccessJwt   string `json:"accessJwt"`
		RefreshJwt  string `json:"refreshJwt"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	t.Logf("Account created via XRPC endpoint: %s → %s", result.Handle, result.DID)

	return result.DID, nil
}

// isPDSAvailable checks if PDS is running
func isPDSAvailable(t *testing.T) bool {
	resp, err := http.Get("http://localhost:3001/xrpc/_health")
	if err != nil {
		t.Logf("PDS not available: %v", err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// isJetstreamAvailable checks if Jetstream is running
func isJetstreamAvailable(t *testing.T) bool {
	// Use 127.0.0.1 instead of localhost to force IPv4
	resp, err := http.Get("http://127.0.0.1:6009/metrics")
	if err != nil {
		t.Logf("Jetstream not available: %v", err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// isAppViewAvailable checks if AppView is running
func isAppViewAvailable(t *testing.T) bool {
	resp, err := http.Get("http://localhost:8081/health")
	if err != nil {
		t.Logf("AppView not available: %v", err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// setupTestDB connects to test database and runs migrations
func setupTestDB(t *testing.T) *sql.DB {
	// Build connection string from environment variables (set by .env.dev)
	testUser := os.Getenv("POSTGRES_TEST_USER")
	testPassword := os.Getenv("POSTGRES_TEST_PASSWORD")
	testPort := os.Getenv("POSTGRES_TEST_PORT")
	testDB := os.Getenv("POSTGRES_TEST_DB")

	// Fallback to defaults if not set
	if testUser == "" {
		testUser = "test_user"
	}
	if testPassword == "" {
		testPassword = "test_password"
	}
	if testPort == "" {
		testPort = "5434"
	}
	if testDB == "" {
		testDB = "coves_test"
	}

	dbURL := fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		testUser, testPassword, testPort, testDB)

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("Failed to ping test database: %v", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("Failed to set goose dialect: %v", err)
	}

	if err := goose.Up(db, "../../internal/db/migrations"); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	// Clean up any existing test data
	_, err = db.Exec("DELETE FROM users WHERE handle LIKE '%.test' OR handle LIKE '%.local.coves.dev'")
	if err != nil {
		t.Logf("Warning: Failed to clean up test data: %v", err)
	}

	return db
}
