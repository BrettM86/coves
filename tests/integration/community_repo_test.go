package integration

import (
	"Coves/internal/atproto/did"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCommunityRepository_Create(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	didGen := did.NewGenerator(true, "https://plc.directory")
	ctx := context.Background()

	t.Run("creates community successfully", func(t *testing.T) {
		communityDID, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate community DID: %v", err)
		}
		// Generate unique handle using timestamp to avoid collisions
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		community := &communities.Community{
			DID:                    communityDID,
			Handle:                 fmt.Sprintf("!test-gaming-%s@coves.local", uniqueSuffix),
			Name:                   "test-gaming",
			DisplayName:            "Test Gaming Community",
			Description:            "A community for testing",
			OwnerDID:               "did:web:coves.local",
			CreatedByDID:           "did:plc:user123",
			HostedByDID:            "did:web:coves.local",
			Visibility:             "public",
			AllowExternalDiscovery: true,
			CreatedAt:              time.Now(),
			UpdatedAt:              time.Now(),
		}

		created, err := repo.Create(ctx, community)
		if err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		if created.ID == 0 {
			t.Error("Expected non-zero ID")
		}
		if created.DID != communityDID {
			t.Errorf("Expected DID %s, got %s", communityDID, created.DID)
		}
	})

	t.Run("returns error for duplicate DID", func(t *testing.T) {
		communityDID, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate community DID: %v", err)
		}
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!duplicate-test-%s@coves.local", uniqueSuffix),
			Name:         "duplicate-test",
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		// Create first time
		if _, err := repo.Create(ctx, community); err != nil {
			t.Fatalf("First create failed: %v", err)
		}

		// Try to create again with same DID
		if _, err = repo.Create(ctx, community); err != communities.ErrCommunityAlreadyExists {
			t.Errorf("Expected ErrCommunityAlreadyExists, got: %v", err)
		}
	})

	t.Run("returns error for duplicate handle", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		handle := fmt.Sprintf("!unique-handle-%s@coves.local", uniqueSuffix)

		// First community
		did1, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate first community DID: %v", err)
		}
		community1 := &communities.Community{
			DID:          did1,
			Handle:       handle,
			Name:         "unique-handle",
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		if _, err := repo.Create(ctx, community1); err != nil {
			t.Fatalf("First create failed: %v", err)
		}

		// Second community with different DID but same handle
		did2, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate second community DID: %v", err)
		}
		community2 := &communities.Community{
			DID:          did2,
			Handle:       handle, // Same handle!
			Name:         "unique-handle",
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:user456",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		if _, err = repo.Create(ctx, community2); err != communities.ErrHandleTaken {
			t.Errorf("Expected ErrHandleTaken, got: %v", err)
		}
	})
}

func TestCommunityRepository_GetByDID(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	didGen := did.NewGenerator(true, "https://plc.directory")
	ctx := context.Background()

	t.Run("retrieves existing community", func(t *testing.T) {
		communityDID, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate community DID: %v", err)
		}
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!getbyid-test-%s@coves.local", uniqueSuffix),
			Name:         "getbyid-test",
			DisplayName:  "Get By ID Test",
			Description:  "Testing retrieval",
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		created, err := repo.Create(ctx, community)
		if err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		retrieved, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to get community: %v", err)
		}

		if retrieved.DID != created.DID {
			t.Errorf("Expected DID %s, got %s", created.DID, retrieved.DID)
		}
		if retrieved.Handle != created.Handle {
			t.Errorf("Expected Handle %s, got %s", created.Handle, retrieved.Handle)
		}
		if retrieved.DisplayName != created.DisplayName {
			t.Errorf("Expected DisplayName %s, got %s", created.DisplayName, retrieved.DisplayName)
		}
	})

	t.Run("returns error for non-existent community", func(t *testing.T) {
		fakeDID, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate fake DID: %v", err)
		}
		if _, err := repo.GetByDID(ctx, fakeDID); err != communities.ErrCommunityNotFound {
			t.Errorf("Expected ErrCommunityNotFound, got: %v", err)
		}
	})
}

func TestCommunityRepository_GetByHandle(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	didGen := did.NewGenerator(true, "https://plc.directory")
	ctx := context.Background()

	t.Run("retrieves community by handle", func(t *testing.T) {
		communityDID, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate community DID: %v", err)
		}
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		handle := fmt.Sprintf("!handle-lookup-%s@coves.local", uniqueSuffix)

		community := &communities.Community{
			DID:          communityDID,
			Handle:       handle,
			Name:         "handle-lookup",
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

		retrieved, err := repo.GetByHandle(ctx, handle)
		if err != nil {
			t.Fatalf("Failed to get community by handle: %v", err)
		}

		if retrieved.Handle != handle {
			t.Errorf("Expected handle %s, got %s", handle, retrieved.Handle)
		}
		if retrieved.DID != communityDID {
			t.Errorf("Expected DID %s, got %s", communityDID, retrieved.DID)
		}
	})
}

func TestCommunityRepository_Subscriptions(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	didGen := did.NewGenerator(true, "https://plc.directory")
	ctx := context.Background()

	// Create a community for subscription tests
	communityDID, err := didGen.GenerateCommunityDID()
	if err != nil {
		t.Fatalf("Failed to generate community DID: %v", err)
	}
	uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	community := &communities.Community{
		DID:          communityDID,
		Handle:       fmt.Sprintf("!subscription-test-%s@coves.local", uniqueSuffix),
		Name:         "subscription-test",
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

	t.Run("creates subscription successfully", func(t *testing.T) {
		sub := &communities.Subscription{
			UserDID:      "did:plc:subscriber1",
			CommunityDID: communityDID,
			SubscribedAt: time.Now(),
		}

		created, err := repo.Subscribe(ctx, sub)
		if err != nil {
			t.Fatalf("Failed to subscribe: %v", err)
		}

		if created.ID == 0 {
			t.Error("Expected non-zero subscription ID")
		}
	})

	t.Run("prevents duplicate subscriptions", func(t *testing.T) {
		sub := &communities.Subscription{
			UserDID:      "did:plc:duplicate-sub",
			CommunityDID: communityDID,
			SubscribedAt: time.Now(),
		}

		if _, err := repo.Subscribe(ctx, sub); err != nil {
			t.Fatalf("First subscription failed: %v", err)
		}

		// Try to subscribe again
		_, err = repo.Subscribe(ctx, sub)
		if err != communities.ErrSubscriptionAlreadyExists {
			t.Errorf("Expected ErrSubscriptionAlreadyExists, got: %v", err)
		}
	})

	t.Run("unsubscribes successfully", func(t *testing.T) {
		userDID := "did:plc:unsub-user"
		sub := &communities.Subscription{
			UserDID:      userDID,
			CommunityDID: communityDID,
			SubscribedAt: time.Now(),
		}

		_, err := repo.Subscribe(ctx, sub)
		if err != nil {
			t.Fatalf("Failed to subscribe: %v", err)
		}

		err = repo.Unsubscribe(ctx, userDID, communityDID)
		if err != nil {
			t.Fatalf("Failed to unsubscribe: %v", err)
		}

		// Verify subscription is gone
		_, err = repo.GetSubscription(ctx, userDID, communityDID)
		if err != communities.ErrSubscriptionNotFound {
			t.Errorf("Expected ErrSubscriptionNotFound after unsubscribe, got: %v", err)
		}
	})
}

func TestCommunityRepository_List(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	didGen := did.NewGenerator(true, "https://plc.directory")
	ctx := context.Background()

	t.Run("lists communities with pagination", func(t *testing.T) {
		// Create multiple communities
		baseSuffix := time.Now().UnixNano()
		for i := 0; i < 5; i++ {
			communityDID, err := didGen.GenerateCommunityDID()
			if err != nil {
				t.Fatalf("Failed to generate community DID: %v", err)
			}
			community := &communities.Community{
				DID:          communityDID,
				Handle:       fmt.Sprintf("!list-test-%d-%d@coves.local", baseSuffix, i),
				Name:         fmt.Sprintf("list-test-%d", i),
				OwnerDID:     "did:web:coves.local",
				CreatedByDID: "did:plc:user123",
				HostedByDID:  "did:web:coves.local",
				Visibility:   "public",
				CreatedAt:    time.Now(),
				UpdatedAt:    time.Now(),
			}
			if _, err := repo.Create(ctx, community); err != nil {
				t.Fatalf("Failed to create community %d: %v", i, err)
			}
			time.Sleep(10 * time.Millisecond) // Ensure different timestamps
		}

		// List with limit
		req := communities.ListCommunitiesRequest{
			Limit:  3,
			Offset: 0,
		}

		results, total, err := repo.List(ctx, req)
		if err != nil {
			t.Fatalf("Failed to list communities: %v", err)
		}

		if len(results) != 3 {
			t.Errorf("Expected 3 communities, got %d", len(results))
		}

		if total < 5 {
			t.Errorf("Expected total >= 5, got %d", total)
		}
	})

	t.Run("filters by visibility", func(t *testing.T) {
		// Create an unlisted community
		communityDID, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate community DID: %v", err)
		}
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!unlisted-test-%s@coves.local", uniqueSuffix),
			Name:         "unlisted-test",
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "unlisted",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		if _, err := repo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create unlisted community: %v", err)
		}

		// List only public communities
		req := communities.ListCommunitiesRequest{
			Limit:      100,
			Offset:     0,
			Visibility: "public",
		}

		results, _, err := repo.List(ctx, req)
		if err != nil {
			t.Fatalf("Failed to list public communities: %v", err)
		}

		// Verify no unlisted communities in results
		for _, c := range results {
			if c.Visibility != "public" {
				t.Errorf("Found non-public community in public-only results: %s", c.Handle)
			}
		}
	})
}

func TestCommunityRepository_Search(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	didGen := did.NewGenerator(true, "https://plc.directory")
	ctx := context.Background()

	t.Run("searches communities by name", func(t *testing.T) {
		// Create a community with searchable name
		communityDID, err := didGen.GenerateCommunityDID()
		if err != nil {
			t.Fatalf("Failed to generate community DID: %v", err)
		}
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!golang-search-%s@coves.local", uniqueSuffix),
			Name:         "golang-search",
			DisplayName:  "Go Programming",
			Description:  "A community for Go developers",
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

		// Search for it
		req := communities.SearchCommunitiesRequest{
			Query:  "golang",
			Limit:  10,
			Offset: 0,
		}

		results, total, err := repo.Search(ctx, req)
		if err != nil {
			t.Fatalf("Failed to search communities: %v", err)
		}

		if total == 0 {
			t.Error("Expected to find at least one result")
		}

		// Verify our community is in results
		found := false
		for _, c := range results {
			if c.DID == communityDID {
				found = true
				break
			}
		}

		if !found {
			t.Error("Expected to find created community in search results")
		}
	})
}
