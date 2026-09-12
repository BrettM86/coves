package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockPDSClient implements pds.Client for testing error paths
type mockPDSClient struct {
	getRecordResponse         *pds.RecordResponse
	getRecordError            error
	uploadBlobError           error
	uploadBlobRef             *blobs.BlobRef
	putRecordError            error
	putRecordWithCommitError  error
	putRecordURI              string
	putRecordCID              string
	putRecordCalled           bool
	putRecordWithCommitCalled bool

	// The arguments the handler actually passed to PutRecord. Captured because
	// the RECORD is the wire contract with the firehose consumer that indexes
	// it: a 200 from this handler says nothing about whether the thing written
	// to the repo is the thing internal/atproto/jetstream can read back.
	putRecordCollection string
	putRecordRKey       string
	putRecordValue      any
	putRecordSwap       string
}

var _ pds.CommitClient = (*mockPDSClient)(nil)

func (m *mockPDSClient) CreateRecord(_ context.Context, _ string, _ string, _ any) (string, string, error) {
	return "", "", nil
}

func (m *mockPDSClient) DeleteRecord(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockPDSClient) ListRecords(_ context.Context, _ string, _ int, _ string) (*pds.ListRecordsResponse, error) {
	return nil, nil
}

func (m *mockPDSClient) GetRecord(_ context.Context, _ string, _ string) (*pds.RecordResponse, error) {
	return m.getRecordResponse, m.getRecordError
}

func (m *mockPDSClient) PutRecord(_ context.Context, collection string, rkey string, record any, swapRecord string) (string, string, error) {
	m.putRecordCalled = true
	m.putRecordCollection, m.putRecordRKey, m.putRecordValue, m.putRecordSwap = collection, rkey, record, swapRecord
	if m.putRecordError != nil {
		return "", "", m.putRecordError
	}
	return m.putRecordURI, m.putRecordCID, nil
}

// PutRecordWithCommit lets the mock exercise create-only guarded writes.
func (m *mockPDSClient) PutRecordWithCommit(_ context.Context, collection string, rkey string, record any, swapRecord string) (*pds.RecordCommit, error) {
	m.putRecordWithCommitCalled = true
	m.putRecordCollection, m.putRecordRKey, m.putRecordValue, m.putRecordSwap = collection, rkey, record, swapRecord
	if m.putRecordWithCommitError != nil {
		if errors.Is(m.putRecordWithCommitError, pds.ErrNoCommit) {
			return &pds.RecordCommit{URI: m.putRecordURI, CID: m.putRecordCID}, m.putRecordWithCommitError
		}
		return nil, m.putRecordWithCommitError
	}
	return &pds.RecordCommit{URI: m.putRecordURI, CID: m.putRecordCID}, nil
}

func (m *mockPDSClient) ApplyWrites(_ context.Context, _ []pds.Write, _ string) (*pds.ApplyWritesResult, error) {
	panic("unexpected ApplyWrites call")
}

func (m *mockPDSClient) CreateRecordWithCommit(_ context.Context, _ string, _ string, _ any) (*pds.RecordCommit, error) {
	panic("unexpected CreateRecordWithCommit call")
}

func (m *mockPDSClient) GetLatestCommit(_ context.Context) (*pds.LatestCommit, error) {
	panic("unexpected GetLatestCommit call")
}

func (m *mockPDSClient) UploadBlob(_ context.Context, _ []byte, _ string) (*blobs.BlobRef, error) {
	if m.uploadBlobError != nil {
		return nil, m.uploadBlobError
	}
	return m.uploadBlobRef, nil
}

func (m *mockPDSClient) DID() string {
	return "did:plc:test123"
}

func (m *mockPDSClient) HostURL() string {
	return "https://test.pds.example"
}

// createMockFactory creates a commit-aware factory that returns the given mock client.
func createMockFactory(client pds.CommitClient, err error) PDSClientFactory {
	return func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.CommitClient, error) {
		if err != nil {
			return nil, err
		}
		return client, nil
	}
}

// createTestHandler creates a handler with a mock factory for testing validation paths
// that don't require actual PDS client operations
func createTestHandler() *UpdateProfileHandler {
	// Use a factory that will never be called (tests exit before PDS client creation)
	return NewUpdateProfileHandlerWithFactory(func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.CommitClient, error) {
		return nil, errors.New("mock factory should not be called in validation tests")
	})
}

// createTestOAuthSession creates a test OAuth session for testing
func createTestOAuthSession(did string) *oauthlib.ClientSessionData {
	parsedDID, _ := syntax.ParseDID(did)
	return &oauthlib.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   "test-session-id",
		HostURL:     "https://test.pds.example",
		AccessToken: "test-access-token",
	}
}

// setTestOAuthSession sets both user DID and OAuth session in context
func setTestOAuthSession(req *http.Request, userDID string, session *oauthlib.ClientSessionData) *http.Request {
	ctx := middleware.SetTestUserDID(req.Context(), userDID)
	ctx = middleware.SetTestOAuthSession(ctx, session)
	return req.WithContext(ctx)
}

// TestUpdateProfileHandler_Unauthenticated tests that unauthenticated requests return 401
func TestUpdateProfileHandler_Unauthenticated(t *testing.T) {
	handler := createTestHandler()

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No auth context - simulates unauthenticated request

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "AuthRequired")
}

// TestUpdateProfileHandler_MissingOAuthSession tests that missing OAuth session returns 401
func TestUpdateProfileHandler_MissingOAuthSession(t *testing.T) {
	handler := createTestHandler()

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// Set user DID but no OAuth session
	ctx := middleware.SetTestUserDID(req.Context(), "did:plc:testuser123")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Missing PDS credentials")
}

// TestUpdateProfileHandler_InvalidRequestBody tests that invalid JSON returns 400
func TestUpdateProfileHandler_InvalidRequestBody(t *testing.T) {
	handler := createTestHandler()

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", strings.NewReader("not valid json"))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid request body")
}

// TestUpdateProfileHandler_AvatarSizeExceedsLimit tests that avatar over 1MB is rejected
func TestUpdateProfileHandler_AvatarSizeExceedsLimit(t *testing.T) {
	handler := createTestHandler()

	// Create avatar blob larger than 1MB (1,000,001 bytes)
	largeBlob := make([]byte, 1_000_001)

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     largeBlob,
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Avatar exceeds 1MB limit")
}

// TestUpdateProfileHandler_BannerSizeExceedsLimit tests that banner over 2MB is rejected
func TestUpdateProfileHandler_BannerSizeExceedsLimit(t *testing.T) {
	handler := createTestHandler()

	// Create banner blob larger than 2MB (2,000,001 bytes)
	largeBlob := make([]byte, 2_000_001)

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     largeBlob,
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Banner exceeds 2MB limit")
}

// TestUpdateProfileHandler_InvalidAvatarMimeType tests that invalid avatar mime type is rejected
func TestUpdateProfileHandler_InvalidAvatarMimeType(t *testing.T) {
	handler := createTestHandler()

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/gif", // Not allowed - only png/jpeg/webp
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid avatar mime type")
}

// TestUpdateProfileHandler_InvalidBannerMimeType tests that invalid banner mime type is rejected
func TestUpdateProfileHandler_InvalidBannerMimeType(t *testing.T) {
	handler := createTestHandler()

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     []byte("fake image data"),
		BannerMimeType: "application/pdf", // Not allowed
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid banner mime type")
}

// TestUpdateProfileHandler_MethodNotAllowed tests that non-POST methods are rejected
func TestUpdateProfileHandler_MethodNotAllowed(t *testing.T) {
	handler := createTestHandler()

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/xrpc/social.coves.actor.updateProfile", nil)

			testDID := "did:plc:testuser123"
			session := createTestOAuthSession(testDID)
			req = setTestOAuthSession(req, testDID, session)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
		})
	}
}

// TestUpdateProfileHandler_AvatarBlobWithoutMimeType tests that providing blob without mime type fails
func TestUpdateProfileHandler_AvatarBlobWithoutMimeType(t *testing.T) {
	handler := createTestHandler()

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
		AvatarBlob:  []byte("fake image data"),
		// Missing AvatarMimeType
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "mime type")
}

// TestUpdateProfileHandler_BannerBlobWithoutMimeType tests that providing banner without mime type fails
func TestUpdateProfileHandler_BannerBlobWithoutMimeType(t *testing.T) {
	handler := createTestHandler()

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
		BannerBlob:  []byte("fake image data"),
		// Missing BannerMimeType
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "mime type")
}

// TestUpdateProfileHandler_DisplayNameTooLong tests that displayName exceeding limit is rejected
func TestUpdateProfileHandler_DisplayNameTooLong(t *testing.T) {
	handler := createTestHandler()

	longName := strings.Repeat("a", MaxDisplayNameGraphemes+1)
	reqBody := UpdateProfileRequest{
		DisplayName: &longName,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "DisplayNameTooLong")
}

// TestUpdateProfileHandler_BioTooLong tests that bio exceeding limit is rejected
func TestUpdateProfileHandler_BioTooLong(t *testing.T) {
	handler := createTestHandler()

	longBio := strings.Repeat("a", MaxBioGraphemes+1)
	reqBody := UpdateProfileRequest{
		Bio: &longBio,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "BioTooLong")
}

// TestUpdateProfileHandler_MissingHostURL tests that missing PDS host URL returns error
func TestUpdateProfileHandler_MissingHostURL(t *testing.T) {
	handler := createTestHandler()

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = "" // Missing host URL
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Missing PDS credentials")
}

// TestIsValidImageMimeType tests the mime type validation function
func TestIsValidImageMimeType(t *testing.T) {
	validTypes := []string{"image/png", "image/jpeg", "image/webp"}
	for _, mt := range validTypes {
		t.Run("valid_"+mt, func(t *testing.T) {
			assert.True(t, isValidImageMimeType(mt))
		})
	}

	invalidTypes := []string{"image/gif", "image/bmp", "application/pdf", "text/plain", "", "image/svg+xml"}
	for _, mt := range invalidTypes {
		t.Run("invalid_"+mt, func(t *testing.T) {
			assert.False(t, isValidImageMimeType(mt))
		})
	}
}

// ============================================================================
// PDS Client Error Path Tests
// These tests use mock factories to test error handling for PDS operations
// ============================================================================

// TestUpdateProfileHandler_PDSClientCreationFails tests session restoration failure
func TestUpdateProfileHandler_PDSClientCreationFails(t *testing.T) {
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(nil, errors.New("session restoration failed")))

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "SessionError")
	assert.Contains(t, w.Body.String(), "Failed to restore session")
}

// TestUpdateProfileHandler_AvatarUploadUnauthorized tests avatar upload auth error
func TestUpdateProfileHandler_AvatarUploadUnauthorized(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobError: pds.ErrUnauthorized,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "AuthExpired")
}

// TestUpdateProfileHandler_AvatarUploadRateLimited tests avatar upload rate limiting
func TestUpdateProfileHandler_AvatarUploadRateLimited(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobError: pds.ErrRateLimited,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "RateLimited")
}

// TestUpdateProfileHandler_AvatarUploadPayloadTooLarge tests avatar upload payload size error
func TestUpdateProfileHandler_AvatarUploadPayloadTooLarge(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobError: pds.ErrPayloadTooLarge,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "AvatarTooLarge")
}

// TestUpdateProfileHandler_BannerUploadUnauthorized tests banner upload auth error
func TestUpdateProfileHandler_BannerUploadUnauthorized(t *testing.T) {
	// First upload succeeds (avatar), second fails (banner)
	callCount := 0
	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      map[string]string{"$link": "bafytest"},
			MimeType: "image/jpeg",
			Size:     100,
		},
	}
	handler := NewUpdateProfileHandlerWithFactory(func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.CommitClient, error) {
		// Return a mock that fails on second UploadBlob call
		return &mockPDSClientWithCallCounter{
			mockPDSClient: mockClient,
			callCount:     &callCount,
			failOnCall:    2, // Fail on banner upload
			failError:     pds.ErrUnauthorized,
		}, nil
	})

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("avatar data"),
		AvatarMimeType: "image/jpeg",
		BannerBlob:     []byte("banner data"),
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "AuthExpired")
}

// TestUpdateProfileHandler_ProfileWriteUnauthorized tests profile write auth error
func TestUpdateProfileHandler_ProfileWriteUnauthorized(t *testing.T) {
	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			CID:   "bafyexisting",
			Value: map[string]any{"$type": CovesProfileCollection},
		},
		putRecordWithCommitError: pds.ErrUnauthorized,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "AuthExpired")
}

// TestUpdateProfileHandler_ProfileWriteRateLimited tests profile write rate limiting
func TestUpdateProfileHandler_ProfileWriteRateLimited(t *testing.T) {
	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			CID:   "bafyexisting",
			Value: map[string]any{"$type": CovesProfileCollection},
		},
		putRecordWithCommitError: pds.ErrRateLimited,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "RateLimited")
}

// TestUpdateProfileHandler_ProfileWritePayloadTooLarge tests profile write payload size error
func TestUpdateProfileHandler_ProfileWritePayloadTooLarge(t *testing.T) {
	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			CID:   "bafyexisting",
			Value: map[string]any{"$type": CovesProfileCollection},
		},
		putRecordWithCommitError: pds.ErrPayloadTooLarge,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "PayloadTooLarge")
}

// TestUpdateProfileHandler_ProfileWriteForbidden tests that a PDS 403 maps to
// PermissionDenied (403), NOT AuthExpired (401) — a permissions error must not
// trigger a client sign-out of a valid session.
func TestUpdateProfileHandler_ProfileWriteForbidden(t *testing.T) {
	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			CID:   "bafyexisting",
			Value: map[string]any{"$type": CovesProfileCollection},
		},
		putRecordWithCommitError: pds.ErrForbidden,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "PermissionDenied")
}

// TestUpdateProfileHandler_Success tests successful profile update
func TestUpdateProfileHandler_Success(t *testing.T) {
	mockClient := &mockPDSClient{
		putRecordURI: "at://did:plc:test123/social.coves.actor.profile/self",
		putRecordCID: "bafyreifake",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
		Bio:         strPtr("Hello world"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp UpdateProfileResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "at://did:plc:test123/social.coves.actor.profile/self", resp.URI)
	assert.Equal(t, "bafyreifake", resp.CID)
}

func TestUpdateProfileHandler_TextUpdatePreservesExistingRecord(t *testing.T) {
	avatar := map[string]any{
		"$type":    "blob",
		"ref":      map[string]any{"$link": "bafyavatar"},
		"mimeType": "image/png",
		"size":     float64(1000),
	}
	banner := map[string]any{
		"$type":    "blob",
		"ref":      map[string]any{"$link": "bafybanner"},
		"mimeType": "image/jpeg",
		"size":     float64(2000),
	}
	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			CID: "bafyexisting",
			Value: map[string]any{
				"$type":       CovesProfileCollection,
				"displayName": "Old name",
				"description": "Keep this bio",
				"avatar":      avatar,
				"banner":      banner,
				"createdAt":   "2026-09-01T00:00:00Z",
			},
		},
		putRecordURI: "at://did:plc:test123/social.coves.actor.profile/self",
		putRecordCID: "bafyupdated",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{DisplayName: strPtr("New name")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{
		"$type":       CovesProfileCollection,
		"displayName": "New name",
		"description": "Keep this bio",
		"avatar":      avatar,
		"banner":      banner,
		"createdAt":   "2026-09-01T00:00:00Z",
	}, mockClient.putRecordValue)
	assert.Equal(t, "bafyexisting", mockClient.putRecordSwap)
}

func TestUpdateProfileHandler_BioChangeRemovesDescriptionFacets(t *testing.T) {
	facets := []any{
		map[string]any{
			"index": map[string]any{"byteStart": float64(0), "byteEnd": float64(8)},
			"features": []any{map[string]any{
				"$type": "social.coves.richtext.facet#link",
				"uri":   "https://example.com/old",
			}},
		},
	}

	tests := []struct {
		name string
		bio  string
	}{
		{name: "changed", bio: "A new bio"},
		{name: "cleared", bio: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockPDSClient{
				getRecordResponse: &pds.RecordResponse{
					CID: "bafyexisting",
					Value: map[string]any{
						"$type":             CovesProfileCollection,
						"description":       "Old link",
						"descriptionFacets": facets,
					},
				},
				putRecordURI: "at://did:plc:testuser123/social.coves.actor.profile/self",
				putRecordCID: "bafyupdated",
			}
			handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

			body, _ := json.Marshal(UpdateProfileRequest{Bio: strPtr(tt.bio)})
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			const testDID = "did:plc:testuser123"
			req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
			profile, ok := mockClient.putRecordValue.(map[string]any)
			assert.True(t, ok)
			assert.Equal(t, tt.bio, profile["description"])
			assert.NotContains(t, profile, "descriptionFacets",
				"facets indexed into the old description must not survive a changed or cleared bio")
		})
	}
}

func TestUpdateProfileHandler_UnchangedBioPreservesDescriptionFacets(t *testing.T) {
	const existingBio = "Keep https://example.com"
	facets := []any{
		map[string]any{
			"index": map[string]any{"byteStart": float64(5), "byteEnd": float64(24)},
			"features": []any{map[string]any{
				"$type": "social.coves.richtext.facet#link",
				"uri":   "https://example.com",
			}},
		},
	}

	tests := []struct {
		name string
		bio  *string
	}{
		{name: "omitted"},
		{name: "explicitly unchanged", bio: strPtr(existingBio)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockPDSClient{
				getRecordResponse: &pds.RecordResponse{
					CID: "bafyexisting",
					Value: map[string]any{
						"$type":             CovesProfileCollection,
						"description":       existingBio,
						"descriptionFacets": facets,
					},
				},
				putRecordURI: "at://did:plc:testuser123/social.coves.actor.profile/self",
				putRecordCID: "bafyupdated",
			}
			handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

			body, _ := json.Marshal(UpdateProfileRequest{Bio: tt.bio})
			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			const testDID = "did:plc:testuser123"
			req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
			profile, ok := mockClient.putRecordValue.(map[string]any)
			assert.True(t, ok)
			assert.Equal(t, existingBio, profile["description"])
			assert.Equal(t, facets, profile["descriptionFacets"])
		})
	}
}

func TestUpdateProfileHandler_AvatarReplacementPreservesRestOfStandingRecord(t *testing.T) {
	oldAvatar := map[string]any{
		"$type":    "blob",
		"ref":      map[string]any{"$link": "bafyoldavatar"},
		"mimeType": "image/png",
		"size":     float64(500),
	}
	banner := map[string]any{
		"$type":    "blob",
		"ref":      map[string]any{"$link": "bafybanner"},
		"mimeType": "image/jpeg",
		"size":     float64(2000),
	}
	facets := []any{map[string]any{"index": map[string]any{
		"byteStart": float64(0), "byteEnd": float64(7),
	}}}
	unknown := map[string]any{"nested": []any{"future", float64(2)}}

	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			CID: "bafystanding",
			Value: map[string]any{
				"$type":              CovesProfileCollection,
				"displayName":        "Existing name",
				"description":        "Old bio",
				"descriptionFacets":  facets,
				"avatar":             oldAvatar,
				"banner":             banner,
				"createdAt":          "2026-09-01T00:00:00Z",
				"unknownFutureField": unknown,
			},
		},
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      map[string]string{"$link": "bafynewavatar"},
			MimeType: "image/webp",
			Size:     1000,
		},
		putRecordURI: "at://did:plc:testuser123/social.coves.actor.profile/self",
		putRecordCID: "bafyupdated",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{
		AvatarBlob:     []byte("new avatar"),
		AvatarMimeType: "image/webp",
	})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{
		"$type":             CovesProfileCollection,
		"displayName":       "Existing name",
		"description":       "Old bio",
		"descriptionFacets": facets,
		"avatar": map[string]any{
			"$type":    "blob",
			"ref":      map[string]string{"$link": "bafynewavatar"},
			"mimeType": "image/webp",
			"size":     1000,
		},
		"banner":             banner,
		"createdAt":          "2026-09-01T00:00:00Z",
		"unknownFutureField": unknown,
	}, mockClient.putRecordValue)
	assert.Equal(t, "bafystanding", mockClient.putRecordSwap,
		"avatar replacement must compare against the CID of the record whose other fields were preserved")
}

func TestUpdateProfileHandler_PutRecordSwapConflictIsHTTPConflict(t *testing.T) {
	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			CID:   "bafystanding",
			Value: map[string]any{"$type": CovesProfileCollection},
		},
		putRecordWithCommitError: pds.ErrSwapConflict,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{DisplayName: strPtr("Racing update")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `"error":"Conflict"`)
	assert.Equal(t, "bafystanding", mockClient.putRecordSwap)
}

func TestUpdateProfileHandler_NoCommitReturnsAcceptedRecordIdentity(t *testing.T) {
	const (
		acceptedURI = "at://did:plc:testuser123/social.coves.actor.profile/self"
		staleCID    = "bafystale"
		acceptedCID = "bafyaccepted"
	)
	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			URI: acceptedURI,
			CID: staleCID,
			Value: map[string]any{
				"$type":       CovesProfileCollection,
				"displayName": "Standing name",
			},
		},
		putRecordWithCommitError: pds.ErrNoCommit,
		putRecordURI:             acceptedURI,
		putRecordCID:             acceptedCID,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{DisplayName: strPtr("Standing name")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.True(t, mockClient.putRecordWithCommitCalled)
	assert.False(t, mockClient.putRecordCalled, "ErrNoCommit must never trigger an unguarded retry")
	require.Equal(t, http.StatusOK, w.Code,
		"a no-op save already has the requested state and must not surface as a failure")
	var resp UpdateProfileResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, acceptedURI, resp.URI)
	assert.Equal(t, acceptedCID, resp.CID,
		"the accepted write identity must win over the stale pre-read CID")
}

func TestUpdateProfileHandler_NoCommitAfterNotFoundReturnsAcceptedRecordIdentity(t *testing.T) {
	const (
		acceptedURI = "at://did:plc:testuser123/social.coves.actor.profile/self"
		acceptedCID = "bafyaccepted"
	)
	mockClient := &mockPDSClient{
		getRecordError:           pds.ErrNotFound,
		putRecordWithCommitError: pds.ErrNoCommit,
		putRecordURI:             acceptedURI,
		putRecordCID:             acceptedCID,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{DisplayName: strPtr("First profile")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.True(t, mockClient.putRecordWithCommitCalled)
	assert.False(t, mockClient.putRecordCalled, "ErrNoCommit must never trigger an unguarded retry")
	require.Equal(t, http.StatusOK, w.Code,
		"the PDS accepted the initially missing record even though it emitted no commit metadata")
	var resp UpdateProfileResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, acceptedURI, resp.URI)
	assert.Equal(t, acceptedCID, resp.CID)
}

func TestUpdateProfileHandler_MissingRecordCreatesProfile(t *testing.T) {
	mockClient := &mockPDSClient{
		getRecordError: pds.ErrNotFound,
		putRecordURI:   "at://did:plc:test123/social.coves.actor.profile/self",
		putRecordCID:   "bafycreated",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{DisplayName: strPtr("First profile")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{
		"$type":       CovesProfileCollection,
		"displayName": "First profile",
	}, mockClient.putRecordValue)
	assert.Empty(t, mockClient.putRecordSwap)
}

func TestUpdateProfileHandler_MissingRecordUsesCreateOnlyGuard(t *testing.T) {
	mockClient := &mockPDSClient{
		getRecordError:           pds.ErrNotFound,
		putRecordWithCommitError: pds.ErrSwapConflict,
		putRecordURI:             "at://did:plc:testuser123/social.coves.actor.profile/self",
		putRecordCID:             "bafyoverwritten",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{DisplayName: strPtr("First profile")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code,
		"a profile created after the missing-record read must win; this request must not overwrite it")
	assert.True(t, mockClient.putRecordWithCommitCalled,
		"the guarded PDS writer gives an empty swapRecord create-only semantics")
	assert.False(t, mockClient.putRecordCalled,
		"Client.PutRecord treats an empty swapRecord as unguarded and can overwrite a concurrent create")
}

func TestUpdateProfileHandler_NilGetRecordResponseDoesNotUseUnguardedWrite(t *testing.T) {
	mockClient := &mockPDSClient{}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{DisplayName: strPtr("First profile")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, mockClient.putRecordCalled,
		"a nil record response must not cause an unguarded PutRecord")
	assert.True(t, mockClient.putRecordWithCommitCalled,
		"a nil record response must use the guarded writer")
	assert.Empty(t, mockClient.putRecordSwap,
		"a nil record response may only be written with the create-only guard")
}

func TestUpdateProfileHandler_ExistingRecordWithEmptyCIDDoesNotUseUnguardedWrite(t *testing.T) {
	mockClient := &mockPDSClient{
		getRecordResponse: &pds.RecordResponse{
			Value: map[string]any{"$type": CovesProfileCollection},
		},
		putRecordWithCommitError: pds.ErrSwapConflict,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{DisplayName: strPtr("Updated profile")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !assert.False(t, mockClient.putRecordCalled,
		"an existing record with an empty CID must not cause an unguarded PutRecord") {
		return
	}
	assert.True(t, mockClient.putRecordWithCommitCalled)
	assert.Empty(t, mockClient.putRecordSwap,
		"the commit-aware writer must give the empty swap create-only semantics")
}

func TestUpdateProfileHandler_GetRecordRateLimitedStopsWrite(t *testing.T) {
	mockClient := &mockPDSClient{getRecordError: pds.ErrRateLimited}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{Bio: strPtr("New bio")})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "RateLimited")
	assert.Nil(t, mockClient.putRecordValue)
}

// TestUpdateProfileHandler_SuccessWithAvatar tests successful profile update with avatar
func TestUpdateProfileHandler_SuccessWithAvatar(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      map[string]string{"$link": "bafyavatartest"},
			MimeType: "image/jpeg",
			Size:     1000,
		},
		putRecordURI: "at://did:plc:test123/social.coves.actor.profile/self",
		putRecordCID: "bafyreifake",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// Helper function to create string pointers
func strPtr(s string) *string {
	return &s
}

// mockPDSClientWithCallCounter wraps mockPDSClient to track call count
type mockPDSClientWithCallCounter struct {
	*mockPDSClient
	callCount  *int
	failOnCall int
	failError  error
}

func (m *mockPDSClientWithCallCounter) UploadBlob(_ context.Context, _ []byte, _ string) (*blobs.BlobRef, error) {
	*m.callCount++
	if *m.callCount == m.failOnCall {
		return nil, m.failError
	}
	return m.mockPDSClient.uploadBlobRef, nil
}

// ============================================================================
// Constructor Panic Tests
// ============================================================================

// TestNewUpdateProfileHandler_NilOAuthClientPanics verifies that passing nil oauthClient panics
func TestNewUpdateProfileHandler_NilOAuthClientPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected NewUpdateProfileHandler to panic with nil oauthClient, but it did not panic")
		} else {
			// Verify the panic message is as expected
			panicMsg, ok := r.(string)
			if !ok {
				t.Errorf("Expected panic message to be a string, got %T", r)
				return
			}
			assert.Contains(t, panicMsg, "oauthClient is required")
		}
	}()

	NewUpdateProfileHandler(nil)
}

// TestNewUpdateProfileHandlerWithFactory_NilFactoryPanics verifies that passing nil factory panics
func TestNewUpdateProfileHandlerWithFactory_NilFactoryPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected NewUpdateProfileHandlerWithFactory to panic with nil factory, but it did not panic")
		} else {
			// Verify the panic message is as expected
			panicMsg, ok := r.(string)
			if !ok {
				t.Errorf("Expected panic message to be a string, got %T", r)
				return
			}
			assert.Contains(t, panicMsg, "factory is required")
		}
	}()

	NewUpdateProfileHandlerWithFactory(nil)
}

// ============================================================================
// Invalid BlobRef Handling Tests
// ============================================================================

// TestUpdateProfileHandler_AvatarUploadReturnsNilRef tests handling of nil BlobRef from avatar upload
func TestUpdateProfileHandler_AvatarUploadReturnsNilRef(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobRef: nil, // Nil BlobRef
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "BlobUploadFailed")
	assert.Contains(t, w.Body.String(), "Invalid avatar blob reference")
}

// TestUpdateProfileHandler_AvatarUploadReturnsNilRefField tests handling of BlobRef with nil Ref field
func TestUpdateProfileHandler_AvatarUploadReturnsNilRefField(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      nil, // Nil Ref field
			MimeType: "image/jpeg",
			Size:     100,
		},
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "BlobUploadFailed")
	assert.Contains(t, w.Body.String(), "Invalid avatar blob reference")
}

// TestUpdateProfileHandler_AvatarUploadReturnsEmptyType tests handling of BlobRef with empty Type
func TestUpdateProfileHandler_AvatarUploadReturnsEmptyType(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "", // Empty Type
			Ref:      map[string]string{"$link": "bafytest"},
			MimeType: "image/jpeg",
			Size:     100,
		},
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "BlobUploadFailed")
	assert.Contains(t, w.Body.String(), "Invalid avatar blob reference")
}

// TestUpdateProfileHandler_BannerUploadReturnsNilRef tests handling of nil BlobRef from banner upload
func TestUpdateProfileHandler_BannerUploadReturnsNilRef(t *testing.T) {
	// We need the avatar upload to succeed and banner upload to return nil
	callCount := 0
	handler := NewUpdateProfileHandlerWithFactory(func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.CommitClient, error) {
		return &mockPDSClientWithNilBannerRef{
			callCount: &callCount,
		}, nil
	})

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     []byte("banner data"),
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "BlobUploadFailed")
	assert.Contains(t, w.Body.String(), "Invalid banner blob reference")
}

// TestUpdateProfileHandler_BannerUploadReturnsNilRefField tests handling of BlobRef with nil Ref field for banner
func TestUpdateProfileHandler_BannerUploadReturnsNilRefField(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      nil, // Nil Ref field - will affect banner since no avatar in request
			MimeType: "image/jpeg",
			Size:     100,
		},
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     []byte("banner data"),
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "BlobUploadFailed")
	assert.Contains(t, w.Body.String(), "Invalid banner blob reference")
}

// TestUpdateProfileHandler_BannerUploadReturnsEmptyType tests handling of BlobRef with empty Type for banner
func TestUpdateProfileHandler_BannerUploadReturnsEmptyType(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "", // Empty Type - will affect banner since no avatar in request
			Ref:      map[string]string{"$link": "bafytest"},
			MimeType: "image/jpeg",
			Size:     100,
		},
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     []byte("banner data"),
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "BlobUploadFailed")
	assert.Contains(t, w.Body.String(), "Invalid banner blob reference")
}

// mockPDSClientWithNilBannerRef returns nil for banner uploads (called when no avatar blob is present)
type mockPDSClientWithNilBannerRef struct {
	callCount *int
}

var _ pds.CommitClient = (*mockPDSClientWithNilBannerRef)(nil)

func (m *mockPDSClientWithNilBannerRef) CreateRecord(_ context.Context, _ string, _ string, _ any) (string, string, error) {
	return "", "", nil
}

func (m *mockPDSClientWithNilBannerRef) DeleteRecord(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockPDSClientWithNilBannerRef) ListRecords(_ context.Context, _ string, _ int, _ string) (*pds.ListRecordsResponse, error) {
	return nil, nil
}

func (m *mockPDSClientWithNilBannerRef) GetRecord(_ context.Context, _ string, _ string) (*pds.RecordResponse, error) {
	return nil, nil
}

func (m *mockPDSClientWithNilBannerRef) PutRecord(_ context.Context, _ string, _ string, _ any, _ string) (string, string, error) {
	return "", "", nil
}

func (m *mockPDSClientWithNilBannerRef) ApplyWrites(_ context.Context, _ []pds.Write, _ string) (*pds.ApplyWritesResult, error) {
	panic("unexpected ApplyWrites call")
}

func (m *mockPDSClientWithNilBannerRef) PutRecordWithCommit(_ context.Context, _ string, _ string, _ any, _ string) (*pds.RecordCommit, error) {
	panic("unexpected PutRecordWithCommit call")
}

func (m *mockPDSClientWithNilBannerRef) CreateRecordWithCommit(_ context.Context, _ string, _ string, _ any) (*pds.RecordCommit, error) {
	panic("unexpected CreateRecordWithCommit call")
}

func (m *mockPDSClientWithNilBannerRef) GetLatestCommit(_ context.Context) (*pds.LatestCommit, error) {
	panic("unexpected GetLatestCommit call")
}

func (m *mockPDSClientWithNilBannerRef) UploadBlob(_ context.Context, _ []byte, _ string) (*blobs.BlobRef, error) {
	// Return nil to simulate invalid response
	return nil, nil
}

func (m *mockPDSClientWithNilBannerRef) DID() string {
	return "did:plc:test123"
}

func (m *mockPDSClientWithNilBannerRef) HostURL() string {
	return "https://test.pds.example"
}

// ============================================================================
// Boundary Size Tests
// ============================================================================

// TestUpdateProfileHandler_AvatarExactlyAtMaxSize tests that avatar at exactly 1MB is accepted
func TestUpdateProfileHandler_AvatarExactlyAtMaxSize(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      map[string]string{"$link": "bafyavatartest"},
			MimeType: "image/jpeg",
			Size:     MaxAvatarBlobSize,
		},
		putRecordURI: "at://did:plc:test123/social.coves.actor.profile/self",
		putRecordCID: "bafyreifake",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	// Create avatar blob at exactly 1MB (1,000,000 bytes)
	avatarBlob := make([]byte, MaxAvatarBlobSize)

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     avatarBlob,
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp UpdateProfileResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.NotEmpty(t, resp.URI)
	assert.NotEmpty(t, resp.CID)
}

// TestUpdateProfileHandler_BannerExactlyAtMaxSize tests that banner at exactly 2MB is accepted
func TestUpdateProfileHandler_BannerExactlyAtMaxSize(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      map[string]string{"$link": "bafybannertest"},
			MimeType: "image/jpeg",
			Size:     MaxBannerBlobSize,
		},
		putRecordURI: "at://did:plc:test123/social.coves.actor.profile/self",
		putRecordCID: "bafyreifake",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	// Create banner blob at exactly 2MB (2,000,000 bytes)
	bannerBlob := make([]byte, MaxBannerBlobSize)

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     bannerBlob,
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp UpdateProfileResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.NotEmpty(t, resp.URI)
	assert.NotEmpty(t, resp.CID)
}

// ============================================================================
// Banner-Specific Error Path Tests
// ============================================================================

// TestUpdateProfileHandler_BannerUploadRateLimited tests banner upload rate limiting
func TestUpdateProfileHandler_BannerUploadRateLimited(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobError: pds.ErrRateLimited,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     []byte("banner data"),
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "RateLimited")
}

// TestUpdateProfileHandler_BannerUploadPayloadTooLarge tests banner upload payload size error
func TestUpdateProfileHandler_BannerUploadPayloadTooLarge(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobError: pds.ErrPayloadTooLarge,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     []byte("banner data"),
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "BannerTooLarge")
}

// TestUpdateProfileHandler_BannerUploadForbidden tests that a PDS 403 on blob
// upload maps to PermissionDenied (403), NOT AuthExpired (401) — a missing
// blob:*/* scope must not trigger a client sign-out of a valid session.
func TestUpdateProfileHandler_BannerUploadForbidden(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobError: pds.ErrForbidden,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     []byte("banner data"),
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "PermissionDenied")
}

// TestUpdateProfileHandler_AvatarUploadForbidden tests that a PDS 403 on avatar
// upload maps to PermissionDenied (403), NOT AuthExpired (401).
func TestUpdateProfileHandler_AvatarUploadForbidden(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobError: pds.ErrForbidden,
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "PermissionDenied")
}

// TestUpdateProfileHandler_BannerUploadGenericError tests banner upload with generic error
func TestUpdateProfileHandler_BannerUploadGenericError(t *testing.T) {
	mockClient := &mockPDSClient{
		uploadBlobError: errors.New("network error"),
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     []byte("banner data"),
		BannerMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "BlobUploadFailed")
	assert.Contains(t, w.Body.String(), "Failed to upload banner")
}

// ============================================================================
// Empty Request Success Test
// ============================================================================

// TestUpdateProfileHandler_EmptyRequestSuccess tests that an empty request succeeds
// This verifies that when no fields are provided, the profile is still created/updated
// with just the $type field
func TestUpdateProfileHandler_EmptyRequestSuccess(t *testing.T) {
	mockClient := &mockPDSClient{
		putRecordURI: "at://did:plc:test123/social.coves.actor.profile/self",
		putRecordCID: "bafyreifake",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	// Empty request - no fields set
	reqBody := UpdateProfileRequest{}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	req = setTestOAuthSession(req, testDID, session)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp UpdateProfileResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "at://did:plc:test123/social.coves.actor.profile/self", resp.URI)
	assert.Equal(t, "bafyreifake", resp.CID)
}

// TestUpdateProfileHandler_WritesTheRecordTheConsumerReadsBack asserts the SHAPE
// of the profile record the handler puts in the user's repo — not that the
// request succeeded, which every other test here already covers.
//
// # WHY THIS IS THE ASSERTION THAT WAS MISSING
//
// The handler's job ends at a repo write; everything a user sees afterwards
// comes from the firehose consumer reading that record back
// (jetstream.handleProfileUpdate → extractBlobCID → users.avatar_cid → a
// hydrated URL on getProfile). The two sides agree on nothing but a JSON shape,
// and neither has the other in scope: this package's tests mocked PutRecord and
// discarded the record, and the consumer's tests build their own record
// literals. So a handler that uploaded a blob correctly and then embedded the
// reference under the wrong key, or flattened it to a bare CID string, produced
// a 200 here, a valid-looking record on the PDS, and an avatar that silently
// never appeared — with no failing test anywhere.
//
// tests/integration/user_profile_avatar_e2e_test.go was the only thing covering
// it, at 1,022 lines and four hand-dialled websockets, and it covered it by
// accident: it watched the real firehose event go past and then re-implemented
// the consumer's extraction inside the test body. This is that claim, stated
// directly. Its other half — that a record of this shape really does reach
// getProfile as a working image URL — is tests/e2e/user_contract_test.go.
func TestUpdateProfileHandler_WritesTheRecordTheConsumerReadsBack(t *testing.T) {
	const avatarCID = "bafyavatartest"
	const bannerCID = "bafybannertest"

	mockClient := &mockPDSClient{
		uploadBlobRef: &blobs.BlobRef{
			Type:     "blob",
			Ref:      map[string]string{"$link": avatarCID},
			MimeType: "image/png",
			Size:     1000,
		},
		putRecordURI: "at://did:plc:testuser123/social.coves.actor.profile/self",
		putRecordCID: "bafyreifake",
	}
	handler := NewUpdateProfileHandlerWithFactory(createMockFactory(mockClient, nil))

	body, _ := json.Marshal(UpdateProfileRequest{
		DisplayName:    strPtr("Written Through"),
		Bio:            strPtr("and read back by the consumer"),
		AvatarBlob:     []byte("fake image data"),
		AvatarMimeType: "image/png",
	})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	const testDID = "did:plc:testuser123"
	req = setTestOAuthSession(req, testDID, createTestOAuthSession(testDID))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Where it was written. rkey "self" is not a convention the handler is free
	// to change: the profile is a singleton record and the consumer, the
	// backfill path and every other client all address it by that key.
	assert.Equal(t, "social.coves.actor.profile", mockClient.putRecordCollection)
	assert.Equal(t, "self", mockClient.putRecordRKey)

	// What was written. Round-tripped through JSON rather than type-asserted,
	// because JSON is what the PDS stores and what the consumer decodes — a
	// field with a Go name that marshals to the wrong key would pass a
	// type-assertion and fail in production.
	encoded, err := json.Marshal(mockClient.putRecordValue)
	assert.NoError(t, err)
	var record map[string]any
	assert.NoError(t, json.Unmarshal(encoded, &record))

	assert.Equal(t, "social.coves.actor.profile", record["$type"])
	assert.Equal(t, "Written Through", record["displayName"])
	assert.Equal(t, "and read back by the consumer", record["description"],
		"the bio is called `bio` on the request and `description` in the record, and the "+
			"consumer reads `description` back into the bio column: three names for one field, "+
			"and this is the only place all three are in scope at once")

	// The blob reference, in the shape jetstream.extractBlobCID insists on:
	// $type == "blob" and a string at ref.$link. Anything else and the consumer
	// declines the ref — silently, because a malformed picture is not worth
	// failing a profile event over.
	avatar, ok := record["avatar"].(map[string]any)
	assert.True(t, ok, "the avatar must be an object; a bare CID string is not a blob ref and "+
		"the consumer would ignore it")
	assert.Equal(t, "blob", avatar["$type"])
	assert.Equal(t, "image/png", avatar["mimeType"])
	ref, ok := avatar["ref"].(map[string]any)
	assert.True(t, ok, "the blob ref's `ref` must be an object holding $link")
	assert.Equal(t, avatarCID, ref["$link"],
		"the record must name the CID the PDS returned from uploadBlob: any other value points "+
			"at bytes that do not exist and the image URL 502s")

	// A field the request did not set must be absent, not present and empty.
	// The consumer treats an ABSENT key as "leave it alone" and an empty string
	// as "clear it" (handleProfileUpdate builds a nil pointer for the former),
	// so an empty banner emitted here would wipe a banner the user still has.
	_, hasBanner := record["banner"]
	assert.False(t, hasBanner,
		"a request that did not touch the banner emitted a banner key: the consumer reads an "+
			"empty value as an instruction to clear the stored one")
	assert.NotContains(t, record, bannerCID)
}
