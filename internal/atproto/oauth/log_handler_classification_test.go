package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeSingleDiagnostic(t *testing.T, output *bytes.Buffer) map[string]any {
	t.Helper()
	var record map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record), "expected exactly one structured diagnostic, got %q", output.String())
	return record
}

// Coves messages are compile-time literals (pinned below), so the operator
// keeps them. Indigo messages are replaced because they may embed provider data.
func TestOAuthLogHandler_MessageRetainedForCovesReplacedForIndigo(t *testing.T) {
	t.Run("coves", func(t *testing.T) {
		output := captureOAuthDiagnostics(t, true)
		slog.Error("failed to process OAuth callback", "operation", "callback", "error", errors.New(diagnosticPrivateMarker))
		record := decodeSingleDiagnostic(t, output)
		assert.Equal(t, "failed to process OAuth callback", record["msg"])
		assertNoPrivateDiagnostics(t, output)
	})
	t.Run("indigo", func(t *testing.T) {
		var output bytes.Buffer
		protected := NewOAuthLogHandler(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
		record := slog.NewRecord(time.Now(), slog.LevelWarn, "auth server request failed: "+diagnosticPrivateMarker, reflect.ValueOf(indigooauth.S256CodeChallenge).Pointer())
		require.NoError(t, protected.Handle(context.Background(), record))
		decoded := decodeSingleDiagnostic(t, &output)
		assert.Equal(t, "OAuth diagnostic", decoded["msg"])
		assertNoPrivateDiagnostics(t, &output)
	})
}

// Retaining Coves messages is only safe while every message is a compile-time
// literal. Fail if any slog call in this package builds its message at runtime.
func TestOAuthPackage_LogMessagesAreCompileTimeLiterals(t *testing.T) {
	fileSet := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	messageArgument := map[string]int{"Debug": 0, "Info": 0, "Warn": 0, "Error": 0, "Log": 2, "LogAttrs": 2,
		"DebugContext": 1, "InfoContext": 1, "WarnContext": 1, "ErrorContext": 1}
	var violations []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		require.NoError(t, err)
		if !bytes.Contains(source, []byte("slog")) {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, source, 0)
		require.NoError(t, err)
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isLoggerReceiver(selector.X) {
				return true
			}
			index, logging := messageArgument[selector.Sel.Name]
			if !logging || len(call.Args) <= index {
				return true
			}
			if !isConstantStringExpression(call.Args[index]) {
				violations = append(violations, fileSet.Position(call.Pos()).String())
			}
			return true
		})
	}
	assert.Empty(t, violations, "slog messages in this package must be string literals: the log handler forwards Coves messages verbatim")
}

// The slog package itself, anything derived from it (slog.Default().With(...)),
// or a variable whose name says it is a logger.
func isLoggerReceiver(expression ast.Expr) bool {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name == "slog" || strings.Contains(strings.ToLower(typed.Name), "log")
	case *ast.CallExpr:
		if selector, ok := typed.Fun.(*ast.SelectorExpr); ok {
			return isLoggerReceiver(selector.X)
		}
	case *ast.SelectorExpr:
		return isLoggerReceiver(typed.X)
	}
	return false
}

func isConstantStringExpression(expression ast.Expr) bool {
	switch typed := expression.(type) {
	case *ast.BasicLit:
		return typed.Kind == token.STRING
	case *ast.BinaryExpr:
		return typed.Op == token.ADD && isConstantStringExpression(typed.X) && isConstantStringExpression(typed.Y)
	case *ast.ParenExpr:
		return isConstantStringExpression(typed.X)
	}
	return false
}

func TestOAuthLogHandler_ProviderErrorCodeSurvivesScrubbingOnlyWhenClamped(t *testing.T) {
	for _, code := range []string{"access_denied", "invalid_request", "server_error", "temporarily_unavailable"} {
		attrs := safeOAuthLogAttrs(slog.String("code", code))
		require.Len(t, attrs, 1, code)
		assert.Equal(t, code, attrs[0].Value.String())
	}
	assert.Empty(t, safeOAuthLogAttrs(slog.String("code", diagnosticPrivateMarker)), "unknown codes are provider-controlled text")
	assert.Empty(t, safeOAuthLogAttrs(slog.String("code", "login_required")), "only the clamped set is operator-facing")
	assert.Empty(t, safeOAuthLogAttrs(slog.Int("code", 400)), "code is a string classification, not a status")
}

func TestWebOAuth_ProviderDenialLogsClampedCode(t *testing.T) {
	handler, store := newWebFlowHandler(t)
	started := startWebLogin(t, handler, "/saved")
	state := store.states[0]
	binding := store.bindings[state]
	cookie := webBindingCookie(t, started, binding)
	query := url.Values{"state": {state}, "error": {"access_denied"}, "error_description": {diagnosticPrivateMarker}}
	callback := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+query.Encode(), nil)
	callback.AddCookie(cookie)
	output := captureOAuthDiagnostics(t, true)
	response := httptest.NewRecorder()
	handler.HandleCallback(response, callback)
	assertWebLoginError(t, response, "access_denied", "/saved")
	assert.Contains(t, output.String(), `"code":"access_denied"`, "operator must see which provider error ended the flow")
	assertNoPrivateDiagnostics(t, output, state, binding.BrowserNonce)
}

type noPDSDirectory struct{ identity.Directory }

func (noPDSDirectory) Lookup(_ context.Context, atid syntax.AtIdentifier) (*identity.Identity, error) {
	return &identity.Identity{DID: syntax.DID(completionDID), Handle: syntax.Handle("alice.example.invalid")}, nil
}

func TestOAuthErrorAttributes_ClassifiesIdentityResolutionFailures(t *testing.T) {
	for _, err := range []error{
		identity.ErrHandleNotFound, identity.ErrHandleMismatch, identity.ErrHandleNotDeclared,
		identity.ErrHandleReservedTLD, identity.ErrInvalidHandle, identity.ErrDIDNotFound, ErrLoginIdentifierInvalid,
	} {
		wrapped := fmt.Errorf("failed to resolve username (%s): %w", diagnosticPrivateMarker, err)
		assert.Equal(t, []slog.Attr{slog.String("category", "identity")}, oauthErrorAttributes(wrapped), err.Error())
	}
	for _, err := range []error{
		fmt.Errorf("%w: PLC directory status 503", identity.ErrDIDResolutionFailed),
		fmt.Errorf("%w: HTTP well-known status 503 for alice.example", identity.ErrHandleResolutionFailed),
		fmt.Errorf("%w: DNS error: %w", identity.ErrHandleResolutionFailed, errors.New(diagnosticPrivateMarker)),
		fmt.Errorf("fetching auth server metadata: %w", errors.New(diagnosticPrivateMarker)),
		errors.New("web OAuth binding was not persisted"),
	} {
		assert.Equal(t, []slog.Attr{slog.String("category", "internal")}, oauthErrorAttributes(err), err.Error())
	}
	assert.Equal(t, []slog.Attr{slog.String("category", "binding")}, oauthErrorAttributes(fmt.Errorf("save: %w", ErrWebBindingNotSaved)))
}

func TestWebOAuth_IdentityResolutionFailuresEndWithInvalidRequest(t *testing.T) {
	for _, scenario := range []struct{ name, identifier string }{
		{"malformed identifier", "invalid-identifier"},
		{"unknown DID", "did:plc:zzzzzzzzzzzzzzzzzzzzzzzz"},
		{"identity without PDS", "alice.example.invalid"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server, _ := newOAuthFlowServer(t)
			store := &webFlowRecordingStore{fakeMobileAuthStore: newFakeMobileAuthStore(), bindings: make(map[string]WebOAuthData)}
			// The fixture certificate is issued for example.com, so the PLC lives there too.
			client, err := NewOAuthClient(guardTestConfig("https://example.com", true), NewMobileAwareStoreWrapper(store), resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: routes only to in-process TLS fixture
			require.NoError(t, err)
			routeGuardedClientToTLSServer(t, client.ClientApp.Resolver.Client, server)
			routeGuardedClientToTLSServer(t, client.ClientApp.Client, server)
			// Real Indigo directory: the fixture answers the PLC lookup with 404.
			cache, ok := client.ClientApp.Dir.(*identity.CacheDirectory)
			require.True(t, ok)
			base, ok := cache.Inner.(*identity.BaseDirectory)
			require.True(t, ok)
			routeGuardedClientToTLSServer(t, &base.HTTPClient, server)
			handler := NewOAuthHandler(client, store)
			if scenario.name == "identity without PDS" {
				handler.client.ClientApp.Dir = noPDSDirectory{}
			}
			output := captureOAuthDiagnostics(t, true)
			response := httptest.NewRecorder()
			handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?"+url.Values{"handle": {scenario.identifier}, "redirect": {"/saved"}}.Encode(), nil))
			assertWebLoginError(t, response, "invalid_request", "/saved")
			assert.Empty(t, store.states, "identity failures happen before any auth request is saved")
			assert.Empty(t, response.Result().Cookies())
			assertOAuthDiagnostic(t, output, "ERROR", "login_start", "identity")
			// Indigo's identity package emits its own DEBUG lookups outside the
			// protected prefixes; the Coves failure diagnostic must not name the account.
			for _, line := range strings.Split(output.String(), "\n") {
				if strings.Contains(line, `"level":"ERROR"`) {
					assert.NotContains(t, line, scenario.identifier)
				}
			}
			assertNoPrivateDiagnostics(t, output)
		})
	}
}

func TestWebOAuth_UnboundWebRequestLogsBindingCategory(t *testing.T) {
	output := captureOAuthDiagnostics(t, true)
	handler, store := newWebFlowHandler(t)
	store.bindingError = ErrWebBindingNotSaved
	response := httptest.NewRecorder()
	handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "/oauth/login?"+url.Values{"handle": {"https://example.com"}, "redirect": {"/saved"}}.Encode(), nil))
	assertWebLoginError(t, response, "server_error", "/saved")
	assertOAuthDiagnostic(t, output, "ERROR", "login_start", "binding")
	for _, cookie := range response.Result().Cookies() {
		assert.Less(t, cookie.MaxAge, 0)
	}
}
