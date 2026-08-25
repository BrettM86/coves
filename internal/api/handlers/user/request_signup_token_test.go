package user

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/reqbody"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestRequestSignupToken_Success(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	mockService.On("RequestSignupToken", mock.Anything, mock.MatchedBy(func(req users.RequestSignupTokenRequest) bool {
		return req.TurnstileToken == "tok" && req.RemoteIP != ""
	})).Return(&users.RequestSignupTokenResponse{InviteCode: "pds-invite-123"}, nil)

	body := `{"turnstileToken":"tok"}`
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.requestSignupToken", strings.NewReader(body))
	req.Header.Set("X-Real-IP", "9.9.9.9")

	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "pds-invite-123", resp["inviteCode"])

	mockService.AssertExpectations(t)
}

func TestRequestSignupToken_InvalidCaptcha(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	mockService.On("RequestSignupToken", mock.Anything, mock.Anything).
		Return(nil, &users.InvalidCaptchaError{Reason: "rejected"})

	body := `{"turnstileToken":"bad"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "InvalidCaptcha", resp["error"])
	// Reason should NOT leak to the client
	assert.NotContains(t, resp["message"], "rejected")
}

// Cloudflare outage / transport failure must surface as 503 — distinct from a
// user-side 403 — so clients back off instead of treating it as user error.
func TestRequestSignupToken_CaptchaUnavailable(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	mockService.On("RequestSignupToken", mock.Anything, mock.Anything).
		Return(nil, &users.CaptchaUnavailableError{Reason: "transport"})

	body := `{"turnstileToken":"t"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "CaptchaUnavailable", resp["error"])
	assert.NotContains(t, resp["message"], "transport") // don't leak internals
}

// Misconfig must surface as 503 SignupTokenDisabled — distinct from captcha 403.
func TestRequestSignupToken_Disabled(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	mockService.On("RequestSignupToken", mock.Anything, mock.Anything).
		Return(nil, users.ErrSignupTokenDisabled)

	body := `{"turnstileToken":"t"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "SignupTokenDisabled", resp["error"])
}

// PDS-side transport/decode failure surfaces as 503 PDSUnavailable — distinct
// from InviteMintError (PDS responded with non-2xx → 500) so ops can alert on
// "PDS is unreachable" separately from "PDS rejected our request".
func TestRequestSignupToken_PDSUnavailable(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	mockService.On("RequestSignupToken", mock.Anything, mock.Anything).
		Return(nil, fmt.Errorf("%w: wrapped transport failure", users.ErrPDSAdminUnavailable))

	body := `{"turnstileToken":"t"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "PDSUnavailable", resp["error"])
}

func TestRequestSignupToken_InviteMint(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	mockService.On("RequestSignupToken", mock.Anything, mock.Anything).
		Return(nil, users.NewInviteMintError(500, "boom"))

	body := `{"turnstileToken":"tok"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "InternalServerError", resp["error"])
	assert.NotContains(t, resp["message"], "boom") // don't leak PDS details
}

func TestRequestSignupToken_RejectsGET(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	mockService.AssertNotCalled(t, "RequestSignupToken", mock.Anything, mock.Anything)
}

func TestRequestSignupToken_BadJSON(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	mockService.AssertNotCalled(t, "RequestSignupToken", mock.Anything, mock.Anything)
}

// DisallowUnknownFields() must reject any body containing fields outside the
// declared schema — important for catching client-injected fields like a
// fake `remoteIp`.
func TestRequestSignupToken_RejectsUnknownFields(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	body := `{"turnstileToken":"t","extra":"junk"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	mockService.AssertNotCalled(t, "RequestSignupToken", mock.Anything, mock.Anything)
}

// Body limit must be enforced via http.MaxBytesReader so oversized bodies get a
// proper 413, not a confusing 400 from a truncated-JSON decode error. Anyone
// who downgrades to io.LimitReader will break this test.
func TestRequestSignupToken_BodyLimitEnforced(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	huge := strings.Repeat("A", 8192) // > reqbody.LimitTiny (4096)
	body := fmt.Sprintf(`{"turnstileToken":%q}`, huge)
	require.Greater(t, len(body), 4096)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	mockService.AssertNotCalled(t, "RequestSignupToken", mock.Anything, mock.Anything)
}

// Explicit 413 contract: oversize body returns Payload Too Large, never a 200
// with the giant token forwarded downstream. The body must be parsing-valid up
// to the cap so the failure comes from MaxBytesReader, not a JSON parse error.
func TestRequestSignupToken_BodyOverLimitReturns413(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	// Construct a JSON object whose token field alone exceeds reqbody.LimitTiny.
	huge := strings.Repeat("A", 10000)
	body := fmt.Sprintf(`{"turnstileToken":%q}`, huge)
	require.Greater(t, len(body), int(reqbody.LimitTiny))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "PayloadTooLarge", resp["error"])
	mockService.AssertNotCalled(t, "RequestSignupToken", mock.Anything, mock.Anything)
}

// Trailing data after the JSON object (concatenated objects, padding) is a
// smuggling smell — reject as 400. Catches anyone who removes the dec.More() check.
func TestRequestSignupToken_RejectsTrailingData(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	body := `{"turnstileToken":"tok"}{"turnstileToken":"two"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	mockService.AssertNotCalled(t, "RequestSignupToken", mock.Anything, mock.Anything)
}

// Stronger than "the json:"-" tag drops it": this proves the handler ACTIVELY
// sets RemoteIP from the request headers. With no body field present at all,
// the handler must still populate RemoteIP from X-Real-IP. Bonus: also pin
// down precedence (header IP wins, never trust body).
func TestRequestSignupToken_HandlerOverwritesRemoteIP(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	// Mock matcher requires RemoteIP equal to the header value.
	mockService.On("RequestSignupToken", mock.Anything, mock.MatchedBy(func(req users.RequestSignupTokenRequest) bool {
		return req.RemoteIP == "5.5.5.5"
	})).Return(&users.RequestSignupTokenResponse{InviteCode: "ok"}, nil)

	// Body has NO RemoteIP/remoteIp field at all — proves the handler is the
	// one populating it, not a deserialized field.
	body := `{"turnstileToken":"t"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Real-IP", "5.5.5.5")

	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	mockService.AssertExpectations(t)
}

// Sanity: handler maps unknown service errors as 500.
func TestRequestSignupToken_UnknownErrorIs500(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewRequestSignupTokenHandler(mockService)

	mockService.On("RequestSignupToken", mock.Anything, mock.Anything).
		Return(nil, errors.New("something weird"))

	body := `{"turnstileToken":"t"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleRequestSignupToken(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "InternalServerError", resp["error"])
}
