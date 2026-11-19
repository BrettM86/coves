//go:build integration

package routes_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	webCSRFInputPattern = regexp.MustCompile(`<input[^>]*name="csrf_token"[^>]*>`)
	webCSRFValuePattern = regexp.MustCompile(`value="([^"]*)"`)
)

// renderedCSRFToken returns the csrf_token value in a rendered
// delete-account page, or "" when the page carries none.
func renderedCSRFToken(body string) string {
	input := webCSRFInputPattern.FindString(body)
	if input == "" {
		return ""
	}
	match := webCSRFValuePattern.FindStringSubmatch(input)
	if match == nil {
		return ""
	}
	return match[1]
}

// TestWebDeleteAccountHonoursServerSideSession drives /delete-account through
// routes.RegisterWebRoutes with the Postgres OAuth store, the real users
// service and real session sealing. Only a cookie whose oauth_sessions row is
// still live and belongs to the sealed DID may delete an account.
func TestWebDeleteAccountHonoursServerSideSession(t *testing.T) {
	const (
		publicURL = "https://coves.social"
		aliceDID  = "did:plc:alicedeleteweb0000000000"
		bobDID    = "did:plc:bobdeleteweb000000000000"
		sessionID = "web-session"
	)

	type fixture struct {
		t           *testing.T
		db          *sql.DB
		client      *oauth.OAuthClient
		store       indigooauth.ClientAuthStore
		userService users.UserService
		router      *chi.Mux
	}

	newFixture := func(t *testing.T) *fixture {
		db := testkit.DB(t)
		ctx := context.Background()
		store := oauth.NewMobileAwareStoreWrapper(oauth.NewPostgresOAuthStore(db, 0))
		app := indigooauth.NewClientApp(&indigooauth.ClientConfig{ClientID: publicURL + "/oauth/client-metadata.json"}, store)
		client := &oauth.OAuthClient{
			ClientApp:  app,
			Config:     &oauth.OAuthConfig{SealedTokenTTL: time.Hour},
			SealSecret: bytes.Repeat([]byte{5}, 32),
		}
		repo := postgres.NewUserRepository(db)
		userService := users.NewUserService(repo, noNetworkResolver{t: t}, testkit.Endpoints().PDS.BaseURL, nil, "")
		for did, handle := range map[string]string{aliceDID: "alice-delete.test", bobDID: "bob-delete.test"} {
			_, err := userService.CreateUser(ctx, users.CreateUserRequest{DID: did, Handle: handle, PDSURL: testkit.Endpoints().PDS.BaseURL})
			require.NoError(t, err)
		}
		router := chi.NewRouter()
		routes.RegisterWebRoutes(router, client, userService, middleware.NewOAuthAuthMiddleware(client, store), publicURL, "")
		return &fixture{t: t, db: db, client: client, store: store, userService: userService, router: router}
	}

	saveSession := func(f *fixture, accountDID, sid string) {
		key, err := atcrypto.GeneratePrivateKeyP256()
		require.NoError(f.t, err)
		require.NoError(f.t, f.store.SaveSession(context.Background(), indigooauth.ClientSessionData{
			AccountDID:              syntax.DID(accountDID),
			SessionID:               sid,
			HostURL:                 "https://pds.example.test",
			AuthServerURL:           "https://auth.example.test",
			AuthServerTokenEndpoint: "https://auth.example.test/token",
			AccessToken:             "access-" + sid,
			RefreshToken:            "refresh-" + sid,
			DPoPPrivateKeyMultibase: key.Multibase(),
		}))
	}

	seal := func(f *fixture, did, sid string) string {
		sealed, err := f.client.SealSession(did, sid, time.Hour)
		require.NoError(f.t, err)
		return sealed
	}

	getPage := func(f *fixture, cookie string) string {
		request := httptest.NewRequest(http.MethodGet, "/delete-account", nil)
		request.AddCookie(&http.Cookie{Name: "coves_session", Value: cookie})
		response := httptest.NewRecorder()
		f.router.ServeHTTP(response, request)
		require.Equal(f.t, http.StatusOK, response.Code, "GET /delete-account")
		return response.Body.String()
	}

	postDelete := func(f *fixture, cookie, token string) *httptest.ResponseRecorder {
		form := url.Values{"confirm": {"true"}, "csrf_token": {token}}
		request := httptest.NewRequest(http.MethodPost, "/delete-account", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", publicURL)
		request.AddCookie(&http.Cookie{Name: "coves_session", Value: cookie})
		response := httptest.NewRecorder()
		f.router.ServeHTTP(response, request)
		return response
	}

	// logout runs the real web logout with the cookie and confirms the
	// session row is gone.
	logout := func(f *fixture, cookie, did, sid string) {
		request := httptest.NewRequest(http.MethodPost, "/oauth/logout", nil)
		request.AddCookie(&http.Cookie{Name: "coves_session", Value: cookie})
		response := httptest.NewRecorder()
		oauth.NewOAuthHandler(f.client, f.store).HandleLogout(response, request)
		require.Equal(f.t, http.StatusOK, response.Code, "logout")
		var rows int
		require.NoError(f.t, f.db.QueryRow(
			`SELECT count(*) FROM oauth_sessions WHERE did = $1 AND session_id = $2`, did, sid).Scan(&rows))
		require.Equal(f.t, 0, rows, "logout must delete the oauth_sessions row")
	}

	// assertRedirectedWithClearedCookie checks the browser is sent back to the
	// delete-account page with its dead coves_session cookie cleared.
	assertRedirectedWithClearedCookie := func(t *testing.T, response *httptest.ResponseRecorder) {
		assert.Equal(t, http.StatusFound, response.Code)
		assert.Equal(t, "/delete-account", response.Header().Get("Location"))
		cleared := false
		for _, c := range response.Result().Cookies() {
			if c.Name == "coves_session" && c.Value == "" && c.MaxAge < 0 {
				cleared = true
			}
		}
		assert.True(t, cleared, "the dead coves_session cookie must be cleared; Set-Cookie: %v", response.Header().Values("Set-Cookie"))
	}

	accountExists := func(f *fixture, did string) bool {
		_, err := f.userService.GetUserByDID(context.Background(), did)
		return err == nil
	}

	tests := []struct {
		name string
		run  func(t *testing.T, f *fixture)
	}{
		{
			name: "live session deletes the account",
			run: func(t *testing.T, f *fixture) {
				saveSession(f, aliceDID, sessionID)
				cookie := seal(f, aliceDID, sessionID)
				body := getPage(f, cookie)
				assert.Contains(t, body, `id="delete-form"`, "a live session must see the delete form")
				token := renderedCSRFToken(body)
				assert.NotEmpty(t, token, "the delete form must carry a csrf_token")

				response := postDelete(f, cookie, token)
				assert.Equal(t, http.StatusFound, response.Code)
				assert.Equal(t, "/delete-account/success", response.Header().Get("Location"))
				assert.False(t, accountExists(f, aliceDID), "the account must be deleted")
			},
		},
		{
			name: "cookie and token replayed after logout are refused",
			run: func(t *testing.T, f *fixture) {
				saveSession(f, aliceDID, sessionID)
				cookie := seal(f, aliceDID, sessionID)
				token := renderedCSRFToken(getPage(f, cookie))
				assert.NotEmpty(t, token, "the delete form must carry a csrf_token before logout")
				logout(f, cookie, aliceDID, sessionID)

				response := postDelete(f, cookie, token)
				assertRedirectedWithClearedCookie(t, response)
				assert.True(t, accountExists(f, aliceDID), "a logged-out cookie must not delete the account")
			},
		},
		{
			name: "session row owned by another account is refused",
			run: func(t *testing.T, f *fixture) {
				saveSession(f, bobDID, sessionID)
				cookie := seal(f, aliceDID, sessionID)
				token := renderedCSRFToken(getPage(f, cookie))

				response := postDelete(f, cookie, token)
				assertRedirectedWithClearedCookie(t, response)
				assert.True(t, accountExists(f, aliceDID), "the sealed DID's account must survive")
				assert.True(t, accountExists(f, bobDID), "the session owner's account must survive")
			},
		},
		{
			name: "logged-out cookie renders no delete form",
			run: func(t *testing.T, f *fixture) {
				saveSession(f, aliceDID, sessionID)
				cookie := seal(f, aliceDID, sessionID)
				logout(f, cookie, aliceDID, sessionID)

				body := getPage(f, cookie)
				assert.False(t, strings.Contains(body, `id="delete-form"`), "a logged-out cookie must not see the delete form")
				assert.False(t, strings.Contains(body, `name="csrf_token"`), "a logged-out cookie must not get a csrf_token")
				assert.False(t, strings.Contains(body, "alice-delete.test"), "a logged-out cookie must not see the account handle")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newFixture(t))
		})
	}
}
