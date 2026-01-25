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
)

// mockPDSClient implements pds.Client for testing error paths
type mockPDSClient struct {
	uploadBlobError error
	uploadBlobRef   *blobs.BlobRef
	putRecordError  error
	putRecordURI    string
	putRecordCID    string
}

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
	return nil, nil
}

func (m *mockPDSClient) PutRecord(_ context.Context, _ string, _ string, _ any, _ string) (string, string, error) {
	if m.putRecordError != nil {
		return "", "", m.putRecordError
	}
	return m.putRecordURI, m.putRecordCID, nil
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

// createMockFactory creates a PDSClientFactory that returns the given mock client
func createMockFactory(client pds.Client, err error) PDSClientFactory {
	return func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.Client, error) {
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
	return NewUpdateProfileHandlerWithFactory(func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.Client, error) {
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

	longName := strings.Repeat("a", MaxDisplayNameLength+1)
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

	longBio := strings.Repeat("a", MaxBioLength+1)
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
	handler := NewUpdateProfileHandlerWithFactory(func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.Client, error) {
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

// TestUpdateProfileHandler_PutRecordUnauthorized tests PutRecord auth error
func TestUpdateProfileHandler_PutRecordUnauthorized(t *testing.T) {
	mockClient := &mockPDSClient{
		putRecordError: pds.ErrUnauthorized,
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

// TestUpdateProfileHandler_PutRecordRateLimited tests PutRecord rate limiting
func TestUpdateProfileHandler_PutRecordRateLimited(t *testing.T) {
	mockClient := &mockPDSClient{
		putRecordError: pds.ErrRateLimited,
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

// TestUpdateProfileHandler_PutRecordPayloadTooLarge tests PutRecord payload size error
func TestUpdateProfileHandler_PutRecordPayloadTooLarge(t *testing.T) {
	mockClient := &mockPDSClient{
		putRecordError: pds.ErrPayloadTooLarge,
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

// TestUpdateProfileHandler_PutRecordForbidden tests PutRecord forbidden error
func TestUpdateProfileHandler_PutRecordForbidden(t *testing.T) {
	mockClient := &mockPDSClient{
		putRecordError: pds.ErrForbidden,
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
	handler := NewUpdateProfileHandlerWithFactory(func(_ context.Context, _ *oauthlib.ClientSessionData) (pds.Client, error) {
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

// TestUpdateProfileHandler_BannerUploadForbidden tests banner upload forbidden error
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

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "AuthExpired")
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
