package postgres

import (
	"Coves/internal/core/users"
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupUserTestDB creates a test database connection and runs migrations
func setupUserTestDB(t *testing.T) *sql.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err, "Failed to connect to test database")

	// Run migrations
	require.NoError(t, goose.Up(db, "../../db/migrations"), "Failed to run migrations")

	return db
}

// cleanupUserData removes all test data related to users
func cleanupUserData(t *testing.T, db *sql.DB, did string) {
	// Clean up in reverse order of foreign key dependencies
	_, err := db.Exec("DELETE FROM votes WHERE voter_did = $1", did)
	require.NoError(t, err)

	_, err = db.Exec("DELETE FROM comments WHERE commenter_did = $1", did)
	require.NoError(t, err)

	_, err = db.Exec("DELETE FROM community_blocks WHERE user_did = $1", did)
	require.NoError(t, err)

	_, err = db.Exec("DELETE FROM community_memberships WHERE user_did = $1", did)
	require.NoError(t, err)

	_, err = db.Exec("DELETE FROM community_subscriptions WHERE user_did = $1", did)
	require.NoError(t, err)

	_, err = db.Exec("DELETE FROM oauth_requests WHERE did = $1", did)
	require.NoError(t, err)

	_, err = db.Exec("DELETE FROM oauth_sessions WHERE did = $1", did)
	require.NoError(t, err)

	// Posts are deleted by CASCADE when user is deleted
	_, err = db.Exec("DELETE FROM users WHERE did = $1", did)
	require.NoError(t, err)
}

// createTestCommunity creates a minimal test community for foreign key constraints
func createTestCommunity(t *testing.T, db *sql.DB, did, handle, ownerDID string) {
	query := `
		INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
		VALUES ($1, $2, $3, $4, $4, $4, NOW())
		ON CONFLICT (did) DO NOTHING
	`
	_, err := db.Exec(query, did, handle, "Test Community", ownerDID)
	require.NoError(t, err, "Failed to create test community")
}

func TestUserRepo_Delete_Success(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testdeleteuser123"
	testHandle := "testdeleteuser123.test"
	communityDID := "did:plc:testdeletecommunity"

	defer cleanupUserData(t, db, testDID)
	defer func() {
		// Cleanup community
		_, _ = db.Exec("DELETE FROM communities WHERE did = $1", communityDID)
	}()

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create test user
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Create test community (needed for subscriptions/memberships)
	createTestCommunity(t, db, communityDID, "c.testdeletecommunity", testDID)

	// Add related data to verify cascade deletion

	// 1. OAuth session
	_, err = db.Exec(`
		INSERT INTO oauth_sessions (did, handle, pds_url, access_token, refresh_token, dpop_private_jwk, auth_server_iss, expires_at, session_id)
		VALUES ($1, $2, $3, 'test_access', 'test_refresh', '{}', 'https://auth.test', NOW() + INTERVAL '1 day', 'test_session_id')
	`, testDID, testHandle, "https://test.pds")
	require.NoError(t, err)

	// 2. Community subscription
	_, err = db.Exec(`
		INSERT INTO community_subscriptions (user_did, community_did, record_uri, record_cid)
		VALUES ($1, $2, 'at://test/sub', 'bafytest')
	`, testDID, communityDID)
	require.NoError(t, err)

	// 3. Community membership
	_, err = db.Exec(`
		INSERT INTO community_memberships (user_did, community_did)
		VALUES ($1, $2)
	`, testDID, communityDID)
	require.NoError(t, err)

	// 4. Comment (no FK constraint)
	_, err = db.Exec(`
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, 'bafycomment', 'rkey123', $2, 'at://test/post', 'bafyroot', 'at://test/post', 'bafyparent', 'Test comment', NOW())
	`, "at://"+testDID+"/social.coves.community.comment/test123", testDID)
	require.NoError(t, err)

	// 5. Vote (no FK constraint)
	_, err = db.Exec(`
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at)
		VALUES ($1, 'bafyvote', 'rkey456', $2, 'at://test/post', 'bafysubject', 'up', NOW())
	`, "at://"+testDID+"/social.coves.feed.vote/test456", testDID)
	require.NoError(t, err)

	// Verify user exists before deletion
	_, err = repo.GetByDID(ctx, testDID)
	require.NoError(t, err)

	// Delete the user
	err = repo.Delete(ctx, testDID)
	assert.NoError(t, err)

	// Verify user is deleted
	_, err = repo.GetByDID(ctx, testDID)
	assert.ErrorIs(t, err, users.ErrUserNotFound)

	// Verify related data is cleaned up
	var count int

	// OAuth sessions should be deleted
	err = db.QueryRow("SELECT COUNT(*) FROM oauth_sessions WHERE did = $1", testDID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "OAuth sessions should be deleted")

	// Community subscriptions should be deleted
	err = db.QueryRow("SELECT COUNT(*) FROM community_subscriptions WHERE user_did = $1", testDID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "Community subscriptions should be deleted")

	// Community memberships should be deleted
	err = db.QueryRow("SELECT COUNT(*) FROM community_memberships WHERE user_did = $1", testDID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "Community memberships should be deleted")

	// Comments should be deleted
	err = db.QueryRow("SELECT COUNT(*) FROM comments WHERE commenter_did = $1", testDID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "Comments should be deleted")

	// Votes should be deleted (note: the delete happens through transaction, not FK)
	err = db.QueryRow("SELECT COUNT(*) FROM votes WHERE voter_did = $1", testDID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "Votes should be deleted")
}

func TestUserRepo_Delete_NonExistentUser(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Try to delete a user that doesn't exist
	err := repo.Delete(ctx, "did:plc:nonexistentuser999")
	assert.ErrorIs(t, err, users.ErrUserNotFound)
}

func TestUserRepo_Delete_InvalidDID(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Try to delete with invalid DID format
	err := repo.Delete(ctx, "invalid-did-format")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must start with 'did:'")
}

func TestUserRepo_Delete_Idempotent(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testdeletetwice"
	testHandle := "testdeletetwice.test"

	defer cleanupUserData(t, db, testDID)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create test user
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Delete the user first time
	err = repo.Delete(ctx, testDID)
	assert.NoError(t, err)

	// Delete again - should return ErrUserNotFound (not crash)
	err = repo.Delete(ctx, testDID)
	assert.ErrorIs(t, err, users.ErrUserNotFound)
}

func TestUserRepo_Delete_WithPosts_CascadeDeletes(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testdeletewithposts"
	testHandle := "testdeletewithposts.test"
	communityDID := "did:plc:testpostcommunity"

	defer cleanupUserData(t, db, testDID)
	defer func() {
		// Cleanup posts and community
		_, _ = db.Exec("DELETE FROM posts WHERE author_did = $1", testDID)
		_, _ = db.Exec("DELETE FROM communities WHERE did = $1", communityDID)
	}()

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create test user
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Create test community (needed for post FK)
	createTestCommunity(t, db, communityDID, "c.testpostcommunity", testDID)

	// Create post (has FK constraint with CASCADE delete)
	_, err = db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, 'bafypost', 'postkey', $2, $3, 'Test Post', NOW())
	`, "at://"+communityDID+"/social.coves.community.post/testpost", testDID, communityDID)
	require.NoError(t, err)

	// Verify post exists
	var postCount int
	err = db.QueryRow("SELECT COUNT(*) FROM posts WHERE author_did = $1", testDID).Scan(&postCount)
	require.NoError(t, err)
	assert.Equal(t, 1, postCount)

	// Delete the user
	err = repo.Delete(ctx, testDID)
	assert.NoError(t, err)

	// Verify user is deleted
	_, err = repo.GetByDID(ctx, testDID)
	assert.ErrorIs(t, err, users.ErrUserNotFound)

	// Verify posts are cascade deleted (FK ON DELETE CASCADE)
	err = db.QueryRow("SELECT COUNT(*) FROM posts WHERE author_did = $1", testDID).Scan(&postCount)
	require.NoError(t, err)
	assert.Equal(t, 0, postCount, "Posts should be cascade deleted with user")
}

func TestUserRepo_Delete_TransactionRollback(t *testing.T) {
	// This test verifies that if any part of the deletion fails,
	// the entire transaction is rolled back and no partial deletions occur.
	// We can't easily simulate a failure in the middle of the transaction,
	// but we verify that the function properly handles the transaction.
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testtransaction"
	testHandle := "testtransaction.test"

	defer cleanupUserData(t, db, testDID)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create test user
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Create a cancelled context to simulate a failure
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel() // Cancel immediately

	// Try to delete with cancelled context
	err = repo.Delete(cancelledCtx, testDID)
	assert.Error(t, err, "Should fail with cancelled context")

	// Verify user still exists (transaction was rolled back)
	_, err = repo.GetByDID(ctx, testDID)
	assert.NoError(t, err, "User should still exist after failed deletion")
}

func TestUserRepo_Create(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testcreateuser"
	testHandle := "testcreateuser.test"

	defer cleanupUserData(t, db, testDID)

	repo := NewUserRepository(db)
	ctx := context.Background()

	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}

	created, err := repo.Create(ctx, user)
	assert.NoError(t, err)
	assert.Equal(t, testDID, created.DID)
	assert.Equal(t, testHandle, created.Handle)
	assert.NotZero(t, created.CreatedAt)
}

func TestUserRepo_Create_DuplicateDID(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testduplicatedid"
	testHandle := "testduplicatedid.test"

	defer cleanupUserData(t, db, testDID)

	repo := NewUserRepository(db)
	ctx := context.Background()

	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}

	// Create first time
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Try to create again with same DID
	user2 := &users.User{
		DID:    testDID,
		Handle: "different.handle.test",
		PDSURL: "https://test.pds",
	}

	_, err = repo.Create(ctx, user2)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "user with DID already exists")
}

func TestUserRepo_GetByDID(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testgetbydid"
	testHandle := "testgetbydid.test"

	defer cleanupUserData(t, db, testDID)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create user first
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Get by DID
	retrieved, err := repo.GetByDID(ctx, testDID)
	assert.NoError(t, err)
	assert.Equal(t, testDID, retrieved.DID)
	assert.Equal(t, testHandle, retrieved.Handle)
}

func TestUserRepo_GetByDID_NotFound(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	repo := NewUserRepository(db)
	ctx := context.Background()

	_, err := repo.GetByDID(ctx, "did:plc:nonexistent")
	assert.ErrorIs(t, err, users.ErrUserNotFound)
}

func TestUserRepo_GetByHandle(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testgetbyhandle"
	testHandle := "testgetbyhandle.test"

	defer cleanupUserData(t, db, testDID)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create user first
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Get by handle
	retrieved, err := repo.GetByHandle(ctx, testHandle)
	assert.NoError(t, err)
	assert.Equal(t, testDID, retrieved.DID)
	assert.Equal(t, testHandle, retrieved.Handle)
}

func TestUserRepo_UpdateHandle(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testupdatehandle"
	oldHandle := "testupdatehandle.test"
	newHandle := "newhandle.test"

	defer cleanupUserData(t, db, testDID)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create user first
	user := &users.User{
		DID:    testDID,
		Handle: oldHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Update handle
	updated, err := repo.UpdateHandle(ctx, testDID, newHandle)
	assert.NoError(t, err)
	assert.Equal(t, newHandle, updated.Handle)

	// Verify by fetching again
	retrieved, err := repo.GetByDID(ctx, testDID)
	assert.NoError(t, err)
	assert.Equal(t, newHandle, retrieved.Handle)
}

func TestUserRepo_GetProfileStats(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testprofilestats"
	testHandle := "testprofilestats.test"
	communityDID := "did:plc:teststatscommunity"

	defer cleanupUserData(t, db, testDID)
	defer func() {
		_, _ = db.Exec("DELETE FROM communities WHERE did = $1", communityDID)
	}()

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create user first
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Create test community
	createTestCommunity(t, db, communityDID, "c.teststatscommunity", testDID)

	// Add subscription
	_, err = db.Exec(`
		INSERT INTO community_subscriptions (user_did, community_did, record_uri, record_cid)
		VALUES ($1, $2, 'at://test/sub', 'bafytest')
	`, testDID, communityDID)
	require.NoError(t, err)

	// Add membership
	_, err = db.Exec(`
		INSERT INTO community_memberships (user_did, community_did, reputation_score)
		VALUES ($1, $2, 100)
	`, testDID, communityDID)
	require.NoError(t, err)

	// Add post
	_, err = db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, 'bafystatpost', 'statpostkey', $2, $3, 'Stats Test Post', NOW())
	`, "at://"+communityDID+"/social.coves.community.post/statspost", testDID, communityDID)
	require.NoError(t, err)

	// Add comment
	_, err = db.Exec(`
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, 'bafystatcomment', 'statcommentkey', $2, 'at://test/post', 'bafyroot', 'at://test/post', 'bafyparent', 'Stats Test Comment', NOW())
	`, "at://"+testDID+"/social.coves.community.comment/statscomment", testDID)
	require.NoError(t, err)

	// Get profile stats
	stats, err := repo.GetProfileStats(ctx, testDID)
	assert.NoError(t, err)
	assert.Equal(t, 1, stats.PostCount)
	assert.Equal(t, 1, stats.CommentCount)
	assert.Equal(t, 1, stats.CommunityCount)
	assert.Equal(t, 1, stats.MembershipCount)
	assert.Equal(t, 100, stats.Reputation)
}

func TestUserRepo_Delete_WithOAuthRequests(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testoauthrequests"
	testHandle := "testoauthrequests.test"

	defer cleanupUserData(t, db, testDID)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create test user
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Add OAuth request (pending authorization)
	_, err = db.Exec(`
		INSERT INTO oauth_requests (state, did, handle, pds_url, pkce_verifier, dpop_private_jwk, auth_server_iss)
		VALUES ($1, $2, $3, $4, 'verifier', '{}', 'https://auth.test')
	`, "test_state_"+testDID, testDID, testHandle, "https://test.pds")
	require.NoError(t, err)

	// Delete the user
	err = repo.Delete(ctx, testDID)
	assert.NoError(t, err)

	// Verify OAuth requests are deleted
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM oauth_requests WHERE did = $1", testDID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "OAuth requests should be deleted")
}

func TestUserRepo_Delete_WithCommunityBlocks(t *testing.T) {
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testcommunityblocks"
	testHandle := "testcommunityblocks.test"
	communityDID := "did:plc:testblockcommunity"

	defer cleanupUserData(t, db, testDID)
	defer func() {
		_, _ = db.Exec("DELETE FROM communities WHERE did = $1", communityDID)
	}()

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create test user
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Create test community
	createTestCommunity(t, db, communityDID, "c.testblockcommunity", testDID)

	// Add community block
	_, err = db.Exec(`
		INSERT INTO community_blocks (user_did, community_did, record_uri, record_cid)
		VALUES ($1, $2, 'at://test/block', 'bafyblock')
	`, testDID, communityDID)
	require.NoError(t, err)

	// Delete the user
	err = repo.Delete(ctx, testDID)
	assert.NoError(t, err)

	// Verify community blocks are deleted
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM community_blocks WHERE user_did = $1", testDID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "Community blocks should be deleted")
}

func TestUserRepo_Delete_TimingPerformance(t *testing.T) {
	// This test ensures deletion completes in a reasonable time
	// even with multiple related records
	db := setupUserTestDB(t)
	defer func() { _ = db.Close() }()

	testDID := "did:plc:testperformance"
	testHandle := "testperformance.test"
	communityDID := "did:plc:testperfcommunity"

	// Clean up any leftover data from previous test runs
	cleanupUserData(t, db, testDID)
	_, _ = db.Exec("DELETE FROM communities WHERE did = $1", communityDID)

	defer cleanupUserData(t, db, testDID)
	defer func() {
		_, _ = db.Exec("DELETE FROM communities WHERE did = $1", communityDID)
	}()

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create test user
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	_, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Create test community
	createTestCommunity(t, db, communityDID, "c.testperfcommunity", testDID)

	// Add multiple comments
	for i := 0; i < 10; i++ {
		_, err = db.Exec(`
			INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
			VALUES ($1, $2, $3, $4, 'at://test/post', 'bafyroot', 'at://test/post', 'bafyparent', 'Test comment', NOW())
		`, "at://"+testDID+"/social.coves.community.comment/perf"+string(rune('0'+i)), "bafyperf"+string(rune('0'+i)), "perfkey"+string(rune('0'+i)), testDID)
		require.NoError(t, err)
	}

	// Add multiple votes (each must have unique subject_uri due to unique_voter_subject_active constraint)
	for i := 0; i < 10; i++ {
		subjectURI := fmt.Sprintf("at://test/post/perf%d", i)
		_, err = db.Exec(`
			INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at)
			VALUES ($1, $2, $3, $4, $5, 'bafysubject', 'up', NOW())
		`, "at://"+testDID+"/social.coves.feed.vote/perf"+string(rune('0'+i)), "bafyvoteperf"+string(rune('0'+i)), "voteperfkey"+string(rune('0'+i)), testDID, subjectURI)
		require.NoError(t, err)
	}

	// Time the deletion
	start := time.Now()
	err = repo.Delete(ctx, testDID)
	elapsed := time.Since(start)

	assert.NoError(t, err)
	assert.Less(t, elapsed, 5*time.Second, "Deletion should complete in under 5 seconds")

	t.Logf("Deletion of user with %d comments and %d votes took %v", 10, 10, elapsed)
}
