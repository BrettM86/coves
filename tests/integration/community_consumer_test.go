package integration

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"context"
	"errors"
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
	ctx := context.Background()

	t.Run("creates community from firehose event", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)
		communityName := fmt.Sprintf("test-community-%s", uniqueSuffix)
		expectedHandle := fmt.Sprintf("%s.community.coves.local", communityName)

		// Set up mock resolver for this test DID
		mockResolver := newMockIdentityResolver()
		mockResolver.resolutions[communityDID] = expectedHandle
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true, mockResolver)

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
					// Note: No 'did', 'handle', 'memberCount', or 'subscriberCount' in record
					// These are resolved/computed by AppView, not stored in immutable records
					"name":        communityName,
					"displayName": "Test Community",
					"description": "A test community",
					"owner":       "did:web:coves.local",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:coves.local",
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"createdAt": time.Now().Format(time.RFC3339),
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
		communityName := "update-test"
		expectedHandle := fmt.Sprintf("%s.community.coves.local", communityName)

		// Set up mock resolver for this test DID
		mockResolver := newMockIdentityResolver()
		mockResolver.resolutions[communityDID] = expectedHandle
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true, mockResolver)

		// Create initial community
		initialCommunity := &communities.Community{
			DID:                    communityDID,
			Handle:                 expectedHandle,
			Name:                   communityName,
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
					// Note: No 'did', 'handle', 'memberCount', or 'subscriberCount' in record
					// These are resolved/computed by AppView, not stored in immutable records
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
					"createdAt": time.Now().Format(time.RFC3339),
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
		communityName := "delete-test"
		expectedHandle := fmt.Sprintf("%s.community.coves.local", communityName)

		// Set up mock resolver for this test DID
		mockResolver := newMockIdentityResolver()
		mockResolver.resolutions[communityDID] = expectedHandle
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true, mockResolver)

		// Create community to delete
		community := &communities.Community{
			DID:          communityDID,
			Handle:       expectedHandle,
			Name:         communityName,
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
	ctx := context.Background()

	t.Run("creates subscription from event", func(t *testing.T) {
		// Create a community first
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)
		communityName := "sub-test"
		expectedHandle := fmt.Sprintf("%s.community.coves.local", communityName)

		// Set up mock resolver for this test DID
		mockResolver := newMockIdentityResolver()
		mockResolver.resolutions[communityDID] = expectedHandle
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true, mockResolver)

		community := &communities.Community{
			DID:          communityDID,
			Handle:       expectedHandle,
			Name:         communityName,
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
	// Use mock resolver (though these tests don't create communities, so it won't be called)
	mockResolver := newMockIdentityResolver()
	consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true, mockResolver)
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

// mockIdentityResolver is a test double for identity resolution
type mockIdentityResolver struct {
	// Map of DID -> handle for successful resolutions
	resolutions map[string]string
	// If true, Resolve returns an error
	shouldFail bool
	// Track calls to verify invocation
	callCount int
	lastDID   string
}

func newMockIdentityResolver() *mockIdentityResolver {
	return &mockIdentityResolver{
		resolutions: make(map[string]string),
	}
}

func (m *mockIdentityResolver) Resolve(ctx context.Context, did string) (*identity.Identity, error) {
	m.callCount++
	m.lastDID = did

	if m.shouldFail {
		return nil, errors.New("mock PLC resolution failure")
	}

	handle, ok := m.resolutions[did]
	if !ok {
		return nil, fmt.Errorf("no resolution configured for DID: %s", did)
	}

	return &identity.Identity{
		DID:        did,
		Handle:     handle,
		PDSURL:     "https://pds.example.com",
		ResolvedAt: time.Now(),
		Method:     identity.MethodHTTPS,
	}, nil
}

func TestCommunityConsumer_PLCHandleResolution(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("resolves handle from PLC successfully", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)
		communityName := fmt.Sprintf("test-plc-%s", uniqueSuffix)
		expectedHandle := fmt.Sprintf("%s.community.coves.social", communityName)

		// Create mock resolver
		mockResolver := newMockIdentityResolver()
		mockResolver.resolutions[communityDID] = expectedHandle

		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true, mockResolver)

		// Simulate Jetstream event without handle in record
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
					// No handle field - should trigger PLC resolution
					"name":        communityName,
					"displayName": "Test PLC Community",
					"description": "Testing PLC resolution",
					"owner":       "did:web:coves.local",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:coves.local",
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// Handle the event
		if err := consumer.HandleEvent(ctx, event); err != nil {
			t.Fatalf("Failed to handle event: %v", err)
		}

		// Verify mock was called
		if mockResolver.callCount != 1 {
			t.Errorf("Expected 1 PLC resolution call, got %d", mockResolver.callCount)
		}
		if mockResolver.lastDID != communityDID {
			t.Errorf("Expected PLC resolution for DID %s, got %s", communityDID, mockResolver.lastDID)
		}

		// Verify community was indexed with PLC-resolved handle
		community, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to get indexed community: %v", err)
		}

		if community.Handle != expectedHandle {
			t.Errorf("Expected handle %s from PLC, got %s", expectedHandle, community.Handle)
		}
	})

	t.Run("fails when PLC resolution fails (no fallback)", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)
		communityName := fmt.Sprintf("test-plc-fail-%s", uniqueSuffix)

		// Create mock resolver that fails
		mockResolver := newMockIdentityResolver()
		mockResolver.shouldFail = true

		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true, mockResolver)

		// Simulate Jetstream event without handle in record
		event := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev456",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy456def",
				Record: map[string]interface{}{
					"name":        communityName,
					"displayName": "Test PLC Failure",
					"description": "Testing PLC failure",
					"owner":       "did:web:coves.local",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:coves.local",
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// Handle the event - should fail
		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Fatal("Expected error when PLC resolution fails, got nil")
		}

		// Verify error message indicates PLC failure
		expectedErrSubstring := "failed to resolve handle from PLC"
		if !contains(err.Error(), expectedErrSubstring) {
			t.Errorf("Expected error containing '%s', got: %v", expectedErrSubstring, err)
		}

		// Verify community was NOT indexed
		_, err = repo.GetByDID(ctx, communityDID)
		if !communities.IsNotFound(err) {
			t.Errorf("Expected community NOT to be indexed when PLC fails, but got: %v", err)
		}

		// Verify mock was called (failure happened during resolution, not before)
		if mockResolver.callCount != 1 {
			t.Errorf("Expected 1 PLC resolution attempt, got %d", mockResolver.callCount)
		}
	})

	t.Run("test mode rejects invalid hostedBy format", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)
		communityName := fmt.Sprintf("test-invalid-hosted-%s", uniqueSuffix)

		// No identity resolver (test mode)
		consumer := jetstream.NewCommunityEventConsumer(repo, "did:web:coves.local", true, nil)

		// Event with invalid hostedBy format (not did:web)
		event := &jetstream.JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "rev789",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy789ghi",
				Record: map[string]interface{}{
					"name":        communityName,
					"displayName": "Test Invalid HostedBy",
					"description": "Testing validation",
					"owner":       "did:web:coves.local",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:plc:invalid", // Invalid format - not did:web
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		// Handle the event - should fail due to empty handle
		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Fatal("Expected error for invalid hostedBy format in test mode, got nil")
		}

		// Verify error is about handle being required
		expectedErrSubstring := "handle is required"
		if !contains(err.Error(), expectedErrSubstring) {
			t.Errorf("Expected error containing '%s', got: %v", expectedErrSubstring, err)
		}
	})
}
