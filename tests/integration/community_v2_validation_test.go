package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"context"
	"fmt"
	"testing"
	"time"
)

// TestCommunityConsumer_V2RKeyValidation tests that only V2 communities (rkey="self") are accepted
func TestCommunityConsumer_V2RKeyValidation(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	// Skip verification in tests
	consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true)
	ctx := context.Background()

	t.Run("accepts V2 community with rkey=self", func(t *testing.T) {
		// Use unique DID and handle to avoid conflicts with other test runs
		timestamp := time.Now().UnixNano()
		testDID := fmt.Sprintf("did:plc:testv2rkey%d", timestamp)
		testHandle := fmt.Sprintf("testv2rkey%d.community.coves.social", timestamp)

		event := &jetstream.JetstreamEvent{
			Did:  testDID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self", // V2: correct rkey
				CID:        "bafyreigaming123",
				Record: map[string]interface{}{
					"$type":      "social.coves.community.profile",
					"handle":     testHandle,
					"name":       "testv2rkey",
					"createdBy":  "did:plc:user123",
					"hostedBy":   "did:web:coves.social",
					"visibility": "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("V2 community with rkey=self should be accepted, got error: %v", err)
		}

		// Verify community was indexed
		community, err := repo.GetByDID(ctx, testDID)
		if err != nil {
			t.Fatalf("Community should have been indexed: %v", err)
		}

		// Verify V2 self-ownership
		if community.OwnerDID != community.DID {
			t.Errorf("V2 community should be self-owned: expected OwnerDID=%s, got %s", community.DID, community.OwnerDID)
		}

		// Verify record URI uses "self"
		expectedURI := fmt.Sprintf("at://%s/social.coves.community.profile/self", testDID)
		if community.RecordURI != expectedURI {
			t.Errorf("Expected RecordURI %s, got %s", expectedURI, community.RecordURI)
		}
	})

	t.Run("rejects V1 community with non-self rkey", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  "did:plc:community456",
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "3k2j4h5g6f7d", // V1: TID-based rkey (INVALID for V2!)
				CID:        "bafyreiv1community",
				Record: map[string]interface{}{
					"$type":      "social.coves.community.profile",
					"handle":     "v1community.community.coves.social",
					"name":       "v1community",
					"createdBy":  "did:plc:user456",
					"hostedBy":   "did:web:coves.social",
					"visibility": "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("V1 community with TID rkey should be rejected")
		}

		// Verify error message indicates V1 not supported
		if err != nil {
			errMsg := err.Error()
			if errMsg != "invalid community profile rkey: expected 'self', got '3k2j4h5g6f7d' (V1 communities not supported)" {
				t.Errorf("Expected V1 rejection error, got: %s", errMsg)
			}
		}

		// Verify community was NOT indexed
		_, err = repo.GetByDID(ctx, "did:plc:community456")
		if err != communities.ErrCommunityNotFound {
			t.Errorf("V1 community should not have been indexed, expected ErrCommunityNotFound, got: %v", err)
		}
	})

	t.Run("rejects community with custom rkey", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:  "did:plc:community789",
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "custom-profile-name", // Custom rkey (INVALID!)
				CID:        "bafyreicustom",
				Record: map[string]interface{}{
					"$type":      "social.coves.community.profile",
					"handle":     "custom.community.coves.social",
					"name":       "custom",
					"createdBy":  "did:plc:user789",
					"hostedBy":   "did:web:coves.social",
					"visibility": "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Error("Community with custom rkey should be rejected")
		}

		// Verify community was NOT indexed
		_, err = repo.GetByDID(ctx, "did:plc:community789")
		if err != communities.ErrCommunityNotFound {
			t.Error("Community with custom rkey should not have been indexed")
		}
	})

	t.Run("update event also requires rkey=self", func(t *testing.T) {
		// First create a V2 community
		createEvent := &jetstream.JetstreamEvent{
			Did:  "did:plc:updatetest",
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafyreiupdate1",
				Record: map[string]interface{}{
					"$type":      "social.coves.community.profile",
					"handle":     "updatetest.community.coves.social",
					"name":       "updatetest",
					"createdBy":  "did:plc:userUpdate",
					"hostedBy":   "did:web:coves.social",
					"visibility": "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create community for update test: %v", err)
		}

		// Try to update with wrong rkey
		updateEvent := &jetstream.JetstreamEvent{
			Did:  "did:plc:updatetest",
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "update",
				Collection: "social.coves.community.profile",
				RKey:       "wrong-rkey", // INVALID!
				CID:        "bafyreiupdate2",
				Record: map[string]interface{}{
					"$type":       "social.coves.community.profile",
					"handle":      "updatetest.community.coves.social",
					"name":        "updatetest",
					"displayName": "Updated Name",
					"createdBy":   "did:plc:userUpdate",
					"hostedBy":    "did:web:coves.social",
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		err = consumer.HandleEvent(ctx, updateEvent)
		if err == nil {
			t.Error("Update event with wrong rkey should be rejected")
		}

		// Verify original community still exists unchanged
		community, err := repo.GetByDID(ctx, "did:plc:updatetest")
		if err != nil {
			t.Fatalf("Original community should still exist: %v", err)
		}

		if community.DisplayName == "Updated Name" {
			t.Error("Community should not have been updated with invalid rkey")
		}
	})
}

// TestCommunityConsumer_HandleField tests the V2 handle field
func TestCommunityConsumer_HandleField(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	// Skip verification in tests
	consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true)
	ctx := context.Background()

	t.Run("indexes community with atProto handle", func(t *testing.T) {
		uniqueDID := "did:plc:handletestunique987"
		event := &jetstream.JetstreamEvent{
			Did:  uniqueDID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafyreihandle",
				Record: map[string]interface{}{
					"$type":      "social.coves.community.profile",
					"handle":     "gamingtest.community.coves.social", // atProto handle (DNS-resolvable)
					"name":       "gamingtest",                        // Short name for !mentions
					"createdBy":  "did:plc:user123",
					"hostedBy":   "did:web:coves.social",
					"visibility": "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("Failed to index community with handle: %v", err)
		}

		community, err := repo.GetByDID(ctx, uniqueDID)
		if err != nil {
			t.Fatalf("Community should have been indexed: %v", err)
		}

		// Verify the atProto handle is stored
		if community.Handle != "gamingtest.community.coves.social" {
			t.Errorf("Expected handle gamingtest.community.coves.social, got %s", community.Handle)
		}

		// Note: The DID is the authoritative identifier for atProto resolution
		// The handle is DNS-resolvable via .well-known/atproto-did
	})
}
