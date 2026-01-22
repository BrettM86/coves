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
	"time"

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
	_, err = db.Exec("DELETE FROM community_subscriptions")
	if err != nil {
		t.Logf("Warning: Failed to clean up subscriptions: %v", err)
	}
	// Delete comments (references posts)
	_, err = db.Exec("DELETE FROM comments")
	if err != nil {
		t.Logf("Warning: Failed to clean up comments: %v", err)
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

	// Set up HTTP router with auth middleware
	r := chi.NewRouter()
	authMiddleware, _ := CreateTestOAuthMiddleware("did:plc:testuser")
	routes.RegisterUserRoutes(r, userService, authMiddleware)

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

		// New structure: flat profileViewDetailed (not nested profile object)
		if response["handle"] != "bob.test" {
			t.Errorf("Expected handle bob.test, got %v", response["handle"])
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

// TestProfileStats tests that profile stats are returned correctly
func TestProfileStats(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Use unique test DID to avoid conflicts with other test runs
	uniqueSuffix := time.Now().UnixNano()
	testDID := fmt.Sprintf("did:plc:profilestats%d", uniqueSuffix)

	// Wire up dependencies
	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")

	ctx := context.Background()

	// Create test user
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testDID,
		Handle: fmt.Sprintf("statsuser%d.test", uniqueSuffix),
		PDSURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	t.Run("Profile includes stats with zero counts for new user", func(t *testing.T) {
		profile, err := userService.GetProfile(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to get profile: %v", err)
		}

		if profile.Stats == nil {
			t.Fatal("Expected stats to be non-nil")
		}

		// New user should have zero counts
		if profile.Stats.PostCount != 0 {
			t.Errorf("Expected postCount 0, got %d", profile.Stats.PostCount)
		}
		if profile.Stats.CommentCount != 0 {
			t.Errorf("Expected commentCount 0, got %d", profile.Stats.CommentCount)
		}
		if profile.Stats.CommunityCount != 0 {
			t.Errorf("Expected communityCount 0, got %d", profile.Stats.CommunityCount)
		}
		if profile.Stats.MembershipCount != 0 {
			t.Errorf("Expected membershipCount 0, got %d", profile.Stats.MembershipCount)
		}
		if profile.Stats.Reputation != 0 {
			t.Errorf("Expected reputation 0, got %d", profile.Stats.Reputation)
		}
	})

	t.Run("Profile stats count posts correctly", func(t *testing.T) {
		// Create a test community (required for posts FK)
		testCommunityDID := fmt.Sprintf("did:plc:statscommunity%d", uniqueSuffix)
		_, err := db.Exec(`
			INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
			VALUES ($1, $2, 'Test Community', 'did:plc:owner1', 'did:plc:owner1', 'did:plc:owner1', NOW())
		`, testCommunityDID, fmt.Sprintf("statscommunity%d.test", uniqueSuffix))
		if err != nil {
			t.Fatalf("Failed to insert test community: %v", err)
		}

		// Insert test posts
		for i := 1; i <= 3; i++ {
			_, err = db.Exec(`
				INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at, indexed_at)
				VALUES ($1, $2, $3, $4, $5, 'Post', 'Content', NOW(), NOW())
			`, fmt.Sprintf("at://%s/social.coves.post/%d", testDID, i), fmt.Sprintf("cid%d", i), fmt.Sprintf("%d", i), testDID, testCommunityDID)
			if err != nil {
				t.Fatalf("Failed to insert post %d: %v", i, err)
			}
		}

		profile, err := userService.GetProfile(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to get profile: %v", err)
		}

		if profile.Stats.PostCount != 3 {
			t.Errorf("Expected postCount 3, got %d", profile.Stats.PostCount)
		}

		// Test that soft-deleted posts are not counted (delete the first one by URI)
		_, err = db.Exec(`UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, fmt.Sprintf("at://%s/social.coves.post/1", testDID))
		if err != nil {
			t.Fatalf("Failed to soft-delete post: %v", err)
		}

		profile, err = userService.GetProfile(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to get profile: %v", err)
		}

		if profile.Stats.PostCount != 2 {
			t.Errorf("Expected postCount 2 after deletion, got %d", profile.Stats.PostCount)
		}
	})

	t.Run("Profile stats count memberships and sum reputation", func(t *testing.T) {
		// Need a community that exists for the FK
		testCommunityDID := fmt.Sprintf("did:plc:statscommunity%d", uniqueSuffix)

		// Insert membership with reputation
		_, err := db.Exec(`
			INSERT INTO community_memberships (user_did, community_did, reputation_score, contribution_count, is_banned, is_moderator, joined_at, last_active_at)
			VALUES ($1, $2, 150, 10, false, false, NOW(), NOW())
		`, testDID, testCommunityDID)
		if err != nil {
			t.Fatalf("Failed to insert test membership: %v", err)
		}

		profile, err := userService.GetProfile(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to get profile: %v", err)
		}

		if profile.Stats.MembershipCount != 1 {
			t.Errorf("Expected membershipCount 1, got %d", profile.Stats.MembershipCount)
		}
		if profile.Stats.Reputation != 150 {
			t.Errorf("Expected reputation 150, got %d", profile.Stats.Reputation)
		}

		// Test that banned memberships are not counted
		_, err = db.Exec(`UPDATE community_memberships SET is_banned = true WHERE user_did = $1`, testDID)
		if err != nil {
			t.Fatalf("Failed to ban user: %v", err)
		}

		profile, err = userService.GetProfile(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to get profile: %v", err)
		}

		if profile.Stats.MembershipCount != 0 {
			t.Errorf("Expected membershipCount 0 after ban, got %d", profile.Stats.MembershipCount)
		}
		// Note: Reputation still counts from banned memberships (this is intentional per lexicon spec)
	})
}

// TestProfileStats_CommentCount tests that comment counting works correctly
func TestProfileStats_CommentCount(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	uniqueSuffix := time.Now().UnixNano()
	testDID := fmt.Sprintf("did:plc:commentcount%d", uniqueSuffix)

	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")

	ctx := context.Background()

	// Create test user
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testDID,
		Handle: fmt.Sprintf("commentuser%d.test", uniqueSuffix),
		PDSURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	// Create test community (required for posts FK, which is required for comments)
	testCommunityDID := fmt.Sprintf("did:plc:commentcommunity%d", uniqueSuffix)
	_, err = db.Exec(`
		INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
		VALUES ($1, $2, 'Comment Test Community', 'did:plc:owner1', 'did:plc:owner1', 'did:plc:owner1', NOW())
	`, testCommunityDID, fmt.Sprintf("commentcommunity%d.test", uniqueSuffix))
	if err != nil {
		t.Fatalf("Failed to insert test community: %v", err)
	}

	// Create a test post (required for comments FK)
	testPostURI := fmt.Sprintf("at://%s/social.coves.post/commenttest", testDID)
	_, err = db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at, indexed_at)
		VALUES ($1, 'testcid', 'commenttest', $2, $3, 'Test Post', 'Content', NOW(), NOW())
	`, testPostURI, testDID, testCommunityDID)
	if err != nil {
		t.Fatalf("Failed to insert test post: %v", err)
	}

	t.Run("Counts comments correctly", func(t *testing.T) {
		// Insert test comments
		testPostCID := "testpostcid123"
		for i := 1; i <= 5; i++ {
			_, err = db.Exec(`
				INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at, indexed_at)
				VALUES ($1, $2, $3, $4, $5, $6, $5, $6, 'Comment content', NOW(), NOW())
			`, fmt.Sprintf("at://%s/social.coves.comment/%d", testDID, i),
				fmt.Sprintf("commentcid%d", i),
				fmt.Sprintf("comment%d", i),
				testDID,
				testPostURI,
				testPostCID)
			if err != nil {
				t.Fatalf("Failed to insert comment %d: %v", i, err)
			}
		}

		profile, err := userService.GetProfile(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to get profile: %v", err)
		}

		if profile.Stats.CommentCount != 5 {
			t.Errorf("Expected commentCount 5, got %d", profile.Stats.CommentCount)
		}
	})

	t.Run("Excludes soft-deleted comments", func(t *testing.T) {
		// Soft-delete 2 comments
		_, err = db.Exec(`UPDATE comments SET deleted_at = NOW() WHERE uri LIKE $1 AND rkey IN ('comment1', 'comment2')`,
			fmt.Sprintf("at://%s/%%", testDID))
		if err != nil {
			t.Fatalf("Failed to soft-delete comments: %v", err)
		}

		profile, err := userService.GetProfile(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to get profile: %v", err)
		}

		if profile.Stats.CommentCount != 3 {
			t.Errorf("Expected commentCount 3 after soft-delete, got %d", profile.Stats.CommentCount)
		}
	})
}

// TestProfileStats_CommunityCount tests that subscription counting works correctly
func TestProfileStats_CommunityCount(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	uniqueSuffix := time.Now().UnixNano()
	testDID := fmt.Sprintf("did:plc:subcount%d", uniqueSuffix)

	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")

	ctx := context.Background()

	// Create test user
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testDID,
		Handle: fmt.Sprintf("subuser%d.test", uniqueSuffix),
		PDSURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	// Create test communities to subscribe to
	for i := 1; i <= 3; i++ {
		communityDID := fmt.Sprintf("did:plc:subcommunity%d_%d", uniqueSuffix, i)
		_, err = db.Exec(`
			INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
			VALUES ($1, $2, $3, 'did:plc:owner1', 'did:plc:owner1', 'did:plc:owner1', NOW())
		`, communityDID, fmt.Sprintf("subcommunity%d_%d.test", uniqueSuffix, i), fmt.Sprintf("Community %d", i))
		if err != nil {
			t.Fatalf("Failed to insert test community %d: %v", i, err)
		}
	}

	t.Run("Counts subscriptions correctly", func(t *testing.T) {
		// Subscribe to communities
		for i := 1; i <= 3; i++ {
			communityDID := fmt.Sprintf("did:plc:subcommunity%d_%d", uniqueSuffix, i)
			_, err = db.Exec(`
				INSERT INTO community_subscriptions (user_did, community_did, subscribed_at)
				VALUES ($1, $2, NOW())
			`, testDID, communityDID)
			if err != nil {
				t.Fatalf("Failed to insert subscription %d: %v", i, err)
			}
		}

		profile, err := userService.GetProfile(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to get profile: %v", err)
		}

		if profile.Stats.CommunityCount != 3 {
			t.Errorf("Expected communityCount 3, got %d", profile.Stats.CommunityCount)
		}
	})
}

// TestGetProfile_NonExistentDID tests that GetProfile returns appropriate error for non-existent DID
func TestGetProfile_NonExistentDID(t *testing.T) {
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

	t.Run("Returns error for non-existent DID", func(t *testing.T) {
		_, err := userService.GetProfile(ctx, "did:plc:nonexistentuser12345")
		if err == nil {
			t.Fatal("Expected error for non-existent DID, got nil")
		}

		// Should contain the wrapped ErrUserNotFound
		if !strings.Contains(err.Error(), "user not found") && !strings.Contains(err.Error(), "failed to get user") {
			t.Errorf("Expected 'user not found' or 'failed to get user' error, got: %v", err)
		}
	})

	t.Run("HTTP endpoint returns 404 for non-existent DID", func(t *testing.T) {
		r := chi.NewRouter()
		authMiddleware, _ := CreateTestOAuthMiddleware("did:plc:testuser")
		routes.RegisterUserRoutes(r, userService, authMiddleware)

		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getprofile?actor=did:plc:nonexistentuser12345", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("Expected status 404, got %d. Response: %s", w.Code, w.Body.String())
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if response["error"] != "ProfileNotFound" {
			t.Errorf("Expected error 'ProfileNotFound', got %v", response["error"])
		}
	})
}

// TestProfileStatsEndpoint tests the HTTP endpoint returns stats correctly
func TestProfileStatsEndpoint(t *testing.T) {
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

	// Create test user
	testDID := "did:plc:endpointstats123"
	ctx := context.Background()
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testDID,
		Handle: "endpointstats.test",
		PDSURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	// Set up HTTP router with auth middleware
	r := chi.NewRouter()
	authMiddleware, _ := CreateTestOAuthMiddleware("did:plc:testuser")
	routes.RegisterUserRoutes(r, userService, authMiddleware)

	t.Run("Response includes stats object", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getprofile?actor="+testDID, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status %d, got %d. Response: %s", http.StatusOK, w.Code, w.Body.String())
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		// Check that stats is present
		stats, ok := response["stats"].(map[string]interface{})
		if !ok {
			t.Fatalf("Expected stats object in response, got: %v", response)
		}

		// Verify stats fields exist
		expectedFields := []string{"postCount", "commentCount", "communityCount", "reputation", "membershipCount"}
		for _, field := range expectedFields {
			if _, exists := stats[field]; !exists {
				t.Errorf("Expected %s in stats, but it's missing", field)
			}
		}

		// All stats should be 0 for new user
		for _, field := range expectedFields {
			if val, ok := stats[field].(float64); !ok || val != 0 {
				t.Errorf("Expected %s to be 0, got %v", field, stats[field])
			}
		}
	})

	t.Run("Response matches lexicon structure", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xrpc/social.coves.actor.getprofile?actor=endpointstats.test", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status %d, got %d", http.StatusOK, w.Code)
		}

		var response map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		// Verify profileViewDetailed structure (flat, not nested)
		if response["did"] != testDID {
			t.Errorf("Expected did %s, got %v", testDID, response["did"])
		}
		if response["handle"] != "endpointstats.test" {
			t.Errorf("Expected handle endpointstats.test, got %v", response["handle"])
		}
		if _, ok := response["createdAt"]; !ok {
			t.Error("Expected createdAt in response")
		}
		if _, ok := response["stats"]; !ok {
			t.Error("Expected stats in response")
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

// TestAccountDeletion_Integration tests the complete account deletion flow
// from handler → service → repository with a real database
func TestAccountDeletion_Integration(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	uniqueSuffix := time.Now().UnixNano()
	testDID := fmt.Sprintf("did:plc:deletetest%d", uniqueSuffix)
	testHandle := fmt.Sprintf("deletetest%d.test", uniqueSuffix)

	// Wire up dependencies
	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, "http://localhost:3001")

	ctx := context.Background()

	// Create test user
	_, err := userService.CreateUser(ctx, users.CreateUserRequest{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "http://localhost:3001",
	})
	if err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	// Create test community for FK relationships
	testCommunityDID := fmt.Sprintf("did:plc:deletetestcommunity%d", uniqueSuffix)
	_, err = db.Exec(`
		INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
		VALUES ($1, $2, 'Delete Test Community', 'did:plc:owner1', 'did:plc:owner1', 'did:plc:owner1', NOW())
	`, testCommunityDID, fmt.Sprintf("deletetestcommunity%d.test", uniqueSuffix))
	if err != nil {
		t.Fatalf("Failed to insert test community: %v", err)
	}

	// Create related data across all tables
	t.Run("Setup test data", func(t *testing.T) {
		// Posts
		for i := 1; i <= 3; i++ {
			_, err = db.Exec(`
				INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at, indexed_at)
				VALUES ($1, $2, $3, $4, $5, 'Test Post', 'Content', NOW(), NOW())
			`, fmt.Sprintf("at://%s/social.coves.post/delete%d", testDID, i),
				fmt.Sprintf("deletecid%d", i),
				fmt.Sprintf("delete%d", i),
				testDID,
				testCommunityDID)
			if err != nil {
				t.Fatalf("Failed to insert post %d: %v", i, err)
			}
		}

		// Community subscription
		_, err = db.Exec(`
			INSERT INTO community_subscriptions (user_did, community_did, subscribed_at)
			VALUES ($1, $2, NOW())
		`, testDID, testCommunityDID)
		if err != nil {
			t.Fatalf("Failed to insert subscription: %v", err)
		}

		// Community membership
		_, err = db.Exec(`
			INSERT INTO community_memberships (user_did, community_did, reputation_score, contribution_count, is_banned, is_moderator, joined_at, last_active_at)
			VALUES ($1, $2, 100, 5, false, false, NOW(), NOW())
		`, testDID, testCommunityDID)
		if err != nil {
			t.Fatalf("Failed to insert membership: %v", err)
		}

		// Vote (using one of the posts)
		postURI := fmt.Sprintf("at://%s/social.coves.post/delete1", testDID)
		_, err = db.Exec(`
			INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at)
			VALUES ($1, 'votecid', 'vote1', $2, $3, 'postcid', 'up', NOW())
		`, fmt.Sprintf("at://%s/social.coves.vote/delete1", testDID), testDID, postURI)
		if err != nil {
			t.Fatalf("Failed to insert vote: %v", err)
		}

		// Comments
		postCID := "deletecid1"
		for i := 1; i <= 2; i++ {
			_, err = db.Exec(`
				INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at, indexed_at)
				VALUES ($1, $2, $3, $4, $5, $6, $5, $6, 'Test comment', NOW(), NOW())
			`, fmt.Sprintf("at://%s/social.coves.comment/delete%d", testDID, i),
				fmt.Sprintf("deletecommentcid%d", i),
				fmt.Sprintf("deletecomment%d", i),
				testDID,
				postURI,
				postCID)
			if err != nil {
				t.Fatalf("Failed to insert comment %d: %v", i, err)
			}
		}
	})

	// Verify data exists before deletion
	t.Run("Verify data exists before deletion", func(t *testing.T) {
		var count int

		// Check user exists
		err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE did = $1`, testDID).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("Expected 1 user, got %d (err: %v)", count, err)
		}

		// Check posts exist
		err = db.QueryRow(`SELECT COUNT(*) FROM posts WHERE author_did = $1`, testDID).Scan(&count)
		if err != nil || count != 3 {
			t.Fatalf("Expected 3 posts, got %d (err: %v)", count, err)
		}

		// Check subscription exists
		err = db.QueryRow(`SELECT COUNT(*) FROM community_subscriptions WHERE user_did = $1`, testDID).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("Expected 1 subscription, got %d (err: %v)", count, err)
		}

		// Check membership exists
		err = db.QueryRow(`SELECT COUNT(*) FROM community_memberships WHERE user_did = $1`, testDID).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("Expected 1 membership, got %d (err: %v)", count, err)
		}

		// Check vote exists
		err = db.QueryRow(`SELECT COUNT(*) FROM votes WHERE voter_did = $1`, testDID).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("Expected 1 vote, got %d (err: %v)", count, err)
		}

		// Check comments exist
		err = db.QueryRow(`SELECT COUNT(*) FROM comments WHERE commenter_did = $1`, testDID).Scan(&count)
		if err != nil || count != 2 {
			t.Fatalf("Expected 2 comments, got %d (err: %v)", count, err)
		}
	})

	// Delete account
	t.Run("Delete account via service", func(t *testing.T) {
		err := userService.DeleteAccount(ctx, testDID)
		if err != nil {
			t.Fatalf("Failed to delete account: %v", err)
		}
	})

	// Verify all data is deleted
	t.Run("Verify all data deleted after deletion", func(t *testing.T) {
		var count int

		// Check user deleted
		err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE did = $1`, testDID).Scan(&count)
		if err != nil {
			t.Fatalf("Error checking users: %v", err)
		}
		if count != 0 {
			t.Errorf("Expected 0 users after deletion, got %d", count)
		}

		// Check posts deleted (via FK CASCADE)
		err = db.QueryRow(`SELECT COUNT(*) FROM posts WHERE author_did = $1`, testDID).Scan(&count)
		if err != nil {
			t.Fatalf("Error checking posts: %v", err)
		}
		if count != 0 {
			t.Errorf("Expected 0 posts after deletion, got %d", count)
		}

		// Check subscription deleted
		err = db.QueryRow(`SELECT COUNT(*) FROM community_subscriptions WHERE user_did = $1`, testDID).Scan(&count)
		if err != nil {
			t.Fatalf("Error checking subscriptions: %v", err)
		}
		if count != 0 {
			t.Errorf("Expected 0 subscriptions after deletion, got %d", count)
		}

		// Check membership deleted
		err = db.QueryRow(`SELECT COUNT(*) FROM community_memberships WHERE user_did = $1`, testDID).Scan(&count)
		if err != nil {
			t.Fatalf("Error checking memberships: %v", err)
		}
		if count != 0 {
			t.Errorf("Expected 0 memberships after deletion, got %d", count)
		}

		// Check vote deleted
		err = db.QueryRow(`SELECT COUNT(*) FROM votes WHERE voter_did = $1`, testDID).Scan(&count)
		if err != nil {
			t.Fatalf("Error checking votes: %v", err)
		}
		if count != 0 {
			t.Errorf("Expected 0 votes after deletion, got %d", count)
		}

		// Check comments deleted
		err = db.QueryRow(`SELECT COUNT(*) FROM comments WHERE commenter_did = $1`, testDID).Scan(&count)
		if err != nil {
			t.Fatalf("Error checking comments: %v", err)
		}
		if count != 0 {
			t.Errorf("Expected 0 comments after deletion, got %d", count)
		}
	})

	// Verify second delete returns ErrUserNotFound
	t.Run("Delete non-existent account returns error", func(t *testing.T) {
		err := userService.DeleteAccount(ctx, testDID)
		if err == nil {
			t.Error("Expected error when deleting already-deleted account")
		}
		if err != users.ErrUserNotFound {
			t.Errorf("Expected ErrUserNotFound, got: %v", err)
		}
	})

	// Verify community still exists (only user data deleted)
	t.Run("Community still exists after user deletion", func(t *testing.T) {
		var count int
		err := db.QueryRow(`SELECT COUNT(*) FROM communities WHERE did = $1`, testCommunityDID).Scan(&count)
		if err != nil {
			t.Fatalf("Error checking community: %v", err)
		}
		if count != 1 {
			t.Errorf("Expected community to still exist, got count %d", count)
		}
	})
}
