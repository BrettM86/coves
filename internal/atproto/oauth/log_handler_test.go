package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthLogHandler_RealProviderFailureDoesNotExposeResponseOrFlow(t *testing.T) {
	output := captureOAuthDiagnostics(t, true)
	server, _ := newOAuthFlowServer(t)
	original := server.Config.Handler
	tokenRequests := 0
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			original.ServeHTTP(w, r)
			return
		}
		tokenRequests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": diagnosticPrivateMarker, "error_description": diagnosticPrivateMarker,
			"access_token": diagnosticPrivateMarker, "state": diagnosticPrivateMarker,
			"identity": map[string]string{"did": diagnosticPrivateMarker, "handle": diagnosticPrivateMarker},
		})
	})
	store := &webFlowRecordingStore{fakeMobileAuthStore: newFakeMobileAuthStore(), bindings: make(map[string]WebOAuthData)}
	client, err := NewOAuthClient(guardTestConfig("https://plc.example.invalid", true), NewMobileAwareStoreWrapper(store), resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: routes only to in-process TLS fixture
	require.NoError(t, err)
	routeGuardedClientToTLSServer(t, client.ClientApp.Resolver.Client, server)
	routeGuardedClientToTLSServer(t, client.ClientApp.Client, server)
	handler := NewOAuthHandler(client, store)
	started := startWebLogin(t, handler, "/saved")
	state := store.states[0]
	nonce := store.bindings[state].BrowserNonce
	response := completeLogin(t, handler, state, started.Result().Cookies())
	assertWebLoginError(t, response, "server_error", "/saved")
	require.Equal(t, 1, tokenRequests, "must exercise pinned Indigo's actual provider error logger")
	assertNoPrivateDiagnostics(t, output, state, nonce)
	// Keep useful provider HTTP diagnostics even though its arbitrary JSON body
	// and the raw error it generates cannot be forwarded to the operator.
	found := false
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.Contains(line, "400") && strings.Contains(strings.ToLower(line), "token") {
			found = true
		}
	}
	assert.True(t, found, "provider failure must retain HTTP status and token operation")
}

func TestOAuthLogHandler_IndigoSourceDropsDirectAndInheritedSensitiveAttributes(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelWarn} {
		t.Run(level.String(), func(t *testing.T) {
			var output bytes.Buffer
			downstream := slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})
			protected := NewOAuthLogHandler(downstream)
			protected = protected.WithAttrs([]slog.Attr{slog.String("did", diagnosticPrivateMarker), slog.String("state", diagnosticPrivateMarker)})
			protected = protected.WithGroup("oauth").WithAttrs([]slog.Attr{slog.String("callback_url", diagnosticPrivateMarker)})
			protected = protected.WithGroup("request")
			record := slog.NewRecord(time.Now(), level, "auth server request failed", reflect.ValueOf(indigooauth.S256CodeChallenge).Pointer())
			record.AddAttrs(slog.String("request", "token"), slog.Int("statusCode", 400), slog.Any("body", map[string]string{"error": diagnosticPrivateMarker}), slog.String("err", diagnosticPrivateMarker), slog.Group("nested", slog.String("access_token", diagnosticPrivateMarker)))
			require.NoError(t, protected.Handle(context.Background(), record))
			assertNoPrivateDiagnostics(t, &output)
			assert.False(t, strings.Contains(output.String(), "auth server request failed"), "Indigo messages are replaced, never forwarded")
			assert.True(t, strings.Contains(output.String(), "400"), "safe HTTP status must survive handler groups")
			assert.True(t, strings.Contains(output.String(), "token"), "safe operation must survive handler groups")
			assert.True(t, strings.Contains(output.String(), level.String()), "preserve record level")
		})
	}
}

func TestOAuthLogHandler_PreservesUnrelatedSourceAndHandlerSemantics(t *testing.T) {
	var actual, expected bytes.Buffer
	options := &slog.HandlerOptions{Level: slog.LevelInfo}
	handlers := []slog.Handler{NewOAuthLogHandler(slog.NewJSONHandler(&actual, options)), slog.NewJSONHandler(&expected, options)}
	record := slog.NewRecord(time.Unix(100, 0), slog.LevelWarn, "unrelated request diagnostic", reflect.ValueOf(http.ListenAndServe).Pointer())
	record.AddAttrs(slog.String("body", "unrelated application body"), slog.Int("status", 503))
	for _, handler := range handlers {
		assert.False(t, handler.Enabled(t.Context(), slog.LevelDebug), "retain configured minimum level")
		assert.True(t, handler.Enabled(t.Context(), slog.LevelWarn))
		handler = handler.WithAttrs([]slog.Attr{slog.String("service", "appview")}).WithGroup("request").WithAttrs([]slog.Attr{slog.String("state", "unrelated-state")}).WithGroup("")
		require.NoError(t, handler.Handle(t.Context(), record))
	}
	assert.JSONEq(t, expected.String(), actual.String(), "OAuth source filtering cannot strip or regroup unrelated application diagnostics")
}

func TestOAuthLogHandler_CovesSuccessfulCallbackDoesNotLogIdentityOrSession(t *testing.T) {
	output := captureOAuthDiagnostics(t, true)
	handler, store, _, _ := newCompletionHandler(t)
	started := startWebLogin(t, handler, "/saved")
	state := store.states[0]
	nonce := store.bindings[state].BrowserNonce
	response := completeLogin(t, handler, state, started.Result().Cookies())
	require.Equal(t, "/saved", response.Header().Get("Location"))
	assertNoPrivateDiagnostics(t, output, state, nonce, completionDID, "alice.example.invalid", "fixture-access", "fixture-refresh")
}

func TestOAuthLogHandler_CovesSourceDropsRawErrorsAndInheritedIdentity(t *testing.T) {
	output := captureOAuthDiagnostics(t, true)
	logger := slog.Default().With("did", diagnosticPrivateMarker).WithGroup("oauth").With("state", diagnosticPrivateMarker)
	logger.Error("failed to process OAuth callback", "operation", "callback", "category", "provider", "error", errors.New(diagnosticPrivateMarker))
	assertNoPrivateDiagnostics(t, output)
	assertOAuthDiagnostic(t, output, "ERROR", "callback", "provider")
	assert.Contains(t, output.String(), "failed to process OAuth callback", "Coves messages are literals and stay readable")
}
