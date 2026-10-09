package routes_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/moderation"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// viewerAuthoritySessions is an in-memory sealed-session unsealer and OAuth
// store. tests/fixtures holds the same fakes but is integration-tagged, so
// this T0 test keeps its own.
type viewerAuthoritySessions struct {
	oauthlib.ClientAuthStore
	sealed map[string]*oauth.SealedSession
	stored map[string]*oauthlib.ClientSessionData
}

func newViewerAuthoritySessions() *viewerAuthoritySessions {
	return &viewerAuthoritySessions{
		sealed: map[string]*oauth.SealedSession{},
		stored: map[string]*oauthlib.ClientSessionData{},
	}
}

// add registers a sealed session for did and returns its bearer token.
func (s *viewerAuthoritySessions) add(did string) string {
	token := "viewer-authority-session-" + did
	s.sealed[token] = &oauth.SealedSession{DID: did, SessionID: "browser", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	s.stored[did+":browser"] = &oauthlib.ClientSessionData{AccountDID: syntax.DID(did), SessionID: "browser"}
	return token
}

func (s *viewerAuthoritySessions) UnsealSession(token string) (*oauth.SealedSession, error) {
	if sealed, ok := s.sealed[token]; ok {
		return sealed, nil
	}
	return nil, fmt.Errorf("unknown sealed token")
}

func (s *viewerAuthoritySessions) GetSession(_ context.Context, did syntax.DID, sessionID string) (*oauthlib.ClientSessionData, error) {
	if session, ok := s.stored[did.String()+":"+sessionID]; ok {
		return session, nil
	}
	return nil, oauth.ErrSessionNotFound
}

// TestGetViewerAuthorityRoute drives social.coves.actor.getViewerAuthority
// through its route registration. Authentication refusals belong to
// middleware.TestRequireAuthenticated.
func TestGetViewerAuthorityRoute(t *testing.T) {
	const (
		adminDID  = "did:plc:viewerauthorityadmin"
		memberDID = "did:plc:viewerauthoritymember"
	)
	sessions := newViewerAuthoritySessions()
	authority := moderation.NewAllowlistAuthority([]string{adminDID})
	mux := chi.NewRouter()
	routes.RegisterViewerAuthorityRoute(mux,
		middleware.NewInstanceAdminMiddleware(sessions, sessions, nil, authority), authority)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	for _, test := range []struct {
		name     string
		viewer   string
		wantBody string
	}{
		{name: "instance admin", viewer: adminDID, wantBody: `{"instanceAdmin":true}`},
		{name: "non-admin", viewer: memberDID, wantBody: `{"instanceAdmin":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				server.URL+"/xrpc/social.coves.actor.getViewerAuthority", nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+sessions.add(test.viewer))
			response, err := server.Client().Do(request)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)

			assert.Equal(t, http.StatusOK, response.StatusCode, "body: %s", body)
			assert.JSONEq(t, test.wantBody, string(body))
		})
	}
}
