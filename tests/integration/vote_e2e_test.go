package integration

import (
	"Coves/internal/api/routes"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// TestVoteE2E_CreateUpvote tests the full vote creation flow with a real local PDS
// Flow: Client → XRPC → PDS Write → Jetstream → Consumer → AppView
func TestVoteE2E_CreateUpvote(t *testing.T) {
	// Skip in short mode since this requires real PDS
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Setup test database
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Logf("Failed to close database: %v", closeErr)
		}
	}()

	// Run migrations
	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	// Check if PDS is running
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	func() {
		if closeErr := healthResp.Body.Close(); closeErr != nil {
			t.Logf("Failed to close health response: %v", closeErr)
		}
	}()

	ctx := context.Background()

	// Setup repositories
	voteRepo := postgres.NewVoteRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Setup services with password-based PDS client factory for E2E testing
	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user on PDS
	testUserHandle := fmt.Sprintf("voter-%d.local.coves.dev", time.Now().Unix())
	testUserEmail := fmt.Sprintf("voter-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	t.Logf("Creating test user on PDS: %s", testUserHandle)
	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}
	t.Logf("Test user created: DID=%s", userDID)

	// Index user in AppView
	testUser := createTestUser(t, db, testUserHandle, userDID)

	// Create test post to vote on
	testCommunityDID, err := createFeedTestCommunity(db, ctx, "test-community", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post", 0, time.Now())
	postCID := "bafypost123"

	// Setup OAuth middleware with real PDS access token
	e2eAuth := NewE2EOAuthMiddleware()
	token := e2eAuth.AddUserWithPDSToken(userDID, pdsAccessToken, pdsURL)

	// Setup HTTP server with XRPC routes
	r := chi.NewRouter()
	routes.RegisterVoteRoutes(r, voteService, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Setup Jetstream consumer
	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, nil, db)

	// ====================================================================================
	// TEST: Create upvote on post
	// ====================================================================================
	t.Logf("\n📝 Creating upvote via XRPC endpoint...")

	voteReq := map[string]interface{}{
		"subject": map[string]interface{}{
			"uri": postURI,
			"cid": postCID,
		},
		"direction": "up",
	}

	reqBody, marshalErr := json.Marshal(voteReq)
	if marshalErr != nil {
		t.Fatalf("Failed to marshal request: %v", marshalErr)
	}

	req, err := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.feed.vote.create",
		bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to POST vote: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatalf("Expected 200, got %d (failed to read body: %v)", resp.StatusCode, readErr)
		}
		t.Logf("XRPC Vote Failed")
		t.Logf("   Status: %d", resp.StatusCode)
		t.Logf("   Response: %s", string(body))
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	var voteResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}

	if decodeErr := json.NewDecoder(resp.Body).Decode(&voteResp); decodeErr != nil {
		t.Fatalf("Failed to decode vote response: %v", decodeErr)
	}

	t.Logf("✅ XRPC response received:")
	t.Logf("   URI: %s", voteResp.URI)
	t.Logf("   CID: %s", voteResp.CID)

	// Verify vote record was written to PDS
	t.Logf("\n🔍 Verifying vote record on PDS...")
	rkey := utils.ExtractRKeyFromURI(voteResp.URI)
	collection := "social.coves.feed.vote"

	pdsResp, pdsErr := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=%s&rkey=%s",
		pdsURL, userDID, collection, rkey))
	if pdsErr != nil {
		t.Fatalf("Failed to fetch vote record from PDS: %v", pdsErr)
	}
	defer func() {
		if closeErr := pdsResp.Body.Close(); closeErr != nil {
			t.Logf("Failed to close PDS response: %v", closeErr)
		}
	}()

	if pdsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(pdsResp.Body)
		t.Fatalf("Vote record not found on PDS: status %d, body: %s", pdsResp.StatusCode, string(body))
	}

	var pdsRecord struct {
		Value map[string]interface{} `json:"value"`
		CID   string                 `json:"cid"`
	}
	if decodeErr := json.NewDecoder(pdsResp.Body).Decode(&pdsRecord); decodeErr != nil {
		t.Fatalf("Failed to decode PDS record: %v", decodeErr)
	}

	t.Logf("✅ Vote record found on PDS:")
	t.Logf("   CID: %s", pdsRecord.CID)
	t.Logf("   Direction: %v", pdsRecord.Value["direction"])

	// Verify direction
	if pdsRecord.Value["direction"] != "up" {
		t.Errorf("Expected direction 'up', got %v", pdsRecord.Value["direction"])
	}

	// Simulate Jetstream consumer indexing the vote
	t.Logf("\n🔄 Simulating Jetstream consumer indexing vote...")
	voteEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-vote-rev",
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       rkey,
			CID:        pdsRecord.CID,
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

	if handleErr := voteConsumer.HandleEvent(ctx, &voteEvent); handleErr != nil {
		t.Fatalf("Failed to handle vote event: %v", handleErr)
	}

	// Verify vote was indexed in AppView
	t.Logf("\n🔍 Verifying vote indexed in AppView...")
	indexedVote, err := voteRepo.GetByURI(ctx, voteResp.URI)
	if err != nil {
		t.Fatalf("Vote not indexed in AppView: %v", err)
	}

	t.Logf("✅ Vote indexed in AppView:")
	t.Logf("   VoterDID:    %s", indexedVote.VoterDID)
	t.Logf("   SubjectURI:  %s", indexedVote.SubjectURI)
	t.Logf("   Direction:   %s", indexedVote.Direction)
	t.Logf("   URI:         %s", indexedVote.URI)

	// Verify vote details
	if indexedVote.VoterDID != userDID {
		t.Errorf("Expected voter_did %s, got %s", userDID, indexedVote.VoterDID)
	}
	if indexedVote.SubjectURI != postURI {
		t.Errorf("Expected subject_uri %s, got %s", postURI, indexedVote.SubjectURI)
	}
	if indexedVote.Direction != "up" {
		t.Errorf("Expected direction 'up', got %s", indexedVote.Direction)
	}

	// Verify post counts updated
	t.Logf("\n🔍 Verifying post vote counts updated...")
	updatedPost, err := postRepo.GetByURI(ctx, postURI)
	if err != nil {
		t.Fatalf("Failed to get updated post: %v", err)
	}

	if updatedPost.UpvoteCount != 1 {
		t.Errorf("Expected upvote_count = 1, got %d", updatedPost.UpvoteCount)
	}
	if updatedPost.Score != 1 {
		t.Errorf("Expected score = 1, got %d", updatedPost.Score)
	}

	t.Logf("✅ TRUE E2E UPVOTE FLOW COMPLETE:")
	t.Logf("   Client → XRPC → PDS Write → Jetstream → Consumer → AppView ✓")
	t.Logf("   ✓ Vote written to PDS")
	t.Logf("   ✓ Vote indexed in AppView")
	t.Logf("   ✓ Post vote counts updated")
}

// TestVoteE2E_ToggleSameDirection tests voting twice in same direction (toggle off)
func TestVoteE2E_ToggleSameDirection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories and services
	voteRepo := postgres.NewVoteRepository(db)
	postRepo := postgres.NewPostRepository(db)

	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user
	testUserHandle := fmt.Sprintf("toggle-%d.local.coves.dev", time.Now().Unix())
	testUserEmail := fmt.Sprintf("toggle-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	testUser := createTestUser(t, db, testUserHandle, userDID)

	// Create test post
	testCommunityDID, _ := createFeedTestCommunity(db, ctx, "toggle-community", "owner.test")
	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post", 0, time.Now())
	postCID := "bafypost456"

	// Setup OAuth and HTTP server with real PDS access token
	e2eAuth := NewE2EOAuthMiddleware()
	token := e2eAuth.AddUserWithPDSToken(userDID, pdsAccessToken, pdsURL)

	r := chi.NewRouter()
	routes.RegisterVoteRoutes(r, voteService, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, nil, db)

	// First upvote
	t.Logf("\n📝 Creating first upvote...")
	voteReq := map[string]interface{}{
		"subject": map[string]interface{}{
			"uri": postURI,
			"cid": postCID,
		},
		"direction": "up",
	}

	reqBody, _ := json.Marshal(voteReq)
	req, _ := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.feed.vote.create",
		bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to create first vote: %v", err)
	}

	var firstVoteResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&firstVoteResp); decodeErr != nil {
		t.Fatalf("Failed to decode first vote response: %v", decodeErr)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Logf("Failed to close response body: %v", closeErr)
	}

	t.Logf("✅ First vote created: %s", firstVoteResp.URI)

	// Index first vote
	rkey := utils.ExtractRKeyFromURI(firstVoteResp.URI)
	voteEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-vote-rev-1",
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       rkey,
			CID:        firstVoteResp.CID,
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
	if handleErr := voteConsumer.HandleEvent(ctx, &voteEvent); handleErr != nil {
		t.Fatalf("Failed to handle first vote event: %v", handleErr)
	}

	// Second upvote (same direction) - should toggle off (delete)
	t.Logf("\n📝 Creating second upvote (toggle off)...")
	req2, _ := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.feed.vote.create",
		bytes.NewBuffer(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+token)

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Failed to toggle vote: %v", err)
	}
	defer func() {
		if closeErr := resp2.Body.Close(); closeErr != nil {
			t.Logf("Failed to close response body: %v", closeErr)
		}
	}()

	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("Expected 200, got %d: %s", resp2.StatusCode, string(body))
	}

	t.Logf("✅ Second vote request completed (toggle)")

	// Simulate Jetstream DELETE event
	t.Logf("\n🔄 Simulating Jetstream DELETE event...")
	deleteEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-vote-rev-2",
			Operation:  "delete",
			Collection: "social.coves.feed.vote",
			RKey:       rkey,
		},
	}
	if handleErr := voteConsumer.HandleEvent(ctx, &deleteEvent); handleErr != nil {
		t.Fatalf("Failed to handle delete event: %v", handleErr)
	}

	// Verify vote was removed from AppView
	t.Logf("\n🔍 Verifying vote removed from AppView...")
	_, err = voteRepo.GetByURI(ctx, firstVoteResp.URI)
	if err == nil {
		t.Error("Expected vote to be deleted, but it still exists")
	}

	// Verify post counts reset
	updatedPost, _ := postRepo.GetByURI(ctx, postURI)
	if updatedPost.UpvoteCount != 0 {
		t.Errorf("Expected upvote_count = 0 after toggle, got %d", updatedPost.UpvoteCount)
	}

	t.Logf("✅ TOGGLE SAME DIRECTION FLOW COMPLETE:")
	t.Logf("   ✓ First vote created and indexed")
	t.Logf("   ✓ Second vote toggled off (deleted)")
	t.Logf("   ✓ Post counts updated correctly")
}

// TestVoteE2E_ToggleDifferentDirection tests changing vote direction
func TestVoteE2E_ToggleDifferentDirection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories and services
	voteRepo := postgres.NewVoteRepository(db)
	postRepo := postgres.NewPostRepository(db)

	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user
	testUserHandle := fmt.Sprintf("flip-%d.local.coves.dev", time.Now().Unix())
	testUserEmail := fmt.Sprintf("flip-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	testUser := createTestUser(t, db, testUserHandle, userDID)

	// Create test post
	testCommunityDID, _ := createFeedTestCommunity(db, ctx, "flip-community", "owner.test")
	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post", 0, time.Now())
	postCID := "bafypost789"

	// Setup OAuth and HTTP server with real PDS access token
	e2eAuth := NewE2EOAuthMiddleware()
	token := e2eAuth.AddUserWithPDSToken(userDID, pdsAccessToken, pdsURL)

	r := chi.NewRouter()
	routes.RegisterVoteRoutes(r, voteService, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, nil, db)

	// Create upvote
	t.Logf("\n📝 Creating upvote...")
	upvoteReq := map[string]interface{}{
		"subject": map[string]interface{}{
			"uri": postURI,
			"cid": postCID,
		},
		"direction": "up",
	}

	reqBody, _ := json.Marshal(upvoteReq)
	req, _ := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.feed.vote.create",
		bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to create upvote: %v", err)
	}
	var upvoteResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&upvoteResp); decodeErr != nil {
		t.Fatalf("Failed to decode upvote response: %v", decodeErr)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Logf("Failed to close response body: %v", closeErr)
	}

	// Index upvote
	rkey := utils.ExtractRKeyFromURI(upvoteResp.URI)
	upvoteEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-vote-rev-up",
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       rkey,
			CID:        upvoteResp.CID,
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
	if handleErr := voteConsumer.HandleEvent(ctx, &upvoteEvent); handleErr != nil {
		t.Fatalf("Failed to handle upvote event: %v", handleErr)
	}

	t.Logf("✅ Upvote created and indexed")

	// Change to downvote
	t.Logf("\n📝 Changing to downvote...")
	downvoteReq := map[string]interface{}{
		"subject": map[string]interface{}{
			"uri": postURI,
			"cid": postCID,
		},
		"direction": "down",
	}

	reqBody2, _ := json.Marshal(downvoteReq)
	req2, _ := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.feed.vote.create",
		bytes.NewBuffer(reqBody2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+token)

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Failed to create downvote: %v", err)
	}
	var downvoteResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if decodeErr := json.NewDecoder(resp2.Body).Decode(&downvoteResp); decodeErr != nil {
		t.Fatalf("Failed to decode downvote response: %v", decodeErr)
	}
	if closeErr := resp2.Body.Close(); closeErr != nil {
		t.Logf("Failed to close response body: %v", closeErr)
	}

	// The service flow for direction change is:
	// 1. DELETE old vote on PDS
	// 2. CREATE new vote with NEW rkey on PDS
	// So we simulate DELETE + CREATE events (not UPDATE)

	// Simulate Jetstream DELETE event for old vote
	t.Logf("\n🔄 Simulating Jetstream DELETE event for old upvote...")
	deleteEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-vote-rev-delete",
			Operation:  "delete",
			Collection: "social.coves.feed.vote",
			RKey:       rkey, // Old upvote rkey
		},
	}
	if handleErr := voteConsumer.HandleEvent(ctx, &deleteEvent); handleErr != nil {
		t.Fatalf("Failed to handle delete event: %v", handleErr)
	}

	// Simulate Jetstream CREATE event for new downvote
	t.Logf("\n🔄 Simulating Jetstream CREATE event for new downvote...")
	newRkey := utils.ExtractRKeyFromURI(downvoteResp.URI)
	createEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-vote-rev-down",
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       newRkey, // NEW rkey from downvote response
			CID:        downvoteResp.CID,
			Record: map[string]interface{}{
				"$type": "social.coves.feed.vote",
				"subject": map[string]interface{}{
					"uri": postURI,
					"cid": postCID,
				},
				"direction": "down",
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}
	if handleErr := voteConsumer.HandleEvent(ctx, &createEvent); handleErr != nil {
		t.Fatalf("Failed to handle create event: %v", handleErr)
	}

	// Verify old upvote was deleted
	t.Logf("\n🔍 Verifying old upvote was deleted...")
	_, err = voteRepo.GetByURI(ctx, upvoteResp.URI)
	if err == nil {
		t.Error("Expected old upvote to be deleted, but it still exists")
	}

	// Verify new downvote was indexed
	t.Logf("\n🔍 Verifying new downvote indexed in AppView...")
	newVote, err := voteRepo.GetByURI(ctx, downvoteResp.URI)
	if err != nil {
		t.Fatalf("New downvote not found: %v", err)
	}

	if newVote.Direction != "down" {
		t.Errorf("Expected direction 'down', got %s", newVote.Direction)
	}

	// Verify post counts updated
	updatedPost, _ := postRepo.GetByURI(ctx, postURI)
	if updatedPost.UpvoteCount != 0 {
		t.Errorf("Expected upvote_count = 0, got %d", updatedPost.UpvoteCount)
	}
	if updatedPost.DownvoteCount != 1 {
		t.Errorf("Expected downvote_count = 1, got %d", updatedPost.DownvoteCount)
	}
	if updatedPost.Score != -1 {
		t.Errorf("Expected score = -1, got %d", updatedPost.Score)
	}

	t.Logf("✅ TOGGLE DIFFERENT DIRECTION FLOW COMPLETE:")
	t.Logf("   ✓ Upvote created (score: +1)")
	t.Logf("   ✓ Changed to downvote (score: -1)")
	t.Logf("   ✓ Post counts updated correctly")
}

// TestVoteE2E_DeleteVote tests explicit vote deletion
func TestVoteE2E_DeleteVote(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories and services
	voteRepo := postgres.NewVoteRepository(db)
	postRepo := postgres.NewPostRepository(db)

	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, PasswordAuthPDSClientFactory())

	// Create test user
	testUserHandle := fmt.Sprintf("delete-%d.local.coves.dev", time.Now().Unix())
	testUserEmail := fmt.Sprintf("delete-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	testUser := createTestUser(t, db, testUserHandle, userDID)

	// Create test post
	testCommunityDID, _ := createFeedTestCommunity(db, ctx, "delete-community", "owner.test")
	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post", 0, time.Now())
	postCID := "bafypost999"

	// Setup OAuth and HTTP server with real PDS access token
	e2eAuth := NewE2EOAuthMiddleware()
	token := e2eAuth.AddUserWithPDSToken(userDID, pdsAccessToken, pdsURL)

	r := chi.NewRouter()
	routes.RegisterVoteRoutes(r, voteService, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, nil, db)

	// Create vote first
	t.Logf("\n📝 Creating vote to delete...")
	voteReq := map[string]interface{}{
		"subject": map[string]interface{}{
			"uri": postURI,
			"cid": postCID,
		},
		"direction": "up",
	}

	reqBody, _ := json.Marshal(voteReq)
	req, _ := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.feed.vote.create",
		bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to create vote: %v", err)
	}
	var voteResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&voteResp); decodeErr != nil {
		t.Fatalf("Failed to decode vote response: %v", decodeErr)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Logf("Failed to close response body: %v", closeErr)
	}

	// Index vote
	rkey := utils.ExtractRKeyFromURI(voteResp.URI)
	voteEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-vote-create",
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       rkey,
			CID:        voteResp.CID,
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
	if handleErr := voteConsumer.HandleEvent(ctx, &voteEvent); handleErr != nil {
		t.Fatalf("Failed to handle vote event: %v", handleErr)
	}

	t.Logf("✅ Vote created and indexed")

	// Delete vote via XRPC
	t.Logf("\n📝 Deleting vote via XRPC...")
	deleteReq := map[string]interface{}{
		"subject": map[string]interface{}{
			"uri": postURI,
			"cid": postCID,
		},
	}

	deleteBody, _ := json.Marshal(deleteReq)
	deleteHttpReq, _ := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.feed.vote.delete",
		bytes.NewBuffer(deleteBody))
	deleteHttpReq.Header.Set("Content-Type", "application/json")
	deleteHttpReq.Header.Set("Authorization", "Bearer "+token)

	deleteResp, err := http.DefaultClient.Do(deleteHttpReq)
	if err != nil {
		t.Fatalf("Failed to delete vote: %v", err)
	}
	defer func() {
		if closeErr := deleteResp.Body.Close(); closeErr != nil {
			t.Logf("Failed to close response body: %v", closeErr)
		}
	}()

	if deleteResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("Delete failed: status %d, body: %s", deleteResp.StatusCode, string(body))
	}

	// Per lexicon, delete returns empty object {}
	var deleteRespBody map[string]interface{}
	if decodeErr := json.NewDecoder(deleteResp.Body).Decode(&deleteRespBody); decodeErr != nil {
		t.Fatalf("Failed to decode delete response: %v", decodeErr)
	}

	if len(deleteRespBody) != 0 {
		t.Errorf("Expected empty object per lexicon, got %v", deleteRespBody)
	}

	t.Logf("✅ Delete vote request succeeded")

	// Simulate Jetstream DELETE event
	t.Logf("\n🔄 Simulating Jetstream DELETE event...")
	deleteEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-vote-delete",
			Operation:  "delete",
			Collection: "social.coves.feed.vote",
			RKey:       rkey,
		},
	}
	if handleErr := voteConsumer.HandleEvent(ctx, &deleteEvent); handleErr != nil {
		t.Fatalf("Failed to handle delete event: %v", handleErr)
	}

	// Verify vote removed from AppView
	t.Logf("\n🔍 Verifying vote removed from AppView...")
	_, err = voteRepo.GetByURI(ctx, voteResp.URI)
	if err == nil {
		t.Error("Expected vote to be deleted, but it still exists")
	}

	// Verify post counts reset
	updatedPost, _ := postRepo.GetByURI(ctx, postURI)
	if updatedPost.UpvoteCount != 0 {
		t.Errorf("Expected upvote_count = 0 after delete, got %d", updatedPost.UpvoteCount)
	}
	if updatedPost.Score != 0 {
		t.Errorf("Expected score = 0 after delete, got %d", updatedPost.Score)
	}

	t.Logf("✅ EXPLICIT DELETE FLOW COMPLETE:")
	t.Logf("   ✓ Vote created and indexed")
	t.Logf("   ✓ Vote deleted via XRPC")
	t.Logf("   ✓ Vote removed from AppView")
	t.Logf("   ✓ Post counts updated correctly")
}

// TestVoteE2E_JetstreamIndexing tests real Jetstream firehose consumption
func TestVoteE2E_JetstreamIndexing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories
	voteRepo := postgres.NewVoteRepository(db)

	// Create test user on PDS
	testUserHandle := fmt.Sprintf("jetstream-%d.local.coves.dev", time.Now().Unix())
	testUserEmail := fmt.Sprintf("jetstream-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	accessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	testUser := createTestUser(t, db, testUserHandle, userDID)

	// Create test post
	testCommunityDID, _ := createFeedTestCommunity(db, ctx, "jetstream-community", "owner.test")
	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post", 0, time.Now())
	postCID := "bafypostjetstream"

	// Write vote directly to PDS
	t.Logf("\n📝 Writing vote to PDS...")
	voteRecord := map[string]interface{}{
		"$type": "social.coves.feed.vote",
		"subject": map[string]interface{}{
			"uri": postURI,
			"cid": postCID,
		},
		"direction": "up",
		"createdAt": time.Now().Format(time.RFC3339),
	}

	voteURI, voteCID, err := writePDSRecord(pdsURL, accessToken, userDID, "social.coves.feed.vote", "", voteRecord)
	if err != nil {
		t.Fatalf("Failed to write vote to PDS: %v", err)
	}

	t.Logf("✅ Vote written to PDS:")
	t.Logf("   URI: %s", voteURI)
	t.Logf("   CID: %s", voteCID)

	// Setup Jetstream consumer
	voteConsumer := jetstream.NewVoteEventConsumer(voteRepo, nil, db)

	// Subscribe to Jetstream
	t.Logf("\n🔄 Subscribing to real Jetstream firehose...")
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]

	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.feed.vote", pdsHostname)
	t.Logf("   Jetstream URL: %s", jetstreamURL)
	t.Logf("   Looking for vote DID: %s", userDID)

	// Channels for event communication
	eventChan := make(chan *jetstream.JetstreamEvent, 10)
	errorChan := make(chan error, 1)
	done := make(chan bool)

	// Start Jetstream consumer in background
	go func() {
		err := subscribeToJetstreamForVote(ctx, jetstreamURL, userDID, voteConsumer, eventChan, errorChan, done)
		if err != nil {
			errorChan <- err
		}
	}()

	// Wait for event or timeout
	t.Logf("⏳ Waiting for Jetstream event (max 30 seconds)...")

	select {
	case event := <-eventChan:
		t.Logf("✅ Received real Jetstream event!")
		t.Logf("   Event DID:    %s", event.Did)
		t.Logf("   Collection:   %s", event.Commit.Collection)
		t.Logf("   Operation:    %s", event.Commit.Operation)
		t.Logf("   RKey:         %s", event.Commit.RKey)

		// Verify it's our vote
		if event.Did != userDID {
			t.Errorf("Expected DID %s, got %s", userDID, event.Did)
		}

		// Verify indexed in AppView database
		t.Logf("\n🔍 Querying AppView database...")
		indexedVote, err := voteRepo.GetByURI(ctx, voteURI)
		if err != nil {
			t.Fatalf("Vote not indexed in AppView: %v", err)
		}

		t.Logf("✅ Vote indexed in AppView:")
		t.Logf("   VoterDID:    %s", indexedVote.VoterDID)
		t.Logf("   SubjectURI:  %s", indexedVote.SubjectURI)
		t.Logf("   Direction:   %s", indexedVote.Direction)
		t.Logf("   URI:         %s", indexedVote.URI)

		// Signal to stop Jetstream consumer
		close(done)

	case err := <-errorChan:
		t.Fatalf("Jetstream error: %v", err)

	case <-time.After(30 * time.Second):
		t.Fatalf("Timeout: No Jetstream event received within 30 seconds")
	}

	t.Logf("\n✅ TRUE E2E JETSTREAM FLOW COMPLETE:")
	t.Logf("   PDS → Jetstream → Consumer → AppView ✓")
}

// subscribeToJetstreamForVote subscribes to real Jetstream firehose for vote events
func subscribeToJetstreamForVote(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	consumer *jetstream.VoteEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	errorChan chan<- error,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Read messages until we find our event or receive done signal
	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Set read deadline to avoid blocking forever
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				// Check if it's a timeout (expected)
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					return nil
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue // Timeout is expected, keep listening
				}
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			// Check if this is the event we're looking for
			if event.Did == targetDID && event.Kind == "commit" && event.Commit.Collection == "social.coves.feed.vote" {
				// Process the event through the consumer
				if err := consumer.HandleEvent(ctx, &event); err != nil {
					return fmt.Errorf("failed to process event: %w", err)
				}

				// Send to channel so test can verify
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
