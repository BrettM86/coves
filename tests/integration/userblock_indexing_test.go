//go:build integration

package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/userblocks"
	"Coves/tests/testkit"
	"context"
	"fmt"
	"testing"
	"time"

	postgresRepo "Coves/internal/db/postgres"
)

// TestUserBlockIndexing_CreateEvent tests that a Jetstream CREATE event for
// social.coves.actor.block is properly indexed in the AppView.
func TestUserBlockIndexing_CreateEvent(t *testing.T) {
	ctx := context.Background()
	db := testkit.DB(t)

	repo := postgresRepo.NewUserBlockRepository(db)
	consumer := createUserBlockConsumer(t, repo)

	blockerDID := "did:plc:test-blocker-create"
	blockedDID := "did:plc:test-blocked-create"
	rkey := "test-block-create-1"

	// Simulate Jetstream CREATE event
	event := &jetstream.JetstreamEvent{
		Did:    blockerDID,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev:        "test-rev-1",
			Operation:  "create",
			Collection: "social.coves.actor.block",
			RKey:       rkey,
			CID:        "bafyuserblock123",
			Record: map[string]interface{}{
				"$type":     "social.coves.actor.block",
				"subject":   blockedDID,
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}

	// Process event through the consumer
	err := consumer.HandleEvent(ctx, event)
	if err != nil {
		t.Fatalf("Failed to handle block CREATE event: %v", err)
	}

	// Verify block indexed in AppView via repo.GetBlock()
	block, err := repo.GetBlock(ctx, blockerDID, blockedDID)
	if err != nil {
		t.Fatalf("Failed to get block after indexing: %v", err)
	}

	if block.BlockerDID != blockerDID {
		t.Errorf("Expected blockerDID=%s, got %s", blockerDID, block.BlockerDID)
	}
	if block.BlockedDID != blockedDID {
		t.Errorf("Expected blockedDID=%s, got %s", blockedDID, block.BlockedDID)
	}

	expectedURI := fmt.Sprintf("at://%s/social.coves.actor.block/%s", blockerDID, rkey)
	if block.RecordURI != expectedURI {
		t.Errorf("Expected recordURI=%s, got %s", expectedURI, block.RecordURI)
	}
	if block.RecordCID != "bafyuserblock123" {
		t.Errorf("Expected recordCID=bafyuserblock123, got %s", block.RecordCID)
	}

	// Verify IsBlocked returns true
	isBlocked, err := repo.IsBlocked(ctx, blockerDID, blockedDID)
	if err != nil {
		t.Fatalf("IsBlocked failed: %v", err)
	}
	if !isBlocked {
		t.Error("Expected IsBlocked=true, got false")
	}
}

// TestUserBlockIndexing_DeleteEvent tests that a Jetstream DELETE event
// properly removes a previously indexed block from the AppView.
func TestUserBlockIndexing_DeleteEvent(t *testing.T) {
	ctx := context.Background()
	db := testkit.DB(t)

	repo := postgresRepo.NewUserBlockRepository(db)
	consumer := createUserBlockConsumer(t, repo)

	blockerDID := "did:plc:test-blocker-delete"
	blockedDID := "did:plc:test-blocked-delete"
	rkey := "test-block-delete-1"
	uri := fmt.Sprintf("at://%s/social.coves.actor.block/%s", blockerDID, rkey)

	// Pre-index a block in AppView via repo.BlockUser()
	block := &userblocks.UserBlock{
		BlockerDID: blockerDID,
		BlockedDID: blockedDID,
		BlockedAt:  time.Now(),
		RecordURI:  uri,
		RecordCID:  "bafyuserblock456",
	}
	_, err := repo.BlockUser(ctx, block)
	if err != nil {
		t.Fatalf("Failed to pre-index block: %v", err)
	}

	// Verify block exists before delete
	isBlocked, err := repo.IsBlocked(ctx, blockerDID, blockedDID)
	if err != nil {
		t.Fatalf("IsBlocked failed: %v", err)
	}
	if !isBlocked {
		t.Fatal("Expected block to exist before DELETE event")
	}

	// Simulate Jetstream DELETE event (no record data, just rkey)
	event := &jetstream.JetstreamEvent{
		Did:    blockerDID,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev:        "test-rev-2",
			Operation:  "delete",
			Collection: "social.coves.actor.block",
			RKey:       rkey,
		},
	}

	// Process delete event
	err = consumer.HandleEvent(ctx, event)
	if err != nil {
		t.Fatalf("Failed to handle block DELETE event: %v", err)
	}

	// Verify block removed via repo.IsBlocked() == false
	isBlocked, err = repo.IsBlocked(ctx, blockerDID, blockedDID)
	if err != nil {
		t.Fatalf("IsBlocked failed after delete: %v", err)
	}
	if isBlocked {
		t.Error("Expected IsBlocked=false after DELETE event, got true")
	}

	// Also verify GetBlock returns ErrBlockNotFound
	_, err = repo.GetBlock(ctx, blockerDID, blockedDID)
	if !userblocks.IsNotFound(err) {
		t.Errorf("Expected ErrBlockNotFound after delete, got: %v", err)
	}
}

// TestUserBlockIndexing_Idempotent tests that processing the same CREATE event
// twice results in only 1 block (idempotent via ON CONFLICT DO UPDATE).
func TestUserBlockIndexing_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := testkit.DB(t)

	repo := postgresRepo.NewUserBlockRepository(db)
	consumer := createUserBlockConsumer(t, repo)

	blockerDID := "did:plc:test-blocker-idempotent"
	blockedDID := "did:plc:test-blocked-idempotent"
	rkey := "test-block-idempotent-1"

	event := &jetstream.JetstreamEvent{
		Did:    blockerDID,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev:        "test-rev-3",
			Operation:  "create",
			Collection: "social.coves.actor.block",
			RKey:       rkey,
			CID:        "bafyuserblock789",
			Record: map[string]interface{}{
				"$type":     "social.coves.actor.block",
				"subject":   blockedDID,
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}

	// Process event twice
	err := consumer.HandleEvent(ctx, event)
	if err != nil {
		t.Fatalf("First block CREATE failed: %v", err)
	}

	err = consumer.HandleEvent(ctx, event)
	if err != nil {
		t.Fatalf("Second block CREATE (idempotent) failed: %v", err)
	}

	// Verify only 1 block exists via ListBlockedUsers
	blocks, err := repo.ListBlockedUsers(ctx, blockerDID, 10, 0)
	if err != nil {
		t.Fatalf("ListBlockedUsers failed: %v", err)
	}
	if len(blocks) != 1 {
		t.Errorf("Expected 1 block after idempotent create, got %d", len(blocks))
	}
}

// TestUserBlockIndexing_DeleteNonExistent tests that a DELETE event for a
// non-existent block does not error (graceful/idempotent).
func TestUserBlockIndexing_DeleteNonExistent(t *testing.T) {
	ctx := context.Background()
	db := testkit.DB(t)

	repo := postgresRepo.NewUserBlockRepository(db)
	consumer := createUserBlockConsumer(t, repo)

	blockerDID := "did:plc:test-blocker-nonexistent"
	rkey := "test-block-nonexistent"

	// Simulate DELETE event for block that doesn't exist
	event := &jetstream.JetstreamEvent{
		Did:    blockerDID,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev:        "test-rev-99",
			Operation:  "delete",
			Collection: "social.coves.actor.block",
			RKey:       rkey,
		},
	}

	// Should not error (idempotent)
	err := consumer.HandleEvent(ctx, event)
	if err != nil {
		t.Errorf("DELETE of non-existent block should be idempotent, got error: %v", err)
	}
}

// Helper functions for user block indexing tests

// createUserBlockConsumer creates a UserEventConsumer configured with a user block
// repo for testing. Uses nil for userService and identityResolver since block
// handling doesn't need them.
func createUserBlockConsumer(t *testing.T, repo userblocks.Repository) *jetstream.UserEventConsumer {
	t.Helper()
	return jetstream.NewUserEventConsumer(nil, nil, jetstream.WithUserBlockRepo(repo))
}
