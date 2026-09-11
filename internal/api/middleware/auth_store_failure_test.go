package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/core/aggregators"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
)

func TestAuthenticationStoreFailureIsNotSessionExpiration(t *testing.T) {
	for _, mode := range []string{"required", "optional", "dual"} {
		for _, missing := range []bool{false, true} {
			name := "database_failure"
			if missing {
				name = "missing_session"
			}
			t.Run(mode+"/"+name, func(t *testing.T) {
				client := newMockOAuthClient()
				failure := errors.New("database connection unavailable")
				if missing {
					failure = covesoauth.ErrSessionNotFound
				}
				store := &authenticationFailureStore{failure: failure}
				middleware := NewOAuthAuthMiddleware(client, store)
				called := false
				next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					assert.Empty(t, GetUserDID(r))
					w.WriteHeader(http.StatusNoContent)
				})
				var handler http.Handler
				switch mode {
				case "required":
					handler = middleware.RequireAuth(next)
				case "optional":
					handler = middleware.OptionalAuth(next)
				case "dual":
					handler = NewDualAuthMiddleware(client, store, nil, nil).RequireAuth(next)
				}
				var logged bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&logged)
				t.Cleanup(func() { log.SetOutput(previous) })
				request := httptest.NewRequest(http.MethodGet, "/api/me", nil)
				request.Header.Set("Authorization", "Bearer "+client.createTestToken("did:plc:storefailure", "browser", time.Hour))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if !missing {
					assert.GreaterOrEqual(t, response.Code, 500, "storage outage must be a retryable server failure")
					assert.Less(t, response.Code, 600)
					assert.False(t, called, "storage outage must not silently run an authenticated request as guest")
					assert.NotContains(t, response.Body.String(), failure.Error(), "internal infrastructure detail must not leak")
					for _, fragment := range []string{"[AUTH_ERROR] type=session_store_failure", "method=GET", "path=/api/me", "did=did:plc:storefailure", "session_id=browser", failure.Error()} {
						assert.Contains(t, logged.String(), fragment, "storage outage must be logged server-side")
					}
				} else if mode == "optional" {
					assert.Equal(t, http.StatusNoContent, response.Code)
					assert.True(t, called)
				} else {
					assert.Equal(t, http.StatusUnauthorized, response.Code)
					assert.False(t, called)
				}
			})
		}
	}
}

// Only a dead session tells an aggregator to re-authenticate. Coordination
// contention, an authorization-server outage and a persistence failure are
// transient: re-authenticating cannot help and would discard a working session.
func TestAPIKeyRefreshFailureIsNotAlwaysReauthentication(t *testing.T) {
	const apiKey = "ckapi_test1234567890123456789012345678"
	for _, testcase := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "session_busy", err: fmt.Errorf("failed to refresh tokens: %w", covesoauth.ErrSessionBusy), wantStatus: http.StatusServiceUnavailable},
		{name: "authorization_server_unavailable", err: fmt.Errorf("failed to refresh tokens: %w", errors.New("OAuth refresh failed (HTTP 503)")), wantStatus: http.StatusServiceUnavailable},
		{name: "persistence_failure", err: fmt.Errorf("failed to refresh tokens: %w", errors.New("persist OAuth session: database connection unavailable")), wantStatus: http.StatusServiceUnavailable},
		{name: "dead_session", err: fmt.Errorf("%w: %w", aggregators.ErrOAuthSessionDead, covesoauth.ErrRefreshRejected), wantStatus: http.StatusUnauthorized},
		{name: "missing_session", err: fmt.Errorf("failed to resume session: %w", covesoauth.ErrSessionNotFound), wantStatus: http.StatusUnauthorized},
		{name: "revoked_key", err: aggregators.ErrAPIKeyRevoked, wantStatus: http.StatusUnauthorized},
		{name: "unknown_aggregator", err: aggregators.ErrAggregatorNotFound, wantStatus: http.StatusUnauthorized},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			client := newMockOAuthClient()
			validator := &mockAPIKeyValidator{aggregators: map[string]string{apiKey: "did:plc:aggregator123"}, refreshError: testcase.err}
			middleware := NewDualAuthMiddleware(client, newMockOAuthStore(), &mockServiceAuthValidator{}, &mockAggregatorChecker{aggregators: map[string]bool{}}).WithAPIKeyValidator(validator)
			called := false
			handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			var logged bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(previous) })
			request := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.write", nil)
			request.Header.Set("Authorization", "Bearer "+apiKey)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, testcase.wantStatus, response.Code)
			assert.False(t, called)
			assert.NotContains(t, response.Body.String(), testcase.err.Error(), "internal detail must not leak")
			assert.Contains(t, logged.String(), "did=did:plc:aggregator123")
			assert.Contains(t, logged.String(), testcase.err.Error())
			if testcase.wantStatus == http.StatusUnauthorized {
				assert.Contains(t, response.Body.String(), "re-authenticate")
				assert.Contains(t, logged.String(), "[AUTH_FAILURE]")
			} else {
				assert.NotContains(t, response.Body.String(), "re-authenticate", "transient failure must not tell the aggregator to throw away a working session")
				assert.Contains(t, logged.String(), "[AUTH_ERROR] type=token_refresh_unavailable")
			}
		})
	}
}

type authenticationFailureStore struct {
	indigooauth.ClientAuthStore
	failure error
}

func (s *authenticationFailureStore) GetSession(context.Context, syntax.DID, string) (*indigooauth.ClientSessionData, error) {
	return nil, s.failure
}
