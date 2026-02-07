package integration

import (
	"Coves/internal/core/userblocks"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	postgresRepo "Coves/internal/db/postgres"
)

// TestUserBlockRepo_BlockUser tests creating user blocks
func TestUserBlockRepo_BlockUser(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupUserBlockTestDB(t, db)

	repo := postgresRepo.NewUserBlockRepository(db)

	t.Run("creates block successfully", func(t *testing.T) {
		// Save expected values independently — BlockUser mutates the input pointer,
		// so comparing created vs block fields would be self-referential.
		expectedBlockerDID := "did:plc:test-blocker-1"
		expectedBlockedDID := "did:plc:test-blocked-1"
		expectedRecordURI := "at://did:plc:test-blocker-1/social.coves.actor.block/abc123"
		expectedRecordCID := "bafyblock123"

		block := &userblocks.UserBlock{
			BlockerDID: expectedBlockerDID,
			BlockedDID: expectedBlockedDID,
			BlockedAt:  time.Now(),
			RecordURI:  expectedRecordURI,
			RecordCID:  expectedRecordCID,
		}

		created, err := repo.BlockUser(ctx, block)
		if err != nil {
			t.Fatalf("BlockUser failed: %v", err)
		}

		if created.ID == 0 {
			t.Error("Expected non-zero ID")
		}
		if created.BlockerDID != expectedBlockerDID {
			t.Errorf("Expected BlockerDID=%s, got %s", expectedBlockerDID, created.BlockerDID)
		}
		if created.BlockedDID != expectedBlockedDID {
			t.Errorf("Expected BlockedDID=%s, got %s", expectedBlockedDID, created.BlockedDID)
		}
		if created.RecordURI != expectedRecordURI {
			t.Errorf("Expected RecordURI=%s, got %s", expectedRecordURI, created.RecordURI)
		}
		if created.RecordCID != expectedRecordCID {
			t.Errorf("Expected RecordCID=%s, got %s", expectedRecordCID, created.RecordCID)
		}
	})

	t.Run("idempotent on duplicate block", func(t *testing.T) {
		blockerDID := "did:plc:test-blocker-idem"
		blockedDID := "did:plc:test-blocked-idem"

		block1 := &userblocks.UserBlock{
			BlockerDID: blockerDID,
			BlockedDID: blockedDID,
			BlockedAt:  time.Now(),
			RecordURI:  "at://did:plc:test-blocker-idem/social.coves.actor.block/first",
			RecordCID:  "bafyfirst",
		}

		firstBlock, err := repo.BlockUser(ctx, block1)
		if err != nil {
			t.Fatalf("First BlockUser failed: %v", err)
		}
		firstBlockID := firstBlock.ID

		// Insert again with updated URI/CID (ON CONFLICT DO UPDATE)
		expectedURI := "at://did:plc:test-blocker-idem/social.coves.actor.block/second"
		expectedCID := "bafysecond"
		block2 := &userblocks.UserBlock{
			BlockerDID: blockerDID,
			BlockedDID: blockedDID,
			BlockedAt:  time.Now(),
			RecordURI:  expectedURI,
			RecordCID:  expectedCID,
		}

		updated, err := repo.BlockUser(ctx, block2)
		if err != nil {
			t.Fatalf("Second BlockUser (idempotent) failed: %v", err)
		}

		// Core invariant: row identity should be stable across upsert
		if updated.ID != firstBlockID {
			t.Errorf("Expected stable row ID=%d after upsert, got %d", firstBlockID, updated.ID)
		}

		// Should have updated URI/CID
		if updated.RecordURI != expectedURI {
			t.Errorf("Expected updated RecordURI=%s, got %s", expectedURI, updated.RecordURI)
		}
		if updated.RecordCID != expectedCID {
			t.Errorf("Expected updated RecordCID=%s, got %s", expectedCID, updated.RecordCID)
		}

		// Should still be only 1 block
		blocks, err := repo.ListBlockedUsers(ctx, blockerDID, 10, 0)
		if err != nil {
			t.Fatalf("ListBlockedUsers failed: %v", err)
		}
		if len(blocks) != 1 {
			t.Errorf("Expected 1 block after idempotent insert, got %d", len(blocks))
		}
	})
}

// TestUserBlockRepo_UnblockUser tests removing user blocks
func TestUserBlockRepo_UnblockUser(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupUserBlockTestDB(t, db)

	repo := postgresRepo.NewUserBlockRepository(db)

	t.Run("removes existing block", func(t *testing.T) {
		blockerDID := "did:plc:test-unblocker-1"
		blockedDID := "did:plc:test-unblocked-1"

		// Create a block first
		block := &userblocks.UserBlock{
			BlockerDID: blockerDID,
			BlockedDID: blockedDID,
			BlockedAt:  time.Now(),
			RecordURI:  "at://did:plc:test-unblocker-1/social.coves.actor.block/del1",
			RecordCID:  "bafydel1",
		}
		_, err := repo.BlockUser(ctx, block)
		if err != nil {
			t.Fatalf("BlockUser failed: %v", err)
		}

		// Unblock
		err = repo.UnblockUser(ctx, blockerDID, blockedDID)
		if err != nil {
			t.Fatalf("UnblockUser failed: %v", err)
		}

		// Verify removed
		_, err = repo.GetBlock(ctx, blockerDID, blockedDID)
		if !userblocks.IsNotFound(err) {
			t.Errorf("Expected ErrBlockNotFound after unblock, got: %v", err)
		}
	})

	t.Run("returns ErrBlockNotFound for non-existent block", func(t *testing.T) {
		err := repo.UnblockUser(ctx, "did:plc:test-nonexistent-blocker", "did:plc:test-nonexistent-blocked")
		if !userblocks.IsNotFound(err) {
			t.Errorf("Expected ErrBlockNotFound, got: %v", err)
		}
	})
}

// TestUserBlockRepo_GetBlock tests block retrieval by blocker + blocked DID
func TestUserBlockRepo_GetBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupUserBlockTestDB(t, db)

	repo := postgresRepo.NewUserBlockRepository(db)

	blockerDID := "did:plc:test-getblock-blocker"
	blockedDID := "did:plc:test-getblock-blocked"

	t.Run("returns ErrBlockNotFound when not exists", func(t *testing.T) {
		_, err := repo.GetBlock(ctx, blockerDID, blockedDID)
		if !userblocks.IsNotFound(err) {
			t.Errorf("Expected ErrBlockNotFound, got: %v", err)
		}
	})

	t.Run("retrieves block by blocker + blocked DID", func(t *testing.T) {
		recordURI := "at://did:plc:test-getblock-blocker/social.coves.actor.block/get1"
		block := &userblocks.UserBlock{
			BlockerDID: blockerDID,
			BlockedDID: blockedDID,
			BlockedAt:  time.Now(),
			RecordURI:  recordURI,
			RecordCID:  "bafyget1",
		}
		_, err := repo.BlockUser(ctx, block)
		if err != nil {
			t.Fatalf("BlockUser failed: %v", err)
		}

		retrieved, err := repo.GetBlock(ctx, blockerDID, blockedDID)
		if err != nil {
			t.Fatalf("GetBlock failed: %v", err)
		}

		if retrieved.BlockerDID != blockerDID {
			t.Errorf("Expected BlockerDID=%s, got %s", blockerDID, retrieved.BlockerDID)
		}
		if retrieved.BlockedDID != blockedDID {
			t.Errorf("Expected BlockedDID=%s, got %s", blockedDID, retrieved.BlockedDID)
		}
		if retrieved.RecordURI != recordURI {
			t.Errorf("Expected RecordURI=%s, got %s", recordURI, retrieved.RecordURI)
		}
	})
}

// TestUserBlockRepo_GetBlockByURI tests block retrieval by record URI
func TestUserBlockRepo_GetBlockByURI(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupUserBlockTestDB(t, db)

	repo := postgresRepo.NewUserBlockRepository(db)

	t.Run("retrieves block by record_uri", func(t *testing.T) {
		recordURI := "at://did:plc:test-uri-blocker/social.coves.actor.block/uri1"
		block := &userblocks.UserBlock{
			BlockerDID: "did:plc:test-uri-blocker",
			BlockedDID: "did:plc:test-uri-blocked",
			BlockedAt:  time.Now(),
			RecordURI:  recordURI,
			RecordCID:  "bafyuri1",
		}
		_, err := repo.BlockUser(ctx, block)
		if err != nil {
			t.Fatalf("BlockUser failed: %v", err)
		}

		retrieved, err := repo.GetBlockByURI(ctx, recordURI)
		if err != nil {
			t.Fatalf("GetBlockByURI failed: %v", err)
		}

		if retrieved.RecordURI != recordURI {
			t.Errorf("Expected RecordURI=%s, got %s", recordURI, retrieved.RecordURI)
		}
		if retrieved.BlockerDID != "did:plc:test-uri-blocker" {
			t.Errorf("Expected BlockerDID=did:plc:test-uri-blocker, got %s", retrieved.BlockerDID)
		}
		if retrieved.BlockedDID != "did:plc:test-uri-blocked" {
			t.Errorf("Expected BlockedDID=did:plc:test-uri-blocked, got %s", retrieved.BlockedDID)
		}
	})

	t.Run("returns ErrBlockNotFound for unknown URI", func(t *testing.T) {
		_, err := repo.GetBlockByURI(ctx, "at://did:plc:test-unknown/social.coves.actor.block/nonexistent")
		if !userblocks.IsNotFound(err) {
			t.Errorf("Expected ErrBlockNotFound, got: %v", err)
		}
	})
}

// TestUserBlockRepo_ListBlockedUsers tests listing blocked users with pagination
func TestUserBlockRepo_ListBlockedUsers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupUserBlockTestDB(t, db)

	repo := postgresRepo.NewUserBlockRepository(db)

	blockerDID := "did:plc:test-list-blocker"

	// Use deterministic timestamps with clear ordering to avoid DB rounding issues.
	// i=0 is oldest, i=2 is newest.
	baseTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	blockedDIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		blockedDIDs[i] = fmt.Sprintf("did:plc:test-list-blocked-%d", i)
		block := &userblocks.UserBlock{
			BlockerDID: blockerDID,
			BlockedDID: blockedDIDs[i],
			BlockedAt:  baseTime.Add(time.Duration(i) * time.Hour),
			RecordURI:  fmt.Sprintf("at://%s/social.coves.actor.block/list%d", blockerDID, i),
			RecordCID:  fmt.Sprintf("bafylist%d", i),
		}
		_, err := repo.BlockUser(ctx, block)
		if err != nil {
			t.Fatalf("Failed to create block %d: %v", i, err)
		}
	}

	t.Run("lists all blocked users in DESC order", func(t *testing.T) {
		blocks, err := repo.ListBlockedUsers(ctx, blockerDID, 10, 0)
		if err != nil {
			t.Fatalf("ListBlockedUsers failed: %v", err)
		}

		if len(blocks) != 3 {
			t.Fatalf("Expected 3 blocks, got %d", len(blocks))
		}

		// Verify all blocks belong to correct blocker
		for _, block := range blocks {
			if block.BlockerDID != blockerDID {
				t.Errorf("Expected BlockerDID=%s, got %s", blockerDID, block.BlockerDID)
			}
		}

		// Verify ORDER BY blocked_at DESC: most recently blocked first
		if blocks[0].BlockedDID != blockedDIDs[2] {
			t.Errorf("Expected first result (most recent) to be %s, got %s", blockedDIDs[2], blocks[0].BlockedDID)
		}
		if blocks[2].BlockedDID != blockedDIDs[0] {
			t.Errorf("Expected last result (oldest) to be %s, got %s", blockedDIDs[0], blocks[2].BlockedDID)
		}
	})

	t.Run("pagination works correctly", func(t *testing.T) {
		// Get first 2
		blocks, err := repo.ListBlockedUsers(ctx, blockerDID, 2, 0)
		if err != nil {
			t.Fatalf("ListBlockedUsers with limit failed: %v", err)
		}
		if len(blocks) != 2 {
			t.Errorf("Expected 2 blocks (paginated), got %d", len(blocks))
		}

		// Get next page (should only get 1)
		blocksPage2, err := repo.ListBlockedUsers(ctx, blockerDID, 2, 2)
		if err != nil {
			t.Fatalf("ListBlockedUsers page 2 failed: %v", err)
		}
		if len(blocksPage2) != 1 {
			t.Errorf("Expected 1 block on page 2, got %d", len(blocksPage2))
		}
	})

	t.Run("returns empty list for user with no blocks", func(t *testing.T) {
		blocks, err := repo.ListBlockedUsers(ctx, "did:plc:test-no-blocks-user", 10, 0)
		if err != nil {
			t.Fatalf("ListBlockedUsers failed: %v", err)
		}
		if len(blocks) != 0 {
			t.Errorf("Expected 0 blocks, got %d", len(blocks))
		}
	})

	t.Run("limit=0 returns no results", func(t *testing.T) {
		blocks, err := repo.ListBlockedUsers(ctx, blockerDID, 0, 0)
		if err != nil {
			t.Fatalf("ListBlockedUsers with limit=0 failed: %v", err)
		}
		if len(blocks) != 0 {
			t.Errorf("Expected 0 blocks with limit=0, got %d", len(blocks))
		}
	})
}

// TestUserBlockRepo_IsBlocked tests the fast block check
func TestUserBlockRepo_IsBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupUserBlockTestDB(t, db)

	repo := postgresRepo.NewUserBlockRepository(db)

	t.Run("returns false when not blocked", func(t *testing.T) {
		isBlocked, err := repo.IsBlocked(ctx, "did:plc:test-isblocked-a", "did:plc:test-isblocked-b")
		if err != nil {
			t.Fatalf("IsBlocked failed: %v", err)
		}
		if isBlocked {
			t.Error("Expected IsBlocked=false, got true")
		}
	})

	t.Run("returns true when blocked", func(t *testing.T) {
		blockerDID := "did:plc:test-isblocked-blocker-2"
		blockedDID := "did:plc:test-isblocked-blocked-2"

		block := &userblocks.UserBlock{
			BlockerDID: blockerDID,
			BlockedDID: blockedDID,
			BlockedAt:  time.Now(),
			RecordURI:  fmt.Sprintf("at://%s/social.coves.actor.block/isblocked1", blockerDID),
			RecordCID:  "bafyisblocked1",
		}
		_, err := repo.BlockUser(ctx, block)
		if err != nil {
			t.Fatalf("BlockUser failed: %v", err)
		}

		isBlocked, err := repo.IsBlocked(ctx, blockerDID, blockedDID)
		if err != nil {
			t.Fatalf("IsBlocked failed: %v", err)
		}
		if !isBlocked {
			t.Error("Expected IsBlocked=true, got false")
		}
	})

	t.Run("returns false after unblock", func(t *testing.T) {
		blockerDID := "did:plc:test-isblocked-blocker-3"
		blockedDID := "did:plc:test-isblocked-blocked-3"

		// Create block first
		block := &userblocks.UserBlock{
			BlockerDID: blockerDID,
			BlockedDID: blockedDID,
			BlockedAt:  time.Now(),
			RecordURI:  fmt.Sprintf("at://%s/social.coves.actor.block/isblocked2", blockerDID),
			RecordCID:  "bafyisblocked2",
		}
		_, err := repo.BlockUser(ctx, block)
		if err != nil {
			t.Fatalf("BlockUser failed: %v", err)
		}

		// Unblock
		err = repo.UnblockUser(ctx, blockerDID, blockedDID)
		if err != nil {
			t.Fatalf("UnblockUser failed: %v", err)
		}

		isBlocked, err := repo.IsBlocked(ctx, blockerDID, blockedDID)
		if err != nil {
			t.Fatalf("IsBlocked failed: %v", err)
		}
		if isBlocked {
			t.Error("Expected IsBlocked=false after unblock, got true")
		}
	})
}

// TestUserBlockRepo_AreBlocked tests the batch block check
func TestUserBlockRepo_AreBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupUserBlockTestDB(t, db)

	repo := postgresRepo.NewUserBlockRepository(db)

	blockerDID := "did:plc:test-areblocked-blocker"

	// Block user 0 and user 2, but NOT user 1
	blockedDIDs := []string{
		"did:plc:test-areblocked-target-0",
		"did:plc:test-areblocked-target-1",
		"did:plc:test-areblocked-target-2",
	}

	for _, i := range []int{0, 2} {
		block := &userblocks.UserBlock{
			BlockerDID: blockerDID,
			BlockedDID: blockedDIDs[i],
			BlockedAt:  time.Now(),
			RecordURI:  fmt.Sprintf("at://%s/social.coves.actor.block/areblocked%d", blockerDID, i),
			RecordCID:  fmt.Sprintf("bafyareblocked%d", i),
		}
		_, err := repo.BlockUser(ctx, block)
		if err != nil {
			t.Fatalf("BlockUser failed for target %d: %v", i, err)
		}
	}

	t.Run("batch check returns correct map for mixed blocked/unblocked DIDs", func(t *testing.T) {
		result, err := repo.AreBlocked(ctx, blockerDID, blockedDIDs)
		if err != nil {
			t.Fatalf("AreBlocked failed: %v", err)
		}

		// target-0 should be blocked
		if !result[blockedDIDs[0]] {
			t.Errorf("Expected %s to be blocked", blockedDIDs[0])
		}

		// target-1 should NOT be blocked
		if result[blockedDIDs[1]] {
			t.Errorf("Expected %s to NOT be blocked", blockedDIDs[1])
		}

		// target-2 should be blocked
		if !result[blockedDIDs[2]] {
			t.Errorf("Expected %s to be blocked", blockedDIDs[2])
		}
	})

	t.Run("empty input returns empty map without hitting DB", func(t *testing.T) {
		result, err := repo.AreBlocked(ctx, blockerDID, []string{})
		if err != nil {
			t.Fatalf("AreBlocked with empty input failed: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("Expected empty map for empty input, got %d entries", len(result))
		}
	})
}

// TestUserBlockRepo_UnblockByRecordURI tests the full flow of looking up a block
// by record URI and then deleting it — the path used by the Jetstream consumer
// when processing DELETE operations (which only carry the record URI, not DID pairs).
func TestUserBlockRepo_UnblockByRecordURI(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupUserBlockTestDB(t, db)

	repo := postgresRepo.NewUserBlockRepository(db)

	blockerDID := "did:plc:test-uri-unblock-blocker"
	blockedDID := "did:plc:test-uri-unblock-blocked"
	recordURI := "at://did:plc:test-uri-unblock-blocker/social.coves.actor.block/del1"

	// Create the block
	block := &userblocks.UserBlock{
		BlockerDID: blockerDID,
		BlockedDID: blockedDID,
		BlockedAt:  time.Now(),
		RecordURI:  recordURI,
		RecordCID:  "bafyuriunblock1",
	}
	_, err := repo.BlockUser(ctx, block)
	if err != nil {
		t.Fatalf("BlockUser failed: %v", err)
	}

	// Look up by URI (as the Jetstream consumer would)
	found, err := repo.GetBlockByURI(ctx, recordURI)
	if err != nil {
		t.Fatalf("GetBlockByURI failed: %v", err)
	}
	if found.BlockerDID != blockerDID || found.BlockedDID != blockedDID {
		t.Fatalf("GetBlockByURI returned wrong block: blocker=%s blocked=%s", found.BlockerDID, found.BlockedDID)
	}

	// Delete using the DIDs from the lookup
	err = repo.UnblockUser(ctx, found.BlockerDID, found.BlockedDID)
	if err != nil {
		t.Fatalf("UnblockUser after URI lookup failed: %v", err)
	}

	// Verify block is gone
	_, err = repo.GetBlock(ctx, blockerDID, blockedDID)
	if !userblocks.IsNotFound(err) {
		t.Errorf("Expected ErrBlockNotFound after unblock-by-URI flow, got: %v", err)
	}
}

// cleanupUserBlockTestDB removes test data from the user_blocks table
func cleanupUserBlockTestDB(t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.Exec("DELETE FROM user_blocks WHERE blocker_did LIKE 'did:plc:test-%'")
	if err != nil {
		t.Logf("Warning: Failed to clean up user blocks: %v", err)
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Logf("Failed to close database: %v", closeErr)
	}
}
