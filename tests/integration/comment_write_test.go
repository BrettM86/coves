package integration

import (
	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/pds"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/comments"
	"Coves/internal/db/postgres"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// TestCommentWrite_CreateTopLevelComment tests creating a comment on a post via E2E flow
func TestCommentWrite_CreateTopLevelComment(t *testing.T) {
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
	pdsURL := getTestPDSURL()
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
	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Setup service with password-based PDS client factory for E2E testing
	// CommentPDSClientFactory creates a PDS client for comment operations
	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
		}
		if session.HostURL == "" {
			return nil, fmt.Errorf("session has no host URL")
		}

		return pds.NewFromAccessToken(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}

	commentService := comments.NewCommentServiceWithPDSFactory(
		commentRepo,
		nil, // userRepo not needed for write ops
		postRepo,
		nil, // communityRepo not needed for write ops
		nil, // logger
		commentPDSFactory,
	)

	// Create test user on PDS
	testUserHandle := fmt.Sprintf("cmw%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("commenter-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	t.Logf("Creating test user on PDS: %s", testUserHandle)
	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}
	t.Logf("Test user created: DID=%s", userDID)

	// Index user in AppView
	testUser := createTestUser(t, db, testUserHandle, userDID)

	// Create test community and post to comment on
	testCommunityDID, err := createFeedTestCommunity(db, ctx, "test-community", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post", 0, time.Now())
	postCID := "bafypost123"

	// Create mock OAuth session for service layer
	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	// ====================================================================================
	// TEST: Create top-level comment on post
	// ====================================================================================
	t.Logf("\n📝 Creating top-level comment via service...")

	commentReq := comments.CreateCommentRequest{
		Reply: comments.ReplyRef{
			Root: comments.StrongRef{
				URI: postURI,
				CID: postCID,
			},
			Parent: comments.StrongRef{
				URI: postURI,
				CID: postCID,
			},
		},
		Content: "This is a test comment on the post",
		Langs:   []string{"en"},
	}

	// Get session from store
	parsedDID, _ := parseTestDID(userDID)
	session, err := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}

	commentResp, err := commentService.CreateComment(ctx, session, commentReq)
	if err != nil {
		t.Fatalf("Failed to create comment: %v", err)
	}

	t.Logf("✅ Comment created:")
	t.Logf("   URI: %s", commentResp.URI)
	t.Logf("   CID: %s", commentResp.CID)

	// Verify comment record was written to PDS
	t.Logf("\n🔍 Verifying comment record on PDS...")
	rkey := utils.ExtractRKeyFromURI(commentResp.URI)
	collection := "social.coves.community.comment"

	pdsResp, pdsErr := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=%s&rkey=%s",
		pdsURL, userDID, collection, rkey))
	if pdsErr != nil {
		t.Fatalf("Failed to fetch comment record from PDS: %v", pdsErr)
	}
	defer func() {
		if closeErr := pdsResp.Body.Close(); closeErr != nil {
			t.Logf("Failed to close PDS response: %v", closeErr)
		}
	}()

	if pdsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(pdsResp.Body)
		t.Fatalf("Comment record not found on PDS: status %d, body: %s", pdsResp.StatusCode, string(body))
	}

	var pdsRecord struct {
		Value map[string]interface{} `json:"value"`
		CID   string                 `json:"cid"`
	}
	if decodeErr := json.NewDecoder(pdsResp.Body).Decode(&pdsRecord); decodeErr != nil {
		t.Fatalf("Failed to decode PDS record: %v", decodeErr)
	}

	t.Logf("✅ Comment record found on PDS:")
	t.Logf("   CID: %s", pdsRecord.CID)
	t.Logf("   Content: %v", pdsRecord.Value["content"])

	// Verify content
	if pdsRecord.Value["content"] != "This is a test comment on the post" {
		t.Errorf("Expected content 'This is a test comment on the post', got %v", pdsRecord.Value["content"])
	}

	// Simulate Jetstream consumer indexing the comment
	t.Logf("\n🔄 Simulating Jetstream consumer indexing comment...")
	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	commentEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-comment-rev",
			Operation:  "create",
			Collection: "social.coves.community.comment",
			RKey:       rkey,
			CID:        pdsRecord.CID,
			Record: map[string]interface{}{
				"$type": "social.coves.community.comment",
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
				"content":   "This is a test comment on the post",
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}

	if handleErr := commentConsumer.HandleEvent(ctx, &commentEvent); handleErr != nil {
		t.Fatalf("Failed to handle comment event: %v", handleErr)
	}

	// Verify comment was indexed in AppView
	t.Logf("\n🔍 Verifying comment indexed in AppView...")
	indexedComment, err := commentRepo.GetByURI(ctx, commentResp.URI)
	if err != nil {
		t.Fatalf("Comment not indexed in AppView: %v", err)
	}

	t.Logf("✅ Comment indexed in AppView:")
	t.Logf("   CommenterDID: %s", indexedComment.CommenterDID)
	t.Logf("   Content:      %s", indexedComment.Content)
	t.Logf("   RootURI:      %s", indexedComment.RootURI)
	t.Logf("   ParentURI:    %s", indexedComment.ParentURI)

	// Verify comment details
	if indexedComment.CommenterDID != userDID {
		t.Errorf("Expected commenter_did %s, got %s", userDID, indexedComment.CommenterDID)
	}
	if indexedComment.RootURI != postURI {
		t.Errorf("Expected root_uri %s, got %s", postURI, indexedComment.RootURI)
	}
	if indexedComment.ParentURI != postURI {
		t.Errorf("Expected parent_uri %s, got %s", postURI, indexedComment.ParentURI)
	}
	if indexedComment.Content != "This is a test comment on the post" {
		t.Errorf("Expected content 'This is a test comment on the post', got %s", indexedComment.Content)
	}

	// Verify post comment count updated
	t.Logf("\n🔍 Verifying post comment count updated...")
	updatedPost, err := postRepo.GetByURI(ctx, postURI)
	if err != nil {
		t.Fatalf("Failed to get updated post: %v", err)
	}

	if updatedPost.CommentCount != 1 {
		t.Errorf("Expected comment_count = 1, got %d", updatedPost.CommentCount)
	}

	t.Logf("✅ TRUE E2E COMMENT CREATE FLOW COMPLETE:")
	t.Logf("   Client → Service → PDS Write → Jetstream → Consumer → AppView ✓")
	t.Logf("   ✓ Comment written to PDS")
	t.Logf("   ✓ Comment indexed in AppView")
	t.Logf("   ✓ Post comment count updated")
}

// TestCommentWrite_CreateNestedReply tests creating a reply to another comment
func TestCommentWrite_CreateNestedReply(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories and service
	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// CommentPDSClientFactory creates a PDS client for comment operations
	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
		}
		if session.HostURL == "" {
			return nil, fmt.Errorf("session has no host URL")
		}

		return pds.NewFromAccessToken(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}

	commentService := comments.NewCommentServiceWithPDSFactory(
		commentRepo,
		nil,
		postRepo,
		nil,
		nil,
		commentPDSFactory,
	)

	// Create test user
	testUserHandle := fmt.Sprintf("rpl%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("replier-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	testUser := createTestUser(t, db, testUserHandle, userDID)

	// Create test post and parent comment
	testCommunityDID, _ := createFeedTestCommunity(db, ctx, "reply-community", "owner.test")
	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post", 0, time.Now())
	postCID := "bafypost456"

	// Create parent comment directly in DB (simulating already-indexed comment)
	parentCommentURI := fmt.Sprintf("at://%s/social.coves.community.comment/parent123", userDID)
	parentCommentCID := "bafyparent123"
	_, err = db.ExecContext(ctx, `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
	`, parentCommentURI, parentCommentCID, "parent123", userDID, postURI, postCID, postURI, postCID, "Parent comment")
	if err != nil {
		t.Fatalf("Failed to create parent comment: %v", err)
	}

	// Setup OAuth
	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	// Create nested reply
	t.Logf("\n📝 Creating nested reply...")
	replyReq := comments.CreateCommentRequest{
		Reply: comments.ReplyRef{
			Root: comments.StrongRef{
				URI: postURI,
				CID: postCID,
			},
			Parent: comments.StrongRef{
				URI: parentCommentURI,
				CID: parentCommentCID,
			},
		},
		Content: "This is a reply to the parent comment",
		Langs:   []string{"en"},
	}

	parsedDID, _ := parseTestDID(userDID)
	session, _ := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)

	replyResp, err := commentService.CreateComment(ctx, session, replyReq)
	if err != nil {
		t.Fatalf("Failed to create reply: %v", err)
	}

	t.Logf("✅ Reply created: %s", replyResp.URI)

	// Simulate Jetstream indexing
	rkey := utils.ExtractRKeyFromURI(replyResp.URI)
	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

	replyEvent := jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-reply-rev",
			Operation:  "create",
			Collection: "social.coves.community.comment",
			RKey:       rkey,
			CID:        replyResp.CID,
			Record: map[string]interface{}{
				"$type": "social.coves.community.comment",
				"reply": map[string]interface{}{
					"root": map[string]interface{}{
						"uri": postURI,
						"cid": postCID,
					},
					"parent": map[string]interface{}{
						"uri": parentCommentURI,
						"cid": parentCommentCID,
					},
				},
				"content":   "This is a reply to the parent comment",
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}

	if handleErr := commentConsumer.HandleEvent(ctx, &replyEvent); handleErr != nil {
		t.Fatalf("Failed to handle reply event: %v", handleErr)
	}

	// Verify reply was indexed with correct parent
	indexedReply, err := commentRepo.GetByURI(ctx, replyResp.URI)
	if err != nil {
		t.Fatalf("Reply not indexed: %v", err)
	}

	if indexedReply.RootURI != postURI {
		t.Errorf("Expected root_uri %s, got %s", postURI, indexedReply.RootURI)
	}
	if indexedReply.ParentURI != parentCommentURI {
		t.Errorf("Expected parent_uri %s, got %s", parentCommentURI, indexedReply.ParentURI)
	}

	t.Logf("✅ NESTED REPLY FLOW COMPLETE:")
	t.Logf("   ✓ Reply created with correct parent reference")
	t.Logf("   ✓ Reply indexed in AppView")
}

// TestCommentWrite_UpdateComment tests updating an existing comment
func TestCommentWrite_UpdateComment(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories and service
	commentRepo := postgres.NewCommentRepository(db)

	// CommentPDSClientFactory creates a PDS client for comment operations
	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
		}
		if session.HostURL == "" {
			return nil, fmt.Errorf("session has no host URL")
		}

		return pds.NewFromAccessToken(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}

	commentService := comments.NewCommentServiceWithPDSFactory(
		commentRepo,
		nil,
		nil,
		nil,
		nil,
		commentPDSFactory,
	)

	// Create test user
	testUserHandle := fmt.Sprintf("upd%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("updater-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Setup OAuth
	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	parsedDID, _ := parseTestDID(userDID)
	session, _ := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)

	// First, create a comment to update
	t.Logf("\n📝 Creating initial comment...")
	createReq := comments.CreateCommentRequest{
		Reply: comments.ReplyRef{
			Root: comments.StrongRef{
				URI: "at://did:plc:test/social.coves.community.post/test123",
				CID: "bafypost",
			},
			Parent: comments.StrongRef{
				URI: "at://did:plc:test/social.coves.community.post/test123",
				CID: "bafypost",
			},
		},
		Content: "Original content",
		Langs:   []string{"en"},
	}

	createResp, err := commentService.CreateComment(ctx, session, createReq)
	if err != nil {
		t.Fatalf("Failed to create comment: %v", err)
	}

	t.Logf("✅ Initial comment created: %s", createResp.URI)

	// Now update the comment
	t.Logf("\n📝 Updating comment...")
	updateReq := comments.UpdateCommentRequest{
		URI:     createResp.URI,
		Content: "Updated content - this has been edited",
	}

	updateResp, err := commentService.UpdateComment(ctx, session, updateReq)
	if err != nil {
		t.Fatalf("Failed to update comment: %v", err)
	}

	t.Logf("✅ Comment updated:")
	t.Logf("   URI: %s", updateResp.URI)
	t.Logf("   New CID: %s", updateResp.CID)

	// Verify the update on PDS
	rkey := utils.ExtractRKeyFromURI(updateResp.URI)
	pdsResp, err := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=social.coves.community.comment&rkey=%s",
		pdsURL, userDID, rkey))
	if err != nil {
		t.Fatalf("Failed to get record from PDS: %v", err)
	}
	defer pdsResp.Body.Close()

	var pdsRecord struct {
		Value map[string]interface{} `json:"value"`
		CID   string                 `json:"cid"`
	}
	if err := json.NewDecoder(pdsResp.Body).Decode(&pdsRecord); err != nil {
		t.Fatalf("Failed to decode PDS response: %v", err)
	}

	if pdsRecord.Value["content"] != "Updated content - this has been edited" {
		t.Errorf("Expected updated content, got %v", pdsRecord.Value["content"])
	}

	t.Logf("✅ UPDATE FLOW COMPLETE:")
	t.Logf("   ✓ Comment updated on PDS")
	t.Logf("   ✓ New CID generated")
	t.Logf("   ✓ Content verified")
}

// TestCommentWrite_DeleteComment tests deleting a comment
func TestCommentWrite_DeleteComment(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories and service
	commentRepo := postgres.NewCommentRepository(db)

	// CommentPDSClientFactory creates a PDS client for comment operations
	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
		}
		if session.HostURL == "" {
			return nil, fmt.Errorf("session has no host URL")
		}

		return pds.NewFromAccessToken(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}

	commentService := comments.NewCommentServiceWithPDSFactory(
		commentRepo,
		nil,
		nil,
		nil,
		nil,
		commentPDSFactory,
	)

	// Create test user
	testUserHandle := fmt.Sprintf("del%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("deleter-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Setup OAuth
	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	parsedDID, _ := parseTestDID(userDID)
	session, _ := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)

	// First, create a comment to delete
	t.Logf("\n📝 Creating comment to delete...")
	createReq := comments.CreateCommentRequest{
		Reply: comments.ReplyRef{
			Root: comments.StrongRef{
				URI: "at://did:plc:test/social.coves.community.post/test123",
				CID: "bafypost",
			},
			Parent: comments.StrongRef{
				URI: "at://did:plc:test/social.coves.community.post/test123",
				CID: "bafypost",
			},
		},
		Content: "This comment will be deleted",
		Langs:   []string{"en"},
	}

	createResp, err := commentService.CreateComment(ctx, session, createReq)
	if err != nil {
		t.Fatalf("Failed to create comment: %v", err)
	}

	t.Logf("✅ Comment created: %s", createResp.URI)

	// Now delete the comment
	t.Logf("\n📝 Deleting comment...")
	deleteReq := comments.DeleteCommentRequest{
		URI: createResp.URI,
	}

	err = commentService.DeleteComment(ctx, session, deleteReq)
	if err != nil {
		t.Fatalf("Failed to delete comment: %v", err)
	}

	t.Logf("✅ Comment deleted")

	// Verify deletion on PDS
	rkey := utils.ExtractRKeyFromURI(createResp.URI)
	pdsResp, err := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=social.coves.community.comment&rkey=%s",
		pdsURL, userDID, rkey))
	if err != nil {
		t.Fatalf("Failed to get record from PDS: %v", err)
	}
	defer pdsResp.Body.Close()

	if pdsResp.StatusCode != http.StatusBadRequest && pdsResp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected 400 or 404 for deleted comment, got %d", pdsResp.StatusCode)
	}

	t.Logf("✅ DELETE FLOW COMPLETE:")
	t.Logf("   ✓ Comment deleted from PDS")
	t.Logf("   ✓ Record no longer accessible")
}

// TestCommentWrite_CannotUpdateOthersComment tests authorization for updates
func TestCommentWrite_CannotUpdateOthersComment(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// CommentPDSClientFactory creates a PDS client for comment operations
	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
		}
		if session.HostURL == "" {
			return nil, fmt.Errorf("session has no host URL")
		}

		return pds.NewFromAccessToken(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}

	// Setup service
	commentService := comments.NewCommentServiceWithPDSFactory(
		nil,
		nil,
		nil,
		nil,
		nil,
		commentPDSFactory,
	)

	// Create first user (comment owner)
	ownerHandle := fmt.Sprintf("own%d.local.coves.dev", time.Now().UnixNano()%1000000)
	ownerEmail := fmt.Sprintf("owner-%d@test.local", time.Now().Unix())
	_, ownerDID, err := createPDSAccount(pdsURL, ownerHandle, ownerEmail, "password123")
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Create second user (attacker)
	attackerHandle := fmt.Sprintf("atk%d.local.coves.dev", time.Now().UnixNano()%1000000)
	attackerEmail := fmt.Sprintf("attacker-%d@test.local", time.Now().Unix())
	attackerToken, attackerDID, err := createPDSAccount(pdsURL, attackerHandle, attackerEmail, "password123")
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Setup OAuth for attacker
	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(attackerDID, "session-"+attackerDID, attackerToken, pdsURL)

	parsedDID, _ := parseTestDID(attackerDID)
	session, _ := mockStore.GetSession(ctx, parsedDID, "session-"+attackerDID)

	// Try to update comment owned by different user
	t.Logf("\n🚨 Attempting to update another user's comment...")
	updateReq := comments.UpdateCommentRequest{
		URI:     fmt.Sprintf("at://%s/social.coves.community.comment/test123", ownerDID),
		Content: "Malicious update attempt",
	}

	_, err = commentService.UpdateComment(ctx, session, updateReq)

	// Verify authorization error
	if err == nil {
		t.Fatal("Expected authorization error, got nil")
	}
	if !errors.Is(err, comments.ErrNotAuthorized) {
		t.Errorf("Expected ErrNotAuthorized, got: %v", err)
	}

	t.Logf("✅ AUTHORIZATION CHECK PASSED:")
	t.Logf("   ✓ User cannot update others' comments")
}

// TestCommentWrite_CannotDeleteOthersComment tests authorization for deletes
func TestCommentWrite_CannotDeleteOthersComment(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// CommentPDSClientFactory creates a PDS client for comment operations
	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
		}
		if session.HostURL == "" {
			return nil, fmt.Errorf("session has no host URL")
		}

		return pds.NewFromAccessToken(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}

	// Setup service
	commentService := comments.NewCommentServiceWithPDSFactory(
		nil,
		nil,
		nil,
		nil,
		nil,
		commentPDSFactory,
	)

	// Create first user (comment owner)
	ownerHandle := fmt.Sprintf("own%d.local.coves.dev", time.Now().UnixNano()%1000000)
	ownerEmail := fmt.Sprintf("owner-%d@test.local", time.Now().Unix())
	_, ownerDID, err := createPDSAccount(pdsURL, ownerHandle, ownerEmail, "password123")
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Create second user (attacker)
	attackerHandle := fmt.Sprintf("atk%d.local.coves.dev", time.Now().UnixNano()%1000000)
	attackerEmail := fmt.Sprintf("attacker-%d@test.local", time.Now().Unix())
	attackerToken, attackerDID, err := createPDSAccount(pdsURL, attackerHandle, attackerEmail, "password123")
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Setup OAuth for attacker
	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(attackerDID, "session-"+attackerDID, attackerToken, pdsURL)

	parsedDID, _ := parseTestDID(attackerDID)
	session, _ := mockStore.GetSession(ctx, parsedDID, "session-"+attackerDID)

	// Try to delete comment owned by different user
	t.Logf("\n🚨 Attempting to delete another user's comment...")
	deleteReq := comments.DeleteCommentRequest{
		URI: fmt.Sprintf("at://%s/social.coves.community.comment/test123", ownerDID),
	}

	err = commentService.DeleteComment(ctx, session, deleteReq)

	// Verify authorization error
	if err == nil {
		t.Fatal("Expected authorization error, got nil")
	}
	if !errors.Is(err, comments.ErrNotAuthorized) {
		t.Errorf("Expected ErrNotAuthorized, got: %v", err)
	}

	t.Logf("✅ AUTHORIZATION CHECK PASSED:")
	t.Logf("   ✓ User cannot delete others' comments")
}

// Helper function to parse DID for testing
func parseTestDID(did string) (syntax.DID, error) {
	return syntax.ParseDID(did)
}

// TestCommentWrite_ConcurrentModificationDetection tests that PutRecord's swapRecord
// CID validation correctly detects concurrent modifications.
// This verifies the optimistic locking mechanism that prevents lost updates.
func TestCommentWrite_ConcurrentModificationDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Setup repositories and service
	commentRepo := postgres.NewCommentRepository(db)

	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
		}
		if session.HostURL == "" {
			return nil, fmt.Errorf("session has no host URL")
		}
		return pds.NewFromAccessToken(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}

	commentService := comments.NewCommentServiceWithPDSFactory(
		commentRepo,
		nil,
		nil,
		nil,
		nil,
		commentPDSFactory,
	)

	// Create test user
	testUserHandle := fmt.Sprintf("cnc%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("concurrency-%d@test.local", time.Now().Unix())
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Skipf("PDS not available: %v", err)
	}

	// Setup OAuth
	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	parsedDID, _ := parseTestDID(userDID)
	session, _ := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)

	// Step 1: Create a comment
	t.Logf("\n📝 Step 1: Creating initial comment...")
	createReq := comments.CreateCommentRequest{
		Reply: comments.ReplyRef{
			Root: comments.StrongRef{
				URI: "at://did:plc:test/social.coves.community.post/test123",
				CID: "bafypost",
			},
			Parent: comments.StrongRef{
				URI: "at://did:plc:test/social.coves.community.post/test123",
				CID: "bafypost",
			},
		},
		Content: "Original content for concurrency test",
		Langs:   []string{"en"},
	}

	createResp, err := commentService.CreateComment(ctx, session, createReq)
	if err != nil {
		t.Fatalf("Failed to create comment: %v", err)
	}
	t.Logf("✅ Comment created: URI=%s, CID=%s", createResp.URI, createResp.CID)
	originalCID := createResp.CID

	// Step 2: Update the comment (this changes the CID)
	t.Logf("\n📝 Step 2: Updating comment (this changes CID)...")
	updateReq := comments.UpdateCommentRequest{
		URI:     createResp.URI,
		Content: "Updated content - CID has changed",
	}

	updateResp, err := commentService.UpdateComment(ctx, session, updateReq)
	if err != nil {
		t.Fatalf("Failed to update comment: %v", err)
	}
	t.Logf("✅ Comment updated: New CID=%s", updateResp.CID)
	newCID := updateResp.CID

	// Verify CIDs are different
	if originalCID == newCID {
		t.Fatalf("CIDs should be different after update: original=%s, new=%s", originalCID, newCID)
	}

	// Step 3: Simulate concurrent modification detection using direct PDS client
	// Create a PDS client and attempt to update with the stale (original) CID
	t.Logf("\n🔍 Step 3: Testing concurrent modification detection with stale CID...")

	pdsClient, err := pds.NewFromAccessToken(pdsURL, userDID, pdsAccessToken)
	if err != nil {
		t.Fatalf("Failed to create PDS client: %v", err)
	}

	rkey := utils.ExtractRKeyFromURI(createResp.URI)

	// Try to update with the ORIGINAL (now stale) CID - this should fail with 409
	staleRecord := map[string]interface{}{
		"$type": "social.coves.community.comment",
		"reply": map[string]interface{}{
			"root": map[string]interface{}{
				"uri": "at://did:plc:test/social.coves.community.post/test123",
				"cid": "bafypost",
			},
			"parent": map[string]interface{}{
				"uri": "at://did:plc:test/social.coves.community.post/test123",
				"cid": "bafypost",
			},
		},
		"content":   "This update should fail - using stale CID",
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}

	_, _, err = pdsClient.PutRecord(ctx, "social.coves.community.comment", rkey, staleRecord, originalCID)

	// Verify we get an error indicating CID mismatch
	// PDS returns 400 "bad request" with message "Record was at <cid>" when swap CID doesn't match
	if err == nil {
		t.Fatal("Expected error when updating with stale CID, got nil")
	}

	// Check for either ErrConflict (409) or CID mismatch error (400)
	errMsg := err.Error()
	isCIDMismatch := strings.Contains(errMsg, "Record was at") || errors.Is(err, pds.ErrConflict)
	if !isCIDMismatch {
		t.Errorf("Expected CID mismatch or ErrConflict, got: %v", err)
	}

	t.Logf("✅ Correctly detected concurrent modification!")
	t.Logf("   Error: %v", err)

	// Step 4: Verify that updating with the correct CID succeeds
	t.Logf("\n📝 Step 4: Verifying update with correct CID succeeds...")
	correctRecord := map[string]interface{}{
		"$type": "social.coves.community.comment",
		"reply": map[string]interface{}{
			"root": map[string]interface{}{
				"uri": "at://did:plc:test/social.coves.community.post/test123",
				"cid": "bafypost",
			},
			"parent": map[string]interface{}{
				"uri": "at://did:plc:test/social.coves.community.post/test123",
				"cid": "bafypost",
			},
		},
		"content":   "This update should succeed - using correct CID",
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}

	_, finalCID, err := pdsClient.PutRecord(ctx, "social.coves.community.comment", rkey, correctRecord, newCID)
	if err != nil {
		t.Fatalf("Update with correct CID should succeed, got: %v", err)
	}

	t.Logf("✅ Update with correct CID succeeded: New CID=%s", finalCID)

	t.Logf("\n✅ CONCURRENT MODIFICATION DETECTION TEST COMPLETE:")
	t.Logf("   ✓ PutRecord with stale CID correctly returns ErrConflict")
	t.Logf("   ✓ PutRecord with correct CID succeeds")
	t.Logf("   ✓ Optimistic locking prevents lost updates")
}
