//go:build integration

package postgres

import (
	"Coves/internal/core/users"
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testdeleteuser123"
	testHandle := "testdeleteuser123.test"
	communityDID := "did:plc:testdeletecommunity"

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
	t.Parallel()
	db := testkit.DB(t)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Try to delete a user that doesn't exist
	err := repo.Delete(ctx, "did:plc:nonexistentuser999")
	assert.ErrorIs(t, err, users.ErrUserNotFound)
}

func TestUserRepo_Delete_InvalidDID(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Try to delete with invalid DID format
	err := repo.Delete(ctx, "invalid-did-format")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must start with 'did:'")
}

func TestUserRepo_Delete_Idempotent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testdeletetwice"
	testHandle := "testdeletetwice.test"

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
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testdeletewithposts"
	testHandle := "testdeletewithposts.test"
	communityDID := "did:plc:testpostcommunity"

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

	// Create post. Deletion used to reach this row through fk_author's ON DELETE
	// CASCADE; migration 034 dropped that FK (PRD_AUTHOR_OWNED_POSTS §5.3, so a
	// federated author's post can be indexed at all), and Delete now removes
	// posts with an explicit statement instead. The assertion below is unchanged
	// on purpose: deleting a user must still take their posts with it, whichever
	// mechanism does it.
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

	// Verify the posts went with the user (explicit DELETE since migration 034)
	err = db.QueryRow("SELECT COUNT(*) FROM posts WHERE author_did = $1", testDID).Scan(&postCount)
	require.NoError(t, err)
	assert.Equal(t, 0, postCount, "Deleting a user must still delete their posts")
}

func TestUserRepo_Delete_TransactionRollback(t *testing.T) {
	t.Parallel()
	// This test verifies that if any part of the deletion fails,
	// the entire transaction is rolled back and no partial deletions occur.
	// We can't easily simulate a failure in the middle of the transaction,
	// but we verify that the function properly handles the transaction.
	db := testkit.DB(t)

	testDID := "did:plc:testtransaction"
	testHandle := "testtransaction.test"

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
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testcreateuser"
	testHandle := "testcreateuser.test"

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
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testduplicatedid"
	testHandle := "testduplicatedid.test"

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
	assert.ErrorIs(t, err, users.ErrUserAlreadyExists)
}

func TestUserRepo_GetByDID(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testgetbydid"
	testHandle := "testgetbydid.test"

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
	t.Parallel()
	db := testkit.DB(t)

	repo := NewUserRepository(db)
	ctx := context.Background()

	_, err := repo.GetByDID(ctx, "did:plc:nonexistent")
	assert.ErrorIs(t, err, users.ErrUserNotFound)
}

func TestUserRepo_GetByHandle(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testgetbyhandle"
	testHandle := "testgetbyhandle.test"

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
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testupdatehandle"
	oldHandle := "testupdatehandle.test"
	newHandle := "newhandle.test"

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
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testprofilestats"
	testHandle := "testprofilestats.test"
	communityDID := "did:plc:teststatscommunity"

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
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testoauthrequests"
	testHandle := "testoauthrequests.test"

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
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testcommunityblocks"
	testHandle := "testcommunityblocks.test"
	communityDID := "did:plc:testblockcommunity"

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
	t.Parallel()
	// This test ensures deletion completes in a reasonable time
	// even with multiple related records
	db := testkit.DB(t)

	testDID := "did:plc:testperformance"
	testHandle := "testperformance.test"
	communityDID := "did:plc:testperfcommunity"

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

	// No wall-clock assertion. This test now runs alongside dozens of parallel
	// peers competing for the same Postgres, so elapsed time here measures the
	// machine's load, not the query — and the failure it would produce is a
	// flake that reads like a performance regression. What the test proves is
	// that a cascade delete over this much related data completes correctly;
	// the duration is logged for a human, not asserted.
	start := time.Now()
	err = repo.Delete(ctx, testDID)
	elapsed := time.Since(start)

	assert.NoError(t, err)

	t.Logf("Deletion of user with %d comments and %d votes took %v", 10, 10, elapsed)
}

// ============================================================================
// Profile Update Tests (Phase 2: User Profile Avatar & Banner)
// ============================================================================

// stringPtr returns a pointer to the provided string (helper for optional params)
func stringPtr(s string) *string {
	return &s
}

func TestUserRepo_UpdateProfile(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testupdateprofile"
	testHandle := "testupdateprofile.test"

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

	// Update profile with all fields
	displayName := "Test User"
	bio := "A test user biography"
	avatarCID := "bafyavatarcid123"
	bannerCID := "bafybannercid456"

	input := users.UpdateProfileInput{
		DisplayName: &displayName,
		Bio:         &bio,
		AvatarCID:   &avatarCID,
		BannerCID:   &bannerCID,
	}
	updated, err := repo.UpdateProfile(ctx, testDID, input)
	assert.NoError(t, err)
	require.NotNil(t, updated)

	// Verify all fields were updated
	assert.Equal(t, testDID, updated.DID)
	assert.Equal(t, testHandle, updated.Handle)
	assert.Equal(t, displayName, updated.DisplayName)
	assert.Equal(t, bio, updated.Bio)
	assert.Equal(t, avatarCID, updated.AvatarCID)
	assert.Equal(t, bannerCID, updated.BannerCID)
}

func TestUserRepo_UpdateProfile_PartialUpdate(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testpartialupdate"
	testHandle := "testpartialupdate.test"

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

	// First update: set display name and avatar
	displayName := "Initial Name"
	avatarCID := "bafyinitialavatar"
	input1 := users.UpdateProfileInput{
		DisplayName: &displayName,
		AvatarCID:   &avatarCID,
	}
	_, err = repo.UpdateProfile(ctx, testDID, input1)
	require.NoError(t, err)

	// Second update: only update bio (leave other fields alone)
	bio := "New bio text"
	input2 := users.UpdateProfileInput{
		Bio: &bio,
	}
	updated, err := repo.UpdateProfile(ctx, testDID, input2)
	assert.NoError(t, err)
	require.NotNil(t, updated)

	// Verify bio was updated
	assert.Equal(t, bio, updated.Bio)

	// Verify previous values are preserved (nil means "don't change")
	assert.Equal(t, displayName, updated.DisplayName)
	assert.Equal(t, avatarCID, updated.AvatarCID)
	assert.Empty(t, updated.BannerCID) // Was never set
}

func TestUserRepo_UpdateProfile_ReturnsUpdatedUser(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testreturnsupdated"
	testHandle := "testreturnsupdated.test"

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create user first
	user := &users.User{
		DID:    testDID,
		Handle: testHandle,
		PDSURL: "https://test.pds",
	}
	created, err := repo.Create(ctx, user)
	require.NoError(t, err)

	// Update profile
	displayName := "Updated Name"
	input := users.UpdateProfileInput{
		DisplayName: &displayName,
	}
	updated, err := repo.UpdateProfile(ctx, testDID, input)
	assert.NoError(t, err)
	require.NotNil(t, updated)

	// Verify the returned user has all core fields populated
	assert.Equal(t, testDID, updated.DID)
	assert.Equal(t, testHandle, updated.Handle)
	assert.Equal(t, "https://test.pds", updated.PDSURL)
	assert.Equal(t, displayName, updated.DisplayName)
	assert.NotZero(t, updated.CreatedAt)
	assert.NotZero(t, updated.UpdatedAt)

	// UpdatedAt should be after CreatedAt (or equal if very fast)
	assert.True(t, updated.UpdatedAt.After(created.CreatedAt) || updated.UpdatedAt.Equal(created.CreatedAt))
}

func TestUserRepo_UpdateProfile_UserNotFound(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Try to update a non-existent user
	displayName := "Ghost User"
	input := users.UpdateProfileInput{
		DisplayName: &displayName,
	}
	_, err := repo.UpdateProfile(ctx, "did:plc:nonexistentuserprofile", input)
	assert.ErrorIs(t, err, users.ErrUserNotFound)
}

func TestUserRepo_UpdateProfile_ClearFields(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testclearfields"
	testHandle := "testclearfields.test"

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

	// Set profile fields
	displayName := "Has Name"
	bio := "Has Bio"
	avatarCID := "bafyhasavatar"
	input1 := users.UpdateProfileInput{
		DisplayName: &displayName,
		Bio:         &bio,
		AvatarCID:   &avatarCID,
	}
	_, err = repo.UpdateProfile(ctx, testDID, input1)
	require.NoError(t, err)

	// Clear display name by passing empty string
	emptyName := ""
	input2 := users.UpdateProfileInput{
		DisplayName: &emptyName,
	}
	updated, err := repo.UpdateProfile(ctx, testDID, input2)
	assert.NoError(t, err)
	require.NotNil(t, updated)

	// Verify display name was cleared
	assert.Empty(t, updated.DisplayName)
	// Other fields should remain
	assert.Equal(t, bio, updated.Bio)
	assert.Equal(t, avatarCID, updated.AvatarCID)
}

func TestUserRepo_GetByDID_ReturnsNewFields(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testgetbydidnewfields"
	testHandle := "testgetbydidnewfields.test"

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

	// Update profile with all fields
	displayName := "Profile Name"
	bio := "Profile bio for testing"
	avatarCID := "bafyprofileavatar"
	bannerCID := "bafyprofilebanner"
	input := users.UpdateProfileInput{
		DisplayName: &displayName,
		Bio:         &bio,
		AvatarCID:   &avatarCID,
		BannerCID:   &bannerCID,
	}
	_, err = repo.UpdateProfile(ctx, testDID, input)
	require.NoError(t, err)

	// Retrieve user with GetByDID
	retrieved, err := repo.GetByDID(ctx, testDID)
	assert.NoError(t, err)
	require.NotNil(t, retrieved)

	// Verify all profile fields are returned
	assert.Equal(t, testDID, retrieved.DID)
	assert.Equal(t, testHandle, retrieved.Handle)
	assert.Equal(t, displayName, retrieved.DisplayName)
	assert.Equal(t, bio, retrieved.Bio)
	assert.Equal(t, avatarCID, retrieved.AvatarCID)
	assert.Equal(t, bannerCID, retrieved.BannerCID)
}

func TestUserRepo_GetByHandle_ReturnsNewFields(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID := "did:plc:testgetbyhandlenewfields"
	testHandle := "testgetbyhandlenewfields.test"

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

	// Update profile with all fields
	displayName := "Handle Test Name"
	bio := "Handle test bio"
	avatarCID := "bafyhandleavatar"
	bannerCID := "bafyhandlebanner"
	input := users.UpdateProfileInput{
		DisplayName: &displayName,
		Bio:         &bio,
		AvatarCID:   &avatarCID,
		BannerCID:   &bannerCID,
	}
	_, err = repo.UpdateProfile(ctx, testDID, input)
	require.NoError(t, err)

	// Retrieve user with GetByHandle
	retrieved, err := repo.GetByHandle(ctx, testHandle)
	assert.NoError(t, err)
	require.NotNil(t, retrieved)

	// Verify all profile fields are returned
	assert.Equal(t, testDID, retrieved.DID)
	assert.Equal(t, testHandle, retrieved.Handle)
	assert.Equal(t, displayName, retrieved.DisplayName)
	assert.Equal(t, bio, retrieved.Bio)
	assert.Equal(t, avatarCID, retrieved.AvatarCID)
	assert.Equal(t, bannerCID, retrieved.BannerCID)
}

func TestUpdateProfile_InvalidDID(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewUserRepository(db)
	ctx := context.Background()

	displayName := "Test"
	input := users.UpdateProfileInput{DisplayName: &displayName}

	_, err := repo.UpdateProfile(ctx, "invalid-did", input)

	require.Error(t, err)
	var didErr *users.InvalidDIDError
	require.ErrorAs(t, err, &didErr)
	assert.Equal(t, "invalid-did", didErr.DID)
}

func TestUserRepo_GetByDIDs_ReturnsNewFields(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	testDID1 := "did:plc:testgetbydidsbatch1"
	testHandle1 := "testgetbydidsbatch1.test"
	testDID2 := "did:plc:testgetbydidsbatch2"
	testHandle2 := "testgetbydidsbatch2.test"

	repo := NewUserRepository(db)
	ctx := context.Background()

	// Create users
	user1 := &users.User{DID: testDID1, Handle: testHandle1, PDSURL: "https://test.pds"}
	user2 := &users.User{DID: testDID2, Handle: testHandle2, PDSURL: "https://test.pds"}
	_, err := repo.Create(ctx, user1)
	require.NoError(t, err)
	_, err = repo.Create(ctx, user2)
	require.NoError(t, err)

	// Update profiles
	displayName1 := "Batch User 1"
	avatarCID1 := "bafybatchavatar1"
	displayName2 := "Batch User 2"
	bio2 := "Batch user 2 bio"
	input1 := users.UpdateProfileInput{
		DisplayName: &displayName1,
		AvatarCID:   &avatarCID1,
	}
	_, err = repo.UpdateProfile(ctx, testDID1, input1)
	require.NoError(t, err)
	input2 := users.UpdateProfileInput{
		DisplayName: &displayName2,
		Bio:         &bio2,
	}
	_, err = repo.UpdateProfile(ctx, testDID2, input2)
	require.NoError(t, err)

	// Retrieve with GetByDIDs
	userMap, err := repo.GetByDIDs(ctx, []string{testDID1, testDID2})
	assert.NoError(t, err)
	assert.Len(t, userMap, 2)

	// Verify user 1
	u1 := userMap[testDID1]
	require.NotNil(t, u1)
	assert.Equal(t, displayName1, u1.DisplayName)
	assert.Equal(t, avatarCID1, u1.AvatarCID)
	assert.Empty(t, u1.Bio)

	// Verify user 2
	u2 := userMap[testDID2]
	require.NotNil(t, u2)
	assert.Equal(t, displayName2, u2.DisplayName)
	assert.Equal(t, bio2, u2.Bio)
	assert.Empty(t, u2.AvatarCID)
}
