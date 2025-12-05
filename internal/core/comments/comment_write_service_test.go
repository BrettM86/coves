package comments

import (
	"Coves/internal/atproto/pds"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// ================================================================================
// Mock PDS Client for Write Operations Testing
// ================================================================================

// mockPDSClient implements the pds.Client interface for testing
// It stores records in memory and allows simulating various PDS error conditions
type mockPDSClient struct {
	records     map[string]map[string]interface{} // collection -> rkey -> record
	createError error                             // Error to return on CreateRecord
	getError    error                             // Error to return on GetRecord
	deleteError error                             // Error to return on DeleteRecord
	putError    error                             // Error to return on PutRecord
	did         string                            // DID of the authenticated user
	hostURL     string                            // PDS host URL
}

// newMockPDSClient creates a new mock PDS client for testing
func newMockPDSClient(did string) *mockPDSClient {
	return &mockPDSClient{
		records: make(map[string]map[string]interface{}),
		did:     did,
		hostURL: "https://pds.test.local",
	}
}

func (m *mockPDSClient) DID() string {
	return m.did
}

func (m *mockPDSClient) HostURL() string {
	return m.hostURL
}

func (m *mockPDSClient) CreateRecord(ctx context.Context, collection, rkey string, record interface{}) (string, string, error) {
	if m.createError != nil {
		return "", "", m.createError
	}

	// Generate rkey if not provided
	if rkey == "" {
		rkey = fmt.Sprintf("test_%d", time.Now().UnixNano())
	}

	// Store record
	if m.records[collection] == nil {
		m.records[collection] = make(map[string]interface{})
	}
	m.records[collection][rkey] = record

	// Generate response
	uri := fmt.Sprintf("at://%s/%s/%s", m.did, collection, rkey)
	cid := fmt.Sprintf("bafytest%d", time.Now().UnixNano())

	return uri, cid, nil
}

func (m *mockPDSClient) GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	if m.getError != nil {
		return nil, m.getError
	}

	if m.records[collection] == nil {
		return nil, pds.ErrNotFound
	}

	record, ok := m.records[collection][rkey]
	if !ok {
		return nil, pds.ErrNotFound
	}

	uri := fmt.Sprintf("at://%s/%s/%s", m.did, collection, rkey)
	cid := fmt.Sprintf("bafytest%d", time.Now().UnixNano())

	return &pds.RecordResponse{
		URI:   uri,
		CID:   cid,
		Value: record.(map[string]interface{}),
	}, nil
}

func (m *mockPDSClient) DeleteRecord(ctx context.Context, collection, rkey string) error {
	if m.deleteError != nil {
		return m.deleteError
	}

	if m.records[collection] == nil {
		return pds.ErrNotFound
	}

	if _, ok := m.records[collection][rkey]; !ok {
		return pds.ErrNotFound
	}

	delete(m.records[collection], rkey)
	return nil
}

func (m *mockPDSClient) ListRecords(ctx context.Context, collection string, limit int, cursor string) (*pds.ListRecordsResponse, error) {
	return &pds.ListRecordsResponse{}, nil
}

func (m *mockPDSClient) PutRecord(ctx context.Context, collection, rkey string, record any, swapRecord string) (string, string, error) {
	if m.putError != nil {
		return "", "", m.putError
	}

	// Store record (same logic as CreateRecord)
	if m.records[collection] == nil {
		m.records[collection] = make(map[string]interface{})
	}
	m.records[collection][rkey] = record

	uri := fmt.Sprintf("at://%s/%s/%s", m.did, collection, rkey)
	cid := fmt.Sprintf("bafytest%d", time.Now().UnixNano())
	return uri, cid, nil
}

// mockPDSClientFactory creates mock PDS clients for testing
type mockPDSClientFactory struct {
	client *mockPDSClient
	err    error
}

func (f *mockPDSClientFactory) create(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.client == nil {
		f.client = newMockPDSClient(session.AccountDID.String())
	}
	return f.client, nil
}

// ================================================================================
// Helper Functions
// ================================================================================

// createTestSession creates a test OAuth session for a given DID
func createTestSession(did string) *oauth.ClientSessionData {
	parsedDID, _ := syntax.ParseDID(did)
	return &oauth.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   "test-session-123",
		AccessToken: "test-access-token",
		HostURL:     "https://pds.test.local",
	}
}

// ================================================================================
// CreateComment Tests
// ================================================================================

func TestCreateComment_Success(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	// Create request
	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: "This is a test comment",
		Langs:   []string{"en"},
	}

	session := createTestSession("did:plc:test123")

	// Execute
	resp, err := service.CreateComment(ctx, session, req)

	// Verify
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if resp == nil {
		t.Fatal("Expected response, got nil")
	}
	if resp.URI == "" {
		t.Error("Expected URI to be set")
	}
	if resp.CID == "" {
		t.Error("Expected CID to be set")
	}
	if !strings.HasPrefix(resp.URI, "at://did:plc:test123") {
		t.Errorf("Expected URI to start with user's DID, got: %s", resp.URI)
	}
}

func TestCreateComment_EmptyContent(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: "",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.CreateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrContentEmpty) {
		t.Errorf("Expected ErrContentEmpty, got: %v", err)
	}
}

func TestCreateComment_ContentTooLong(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	// Create content with >10000 graphemes (using Unicode characters)
	longContent := strings.Repeat("あ", 10001) // Japanese character = 1 grapheme

	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: longContent,
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.CreateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrContentTooLong) {
		t.Errorf("Expected ErrContentTooLong, got: %v", err)
	}
}

func TestCreateComment_InvalidReplyRootURI(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "invalid-uri", // Invalid AT-URI
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: "Test comment",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.CreateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrInvalidReply) {
		t.Errorf("Expected ErrInvalidReply, got: %v", err)
	}
}

func TestCreateComment_InvalidReplyRootCID(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "", // Empty CID
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: "Test comment",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.CreateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrInvalidReply) {
		t.Errorf("Expected ErrInvalidReply, got: %v", err)
	}
}

func TestCreateComment_InvalidReplyParentURI(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "invalid-uri", // Invalid AT-URI
				CID: "bafyparent",
			},
		},
		Content: "Test comment",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.CreateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrInvalidReply) {
		t.Errorf("Expected ErrInvalidReply, got: %v", err)
	}
}

func TestCreateComment_InvalidReplyParentCID(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "", // Empty CID
			},
		},
		Content: "Test comment",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.CreateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrInvalidReply) {
		t.Errorf("Expected ErrInvalidReply, got: %v", err)
	}
}

func TestCreateComment_PDSError(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	mockClient.createError = errors.New("PDS connection failed")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: "Test comment",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.CreateComment(ctx, session, req)

	// Verify
	if err == nil {
		t.Fatal("Expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to create comment") {
		t.Errorf("Expected PDS error to be wrapped, got: %v", err)
	}
}

// ================================================================================
// UpdateComment Tests
// ================================================================================

func TestUpdateComment_Success(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	// Pre-create a comment in the mock PDS
	rkey := "testcomment123"
	existingRecord := map[string]interface{}{
		"$type":   "social.coves.community.comment",
		"content": "Original content",
		"reply": map[string]interface{}{
			"root": map[string]interface{}{
				"uri": "at://did:plc:author/social.coves.community.post/root123",
				"cid": "bafyroot",
			},
			"parent": map[string]interface{}{
				"uri": "at://did:plc:author/social.coves.community.post/root123",
				"cid": "bafyroot",
			},
		},
		"createdAt": time.Now().Format(time.RFC3339),
	}
	if mockClient.records["social.coves.community.comment"] == nil {
		mockClient.records["social.coves.community.comment"] = make(map[string]interface{})
	}
	mockClient.records["social.coves.community.comment"][rkey] = existingRecord

	req := UpdateCommentRequest{
		URI:     fmt.Sprintf("at://did:plc:test123/social.coves.community.comment/%s", rkey),
		Content: "Updated content",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	resp, err := service.UpdateComment(ctx, session, req)

	// Verify
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if resp == nil {
		t.Fatal("Expected response, got nil")
	}
	if resp.CID == "" {
		t.Error("Expected new CID to be set")
	}
}

func TestUpdateComment_EmptyURI(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := UpdateCommentRequest{
		URI:     "",
		Content: "Updated content",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.UpdateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("Expected ErrCommentNotFound, got: %v", err)
	}
}

func TestUpdateComment_InvalidURIFormat(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := UpdateCommentRequest{
		URI:     "invalid-uri",
		Content: "Updated content",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.UpdateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("Expected ErrCommentNotFound, got: %v", err)
	}
}

func TestUpdateComment_NotOwner(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	// Try to update a comment owned by a different user
	req := UpdateCommentRequest{
		URI:     "at://did:plc:otheruser/social.coves.community.comment/test123",
		Content: "Updated content",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.UpdateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("Expected ErrNotAuthorized, got: %v", err)
	}
}

func TestUpdateComment_EmptyContent(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := UpdateCommentRequest{
		URI:     "at://did:plc:test123/social.coves.community.comment/test123",
		Content: "",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.UpdateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrContentEmpty) {
		t.Errorf("Expected ErrContentEmpty, got: %v", err)
	}
}

func TestUpdateComment_ContentTooLong(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	longContent := strings.Repeat("あ", 10001)

	req := UpdateCommentRequest{
		URI:     "at://did:plc:test123/social.coves.community.comment/test123",
		Content: longContent,
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.UpdateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrContentTooLong) {
		t.Errorf("Expected ErrContentTooLong, got: %v", err)
	}
}

func TestUpdateComment_CommentNotFound(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	mockClient.getError = pds.ErrNotFound
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := UpdateCommentRequest{
		URI:     "at://did:plc:test123/social.coves.community.comment/nonexistent",
		Content: "Updated content",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	_, err := service.UpdateComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("Expected ErrCommentNotFound, got: %v", err)
	}
}

func TestUpdateComment_PreservesReplyRefs(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	// Pre-create a comment in the mock PDS
	rkey := "testcomment123"
	originalRootURI := "at://did:plc:author/social.coves.community.post/originalroot"
	originalRootCID := "bafyoriginalroot"
	existingRecord := map[string]interface{}{
		"$type":   "social.coves.community.comment",
		"content": "Original content",
		"reply": map[string]interface{}{
			"root": map[string]interface{}{
				"uri": originalRootURI,
				"cid": originalRootCID,
			},
			"parent": map[string]interface{}{
				"uri": originalRootURI,
				"cid": originalRootCID,
			},
		},
		"createdAt": time.Now().Format(time.RFC3339),
	}
	if mockClient.records["social.coves.community.comment"] == nil {
		mockClient.records["social.coves.community.comment"] = make(map[string]interface{})
	}
	mockClient.records["social.coves.community.comment"][rkey] = existingRecord

	req := UpdateCommentRequest{
		URI:     fmt.Sprintf("at://did:plc:test123/social.coves.community.comment/%s", rkey),
		Content: "Updated content",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	resp, err := service.UpdateComment(ctx, session, req)

	// Verify
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify reply refs were preserved by checking the updated record
	updatedRecordInterface := mockClient.records["social.coves.community.comment"][rkey]
	updatedRecord, ok := updatedRecordInterface.(CommentRecord)
	if !ok {
		// Try as map (from pre-existing record)
		recordMap := updatedRecordInterface.(map[string]interface{})
		reply := recordMap["reply"].(map[string]interface{})
		root := reply["root"].(map[string]interface{})

		if root["uri"] != originalRootURI {
			t.Errorf("Expected root URI to be preserved as %s, got %s", originalRootURI, root["uri"])
		}
		if root["cid"] != originalRootCID {
			t.Errorf("Expected root CID to be preserved as %s, got %s", originalRootCID, root["cid"])
		}

		// Verify content was updated
		if recordMap["content"] != "Updated content" {
			t.Errorf("Expected content to be updated to 'Updated content', got %s", recordMap["content"])
		}
	} else {
		// CommentRecord struct
		if updatedRecord.Reply.Root.URI != originalRootURI {
			t.Errorf("Expected root URI to be preserved as %s, got %s", originalRootURI, updatedRecord.Reply.Root.URI)
		}
		if updatedRecord.Reply.Root.CID != originalRootCID {
			t.Errorf("Expected root CID to be preserved as %s, got %s", originalRootCID, updatedRecord.Reply.Root.CID)
		}

		// Verify content was updated
		if updatedRecord.Content != "Updated content" {
			t.Errorf("Expected content to be updated to 'Updated content', got %s", updatedRecord.Content)
		}
	}

	// Verify response
	if resp == nil {
		t.Fatal("Expected response, got nil")
	}
}

// ================================================================================
// DeleteComment Tests
// ================================================================================

func TestDeleteComment_Success(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	// Pre-create a comment in the mock PDS
	rkey := "testcomment123"
	existingRecord := map[string]interface{}{
		"$type":   "social.coves.community.comment",
		"content": "Test content",
	}
	if mockClient.records["social.coves.community.comment"] == nil {
		mockClient.records["social.coves.community.comment"] = make(map[string]interface{})
	}
	mockClient.records["social.coves.community.comment"][rkey] = existingRecord

	req := DeleteCommentRequest{
		URI: fmt.Sprintf("at://did:plc:test123/social.coves.community.comment/%s", rkey),
	}

	session := createTestSession("did:plc:test123")

	// Execute
	err := service.DeleteComment(ctx, session, req)

	// Verify
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify comment was deleted from mock PDS
	_, exists := mockClient.records["social.coves.community.comment"][rkey]
	if exists {
		t.Error("Expected comment to be deleted from PDS")
	}
}

func TestDeleteComment_EmptyURI(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := DeleteCommentRequest{
		URI: "",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	err := service.DeleteComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("Expected ErrCommentNotFound, got: %v", err)
	}
}

func TestDeleteComment_InvalidURIFormat(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := DeleteCommentRequest{
		URI: "invalid-uri",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	err := service.DeleteComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("Expected ErrCommentNotFound, got: %v", err)
	}
}

func TestDeleteComment_NotOwner(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	// Try to delete a comment owned by a different user
	req := DeleteCommentRequest{
		URI: "at://did:plc:otheruser/social.coves.community.comment/test123",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	err := service.DeleteComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("Expected ErrNotAuthorized, got: %v", err)
	}
}

func TestDeleteComment_CommentNotFound(t *testing.T) {
	// Setup
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	mockClient.getError = pds.ErrNotFound
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	req := DeleteCommentRequest{
		URI: "at://did:plc:test123/social.coves.community.comment/nonexistent",
	}

	session := createTestSession("did:plc:test123")

	// Execute
	err := service.DeleteComment(ctx, session, req)

	// Verify
	if !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("Expected ErrCommentNotFound, got: %v", err)
	}
}

// TestCreateComment_GraphemeCounting tests that we count graphemes correctly, not runes
// Flag emoji 🇺🇸 is 2 runes but 1 grapheme
// Emoji with skin tone 👋🏽 is 2 runes but 1 grapheme
func TestCreateComment_GraphemeCounting(t *testing.T) {
	ctx := context.Background()
	mockClient := newMockPDSClient("did:plc:test123")
	factory := &mockPDSClientFactory{client: mockClient}

	commentRepo := newMockCommentRepo()
	userRepo := newMockUserRepo()
	postRepo := newMockPostRepo()
	communityRepo := newMockCommunityRepo()

	service := NewCommentServiceWithPDSFactory(
		commentRepo,
		userRepo,
		postRepo,
		communityRepo,
		nil,
		factory.create,
	)

	// Flag emoji 🇺🇸 is 2 runes but 1 grapheme
	// 10000 flag emojis = 10000 graphemes but 20000 runes
	// This should succeed because we count graphemes
	content := strings.Repeat("🇺🇸", 10000)

	req := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: content,
	}

	session := createTestSession("did:plc:test123")

	// Should succeed - 10000 graphemes is exactly at the limit
	_, err := service.CreateComment(ctx, session, req)
	if err != nil {
		t.Errorf("Expected success for 10000 graphemes, got error: %v", err)
	}

	// Now test that 10001 graphemes fails
	contentTooLong := strings.Repeat("🇺🇸", 10001)
	reqTooLong := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: contentTooLong,
	}

	_, err = service.CreateComment(ctx, session, reqTooLong)
	if !errors.Is(err, ErrContentTooLong) {
		t.Errorf("Expected ErrContentTooLong for 10001 graphemes, got: %v", err)
	}

	// Also test emoji with skin tone modifier: 👋🏽 is 2 runes but 1 grapheme
	contentWithSkinTone := strings.Repeat("👋🏽", 10000)
	reqWithSkinTone := CreateCommentRequest{
		Reply: ReplyRef{
			Root: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
			Parent: StrongRef{
				URI: "at://did:plc:author/social.coves.community.post/root123",
				CID: "bafyroot",
			},
		},
		Content: contentWithSkinTone,
	}

	_, err = service.CreateComment(ctx, session, reqWithSkinTone)
	if err != nil {
		t.Errorf("Expected success for 10000 graphemes with skin tone modifier, got error: %v", err)
	}
}
