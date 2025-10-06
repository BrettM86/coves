package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"

	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
)

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
	_, err = db.Exec("DELETE FROM users WHERE handle LIKE '%.test'")
	if err != nil {
		t.Logf("Warning: Failed to clean up test data: %v", err)
	}

	return db
}

func TestUserCreationAndRetrieval(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

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
	defer db.Close()

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
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getProfile?actor=did:plc:endpoint123", nil)
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
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getProfile?actor=bob.test", nil)
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
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getProfile", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
		}
	})

	// Test 4: User not found
	t.Run("User Not Found", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getProfile?actor=nonexistent.test", nil)
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
	defer db.Close()

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

// TestHandleValidation tests atProto handle validation rules
func TestHandleValidation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")
	ctx := context.Background()

	testCases := []struct {
		name        string
		did         string
		handle      string
		pdsURL      string
		shouldError bool
		errorMsg    string
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
