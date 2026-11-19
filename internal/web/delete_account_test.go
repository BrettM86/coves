package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/users"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/go-chi/chi/v5"
)

// csrfInputPattern finds the hidden csrf_token input of the delete form and
// captures its value, whichever order the attributes are written in.
var (
	csrfInputPattern = regexp.MustCompile(`<input[^>]*name="csrf_token"[^>]*>`)
	csrfValuePattern = regexp.MustCompile(`value="([^"]*)"`)
)

// extractCSRFToken returns the csrf_token value rendered in a delete-account
// page, or "" when the page carries none.
func extractCSRFToken(body string) string {
	input := csrfInputPattern.FindString(body)
	if input == "" {
		return ""
	}
	match := csrfValuePattern.FindStringSubmatch(input)
	if match == nil {
		return ""
	}
	return match[1]
}

// recordingUserService records every DeleteAccount call. The embedded nil
// interface makes any other unmodelled call panic.
type recordingUserService struct {
	users.UserService

	mu      sync.Mutex
	deleted []string
}

func (s *recordingUserService) GetUserByDID(_ context.Context, did string) (*users.User, error) {
	return &users.User{DID: did, Handle: "alice.test"}, nil
}

func (s *recordingUserService) DeleteAccount(_ context.Context, did string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, did)
	return nil
}

func (s *recordingUserService) deletedDIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// liveSessionStore holds the server-side session rows the auth middleware
// checks. The embedded nil interface makes any other call panic.
type liveSessionStore struct {
	oauthlib.ClientAuthStore
	sessions map[string]bool // did + "\x00" + sessionID
}

func (s *liveSessionStore) GetSession(_ context.Context, did syntax.DID, sessionID string) (*oauthlib.ClientSessionData, error) {
	if !s.sessions[did.String()+"\x00"+sessionID] {
		return nil, oauth.ErrSessionNotFound
	}
	return &oauthlib.ClientSessionData{AccountDID: did, SessionID: sessionID, AccessToken: "access-" + sessionID}, nil
}

// failingSessionStore fails every session lookup with a store error.
type failingSessionStore struct {
	oauthlib.ClientAuthStore
}

func (failingSessionStore) GetSession(context.Context, syntax.DID, string) (*oauthlib.ClientSessionData, error) {
	return nil, errors.New("database connection unavailable")
}

// TestDeleteAccountSubmitWithoutLiveSession covers POST /delete-account when
// the browser has no live server-side session: it is sent back to the page
// with the dead cookie cleared, and a session store outage is a 503 rather
// than an unauthenticated pass-through. No case deletes an account.
func TestDeleteAccountSubmitWithoutLiveSession(t *testing.T) {
	const (
		accountDID = "did:plc:deleteaccountunit"
		sessionID  = "session-one"
		publicURL  = "https://coves.social"
	)

	tests := []struct {
		name            string
		store           oauthlib.ClientAuthStore
		sendCookie      bool
		wantStatus      int
		wantLocation    string
		wantClearCookie bool
	}{
		{name: "no cookie is sent back to the page", store: &liveSessionStore{}, wantStatus: http.StatusFound, wantLocation: "/delete-account", wantClearCookie: true},
		{name: "cookie whose session row is gone is cleared and sent back", store: &liveSessionStore{}, sendCookie: true, wantStatus: http.StatusFound, wantLocation: "/delete-account", wantClearCookie: true},
		{name: "session store outage is unavailable", store: failingSessionStore{}, sendCookie: true, wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templates, err := NewTemplates()
			if err != nil {
				t.Fatalf("NewTemplates: %v", err)
			}
			client := &oauth.OAuthClient{
				Config:     &oauth.OAuthConfig{SealedTokenTTL: time.Hour},
				SealSecret: bytes.Repeat([]byte{9}, 32),
			}
			userService := &recordingUserService{}
			handlers := NewHandlers(templates, client, userService, publicURL, "")
			router := chi.NewRouter()
			router.With(middleware.NewOAuthAuthMiddleware(client, tt.store).OptionalAuth).Post("/delete-account", handlers.DeleteAccountSubmitHandler)

			form := url.Values{"confirm": {"true"}, "csrf_token": {"stale-token"}}
			request := httptest.NewRequest(http.MethodPost, "/delete-account", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", publicURL)
			if tt.sendCookie {
				sealed, err := client.SealSession(accountDID, sessionID, time.Hour)
				if err != nil {
					t.Fatalf("SealSession: %v", err)
				}
				request.AddCookie(&http.Cookie{Name: "coves_session", Value: sealed})
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Errorf("POST /delete-account: got %d, want %d", response.Code, tt.wantStatus)
			}
			if response.Header().Get("Location") != tt.wantLocation {
				t.Errorf("Location: got %q, want %q", response.Header().Get("Location"), tt.wantLocation)
			}
			cleared := false
			for _, c := range response.Result().Cookies() {
				if c.Name == "coves_session" && c.Value == "" && c.MaxAge < 0 {
					cleared = true
				}
			}
			if cleared != tt.wantClearCookie {
				t.Errorf("coves_session cleared: got %v, want %v (Set-Cookie: %v)", cleared, tt.wantClearCookie, response.Header().Values("Set-Cookie"))
			}
			if deleted := userService.deletedDIDs(); len(deleted) != 0 {
				t.Errorf("DeleteAccount calls: got %v, want none", deleted)
			}
		})
	}
}

// TestDeleteAccountSubmitRequiresCSRFTokenAndSameOrigin covers B2 (session-bound
// CSRF token) and B3 (Origin / Sec-Fetch-Site) for POST /delete-account with a
// live server-side session.
func TestDeleteAccountSubmitRequiresCSRFTokenAndSameOrigin(t *testing.T) {
	const (
		accountDID     = "did:plc:deleteaccountunit"
		sessionID      = "session-one"
		otherSessionID = "session-two"
		publicURL      = "https://coves.social"
	)

	const (
		tokenValid        = "valid"
		tokenMissing      = "missing"
		tokenWrong        = "wrong"
		tokenOtherSession = "other-session"
	)

	tests := []struct {
		name          string
		token         string
		origin        string
		secFetchSite  string
		wantStatus    int
		wantLocation  string
		wantDeletedBy []string
	}{
		{name: "valid token deletes the session account", token: tokenValid, wantStatus: http.StatusFound, wantLocation: "/delete-account/success", wantDeletedBy: []string{accountDID}},
		{name: "missing token is forbidden", token: tokenMissing, wantStatus: http.StatusForbidden},
		{name: "wrong token is forbidden", token: tokenWrong, wantStatus: http.StatusForbidden},
		{name: "token minted for another session is forbidden", token: tokenOtherSession, wantStatus: http.StatusForbidden},
		{name: "cross-origin Origin is forbidden", token: tokenValid, origin: "https://evil.example", wantStatus: http.StatusForbidden},
		{name: "Origin with a different scheme is forbidden", token: tokenValid, origin: "http://coves.social", wantStatus: http.StatusForbidden},
		{name: "Sec-Fetch-Site cross-site is forbidden", token: tokenValid, secFetchSite: "cross-site", wantStatus: http.StatusForbidden},
		{name: "same-origin Origin proceeds", token: tokenValid, origin: "https://coves.social", wantStatus: http.StatusFound, wantLocation: "/delete-account/success", wantDeletedBy: []string{accountDID}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templates, err := NewTemplates()
			if err != nil {
				t.Fatalf("NewTemplates: %v", err)
			}
			client := &oauth.OAuthClient{
				Config:     &oauth.OAuthConfig{SealedTokenTTL: time.Hour},
				SealSecret: bytes.Repeat([]byte{9}, 32),
			}
			store := &liveSessionStore{sessions: map[string]bool{
				accountDID + "\x00" + sessionID:      true,
				accountDID + "\x00" + otherSessionID: true,
			}}
			userService := &recordingUserService{}
			handlers := NewHandlers(templates, client, userService, publicURL, "")
			auth := middleware.NewOAuthAuthMiddleware(client, store)
			router := chi.NewRouter()
			router.With(auth.OptionalAuth).Get("/delete-account", handlers.DeleteAccountPageHandler)
			router.With(auth.OptionalAuth).Post("/delete-account", handlers.DeleteAccountSubmitHandler)

			seal := func(sid string) string {
				sealed, err := client.SealSession(accountDID, sid, time.Hour)
				if err != nil {
					t.Fatalf("SealSession: %v", err)
				}
				return sealed
			}
			cookie := seal(sessionID)
			renderedToken := func(sealed string) string {
				request := httptest.NewRequest(http.MethodGet, "/delete-account", nil)
				request.AddCookie(&http.Cookie{Name: "coves_session", Value: sealed})
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("GET /delete-account: got %d, want 200", response.Code)
				}
				token := extractCSRFToken(response.Body.String())
				if token == "" {
					t.Errorf("GET /delete-account with a live session must render a hidden csrf_token input")
				}
				return token
			}

			form := url.Values{"confirm": {"true"}}
			switch tt.token {
			case tokenValid:
				form.Set("csrf_token", renderedToken(cookie))
			case tokenWrong:
				form.Set("csrf_token", "not-the-session-token")
			case tokenOtherSession:
				form.Set("csrf_token", renderedToken(seal(otherSessionID)))
			}

			request := httptest.NewRequest(http.MethodPost, "/delete-account", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: "coves_session", Value: cookie})
			if tt.origin != "" {
				request.Header.Set("Origin", tt.origin)
			}
			if tt.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", tt.secFetchSite)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Errorf("POST /delete-account: got %d, want %d", response.Code, tt.wantStatus)
			}
			if tt.wantLocation != "" && response.Header().Get("Location") != tt.wantLocation {
				t.Errorf("Location: got %q, want %q", response.Header().Get("Location"), tt.wantLocation)
			}
			deleted := userService.deletedDIDs()
			if strings.Join(deleted, ",") != strings.Join(tt.wantDeletedBy, ",") {
				t.Errorf("DeleteAccount calls: got %v, want %v", deleted, tt.wantDeletedBy)
			}
		})
	}
}
