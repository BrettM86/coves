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

// TestSubscriptionIndexing_ContentVisibility tests that contentVisibility is properly indexed
// from Jetstream events and stored in the AppView database
func TestSubscriptionIndexing_ContentVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupTestDB(t, db)

	repo := createTestCommunityRepo(t, db)
	consumer := jetstream.NewCommunityEventConsumer(repo)

	// Create a test community first (with unique DID)
	testDID := fmt.Sprintf("did:plc:test-community-%d", time.Now().UnixNano())
	community := createTestCommunity(t, repo, "test-community-visibility", testDID)

	t.Run("indexes subscription with contentVisibility=5", func(t *testing.T) {
		userDID := "did:plc:test-user-123"
		rkey := "test-sub-1"
		uri := "at://" + userDID + "/social.coves.community.subscription/" + rkey

		// Simulate Jetstream CREATE event for subscription
		event := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-1",
				Operation:  "create",
				Collection: "social.coves.community.subscription", // CORRECT collection name
				RKey:       rkey,
				CID:        "bafytest123",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.subscription",
					"subject": community.DID,
					"createdAt":         time.Now().Format(time.RFC3339),
					"contentVisibility": float64(5), // JSON numbers decode as float64
				},
			},
		}

		// Process event through consumer
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Failed to handle subscription event: %v", err)
		}

		// Verify subscription was indexed with correct contentVisibility
		subscription, err := repo.GetSubscription(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("Failed to get subscription: %v", err)
		}

		if subscription.ContentVisibility != 5 {
			t.Errorf("Expected contentVisibility=5, got %d", subscription.ContentVisibility)
		}

		if subscription.UserDID != userDID {
			t.Errorf("Expected userDID=%s, got %s", userDID, subscription.UserDID)
		}

		if subscription.CommunityDID != community.DID {
			t.Errorf("Expected communityDID=%s, got %s", community.DID, subscription.CommunityDID)
		}

		if subscription.RecordURI != uri {
			t.Errorf("Expected recordURI=%s, got %s", uri, subscription.RecordURI)
		}

		t.Logf("✓ Subscription indexed with contentVisibility=5")
	})

	t.Run("defaults to contentVisibility=3 when not provided", func(t *testing.T) {
		userDID := "did:plc:test-user-default"
		rkey := "test-sub-default"

		// Simulate Jetstream CREATE event WITHOUT contentVisibility field
		event := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-default",
				Operation:  "create",
				Collection: "social.coves.community.subscription",
				RKey:       rkey,
				CID:        "bafydefault",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.subscription",
					"subject": community.DID,
					"createdAt": time.Now().Format(time.RFC3339),
					// contentVisibility NOT provided
				},
			},
		}

		// Process event
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Failed to handle subscription event: %v", err)
		}

		// Verify defaults to 3
		subscription, err := repo.GetSubscription(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("Failed to get subscription: %v", err)
		}

		if subscription.ContentVisibility != 3 {
			t.Errorf("Expected contentVisibility=3 (default), got %d", subscription.ContentVisibility)
		}

		t.Logf("✓ Subscription defaulted to contentVisibility=3")
	})

	t.Run("clamps contentVisibility to valid range (1-5)", func(t *testing.T) {
		testCases := []struct {
			input    float64
			expected int
			name     string
		}{
			{input: 0, expected: 1, name: "zero clamped to 1"},
			{input: -5, expected: 1, name: "negative clamped to 1"},
			{input: 10, expected: 5, name: "10 clamped to 5"},
			{input: 100, expected: 5, name: "100 clamped to 5"},
			{input: 1, expected: 1, name: "1 stays 1"},
			{input: 3, expected: 3, name: "3 stays 3"},
			{input: 5, expected: 5, name: "5 stays 5"},
		}

		for i, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				userDID := fmt.Sprintf("did:plc:test-clamp-%d", i)
				rkey := fmt.Sprintf("test-sub-clamp-%d", i)

				event := &jetstream.JetstreamEvent{
					Did:    userDID,
					Kind:   "commit",
					TimeUS: time.Now().UnixMicro(),
					Commit: &jetstream.CommitEvent{
						Rev:        "test-rev-clamp",
						Operation:  "create",
						Collection: "social.coves.community.subscription",
						RKey:       rkey,
						CID:        "bafyclamp",
						Record: map[string]interface{}{
							"$type":             "social.coves.community.subscription",
							"subject":           community.DID,
							"createdAt":         time.Now().Format(time.RFC3339),
							"contentVisibility": tc.input,
						},
					},
				}

				err := consumer.HandleEvent(ctx, event)
				if err != nil {
					t.Fatalf("Failed to handle subscription event: %v", err)
				}

				subscription, err := repo.GetSubscription(ctx, userDID, community.DID)
				if err != nil {
					t.Fatalf("Failed to get subscription: %v", err)
				}

				if subscription.ContentVisibility != tc.expected {
					t.Errorf("Input %.0f: expected %d, got %d", tc.input, tc.expected, subscription.ContentVisibility)
				}

				t.Logf("✓ Input %.0f clamped to %d", tc.input, subscription.ContentVisibility)
			})
		}
	})

	t.Run("idempotency: duplicate subscription events don't fail", func(t *testing.T) {
		userDID := "did:plc:test-idempotent"
		rkey := "test-sub-idempotent"

		event := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-idempotent",
				Operation:  "create",
				Collection: "social.coves.community.subscription",
				RKey:       rkey,
				CID:        "bafyidempotent",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.subscription",
					"subject": community.DID,
					"createdAt":         time.Now().Format(time.RFC3339),
					"contentVisibility": float64(4),
				},
			},
		}

		// Process first time
		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Failed to handle first subscription event: %v", err)
		}

		// Process again (Jetstream replay scenario)
		err = consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Errorf("Idempotency failed: second event should not error, got: %v", err)
		}

		// Verify only one subscription exists
		subscription, err := repo.GetSubscription(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("Failed to get subscription: %v", err)
		}

		if subscription.ContentVisibility != 4 {
			t.Errorf("Expected contentVisibility=4, got %d", subscription.ContentVisibility)
		}

		t.Logf("✓ Duplicate events handled idempotently")
	})
}

// TestSubscriptionIndexing_DeleteOperations tests unsubscribe (DELETE) event handling
func TestSubscriptionIndexing_DeleteOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupTestDB(t, db)

	repo := createTestCommunityRepo(t, db)
	consumer := jetstream.NewCommunityEventConsumer(repo)

	// Create test community (with unique DID)
	testDID := fmt.Sprintf("did:plc:test-unsub-%d", time.Now().UnixNano())
	community := createTestCommunity(t, repo, "test-unsubscribe", testDID)

	t.Run("deletes subscription when DELETE event received", func(t *testing.T) {
		userDID := "did:plc:test-user-delete"
		rkey := "test-sub-delete"

		// First, create a subscription
		createEvent := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-create",
				Operation:  "create",
				Collection: "social.coves.community.subscription",
				RKey:       rkey,
				CID:        "bafycreate",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.subscription",
					"subject": community.DID,
					"createdAt":         time.Now().Format(time.RFC3339),
					"contentVisibility": float64(3),
				},
			},
		}

		err := consumer.HandleEvent(ctx, createEvent)
		if err != nil {
			t.Fatalf("Failed to create subscription: %v", err)
		}

		// Verify subscription exists
		_, err = repo.GetSubscription(ctx, userDID, community.DID)
		if err != nil {
			t.Fatalf("Subscription should exist: %v", err)
		}

		// Now send DELETE event (unsubscribe)
		// IMPORTANT: DELETE operations don't include record data in Jetstream
		deleteEvent := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-delete",
				Operation:  "delete",
				Collection: "social.coves.community.subscription",
				RKey:       rkey,
				CID:        "", // No CID on deletes
				Record:     nil, // No record data on deletes
			},
		}

		err = consumer.HandleEvent(ctx, deleteEvent)
		if err != nil {
			t.Fatalf("Failed to handle delete event: %v", err)
		}

		// Verify subscription was deleted
		_, err = repo.GetSubscription(ctx, userDID, community.DID)
		if err == nil {
			t.Errorf("Subscription should have been deleted")
		}
		if !communities.IsNotFound(err) {
			t.Errorf("Expected NotFound error, got: %v", err)
		}

		t.Logf("✓ Subscription deleted successfully")
	})

	t.Run("idempotent delete: deleting non-existent subscription doesn't fail", func(t *testing.T) {
		userDID := "did:plc:test-user-noexist"
		rkey := "test-sub-noexist"

		// Try to delete a subscription that doesn't exist
		deleteEvent := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-noexist",
				Operation:  "delete",
				Collection: "social.coves.community.subscription",
				RKey:       rkey,
				CID:        "",
				Record:     nil,
			},
		}

		// Should not error (idempotent)
		err := consumer.HandleEvent(ctx, deleteEvent)
		if err != nil {
			t.Errorf("Deleting non-existent subscription should not error, got: %v", err)
		}

		t.Logf("✓ Idempotent delete handled gracefully")
	})
}

// TestSubscriptionIndexing_SubscriberCount tests that subscriber counts are updated atomically
func TestSubscriptionIndexing_SubscriberCount(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer cleanupTestDB(t, db)

	repo := createTestCommunityRepo(t, db)
	consumer := jetstream.NewCommunityEventConsumer(repo)

	// Create test community (with unique DID)
	testDID := fmt.Sprintf("did:plc:test-subcount-%d", time.Now().UnixNano())
	community := createTestCommunity(t, repo, "test-subscriber-count", testDID)

	// Verify initial subscriber count is 0
	comm, err := repo.GetByDID(ctx, community.DID)
	if err != nil {
		t.Fatalf("Failed to get community: %v", err)
	}
	if comm.SubscriberCount != 0 {
		t.Errorf("Initial subscriber count should be 0, got %d", comm.SubscriberCount)
	}

	t.Run("increments subscriber count on subscribe", func(t *testing.T) {
		userDID := "did:plc:test-user-count1"
		rkey := "test-sub-count1"

		event := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-count",
				Operation:  "create",
				Collection: "social.coves.community.subscription",
				RKey:       rkey,
				CID:        "bafycount",
				Record: map[string]interface{}{
					"$type":   "social.coves.community.subscription",
					"subject": community.DID,
					"createdAt":         time.Now().Format(time.RFC3339),
					"contentVisibility": float64(3),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err != nil {
			t.Fatalf("Failed to handle subscription: %v", err)
		}

		// Check subscriber count incremented
		comm, err := repo.GetByDID(ctx, community.DID)
		if err != nil {
			t.Fatalf("Failed to get community: %v", err)
		}

		if comm.SubscriberCount != 1 {
			t.Errorf("Subscriber count should be 1, got %d", comm.SubscriberCount)
		}

		t.Logf("✓ Subscriber count incremented to 1")
	})

	t.Run("decrements subscriber count on unsubscribe", func(t *testing.T) {
		userDID := "did:plc:test-user-count1" // Same user from above
		rkey := "test-sub-count1"

		// Send DELETE event
		deleteEvent := &jetstream.JetstreamEvent{
			Did:    userDID,
			Kind:   "commit",
			TimeUS: time.Now().UnixMicro(),
			Commit: &jetstream.CommitEvent{
				Rev:        "test-rev-unsub",
				Operation:  "delete",
				Collection: "social.coves.community.subscription",
				RKey:       rkey,
				CID:        "",
				Record:     nil,
			},
		}

		err := consumer.HandleEvent(ctx, deleteEvent)
		if err != nil {
			t.Fatalf("Failed to handle unsubscribe: %v", err)
		}

		// Check subscriber count decremented back to 0
		comm, err := repo.GetByDID(ctx, community.DID)
		if err != nil {
			t.Fatalf("Failed to get community: %v", err)
		}

		if comm.SubscriberCount != 0 {
			t.Errorf("Subscriber count should be 0, got %d", comm.SubscriberCount)
		}

		t.Logf("✓ Subscriber count decremented to 0")
	})
}

// Helper functions

func createTestCommunity(t *testing.T, repo communities.Repository, name, did string) *communities.Community {
	t.Helper()

	// Add timestamp to make handles unique across test runs
	uniqueHandle := fmt.Sprintf("%s-%d.test.coves.social", name, time.Now().UnixNano())

	community := &communities.Community{
		DID:          did,
		Handle:       uniqueHandle,
		Name:         name,
		DisplayName:  "Test Community " + name,
		Description:  "Test community for subscription indexing",
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

func createTestCommunityRepo(t *testing.T, db interface{}) communities.Repository {
	t.Helper()
	// Import the postgres package to create a repo
	return postgresRepo.NewCommunityRepository(db.(*sql.DB))
}

func cleanupTestDB(t *testing.T, db interface{}) {
	t.Helper()
	sqlDB := db.(*sql.DB)
	if err := sqlDB.Close(); err != nil {
		t.Logf("Failed to close database: %v", err)
	}
}
