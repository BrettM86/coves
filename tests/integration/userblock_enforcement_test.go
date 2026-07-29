//go:build integration

package integration

import (
	"Coves/internal/core/comments"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/timeline"
	"Coves/internal/core/userblocks"
	"Coves/tests/testkit"
	"context"
	"fmt"
	"testing"
	"time"

	postgresRepo "Coves/internal/db/postgres"
)

// TestUserBlock_CommunityFeedFiltering verifies that user block enforcement filters
// blocked users' posts from community feeds when a viewer is authenticated,
// but still shows them to unauthenticated viewers.
func TestUserBlock_CommunityFeedFiltering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)

	testID := time.Now().UnixNano()

	// Unique DIDs for this test to avoid collisions
	blockerDID := fmt.Sprintf("did:plc:blocker-feed-%d", testID)
	posterDID := fmt.Sprintf("did:plc:poster-feed-%d", testID)
	// Third-party user whose posts should always be visible (positive assertion)
	thirdPartyDID := fmt.Sprintf("did:plc:thirdparty-feed-%d", testID)
	communityName := fmt.Sprintf("blockfeed-%d", testID)
	ownerHandle := fmt.Sprintf("blockfeed-owner-%d.test", testID)

	// Setup: Create test users
	for _, u := range []struct{ did, handle string }{
		{blockerDID, fmt.Sprintf("blocker-%d.bsky.social", testID)},
		{posterDID, fmt.Sprintf("poster-%d.bsky.social", testID)},
		{thirdPartyDID, fmt.Sprintf("thirdparty-%d.bsky.social", testID)},
	} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO users (did, handle, pds_url, created_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (did) DO NOTHING
		`, u.did, u.handle, getTestPDSURL())
		if err != nil {
			t.Fatalf("Failed to create user %s: %v", u.handle, err)
		}
	}

	// Setup: Create community
	communityDID, err := createFeedTestCommunity(db, ctx, communityName, ownerHandle)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	// Setup: Create posts by poster and third-party
	post1URI := createTestPost(t, db, communityDID, posterDID, "Post by poster 1", 10, time.Now().Add(-3*time.Hour))
	post2URI := createTestPost(t, db, communityDID, posterDID, "Post by poster 2", 20, time.Now().Add(-2*time.Hour))
	thirdPartyPostURI := createTestPost(t, db, communityDID, thirdPartyDID, "Post by third party", 15, time.Now().Add(-1*time.Hour))

	// Create the feed repo (testing at repo level, not handler level)
	feedRepo := postgresRepo.NewCommunityFeedRepository(db, "test-secret")

	// Step 1: Query community feed WITHOUT ViewerDID - should see all posts
	t.Run("unauthenticated sees all posts before block", func(t *testing.T) {
		req := communityFeeds.GetCommunityFeedRequest{
			Community: communityDID,
			Sort:      "new",
			Limit:     50,
		}

		posts, _, err := feedRepo.GetCommunityFeed(ctx, req)
		if err != nil {
			t.Fatalf("GetCommunityFeed failed: %v", err)
		}

		if !feedContainsPost(posts, post1URI) {
			t.Errorf("Expected to find post1 (%s) in unauthenticated feed", post1URI)
		}
		if !feedContainsPost(posts, post2URI) {
			t.Errorf("Expected to find post2 (%s) in unauthenticated feed", post2URI)
		}
		if !feedContainsPost(posts, thirdPartyPostURI) {
			t.Errorf("Expected to find third-party post (%s) in unauthenticated feed", thirdPartyPostURI)
		}
	})

	// Step 2: Create a block (blocker blocks poster)
	_, err = db.ExecContext(ctx, `
		INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
		VALUES ($1, $2, $3, $4)
	`, blockerDID, posterDID,
		fmt.Sprintf("at://%s/social.coves.actor.block/block1", blockerDID),
		"bafyblocktest1")
	if err != nil {
		t.Fatalf("Failed to insert user block: %v", err)
	}

	// Step 3: Blocker should NOT see poster's posts, but SHOULD still see third-party posts
	t.Run("blocker does not see blocked user posts but sees others", func(t *testing.T) {
		req := communityFeeds.GetCommunityFeedRequest{
			Community: communityDID,
			ViewerDID: blockerDID,
			Sort:      "new",
			Limit:     50,
		}

		posts, _, err := feedRepo.GetCommunityFeed(ctx, req)
		if err != nil {
			t.Fatalf("GetCommunityFeed with ViewerDID failed: %v", err)
		}

		if feedContainsPost(posts, post1URI) {
			t.Errorf("Blocker should NOT see post1 (%s) from blocked user", post1URI)
		}
		if feedContainsPost(posts, post2URI) {
			t.Errorf("Blocker should NOT see post2 (%s) from blocked user", post2URI)
		}
		// Positive assertion: non-blocked third-party posts must still be visible
		if !feedContainsPost(posts, thirdPartyPostURI) {
			t.Errorf("Blocker should still see third-party post (%s)", thirdPartyPostURI)
		}
	})

	// Step 4: Blocked user (poster) can still see blocker's content (one-directional block)
	t.Run("blocked user still sees blocker content (one-directional)", func(t *testing.T) {
		// Create a post by the blocker so the blocked user can potentially see it
		blockerPostURI := createTestPost(t, db, communityDID, blockerDID, "Post by blocker", 5, time.Now())

		req := communityFeeds.GetCommunityFeedRequest{
			Community: communityDID,
			ViewerDID: posterDID,
			Sort:      "new",
			Limit:     50,
		}

		posts, _, err := feedRepo.GetCommunityFeed(ctx, req)
		if err != nil {
			t.Fatalf("GetCommunityFeed with blocked user as viewer failed: %v", err)
		}

		if !feedContainsPost(posts, blockerPostURI) {
			t.Errorf("Blocked user should still see blocker's post (%s) — block is one-directional", blockerPostURI)
		}
	})

	// Step 5: Unauthenticated user still sees all posts after block
	t.Run("unauthenticated still sees all posts after block", func(t *testing.T) {
		req := communityFeeds.GetCommunityFeedRequest{
			Community: communityDID,
			Sort:      "new",
			Limit:     50,
		}

		posts, _, err := feedRepo.GetCommunityFeed(ctx, req)
		if err != nil {
			t.Fatalf("GetCommunityFeed without ViewerDID failed: %v", err)
		}

		if !feedContainsPost(posts, post1URI) {
			t.Errorf("Unauthenticated user should still see post1 (%s) after block", post1URI)
		}
		if !feedContainsPost(posts, post2URI) {
			t.Errorf("Unauthenticated user should still see post2 (%s) after block", post2URI)
		}
	})
}

// TestUserBlock_DiscoverFeedFiltering verifies that user block enforcement filters
// blocked users' posts from the discover feed when a viewer is authenticated,
// but still shows them to unauthenticated viewers.
func TestUserBlock_DiscoverFeedFiltering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)

	testID := time.Now().UnixNano()

	blockerDID := fmt.Sprintf("did:plc:blocker-discover-%d", testID)
	posterDID := fmt.Sprintf("did:plc:poster-discover-%d", testID)
	thirdPartyDID := fmt.Sprintf("did:plc:thirdparty-discover-%d", testID)
	communityName := fmt.Sprintf("blockdisc-%d", testID)
	ownerHandle := fmt.Sprintf("blockdisc-owner-%d.test", testID)

	// Setup: Create test users
	for _, u := range []struct{ did, handle string }{
		{blockerDID, fmt.Sprintf("blocker-%d.bsky.social", testID)},
		{posterDID, fmt.Sprintf("poster-%d.bsky.social", testID)},
		{thirdPartyDID, fmt.Sprintf("thirdparty-%d.bsky.social", testID)},
	} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO users (did, handle, pds_url, created_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (did) DO NOTHING
		`, u.did, u.handle, getTestPDSURL())
		if err != nil {
			t.Fatalf("Failed to create user %s: %v", u.handle, err)
		}
	}

	// Setup: Create community
	communityDID, err := createFeedTestCommunity(db, ctx, communityName, ownerHandle)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	// Setup: Create posts by poster and third-party
	post1URI := createTestPost(t, db, communityDID, posterDID, "Discover post 1", 15, time.Now().Add(-3*time.Hour))
	post2URI := createTestPost(t, db, communityDID, posterDID, "Discover post 2", 25, time.Now().Add(-2*time.Hour))
	thirdPartyPostURI := createTestPost(t, db, communityDID, thirdPartyDID, "Discover third party", 20, time.Now().Add(-1*time.Hour))

	// Create the discover repo
	discoverRepo := postgresRepo.NewDiscoverRepository(db, "test-secret")

	// Step 1: Query discover feed WITHOUT ViewerDID - should see all posts
	t.Run("unauthenticated sees all posts before block", func(t *testing.T) {
		req := discover.GetDiscoverRequest{
			Sort:  "new",
			Limit: 50,
		}

		posts, _, err := discoverRepo.GetDiscover(ctx, req)
		if err != nil {
			t.Fatalf("GetDiscover failed: %v", err)
		}

		if !discoverFeedContainsPost(posts, post1URI) {
			t.Errorf("Expected to find post1 (%s) in unauthenticated discover feed", post1URI)
		}
		if !discoverFeedContainsPost(posts, post2URI) {
			t.Errorf("Expected to find post2 (%s) in unauthenticated discover feed", post2URI)
		}
		if !discoverFeedContainsPost(posts, thirdPartyPostURI) {
			t.Errorf("Expected to find third-party post (%s) in unauthenticated discover feed", thirdPartyPostURI)
		}
	})

	// Step 2: Create a block (blocker blocks poster)
	_, err = db.ExecContext(ctx, `
		INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
		VALUES ($1, $2, $3, $4)
	`, blockerDID, posterDID,
		fmt.Sprintf("at://%s/social.coves.actor.block/block1", blockerDID),
		"bafyblockdiscover1")
	if err != nil {
		t.Fatalf("Failed to insert user block: %v", err)
	}

	// Step 3: Blocker should NOT see poster's posts, but SHOULD still see third-party posts
	t.Run("blocker does not see blocked user posts but sees others", func(t *testing.T) {
		req := discover.GetDiscoverRequest{
			ViewerDID: blockerDID,
			Sort:      "new",
			Limit:     50,
		}

		posts, _, err := discoverRepo.GetDiscover(ctx, req)
		if err != nil {
			t.Fatalf("GetDiscover with ViewerDID failed: %v", err)
		}

		if discoverFeedContainsPost(posts, post1URI) {
			t.Errorf("Blocker should NOT see post1 (%s) from blocked user in discover", post1URI)
		}
		if discoverFeedContainsPost(posts, post2URI) {
			t.Errorf("Blocker should NOT see post2 (%s) from blocked user in discover", post2URI)
		}
		// Positive assertion: non-blocked third-party posts must still be visible
		if !discoverFeedContainsPost(posts, thirdPartyPostURI) {
			t.Errorf("Blocker should still see third-party post (%s) in discover", thirdPartyPostURI)
		}
	})

	// Step 4: Unauthenticated user still sees all posts after block
	t.Run("unauthenticated still sees all posts after block", func(t *testing.T) {
		req := discover.GetDiscoverRequest{
			Sort:  "new",
			Limit: 50,
		}

		posts, _, err := discoverRepo.GetDiscover(ctx, req)
		if err != nil {
			t.Fatalf("GetDiscover without ViewerDID failed: %v", err)
		}

		if !discoverFeedContainsPost(posts, post1URI) {
			t.Errorf("Unauthenticated user should still see post1 (%s) after block in discover", post1URI)
		}
		if !discoverFeedContainsPost(posts, post2URI) {
			t.Errorf("Unauthenticated user should still see post2 (%s) after block in discover", post2URI)
		}
	})
}

// TestUserBlock_ProfileViewerState verifies that the user block repository correctly
// returns block records, confirming that GetBlock returns a RecordURI when a block exists.
func TestUserBlock_ProfileViewerState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)

	testID := time.Now().UnixNano()

	userA := fmt.Sprintf("did:plc:profile-viewer-a-%d", testID)
	userB := fmt.Sprintf("did:plc:profile-viewer-b-%d", testID)
	expectedRecordURI := fmt.Sprintf("at://%s/social.coves.actor.block/profileblock1", userA)

	// Create the user block repo
	userBlockRepo := postgresRepo.NewUserBlockRepository(db)

	// Step 1: Verify no block exists initially
	t.Run("no block exists initially", func(t *testing.T) {
		_, err := userBlockRepo.GetBlock(ctx, userA, userB)
		if !userblocks.IsNotFound(err) {
			t.Errorf("Expected ErrBlockNotFound before creating block, got: %v", err)
		}
	})

	// Step 2: Create a block (User A blocks User B) via raw SQL
	_, err := db.ExecContext(ctx, `
		INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
		VALUES ($1, $2, $3, $4)
	`, userA, userB, expectedRecordURI, "bafyprofileblock1")
	if err != nil {
		t.Fatalf("Failed to insert user block: %v", err)
	}

	// Step 3: Verify block exists via repo.GetBlock
	t.Run("GetBlock returns block with correct RecordURI", func(t *testing.T) {
		block, err := userBlockRepo.GetBlock(ctx, userA, userB)
		if err != nil {
			t.Fatalf("GetBlock failed: %v", err)
		}

		if block.BlockerDID != userA {
			t.Errorf("Expected BlockerDID=%s, got %s", userA, block.BlockerDID)
		}
		if block.BlockedDID != userB {
			t.Errorf("Expected BlockedDID=%s, got %s", userB, block.BlockedDID)
		}
		if block.RecordURI != expectedRecordURI {
			t.Errorf("Expected RecordURI=%s, got %s", expectedRecordURI, block.RecordURI)
		}
		if block.RecordURI == "" {
			t.Error("RecordURI should not be empty - needed for profile viewer.blocking hydration")
		}
	})

	// Step 4: Verify the reverse direction does not have a block
	t.Run("reverse direction has no block", func(t *testing.T) {
		_, err := userBlockRepo.GetBlock(ctx, userB, userA)
		if !userblocks.IsNotFound(err) {
			t.Errorf("Expected ErrBlockNotFound for reverse direction (B->A), got: %v", err)
		}
	})
}

// TestUserBlock_CommentFiltering verifies that comments from blocked users are
// filtered out when querying with a viewerDID.
func TestUserBlock_CommentFiltering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)

	testID := time.Now().UnixNano()

	blockerDID := fmt.Sprintf("did:plc:blocker-comment-%d", testID)
	commenterDID := fmt.Sprintf("did:plc:commenter-%d", testID)
	otherCommenterDID := fmt.Sprintf("did:plc:othercommenter-%d", testID)

	// Create users
	for _, u := range []struct{ did, handle string }{
		{blockerDID, fmt.Sprintf("blocker-%d.bsky.social", testID)},
		{commenterDID, fmt.Sprintf("commenter-%d.bsky.social", testID)},
		{otherCommenterDID, fmt.Sprintf("other-%d.bsky.social", testID)},
	} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO users (did, handle, pds_url, created_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (did) DO NOTHING
		`, u.did, u.handle, getTestPDSURL())
		if err != nil {
			t.Fatalf("Failed to create user %s: %v", u.handle, err)
		}
	}

	// Create a parent post URI (the comment's parent)
	parentURI := fmt.Sprintf("at://did:plc:somepost/social.coves.community.post/parent%d", testID)

	// Insert comments from blocked commenter and non-blocked commenter
	commentRepo := postgresRepo.NewCommentRepository(db)

	blockedCommentURI := fmt.Sprintf("at://%s/social.coves.community.comment/c1%d", commenterDID, testID)
	if err := commentRepo.Create(ctx, &comments.Comment{
		URI:          blockedCommentURI,
		CID:          "bafyblocked-comment",
		RKey:         fmt.Sprintf("c1%d", testID),
		CommenterDID: commenterDID,
		RootURI:      parentURI,
		RootCID:      "bafyroot",
		ParentURI:    parentURI,
		ParentCID:    "bafyparent",
		Content:      "Comment from blocked user",
		CreatedAt:    time.Now().Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("Failed to create blocked user comment: %v", err)
	}

	otherCommentURI := fmt.Sprintf("at://%s/social.coves.community.comment/c2%d", otherCommenterDID, testID)
	if err := commentRepo.Create(ctx, &comments.Comment{
		URI:          otherCommentURI,
		CID:          "bafyother-comment",
		RKey:         fmt.Sprintf("c2%d", testID),
		CommenterDID: otherCommenterDID,
		RootURI:      parentURI,
		RootCID:      "bafyroot",
		ParentURI:    parentURI,
		ParentCID:    "bafyparent",
		Content:      "Comment from non-blocked user",
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("Failed to create other user comment: %v", err)
	}

	// Verify both comments visible without block
	t.Run("without block both comments visible", func(t *testing.T) {
		result, _, err := commentRepo.ListByParentWithHotRank(ctx, parentURI, "new", "", 50, nil, blockerDID)
		if err != nil {
			t.Fatalf("ListByParentWithHotRank failed: %v", err)
		}
		if !commentListContains(result, blockedCommentURI) {
			t.Errorf("Expected to find blocked user's comment before block")
		}
		if !commentListContains(result, otherCommentURI) {
			t.Errorf("Expected to find other user's comment before block")
		}
	})

	// Create block
	_, err := db.ExecContext(ctx, `
		INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
		VALUES ($1, $2, $3, $4)
	`, blockerDID, commenterDID,
		fmt.Sprintf("at://%s/social.coves.actor.block/cb%d", blockerDID, testID),
		"bafycommentblock")
	if err != nil {
		t.Fatalf("Failed to insert user block: %v", err)
	}

	// Verify blocked commenter's comment is filtered
	t.Run("blocker does not see blocked user comments", func(t *testing.T) {
		result, _, err := commentRepo.ListByParentWithHotRank(ctx, parentURI, "new", "", 50, nil, blockerDID)
		if err != nil {
			t.Fatalf("ListByParentWithHotRank failed: %v", err)
		}
		if commentListContains(result, blockedCommentURI) {
			t.Errorf("Blocker should NOT see blocked user's comment")
		}
		// Positive assertion: non-blocked comment must remain visible
		if !commentListContains(result, otherCommentURI) {
			t.Errorf("Blocker should still see non-blocked user's comment")
		}
	})
}

// TestUserBlock_TimelineFeedFiltering verifies that user block enforcement filters
// blocked users' posts from the authenticated user's timeline feed.
func TestUserBlock_TimelineFeedFiltering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)

	testID := time.Now().UnixNano()

	viewerDID := fmt.Sprintf("did:plc:viewer-timeline-%d", testID)
	blockedAuthorDID := fmt.Sprintf("did:plc:blocked-timeline-%d", testID)
	otherAuthorDID := fmt.Sprintf("did:plc:other-timeline-%d", testID)
	communityName := fmt.Sprintf("blocktl-%d", testID)
	ownerHandle := fmt.Sprintf("blocktl-owner-%d.test", testID)

	// Create users
	for _, u := range []struct{ did, handle string }{
		{viewerDID, fmt.Sprintf("viewer-%d.bsky.social", testID)},
		{blockedAuthorDID, fmt.Sprintf("blocked-%d.bsky.social", testID)},
		{otherAuthorDID, fmt.Sprintf("other-%d.bsky.social", testID)},
	} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO users (did, handle, pds_url, created_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (did) DO NOTHING
		`, u.did, u.handle, getTestPDSURL())
		if err != nil {
			t.Fatalf("Failed to create user %s: %v", u.handle, err)
		}
	}

	// Create community
	communityDID, err := createFeedTestCommunity(db, ctx, communityName, ownerHandle)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	// Subscribe viewer to the community (required for timeline)
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT DO NOTHING
	`, viewerDID, communityDID)
	if err != nil {
		t.Fatalf("Failed to create community subscription: %v", err)
	}

	// Create posts
	blockedPostURI := createTestPost(t, db, communityDID, blockedAuthorDID, "Post from blocked author", 10, time.Now().Add(-2*time.Hour))
	otherPostURI := createTestPost(t, db, communityDID, otherAuthorDID, "Post from other author", 15, time.Now().Add(-1*time.Hour))

	// Create timeline repo
	timelineRepo := postgresRepo.NewTimelineRepository(db, "test-secret")

	// Step 1: Verify both posts visible before block
	t.Run("both posts visible before block", func(t *testing.T) {
		req := timeline.GetTimelineRequest{
			UserDID: viewerDID,
			Sort:    "new",
			Limit:   50,
		}
		posts, _, err := timelineRepo.GetTimeline(ctx, req)
		if err != nil {
			t.Fatalf("GetTimeline failed: %v", err)
		}
		if !timelineContainsPost(posts, blockedPostURI) {
			t.Errorf("Expected to find blocked author's post before block")
		}
		if !timelineContainsPost(posts, otherPostURI) {
			t.Errorf("Expected to find other author's post before block")
		}
	})

	// Step 2: Create block
	_, err = db.ExecContext(ctx, `
		INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
		VALUES ($1, $2, $3, $4)
	`, viewerDID, blockedAuthorDID,
		fmt.Sprintf("at://%s/social.coves.actor.block/tlblock%d", viewerDID, testID),
		"bafytimelineblock")
	if err != nil {
		t.Fatalf("Failed to insert user block: %v", err)
	}

	// Step 3: Verify blocked author's post is filtered from timeline
	t.Run("blocked author posts filtered from timeline", func(t *testing.T) {
		req := timeline.GetTimelineRequest{
			UserDID: viewerDID,
			Sort:    "new",
			Limit:   50,
		}
		posts, _, err := timelineRepo.GetTimeline(ctx, req)
		if err != nil {
			t.Fatalf("GetTimeline with block failed: %v", err)
		}
		if timelineContainsPost(posts, blockedPostURI) {
			t.Errorf("Blocked author's post should NOT appear in viewer's timeline")
		}
		// Positive assertion: non-blocked author's post must remain visible
		if !timelineContainsPost(posts, otherPostURI) {
			t.Errorf("Non-blocked author's post should still appear in viewer's timeline")
		}
	})
}

// commentListContains checks if a list of comments contains one with the given URI
func commentListContains(list []*comments.Comment, uri string) bool {
	for _, c := range list {
		if c.URI == uri {
			return true
		}
	}
	return false
}

// timelineContainsPost checks if a timeline feed result contains a post with the given URI
func timelineContainsPost(posts []*timeline.FeedViewPost, uri string) bool {
	for _, p := range posts {
		if p.Post != nil && p.Post.URI == uri {
			return true
		}
	}
	return false
}

// feedContainsPost checks if a community feed result contains a post with the given URI
func feedContainsPost(posts []*communityFeeds.FeedViewPost, uri string) bool {
	for _, p := range posts {
		if p.Post != nil && p.Post.URI == uri {
			return true
		}
	}
	return false
}

// discoverFeedContainsPost checks if a discover feed result contains a post with the given URI
func discoverFeedContainsPost(posts []*discover.FeedViewPost, uri string) bool {
	for _, p := range posts {
		if p.Post != nil && p.Post.URI == uri {
			return true
		}
	}
	return false
}
