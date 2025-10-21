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

func TestCommunityConsumer_HandleCommunityProfile(t *testing.T) {
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

	t.Run("creates community from firehose event", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		// Simulate a Jetstream commit event
		event := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"did":         communityDID, // Community's unique DID
					"handle":      fmt.Sprintf("!test-community-%s@coves.local", uniqueSuffix),
					"name":        "test-community",
					"displayName": "Test Community",
					"description": "A test community",
					"owner":       "did:web:coves.local",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:coves.local",
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

		// Handle the event
		if err := consumer.HandleEvent(ctx, event); err != nil {
			t.Fatalf("Failed to handle event: %v", err)
		}

		// Verify community was indexed
		community, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to get indexed community: %v", err)
		}

		if community.DID != communityDID {
			t.Errorf("Expected DID %s, got %s", communityDID, community.DID)
		}
		if community.DisplayName != "Test Community" {
			t.Errorf("Expected DisplayName 'Test Community', got %s", community.DisplayName)
		}
		if community.Visibility != "public" {
			t.Errorf("Expected Visibility 'public', got %s", community.Visibility)
		}
	})

	t.Run("updates existing community", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)
		handle := fmt.Sprintf("!update-test-%s@coves.local", uniqueSuffix)

		// Create initial community
		initialCommunity := &communities.Community{
			DID:                    communityDID,
			Handle:                 handle,
			Name:                   "update-test",
			DisplayName:            "Original Name",
			Description:            "Original description",
			OwnerDID:               "did:web:coves.local",
			CreatedByDID:           "did:plc:user123",
			HostedByDID:            "did:web:coves.local",
			Visibility:             "public",
			AllowExternalDiscovery: true,
			CreatedAt:              time.Now(),
			UpdatedAt:              time.Now(),
		}

		if _, err := repo.Create(ctx, initialCommunity); err != nil {
			t.Fatalf("Failed to create initial community: %v", err)
		}

		// Simulate update event
		updateEvent := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev124",
				Operation:  "update",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy456def",
				Record: map[string]interface{}{
					"did":         communityDID, // Community's unique DID
					"handle":      handle,
					"name":        "update-test",
					"displayName": "Updated Name",
					"description": "Updated description",
					"owner":       "did:web:coves.local",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:coves.local",
					"visibility":  "unlisted",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": false,
					},
					"memberCount":     5,
					"subscriberCount": 10,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		// Handle the update
		if err := consumer.HandleEvent(ctx, updateEvent); err != nil {
			t.Fatalf("Failed to handle update event: %v", err)
		}

		// Verify community was updated
		updated, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to get updated community: %v", err)
		}

		if updated.DisplayName != "Updated Name" {
			t.Errorf("Expected DisplayName 'Updated Name', got %s", updated.DisplayName)
		}
		if updated.Description != "Updated description" {
			t.Errorf("Expected Description 'Updated description', got %s", updated.Description)
		}
		if updated.Visibility != "unlisted" {
			t.Errorf("Expected Visibility 'unlisted', got %s", updated.Visibility)
		}
		if updated.AllowExternalDiscovery {
			t.Error("Expected AllowExternalDiscovery to be false")
		}
	})

	t.Run("deletes community", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		// Create community to delete
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!delete-test-%s@coves.local", uniqueSuffix),
			Name:         "delete-test",
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		if _, err := repo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Simulate delete event
		deleteEvent := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev125",
				Operation:  "delete",
				Collection: "social.coves.community.profile",
				RKey:       "self",
			},
		}

		// Handle the delete
		if err := consumer.HandleEvent(ctx, deleteEvent); err != nil {
			t.Fatalf("Failed to handle delete event: %v", err)
		}

		// Verify community was deleted
		if _, err := repo.GetByDID(ctx, communityDID); err != communities.ErrCommunityNotFound {
			t.Errorf("Expected ErrCommunityNotFound, got: %v", err)
		}
	})
}

func TestCommunityConsumer_HandleSubscription(t *testing.T) {
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

	t.Run("creates subscription from event", func(t *testing.T) {
		// Create a community first
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!sub-test-%s@coves.local", uniqueSuffix),
			Name:         "sub-test",
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		if _, err := repo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Simulate subscription event
		// IMPORTANT: Use correct collection name (record type, not XRPC procedure)
		userDID := "did:plc:subscriber123"
		subEvent := &jetstream.JetstreamEvent{
			Did:    userDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev200",
				Operation:  "create",
				Collection: "social.coves.community.subscription", // Updated to communities namespace
				RKey:       "sub123",
				CID:        "bafy789ghi",
				Record: map[string]interface{}{
					"subject":           communityDID, // Using 'subject' per atProto conventions
					"contentVisibility": 3,
					"createdAt":         time.Now().Format(time.RFC3339),
				},
			},
		}

		// Handle the subscription
		if err := consumer.HandleEvent(ctx, subEvent); err != nil {
			t.Fatalf("Failed to handle subscription event: %v", err)
		}

		// Verify subscription was created
		subscription, err := repo.GetSubscription(ctx, userDID, communityDID)
		if err != nil {
			t.Fatalf("Failed to get subscription: %v", err)
		}

		if subscription.UserDID != userDID {
			t.Errorf("Expected UserDID %s, got %s", userDID, subscription.UserDID)
		}
		if subscription.CommunityDID != communityDID {
			t.Errorf("Expected CommunityDID %s, got %s", communityDID, subscription.CommunityDID)
		}

		// Verify subscriber count was incremented
		updated, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to get community: %v", err)
		}

		if updated.SubscriberCount != 1 {
			t.Errorf("Expected SubscriberCount 1, got %d", updated.SubscriberCount)
		}
	})
}

func TestCommunityConsumer_IgnoresNonCommunityEvents(t *testing.T) {
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

	t.Run("ignores identity events", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:    "did:plc:user123",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "did:plc:user123",
				Handle: "alice.bsky.social",
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("Expected no error for identity event, got: %v", err)
		}
	})

	t.Run("ignores non-community collections", func(t *testing.T) {
		event := &jetstream.JetstreamEvent{
			Did:    "did:plc:user123",
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev300",
				Operation:  "create",
				Collection: "app.bsky.communityFeed.post",
				RKey:       "post123",
				Record: map[string]interface{}{
					"text": "Hello world",
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("Expected no error for non-community event, got: %v", err)
		}
	})
}
