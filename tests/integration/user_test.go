package integration

import (
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// TestMain controls test setup for the integration package.
// Set LOG_ENABLED=false to suppress application log output during tests.
func TestMain(m *testing.M) {
	// Silence logs when LOG_ENABLED=false (used by make test-all)
	if os.Getenv("LOG_ENABLED") == "false" {
		log.SetOutput(io.Discard)
	}

	os.Exit(m.Run())
}

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

	// Limit connection pool to prevent "too many clients" error in parallel tests
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	if pingErr := db.Ping(); pingErr != nil {
		t.Fatalf("Failed to ping test database: %v", pingErr)
	}

	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}

	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	// Clean up any existing test data (order matters due to FK constraints)
	// Delete subscriptions first (references communities and users)
	_, err = db.Exec("DELETE FROM subscriptions")
	if err != nil {
		t.Logf("Warning: Failed to clean up subscriptions: %v", err)
	}
	// Delete posts (references communities)
	_, err = db.Exec("DELETE FROM posts")
	if err != nil {
		t.Logf("Warning: Failed to clean up posts: %v", err)
	}
	// Delete communities
	_, err = db.Exec("DELETE FROM communities")
	if err != nil {
		t.Logf("Warning: Failed to clean up communities: %v", err)
	}
	// Delete users
	_, err = db.Exec("DELETE FROM users WHERE handle LIKE '%.test'")
	if err != nil {
		t.Logf("Warning: Failed to clean up test users: %v", err)
	}

	return db
}

// generateTestDID generates a unique test DID for integration tests
// V2.0: No longer uses DID generator - just creates valid did:plc strings
func generateTestDID(suffix string) string {
	// Use a deterministic base + suffix for reproducible test DIDs
	// Format matches did:plc but doesn't need PLC registration for unit/repo tests
	return fmt.Sprintf("did:plc:test%s", suffix)
}

func TestUserCreationAndRetrieval(t *testing.T) {
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

	// Test 1: Create a user
	t.Run("Create User", func(t *testing.T) {
		req := users.CreateUserRequest{
			DID:    "did:plc:test123456",
			Handle: "alice.test",
			PDSURL: "http://localhost:3001",
		}

		user, err := userService.CreateUser(ctx, req)
		if err != nil {
			t.Fatalf("Failed to create user: %v", err)
		}

		if user.DID != req.DID {
			t.Errorf("Expected DID %s, got %s", req.DID, user.DID)
		}

		if user.Handle != req.Handle {
			t.Errorf("Expected handle %s, got %s", req.Handle, user.Handle)
		}

		if user.CreatedAt.IsZero() {
			t.Error("CreatedAt should not be zero")
		}
	})

	// Test 2: Retrieve user by DID
	t.Run("Get User By DID", func(t *testing.T) {
		user, err := userService.GetUserByDID(ctx, "did:plc:test123456")
		if err != nil {
			t.Fatalf("Failed to get user by DID: %v", err)
		}

		if user.Handle != "alice.test" {
			t.Errorf("Expected handle alice.test, got %s", user.Handle)
		}
	})

	// Test 3: Retrieve user by handle
	t.Run("Get User By Handle", func(t *testing.T) {
		user, err := userService.GetUserByHandle(ctx, "alice.test")
		if err != nil {
			t.Fatalf("Failed to get user by handle: %v", err)
		}

		if user.DID != "did:plc:test123456" {
			t.Errorf("Expected DID did:plc:test123456, got %s", user.DID)
		}
	})

	// Test 4: Resolve handle to DID (using real handle)
	t.Run("Resolve Handle to DID", func(t *testing.T) {
		// Test with a real atProto handle
		did, err := userService.ResolveHandleToDID(ctx, "bretton.dev")
		if err != nil {
			t.Fatalf("Failed to resolve handle bretton.dev: %v", err)
		}

		if did == "" {
			t.Error("Expected non-empty DID")
		}

		t.Logf("✅ Resolved bretton.dev → %s", did)
	})
}

func TestGetProfileEndpoint(t *testing.T) {
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

	// Create test user directly in service
	ctx := context.Background()
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    "did:plc:endpoint123",
		Handle: "bob.test",
		PDSURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	// Set up HTTP router
	r := chi.NewRouter()
	routes.RegisterUserRoutes(r, userService)

	// Test 1: Get profile by DID
	t.Run("Get Profile By DID", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getprofile?actor=did:plc:endpoint123", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d. Response: %s", http.StatusOK, w.Code, w.Body.String())
			return
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if response["did"] != "did:plc:endpoint123" {
			t.Errorf("Expected DID did:plc:endpoint123, got %v", response["did"])
		}
	})

	// Test 2: Get profile by handle
	t.Run("Get Profile By Handle", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getprofile?actor=bob.test", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d. Response: %s", http.StatusOK, w.Code, w.Body.String())
			return
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		profile := response["profile"].(map[string]interface{})
		if profile["handle"] != "bob.test" {
			t.Errorf("Expected handle bob.test, got %v", profile["handle"])
		}
	})

	// Test 3: Missing actor parameter
	t.Run("Missing Actor Parameter", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getprofile", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
		}
	})

	// Test 4: User not found
	t.Run("User Not Found", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getprofile?actor=nonexistent.test", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("Expected status %d, got %d", http.StatusNotFound, w.Code)
		}
	})
}

// TestDuplicateCreation tests that duplicate DID/handle creation fails properly
func TestDuplicateCreation(t *testing.T) {
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

	// Create first user
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    "did:plc:duplicate123",
		Handle: "duplicate.test",
		PDSURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("Failed to create first user: %v", err)
	}

	// Test duplicate DID - now idempotent, returns existing user
	t.Run("Duplicate DID - Idempotent", func(t *testing.T) {
		user, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    "did:plc:duplicate123",
			Handle: "different.test", // Different handle, same DID
			PDSURL: "http://localhost:3001",
		})
		// Should return existing user, not error
		if err != nil {
			t.Fatalf("Expected idempotent behavior, got error: %v", err)
		}

		// Should return the original user (with original handle)
		if user.Handle != "duplicate.test" {
			t.Errorf("Expected original handle 'duplicate.test', got: %s", user.Handle)
		}
	})

	// Test duplicate handle
	t.Run("Duplicate Handle", func(t *testing.T) {
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    "did:plc:different456",
			Handle: "duplicate.test",
			PDSURL: "http://localhost:3001",
		})

		if err == nil {
			t.Error("Expected error for duplicate handle, got nil")
		}

		if !strings.Contains(err.Error(), "handle already taken") {
			t.Errorf("Expected 'handle already taken' error, got: %v", err)
		}
	})
}

// TestUserRepository_GetByDIDs tests the batch user retrieval functionality
func TestUserRepository_GetByDIDs(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	userRepo := postgres.NewUserRepository(db)
	ctx := context.Background()

	// Create test users
	user1 := &users.User{
		DID:    "did:plc:getbydids1",
		Handle: "user1.test",
		PDSURL: "https://pds1.example.com",
	}
	user2 := &users.User{
		DID:    "did:plc:getbydids2",
		Handle: "user2.test",
		PDSURL: "https://pds2.example.com",
	}
	user3 := &users.User{
		DID:    "did:plc:getbydids3",
		Handle: "user3.test",
		PDSURL: "https://pds3.example.com",
	}

	_, err := userRepo.Create(ctx, user1)
	if err != nil {
		t.Fatalf("Failed to create user1: %v", err)
	}
	_, err = userRepo.Create(ctx, user2)
	if err != nil {
		t.Fatalf("Failed to create user2: %v", err)
	}
	_, err = userRepo.Create(ctx, user3)
	if err != nil {
		t.Fatalf("Failed to create user3: %v", err)
	}

	t.Run("Empty array returns empty map", func(t *testing.T) {
		result, err := userRepo.GetByDIDs(ctx, []string{})
		if err != nil {
			t.Errorf("Expected no error for empty array, got: %v", err)
		}
		if result == nil {
			t.Error("Expected non-nil map, got nil")
		}
		if len(result) != 0 {
			t.Errorf("Expected empty map, got length: %d", len(result))
		}
	})

	t.Run("Single DID returns one user", func(t *testing.T) {
		result, err := userRepo.GetByDIDs(ctx, []string{"did:plc:getbydids1"})
		if err != nil {
			t.Fatalf("Failed to get user by DID: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("Expected 1 user, got %d", len(result))
		}
		if user, found := result["did:plc:getbydids1"]; !found {
			t.Error("Expected user1 to be in result")
		} else if user.Handle != "user1.test" {
			t.Errorf("Expected handle user1.test, got %s", user.Handle)
		}
	})

	t.Run("Multiple DIDs returns multiple users", func(t *testing.T) {
		result, err := userRepo.GetByDIDs(ctx, []string{
			"did:plc:getbydids1",
			"did:plc:getbydids2",
			"did:plc:getbydids3",
		})
		if err != nil {
			t.Fatalf("Failed to get users by DIDs: %v", err)
		}
		if len(result) != 3 {
			t.Errorf("Expected 3 users, got %d", len(result))
		}
		if result["did:plc:getbydids1"].Handle != "user1.test" {
			t.Errorf("User1 handle mismatch")
		}
		if result["did:plc:getbydids2"].Handle != "user2.test" {
			t.Errorf("User2 handle mismatch")
		}
		if result["did:plc:getbydids3"].Handle != "user3.test" {
			t.Errorf("User3 handle mismatch")
		}
	})

	t.Run("Missing DIDs not in result map", func(t *testing.T) {
		result, err := userRepo.GetByDIDs(ctx, []string{
			"did:plc:getbydids1",
			"did:plc:nonexistent",
		})
		if err != nil {
			t.Fatalf("Failed to get users by DIDs: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("Expected 1 user (missing not included), got %d", len(result))
		}
		if _, found := result["did:plc:nonexistent"]; found {
			t.Error("Expected nonexistent user to not be in result")
		}
	})

	t.Run("Preserves all user fields correctly", func(t *testing.T) {
		result, err := userRepo.GetByDIDs(ctx, []string{"did:plc:getbydids1"})
		if err != nil {
			t.Fatalf("Failed to get user by DID: %v", err)
		}
		user := result["did:plc:getbydids1"]
		if user.DID != "did:plc:getbydids1" {
			t.Errorf("DID mismatch: expected did:plc:getbydids1, got %s", user.DID)
		}
		if user.Handle != "user1.test" {
			t.Errorf("Handle mismatch: expected user1.test, got %s", user.Handle)
		}
		if user.PDSURL != "https://pds1.example.com" {
			t.Errorf("PDSURL mismatch: expected https://pds1.example.com, got %s", user.PDSURL)
		}
		if user.CreatedAt.IsZero() {
			t.Error("CreatedAt should not be zero")
		}
		if user.UpdatedAt.IsZero() {
			t.Error("UpdatedAt should not be zero")
		}
	})

	t.Run("Validates batch size limit", func(t *testing.T) {
		// Create array exceeding MaxBatchSize (1000)
		largeDIDs := make([]string, 1001)
		for i := 0; i < 1001; i++ {
			largeDIDs[i] = fmt.Sprintf("did:plc:test%d", i)
		}

		_, err := userRepo.GetByDIDs(ctx, largeDIDs)
		if err == nil {
			t.Error("Expected error for batch size exceeding limit, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds maximum") {
			t.Errorf("Expected batch size error, got: %v", err)
		}
	})

	t.Run("Validates DID format", func(t *testing.T) {
		invalidDIDs := []string{
			"did:plc:getbydids1",
			"invalid-did", // Invalid DID format
		}

		_, err := userRepo.GetByDIDs(ctx, invalidDIDs)
		if err == nil {
			t.Error("Expected error for invalid DID format, got nil")
		}
		if !strings.Contains(err.Error(), "invalid DID format") {
			t.Errorf("Expected invalid DID format error, got: %v", err)
		}
	})
}

// TestHandleValidation tests atProto handle validation rules
func TestHandleValidation(t *testing.T) {
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

	testCases := []struct {
		name        string
		did         string
		handle      string
		pdsURL      string
		errorMsg    string
		shouldError bool
	}{
		{
			name:        "Valid handle with hyphen",
			did:         "did:plc:valid1",
			handle:      "alice-bob.test",
			pdsURL:      "http://localhost:3001",
			shouldError: false,
		},
		{
			name:        "Valid handle with dots",
			did:         "did:plc:valid2",
			handle:      "alice.bob.test",
			pdsURL:      "http://localhost:3001",
			shouldError: false,
		},
		{
			name:        "Invalid: no dot (not domain-like)",
			did:         "did:plc:invalid8",
			handle:      "alice",
			pdsURL:      "http://localhost:3001",
			shouldError: true,
			errorMsg:    "invalid handle",
		},
		{
			name:        "Valid: consecutive hyphens (allowed per atProto spec)",
			did:         "did:plc:valid3",
			handle:      "alice--bob.test",
			pdsURL:      "http://localhost:3001",
			shouldError: false,
		},
		{
			name:        "Invalid: starts with hyphen",
			did:         "did:plc:invalid2",
			handle:      "-alice.test",
			pdsURL:      "http://localhost:3001",
			shouldError: true,
			errorMsg:    "invalid handle",
		},
		{
			name:        "Invalid: ends with hyphen",
			did:         "did:plc:invalid3",
			handle:      "alice-.test",
			pdsURL:      "http://localhost:3001",
			shouldError: true,
			errorMsg:    "invalid handle",
		},
		{
			name:        "Invalid: special characters",
			did:         "did:plc:invalid4",
			handle:      "alice!bob.test",
			pdsURL:      "http://localhost:3001",
			shouldError: true,
			errorMsg:    "invalid handle",
		},
		{
			name:        "Invalid: spaces",
			did:         "did:plc:invalid5",
			handle:      "alice bob.test",
			pdsURL:      "http://localhost:3001",
			shouldError: true,
			errorMsg:    "invalid handle",
		},
		{
			name:        "Invalid: too long",
			did:         "did:plc:invalid6",
			handle:      strings.Repeat("a", 254) + ".test",
			pdsURL:      "http://localhost:3001",
			shouldError: true,
			errorMsg:    "invalid handle",
		},
		{
			name:        "Invalid: missing DID prefix",
			did:         "plc:invalid7",
			handle:      "valid.test",
			pdsURL:      "http://localhost:3001",
			shouldError: true,
			errorMsg:    "must start with 'did:'",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := userService.CreateUser(ctx, users.CreateUserRequest{
				DID:    tc.did,
				Handle: tc.handle,
				PDSURL: tc.pdsURL,
			})

			if tc.shouldError {
				if err == nil {
					t.Errorf("Expected error, got nil")
				} else if !strings.Contains(err.Error(), tc.errorMsg) {
					t.Errorf("Expected error containing '%s', got: %v", tc.errorMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error, got: %v", err)
				}
			}
		})
	}
}
