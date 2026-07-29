//go:build integration

package integration

import (
	"Coves/internal/api/routes"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/userblocks"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"
)

// mockPDSClient implements pds.Client for handler tests without a real PDS.
// Tracks created and deleted records for verification in tests.
type mockPDSClient struct {
	did            string
	mu             sync.Mutex
	createdRecords map[string]bool
	deletedRecords []string // collection/rkey pairs
	createErr      error    // if set, CreateRecord returns this error
}

func newMockPDSClient(did string) *mockPDSClient {
	return &mockPDSClient{
		did:            did,
		createdRecords: make(map[string]bool),
	}
}

func (m *mockPDSClient) CreateRecord(_ context.Context, collection string, rkey string, _ any) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return "", "", m.createErr
	}
	uri := fmt.Sprintf("at://%s/%s/%s", m.did, collection, rkey)
	m.createdRecords[uri] = true
	return uri, "bafymockrecordcid", nil
}

func (m *mockPDSClient) DeleteRecord(_ context.Context, collection string, rkey string) error {
	m.mu.Lock()
	m.deletedRecords = append(m.deletedRecords, collection+"/"+rkey)
	m.mu.Unlock()
	return nil
}

func (m *mockPDSClient) ListRecords(_ context.Context, _ string, _ int, _ string) (*pds.ListRecordsResponse, error) {
	return &pds.ListRecordsResponse{}, nil
}

func (m *mockPDSClient) GetRecord(_ context.Context, _ string, _ string) (*pds.RecordResponse, error) {
	return &pds.RecordResponse{}, nil
}

func (m *mockPDSClient) PutRecord(_ context.Context, _ string, _ string, _ any, _ string) (string, string, error) {
	return "", "", nil
}

func (m *mockPDSClient) UploadBlob(_ context.Context, _ []byte, _ string) (*blobs.BlobRef, error) {
	return nil, nil
}

func (m *mockPDSClient) DID() string {
	return m.did
}

func (m *mockPDSClient) HostURL() string {
	return "http://localhost:3001"
}

// DeleteCallCount returns the number of DeleteRecord calls made.
func (m *mockPDSClient) DeleteCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.deletedRecords)
}

// mockPDSTracker manages per-session mock PDS clients so tests can inspect PDS interactions.
type mockPDSTracker struct {
	mu      sync.Mutex
	clients map[string]*mockPDSClient // DID -> client
}

func newMockPDSTracker() *mockPDSTracker {
	return &mockPDSTracker{clients: make(map[string]*mockPDSClient)}
}

// Factory returns a PDSClientFactory that creates per-session mock clients.
func (t *mockPDSTracker) Factory() userblocks.PDSClientFactory {
	return func(_ context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		did := session.AccountDID.String()
		t.mu.Lock()
		defer t.mu.Unlock()
		if c, ok := t.clients[did]; ok {
			return c, nil
		}
		c := newMockPDSClient(did)
		t.clients[did] = c
		return c, nil
	}
}

// ClientFor returns the mock PDS client for the given DID.
func (t *mockPDSTracker) ClientFor(did string) *mockPDSClient {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clients[did]
}

// userBlockTestEnv bundles all resources created by setupUserBlockTestServer.
type userBlockTestEnv struct {
	Server     *httptest.Server
	Auth       *E2EOAuthMiddleware
	Repo       userblocks.Repository
	PDSTracker *mockPDSTracker
}

// setupUserBlockTestServer creates a test HTTP server with userblock routes wired up.
func setupUserBlockTestServer(t *testing.T) *userBlockTestEnv {
	t.Helper()

	db := testkit.DB(t)

	repo := postgres.NewUserBlockRepository(db)
	tracker := newMockPDSTracker()
	service := userblocks.NewServiceWithPDSFactory(repo, nil, tracker.Factory())

	e2eAuth := NewE2EOAuthMiddleware()

	r := chi.NewRouter()
	routes.RegisterUserBlockRoutes(r, service, e2eAuth.OAuthAuthMiddleware)

	server := httptest.NewServer(r)
	t.Cleanup(func() { server.Close() })

	return &userBlockTestEnv{
		Server:     server,
		Auth:       e2eAuth,
		Repo:       repo,
		PDSTracker: tracker,
	}
}

// postXRPC sends a POST request to an XRPC endpoint and returns the response.
// Fails the test on marshaling or request creation errors.
func postXRPC(t *testing.T, serverURL, path, token string, body any) *http.Response {
	t.Helper()

	reqBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Failed to marshal request body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, serverURL+path, bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request to %s: %v", path, err)
	}

	return resp
}

// getXRPC sends a GET request to an XRPC endpoint and returns the response.
func getXRPC(t *testing.T, serverURL, path, token string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, serverURL+path, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request to %s: %v", path, err)
	}

	return resp
}

// TestUserBlockHandler_BlockUser tests the block user endpoint
func TestUserBlockHandler_BlockUser(t *testing.T) {
	env := setupUserBlockTestServer(t)

	blockerDID := "did:plc:blocker123"
	targetDID := "did:plc:target456"
	token := env.Auth.AddUser(blockerDID)

	resp := postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", token, map[string]string{
		"subject": targetDID,
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	var blockResp struct {
		Block struct {
			RecordURI string `json:"recordUri"`
			RecordCID string `json:"recordCid"`
		} `json:"block"`
	}

	if decodeErr := json.NewDecoder(resp.Body).Decode(&blockResp); decodeErr != nil {
		t.Fatalf("Failed to decode response: %v", decodeErr)
	}

	if blockResp.Block.RecordURI == "" {
		t.Error("Expected non-empty recordUri in response")
	}
	if blockResp.Block.RecordCID == "" {
		t.Error("Expected non-empty recordCid in response")
	}

	// Verify the record URI contains the blocker's actual DID (not a hardcoded mock DID)
	if !strings.Contains(blockResp.Block.RecordURI, blockerDID) {
		t.Errorf("Expected recordUri to contain blocker DID %q, got %q", blockerDID, blockResp.Block.RecordURI)
	}

	// Simulate Jetstream indexing by inserting directly into repo
	// (In a real E2E test, the Jetstream consumer would handle this)
	ctx := context.Background()
	_, repoErr := env.Repo.BlockUser(ctx, &userblocks.UserBlock{
		BlockerDID: blockerDID,
		BlockedDID: targetDID,
		BlockedAt:  time.Now(),
		RecordURI:  blockResp.Block.RecordURI,
		RecordCID:  blockResp.Block.RecordCID,
	})
	if repoErr != nil {
		t.Fatalf("Failed to index block in repo: %v", repoErr)
	}

	// Verify block exists in AppView
	isBlocked, checkErr := env.Repo.IsBlocked(ctx, blockerDID, targetDID)
	if checkErr != nil {
		t.Fatalf("Failed to check block: %v", checkErr)
	}
	if !isBlocked {
		t.Error("Expected user to be blocked after block request")
	}
}

// TestUserBlockHandler_BlockUser_MissingSubject tests blocking with missing subject
func TestUserBlockHandler_BlockUser_MissingSubject(t *testing.T) {
	env := setupUserBlockTestServer(t)

	blockerDID := "did:plc:blocker789"
	token := env.Auth.AddUser(blockerDID)

	resp := postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", token, map[string]string{})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 400, got %d: %s", resp.StatusCode, string(body))
	}

	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&errResp); decodeErr != nil {
		t.Fatalf("Failed to decode error response: %v", decodeErr)
	}

	if errResp.Error != "InvalidRequest" {
		t.Errorf("Expected error code 'InvalidRequest', got %q", errResp.Error)
	}
	if !strings.Contains(errResp.Message, "subject") {
		t.Errorf("Expected error message to mention 'subject', got %q", errResp.Message)
	}
}

// TestUserBlockHandler_BlockUser_Unauthenticated tests blocking without auth
func TestUserBlockHandler_BlockUser_Unauthenticated(t *testing.T) {
	env := setupUserBlockTestServer(t)

	// No token — unauthenticated request
	resp := postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", "", map[string]string{
		"subject": "did:plc:target",
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 401, got %d: %s", resp.StatusCode, string(body))
	}
}

// TestUserBlockHandler_BlockUser_SelfBlock tests that self-blocking returns 400
func TestUserBlockHandler_BlockUser_SelfBlock(t *testing.T) {
	env := setupUserBlockTestServer(t)

	selfDID := "did:plc:selfblock"
	token := env.Auth.AddUser(selfDID)

	resp := postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", token, map[string]string{
		"subject": selfDID,
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 400, got %d: %s", resp.StatusCode, string(body))
	}

	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&errResp); decodeErr != nil {
		t.Fatalf("Failed to decode error response: %v", decodeErr)
	}

	if !strings.Contains(errResp.Message, "block yourself") {
		t.Errorf("Expected error message about self-blocking, got %q", errResp.Message)
	}
}

// TestUserBlockHandler_UnblockUser tests the unblock user endpoint
func TestUserBlockHandler_UnblockUser(t *testing.T) {
	env := setupUserBlockTestServer(t)

	blockerDID := "did:plc:unblocker1"
	targetDID := "did:plc:unblockee1"
	token := env.Auth.AddUser(blockerDID)

	ctx := context.Background()

	// First, create a block via the API
	blockResp := postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", token, map[string]string{
		"subject": targetDID,
	})

	var blockResult struct {
		Block struct {
			RecordURI string `json:"recordUri"`
			RecordCID string `json:"recordCid"`
		} `json:"block"`
	}
	if decodeErr := json.NewDecoder(blockResp.Body).Decode(&blockResult); decodeErr != nil {
		t.Fatalf("Failed to decode block response: %v", decodeErr)
	}
	_ = blockResp.Body.Close()

	// Simulate Jetstream indexing: insert block record into AppView repo
	_, repoErr := env.Repo.BlockUser(ctx, &userblocks.UserBlock{
		BlockerDID: blockerDID,
		BlockedDID: targetDID,
		BlockedAt:  time.Now(),
		RecordURI:  blockResult.Block.RecordURI,
		RecordCID:  blockResult.Block.RecordCID,
	})
	if repoErr != nil {
		t.Fatalf("Failed to index block: %v", repoErr)
	}

	// Verify block exists before unblock
	isBlocked, err := env.Repo.IsBlocked(ctx, blockerDID, targetDID)
	if err != nil {
		t.Fatalf("Failed to check block: %v", err)
	}
	if !isBlocked {
		t.Fatal("Expected block to exist before unblock")
	}

	// Now unblock
	unblockResp := postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.unblockUser", token, map[string]string{
		"subject": targetDID,
	})
	defer func() { _ = unblockResp.Body.Close() }()

	if unblockResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(unblockResp.Body)
		t.Fatalf("Expected 200, got %d: %s", unblockResp.StatusCode, string(body))
	}

	var unblockResult struct {
		Success bool `json:"success"`
	}
	if decodeErr := json.NewDecoder(unblockResp.Body).Decode(&unblockResult); decodeErr != nil {
		t.Fatalf("Failed to decode unblock response: %v", decodeErr)
	}

	if !unblockResult.Success {
		t.Error("Expected success: true in unblock response")
	}

	// Verify PDS DeleteRecord was actually called
	mockClient := env.PDSTracker.ClientFor(blockerDID)
	if mockClient == nil {
		t.Fatal("Expected mock PDS client to exist for blocker")
	}
	if mockClient.DeleteCallCount() == 0 {
		t.Error("Expected PDS DeleteRecord to be called during unblock, but it was never called")
	}

	// Simulate Jetstream processing the delete and verify block removal from AppView.
	// In production, the Jetstream consumer removes the block from the repo.
	// Here we manually remove it to verify the full flow.
	if removeErr := env.Repo.UnblockUser(ctx, blockerDID, targetDID); removeErr != nil {
		t.Fatalf("Failed to remove block from repo: %v", removeErr)
	}

	isBlockedAfter, checkErr := env.Repo.IsBlocked(ctx, blockerDID, targetDID)
	if checkErr != nil {
		t.Fatalf("Failed to check block after unblock: %v", checkErr)
	}
	if isBlockedAfter {
		t.Error("Expected block to be removed from AppView after unblock")
	}
}

// TestUserBlockHandler_GetBlockedUsers tests listing blocked users
func TestUserBlockHandler_GetBlockedUsers(t *testing.T) {
	env := setupUserBlockTestServer(t)

	blockerDID := "did:plc:lister1"
	target1DID := "did:plc:listed1"
	target2DID := "did:plc:listed2"
	token := env.Auth.AddUser(blockerDID)

	ctx := context.Background()

	// Index two blocks directly in the repo (simulating Jetstream indexing)
	_, err := env.Repo.BlockUser(ctx, &userblocks.UserBlock{
		BlockerDID: blockerDID,
		BlockedDID: target1DID,
		BlockedAt:  time.Now().Add(-1 * time.Minute),
		RecordURI:  fmt.Sprintf("at://%s/social.coves.actor.block/tid1", blockerDID),
		RecordCID:  "bafyblock1",
	})
	if err != nil {
		t.Fatalf("Failed to create block 1: %v", err)
	}

	_, err = env.Repo.BlockUser(ctx, &userblocks.UserBlock{
		BlockerDID: blockerDID,
		BlockedDID: target2DID,
		BlockedAt:  time.Now(),
		RecordURI:  fmt.Sprintf("at://%s/social.coves.actor.block/tid2", blockerDID),
		RecordCID:  "bafyblock2",
	})
	if err != nil {
		t.Fatalf("Failed to create block 2: %v", err)
	}

	resp := getXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.getBlockedUsers?limit=10", token)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	var listResp struct {
		Blocks []struct {
			BlockedDID string `json:"blockedDid"`
			RecordURI  string `json:"recordUri"`
			RecordCID  string `json:"recordCid"`
		} `json:"blocks"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&listResp); decodeErr != nil {
		t.Fatalf("Failed to decode list response: %v", decodeErr)
	}

	if len(listResp.Blocks) != 2 {
		t.Fatalf("Expected 2 blocks, got %d", len(listResp.Blocks))
	}

	// Verify both target DIDs are present
	foundDIDs := make(map[string]bool)
	for _, b := range listResp.Blocks {
		foundDIDs[b.BlockedDID] = true
		if b.RecordURI == "" {
			t.Error("Expected non-empty recordUri")
		}
		if b.RecordCID == "" {
			t.Error("Expected non-empty recordCid")
		}
	}

	if !foundDIDs[target1DID] {
		t.Errorf("Expected %s in blocked list", target1DID)
	}
	if !foundDIDs[target2DID] {
		t.Errorf("Expected %s in blocked list", target2DID)
	}
}

// TestUserBlockHandler_GetBlockedUsers_Unauthenticated tests that getBlockedUsers requires auth
func TestUserBlockHandler_GetBlockedUsers_Unauthenticated(t *testing.T) {
	env := setupUserBlockTestServer(t)

	// No token — unauthenticated request
	resp := getXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.getBlockedUsers", "")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 401, got %d: %s", resp.StatusCode, string(body))
	}
}

// TestUserBlockHandler_BlockUser_DuplicateConflict tests that blocking a user who is
// already blocked on PDS returns the existing block (via repo lookup) or 409 Conflict.
func TestUserBlockHandler_BlockUser_DuplicateConflict(t *testing.T) {
	env := setupUserBlockTestServer(t)

	blockerDID := "did:plc:conflict-blocker"
	targetDID := "did:plc:conflict-target"
	token := env.Auth.AddUser(blockerDID)

	ctx := context.Background()

	// Pre-index an existing block in the AppView repo (as if Jetstream already processed it)
	existingURI := fmt.Sprintf("at://%s/social.coves.actor.block/existing1", blockerDID)
	existingCID := "bafyexistingcid"
	_, err := env.Repo.BlockUser(ctx, &userblocks.UserBlock{
		BlockerDID: blockerDID,
		BlockedDID: targetDID,
		BlockedAt:  time.Now(),
		RecordURI:  existingURI,
		RecordCID:  existingCID,
	})
	if err != nil {
		t.Fatalf("Failed to pre-index block: %v", err)
	}

	// Configure mock PDS to return conflict error (simulating duplicate record on PDS)
	mockClient := env.PDSTracker.ClientFor(blockerDID)
	if mockClient == nil {
		// Trigger client creation by making any authenticated request first
		_ = postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", token, map[string]string{
			"subject": "did:plc:dummy",
		})
		mockClient = env.PDSTracker.ClientFor(blockerDID)
	}
	mockClient.mu.Lock()
	mockClient.createErr = fmt.Errorf("conflict: %w", pds.ErrConflict)
	mockClient.mu.Unlock()

	// Attempt to block the same user again — PDS returns conflict
	resp := postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", token, map[string]string{
		"subject": targetDID,
	})
	defer func() { _ = resp.Body.Close() }()

	// The service should find the existing block in the repo and return it
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200 (existing block returned), got %d: %s", resp.StatusCode, string(body))
	}

	var blockResp struct {
		Block struct {
			RecordURI string `json:"recordUri"`
			RecordCID string `json:"recordCid"`
		} `json:"block"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&blockResp); decodeErr != nil {
		t.Fatalf("Failed to decode response: %v", decodeErr)
	}

	if blockResp.Block.RecordURI != existingURI {
		t.Errorf("Expected existing RecordURI=%s, got %s", existingURI, blockResp.Block.RecordURI)
	}
	if blockResp.Block.RecordCID != existingCID {
		t.Errorf("Expected existing RecordCID=%s, got %s", existingCID, blockResp.Block.RecordCID)
	}
}

// TestUserBlockHandler_BlockUser_DuplicateConflict_NotIndexed tests the 409 path when
// PDS returns conflict but the block hasn't been indexed in AppView yet.
func TestUserBlockHandler_BlockUser_DuplicateConflict_NotIndexed(t *testing.T) {
	env := setupUserBlockTestServer(t)

	blockerDID := "did:plc:conflict-noindex-blocker"
	targetDID := "did:plc:conflict-noindex-target"
	token := env.Auth.AddUser(blockerDID)

	// Force mock PDS client creation, then set conflict error
	_ = postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", token, map[string]string{
		"subject": "did:plc:warmup",
	})
	mockClient := env.PDSTracker.ClientFor(blockerDID)
	mockClient.mu.Lock()
	mockClient.createErr = fmt.Errorf("conflict: %w", pds.ErrConflict)
	mockClient.mu.Unlock()

	// Block target — PDS says conflict, but repo has no block → should return 409
	resp := postXRPC(t, env.Server.URL, "/xrpc/social.coves.actor.blockUser", token, map[string]string{
		"subject": targetDID,
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 409, got %d: %s", resp.StatusCode, string(body))
	}

	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&errResp); decodeErr != nil {
		t.Fatalf("Failed to decode error response: %v", decodeErr)
	}

	if errResp.Error != "AlreadyExists" {
		t.Errorf("Expected error code 'AlreadyExists', got %q", errResp.Error)
	}
}
