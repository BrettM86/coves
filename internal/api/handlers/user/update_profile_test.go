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
	"Coves/internal/core/blobs"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockBlobService is a mock implementation of blobs.Service for testing
type MockBlobService struct {
	mock.Mock
}

func (m *MockBlobService) UploadBlobFromURL(ctx context.Context, owner blobs.BlobOwner, imageURL string) (*blobs.BlobRef, error) {
	args := m.Called(ctx, owner, imageURL)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*blobs.BlobRef), args.Error(1)
}

func (m *MockBlobService) UploadBlob(ctx context.Context, owner blobs.BlobOwner, data []byte, mimeType string) (*blobs.BlobRef, error) {
	args := m.Called(ctx, owner, data, mimeType)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*blobs.BlobRef), args.Error(1)
}

// MockPDSClient is a mock HTTP client for PDS interactions
type MockPDSClient struct {
	mock.Mock
}

// mockRoundTripper implements http.RoundTripper for testing
type mockRoundTripper struct {
	mock.Mock
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	args := m.Called(req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*http.Response), args.Error(1)
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
func setTestOAuthSession(ctx context.Context, userDID string, session *oauthlib.ClientSessionData) context.Context {
	ctx = middleware.SetTestUserDID(ctx, userDID)
	ctx = context.WithValue(ctx, middleware.OAuthSessionKey, session)
	ctx = context.WithValue(ctx, middleware.UserAccessToken, session.AccessToken)
	return ctx
}

// TestUpdateProfileHandler_Unauthenticated tests that unauthenticated requests return 401
func TestUpdateProfileHandler_Unauthenticated(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

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

	mockBlobService.AssertNotCalled(t, "UploadBlob", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestUpdateProfileHandler_MissingOAuthSession tests that missing OAuth session returns 401
func TestUpdateProfileHandler_MissingOAuthSession(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

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
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", strings.NewReader("not valid json"))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid request body")
}

// TestUpdateProfileHandler_AvatarSizeExceedsLimit tests that avatar over 1MB is rejected
func TestUpdateProfileHandler_AvatarSizeExceedsLimit(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

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
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Avatar exceeds 1MB limit")

	mockBlobService.AssertNotCalled(t, "UploadBlob", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestUpdateProfileHandler_BannerSizeExceedsLimit tests that banner over 2MB is rejected
func TestUpdateProfileHandler_BannerSizeExceedsLimit(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

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
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Banner exceeds 2MB limit")

	mockBlobService.AssertNotCalled(t, "UploadBlob", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestUpdateProfileHandler_InvalidAvatarMimeType tests that invalid avatar mime type is rejected
func TestUpdateProfileHandler_InvalidAvatarMimeType(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

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
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid avatar mime type")
}

// TestUpdateProfileHandler_InvalidBannerMimeType tests that invalid banner mime type is rejected
func TestUpdateProfileHandler_InvalidBannerMimeType(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

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
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid banner mime type")
}

// TestUpdateProfileHandler_ValidMimeTypes tests that all valid mime types are accepted
func TestUpdateProfileHandler_ValidMimeTypes(t *testing.T) {
	validMimeTypes := []string{"image/png", "image/jpeg", "image/webp"}

	for _, mimeType := range validMimeTypes {
		t.Run(mimeType, func(t *testing.T) {
			mockBlobService := new(MockBlobService)

			// Set up mock PDS server for putRecord
			mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
					"cid": "bafyreicid123",
				})
			}))
			defer mockPDS.Close()

			handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

			avatarData := []byte("fake avatar image data")
			expectedBlobRef := &blobs.BlobRef{
				Type:     "blob",
				Ref:      map[string]string{"$link": "bafyreiabc123"},
				MimeType: mimeType,
				Size:     len(avatarData),
			}

			mockBlobService.On("UploadBlob", mock.Anything, mock.Anything, avatarData, mimeType).
				Return(expectedBlobRef, nil)

			reqBody := UpdateProfileRequest{
				DisplayName:    strPtr("Test User"),
				AvatarBlob:     avatarData,
				AvatarMimeType: mimeType,
			}
			body, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			testDID := "did:plc:testuser123"
			session := createTestOAuthSession(testDID)
			session.HostURL = mockPDS.URL // Point to mock PDS
			ctx := setTestOAuthSession(req.Context(), testDID, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Should succeed or fail at PDS call, not at validation
			// We just verify the mime type validation passed
			assert.NotEqual(t, http.StatusBadRequest, w.Code)
			mockBlobService.AssertExpectations(t)
		})
	}
}

// TestUpdateProfileHandler_AvatarBlobUploadFailure tests handling of blob upload failure
func TestUpdateProfileHandler_AvatarBlobUploadFailure(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

	avatarData := []byte("fake avatar image data")
	mockBlobService.On("UploadBlob", mock.Anything, mock.Anything, avatarData, "image/jpeg").
		Return(nil, errors.New("PDS upload failed"))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		AvatarBlob:     avatarData,
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to upload avatar")

	mockBlobService.AssertExpectations(t)
}

// TestUpdateProfileHandler_BannerBlobUploadFailure tests handling of banner blob upload failure
func TestUpdateProfileHandler_BannerBlobUploadFailure(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

	bannerData := []byte("fake banner image data")
	mockBlobService.On("UploadBlob", mock.Anything, mock.Anything, bannerData, "image/png").
		Return(nil, errors.New("PDS upload failed"))

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Test User"),
		BannerBlob:     bannerData,
		BannerMimeType: "image/png",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to upload banner")

	mockBlobService.AssertExpectations(t)
}

// TestUpdateProfileHandler_PartialUpdateDisplayNameOnly tests updating only displayName (no blobs)
func TestUpdateProfileHandler_PartialUpdateDisplayNameOnly(t *testing.T) {
	mockBlobService := new(MockBlobService)

	// Mock PDS server for putRecord
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify it's the right endpoint
		assert.Equal(t, "/xrpc/com.atproto.repo.putRecord", r.URL.Path)

		// Parse request body
		var putReq map[string]interface{}
		json.NewDecoder(r.Body).Decode(&putReq)

		// Verify record structure
		record, ok := putReq["record"].(map[string]interface{})
		assert.True(t, ok, "record should exist")
		assert.Equal(t, "app.bsky.actor.profile", record["$type"])
		assert.Equal(t, "Updated Display Name", record["displayName"])
		assert.Nil(t, record["avatar"], "avatar should not be set")
		assert.Nil(t, record["banner"], "banner should not be set")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
			"cid": "bafyreicid123",
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Updated Display Name"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response UpdateProfileResponse
	json.Unmarshal(w.Body.Bytes(), &response)
	assert.Contains(t, response.URI, "did:plc:testuser123")
	assert.NotEmpty(t, response.CID)

	// No blob uploads should have been called
	mockBlobService.AssertNotCalled(t, "UploadBlob", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestUpdateProfileHandler_PartialUpdateBioOnly tests updating only bio (description)
func TestUpdateProfileHandler_PartialUpdateBioOnly(t *testing.T) {
	mockBlobService := new(MockBlobService)

	// Mock PDS server for putRecord
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var putReq map[string]interface{}
		json.NewDecoder(r.Body).Decode(&putReq)

		record := putReq["record"].(map[string]interface{})
		assert.Equal(t, "This is my updated bio", record["description"])
		assert.Nil(t, record["displayName"], "displayName should not be set if not provided")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
			"cid": "bafyreicid123",
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		Bio: strPtr("This is my updated bio"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	mockBlobService.AssertNotCalled(t, "UploadBlob", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestUpdateProfileHandler_FullUpdate tests updating displayName, bio, avatar, and banner
func TestUpdateProfileHandler_FullUpdate(t *testing.T) {
	mockBlobService := new(MockBlobService)

	avatarData := []byte("avatar image data")
	bannerData := []byte("banner image data")

	avatarBlobRef := &blobs.BlobRef{
		Type:     "blob",
		Ref:      map[string]string{"$link": "bafyreiavatarcid"},
		MimeType: "image/jpeg",
		Size:     len(avatarData),
	}
	bannerBlobRef := &blobs.BlobRef{
		Type:     "blob",
		Ref:      map[string]string{"$link": "bafyreibannercid"},
		MimeType: "image/png",
		Size:     len(bannerData),
	}

	mockBlobService.On("UploadBlob", mock.Anything, mock.Anything, avatarData, "image/jpeg").
		Return(avatarBlobRef, nil)
	mockBlobService.On("UploadBlob", mock.Anything, mock.Anything, bannerData, "image/png").
		Return(bannerBlobRef, nil)

	// Mock PDS server for putRecord
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var putReq map[string]interface{}
		json.NewDecoder(r.Body).Decode(&putReq)

		record := putReq["record"].(map[string]interface{})
		assert.Equal(t, "Full Update User", record["displayName"])
		assert.Equal(t, "Updated bio with full profile", record["description"])
		assert.NotNil(t, record["avatar"], "avatar should be set")
		assert.NotNil(t, record["banner"], "banner should be set")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
			"cid": "bafyreifullcid",
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		DisplayName:    strPtr("Full Update User"),
		Bio:            strPtr("Updated bio with full profile"),
		AvatarBlob:     avatarData,
		AvatarMimeType: "image/jpeg",
		BannerBlob:     bannerData,
		BannerMimeType: "image/png",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response UpdateProfileResponse
	json.Unmarshal(w.Body.Bytes(), &response)
	assert.Contains(t, response.URI, "did:plc:testuser123")
	assert.Equal(t, "bafyreifullcid", response.CID)

	mockBlobService.AssertExpectations(t)
}

// TestUpdateProfileHandler_PDSPutRecordFailure tests handling of PDS putRecord failure
func TestUpdateProfileHandler_PDSPutRecordFailure(t *testing.T) {
	mockBlobService := new(MockBlobService)

	// Mock PDS server that returns an error
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "InternalError",
			"message": "Failed to update record",
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to update profile")
}

// TestUpdateProfileHandler_MethodNotAllowed tests that non-POST methods are rejected
func TestUpdateProfileHandler_MethodNotAllowed(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/xrpc/social.coves.actor.updateProfile", nil)

			testDID := "did:plc:testuser123"
			session := createTestOAuthSession(testDID)
			ctx := setTestOAuthSession(req.Context(), testDID, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
		})
	}
}

// TestUpdateProfileHandler_AvatarBlobWithoutMimeType tests that providing blob without mime type fails
func TestUpdateProfileHandler_AvatarBlobWithoutMimeType(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

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
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "mime type")
}

// TestUpdateProfileHandler_BannerBlobWithoutMimeType tests that providing banner without mime type fails
func TestUpdateProfileHandler_BannerBlobWithoutMimeType(t *testing.T) {
	mockBlobService := new(MockBlobService)
	handler := NewUpdateProfileHandler(mockBlobService, nil)

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
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "mime type")
}

// TestUpdateProfileHandler_UserBlobOwnerInterface tests that userBlobOwner correctly implements BlobOwner
func TestUpdateProfileHandler_UserBlobOwnerInterface(t *testing.T) {
	owner := &userBlobOwner{
		pdsURL:      "https://test.pds.example",
		accessToken: "test-token-123",
	}

	// Verify interface compliance
	var _ blobs.BlobOwner = owner

	assert.Equal(t, "https://test.pds.example", owner.GetPDSURL())
	assert.Equal(t, "test-token-123", owner.GetPDSAccessToken())
}

// TestUpdateProfileHandler_EmptyRequest tests that empty request body is handled
func TestUpdateProfileHandler_EmptyRequest(t *testing.T) {
	mockBlobService := new(MockBlobService)

	// Mock PDS server - even empty update should work
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
			"cid": "bafyreicid123",
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	// Empty JSON object
	reqBody := UpdateProfileRequest{}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Empty update is valid - just puts an empty profile record
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestUpdateProfileHandler_PDSURLFromSession tests that PDS URL is correctly extracted from OAuth session
func TestUpdateProfileHandler_PDSURLFromSession(t *testing.T) {
	mockBlobService := new(MockBlobService)

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request was received at the mock PDS
		assert.NotEmpty(t, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
			"cid": "bafyreicid123",
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	// Use the mock server URL
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestUpdateProfileHandler_AvatarExactly1MB tests boundary condition - avatar exactly 1MB should be accepted
func TestUpdateProfileHandler_AvatarExactly1MB(t *testing.T) {
	mockBlobService := new(MockBlobService)

	// Create avatar blob exactly 1MB (1,000,000 bytes)
	avatarData := make([]byte, 1_000_000)

	expectedBlobRef := &blobs.BlobRef{
		Type:     "blob",
		Ref:      map[string]string{"$link": "bafyreiabc123"},
		MimeType: "image/jpeg",
		Size:     len(avatarData),
	}
	mockBlobService.On("UploadBlob", mock.Anything, mock.Anything, avatarData, "image/jpeg").
		Return(expectedBlobRef, nil)

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
			"cid": "bafyreicid123",
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		AvatarBlob:     avatarData,
		AvatarMimeType: "image/jpeg",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	mockBlobService.AssertExpectations(t)
}

// TestUpdateProfileHandler_BannerExactly2MB tests boundary condition - banner exactly 2MB should be accepted
func TestUpdateProfileHandler_BannerExactly2MB(t *testing.T) {
	mockBlobService := new(MockBlobService)

	// Create banner blob exactly 2MB (2,000,000 bytes)
	bannerData := make([]byte, 2_000_000)

	expectedBlobRef := &blobs.BlobRef{
		Type:     "blob",
		Ref:      map[string]string{"$link": "bafyreiabc123"},
		MimeType: "image/png",
		Size:     len(bannerData),
	}
	mockBlobService.On("UploadBlob", mock.Anything, mock.Anything, bannerData, "image/png").
		Return(expectedBlobRef, nil)

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
			"cid": "bafyreicid123",
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		BannerBlob:     bannerData,
		BannerMimeType: "image/png",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	mockBlobService.AssertExpectations(t)
}

// TestUpdateProfileHandler_PDSNetworkError tests handling of network errors when calling PDS
func TestUpdateProfileHandler_PDSNetworkError(t *testing.T) {
	mockBlobService := new(MockBlobService)

	// Create a handler with a client that will fail
	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	// Use an invalid URL that will fail connection
	session.HostURL = "http://localhost:1" // Port 1 is typically refused
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to update profile")
}

// TestUpdateProfileHandler_ResponseFormat tests that response matches expected format
func TestUpdateProfileHandler_ResponseFormat(t *testing.T) {
	mockBlobService := new(MockBlobService)

	expectedURI := "at://did:plc:testuser123/app.bsky.actor.profile/self"
	expectedCID := "bafyreicid456"

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": expectedURI,
			"cid": expectedCID,
		})
	}))
	defer mockPDS.Close()

	handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

	reqBody := UpdateProfileRequest{
		DisplayName: strPtr("Test User"),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	testDID := "did:plc:testuser123"
	session := createTestOAuthSession(testDID)
	session.HostURL = mockPDS.URL
	ctx := setTestOAuthSession(req.Context(), testDID, session)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response UpdateProfileResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)
	assert.Equal(t, expectedURI, response.URI)
	assert.Equal(t, expectedCID, response.CID)
}

// Helper function to create string pointers
func strPtr(s string) *string {
	return &s
}

// TestUserBlobOwner_ImplementsBlobOwnerInterface verifies interface compliance at compile time
func TestUserBlobOwner_ImplementsBlobOwnerInterface(t *testing.T) {
	// This test ensures at compile time that userBlobOwner implements blobs.BlobOwner
	var owner blobs.BlobOwner = &userBlobOwner{
		pdsURL:      "https://test.example",
		accessToken: "token",
	}
	assert.NotNil(t, owner)
}

// TestUpdateProfileHandler_PDSReturnsEmptyURIOrCID tests handling when PDS returns 200 but with empty URI or CID
func TestUpdateProfileHandler_PDSReturnsEmptyURIOrCID(t *testing.T) {
	testCases := []struct {
		name     string
		response map[string]interface{}
	}{
		{
			name: "empty URI",
			response: map[string]interface{}{
				"uri": "",
				"cid": "bafyreicid123",
			},
		},
		{
			name: "empty CID",
			response: map[string]interface{}{
				"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
				"cid": "",
			},
		},
		{
			name: "missing URI",
			response: map[string]interface{}{
				"cid": "bafyreicid123",
			},
		},
		{
			name: "missing CID",
			response: map[string]interface{}{
				"uri": "at://did:plc:testuser123/app.bsky.actor.profile/self",
			},
		},
		{
			name:     "both empty",
			response: map[string]interface{}{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockBlobService := new(MockBlobService)

			// Mock PDS server that returns 200 but with empty/missing fields
			mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(tc.response)
			}))
			defer mockPDS.Close()

			handler := NewUpdateProfileHandler(mockBlobService, http.DefaultClient)

			reqBody := UpdateProfileRequest{
				DisplayName: strPtr("Test User"),
			}
			body, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.updateProfile", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			testDID := "did:plc:testuser123"
			session := createTestOAuthSession(testDID)
			session.HostURL = mockPDS.URL
			ctx := setTestOAuthSession(req.Context(), testDID, session)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Should return an internal server error because URI/CID are required
			assert.Equal(t, http.StatusInternalServerError, w.Code)
			assert.Contains(t, w.Body.String(), "PDSError")
		})
	}
}

