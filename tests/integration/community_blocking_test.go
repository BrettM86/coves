package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	postgresRepo "Coves/internal/db/postgres"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// TestCommunityBlocking_Indexing tests Jetstream indexing of block events
func TestCommunityBlocking_Indexing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupBlockingTestDB(t, db)

	repo := createBlockingTestCommunityRepo(t, db)
	consumer := jetstream.NewCommunityEventConsumer(repo)

	// Create test community
	testDID := fmt.Sprintf("did:plc:test-community-%d", time.Now().UnixNano())
	community := createBlockingTestCommunity(t, repo, "test-community-blocking", testDID)

	t.Run("indexes block CREATE event", func(t *testing.T) {
		userDID := "did:plc:test-user-blocker"
		rkey := "test-block-1"

		// Simulate Jetstream CREATE event
		event := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-1",
				Operation:  "create",
				Collection: "social.coves.community.block",
				RKey:       rkey,
				CID:        "bafyblock123",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.block",
					"subject":   community.DID,
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// Process event
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Failed to handle block event: %v", err)
		}

		// Verify block indexed
		block, err := repo.GetBlock(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("Failed to get block: %v", err)
		}

		if block.UserDID != userDID {
			t.Errorf("Expected userDID=%s, got %s", userDID, block.UserDID)
		}
		if block.CommunityDID != community.DID {
			t.Errorf("Expected communityDID=%s, got %s", community.DID, block.CommunityDID)
		}

		// Verify IsBlocked works
		isBlocked, err := repo.IsBlocked(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("IsBlocked failed: %v", err)
		}
		if !isBlocked {
			t.Error("Expected IsBlocked=true, got false")
		}
	})

	t.Run("indexes block DELETE event", func(t *testing.T) {
		userDID := "did:plc:test-user-unblocker"
		rkey := "test-block-2"
		uri := fmt.Sprintf("at://%s/social.coves.community.block/%s", userDID, rkey)

		// First create a block
		block := &communities.CommunityBlock{
			UserDID:      userDID,
			CommunityDID: community.DID,
			BlockedAt:    time.Now(),
			RecordURI:    uri,
			RecordCID:    "bafyblock456",
		}
		_, err := repo.BlockCommunity(ctx, block)
		if err != nil {
			t.Fatalf("Failed to create block: %v", err)
		}

		// Simulate DELETE event
		event := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-2",
				Operation:  "delete",
				Collection: "social.coves.community.block",
				RKey:       rkey,
			},
		}

		// Process delete
		err = consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Failed to handle delete event: %v", err)
		}

		// Verify block removed
		_, err = repo.GetBlock(ctx, userDID, community.DID)
		if !communities.IsNotFound(err) {
			t.Error("Expected block to be deleted")
		}

		// Verify IsBlocked returns false
		isBlocked, err := repo.IsBlocked(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("IsBlocked failed: %v", err)
		}
		if isBlocked {
			t.Error("Expected IsBlocked=false, got true")
		}
	})

	t.Run("block is idempotent", func(t *testing.T) {
		userDID := "did:plc:test-user-idempotent"
		rkey := "test-block-3"

		event := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-3",
				Operation:  "create",
				Collection: "social.coves.community.block",
				RKey:       rkey,
				CID:        "bafyblock789",
				Record: map[string]interface{}{
					"$type":     "social.coves.community.block",
					"subject":   community.DID,
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// Process event twice
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("First block failed: %v", err)
		}

		err = consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Second block (idempotent) failed: %v", err)
		}

		// Should still exist only once
		blocks, err := repo.ListBlockedCommunities(ctx, userDID, 10, 0)
		if err != nil {
			t.Fatalf("ListBlockedCommunities failed: %v", err)
		}
		if len(blocks) != 1 {
			t.Errorf("Expected 1 block, got %d", len(blocks))
		}
	})

	t.Run("handles DELETE of non-existent block gracefully", func(t *testing.T) {
		userDID := "did:plc:test-user-nonexistent"
		rkey := "test-block-nonexistent"

		// Simulate DELETE event for block that doesn't exist
		event := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-99",
				Operation:  "delete",
				Collection: "social.coves.community.block",
				RKey:       rkey,
			},
		}

		// Should not error (idempotent)
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("DELETE of non-existent block should be idempotent, got error: %v", err)
		}
	})
}

// TestCommunityBlocking_ListBlocked tests listing blocked communities
func TestCommunityBlocking_ListBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupBlockingTestDB(t, db)

	repo := createBlockingTestCommunityRepo(t, db)
	userDID := "did:plc:test-user-list"

	// Create and block 3 communities
	testCommunities := make([]*communities.Community, 3)
	for i := 0; i < 3; i++ {
		communityDID := fmt.Sprintf("did:plc:test-community-list-%d", i)
		testCommunities[i] = createBlockingTestCommunity(t, repo, fmt.Sprintf("community-list-%d", i), communityDID)

		block := &communities.CommunityBlock{
			UserDID:      userDID,
			CommunityDID: testCommunities[i].DID,
			BlockedAt:    time.Now(),
			RecordURI:    fmt.Sprintf("at://%s/social.coves.community.block/%d", userDID, i),
			RecordCID:    fmt.Sprintf("bafyblock%d", i),
		}
		_, err := repo.BlockCommunity(ctx, block)
		if err != nil {
			t.Fatalf("Failed to block community %d: %v", i, err)
		}
	}

	t.Run("lists all blocked communities", func(t *testing.T) {
		blocks, err := repo.ListBlockedCommunities(ctx, userDID, 10, 0)
		if err != nil {
			t.Fatalf("ListBlockedCommunities failed: %v", err)
		}

		if len(blocks) != 3 {
			t.Errorf("Expected 3 blocks, got %d", len(blocks))
		}

		// Verify all blocks belong to correct user
		for _, block := range blocks {
			if block.UserDID != userDID {
				t.Errorf("Expected userDID=%s, got %s", userDID, block.UserDID)
			}
		}
	})

	t.Run("pagination works correctly", func(t *testing.T) {
		// Get first 2
		blocks, err := repo.ListBlockedCommunities(ctx, userDID, 2, 0)
		if err != nil {
			t.Fatalf("ListBlockedCommunities with limit failed: %v", err)
		}
		if len(blocks) != 2 {
			t.Errorf("Expected 2 blocks (paginated), got %d", len(blocks))
		}

		// Get next 2 (should only get 1)
		blocksPage2, err := repo.ListBlockedCommunities(ctx, userDID, 2, 2)
		if err != nil {
			t.Fatalf("ListBlockedCommunities page 2 failed: %v", err)
		}
		if len(blocksPage2) != 1 {
			t.Errorf("Expected 1 block on page 2, got %d", len(blocksPage2))
		}
	})

	t.Run("returns empty list for user with no blocks", func(t *testing.T) {
		blocks, err := repo.ListBlockedCommunities(ctx, "did:plc:user-no-blocks", 10, 0)
		if err != nil {
			t.Fatalf("ListBlockedCommunities failed: %v", err)
		}
		if len(blocks) != 0 {
			t.Errorf("Expected 0 blocks, got %d", len(blocks))
		}
	})
}

// TestCommunityBlocking_IsBlocked tests the fast block check
func TestCommunityBlocking_IsBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupBlockingTestDB(t, db)

	repo := createBlockingTestCommunityRepo(t, db)

	userDID := "did:plc:test-user-isblocked"
	communityDID := fmt.Sprintf("did:plc:test-community-%d", time.Now().UnixNano())
	community := createBlockingTestCommunity(t, repo, "test-community-isblocked", communityDID)

	t.Run("returns false when not blocked", func(t *testing.T) {
		isBlocked, err := repo.IsBlocked(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("IsBlocked failed: %v", err)
		}
		if isBlocked {
			t.Error("Expected IsBlocked=false, got true")
		}
	})

	t.Run("returns true when blocked", func(t *testing.T) {
		// Create block
		block := &communities.CommunityBlock{
			UserDID:      userDID,
			CommunityDID: community.DID,
			BlockedAt:    time.Now(),
			RecordURI:    fmt.Sprintf("at://%s/social.coves.community.block/test", userDID),
			RecordCID:    "bafyblocktest",
		}
		_, err := repo.BlockCommunity(ctx, block)
		if err != nil {
			t.Fatalf("Failed to create block: %v", err)
		}

		// Check IsBlocked
		isBlocked, err := repo.IsBlocked(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("IsBlocked failed: %v", err)
		}
		if !isBlocked {
			t.Error("Expected IsBlocked=true, got false")
		}
	})

	t.Run("returns false after unblock", func(t *testing.T) {
		// Unblock
		err := repo.UnblockCommunity(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("UnblockCommunity failed: %v", err)
		}

		// Check IsBlocked
		isBlocked, err := repo.IsBlocked(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("IsBlocked failed: %v", err)
		}
		if isBlocked {
			t.Error("Expected IsBlocked=false after unblock, got true")
		}
	})
}

// TestCommunityBlocking_GetBlock tests block retrieval
func TestCommunityBlocking_GetBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupBlockingTestDB(t, db)

	repo := createBlockingTestCommunityRepo(t, db)

	userDID := "did:plc:test-user-getblock"
	communityDID := fmt.Sprintf("did:plc:test-community-%d", time.Now().UnixNano())
	community := createBlockingTestCommunity(t, repo, "test-community-getblock", communityDID)

	t.Run("returns error when block doesn't exist", func(t *testing.T) {
		_, err := repo.GetBlock(ctx, userDID, community.DID)
		if !communities.IsNotFound(err) {
			t.Errorf("Expected ErrBlockNotFound, got: %v", err)
		}
	})

	t.Run("retrieves block by user and community DID", func(t *testing.T) {
		// Create block
		recordURI := fmt.Sprintf("at://%s/social.coves.community.block/test-getblock", userDID)
		originalBlock := &communities.CommunityBlock{
			UserDID:      userDID,
			CommunityDID: community.DID,
			BlockedAt:    time.Now(),
			RecordURI:    recordURI,
			RecordCID:    "bafyblockgettest",
		}
		_, err := repo.BlockCommunity(ctx, originalBlock)
		if err != nil {
			t.Fatalf("Failed to create block: %v", err)
		}

		// Retrieve by user+community
		block, err := repo.GetBlock(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("GetBlock failed: %v", err)
		}

		if block.UserDID != userDID {
			t.Errorf("Expected userDID=%s, got %s", userDID, block.UserDID)
		}
		if block.CommunityDID != community.DID {
			t.Errorf("Expected communityDID=%s, got %s", community.DID, block.CommunityDID)
		}
		if block.RecordURI != recordURI {
			t.Errorf("Expected recordURI=%s, got %s", recordURI, block.RecordURI)
		}
	})

	t.Run("retrieves block by URI", func(t *testing.T) {
		recordURI := fmt.Sprintf("at://%s/social.coves.community.block/test-getblock", userDID)

		// Retrieve by URI
		block, err := repo.GetBlockByURI(ctx, recordURI)
		if err != nil {
			t.Fatalf("GetBlockByURI failed: %v", err)
		}

		if block.RecordURI != recordURI {
			t.Errorf("Expected recordURI=%s, got %s", recordURI, block.RecordURI)
		}
		if block.CommunityDID != community.DID {
			t.Errorf("Expected communityDID=%s, got %s", community.DID, block.CommunityDID)
		}
	})
}

// Helper functions for blocking tests

func createBlockingTestCommunityRepo(t *testing.T, db *sql.DB) communities.Repository {
	return postgresRepo.NewCommunityRepository(db)
}

func createBlockingTestCommunity(t *testing.T, repo communities.Repository, name, did string) *communities.Community {
	community := &communities.Community{
		DID:          did,
		Handle:       fmt.Sprintf("!%s@coves.test", name),
		Name:         name,
		DisplayName:  fmt.Sprintf("Test Community %s", name),
		Description:  "Test community for blocking tests",
		OwnerDID:     did,
		CreatedByDID: "did:plc:test-creator",
		HostedByDID:  "did:plc:test-instance",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	created, err := repo.Create(context.Background(), community)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	return created
}

func cleanupBlockingTestDB(t *testing.T, db *sql.DB) {
	// Clean up test data
	_, err := db.Exec("DELETE FROM community_blocks WHERE user_did LIKE 'did:plc:test-%'")
	if err != nil {
		t.Logf("Warning: Failed to clean up blocks: %v", err)
	}

	_, err = db.Exec("DELETE FROM communities WHERE did LIKE 'did:plc:test-community-%'")
	if err != nil {
		t.Logf("Warning: Failed to clean up communities: %v", err)
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Logf("Failed to close database: %v", closeErr)
	}
}
