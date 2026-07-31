package userblocks

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
)

// --- Mock Repository ---

type mockRepo struct {
	blockUserFn     func(ctx context.Context, block *UserBlock) (*UserBlock, error)
	unblockUserFn   func(ctx context.Context, blockerDID, blockedDID string) error
	getBlockFn      func(ctx context.Context, blockerDID, blockedDID string) (*UserBlock, error)
	getBlockByURIFn func(ctx context.Context, recordURI string) (*UserBlock, error)
	listBlockedFn   func(ctx context.Context, blockerDID string, limit, offset int) ([]*UserBlock, error)
	isBlockedFn     func(ctx context.Context, blockerDID, blockedDID string) (bool, error)
	areBlockedFn    func(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error)
}

func (m *mockRepo) BlockUser(ctx context.Context, block *UserBlock) (*UserBlock, error) {
	if m.blockUserFn != nil {
		return m.blockUserFn(ctx, block)
	}
	return block, nil
}

func (m *mockRepo) UnblockUser(ctx context.Context, blockerDID, blockedDID string) error {
	if m.unblockUserFn != nil {
		return m.unblockUserFn(ctx, blockerDID, blockedDID)
	}
	return nil
}

func (m *mockRepo) GetBlock(ctx context.Context, blockerDID, blockedDID string) (*UserBlock, error) {
	if m.getBlockFn != nil {
		return m.getBlockFn(ctx, blockerDID, blockedDID)
	}
	return nil, ErrBlockNotFound
}

func (m *mockRepo) GetBlockByURI(ctx context.Context, recordURI string) (*UserBlock, error) {
	if m.getBlockByURIFn != nil {
		return m.getBlockByURIFn(ctx, recordURI)
	}
	return nil, ErrBlockNotFound
}

func (m *mockRepo) ListBlockedUsers(ctx context.Context, blockerDID string, limit, offset int) ([]*UserBlock, error) {
	if m.listBlockedFn != nil {
		return m.listBlockedFn(ctx, blockerDID, limit, offset)
	}
	return nil, nil
}

func (m *mockRepo) IsBlocked(ctx context.Context, blockerDID, blockedDID string) (bool, error) {
	if m.isBlockedFn != nil {
		return m.isBlockedFn(ctx, blockerDID, blockedDID)
	}
	return false, nil
}

func (m *mockRepo) AreBlocked(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error) {
	if m.areBlockedFn != nil {
		return m.areBlockedFn(ctx, blockerDID, blockedDIDs)
	}
	return make(map[string]bool), nil
}

// --- Mock Handle Resolver ---

type mockHandleResolver struct {
	resolveHandleToDIDFn func(ctx context.Context, handle string) (string, error)
}

func (m *mockHandleResolver) ResolveHandleToDID(ctx context.Context, handle string) (string, error) {
	if m.resolveHandleToDIDFn != nil {
		return m.resolveHandleToDIDFn(ctx, handle)
	}
	return "", errors.New("not implemented")
}

// --- Mock PDS Client ---

type mockPDSClient struct {
	createRecordFn func(ctx context.Context, collection, rkey string, record any) (string, string, error)
	deleteRecordFn func(ctx context.Context, collection, rkey string) error
}

func (m *mockPDSClient) CreateRecord(ctx context.Context, collection, rkey string, record any) (string, string, error) {
	if m.createRecordFn != nil {
		return m.createRecordFn(ctx, collection, rkey, record)
	}
	return "at://did:plc:test/social.coves.actor.block/" + rkey, "bafyreicid", nil
}

func (m *mockPDSClient) DeleteRecord(ctx context.Context, collection, rkey string) error {
	if m.deleteRecordFn != nil {
		return m.deleteRecordFn(ctx, collection, rkey)
	}
	return nil
}

func (m *mockPDSClient) GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	return nil, nil
}

func (m *mockPDSClient) ListRecords(ctx context.Context, collection string, limit int, cursor string) (*pds.ListRecordsResponse, error) {
	return nil, nil
}

func (m *mockPDSClient) PutRecord(ctx context.Context, collection string, rkey string, record any, swapRecord string) (string, string, error) {
	return "", "", nil
}

func (m *mockPDSClient) UploadBlob(ctx context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	return nil, nil
}

func (m *mockPDSClient) DID() string {
	return "did:plc:mock"
}

// mockHostURL is the PDS address the mock client and the fake session report.
//
// Nothing here connects to it: the service under test is built with
// NewServiceWithPDSFactory and a factory that hands back mockPDSClient, so
// every record write is a method call on a struct in this file. The value only
// has to be a well-formed host URL for the session to look real.
const mockHostURL = "http://localhost:3001" // coves:allow-host-literal: value reported by a mock pds.Client and a fake OAuth session; no client is constructed from it.

func (m *mockPDSClient) HostURL() string {
	return mockHostURL
}

// --- Helper to create a test session ---

func testSession(did string) *oauth.ClientSessionData {
	parsedDID, _ := syntax.ParseDID(did)
	return &oauth.ClientSessionData{
		AccountDID:  parsedDID,
		AccessToken: "test-token",
		HostURL:     mockHostURL,
	}
}

// --- Tests ---

func TestBlockUser_PreventsSelfBlock(t *testing.T) {
	repo := &mockRepo{}
	userSvc := &mockHandleResolver{}
	pdsClient := &mockPDSClient{}

	factory := func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	}

	svc := NewServiceWithPDSFactory(repo, userSvc, factory)

	selfDID := "did:plc:selfuser123"
	session := testSession(selfDID)

	// Blocking yourself should return ErrCannotBlockSelf
	_, err := svc.BlockUser(context.Background(), session, selfDID)
	if err == nil {
		t.Fatal("expected error when blocking self, got nil")
	}
	if !errors.Is(err, ErrCannotBlockSelf) {
		t.Fatalf("expected ErrCannotBlockSelf, got: %v", err)
	}
}

func TestBlockUser_ResolvesHandleToDID(t *testing.T) {
	resolvedDID := "did:plc:bobresolved"
	var createCalledWithCollection string
	var createCalledWithSubject string

	repo := &mockRepo{}
	userSvc := &mockHandleResolver{
		resolveHandleToDIDFn: func(ctx context.Context, handle string) (string, error) {
			if handle != "bob.bsky.social" {
				t.Fatalf("expected handle bob.bsky.social, got %s", handle)
			}
			return resolvedDID, nil
		},
	}
	pdsClient := &mockPDSClient{
		createRecordFn: func(ctx context.Context, collection, rkey string, record interface{}) (string, string, error) {
			createCalledWithCollection = collection
			// Extract subject from record map
			if rec, ok := record.(map[string]interface{}); ok {
				if sub, ok := rec["subject"].(string); ok {
					createCalledWithSubject = sub
				}
			}
			uri := "at://did:plc:alice123/social.coves.actor.block/" + rkey
			return uri, "bafyreicid123", nil
		},
	}

	factory := func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	}

	svc := NewServiceWithPDSFactory(repo, userSvc, factory)

	session := testSession("did:plc:alice123")

	result, err := svc.BlockUser(context.Background(), session, "bob.bsky.social")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if createCalledWithSubject != resolvedDID {
		t.Fatalf("expected PDS record subject %s, got %s", resolvedDID, createCalledWithSubject)
	}
	if createCalledWithCollection != "social.coves.actor.block" {
		t.Fatalf("expected collection social.coves.actor.block, got %s", createCalledWithCollection)
	}
	if result.RecordURI == "" {
		t.Fatal("expected non-empty record URI")
	}
	if result.RecordCID == "" {
		t.Fatal("expected non-empty record CID")
	}
}

func TestBlockUser_AcceptsDIDDirectly(t *testing.T) {
	targetDID := "did:plc:target456"
	resolveHandleCalled := false

	repo := &mockRepo{}
	userSvc := &mockHandleResolver{
		resolveHandleToDIDFn: func(ctx context.Context, handle string) (string, error) {
			resolveHandleCalled = true
			return "", errors.New("should not be called")
		},
	}
	pdsClient := &mockPDSClient{}

	factory := func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	}

	svc := NewServiceWithPDSFactory(repo, userSvc, factory)

	session := testSession("did:plc:alice123")

	result, err := svc.BlockUser(context.Background(), session, targetDID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolveHandleCalled {
		t.Fatal("ResolveHandleToDID should not be called for DID identifiers")
	}
	if result.RecordURI == "" {
		t.Fatal("expected non-empty record URI")
	}
}

func TestBlockUser_NilSession(t *testing.T) {
	repo := &mockRepo{}
	userSvc := &mockHandleResolver{}

	factory := func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	}

	svc := NewServiceWithPDSFactory(repo, userSvc, factory)

	_, err := svc.BlockUser(context.Background(), nil, "did:plc:target")
	if err == nil {
		t.Fatal("expected error for nil session, got nil")
	}
}

func TestGetBlockedUsers_DefaultPagination(t *testing.T) {
	tests := []struct {
		name          string
		inputLimit    int
		expectedLimit int
	}{
		{"zero limit defaults to 50", 0, 50},
		{"negative limit defaults to 50", -1, 50},
		{"over 100 capped to 100", 200, 100},
		{"valid limit passes through", 25, 25},
		{"exactly 100 passes through", 100, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedLimit int

			repo := &mockRepo{
				listBlockedFn: func(ctx context.Context, blockerDID string, limit, offset int) ([]*UserBlock, error) {
					capturedLimit = limit
					return nil, nil
				},
			}
			userSvc := &mockHandleResolver{}

			factory := func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
				return &mockPDSClient{}, nil
			}

			svc := NewServiceWithPDSFactory(repo, userSvc, factory)

			_, err := svc.GetBlockedUsers(context.Background(), "did:plc:user123", tt.inputLimit, 0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if capturedLimit != tt.expectedLimit {
				t.Fatalf("expected limit %d, got %d", tt.expectedLimit, capturedLimit)
			}
		})
	}
}

func TestIsBlocked_DelegatesToRepo(t *testing.T) {
	blockerDID := "did:plc:alice"
	blockedDID := "did:plc:bob"
	isBlockedCalled := false

	repo := &mockRepo{
		isBlockedFn: func(ctx context.Context, blocker, blocked string) (bool, error) {
			isBlockedCalled = true
			if blocker != blockerDID {
				t.Fatalf("expected blocker %s, got %s", blockerDID, blocker)
			}
			if blocked != blockedDID {
				t.Fatalf("expected blocked %s, got %s", blockedDID, blocked)
			}
			return true, nil
		},
	}
	userSvc := &mockHandleResolver{}

	factory := func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	}

	svc := NewServiceWithPDSFactory(repo, userSvc, factory)

	blocked, err := svc.IsBlocked(context.Background(), blockerDID, blockedDID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !isBlockedCalled {
		t.Fatal("expected repo.IsBlocked to be called")
	}
	if !blocked {
		t.Fatal("expected blocked=true")
	}
}

func TestUnblockUser_ExtractsRKeyAndDeletesFromPDS(t *testing.T) {
	blockerDID := "did:plc:alice"
	blockedDID := "did:plc:bob"
	expectedRKey := "3lawvb5hii22f"
	recordURI := "at://" + blockerDID + "/social.coves.actor.block/" + expectedRKey

	var deletedCollection, deletedRKey string

	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			return &UserBlock{
				BlockerDID: blocker,
				BlockedDID: blocked,
				RecordURI:  recordURI,
				RecordCID:  "bafyreicid",
			}, nil
		},
	}
	userSvc := &mockHandleResolver{}
	pdsClient := &mockPDSClient{
		deleteRecordFn: func(ctx context.Context, collection, rkey string) error {
			deletedCollection = collection
			deletedRKey = rkey
			return nil
		},
	}

	factory := func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	}

	svc := NewServiceWithPDSFactory(repo, userSvc, factory)

	session := testSession(blockerDID)

	err := svc.UnblockUser(context.Background(), session, blockedDID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if deletedCollection != "social.coves.actor.block" {
		t.Fatalf("expected collection social.coves.actor.block, got %s", deletedCollection)
	}
	if deletedRKey != expectedRKey {
		t.Fatalf("expected rkey %s, got %s", expectedRKey, deletedRKey)
	}
}

func TestBlockUser_EmptyIdentifier(t *testing.T) {
	svc := NewServiceWithPDSFactory(&mockRepo{}, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	session := testSession("did:plc:alice123")

	_, err := svc.BlockUser(context.Background(), session, "")
	if err == nil {
		t.Fatal("expected error for empty identifier, got nil")
	}
	if !strings.Contains(err.Error(), "identifier is required") {
		t.Fatalf("expected 'identifier is required' error, got: %v", err)
	}
}

func TestBlockUser_WhitespaceIdentifier(t *testing.T) {
	svc := NewServiceWithPDSFactory(&mockRepo{}, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	session := testSession("did:plc:alice123")

	_, err := svc.BlockUser(context.Background(), session, "   ")
	if err == nil {
		t.Fatal("expected error for whitespace identifier, got nil")
	}
	if !strings.Contains(err.Error(), "identifier is required") {
		t.Fatalf("expected 'identifier is required' error, got: %v", err)
	}
}

func TestUnblockUser_EmptyIdentifier(t *testing.T) {
	svc := NewServiceWithPDSFactory(&mockRepo{}, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	session := testSession("did:plc:alice123")

	err := svc.UnblockUser(context.Background(), session, "")
	if err == nil {
		t.Fatal("expected error for empty identifier, got nil")
	}
	if !strings.Contains(err.Error(), "identifier is required") {
		t.Fatalf("expected 'identifier is required' error, got: %v", err)
	}
}

func TestBlockUser_InvalidDIDFormat(t *testing.T) {
	svc := NewServiceWithPDSFactory(&mockRepo{}, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	session := testSession("did:plc:alice123")

	// "did:" alone should fail DID validation
	_, err := svc.BlockUser(context.Background(), session, "did:")
	if err == nil {
		t.Fatal("expected error for bare 'did:' identifier, got nil")
	}

	// "did:garbage" should fail DID validation
	_, err = svc.BlockUser(context.Background(), session, "did:garbage")
	if err == nil {
		t.Fatal("expected error for malformed DID, got nil")
	}
}

func TestGetBlockedUsers_NegativeOffsetClamped(t *testing.T) {
	var capturedOffset int

	repo := &mockRepo{
		listBlockedFn: func(ctx context.Context, blockerDID string, limit, offset int) ([]*UserBlock, error) {
			capturedOffset = offset
			return nil, nil
		},
	}

	svc := NewServiceWithPDSFactory(repo, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	_, err := svc.GetBlockedUsers(context.Background(), "did:plc:user123", 50, -5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedOffset != 0 {
		t.Fatalf("expected offset clamped to 0, got %d", capturedOffset)
	}
}

func TestUnblockUser_BlockNotFound(t *testing.T) {
	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			return nil, ErrBlockNotFound
		},
	}
	userSvc := &mockHandleResolver{}

	factory := func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	}

	svc := NewServiceWithPDSFactory(repo, userSvc, factory)

	session := testSession("did:plc:alice")

	err := svc.UnblockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error for non-existent block")
	}
	if !errors.Is(err, ErrBlockNotFound) {
		t.Fatalf("expected ErrBlockNotFound, got: %v", err)
	}
}

// --- Critical: PDS conflict/idempotency tests (service.go lines 133-145) ---

func TestBlockUser_PDSConflict_RepoHasBlock(t *testing.T) {
	// When PDS returns conflict and the repo already has the block,
	// service should return the existing block result (no error).
	existingURI := "at://did:plc:alice/social.coves.actor.block/existing123"
	existingCID := "bafyexisting"

	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			return &UserBlock{
				BlockerDID: blocker,
				BlockedDID: blocked,
				RecordURI:  existingURI,
				RecordCID:  existingCID,
			}, nil
		},
	}
	pdsClient := &mockPDSClient{
		createRecordFn: func(ctx context.Context, collection, rkey string, record any) (string, string, error) {
			return "", "", pds.ErrConflict
		},
	}

	svc := NewServiceWithPDSFactory(repo, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	})

	session := testSession("did:plc:alice")
	result, err := svc.BlockUser(context.Background(), session, "did:plc:bob")
	if err != nil {
		t.Fatalf("expected no error for idempotent block, got: %v", err)
	}
	if result.RecordURI != existingURI {
		t.Errorf("expected RecordURI=%s, got %s", existingURI, result.RecordURI)
	}
	if result.RecordCID != existingCID {
		t.Errorf("expected RecordCID=%s, got %s", existingCID, result.RecordCID)
	}
}

func TestBlockUser_PDSConflict_RepoNotFound(t *testing.T) {
	// When PDS returns conflict but the repo doesn't have the block,
	// service should return ErrBlockAlreadyExists.
	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			return nil, ErrBlockNotFound
		},
	}
	pdsClient := &mockPDSClient{
		createRecordFn: func(ctx context.Context, collection, rkey string, record any) (string, string, error) {
			return "", "", pds.ErrConflict
		},
	}

	svc := NewServiceWithPDSFactory(repo, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	})

	session := testSession("did:plc:alice")
	_, err := svc.BlockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error when PDS conflict and repo has no block")
	}
	if !errors.Is(err, ErrBlockAlreadyExists) {
		t.Fatalf("expected ErrBlockAlreadyExists, got: %v", err)
	}
}

func TestBlockUser_PDSConflict_RepoError(t *testing.T) {
	// When PDS returns conflict and the repo returns a non-NotFound error,
	// service should wrap and return that error.
	repoErr := errors.New("database connection lost")
	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			return nil, repoErr
		},
	}
	pdsClient := &mockPDSClient{
		createRecordFn: func(ctx context.Context, collection, rkey string, record any) (string, string, error) {
			return "", "", pds.ErrConflict
		},
	}

	svc := NewServiceWithPDSFactory(repo, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	})

	session := testSession("did:plc:alice")
	_, err := svc.BlockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error when repo fails during conflict handling")
	}
	if !errors.Is(err, repoErr) {
		t.Fatalf("expected wrapped repo error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "PDS reported duplicate block") {
		t.Fatalf("expected error message to mention PDS duplicate, got: %v", err)
	}
}

// --- Important: Auth error tests ---

func TestBlockUser_PDSAuthError(t *testing.T) {
	pdsClient := &mockPDSClient{
		createRecordFn: func(ctx context.Context, collection, rkey string, record any) (string, string, error) {
			return "", "", pds.ErrUnauthorized
		},
	}

	svc := NewServiceWithPDSFactory(&mockRepo{}, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	})

	session := testSession("did:plc:alice")
	_, err := svc.BlockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error for auth failure")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("expected unauthorized error, got: %v", err)
	}
	if !errors.Is(err, pds.ErrUnauthorized) {
		t.Fatalf("expected wrapped ErrUnauthorized, got: %v", err)
	}
}

func TestUnblockUser_PDSAuthError(t *testing.T) {
	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			return &UserBlock{
				BlockerDID: blocker,
				BlockedDID: blocked,
				RecordURI:  "at://did:plc:alice/social.coves.actor.block/rkey123",
				RecordCID:  "bafycid",
			}, nil
		},
	}
	pdsClient := &mockPDSClient{
		deleteRecordFn: func(ctx context.Context, collection, rkey string) error {
			return pds.ErrUnauthorized
		},
	}

	svc := NewServiceWithPDSFactory(repo, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	})

	session := testSession("did:plc:alice")
	err := svc.UnblockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error for auth failure on unblock")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("expected unauthorized error, got: %v", err)
	}
	if !errors.Is(err, pds.ErrUnauthorized) {
		t.Fatalf("expected wrapped ErrUnauthorized, got: %v", err)
	}
}

// --- Important: UnblockUser nil session ---

func TestUnblockUser_NilSession(t *testing.T) {
	svc := NewServiceWithPDSFactory(&mockRepo{}, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	err := svc.UnblockUser(context.Background(), nil, "did:plc:target")
	if err == nil {
		t.Fatal("expected error for nil session, got nil")
	}
	if !strings.Contains(err.Error(), "session is required") {
		t.Fatalf("expected 'session is required' error, got: %v", err)
	}
}

// --- Important: UnblockUser invalid record URI (empty rkey) ---

func TestUnblockUser_InvalidRecordURI(t *testing.T) {
	// When the stored block has a malformed URI that yields an empty rkey,
	// service should return an error instead of sending an empty rkey to PDS.
	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			return &UserBlock{
				BlockerDID: blocker,
				BlockedDID: blocked,
				RecordURI:  "at://bad", // too few segments → ExtractRKeyFromURI returns ""
				RecordCID:  "bafycid",
			}, nil
		},
	}

	svc := NewServiceWithPDSFactory(repo, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	session := testSession("did:plc:alice")
	err := svc.UnblockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error for invalid record URI")
	}
	if !strings.Contains(err.Error(), "invalid block record URI") {
		t.Fatalf("expected 'invalid block record URI' error, got: %v", err)
	}
}

// --- Moderate: Handle resolution failure ---

func TestBlockUser_HandleResolutionFailure(t *testing.T) {
	resolver := &mockHandleResolver{
		resolveHandleToDIDFn: func(ctx context.Context, handle string) (string, error) {
			return "", errors.New("handle not found")
		},
	}

	svc := NewServiceWithPDSFactory(&mockRepo{}, resolver, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	session := testSession("did:plc:alice")
	_, err := svc.BlockUser(context.Background(), session, "nonexistent.bsky.social")
	if err == nil {
		t.Fatal("expected error for handle resolution failure")
	}
	if !strings.Contains(err.Error(), "handle not found") {
		t.Fatalf("expected handle resolution error, got: %v", err)
	}
}

// --- Moderate: PDS client factory error ---

func TestBlockUser_PDSClientFactoryError(t *testing.T) {
	svc := NewServiceWithPDSFactory(&mockRepo{}, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return nil, errors.New("failed to create PDS client")
	})

	session := testSession("did:plc:alice")
	_, err := svc.BlockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error for PDS client factory failure")
	}
	if !strings.Contains(err.Error(), "failed to create PDS client") {
		t.Fatalf("expected PDS client factory error, got: %v", err)
	}
}

// --- Moderate: Generic PDS errors (non-auth, non-conflict) ---

func TestBlockUser_GenericPDSError(t *testing.T) {
	pdsClient := &mockPDSClient{
		createRecordFn: func(ctx context.Context, collection, rkey string, record any) (string, string, error) {
			return "", "", pds.ErrRateLimited
		},
	}

	svc := NewServiceWithPDSFactory(&mockRepo{}, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	})

	session := testSession("did:plc:alice")
	_, err := svc.BlockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error for generic PDS failure")
	}
	if !strings.Contains(err.Error(), "failed to create block on PDS") {
		t.Fatalf("expected 'failed to create block on PDS' error, got: %v", err)
	}
	if !errors.Is(err, pds.ErrRateLimited) {
		t.Fatalf("expected wrapped ErrRateLimited, got: %v", err)
	}
}

func TestUnblockUser_GenericPDSError(t *testing.T) {
	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			return &UserBlock{
				BlockerDID: blocker,
				BlockedDID: blocked,
				RecordURI:  "at://did:plc:alice/social.coves.actor.block/rkey123",
				RecordCID:  "bafycid",
			}, nil
		},
	}
	pdsClient := &mockPDSClient{
		deleteRecordFn: func(ctx context.Context, collection, rkey string) error {
			return pds.ErrRateLimited
		},
	}

	svc := NewServiceWithPDSFactory(repo, &mockHandleResolver{}, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	})

	session := testSession("did:plc:alice")
	err := svc.UnblockUser(context.Background(), session, "did:plc:bob")
	if err == nil {
		t.Fatal("expected error for generic PDS failure on unblock")
	}
	if !strings.Contains(err.Error(), "failed to delete block on PDS") {
		t.Fatalf("expected 'failed to delete block on PDS' error, got: %v", err)
	}
	if !errors.Is(err, pds.ErrRateLimited) {
		t.Fatalf("expected wrapped ErrRateLimited, got: %v", err)
	}
}

// --- Moderate: UnblockUser handle resolution ---

func TestUnblockUser_ResolvesHandleToDID(t *testing.T) {
	resolvedDID := "did:plc:bobresolved"
	var deletedRKey string

	resolver := &mockHandleResolver{
		resolveHandleToDIDFn: func(ctx context.Context, handle string) (string, error) {
			if handle != "bob.bsky.social" {
				t.Fatalf("expected handle bob.bsky.social, got %s", handle)
			}
			return resolvedDID, nil
		},
	}
	repo := &mockRepo{
		getBlockFn: func(ctx context.Context, blocker, blocked string) (*UserBlock, error) {
			if blocked != resolvedDID {
				t.Fatalf("expected resolved DID %s in repo lookup, got %s", resolvedDID, blocked)
			}
			return &UserBlock{
				BlockerDID: blocker,
				BlockedDID: blocked,
				RecordURI:  "at://did:plc:alice/social.coves.actor.block/rkey456",
				RecordCID:  "bafycid456",
			}, nil
		},
	}
	pdsClient := &mockPDSClient{
		deleteRecordFn: func(ctx context.Context, collection, rkey string) error {
			deletedRKey = rkey
			return nil
		},
	}

	svc := NewServiceWithPDSFactory(repo, resolver, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return pdsClient, nil
	})

	session := testSession("did:plc:alice")
	err := svc.UnblockUser(context.Background(), session, "bob.bsky.social")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deletedRKey != "rkey456" {
		t.Fatalf("expected rkey=rkey456, got %s", deletedRKey)
	}
}

func TestUnblockUser_HandleResolutionFailure(t *testing.T) {
	resolver := &mockHandleResolver{
		resolveHandleToDIDFn: func(ctx context.Context, handle string) (string, error) {
			return "", errors.New("handle not found")
		},
	}

	svc := NewServiceWithPDSFactory(&mockRepo{}, resolver, func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
		return &mockPDSClient{}, nil
	})

	session := testSession("did:plc:alice")
	err := svc.UnblockUser(context.Background(), session, "nonexistent.bsky.social")
	if err == nil {
		t.Fatal("expected error for handle resolution failure on unblock")
	}
	if !strings.Contains(err.Error(), "handle not found") {
		t.Fatalf("expected handle resolution error, got: %v", err)
	}
}
