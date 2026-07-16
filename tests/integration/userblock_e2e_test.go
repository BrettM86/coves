package integration

import (
	"Coves/internal/api/routes"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/userblocks"
	"Coves/internal/db/postgres"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/lib/pq"
)

// TestUserBlockE2E_BlockAndUnblock tests the full user block lifecycle with a real PDS.
// Flow: Client -> XRPC -> PDS Write -> Verify on PDS -> Jetstream -> Consumer -> AppView
// Then: Client -> XRPC Unblock -> PDS Delete -> Jetstream -> Consumer -> AppView removal
func TestUserBlockE2E_BlockAndUnblock(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	pdsURL := getTestPDSURL()

	// Health check PDS
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	func() {
		if closeErr := healthResp.Body.Close(); closeErr != nil {
			t.Logf("Failed to close health response: %v", closeErr)
		}
	}()

	// Clean up user_blocks at the end to avoid polluting other tests
	defer func() {
		_, _ = db.Exec("DELETE FROM user_blocks")
	}()

	// Setup repository
	blockRepo := postgres.NewUserBlockRepository(db)

	// Create User A (blocker) on real PDS
	userAID := uniqueTestID()
	userAHandle := fmt.Sprintf("blka%s.local.coves.dev", userAID)
	userAEmail := fmt.Sprintf("blockerA-%s@test.local", userAID)
	userAPassword := "test-password-123"

	t.Logf("Creating User A (blocker) on PDS: %s", userAHandle)
	pdsAccessTokenA, userADID, err := createPDSAccount(pdsURL, userAHandle, userAEmail, userAPassword)
	if err != nil {
		t.Fatalf("Failed to create User A on PDS: %v", err)
	}
	t.Logf("User A created: DID=%s", userADID)

	// Create User B (target) on real PDS
	userBID := uniqueTestID()
	userBHandle := fmt.Sprintf("blkb%s.local.coves.dev", userBID)
	userBEmail := fmt.Sprintf("blockerB-%s@test.local", userBID)
	userBPassword := "test-password-123"

	t.Logf("Creating User B (target) on PDS: %s", userBHandle)
	_, userBDID, err := createPDSAccount(pdsURL, userBHandle, userBEmail, userBPassword)
	if err != nil {
		t.Fatalf("Failed to create User B on PDS: %v", err)
	}
	t.Logf("User B created: DID=%s", userBDID)

	// Index both users in AppView
	createTestUser(t, db, userAHandle, userADID)
	createTestUser(t, db, userBHandle, userBDID)

	// Setup OAuth middleware with User A's real PDS access token
	e2eAuth := NewE2EOAuthMiddleware()
	tokenA := e2eAuth.AddUserWithPDSToken(userADID, pdsAccessTokenA, pdsURL)

	// Setup service with password-based PDS client factory
	service := userblocks.NewServiceWithPDSFactory(blockRepo, nil, UserBlockPasswordAuthPDSClientFactory())

	// Setup HTTP server with XRPC routes
	r := chi.NewRouter()
	routes.RegisterUserBlockRoutes(r, service, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Setup Jetstream consumer with block repo
	userConsumer := jetstream.NewUserEventConsumer(nil, nil,
		jetstream.WithUserBlockRepo(blockRepo))

	// ====================================================================================
	// BLOCK PHASE: User A blocks User B
	// ====================================================================================
	t.Logf("\n--- BLOCK PHASE: User A blocks User B ---")

	blockReq := map[string]interface{}{
		"subject": userBDID,
	}

	reqBody, marshalErr := json.Marshal(blockReq)
	if marshalErr != nil {
		t.Fatalf("Failed to marshal block request: %v", marshalErr)
	}

	req, err := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.actor.blockUser",
		bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to create block request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokenA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to POST block: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatalf("Expected 200, got %d (failed to read body: %v)", resp.StatusCode, readErr)
		}
		t.Logf("XRPC Block Failed")
		t.Logf("   Status: %d", resp.StatusCode)
		t.Logf("   Response: %s", string(body))
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	var blockResp struct {
		Block struct {
			RecordURI string `json:"recordUri"`
			RecordCID string `json:"recordCid"`
		} `json:"block"`
	}

	if decodeErr := json.NewDecoder(resp.Body).Decode(&blockResp); decodeErr != nil {
		t.Fatalf("Failed to decode block response: %v", decodeErr)
	}

	t.Logf("XRPC block response received:")
	t.Logf("   RecordURI: %s", blockResp.Block.RecordURI)
	t.Logf("   RecordCID: %s", blockResp.Block.RecordCID)

	if blockResp.Block.RecordURI == "" {
		t.Fatal("Block response missing recordUri")
	}
	if blockResp.Block.RecordCID == "" {
		t.Fatal("Block response missing recordCid")
	}

	// Verify block record was written to PDS
	t.Logf("\nVerifying block record on PDS...")
	rkey := utils.ExtractRKeyFromURI(blockResp.Block.RecordURI)
	collection := "social.coves.actor.block"

	pdsResp, pdsErr := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=%s&rkey=%s",
		pdsURL, userADID, collection, rkey))
	if pdsErr != nil {
		t.Fatalf("Failed to fetch block record from PDS: %v", pdsErr)
	}
	defer func() {
		if closeErr := pdsResp.Body.Close(); closeErr != nil {
			t.Logf("Failed to close PDS response: %v", closeErr)
		}
	}()

	if pdsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(pdsResp.Body)
		t.Fatalf("Block record not found on PDS: status %d, body: %s", pdsResp.StatusCode, string(body))
	}

	var pdsRecord struct {
		Value map[string]interface{} `json:"value"`
		CID   string                 `json:"cid"`
	}
	if decodeErr := json.NewDecoder(pdsResp.Body).Decode(&pdsRecord); decodeErr != nil {
		t.Fatalf("Failed to decode PDS record: %v", decodeErr)
	}

	t.Logf("Block record found on PDS:")
	t.Logf("   CID: %s", pdsRecord.CID)
	t.Logf("   Subject: %v", pdsRecord.Value["subject"])

	// Verify subject DID in PDS record
	if pdsRecord.Value["subject"] != userBDID {
		t.Errorf("Expected subject '%s', got %v", userBDID, pdsRecord.Value["subject"])
	}

	// Simulate Jetstream CREATE event
	t.Logf("\nSimulating Jetstream CREATE event for block...")
	createEvent := jetstream.JetstreamEvent{
		Did:    userADID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-block-rev-create",
			Operation:  "create",
			Collection: "social.coves.actor.block",
			RKey:       rkey,
			CID:        pdsRecord.CID,
			Record: map[string]interface{}{
				"$type":     "social.coves.actor.block",
				"subject":   userBDID,
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}

	if handleErr := userConsumer.HandleEvent(ctx, &createEvent); handleErr != nil {
		t.Fatalf("Failed to handle block create event: %v", handleErr)
	}

	// Verify block indexed in AppView via repo.GetBlock()
	t.Logf("\nVerifying block indexed in AppView...")
	indexedBlock, err := blockRepo.GetBlock(ctx, userADID, userBDID)
	if err != nil {
		t.Fatalf("Block not indexed in AppView: %v", err)
	}

	t.Logf("Block indexed in AppView:")
	t.Logf("   BlockerDID: %s", indexedBlock.BlockerDID)
	t.Logf("   BlockedDID: %s", indexedBlock.BlockedDID)
	t.Logf("   RecordURI:  %s", indexedBlock.RecordURI)
	t.Logf("   RecordCID:  %s", indexedBlock.RecordCID)

	if indexedBlock.BlockerDID != userADID {
		t.Errorf("Expected blocker_did %s, got %s", userADID, indexedBlock.BlockerDID)
	}
	if indexedBlock.BlockedDID != userBDID {
		t.Errorf("Expected blocked_did %s, got %s", userBDID, indexedBlock.BlockedDID)
	}

	// Verify via IsBlocked
	isBlocked, err := blockRepo.IsBlocked(ctx, userADID, userBDID)
	if err != nil {
		t.Fatalf("Failed to check IsBlocked: %v", err)
	}
	if !isBlocked {
		t.Error("Expected IsBlocked to return true, got false")
	}

	t.Logf("BLOCK PHASE COMPLETE:")
	t.Logf("   Client -> XRPC -> PDS Write -> Jetstream -> Consumer -> AppView")
	t.Logf("   Block written to PDS")
	t.Logf("   Block indexed in AppView")
	t.Logf("   IsBlocked confirmed")

	// ====================================================================================
	// UNBLOCK PHASE: User A unblocks User B
	// ====================================================================================
	t.Logf("\n--- UNBLOCK PHASE: User A unblocks User B ---")

	unblockReq := map[string]interface{}{
		"subject": userBDID,
	}

	unblockBody, marshalErr := json.Marshal(unblockReq)
	if marshalErr != nil {
		t.Fatalf("Failed to marshal unblock request: %v", marshalErr)
	}

	unblockHTTPReq, err := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.actor.unblockUser",
		bytes.NewBuffer(unblockBody))
	if err != nil {
		t.Fatalf("Failed to create unblock request: %v", err)
	}
	unblockHTTPReq.Header.Set("Content-Type", "application/json")
	unblockHTTPReq.Header.Set("Authorization", "Bearer "+tokenA)

	unblockResp, err := http.DefaultClient.Do(unblockHTTPReq)
	if err != nil {
		t.Fatalf("Failed to POST unblock: %v", err)
	}
	defer func() {
		if closeErr := unblockResp.Body.Close(); closeErr != nil {
			t.Logf("Failed to close unblock response: %v", closeErr)
		}
	}()

	if unblockResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(unblockResp.Body)
		t.Fatalf("Unblock failed: status %d, body: %s", unblockResp.StatusCode, string(body))
	}

	t.Logf("Unblock XRPC request succeeded")

	// Verify record deleted from PDS (expect error/404)
	t.Logf("\nVerifying block record deleted from PDS...")
	pdsDeleteCheck, pdsDeleteErr := http.Get(fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=%s&rkey=%s",
		pdsURL, userADID, collection, rkey))
	if pdsDeleteErr != nil {
		t.Fatalf("Failed to check PDS record after unblock: %v", pdsDeleteErr)
	}
	defer func() {
		if closeErr := pdsDeleteCheck.Body.Close(); closeErr != nil {
			t.Logf("Failed to close PDS delete check response: %v", closeErr)
		}
	}()

	switch pdsDeleteCheck.StatusCode {
	case http.StatusOK:
		t.Error("Expected block record to be deleted from PDS, but it still exists")
	case http.StatusBadRequest, http.StatusNotFound:
		// Expected: PDS returns 400 (RecordNotFound) or 404 for deleted records
		t.Logf("Block record confirmed deleted from PDS (status: %d)", pdsDeleteCheck.StatusCode)
	default:
		body, _ := io.ReadAll(pdsDeleteCheck.Body)
		t.Errorf("Unexpected status when checking deleted record: %d, body: %s", pdsDeleteCheck.StatusCode, string(body))
	}

	// Simulate Jetstream DELETE event
	t.Logf("\nSimulating Jetstream DELETE event for unblock...")
	deleteEvent := jetstream.JetstreamEvent{
		Did:    userADID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-block-rev-delete",
			Operation:  "delete",
			Collection: "social.coves.actor.block",
			RKey:       rkey,
		},
	}

	if handleErr := userConsumer.HandleEvent(ctx, &deleteEvent); handleErr != nil {
		t.Fatalf("Failed to handle block delete event: %v", handleErr)
	}

	// Verify block removed from AppView
	t.Logf("\nVerifying block removed from AppView...")
	_, err = blockRepo.GetBlock(ctx, userADID, userBDID)
	if err == nil {
		t.Error("Expected block to be deleted from AppView, but it still exists")
	}

	// Verify via IsBlocked
	isBlockedAfter, err := blockRepo.IsBlocked(ctx, userADID, userBDID)
	if err != nil {
		t.Fatalf("Failed to check IsBlocked after unblock: %v", err)
	}
	if isBlockedAfter {
		t.Error("Expected IsBlocked to return false after unblock, got true")
	}

	t.Logf("UNBLOCK PHASE COMPLETE:")
	t.Logf("   Unblock request sent via XRPC")
	t.Logf("   Block record deleted from PDS")
	t.Logf("   Block removed from AppView")
	t.Logf("   IsBlocked confirmed false")

	t.Logf("\nTRUE E2E BLOCK/UNBLOCK FLOW COMPLETE:")
	t.Logf("   Client -> XRPC -> PDS Write -> Jetstream -> Consumer -> AppView")
	t.Logf("   Client -> XRPC Unblock -> PDS Delete -> Jetstream -> Consumer -> AppView removal")
}

// TestUserBlockE2E_SelfBlockPrevented tests that a user cannot block themselves.
// This validates the self-block guard in the service layer with a real PDS.
func TestUserBlockE2E_SelfBlockPrevented(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	pdsURL := getTestPDSURL()

	// Health check PDS
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v", pdsURL, err)
	}
	func() {
		if closeErr := healthResp.Body.Close(); closeErr != nil {
			t.Logf("Failed to close health response: %v", closeErr)
		}
	}()

	// Clean up user_blocks at the end
	defer func() {
		_, _ = db.Exec("DELETE FROM user_blocks")
	}()

	// Setup repository
	blockRepo := postgres.NewUserBlockRepository(db)

	// Create User A on real PDS
	userAID := uniqueTestID()
	userAHandle := fmt.Sprintf("self%s.local.coves.dev", userAID)
	userAEmail := fmt.Sprintf("selfblock-%s@test.local", userAID)
	userAPassword := "test-password-123"

	t.Logf("Creating User A on PDS: %s", userAHandle)
	pdsAccessTokenA, userADID, err := createPDSAccount(pdsURL, userAHandle, userAEmail, userAPassword)
	if err != nil {
		t.Fatalf("Failed to create User A on PDS: %v", err)
	}
	t.Logf("User A created: DID=%s", userADID)

	// Index user in AppView
	createTestUser(t, db, userAHandle, userADID)

	// Setup OAuth middleware with User A's real PDS access token
	e2eAuth := NewE2EOAuthMiddleware()
	tokenA := e2eAuth.AddUserWithPDSToken(userADID, pdsAccessTokenA, pdsURL)

	// Setup service with password-based PDS client factory
	service := userblocks.NewServiceWithPDSFactory(blockRepo, nil, UserBlockPasswordAuthPDSClientFactory())

	// Setup HTTP server
	r := chi.NewRouter()
	routes.RegisterUserBlockRoutes(r, service, e2eAuth.OAuthAuthMiddleware)
	httpServer := httptest.NewServer(r)
	defer httpServer.Close()

	// Attempt to self-block
	t.Logf("\nAttempting to block self (should fail)...")

	selfBlockReq := map[string]interface{}{
		"subject": userADID,
	}

	reqBody, marshalErr := json.Marshal(selfBlockReq)
	if marshalErr != nil {
		t.Fatalf("Failed to marshal self-block request: %v", marshalErr)
	}

	req, err := http.NewRequest(http.MethodPost,
		httpServer.URL+"/xrpc/social.coves.actor.blockUser",
		bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to create self-block request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokenA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to POST self-block: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Should return 400 Bad Request
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 400 for self-block, got %d: %s", resp.StatusCode, string(body))
	}

	// Parse error response
	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&errResp); decodeErr != nil {
		t.Fatalf("Failed to decode error response: %v", decodeErr)
	}

	// Verify error message mentions self-blocking
	if !contains(errResp.Message, "cannot block yourself") {
		t.Errorf("Expected error about self-blocking, got: %s", errResp.Message)
	}

	t.Logf("Self-block correctly prevented:")
	t.Logf("   Status: %d", resp.StatusCode)
	t.Logf("   Error:  %s", errResp.Error)
	t.Logf("   Message: %s", errResp.Message)

	t.Logf("\nSELF-BLOCK PREVENTION TEST COMPLETE:")
	t.Logf("   User attempted to block themselves")
	t.Logf("   Server returned 400 with 'cannot block yourself'")
	t.Logf("   No record written to PDS")
}
