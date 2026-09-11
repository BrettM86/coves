package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Errors deliberately carry secrets: their typed classification is safe, their
// Error() string is not. Assertion failures never print the captured log body.
const diagnosticPrivateMarker = "SYNTHETIC_PRIVATE_OAUTH_VALUE"

func captureOAuthDiagnostics(t *testing.T, protectUpstream bool) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	var handler slog.Handler = slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})
	if protectUpstream {
		handler = NewOAuthLogHandler(handler)
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func assertNoPrivateDiagnostics(t *testing.T, output *bytes.Buffer, extra ...string) {
	t.Helper()
	for _, value := range append(extra, diagnosticPrivateMarker) {
		if value != "" {
			assert.False(t, strings.Contains(output.String(), value), "OAuth log exposed confidential fixture data")
		}
	}
}

func assertOAuthDiagnostic(t *testing.T, output *bytes.Buffer, level string, terms ...string) {
	t.Helper()
	found := false
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), "diagnostic must remain structured JSON")
		if record["level"] != level {
			continue
		}
		// Require a meaningful operation and failure category, but leave spelling
		// and attribute grouping to the implementation.
		text := strings.ToLower(line)
		matches := strings.Contains(text, "operation") && (strings.Contains(text, "category") || strings.Contains(text, "reason"))
		for _, term := range terms {
			matches = matches && strings.Contains(text, strings.ToLower(term))
		}
		found = found || matches
	}
	assert.True(t, found, "missing %s OAuth diagnostic with operation, failure category, and expected safe classification %v", level, terms)
}

type diagnosticStore struct {
	*webFlowRecordingStore
	authSaveError error
	claimError    error
	claimCalls    int
}

func (s *diagnosticStore) SaveAuthRequestInfo(ctx context.Context, data indigooauth.AuthRequestData) error {
	if s.authSaveError != nil {
		return s.authSaveError
	}
	return s.webFlowRecordingStore.SaveAuthRequestInfo(ctx, data)
}
func (s *diagnosticStore) ClaimWebOAuthData(ctx context.Context, state, nonce string) (*WebOAuthData, error) {
	s.claimCalls++
	if s.claimError != nil {
		return nil, s.claimError
	}
	return s.webFlowRecordingStore.ClaimWebOAuthData(ctx, state, nonce)
}
func attachDiagnosticStore(handler *OAuthHandler, recording *webFlowRecordingStore) *diagnosticStore {
	store := &diagnosticStore{webFlowRecordingStore: recording}
	wrapper := NewMobileAwareStoreWrapper(store)
	handler.store = wrapper
	handler.mobileStore = wrapper
	handler.client.ClientApp.Store = wrapper
	return store
}
func privateDatabaseError() error {
	return fmt.Errorf("%s: %w", diagnosticPrivateMarker, &pq.Error{Code: "42703", Message: diagnosticPrivateMarker, Detail: diagnosticPrivateMarker})
}

func TestWebOAuth_PersistenceReceiptRetainsActualFailure(t *testing.T) {
	for _, operation := range []string{"auth request", "web binding"} {
		t.Run(operation, func(t *testing.T) {
			store := &diagnosticStore{webFlowRecordingStore: &webFlowRecordingStore{fakeMobileAuthStore: newFakeMobileAuthStore(), bindings: make(map[string]WebOAuthData)}}
			failure := privateDatabaseError()
			if operation == "auth request" {
				store.authSaveError = failure
			} else {
				store.bindingError = failure
			}
			receipt := &webPersistenceReceipt{err: errors.New("not yet persisted")}
			ctx := context.WithValue(t.Context(), webFlowContextKey{}, WebOAuthData{BrowserNonce: "fixture-nonce", ReturnURL: "/saved", ExpiresAt: time.Now().Add(time.Minute)})
			ctx = context.WithValue(ctx, webPersistenceContextKey{}, receipt)
			err := NewMobileAwareStoreWrapper(store).SaveAuthRequestInfo(ctx, indigooauth.AuthRequestData{State: "fixture-state"})
			assert.True(t, errors.Is(err, failure), "store must preserve actual failure")
			assert.True(t, errors.Is(receipt.err, failure), "receipt must retain actual persistence failure even when Indigo ignores return value")
		})
	}
}

func TestWebOAuth_LoginPersistenceFailureHasSafeActionableDiagnostic(t *testing.T) {
	for _, operation := range []string{"auth request", "web binding"} {
		t.Run(operation, func(t *testing.T) {
			output := captureOAuthDiagnostics(t, true)
			handler, recording := newWebFlowHandler(t)
			store := attachDiagnosticStore(handler, recording)
			failure := privateDatabaseError()
			if operation == "auth request" {
				store.authSaveError = failure
			} else {
				store.bindingError = failure
			}
			response := httptest.NewRecorder()
			handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?"+url.Values{"handle": {"https://example.com"}, "redirect": {"/saved"}}.Encode(), nil))
			assertWebLoginError(t, response, "server_error", "/saved")
			assertOAuthDiagnostic(t, output, "ERROR", "42703")
			assertNoPrivateDiagnostics(t, output)
			for _, cookie := range response.Result().Cookies() {
				assert.Less(t, cookie.MaxAge, 0, "failed persistence cannot publish an active browser binding")
			}
		})
	}
}

func TestWebOAuth_CallbackFailureDiagnosticsDistinguishRejectionFromOutage(t *testing.T) {
	for _, scenario := range []struct{ name, code, level string }{
		{"missing cookie", "invalid_request", "WARN"}, {"empty cookie", "invalid_request", "WARN"}, {"missing state", "invalid_request", "WARN"},
		{"mismatch", "invalid_request", "WARN"}, {"expired", "invalid_request", "WARN"}, {"consumed", "invalid_request", "WARN"},
		{"unsupported store", "server_error", "ERROR"}, {"claim database failure", "server_error", "ERROR"}, {"mobile lookup failure", "server_error", "ERROR"},
		{"claim timeout", "server_error", "ERROR"},
		{"malformed database code", "server_error", "ERROR"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			handler, recording := newWebFlowHandler(t)
			store := attachDiagnosticStore(handler, recording)
			started := startWebLogin(t, handler, "/saved")
			state := recording.states[0]
			binding := recording.bindings[state]
			cookie := webBindingCookie(t, started, binding)
			query := url.Values{"state": {state}, "code": {diagnosticPrivateMarker}, "iss": {"https://example.com"}}
			switch scenario.name {
			case "empty cookie":
				cookie.Value = ""
			case "missing state":
				query.Del("state")
			case "mismatch":
				cookie.Value = diagnosticPrivateMarker
			case "expired":
				binding.ExpiresAt = time.Now().Add(-time.Minute)
				recording.bindings[state] = binding
			case "consumed":
				delete(recording.bindings, state)
			case "unsupported store":
				handler.store = recording.ClientAuthStore
			case "claim database failure":
				store.claimError = privateDatabaseError()
			case "mobile lookup failure":
				store.lookupErr = privateDatabaseError()
			case "malformed database code":
				store.claimError = &pq.Error{Code: pq.ErrorCode(diagnosticPrivateMarker), Message: diagnosticPrivateMarker}
			case "claim timeout":
				store.claimError = fmt.Errorf("%s: %w", diagnosticPrivateMarker, context.DeadlineExceeded)
			}
			request := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+query.Encode(), nil)
			if scenario.name != "missing cookie" {
				request.AddCookie(cookie)
			}
			request.AddCookie(&http.Cookie{Name: "oauth_csrf", Value: diagnosticPrivateMarker})
			output := captureOAuthDiagnostics(t, true)
			response := httptest.NewRecorder()
			handler.HandleCallback(response, request)
			assertWebLoginError(t, response, scenario.code, "/")
			assert.Empty(t, response.Result().Cookies(), "rejected callback must preserve other pending browser/mobile cookies")
			assert.Empty(t, recording.readClaims, "reject before Indigo consumes OAuth request")
			terms := []string{}
			switch scenario.name {
			case "missing cookie", "empty cookie":
				terms = append(terms, "cookie")
			case "missing state":
				terms = append(terms, "state")
			case "mismatch", "expired", "consumed":
				terms = append(terms, "binding")
			case "unsupported store":
				terms = append(terms, "store")
			}
			if scenario.name == "claim database failure" || strings.Contains(scenario.name, "lookup failure") {
				terms = append(terms, "42703")
			}
			if scenario.name == "claim timeout" {
				terms = append(terms, "timeout")
			}
			assertOAuthDiagnostic(t, output, scenario.level, terms...)
			assertNoPrivateDiagnostics(t, output, state, binding.BrowserNonce)
		})
	}
}

func TestWebOAuth_CallbackProcessingFailureHasSafeDiagnostic(t *testing.T) {
	handler, store := newWebFlowHandler(t)
	started := startWebLogin(t, handler, "/saved")
	state := store.states[0]
	output := captureOAuthDiagnostics(t, true)
	response := completeLogin(t, handler, state, started.Result().Cookies())
	assertWebLoginError(t, response, "server_error", "/saved")
	assert.Equal(t, 1, store.claims, "test must reach real callback processing")
	assertOAuthDiagnostic(t, output, "ERROR", "callback")
	assertNoPrivateDiagnostics(t, output, state)
}

func TestWebOAuth_LoginResolverTimeoutBeforePersistenceKeepsActualFailureDiagnostic(t *testing.T) {
	output := captureOAuthDiagnostics(t, true)
	store := &webFlowRecordingStore{fakeMobileAuthStore: newFakeMobileAuthStore(), bindings: make(map[string]WebOAuthData)}
	lookups := 0
	client, err := NewOAuthClient(guardTestConfig("https://plc.example.invalid", true), NewMobileAwareStoreWrapper(store), withTransportOptions(WithHostResolver(func(context.Context, string) ([]net.IP, error) {
		lookups++
		return nil, fmt.Errorf("%s: %w", diagnosticPrivateMarker, context.DeadlineExceeded)
	})))
	require.NoError(t, err)
	handler := NewOAuthHandler(client, store)
	const identifier = "https://account-private.example.invalid"
	const destination = "/saved?sort=new#reply"
	response := httptest.NewRecorder()
	handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?"+url.Values{"handle": {identifier}, "redirect": {destination}}.Encode(), nil))
	require.Positive(t, lookups, "test must reach real guarded metadata resolution")
	assert.Empty(t, store.states, "provider resolution failed before saving an auth request")
	assert.Empty(t, store.bindings)
	assertWebLoginError(t, response, "server_error", destination)
	assert.Empty(t, response.Result().Cookies(), "failed login initialization cannot publish a browser binding or session")
	assertOAuthDiagnostic(t, output, "ERROR", "login_start", "timeout")
	assertNoPrivateDiagnostics(t, output, identifier, "account-private.example.invalid")
	assert.False(t, strings.Contains(response.Body.String(), diagnosticPrivateMarker))
}

func TestWebOAuth_LongReturnDestinationWarnsWithoutDisclosingPath(t *testing.T) {
	output := captureOAuthDiagnostics(t, true)
	const ordinary = "/saved?sort=new#reply"
	assert.Equal(t, ordinary, safeWebReturnURL(ordinary))
	assert.Empty(t, output.String(), "ordinary safe return destinations need no warning")
	oversized := "/" + diagnosticPrivateMarker + strings.Repeat("x", 2049)
	assert.Equal(t, "/", safeWebReturnURL(oversized))
	assertOAuthDiagnostic(t, output, "WARN", "return_url", "too_long")
	var record map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record))
	assert.Equal(t, float64(len(oversized)), record["length"], "operator needs only the rejected URL length")
	assertNoPrivateDiagnostics(t, output)
}
