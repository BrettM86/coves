package user

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// decodeXRPCBody reads the handler's response as the flat JSON object every
// XRPC reply on this route is: {"did":...} on success, {"error","message"} on
// failure. Decoding rather than substring-matching is what makes "the body is
// exactly {"did":"..."}" assertable — a handler that also leaked the handle,
// the PDS URL, or a viewer field would pass a Contains check.
func decodeXRPCBody(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not a flat JSON object: %v (body=%q)", err, w.Body.String())
	}
	return body
}

// resolveHandleRequest builds a GET carrying the raw query string, so a test
// can submit a handle exactly as a client would rather than as url.Values
// re-encodes it.
func resolveHandleRequest(rawQuery string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/xrpc/com.atproto.identity.resolveHandle?"+rawQuery, nil)
}

// TestResolveHandleHandler_Success pins the whole success contract, including
// the one thing that is easy to "fix" wrongly: the handle reaches the service
// byte-for-byte as submitted. Handle normalisation — the service lowercases
// and trims surrounding whitespace, and does nothing else — is the service's
// job and is tested there; a handler that
// lowercased on the way past would silently make the service's own
// normalisation untestable through this route and would hide a service that
// had stopped normalising at all.
func TestResolveHandleHandler_Success(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	mockService.On("ResolveHandleToDID", mock.Anything, "Alice.Test").Return("did:plc:abc", nil)

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest("handle=Alice.Test"))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, map[string]string{"did": "did:plc:abc"}, decodeXRPCBody(t, w))

	mockService.AssertExpectations(t)
}

// TestResolveHandleHandler_MissingHandle: the parameter is required by the
// lexicon, so its absence is answered here and never becomes a resolution
// attempt for the empty string.
func TestResolveHandleHandler_MissingHandle(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest(""))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "InvalidRequest", decodeXRPCBody(t, w)["error"])

	mockService.AssertNotCalled(t, "ResolveHandleToDID", mock.Anything, mock.Anything)
}

// TestResolveHandleHandler_WhitespaceHandle: a present-but-blank parameter is
// the same non-request as an absent one. Rejecting it here is what stops an
// unauthenticated caller turning whitespace into DNS and PLC lookups.
func TestResolveHandleHandler_WhitespaceHandle(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest("handle=%20%20%20"))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "InvalidRequest", decodeXRPCBody(t, w)["error"])

	mockService.AssertNotCalled(t, "ResolveHandleToDID", mock.Anything, mock.Anything)
}

// TestResolveHandleHandler_NotFound: the service's not-found is WRAPPED here on
// purpose. Any service call site may add context to the error on its way up, so
// the handler must classify with errors.Is and not by identity comparison.
//
// The reply is a 400 rather than a 404 because that is what the atProto
// resolveHandle contract says an unresolvable handle is, and the message is
// deliberately generic: it is the same text for every unresolvable handle, so
// an anonymous caller cannot use the wording to tell "no such account" apart
// from "not a handle we will look up".
func TestResolveHandleHandler_NotFound(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	wrapped := fmt.Errorf("resolve: %w", users.ErrUserNotFound)
	mockService.On("ResolveHandleToDID", mock.Anything, "ghost.test").Return("", wrapped)

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest("handle=ghost.test"))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	body := decodeXRPCBody(t, w)
	assert.Equal(t, "InvalidRequest", body["error"])
	assert.Equal(t, "Unable to resolve handle", body["message"])

	mockService.AssertExpectations(t)
}

// TestResolveHandleHandler_InvalidHandle: a syntactically invalid handle is the
// caller's mistake, and unlike the not-found case its message is the service's
// own text — the Reason says which rule was broken, which is the whole value of
// the error to a client fixing its input.
func TestResolveHandleHandler_InvalidHandle(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	invalid := &users.InvalidHandleError{
		Handle: "bad",
		Reason: "handle must be domain-like (e.g., user.bsky.social), with segments of alphanumeric/hyphens separated by dots",
	}
	mockService.On("ResolveHandleToDID", mock.Anything, "bad").Return("", invalid)

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest("handle=bad"))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	body := decodeXRPCBody(t, w)
	assert.Equal(t, "InvalidRequest", body["error"])
	assert.Equal(t, invalid.Error(), body["message"])

	mockService.AssertExpectations(t)
}

// TestResolveHandleHandler_InternalError: an infrastructure failure is a 5xx,
// not a 4xx. Reporting a DNS or PLC outage as a bad handle tells the caller
// their input is wrong and hides the outage from anyone watching 5xx rates.
func TestResolveHandleHandler_InternalError(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	mockService.On("ResolveHandleToDID", mock.Anything, "alice.test").
		Return("", errors.New("dns exploded"))

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest("handle=alice.test"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "InternalServerError", decodeXRPCBody(t, w)["error"])

	mockService.AssertExpectations(t)
}

// TestResolveHandleHandler_RejectsDID: resolveHandle takes a handle, and a DID
// is not one. Answering here rather than forwarding matters for a reason
// beyond tidiness: the service's slow path hands anything it cannot find
// locally to the identity resolver, so a forwarded DID becomes a PLC directory
// lookup that this unauthenticated route lets any caller trigger at will. The
// caller also learns nothing from it — they already hold the DID.
func TestResolveHandleHandler_RejectsDID(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest("handle=did:plc:abc123"))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "InvalidRequest", decodeXRPCBody(t, w)["error"])

	mockService.AssertNotCalled(t, "ResolveHandleToDID", mock.Anything, mock.Anything)
}

// TestResolveHandleHandler_Timeout: resolution runs DNS and an HTTPS fetch
// against a host the caller named, so a blown deadline is the ordinary failure
// here, not an exotic one. It answers 504 rather than the generic 500 so an
// upstream that is merely slow is distinguishable in the logs from one that is
// broken.
func TestResolveHandleHandler_Timeout(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	mockService.On("ResolveHandleToDID", mock.Anything, "slow.test").
		Return("", fmt.Errorf("resolving slow.test: %w", context.DeadlineExceeded))

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest("handle=slow.test"))

	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.Equal(t, "Timeout", decodeXRPCBody(t, w)["error"])

	mockService.AssertExpectations(t)
}

// TestResolveHandleHandler_UpstreamFailure: a resolution that broke is an
// UPSTREAM failure — the PLC directory, a DNS server, or the handle's own web
// host — and not a fault in this AppView. It answers 502, the same call the
// shared rules already make for a PDS 5xx, so our own error rate stays a
// signal about us. Reported as a 500 it is indistinguishable from a bug here,
// and every unresolvable third-party host inflates the number that pages
// somebody.
//
// The message is fixed rather than the error's own text: the resolver's Reason
// carries upstream detail, and this route is unauthenticated.
func TestResolveHandleHandler_UpstreamFailure(t *testing.T) {
	mockService := new(MockUserService)
	handler := NewResolveHandleHandler(mockService)

	resolutionFailed := &identity.ErrResolutionFailed{
		Identifier: "alice.test",
		Reason:     "handle resolution failed: HTTP well-known status 503 for alice.test",
	}
	mockService.On("ResolveHandleToDID", mock.Anything, "alice.test").
		Return("", fmt.Errorf("failed to resolve handle alice.test: %w", resolutionFailed))

	w := httptest.NewRecorder()
	handler.HandleResolveHandle(w, resolveHandleRequest("handle=alice.test"))

	assert.Equal(t, http.StatusBadGateway, w.Code)
	body := decodeXRPCBody(t, w)
	assert.Equal(t, "UpstreamFailure", body["error"])
	assert.NotContains(t, body["message"], "503",
		"upstream detail must not reach an unauthenticated caller")

	mockService.AssertExpectations(t)
}
