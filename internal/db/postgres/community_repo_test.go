//go:build integration

package postgres

import (
	"Coves/internal/core/communities"
	"Coves/tests/testkit"
	"context"
	"fmt"
	"testing"
	"time"
)

// The community repository's core write and read paths against real SQL.
//
// Every assertion here is about something Postgres owns rather than something Go
// owns: which unique index a duplicate trips (communities_did_key versus
// communities_handle_key, and therefore ErrCommunityAlreadyExists versus
// ErrHandleTaken), that a missing row surfaces as ErrCommunityNotFound rather
// than sql.ErrNoRows, that Unsubscribe actually removes the row, and that the
// batched GetSubscribedCommunityDIDs answers the same question one row at a time
// would. None of that can be proven against a fake repository, which is why the
// suite lives next to the SQL it exercises instead of in the domain package.
//
// The sibling files split the same repository by concern: community_repo_list_test.go
// covers the four list sorts, community_repo_blocks_test.go the block index, and
// community_repo_credentials_test.go the encrypted PDS credential columns.
//
// Identifiers come from testkit.UniqueID so that rows survive a shared database
// as well as the per-test clone: a timestamp-derived suffix collides between two
// tests that start in the same nanosecond window on different machines.

func TestCommunityRepository_Create(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("creates community successfully", func(t *testing.T) {
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id
		community := &communities.Community{
			DID:                    communityDID,
			Handle:                 fmt.Sprintf("!test-gaming-%s@coves.local", id),
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

	t.Run("starts derived subscriber count at zero", func(t *testing.T) {
		id := testkit.UniqueID(t)
		community := &communities.Community{
			DID:             "did:plc:derivedcount" + id,
			Handle:          fmt.Sprintf("c-derived-count-%s.coves.local", id),
			Name:            "derived-count",
			OwnerDID:        "did:plc:derivedcount" + id,
			CreatedByDID:    "did:plc:user123",
			HostedByDID:     "did:web:coves.local",
			Visibility:      "public",
			SubscriberCount: 2_000_000_000,
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}

		created, err := repo.Create(ctx, community)
		if err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}
		if created.SubscriberCount != 0 {
			t.Fatalf("Expected derived subscriber count to start at zero, got %d", created.SubscriberCount)
		}

		stored, err := repo.GetByDID(ctx, community.DID)
		if err != nil {
			t.Fatalf("Failed to reload community: %v", err)
		}
		if stored.SubscriberCount != 0 {
			t.Errorf("Expected stored subscriber count to start at zero, got %d", stored.SubscriberCount)
		}
	})

	t.Run("returns error for duplicate DID", func(t *testing.T) {
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!duplicate-test-%s@coves.local", id),
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
		if _, err := repo.Create(ctx, community); err != communities.ErrCommunityAlreadyExists {
			t.Errorf("Expected ErrCommunityAlreadyExists, got: %v", err)
		}
	})

	t.Run("returns error for duplicate handle", func(t *testing.T) {
		id := testkit.UniqueID(t)
		handle := fmt.Sprintf("!unique-handle-%s@coves.local", id)

		// First community
		community1 := &communities.Community{
			DID:          "did:plc:test" + id + "a",
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

		// Second community with a different DID but the same handle: the handle
		// index must be the one that rejects it, not the DID index.
		community2 := &communities.Community{
			DID:          "did:plc:test" + id + "b",
			Handle:       handle,
			Name:         "unique-handle",
			OwnerDID:     "did:web:coves.local",
			CreatedByDID: "did:plc:user456",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		if _, err := repo.Create(ctx, community2); err != communities.ErrHandleTaken {
			t.Errorf("Expected ErrHandleTaken, got: %v", err)
		}
	})
}

func TestCommunityRepository_GetByDID(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("retrieves existing community", func(t *testing.T) {
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!getbyid-test-%s@coves.local", id),
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
		fakeDID := "did:plc:test" + testkit.UniqueID(t)
		if _, err := repo.GetByDID(ctx, fakeDID); err != communities.ErrCommunityNotFound {
			t.Errorf("Expected ErrCommunityNotFound, got: %v", err)
		}
	})
}

func TestCommunityRepository_GetByHandle(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("retrieves community by handle", func(t *testing.T) {
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id
		handle := fmt.Sprintf("!handle-lookup-%s@coves.local", id)

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
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db)
	ctx := context.Background()

	// One community for every subscription case below.
	id := testkit.UniqueID(t)
	communityDID := "did:plc:test" + id
	community := &communities.Community{
		DID:          communityDID,
		Handle:       fmt.Sprintf("!subscription-test-%s@coves.local", id),
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
	assertCountMatchesRelationships := func(t *testing.T) {
		t.Helper()
		var stored, relationships int
		if err := db.QueryRowContext(ctx,
			`SELECT subscriber_count FROM communities WHERE did = $1`, communityDID,
		).Scan(&stored); err != nil {
			t.Fatalf("Failed to read stored subscriber count: %v", err)
		}
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM community_subscriptions WHERE community_did = $1`, communityDID,
		).Scan(&relationships); err != nil {
			t.Fatalf("Failed to count subscription relationships: %v", err)
		}
		if stored != relationships {
			t.Fatalf("Stored subscriber count %d does not match %d indexed relationships", stored, relationships)
		}
	}

	t.Run("creates subscription successfully", func(t *testing.T) {
		sub := &communities.Subscription{
			UserDID:           "did:plc:subscriber1",
			CommunityDID:      communityDID,
			ContentVisibility: 3, // Default visibility
			SubscribedAt:      time.Now(),
		}

		created, err := repo.Subscribe(ctx, sub)
		if err != nil {
			t.Fatalf("Failed to subscribe: %v", err)
		}

		if created.ID == 0 {
			t.Error("Expected non-zero subscription ID")
		}
		assertCountMatchesRelationships(t)
	})

	t.Run("prevents duplicate subscriptions", func(t *testing.T) {
		sub := &communities.Subscription{
			UserDID:           "did:plc:duplicate-sub",
			CommunityDID:      communityDID,
			ContentVisibility: 3, // Default visibility
			SubscribedAt:      time.Now(),
		}

		if _, err := repo.Subscribe(ctx, sub); err != nil {
			t.Fatalf("First subscription failed: %v", err)
		}

		// Try to subscribe again
		_, err := repo.Subscribe(ctx, sub)
		if err != communities.ErrSubscriptionAlreadyExists {
			t.Errorf("Expected ErrSubscriptionAlreadyExists, got: %v", err)
		}
		assertCountMatchesRelationships(t)
	})

	t.Run("replay under a new rkey re-points the record without moving the count", func(t *testing.T) {
		// The consumer's idempotency lives in ON CONFLICT DO UPDATE, which only
		// touches record_uri/record_cid. The trigger fires on INSERT, DELETE,
		// and UPDATE OF community_did, so a replay must not reach it. Adding
		// community_did to the conflict SET list would double-count every
		// firehose replay; this is the T1 pin against that.
		userDID := "did:plc:replay-sub"
		first := &communities.Subscription{
			UserDID: userDID, CommunityDID: communityDID, ContentVisibility: 3,
			SubscribedAt: time.Now(),
			RecordURI:    "at://" + userDID + "/social.coves.community.subscription/rkey1",
			RecordCID:    "bafyfirst",
		}
		if _, err := repo.SubscribeWithCount(ctx, first); err != nil {
			t.Fatalf("First SubscribeWithCount failed: %v", err)
		}
		var before int
		if err := db.QueryRowContext(ctx,
			`SELECT subscriber_count FROM communities WHERE did = $1`, communityDID).Scan(&before); err != nil {
			t.Fatalf("Failed to read count: %v", err)
		}

		second := *first
		second.RecordURI = "at://" + userDID + "/social.coves.community.subscription/rkey2"
		second.RecordCID = "bafysecond"
		if _, err := repo.SubscribeWithCount(ctx, &second); err != nil {
			t.Fatalf("Replay SubscribeWithCount failed: %v", err)
		}

		var after int
		var storedURI string
		if err := db.QueryRowContext(ctx,
			`SELECT c.subscriber_count, s.record_uri FROM communities c
			 JOIN community_subscriptions s ON s.community_did = c.did
			 WHERE c.did = $1 AND s.user_did = $2`, communityDID, userDID).Scan(&after, &storedURI); err != nil {
			t.Fatalf("Failed to read replayed row: %v", err)
		}
		if after != before {
			t.Errorf("Replay moved subscriber_count from %d to %d", before, after)
		}
		if storedURI != second.RecordURI {
			t.Errorf("Replay must re-point record_uri to the newer record, got %s", storedURI)
		}
		assertCountMatchesRelationships(t)
	})

	t.Run("unsubscribes successfully", func(t *testing.T) {
		userDID := "did:plc:unsub-user"
		sub := &communities.Subscription{
			UserDID:           userDID,
			CommunityDID:      communityDID,
			ContentVisibility: 3, // Default visibility
			SubscribedAt:      time.Now(),
		}

		_, err := repo.Subscribe(ctx, sub)
		if err != nil {
			t.Fatalf("Failed to subscribe: %v", err)
		}

		err = repo.Unsubscribe(ctx, userDID, communityDID)
		if err != nil {
			t.Fatalf("Failed to unsubscribe: %v", err)
		}

		// The row must be gone, not merely flagged: a soft delete would still
		// satisfy the subscriber count while leaving the user subscribed.
		_, err = repo.GetSubscription(ctx, userDID, communityDID)
		if err != communities.ErrSubscriptionNotFound {
			t.Errorf("Expected ErrSubscriptionNotFound after unsubscribe, got: %v", err)
		}
		assertCountMatchesRelationships(t)
	})
}

func TestCommunityRepository_List(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("lists communities with pagination", func(t *testing.T) {
		// Distinct creation times are set explicitly rather than by spacing the
		// inserts apart in wall-clock time: Create writes the struct's CreatedAt
		// verbatim, so the ordering the sorts see is chosen here.
		base := time.Now()
		for i := 0; i < 5; i++ {
			id := testkit.UniqueID(t)
			community := &communities.Community{
				DID:          "did:plc:test" + id,
				Handle:       fmt.Sprintf("!list-test-%s@coves.local", id),
				Name:         fmt.Sprintf("list-test-%d", i),
				OwnerDID:     "did:web:coves.local",
				CreatedByDID: "did:plc:user123",
				HostedByDID:  "did:web:coves.local",
				Visibility:   "public",
				CreatedAt:    base.Add(-time.Duration(i) * time.Second),
				UpdatedAt:    base.Add(-time.Duration(i) * time.Second),
			}
			if _, err := repo.Create(ctx, community); err != nil {
				t.Fatalf("Failed to create community %d: %v", i, err)
			}
		}

		// List with limit
		req := communities.ListCommunitiesRequest{
			Limit:  3,
			Offset: 0,
		}

		results, err := repo.List(ctx, req)
		if err != nil {
			t.Fatalf("Failed to list communities: %v", err)
		}

		if len(results) != 3 {
			t.Errorf("Expected 3 communities, got %d", len(results))
		}
	})

	t.Run("filters by visibility", func(t *testing.T) {
		id := testkit.UniqueID(t)
		community := &communities.Community{
			DID:          "did:plc:test" + id,
			Handle:       fmt.Sprintf("!unlisted-test-%s@coves.local", id),
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

		results, err := repo.List(ctx, req)
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

// TestCommunityRepository_GetSubscribedCommunityDIDs covers the batched
// membership lookup the list and feed views use to stamp viewer state on many
// communities at once. The interesting cases are the ones a naive IN (...) query
// gets wrong: an empty input slice (which would produce invalid SQL), and DIDs
// that do not exist at all.
func TestCommunityRepository_GetSubscribedCommunityDIDs(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db)
	ctx := context.Background()

	// Three communities so the assertions can distinguish "subscribed",
	// "not subscribed" and "absent from the result" from one another.
	communityDIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		id := testkit.UniqueID(t)
		communityDIDs[i] = "did:plc:test" + id
		community := &communities.Community{
			DID:          communityDIDs[i],
			Handle:       fmt.Sprintf("!batch-sub-test-%s@coves.local", id),
			Name:         fmt.Sprintf("batch-sub-test-%d", i),
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
	}

	userDID := "did:plc:batchsubuser" + testkit.UniqueID(t)

	t.Run("returns empty map when user has no subscriptions", func(t *testing.T) {
		result, err := repo.GetSubscribedCommunityDIDs(ctx, userDID, communityDIDs)
		if err != nil {
			t.Fatalf("Failed to get subscribed community DIDs: %v", err)
		}

		if len(result) != 0 {
			t.Errorf("Expected empty map, got %d entries", len(result))
		}
	})

	t.Run("returns subscribed communities only", func(t *testing.T) {
		// Subscribe to the first and third, leaving a gap in the middle so a
		// query that returned everything it was asked about would fail.
		sub1 := &communities.Subscription{
			UserDID:           userDID,
			CommunityDID:      communityDIDs[0],
			ContentVisibility: 3,
			SubscribedAt:      time.Now(),
		}
		if _, err := repo.Subscribe(ctx, sub1); err != nil {
			t.Fatalf("Failed to subscribe to community 0: %v", err)
		}

		sub3 := &communities.Subscription{
			UserDID:           userDID,
			CommunityDID:      communityDIDs[2],
			ContentVisibility: 3,
			SubscribedAt:      time.Now(),
		}
		if _, err := repo.Subscribe(ctx, sub3); err != nil {
			t.Fatalf("Failed to subscribe to community 2: %v", err)
		}

		result, err := repo.GetSubscribedCommunityDIDs(ctx, userDID, communityDIDs)
		if err != nil {
			t.Fatalf("Failed to get subscribed community DIDs: %v", err)
		}

		if len(result) != 2 {
			t.Errorf("Expected 2 subscribed communities, got %d", len(result))
		}

		if !result[communityDIDs[0]] {
			t.Errorf("Expected community 0 to be subscribed")
		}
		if result[communityDIDs[1]] {
			t.Errorf("Expected community 1 to NOT be subscribed")
		}
		if !result[communityDIDs[2]] {
			t.Errorf("Expected community 2 to be subscribed")
		}
	})

	t.Run("returns empty map for empty community DIDs slice", func(t *testing.T) {
		result, err := repo.GetSubscribedCommunityDIDs(ctx, userDID, []string{})
		if err != nil {
			t.Fatalf("Failed to get subscribed community DIDs: %v", err)
		}

		if len(result) != 0 {
			t.Errorf("Expected empty map for empty input, got %d entries", len(result))
		}
	})

	t.Run("handles non-existent community DIDs gracefully", func(t *testing.T) {
		nonExistentDIDs := []string{
			"did:plc:nonexistent1",
			"did:plc:nonexistent2",
		}

		result, err := repo.GetSubscribedCommunityDIDs(ctx, userDID, nonExistentDIDs)
		if err != nil {
			t.Fatalf("Failed to get subscribed community DIDs: %v", err)
		}

		if len(result) != 0 {
			t.Errorf("Expected empty map for non-existent DIDs, got %d entries", len(result))
		}
	})
}

// Community search has no repository method yet, so there is nothing here to
// cover it. When communities.Repository grows Search, its suite belongs in this
// file alongside List.
