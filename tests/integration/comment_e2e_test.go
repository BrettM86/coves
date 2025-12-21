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
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

// TestCommentE2E_CreateWithJetstream tests the full comment creation flow with real Jetstream
// Flow: Client → Service → PDS Write → Jetstream Firehose → Consumer → AppView
func TestCommentE2E_CreateWithJetstream(t *testing.T) {
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
	defer func() { _ = db.Close() }()

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
	_ = healthResp.Body.Close()

	// Check if Jetstream is running
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.comment", pdsHostname)

	testConn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		t.Skipf("Jetstream not running at %s: %v. Run 'docker-compose --profile jetstream up' to start.", jetstreamURL, err)
	}
	_ = testConn.Close()

	ctx := context.Background()

	// Setup repositories
	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Create test user on PDS
	// Use shorter handle to avoid PDS length limits (max 20 chars for label)
	testUserHandle := fmt.Sprintf("cmt%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("cmt%d@test.local", time.Now().UnixNano()%1000000)
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
	testCommunityDID, err := createFeedTestCommunity(db, ctx, "comment-e2e-community", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post for Comments", 0, time.Now())
	postCID := "bafyposte2etest"

	// Setup comment service with PDS factory
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

	// Create mock OAuth session
	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	t.Run("create comment with real Jetstream indexing", func(t *testing.T) {
		// Setup Jetstream consumer
		commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

		// Channels for event communication
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		errorChan := make(chan error, 1)
		done := make(chan bool)

		// Start Jetstream consumer in background BEFORE writing to PDS
		t.Logf("\n🔄 Starting Jetstream consumer for comments...")
		go func() {
			subscribeErr := subscribeToJetstreamForComment(ctx, jetstreamURL, userDID, commentConsumer, eventChan, done)
			if subscribeErr != nil {
				errorChan <- subscribeErr
			}
		}()

		// Give Jetstream a moment to connect
		time.Sleep(500 * time.Millisecond)

		// Create comment via service (writes to PDS)
		t.Logf("\n📝 Creating comment via service (writes to PDS)...")

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
			Content: "This is a TRUE E2E test comment via Jetstream!",
			Langs:   []string{"en"},
		}

		parsedDID, parseErr := syntax.ParseDID(userDID)
		if parseErr != nil {
			t.Fatalf("Failed to parse DID: %v", parseErr)
		}
		session, sessionErr := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)
		if sessionErr != nil {
			t.Fatalf("Failed to get session: %v", sessionErr)
		}

		commentResp, err := commentService.CreateComment(ctx, session, commentReq)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		t.Logf("✅ Comment written to PDS:")
		t.Logf("   URI: %s", commentResp.URI)
		t.Logf("   CID: %s", commentResp.CID)

		// Wait for Jetstream event
		t.Logf("\n⏳ Waiting for Jetstream event (max 30 seconds)...")

		select {
		case event := <-eventChan:
			t.Logf("✅ Received real Jetstream event!")
			t.Logf("   Event DID:    %s", event.Did)
			t.Logf("   Collection:   %s", event.Commit.Collection)
			t.Logf("   Operation:    %s", event.Commit.Operation)
			t.Logf("   RKey:         %s", event.Commit.RKey)

			// Verify it's our comment
			if event.Did != userDID {
				t.Errorf("Expected DID %s, got %s", userDID, event.Did)
			}
			if event.Commit.Collection != "social.coves.community.comment" {
				t.Errorf("Expected collection social.coves.community.comment, got %s", event.Commit.Collection)
			}
			if event.Commit.Operation != "create" {
				t.Errorf("Expected operation create, got %s", event.Commit.Operation)
			}

			// Verify indexed in AppView database
			t.Logf("\n🔍 Querying AppView database...")
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
			if indexedComment.Content != "This is a TRUE E2E test comment via Jetstream!" {
				t.Errorf("Expected content mismatch, got %s", indexedComment.Content)
			}

			close(done)

		case err := <-errorChan:
			t.Fatalf("Jetstream error: %v", err)

		case <-time.After(30 * time.Second):
			t.Fatalf("Timeout: No Jetstream event received within 30 seconds")
		}

		t.Logf("\n✅ TRUE E2E COMMENT CREATE FLOW COMPLETE:")
		t.Logf("   Client → Service → PDS → Jetstream → Consumer → AppView ✓")
	})
}

// TestCommentE2E_UpdateWithJetstream tests comment update with real Jetstream indexing
func TestCommentE2E_UpdateWithJetstream(t *testing.T) {
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
	defer func() { _ = db.Close() }()

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
	_ = healthResp.Body.Close()

	// Check if Jetstream is running
	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.comment", pdsHostname)

	testConn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		t.Skipf("Jetstream not running at %s: %v", jetstreamURL, err)
	}
	_ = testConn.Close()

	ctx := context.Background()

	// Setup repositories
	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Create test user on PDS
	testUserHandle := fmt.Sprintf("cmtup%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("cmtup%d@test.local", time.Now().UnixNano()%1000000)
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}

	testUser := createTestUser(t, db, testUserHandle, userDID)

	testCommunityDID, err := createFeedTestCommunity(db, ctx, "comment-upd-community", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post for Update", 0, time.Now())
	postCID := "bafypostupdate"

	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
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

	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	t.Run("update comment with real Jetstream indexing", func(t *testing.T) {
		commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

		// First, create a comment and wait for it to be indexed
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		errorChan := make(chan error, 1)
		done := make(chan bool)

		go func() {
			subscribeErr := subscribeToJetstreamForComment(ctx, jetstreamURL, userDID, commentConsumer, eventChan, done)
			if subscribeErr != nil {
				errorChan <- subscribeErr
			}
		}()

		time.Sleep(500 * time.Millisecond)

		// Create initial comment
		t.Logf("\n📝 Creating initial comment...")
		commentReq := comments.CreateCommentRequest{
			Reply: comments.ReplyRef{
				Root:   comments.StrongRef{URI: postURI, CID: postCID},
				Parent: comments.StrongRef{URI: postURI, CID: postCID},
			},
			Content: "Original comment content",
			Langs:   []string{"en"},
		}

		parsedDID, parseErr := syntax.ParseDID(userDID)
		if parseErr != nil {
			t.Fatalf("Failed to parse DID: %v", parseErr)
		}
		session, sessionErr := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)
		if sessionErr != nil {
			t.Fatalf("Failed to get session: %v", sessionErr)
		}
		commentResp, err := commentService.CreateComment(ctx, session, commentReq)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Wait for create event
		select {
		case <-eventChan:
			t.Logf("✅ Create event received and indexed")
		case err := <-errorChan:
			t.Fatalf("Jetstream error: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatalf("Timeout waiting for create event")
		}
		close(done)

		// Now update the comment
		t.Logf("\n📝 Updating comment via service...")

		// Start new Jetstream subscription for update event
		updateEventChan := make(chan *jetstream.JetstreamEvent, 10)
		updateErrorChan := make(chan error, 1)
		updateDone := make(chan bool)

		go func() {
			subscribeErr := subscribeToJetstreamForCommentUpdate(ctx, jetstreamURL, userDID, commentConsumer, updateEventChan, updateDone)
			if subscribeErr != nil {
				updateErrorChan <- subscribeErr
			}
		}()

		time.Sleep(500 * time.Millisecond)

		// Get existing comment CID from PDS for optimistic locking
		rkey := utils.ExtractRKeyFromURI(commentResp.URI)
		pdsResp, httpErr := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=social.coves.community.comment&rkey=%s",
			pdsURL, userDID, rkey))
		if httpErr != nil {
			t.Fatalf("Failed to get record from PDS: %v", httpErr)
		}
		defer func() { _ = pdsResp.Body.Close() }()
		if pdsResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(pdsResp.Body)
			t.Fatalf("Failed to get record from PDS: status=%d body=%s", pdsResp.StatusCode, string(body))
		}
		var pdsRecord struct {
			CID string `json:"cid"`
		}
		if decodeErr := json.NewDecoder(pdsResp.Body).Decode(&pdsRecord); decodeErr != nil {
			t.Fatalf("Failed to decode PDS response: %v", decodeErr)
		}

		updateReq := comments.UpdateCommentRequest{
			URI:     commentResp.URI,
			Content: "Updated comment content via E2E test!",
			Langs:   []string{"en"},
		}

		updatedComment, err := commentService.UpdateComment(ctx, session, updateReq)
		if err != nil {
			t.Fatalf("Failed to update comment: %v", err)
		}

		t.Logf("✅ Comment updated on PDS:")
		t.Logf("   URI: %s", updatedComment.URI)
		t.Logf("   CID: %s", updatedComment.CID)

		// Wait for update event from Jetstream
		t.Logf("\n⏳ Waiting for update event from Jetstream...")

		select {
		case event := <-updateEventChan:
			t.Logf("✅ Received update event from Jetstream!")
			t.Logf("   Operation: %s", event.Commit.Operation)

			if event.Commit.Operation != "update" {
				t.Errorf("Expected operation 'update', got '%s'", event.Commit.Operation)
			}

			// Verify updated content in AppView
			indexedComment, err := commentRepo.GetByURI(ctx, commentResp.URI)
			if err != nil {
				t.Fatalf("Failed to get updated comment: %v", err)
			}

			if indexedComment.Content != "Updated comment content via E2E test!" {
				t.Errorf("Expected updated content, got: %s", indexedComment.Content)
			}

			t.Logf("✅ Comment updated in AppView:")
			t.Logf("   Content: %s", indexedComment.Content)

			close(updateDone)

		case err := <-updateErrorChan:
			t.Fatalf("Jetstream error: %v", err)

		case <-time.After(30 * time.Second):
			t.Fatalf("Timeout: No update event received within 30 seconds")
		}

		t.Logf("\n✅ TRUE E2E COMMENT UPDATE FLOW COMPLETE:")
		t.Logf("   Client → Service → PDS PutRecord → Jetstream → Consumer → AppView ✓")
	})
}

// TestCommentE2E_DeleteWithJetstream tests comment deletion with real Jetstream indexing
func TestCommentE2E_DeleteWithJetstream(t *testing.T) {
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
	defer func() { _ = db.Close() }()

	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	pdsHostname := strings.TrimPrefix(pdsURL, "http://")
	pdsHostname = strings.TrimPrefix(pdsHostname, "https://")
	pdsHostname = strings.Split(pdsHostname, ":")[0]
	jetstreamURL := fmt.Sprintf("ws://%s:6008/subscribe?wantedCollections=social.coves.community.comment", pdsHostname)

	testConn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		t.Skipf("Jetstream not running at %s: %v", jetstreamURL, err)
	}
	_ = testConn.Close()

	ctx := context.Background()

	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)

	testUserHandle := fmt.Sprintf("cmtdl%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("cmtdl%d@test.local", time.Now().UnixNano()%1000000)
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}

	testUser := createTestUser(t, db, testUserHandle, userDID)

	testCommunityDID, err := createFeedTestCommunity(db, ctx, "comment-del-community", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Test Post for Delete", 0, time.Now())
	postCID := "bafypostdelete"

	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
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

	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	t.Run("delete comment with real Jetstream indexing", func(t *testing.T) {
		commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)

		// First, create a comment
		eventChan := make(chan *jetstream.JetstreamEvent, 10)
		errorChan := make(chan error, 1)
		done := make(chan bool)

		go func() {
			subscribeErr := subscribeToJetstreamForComment(ctx, jetstreamURL, userDID, commentConsumer, eventChan, done)
			if subscribeErr != nil {
				errorChan <- subscribeErr
			}
		}()

		time.Sleep(500 * time.Millisecond)

		t.Logf("\n📝 Creating comment to delete...")
		commentReq := comments.CreateCommentRequest{
			Reply: comments.ReplyRef{
				Root:   comments.StrongRef{URI: postURI, CID: postCID},
				Parent: comments.StrongRef{URI: postURI, CID: postCID},
			},
			Content: "This comment will be deleted",
			Langs:   []string{"en"},
		}

		parsedDID, parseErr := syntax.ParseDID(userDID)
		if parseErr != nil {
			t.Fatalf("Failed to parse DID: %v", parseErr)
		}
		session, sessionErr := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)
		if sessionErr != nil {
			t.Fatalf("Failed to get session: %v", sessionErr)
		}
		commentResp, err := commentService.CreateComment(ctx, session, commentReq)
		if err != nil {
			t.Fatalf("Failed to create comment: %v", err)
		}

		// Wait for create event
		select {
		case <-eventChan:
			t.Logf("✅ Create event received")
		case err := <-errorChan:
			t.Fatalf("Jetstream error: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatalf("Timeout waiting for create event")
		}
		close(done)

		// Verify comment exists
		_, err = commentRepo.GetByURI(ctx, commentResp.URI)
		if err != nil {
			t.Fatalf("Comment should exist before delete: %v", err)
		}

		// Now delete the comment
		t.Logf("\n🗑️ Deleting comment via service...")

		deleteEventChan := make(chan *jetstream.JetstreamEvent, 10)
		deleteErrorChan := make(chan error, 1)
		deleteDone := make(chan bool)

		go func() {
			subscribeErr := subscribeToJetstreamForCommentDelete(ctx, jetstreamURL, userDID, commentConsumer, deleteEventChan, deleteDone)
			if subscribeErr != nil {
				deleteErrorChan <- subscribeErr
			}
		}()

		time.Sleep(500 * time.Millisecond)

		err = commentService.DeleteComment(ctx, session, comments.DeleteCommentRequest{URI: commentResp.URI})
		if err != nil {
			t.Fatalf("Failed to delete comment: %v", err)
		}

		t.Logf("✅ Comment delete request sent to PDS")

		// Wait for delete event from Jetstream
		t.Logf("\n⏳ Waiting for delete event from Jetstream...")

		select {
		case event := <-deleteEventChan:
			t.Logf("✅ Received delete event from Jetstream!")
			t.Logf("   Operation: %s", event.Commit.Operation)

			if event.Commit.Operation != "delete" {
				t.Errorf("Expected operation 'delete', got '%s'", event.Commit.Operation)
			}

			// Verify comment is soft-deleted in AppView
			deletedComment, err := commentRepo.GetByURI(ctx, commentResp.URI)
			if err != nil {
				t.Fatalf("Failed to get deleted comment: %v", err)
			}

			if deletedComment.DeletedAt == nil {
				t.Errorf("Expected comment to be soft-deleted (deleted_at should be set)")
			} else {
				t.Logf("✅ Comment soft-deleted in AppView at: %v", *deletedComment.DeletedAt)
			}

			close(deleteDone)

		case err := <-deleteErrorChan:
			t.Fatalf("Jetstream error: %v", err)

		case <-time.After(30 * time.Second):
			t.Fatalf("Timeout: No delete event received within 30 seconds")
		}

		t.Logf("\n✅ TRUE E2E COMMENT DELETE FLOW COMPLETE:")
		t.Logf("   Client → Service → PDS DeleteRecord → Jetstream → Consumer → AppView ✓")
	})
}

// subscribeToJetstreamForComment subscribes to real Jetstream firehose for comment create events
func subscribeToJetstreamForComment(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	consumer *jetstream.CommentEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					return nil
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			// Check if this is a comment create event for the target DID
			if event.Did == targetDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.community.comment" &&
				event.Commit.Operation == "create" {

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

// subscribeToJetstreamForCommentUpdate subscribes for comment update events
func subscribeToJetstreamForCommentUpdate(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	consumer *jetstream.CommentEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					return nil
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			if event.Did == targetDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.community.comment" &&
				event.Commit.Operation == "update" {

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

// subscribeToJetstreamForCommentDelete subscribes for comment delete events
func subscribeToJetstreamForCommentDelete(
	ctx context.Context,
	jetstreamURL string,
	targetDID string,
	consumer *jetstream.CommentEventConsumer,
	eventChan chan<- *jetstream.JetstreamEvent,
	done <-chan bool,
) error {
	conn, _, err := websocket.DefaultDialer.Dial(jetstreamURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() { _ = conn.Close() }()

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return fmt.Errorf("failed to set read deadline: %w", err)
			}

			var event jetstream.JetstreamEvent
			err := conn.ReadJSON(&event)
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					return nil
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return fmt.Errorf("failed to read Jetstream message: %w", err)
			}

			if event.Did == targetDID && event.Kind == "commit" &&
				event.Commit != nil && event.Commit.Collection == "social.coves.community.comment" &&
				event.Commit.Operation == "delete" {

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

// TestCommentE2E_Authorization tests that users cannot modify other users' comments
func TestCommentE2E_Authorization(t *testing.T) {
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
	defer func() { _ = db.Close() }()

	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	ctx := context.Background()

	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Create two test users on PDS
	userAHandle := fmt.Sprintf("usera%d.local.coves.dev", time.Now().UnixNano()%1000000)
	userAEmail := fmt.Sprintf("usera%d@test.local", time.Now().UnixNano()%1000000)
	userAPassword := "test-password-123"

	userBHandle := fmt.Sprintf("userb%d.local.coves.dev", time.Now().UnixNano()%1000000)
	userBEmail := fmt.Sprintf("userb%d@test.local", time.Now().UnixNano()%1000000)
	userBPassword := "test-password-123"

	pdsAccessTokenA, userADID, err := createPDSAccount(pdsURL, userAHandle, userAEmail, userAPassword)
	if err != nil {
		t.Fatalf("Failed to create test user A on PDS: %v", err)
	}

	pdsAccessTokenB, userBDID, err := createPDSAccount(pdsURL, userBHandle, userBEmail, userBPassword)
	if err != nil {
		t.Fatalf("Failed to create test user B on PDS: %v", err)
	}

	testUserA := createTestUser(t, db, userAHandle, userADID)
	_ = createTestUser(t, db, userBHandle, userBDID)

	testCommunityDID, err := createFeedTestCommunity(db, ctx, "auth-test-community", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	postURI := createTestPost(t, db, testCommunityDID, testUserA.DID, "Auth Test Post", 0, time.Now())
	postCID := "bafypostauthtest"

	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
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

	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userADID, "session-"+userADID, pdsAccessTokenA, pdsURL)
	mockStore.AddSessionWithPDS(userBDID, "session-"+userBDID, pdsAccessTokenB, pdsURL)

	t.Run("user cannot update another user's comment", func(t *testing.T) {
		// User A creates a comment
		parsedDIDA, parseErr := syntax.ParseDID(userADID)
		if parseErr != nil {
			t.Fatalf("Failed to parse DID A: %v", parseErr)
		}
		sessionA, sessionErr := mockStore.GetSession(ctx, parsedDIDA, "session-"+userADID)
		if sessionErr != nil {
			t.Fatalf("Failed to get session A: %v", sessionErr)
		}

		commentReq := comments.CreateCommentRequest{
			Reply: comments.ReplyRef{
				Root:   comments.StrongRef{URI: postURI, CID: postCID},
				Parent: comments.StrongRef{URI: postURI, CID: postCID},
			},
			Content: "User A's comment",
			Langs:   []string{"en"},
		}

		commentResp, err := commentService.CreateComment(ctx, sessionA, commentReq)
		if err != nil {
			t.Fatalf("User A failed to create comment: %v", err)
		}
		t.Logf("User A created comment: %s", commentResp.URI)

		// User B tries to update User A's comment
		parsedDIDB, parseErr := syntax.ParseDID(userBDID)
		if parseErr != nil {
			t.Fatalf("Failed to parse DID B: %v", parseErr)
		}
		sessionB, sessionErr := mockStore.GetSession(ctx, parsedDIDB, "session-"+userBDID)
		if sessionErr != nil {
			t.Fatalf("Failed to get session B: %v", sessionErr)
		}

		updateReq := comments.UpdateCommentRequest{
			URI:     commentResp.URI,
			Content: "User B trying to update User A's comment",
			Langs:   []string{"en"},
		}

		_, err = commentService.UpdateComment(ctx, sessionB, updateReq)
		if err == nil {
			t.Errorf("Expected error when User B tries to update User A's comment, got nil")
		} else if err != comments.ErrNotAuthorized {
			t.Errorf("Expected ErrNotAuthorized, got: %v", err)
		} else {
			t.Logf("✅ Correctly rejected: User B cannot update User A's comment")
		}
	})

	t.Run("user cannot delete another user's comment", func(t *testing.T) {
		// User A creates a comment
		parsedDIDA, parseErr := syntax.ParseDID(userADID)
		if parseErr != nil {
			t.Fatalf("Failed to parse DID A: %v", parseErr)
		}
		sessionA, sessionErr := mockStore.GetSession(ctx, parsedDIDA, "session-"+userADID)
		if sessionErr != nil {
			t.Fatalf("Failed to get session A: %v", sessionErr)
		}

		commentReq := comments.CreateCommentRequest{
			Reply: comments.ReplyRef{
				Root:   comments.StrongRef{URI: postURI, CID: postCID},
				Parent: comments.StrongRef{URI: postURI, CID: postCID},
			},
			Content: "User A's comment for delete test",
			Langs:   []string{"en"},
		}

		commentResp, err := commentService.CreateComment(ctx, sessionA, commentReq)
		if err != nil {
			t.Fatalf("User A failed to create comment: %v", err)
		}
		t.Logf("User A created comment: %s", commentResp.URI)

		// User B tries to delete User A's comment
		parsedDIDB, parseErr := syntax.ParseDID(userBDID)
		if parseErr != nil {
			t.Fatalf("Failed to parse DID B: %v", parseErr)
		}
		sessionB, sessionErr := mockStore.GetSession(ctx, parsedDIDB, "session-"+userBDID)
		if sessionErr != nil {
			t.Fatalf("Failed to get session B: %v", sessionErr)
		}

		deleteReq := comments.DeleteCommentRequest{
			URI: commentResp.URI,
		}

		err = commentService.DeleteComment(ctx, sessionB, deleteReq)
		if err == nil {
			t.Errorf("Expected error when User B tries to delete User A's comment, got nil")
		} else if err != comments.ErrNotAuthorized {
			t.Errorf("Expected ErrNotAuthorized, got: %v", err)
		} else {
			t.Logf("✅ Correctly rejected: User B cannot delete User A's comment")
		}
	})
}

// TestCommentE2E_ValidationErrors tests that validation errors are properly returned
func TestCommentE2E_ValidationErrors(t *testing.T) {
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
	defer func() { _ = db.Close() }()

	if dialectErr := goose.SetDialect("postgres"); dialectErr != nil {
		t.Fatalf("Failed to set goose dialect: %v", dialectErr)
	}
	if migrateErr := goose.Up(db, "../../internal/db/migrations"); migrateErr != nil {
		t.Fatalf("Failed to run migrations: %v", migrateErr)
	}

	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	ctx := context.Background()

	commentRepo := postgres.NewCommentRepository(db)
	postRepo := postgres.NewPostRepository(db)

	// Create test user on PDS
	testUserHandle := fmt.Sprintf("valtest%d.local.coves.dev", time.Now().UnixNano()%1000000)
	testUserEmail := fmt.Sprintf("valtest%d@test.local", time.Now().UnixNano()%1000000)
	testUserPassword := "test-password-123"

	pdsAccessToken, userDID, err := createPDSAccount(pdsURL, testUserHandle, testUserEmail, testUserPassword)
	if err != nil {
		t.Fatalf("Failed to create test user on PDS: %v", err)
	}

	testUser := createTestUser(t, db, testUserHandle, userDID)

	testCommunityDID, err := createFeedTestCommunity(db, ctx, "val-test-community", "owner.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	postURI := createTestPost(t, db, testCommunityDID, testUser.DID, "Validation Test Post", 0, time.Now())
	postCID := "bafypostvaltest"

	commentPDSFactory := func(ctx context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
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

	mockStore := NewMockOAuthStore()
	mockStore.AddSessionWithPDS(userDID, "session-"+userDID, pdsAccessToken, pdsURL)

	parsedDID, parseErr := syntax.ParseDID(userDID)
	if parseErr != nil {
		t.Fatalf("Failed to parse DID: %v", parseErr)
	}
	session, sessionErr := mockStore.GetSession(ctx, parsedDID, "session-"+userDID)
	if sessionErr != nil {
		t.Fatalf("Failed to get session: %v", sessionErr)
	}

	t.Run("empty content returns ErrContentEmpty", func(t *testing.T) {
		commentReq := comments.CreateCommentRequest{
			Reply: comments.ReplyRef{
				Root:   comments.StrongRef{URI: postURI, CID: postCID},
				Parent: comments.StrongRef{URI: postURI, CID: postCID},
			},
			Content: "",
			Langs:   []string{"en"},
		}

		_, err := commentService.CreateComment(ctx, session, commentReq)
		if err == nil {
			t.Errorf("Expected error for empty content, got nil")
		} else if err != comments.ErrContentEmpty {
			t.Errorf("Expected ErrContentEmpty, got: %v", err)
		} else {
			t.Logf("✅ Correctly rejected: empty content returns ErrContentEmpty")
		}
	})

	t.Run("whitespace-only content returns ErrContentEmpty", func(t *testing.T) {
		commentReq := comments.CreateCommentRequest{
			Reply: comments.ReplyRef{
				Root:   comments.StrongRef{URI: postURI, CID: postCID},
				Parent: comments.StrongRef{URI: postURI, CID: postCID},
			},
			Content: "   \t\n   ",
			Langs:   []string{"en"},
		}

		_, err := commentService.CreateComment(ctx, session, commentReq)
		if err == nil {
			t.Errorf("Expected error for whitespace-only content, got nil")
		} else if err != comments.ErrContentEmpty {
			t.Errorf("Expected ErrContentEmpty, got: %v", err)
		} else {
			t.Logf("✅ Correctly rejected: whitespace-only content returns ErrContentEmpty")
		}
	})

	t.Run("invalid reply reference returns ErrInvalidReply", func(t *testing.T) {
		commentReq := comments.CreateCommentRequest{
			Reply: comments.ReplyRef{
				Root:   comments.StrongRef{URI: "", CID: ""},
				Parent: comments.StrongRef{URI: "", CID: ""},
			},
			Content: "Valid content",
			Langs:   []string{"en"},
		}

		_, err := commentService.CreateComment(ctx, session, commentReq)
		if err == nil {
			t.Errorf("Expected error for invalid reply, got nil")
		} else if err != comments.ErrInvalidReply {
			t.Errorf("Expected ErrInvalidReply, got: %v", err)
		} else {
			t.Logf("✅ Correctly rejected: invalid reply returns ErrInvalidReply")
		}
	})
}

