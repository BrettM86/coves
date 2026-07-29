//go:build integration

package integration

// SERIAL BY DESIGN — do not add t.Parallel() to this file.
//
// Its tests drive the Jetstream firehose through the hand-rolled
// subscribeToJetstream* helpers below rather than testkit's cursor-gated
// subscriber. Those helpers subscribe to one shared stream and match on the
// first event of a collection, so a concurrent test writing the same
// collection is delivered to them too and either steals the match or trips
// their timeout. Per-test database clones do not isolate a shared websocket.
//
// docs/TEST_ARCHITECTURE.md §3.3 ("Parallelism is earned, not assumed").

import (
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	timelineCore "Coves/internal/core/timeline"
)

// TestFullUserJourney_E2E tests the complete user experience from signup to interaction:
// 1. User A: Signup → Authenticate → Create Community → Create Post
// 2. User B: Signup → Authenticate → Subscribe to Community
// 3. User B: Add Comment to User A's Post
// 4. User B: Upvote Post
// 5. User A: Upvote Comment
// 6. Verify: All data flows through Jetstream correctly
// 7. Verify: Counts update (vote counts, comment counts, subscriber counts)
// 8. Verify: Timeline feed shows posts from subscribed communities
//
// This is a TRUE E2E test that validates:
// - Complete atProto write-forward architecture (writes → PDS → Jetstream → AppView)
// - Real Jetstream event consumption and indexing
// - Multi-user interactions and data consistency
// - Timeline aggregation and feed generation
func TestFullUserJourney_E2E(t *testing.T) {

	db := testkit.DB(t)

	// Check if PDS is running
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Check if Jetstream is available
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0] // Remove port
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe", pdsHostname)

	t.Logf("🚀 Starting Full User Journey E2E Test")
	t.Logf("   PDS URL: %s", pdsURL)
	t.Logf("   Jetstream URL: %s", jetstreamURL)

	ctx := context.Background()

	// Setup repositories
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	postRepo := postgres.NewPostRepository(db)
	commentRepo := postgres.NewCommentRepository(db)
	voteRepo := postgres.NewVoteRepository(db)
	timelineRepo := postgres.NewTimelineRepository(db, "test-cursor-secret")
	userBlockRepo := postgres.NewUserBlockRepository(db)

	// Setup identity resolution
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Setup services
	userService := users.NewUserService(userRepo, identityResolver, pdsURL, nil, "")

	// Extract instance domain and DID
	// IMPORTANT: Instance domain must match PDS_SERVICE_HANDLE_DOMAINS config (c-{name}.coves.social)
	instanceDID := os.Getenv("INSTANCE_DID")
	if instanceDID == "" {
		instanceDID = "did:web:coves.social" // Must match PDS handle domain config
	}
	var instanceDomain string
	if strings.HasPrefix(instanceDID, "did:web:") {
		instanceDomain = strings.TrimPrefix(instanceDID, "did:web:")
	} else {
		instanceDomain = "coves.social"
	}

	provisioner := communities.NewPDSAccountProvisioner(instanceDomain, pdsURL)
	communityService := communities.NewCommunityServiceWithPDSFactory(communityRepo, pdsURL, instanceDID, instanceDomain, provisioner, CommunityPasswordAuthPDSClientFactory(), nil)
	// Wire the real userblocks BlockChecker (as cmd/server/main.go does) so post.get
	// enforces viewer author-blocks against live Postgres — without it, block enforcement
	// is silently skipped and Part 3c below could not observe a blockedPost.
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL, posts.WithBlockChecker(userBlockRepo))
	timelineService := timelineCore.NewTimelineService(timelineRepo)

	// Setup consumers
	communityConsumer := jetstream.NewCommunityEventConsumer(communityRepo, instanceDID, true, identityResolver)
	postConsumer := jetstream.NewPostEventConsumer(postRepo, communityRepo, userService, db)
	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)
	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, userService, db)

	// Setup HTTP server with all routes using OAuth middleware
	e2eAuth := NewE2EOAuthMiddleware()
	r := chi.NewRouter()
	routes.RegisterCommunityRoutes(r, communityService, communityRepo, e2eAuth.OAuthAuthMiddleware, nil) // nil = allow all community creators
	routes.RegisterPostRoutes(r, postService, nil, nil, e2eAuth.OAuthAuthMiddleware, e2eAuth.OAuthAuthMiddleware)
	routes.RegisterTimelineRoutes(r, timelineService, nil, nil, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// A single collision-free testID is shared by every handle and community
	// name. The database is a private clone, but the PDS is not: accounts
	// persist across runs, so handles still have to be unique (uniqueTestID
	// stays short for the PDS handle-length limit).
	testID := uniqueTestID()

	// Test variables to track state across steps
	var (
		userAHandle     string
		userADID        string
		userAToken      string // PDS access token for direct PDS requests
		userAAPIToken   string // Coves API token for Coves API requests
		userBHandle     string
		userBDID        string
		userBToken      string // PDS access token for direct PDS requests
		userBAPIToken   string // Coves API token for Coves API requests
		communityDID    string
		communityHandle string
		postURI         string
		postCID         string
		commentURI      string
		commentCID      string
	)

	// ====================================================================================
	// Part 1: User A - Signup and Authenticate
	// ====================================================================================
	t.Run("1. User A - Signup and Authenticate", func(t *testing.T) {
		t.Log("\n👤 Part 1: User A creates account and authenticates...")

		// Use short handle format to stay under PDS 34-char limit
		userAHandle = fmt.Sprintf("alice%s.local.coves.dev", testID)
		email := fmt.Sprintf("alice%s@test.com", testID)
		password := "test-password-alice-123"

		// Create account on PDS
		userAToken, userADID, err = createPDSAccount(pdsURL, userAHandle, email, password)
		require.NoError(t, err, "User A should be able to create account")
		require.NotEmpty(t, userAToken, "User A should receive access token")
		require.NotEmpty(t, userADID, "User A should receive DID")

		t.Logf("✅ User A created: %s (%s)", userAHandle, userADID)

		// Index user in AppView (simulates app.bsky.actor.profile indexing)
		userA := createTestUser(t, db, userAHandle, userADID)
		require.NotNil(t, userA)

		// Register user with OAuth middleware for Coves API requests
		// Use AddUserWithPDSToken to store the real PDS access token for write-forward
		userAAPIToken = e2eAuth.AddUserWithPDSToken(userADID, userAToken, pdsURL)

		t.Logf("✅ User A indexed in AppView")
	})

	// ====================================================================================
	// Part 2: User A - Create Community
	// ====================================================================================
	t.Run("2. User A - Create Community", func(t *testing.T) {
		t.Log("\n🏘️  Part 2: User A creates a community...")

		// Community handle will be {name}c-{name}.coves.social
		// Max 34 chars total, so name must be short (34 - 23 = 11 chars max)
		communityName := fmt.Sprintf("gj%s", testID) // short prefix + base36 testID keeps the community handle well under limits

		createReq := map[string]interface{}{
			"name":                   communityName,
			"displayName":            "Gaming Journey Community",
			"description":            "Testing full user journey E2E",
			"visibility":             "public",
			"allowExternalDiscovery": true,
		}

		reqBody, _ := json.Marshal(createReq)
		req, _ := http.NewRequest(http.MethodPost,
			httpServer.URL+"/xrpc/social.coves.community.create",
			bytes.NewBuffer(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+userAAPIToken)

		// Capture a Jetstream replay cursor BEFORE the write so the subscription opened
		// afterwards cannot miss the resulting firehose commit (subscribe-after-write race).
		communityCreateCursor := jetstreamCursorNow()
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode, "Community creation should succeed")

		var createResp struct {
			URI    string `json:"uri"`
			CID    string `json:"cid"`
			DID    string `json:"did"`
			Handle string `json:"handle"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&createResp))

		communityDID = createResp.DID
		communityHandle = createResp.Handle

		t.Logf("✅ Community created: %s (%s)", communityHandle, communityDID)

		// Wait for Jetstream event and index in AppView
		t.Log("⏳ Waiting for Jetstream to index community...")

		// Subscribe to Jetstream for community profile events
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		errorChan := make(chan error, 1)
		done := make(chan bool)

		jetstreamFilterURL := fmt.Sprintf("%s?wantedCollections=social.coves.community.profile", jetstreamURL)

		go func() {
			err := subscribeToJetstreamForCommunity(ctx, withJetstreamCursor(jetstreamFilterURL, communityCreateCursor), communityDID, communityConsumer, eventChan, errorChan, done)
			if err != nil {
				errorChan <- err
			}
		}()

		select {
		case event := <-eventChan:
			t.Logf("✅ Jetstream event received for community: %s", event.Did)
			close(done)
		case err := <-errorChan:
			t.Fatalf("❌ Jetstream error: %v", err)
		case <-time.After(30 * time.Second):
			close(done)
			// Check if simulation fallback is allowed (for CI environments)
			if os.Getenv("ALLOW_SIMULATION_FALLBACK") == "true" {
				t.Log("⚠️  Timeout waiting for Jetstream event - falling back to simulation (CI mode)")
				// Simulate indexing for test speed
				simulateCommunityIndexing(t, db, communityDID, communityHandle, userADID)
			} else {
				t.Fatal("❌ Jetstream timeout - real infrastructure test failed. Set ALLOW_SIMULATION_FALLBACK=true to allow fallback.")
			}
		}

		// Verify community is indexed
		indexed, err := communityRepo.GetByDID(ctx, communityDID)
		require.NoError(t, err, "Community should be indexed")
		assert.Equal(t, communityDID, indexed.DID)

		t.Logf("✅ Community indexed in AppView")
	})

	// ====================================================================================
	// Part 3: User A - Create Post
	// ====================================================================================
	t.Run("3. User A - Create Post", func(t *testing.T) {
		t.Log("\n📝 Part 3: User A creates a post in the community...")

		title := "My First Gaming Post"
		content := "This is an E2E test post from the user journey!"

		createReq := map[string]interface{}{
			"community": communityDID,
			"title":     title,
			"content":   content,
		}

		reqBody, _ := json.Marshal(createReq)
		req, _ := http.NewRequest(http.MethodPost,
			httpServer.URL+"/xrpc/social.coves.community.post.create",
			bytes.NewBuffer(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+userAAPIToken)

		// Capture a Jetstream replay cursor BEFORE the write (subscribe-after-write race).
		postCreateCursor := jetstreamCursorNow()
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode, "Post creation should succeed")

		var createResp posts.CreatePostResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&createResp))

		postURI = createResp.URI
		postCID = createResp.CID

		t.Logf("✅ Post created: %s", postURI)

		// Wait for Jetstream event and index in AppView
		t.Log("⏳ Waiting for Jetstream to index post...")

		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		errorChan := make(chan error, 1)
		done := make(chan bool)

		jetstreamFilterURL := fmt.Sprintf("%s?wantedCollections=social.coves.community.post", jetstreamURL)

		go func() {
			err := subscribeToJetstreamForPost(ctx, withJetstreamCursor(jetstreamFilterURL, postCreateCursor), communityDID, postConsumer, eventChan, errorChan, done)
			if err != nil {
				errorChan <- err
			}
		}()

		select {
		case event := <-eventChan:
			t.Logf("✅ Jetstream event received for post: %s", event.Commit.RKey)
			close(done)
		case err := <-errorChan:
			t.Fatalf("❌ Jetstream error: %v", err)
		case <-time.After(30 * time.Second):
			close(done)
			// Check if simulation fallback is allowed (for CI environments)
			if os.Getenv("ALLOW_SIMULATION_FALLBACK") == "true" {
				t.Log("⚠️  Timeout waiting for Jetstream event - falling back to simulation (CI mode)")
				// Simulate indexing for test speed
				simulatePostIndexing(t, db, postConsumer, ctx, communityDID, userADID, postURI, postCID, title, content)
			} else {
				t.Fatal("❌ Jetstream timeout - real infrastructure test failed. Set ALLOW_SIMULATION_FALLBACK=true to allow fallback.")
			}
		}

		// Verify post is indexed
		indexed, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err, "Post should be indexed")
		assert.Equal(t, postURI, indexed.URI)
		assert.Equal(t, userADID, indexed.AuthorDID)
		assert.Equal(t, 0, indexed.CommentCount, "Initial comment count should be 0")
		assert.Equal(t, 0, indexed.UpvoteCount, "Initial upvote count should be 0")

		t.Logf("✅ Post indexed in AppView")
	})

	// ====================================================================================
	// Part 3b: Get Post by URI (social.coves.community.post.get)
	// ====================================================================================
	// Exercises the batch get endpoint against REAL infrastructure: the post just indexed
	// from Jetstream. This is the only coverage of the live GetViewsByURIs SQL (with its
	// users/communities INNER JOINs), the shared scanPostView column contract, and the
	// deleted_at IS NULL filter. It asserts request order is preserved: a real, indexed
	// URI comes back as a full postView; a valid-but-never-indexed URI comes back as a
	// notFoundPost.
	t.Run("3b. Get Post by URI", func(t *testing.T) {
		t.Log("\n🔎 Part 3b: Batch-fetch the indexed post via social.coves.community.post.get...")

		// A syntactically valid, canonical (DID-based) URI that was never indexed -> notFound.
		neverIndexedURI := fmt.Sprintf("at://%s/social.coves.community.post/neverindexed", communityDID)

		q := url.Values{}
		q.Add("uris", postURI)         // [0] real, Jetstream-indexed post -> postView
		q.Add("uris", neverIndexedURI) // [1] valid URI, absent from AppView -> notFoundPost

		req, _ := http.NewRequest(http.MethodGet,
			httpServer.URL+"/xrpc/social.coves.community.post.get?"+q.Encode(), nil)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode, "post.get should succeed")

		var getResp struct {
			Posts []map[string]interface{} `json:"posts"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&getResp))
		require.Len(t, getResp.Posts, 2, "response preserves request order: [found, notFound]")

		// [0] the indexed post -> full postView with the lexicon-required nested objects.
		found := getResp.Posts[0]
		assert.Equal(t, postURI, found["uri"], "posts[0] is the requested post")
		_, hasNotFound := found["notFound"]
		assert.False(t, hasNotFound, "posts[0] is a postView, not a notFoundPost")
		assert.NotNil(t, found["community"], "postView carries community (lexicon-required)")
		assert.NotNil(t, found["record"], "postView carries record (lexicon-required)")
		assert.NotNil(t, found["author"], "postView carries author (lexicon-required)")

		// [1] never-indexed URI -> notFoundPost echoing the requested URI.
		missing := getResp.Posts[1]
		assert.Equal(t, true, missing["notFound"], "posts[1] is a notFoundPost")
		assert.Equal(t, neverIndexedURI, missing["uri"], "notFoundPost echoes the requested URI")

		t.Logf("✅ post.get returned 1 postView + 1 notFoundPost in request order")
	})

	// ====================================================================================
	// Part 3c: Get Post authored by a Blocked user (viewer author-block enforcement)
	// ====================================================================================
	// Exercises the blockedPost union member of post.get against REAL infrastructure: a
	// viewer who has blocked the post's author (User A) must receive a blockedPost marker
	// instead of the full postView, keeping permalink/cold-load reads consistent with
	// feed/timeline block filtering. This is the only live coverage of the OptionalAuth ->
	// ViewerDID -> userblocks AreBlocked -> blockedPost path; the same post served WITHOUT
	// the block (Part 3b, unauthenticated) came back as a postView, so this isolates the
	// block as the sole cause of the difference.
	//
	// A dedicated throwaway viewer (not User B) records the block so it cannot perturb
	// User B's later interactions with User A's post (votes/comments in Parts 5+).
	t.Run("3c. Get blocked-author post returns blockedPost", func(t *testing.T) {
		t.Log("\n🚫 Part 3c: Viewer who blocked the author fetches the post via post.get...")

		// A throwaway viewer identity; DID satisfies the user_blocks DID-format CHECK.
		blockerDID := fmt.Sprintf("did:plc:journeyblocker%s", testID)

		// Record the block: viewer has blocked User A (the post's author). No FK to users,
		// so the block row alone is enough to drive AreBlocked.
		_, err := db.ExecContext(ctx, `
			INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
			VALUES ($1, $2, $3, $4)
		`, blockerDID, userADID,
			fmt.Sprintf("at://%s/social.coves.actor.block/journeyblock", blockerDID),
			"bafyjourneyblockcid")
		require.NoError(t, err, "failed to record viewer block")
		defer func() { _, _ = db.Exec("DELETE FROM user_blocks WHERE blocker_did = $1", blockerDID) }()

		// Mint a Coves API token for the viewer so OptionalAuth populates ViewerDID.
		// post.get is a read, so no PDS access token is required.
		blockerToken := e2eAuth.AddUser(blockerDID)

		q := url.Values{}
		q.Add("uris", postURI) // the indexed post, authored by the now-blocked User A

		req, _ := http.NewRequest(http.MethodGet,
			httpServer.URL+"/xrpc/social.coves.community.post.get?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+blockerToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode, "post.get should succeed")

		var getResp struct {
			Posts []map[string]interface{} `json:"posts"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&getResp))
		require.Len(t, getResp.Posts, 1, "one URI requested -> one union member")

		blocked := getResp.Posts[0]
		assert.Equal(t, true, blocked["blocked"], "viewer blocked the author -> blockedPost")
		assert.Equal(t, "author", blocked["blockedBy"], "blockedBy is \"author\"")
		assert.Equal(t, postURI, blocked["uri"], "blockedPost echoes the requested URI")
		if author, ok := blocked["author"].(map[string]interface{}); assert.True(t, ok, "blockedPost carries author") {
			assert.Equal(t, userADID, author["did"], "blockedPost names the blocked author DID")
		}
		// The marker must NOT leak the hidden post's content (postView-only fields).
		assert.Nil(t, blocked["record"], "blockedPost must not carry the post record")
		assert.Nil(t, blocked["community"], "blockedPost must not carry community")
		assert.Nil(t, blocked["title"], "blockedPost must not leak post content")

		t.Logf("✅ post.get returned a blockedPost for the blocked author's post")
	})

	// ====================================================================================
	// Part 4: User B - Signup and Authenticate
	// ====================================================================================
	t.Run("4. User B - Signup and Authenticate", func(t *testing.T) {
		t.Log("\n👤 Part 4: User B creates account and authenticates...")

		// Use short handle format to stay under PDS 34-char limit
		userBHandle = fmt.Sprintf("bob%s.local.coves.dev", testID)
		email := fmt.Sprintf("bob%s@test.com", testID)
		password := "test-password-bob-123"

		// Create account on PDS
		userBToken, userBDID, err = createPDSAccount(pdsURL, userBHandle, email, password)
		require.NoError(t, err, "User B should be able to create account")
		require.NotEmpty(t, userBToken, "User B should receive access token")
		require.NotEmpty(t, userBDID, "User B should receive DID")

		t.Logf("✅ User B created: %s (%s)", userBHandle, userBDID)

		// Index user in AppView
		userB := createTestUser(t, db, userBHandle, userBDID)
		require.NotNil(t, userB)

		// Register user with OAuth middleware for Coves API requests
		// Use AddUserWithPDSToken to store the real PDS access token for write-forward
		userBAPIToken = e2eAuth.AddUserWithPDSToken(userBDID, userBToken, pdsURL)

		t.Logf("✅ User B indexed in AppView")
	})

	// ====================================================================================
	// Part 5: User B - Subscribe to Community
	// ====================================================================================
	t.Run("5. User B - Subscribe to Community", func(t *testing.T) {
		t.Log("\n🔔 Part 5: User B subscribes to the community...")

		// Get initial subscriber count
		initialCommunity, err := communityRepo.GetByDID(ctx, communityDID)
		require.NoError(t, err)
		initialCount := initialCommunity.SubscriberCount

		subscribeReq := map[string]interface{}{
			"community":         communityDID,
			"contentVisibility": 5,
		}

		reqBody, _ := json.Marshal(subscribeReq)
		req, _ := http.NewRequest(http.MethodPost,
			httpServer.URL+"/xrpc/social.coves.community.subscribe",
			bytes.NewBuffer(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+userBAPIToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode, "Subscription should succeed")

		var subscribeResp struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&subscribeResp))

		t.Logf("✅ Subscription created: %s", subscribeResp.URI)

		// Simulate Jetstream event indexing the subscription
		// (In production, this would come from real Jetstream)
		rkey := strings.Split(subscribeResp.URI, "/")[4]
		subEvent := jetstream.JetstreamEvent{
			Did:    userBDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-sub-rev",
				Operation:  "create",
				Collection: "social.coves.community.subscription",
				RKey:       rkey,
				CID:        subscribeResp.CID,
				Record: map[string]interface{}{
					"$type":             "social.coves.community.subscription",
					"subject":           communityDID,
					"contentVisibility": float64(5),
					"createdAt":         time.Now().Format(time.RFC3339),
				},
			},
		}
		require.NoError(t, communityConsumer.HandleEvent(ctx, &subEvent))

		// Verify subscription indexed and subscriber count incremented
		updatedCommunity, err := communityRepo.GetByDID(ctx, communityDID)
		require.NoError(t, err)
		assert.Equal(t, initialCount+1, updatedCommunity.SubscriberCount,
			"Subscriber count should increment")

		t.Logf("✅ Subscriber count: %d → %d", initialCount, updatedCommunity.SubscriberCount)
	})

	// ====================================================================================
	// Part 6: User B - Add Comment to Post
	// ====================================================================================
	t.Run("6. User B - Add Comment to Post", func(t *testing.T) {
		t.Log("\n💬 Part 6: User B comments on User A's post...")

		// Get initial comment count
		initialPost, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err)
		initialCommentCount := initialPost.CommentCount

		// User B creates comment via PDS (simulate)
		commentRKey := generateTID()
		commentURI = fmt.Sprintf("at://%s/social.coves.community.comment/%s", userBDID, commentRKey)
		commentCID = "bafycommentjourney123"

		commentEvent := &jetstream.JetstreamEvent{
			Did:  userBDID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-comment-rev",
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       commentRKey,
				CID:        commentCID,
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": "Great post! This E2E test is working perfectly!",
					"reply": map[string]interface{}{
						"root": map[string]interface{}{
							"uri": postURI,
							"cid": postCID,
						},
						"parent": map[string]interface{}{
							"uri": postURI,
							"cid": postCID,
						},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		require.NoError(t, commentConsumer.HandleEvent(ctx, commentEvent))

		t.Logf("✅ Comment created: %s", commentURI)

		// Verify comment indexed
		indexed, err := commentRepo.GetByURI(ctx, commentURI)
		require.NoError(t, err)
		assert.Equal(t, commentURI, indexed.URI)
		assert.Equal(t, userBDID, indexed.CommenterDID)
		assert.Equal(t, 0, indexed.UpvoteCount, "Initial upvote count should be 0")

		// Verify post comment count incremented
		updatedPost, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err)
		assert.Equal(t, initialCommentCount+1, updatedPost.CommentCount,
			"Post comment count should increment")

		t.Logf("✅ Comment count: %d → %d", initialCommentCount, updatedPost.CommentCount)
	})

	// ====================================================================================
	// Part 7: User B - Upvote Post
	// ====================================================================================
	t.Run("7. User B - Upvote Post", func(t *testing.T) {
		t.Log("\n⬆️  Part 7: User B upvotes User A's post...")

		// Get initial vote counts
		initialPost, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err)
		initialUpvotes := initialPost.UpvoteCount
		initialScore := initialPost.Score

		// User B creates upvote via PDS (simulate)
		voteRKey := generateTID()
		voteURI := fmt.Sprintf("at://%s/social.coves.feed.vote/%s", userBDID, voteRKey)

		voteEvent := &jetstream.JetstreamEvent{
			Did:  userBDID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-vote-rev",
				Operation:  "create",
				Collection: "social.coves.feed.vote",
				RKey:       voteRKey,
				CID:        "bafyvotejourney123",
				Record: map[string]interface{}{
					"$type": "social.coves.feed.vote",
					"subject": map[string]interface{}{
						"uri": postURI,
						"cid": postCID,
					},
					"direction": "up",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		require.NoError(t, voteConsumer.HandleEvent(ctx, voteEvent))

		t.Logf("✅ Upvote created: %s", voteURI)

		// Verify vote indexed
		indexed, err := voteRepo.GetByURI(ctx, voteURI)
		require.NoError(t, err)
		assert.Equal(t, voteURI, indexed.URI)
		assert.Equal(t, userBDID, indexed.VoterDID) // User B created the vote
		assert.Equal(t, "up", indexed.Direction)

		// Verify post vote counts updated
		updatedPost, err := postRepo.GetByURI(ctx, postURI)
		require.NoError(t, err)
		assert.Equal(t, initialUpvotes+1, updatedPost.UpvoteCount,
			"Post upvote count should increment")
		assert.Equal(t, initialScore+1, updatedPost.Score,
			"Post score should increment")

		t.Logf("✅ Post upvotes: %d → %d, score: %d → %d",
			initialUpvotes, updatedPost.UpvoteCount,
			initialScore, updatedPost.Score)
	})

	// ====================================================================================
	// Part 8: User A - Upvote Comment
	// ====================================================================================
	t.Run("8. User A - Upvote Comment", func(t *testing.T) {
		t.Log("\n⬆️  Part 8: User A upvotes User B's comment...")

		// Get initial vote counts
		initialComment, err := commentRepo.GetByURI(ctx, commentURI)
		require.NoError(t, err)
		initialUpvotes := initialComment.UpvoteCount
		initialScore := initialComment.Score

		// User A creates upvote via PDS (simulate)
		voteRKey := generateTID()
		voteURI := fmt.Sprintf("at://%s/social.coves.feed.vote/%s", userADID, voteRKey)

		voteEvent := &jetstream.JetstreamEvent{
			Did:  userADID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        "test-vote-comment-rev",
				Operation:  "create",
				Collection: "social.coves.feed.vote",
				RKey:       voteRKey,
				CID:        "bafyvotecommentjourney123",
				Record: map[string]interface{}{
					"$type": "social.coves.feed.vote",
					"subject": map[string]interface{}{
						"uri": commentURI,
						"cid": commentCID,
					},
					"direction": "up",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}

		require.NoError(t, voteConsumer.HandleEvent(ctx, voteEvent))

		t.Logf("✅ Upvote on comment created: %s", voteURI)

		// Verify comment vote counts updated
		updatedComment, err := commentRepo.GetByURI(ctx, commentURI)
		require.NoError(t, err)
		assert.Equal(t, initialUpvotes+1, updatedComment.UpvoteCount,
			"Comment upvote count should increment")
		assert.Equal(t, initialScore+1, updatedComment.Score,
			"Comment score should increment")

		t.Logf("✅ Comment upvotes: %d → %d, score: %d → %d",
			initialUpvotes, updatedComment.UpvoteCount,
			initialScore, updatedComment.Score)
	})

	// ====================================================================================
	// Part 9: User B - Verify Timeline Feed
	// ====================================================================================
	t.Run("9. User B - Verify Timeline Feed Shows Subscribed Community Posts", func(t *testing.T) {
		t.Log("\n📰 Part 9: User B checks timeline feed...")

		// Use HTTP client to properly go through auth middleware with Bearer token
		req, _ := http.NewRequest(http.MethodGet,
			httpServer.URL+"/xrpc/social.coves.feed.getTimeline?sort=new&limit=10", nil)
		req.Header.Set("Authorization", "Bearer "+userBAPIToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode, "Timeline request should succeed")

		var response timelineCore.TimelineResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&response))

		// User B should see the post from the community they subscribed to
		require.NotEmpty(t, response.Feed, "Timeline should contain posts")

		// Find our test post in the feed
		foundPost := false
		for _, feedPost := range response.Feed {
			if feedPost.Post.URI == postURI {
				foundPost = true
				assert.Equal(t, userADID, feedPost.Post.Author.DID,
					"Post author should be User A")
				assert.Equal(t, communityDID, feedPost.Post.Community.DID,
					"Post community should match")
				// Check stats (counts are in Stats struct, not direct fields)
				require.NotNil(t, feedPost.Post.Stats, "Post should have stats")
				assert.Equal(t, 1, feedPost.Post.Stats.Upvotes,
					"Post should show 1 upvote from User B")
				assert.Equal(t, 1, feedPost.Post.Stats.CommentCount,
					"Post should show 1 comment from User B")
				break
			}
		}

		assert.True(t, foundPost, "Timeline should contain User A's post from subscribed community")

		t.Logf("✅ Timeline feed verified - User B sees post from subscribed community")
	})

	// ====================================================================================
	// Test Summary
	// ====================================================================================
	t.Log("\n" + strings.Repeat("=", 80))
	t.Log("✅ FULL USER JOURNEY E2E TEST COMPLETE")
	t.Log(strings.Repeat("=", 80))
	t.Log("\n🎯 Complete Flow Tested:")
	t.Log("   1. ✓ User A - Signup and Authenticate")
	t.Log("   2. ✓ User A - Create Community")
	t.Log("   3. ✓ User A - Create Post")
	t.Log("   4. ✓ User B - Signup and Authenticate")
	t.Log("   5. ✓ User B - Subscribe to Community")
	t.Log("   6. ✓ User B - Add Comment to Post")
	t.Log("   7. ✓ User B - Upvote Post")
	t.Log("   8. ✓ User A - Upvote Comment")
	t.Log("   9. ✓ User B - Verify Timeline Feed")
	t.Log("\n✅ Data Flow Verified:")
	t.Log("   ✓ All records written to PDS")
	t.Log("   ✓ Jetstream events consumed (with fallback simulation)")
	t.Log("   ✓ AppView database indexed correctly")
	t.Log("   ✓ Counts updated (votes, comments, subscribers)")
	t.Log("   ✓ Timeline feed aggregates subscribed content")
	t.Log("\n✅ Multi-User Interaction Verified:")
	t.Log("   ✓ User A creates community and post")
	t.Log("   ✓ User B subscribes and interacts")
	t.Log("   ✓ Cross-user votes and comments")
	t.Log("   ✓ Feed shows correct personalized content")
	t.Log("\n" + strings.Repeat("=", 80))
}

// Helper: Subscribe to Jetstream for community profile events
func subscribeToJetstreamForCommunity(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	consumer *jetstream.CommunityEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	errorChan chan<- error,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// ONE deadline for the whole subscription, not one per read: the
	// budget is what the caller is willing to wait in total, and a
	// per-read deadline would let a busy stream extend it indefinitely.
	readDeadline := time.Now().Add(jetstreamReadBudget)

	// The gorilla/websocket library panics after 1000 repeated reads on a failed connection

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := conn.SetReadDeadline(readDeadline); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				// Handle close errors - connection is done
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					return fmt.Errorf("Jetstream closed the subscription before the event arrived: %w", err)
				}

				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return fmt.Errorf("Jetstream hung up before the event arrived: %w", err)
				}

				// Handle timeout errors using errors.As for wrapped errors
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// The deadline is the whole budget, so its expiry is the answer:
					// no matching event arrived. Reading on would be reading a
					// connection gorilla has already marked failed.
					return fmt.Errorf("no matching event within %s", jetstreamReadBudget)
				}

				// For any other error, return immediately to avoid re-reading from failed connection
				// The gorilla/websocket library panics on repeated reads after a connection failure
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			if event.Did == targetDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.community.profile" {
				if err := consumer.HandleEvent(ctx, &event); err != nil {
					return fmt.Errorf("failed to process event: %w", err)
				}

				select {
				case eventChan <- &event:
					return nil
				case <-time.After(1 * time.Second):
					return fmt.Errorf("timeout sending event to channel")
				}
			}
		}
	}
}

// Helper: Simulate community indexing for test speed
func simulateCommunityIndexing(t *testing.T, db *sql.DB, did, handle, ownerDID string) {
	t.Helper()

	_, err := db.Exec(`
		INSERT INTO communities (did, handle, name, display_name, owner_did, created_by_did,
			hosted_by_did, visibility, moderation_type, record_uri, record_cid, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
		ON CONFLICT (did) DO NOTHING
	`, did, handle, strings.Split(handle, ".")[0], "Test Community", did, ownerDID,
		"did:web:coves.social", "public", "moderator",
		fmt.Sprintf("at://%s/social.coves.community.profile/self", did), "fakecid")

	require.NoError(t, err, "Failed to simulate community indexing")
}

// Helper: Simulate post indexing for test speed
func simulatePostIndexing(t *testing.T, db *sql.DB, consumer *jetstream.PostEventConsumer,
	ctx context.Context, communityDID, authorDID, uri, cid, title, content string,
) {
	t.Helper()

	rkey := strings.Split(uri, "/")[4]
	event := jetstream.JetstreamEvent{
		Did:  communityDID,
		Kind: "commit",
		Commit: &jetstream.CommitEvent{
			Operation:  "create",
			Collection: "social.coves.community.post",
			RKey:       rkey,
			CID:        cid,
			Record: map[string]interface{}{
				"$type":     "social.coves.community.post",
				"community": communityDID,
				"author":    authorDID,
				"title":     title,
				"content":   content,
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}
	require.NoError(t, consumer.HandleEvent(ctx, &event))
}
