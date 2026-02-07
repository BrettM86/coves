package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestHandleMe_Success(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewMeHandler(mockService)

	testDID := "did:plc:testme123"
	profile := &users.ProfileViewDetailed{
		DID:         testDID,
		Handle:      "alice.test",
		CreatedAt:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		DisplayName: "Alice",
		Bio:         "Hello world",
		Avatar:      "https://cdn.example.com/avatar.jpg",
		Stats: &users.ProfileStats{
			PostCount:      10,
			CommentCount:   5,
			CommunityCount: 3,
			Reputation:     42,
		},
	}
	mockService.On("GetProfile", mock.Anything, testDID).Return(profile, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleMe(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	// Unmarshal into raw map to verify wire-format JSON key names
	var raw map[string]json.RawMessage
	err := json.Unmarshal(w.Body.Bytes(), &raw)
	assert.NoError(t, err)

	// Bio field has json:"description" tag — verify the wire key is "description", not "bio"
	assert.Contains(t, raw, "description", "Bio should serialize as 'description' per json tag")
	assert.NotContains(t, raw, "bio", "'bio' key should not appear in JSON output")

	// Also verify typed deserialization
	var resp users.ProfileViewDetailed
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, testDID, resp.DID)
	assert.Equal(t, "alice.test", resp.Handle)
	assert.Equal(t, "Alice", resp.DisplayName)
	assert.Equal(t, "Hello world", resp.Bio)
	assert.Equal(t, 10, resp.Stats.PostCount)

	mockService.AssertExpectations(t)
}

func TestHandleMe_NilStats(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewMeHandler(mockService)

	testDID := "did:plc:nostats"
	profile := &users.ProfileViewDetailed{
		DID:       testDID,
		Handle:    "bob.test",
		CreatedAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
		Stats:     nil,
	}
	mockService.On("GetProfile", mock.Anything, testDID).Return(profile, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleMe(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var raw map[string]json.RawMessage
	err := json.Unmarshal(w.Body.Bytes(), &raw)
	assert.NoError(t, err)

	// stats should be omitted when nil (omitempty tag)
	assert.NotContains(t, raw, "stats", "nil Stats should be omitted from JSON")

	mockService.AssertExpectations(t)
}

func TestHandleMe_Unauthenticated(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewMeHandler(mockService)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)

	w := httptest.NewRecorder()
	handler.HandleMe(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Validate the full error JSON structure
	var errResp map[string]string
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	assert.NoError(t, err)
	assert.Equal(t, "AuthRequired", errResp["error"])
	assert.Equal(t, "Authentication required", errResp["message"])

	mockService.AssertNotCalled(t, "GetProfile", mock.Anything, mock.Anything)
}

func TestHandleMe_UserNotFound(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewMeHandler(mockService)

	testDID := "did:plc:nonexistent"
	mockService.On("GetProfile", mock.Anything, testDID).Return(nil, users.ErrUserNotFound)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleMe(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "AccountNotFound")

	mockService.AssertExpectations(t)
}

func TestHandleMe_InternalError(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewMeHandler(mockService)

	testDID := "did:plc:erroruser"
	mockService.On("GetProfile", mock.Anything, testDID).Return(nil, assert.AnError)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleMe(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "InternalServerError")

	mockService.AssertExpectations(t)
}

func TestHandleMe_Timeout(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewMeHandler(mockService)

	testDID := "did:plc:timeoutuser"
	mockService.On("GetProfile", mock.Anything, testDID).Return(nil, context.DeadlineExceeded)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleMe(w, req)

	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.Contains(t, w.Body.String(), "Timeout")

	mockService.AssertExpectations(t)
}

func TestHandleMe_ContextCanceled(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewMeHandler(mockService)

	testDID := "did:plc:canceluser"
	mockService.On("GetProfile", mock.Anything, testDID).Return(nil, context.Canceled)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleMe(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "RequestCanceled")

	mockService.AssertExpectations(t)
}
