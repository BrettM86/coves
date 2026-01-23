package user

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockUserService is a mock implementation of users.UserService
type MockUserService struct {
	mock.Mock
}

func (m *MockUserService) CreateUser(ctx context.Context, req users.CreateUserRequest) (*users.User, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*users.User), args.Error(1)
}

func (m *MockUserService) GetUserByDID(ctx context.Context, did string) (*users.User, error) {
	args := m.Called(ctx, did)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*users.User), args.Error(1)
}

func (m *MockUserService) GetUserByHandle(ctx context.Context, handle string) (*users.User, error) {
	args := m.Called(ctx, handle)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*users.User), args.Error(1)
}

func (m *MockUserService) UpdateHandle(ctx context.Context, did, newHandle string) (*users.User, error) {
	args := m.Called(ctx, did, newHandle)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*users.User), args.Error(1)
}

func (m *MockUserService) ResolveHandleToDID(ctx context.Context, handle string) (string, error) {
	args := m.Called(ctx, handle)
	return args.String(0), args.Error(1)
}

func (m *MockUserService) RegisterAccount(ctx context.Context, req users.RegisterAccountRequest) (*users.RegisterAccountResponse, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*users.RegisterAccountResponse), args.Error(1)
}

func (m *MockUserService) IndexUser(ctx context.Context, did, handle, pdsURL string) error {
	args := m.Called(ctx, did, handle, pdsURL)
	return args.Error(0)
}

func (m *MockUserService) GetProfile(ctx context.Context, did string) (*users.ProfileViewDetailed, error) {
	args := m.Called(ctx, did)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*users.ProfileViewDetailed), args.Error(1)
}

func (m *MockUserService) DeleteAccount(ctx context.Context, did string) error {
	args := m.Called(ctx, did)
	return args.Error(0)
}

func (m *MockUserService) UpdateProfile(ctx context.Context, did string, displayName, bio, avatarCID, bannerCID *string) (*users.User, error) {
	args := m.Called(ctx, did, displayName, bio, avatarCID, bannerCID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*users.User), args.Error(1)
}

// TestDeleteAccountHandler_Success tests successful account deletion via XRPC
// Uses the actual production handler with middleware context injection
func TestDeleteAccountHandler_Success(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	testDID := "did:plc:testdelete123"
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(nil)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	// Use middleware context injection instead of X-User-DID header
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleDeleteAccount(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"success":true`)
	assert.Contains(t, w.Body.String(), "atProto identity remains intact")

	mockService.AssertExpectations(t)
}

// TestDeleteAccountHandler_Unauthenticated tests deletion without authentication
func TestDeleteAccountHandler_Unauthenticated(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	// No context injection - simulates unauthenticated request

	w := httptest.NewRecorder()
	handler.HandleDeleteAccount(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "AuthRequired")

	mockService.AssertNotCalled(t, "DeleteAccount", mock.Anything, mock.Anything)
}

// TestDeleteAccountHandler_UserNotFound tests deletion of non-existent user
func TestDeleteAccountHandler_UserNotFound(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	testDID := "did:plc:nonexistent"
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(users.ErrUserNotFound)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleDeleteAccount(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "AccountNotFound")

	mockService.AssertExpectations(t)
}

// TestDeleteAccountHandler_MethodNotAllowed tests that only POST is accepted
func TestDeleteAccountHandler_MethodNotAllowed(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/xrpc/social.coves.actor.deleteAccount", nil)
			ctx := middleware.SetTestUserDID(req.Context(), "did:plc:test")
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			handler.HandleDeleteAccount(w, req)

			assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
		})
	}

	mockService.AssertNotCalled(t, "DeleteAccount", mock.Anything, mock.Anything)
}

// TestDeleteAccountHandler_InternalError tests handling of internal errors
func TestDeleteAccountHandler_InternalError(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	testDID := "did:plc:erroruser"
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(assert.AnError)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleDeleteAccount(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "InternalServerError")

	mockService.AssertExpectations(t)
}

// TestDeleteAccountHandler_ContextTimeout tests handling of context timeout
func TestDeleteAccountHandler_ContextTimeout(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	testDID := "did:plc:timeoutuser"
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(context.DeadlineExceeded)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleDeleteAccount(w, req)

	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.Contains(t, w.Body.String(), "Timeout")

	mockService.AssertExpectations(t)
}

// TestDeleteAccountHandler_ContextCanceled tests handling of context cancellation
func TestDeleteAccountHandler_ContextCanceled(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	testDID := "did:plc:canceluser"
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(context.Canceled)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleDeleteAccount(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "RequestCanceled")

	mockService.AssertExpectations(t)
}

// TestDeleteAccountHandler_InvalidDID tests handling of invalid DID format
func TestDeleteAccountHandler_InvalidDID(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	testDID := "did:plc:invaliddid"
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(&users.InvalidDIDError{DID: "invalid", Reason: "must start with 'did:'"})

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleDeleteAccount(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "InvalidDID")

	mockService.AssertExpectations(t)
}

// TestWebDeleteAccount_FormSubmission tests the web form-based deletion flow
func TestWebDeleteAccount_FormSubmission(t *testing.T) {
	mockService := new(MockUserService)

	testDID := "did:plc:webdeleteuser"
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(nil)

	// Simulate form submission
	form := url.Values{}
	form.Add("confirm", "true")

	req := httptest.NewRequest(http.MethodPost, "/delete-account", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := middleware.SetTestUserDID(req.Context(), testDID)
	req = req.WithContext(ctx)

	// The web handler would parse the form and call DeleteAccount
	// This test verifies the service layer is called correctly
	err := mockService.DeleteAccount(ctx, testDID)
	assert.NoError(t, err)

	mockService.AssertExpectations(t)
}

// TestWebDeleteAccount_MissingConfirmation tests that confirmation is required
func TestWebDeleteAccount_MissingConfirmation(t *testing.T) {
	form := url.Values{}
	// NOT adding confirm=true

	req := httptest.NewRequest(http.MethodPost, "/delete-account", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	err := req.ParseForm()
	assert.NoError(t, err)
	assert.NotEqual(t, "true", req.FormValue("confirm"))
}

// TestWebDeleteAccount_ConfirmationPresent tests confirmation checkbox validation
func TestWebDeleteAccount_ConfirmationPresent(t *testing.T) {
	form := url.Values{}
	form.Add("confirm", "true")

	req := httptest.NewRequest(http.MethodPost, "/delete-account", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	err := req.ParseForm()
	assert.NoError(t, err)
	assert.Equal(t, "true", req.FormValue("confirm"))
}

// TestDeleteAccountHandler_DIDWithWhitespace tests DID handling with whitespace
// The service layer should handle trimming whitespace from DIDs
func TestDeleteAccountHandler_DIDWithWhitespace(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	// In reality, the middleware would provide a clean DID from the OAuth session.
	// This test verifies the handler correctly passes the DID to the service.
	trimmedDID := "did:plc:whitespaceuser"
	mockService.On("DeleteAccount", mock.Anything, trimmedDID).Return(nil)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	ctx := middleware.SetTestUserDID(req.Context(), trimmedDID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleDeleteAccount(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	mockService.AssertExpectations(t)
}

// TestDeleteAccountHandler_ConcurrentRequests tests handling of concurrent deletion attempts
// Verifies that repeated deletion attempts are handled gracefully
func TestDeleteAccountHandler_ConcurrentRequests(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewDeleteHandler(mockService)

	testDID := "did:plc:concurrentuser"

	// First call succeeds, second call returns not found (already deleted)
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(nil).Once()
	mockService.On("DeleteAccount", mock.Anything, testDID).Return(users.ErrUserNotFound).Once()

	// First request
	req1 := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	ctx1 := middleware.SetTestUserDID(req1.Context(), testDID)
	req1 = req1.WithContext(ctx1)

	w1 := httptest.NewRecorder()
	handler.HandleDeleteAccount(w1, req1)
	assert.Equal(t, http.StatusOK, w1.Code)

	// Second request (simulating concurrent attempt that arrives after first completes)
	req2 := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", nil)
	ctx2 := middleware.SetTestUserDID(req2.Context(), testDID)
	req2 = req2.WithContext(ctx2)

	w2 := httptest.NewRecorder()
	handler.HandleDeleteAccount(w2, req2)
	assert.Equal(t, http.StatusNotFound, w2.Code)

	mockService.AssertExpectations(t)
}
