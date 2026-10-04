package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/config"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type wiringSessionUnsealer map[string]*oauth.SealedSession

func (unsealer wiringSessionUnsealer) UnsealSession(token string) (*oauth.SealedSession, error) {
	if session, ok := unsealer[token]; ok {
		return session, nil
	}
	return nil, fmt.Errorf("unknown session")
}

type wiringOAuthStore struct {
	indigooauth.ClientAuthStore
	sessions map[string]*indigooauth.ClientSessionData
}

func (store *wiringOAuthStore) GetSession(_ context.Context, did syntax.DID, sessionID string) (*indigooauth.ClientSessionData, error) {
	if session, ok := store.sessions[did.String()+":"+sessionID]; ok {
		return session, nil
	}
	return nil, oauth.ErrSessionNotFound
}

func TestBuildInstanceAdminMiddlewareUsesModerationAdminsOnly(t *testing.T) {
	const (
		creatorDID = "did:plc:creator"
		adminDID   = "did:plc:admin"
		path       = "/xrpc/social.coves.moderation.getSubjectState"
	)
	for _, test := range []struct {
		name       string
		admins     []string
		caller     string
		wantStatus int
		wantCode   string
		wantCalled bool
	}{
		{name: "community creator is not admin", admins: []string{adminDID}, caller: creatorDID, wantStatus: http.StatusForbidden, wantCode: "Forbidden"},
		{name: "listed admin is authorized", admins: []string{adminDID}, caller: adminDID, wantStatus: http.StatusNoContent, wantCalled: true},
		{name: "empty admin list grants nobody", caller: adminDID, wantStatus: http.StatusForbidden, wantCode: "Forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{
				Instance:   config.InstanceConfig{AllowedCommunityCreators: []string{creatorDID}},
				Moderation: config.ModerationConfig{Admins: test.admins},
			}
			unsealer := wiringSessionUnsealer{}
			store := &wiringOAuthStore{sessions: map[string]*indigooauth.ClientSessionData{}}
			for _, did := range []string{creatorDID, adminDID} {
				token := "session-for-" + did
				unsealer[token] = &oauth.SealedSession{DID: did, SessionID: "browser", ExpiresAt: time.Now().Add(time.Hour).Unix()}
				store.sessions[did+":browser"] = &indigooauth.ClientSessionData{AccountDID: syntax.DID(did), SessionID: "browser"}
			}

			gate := buildInstanceAdminMiddleware(cfg, unsealer, store, nil)
			if !assert.NotNil(t, gate, "the server must construct the instance-admin gate") {
				return
			}
			called := false
			var authenticatedDID string
			handler := gate.RequireInstanceAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				authenticatedDID = middleware.GetUserDID(r)
				w.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer session-for-"+test.caller)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, test.wantStatus, response.Code)
			assert.Equal(t, test.wantCalled, called)
			if test.wantCalled {
				assert.Equal(t, test.caller, authenticatedDID)
			} else if assert.NotEmpty(t, response.Body.Bytes(), "refusal must include a JSON error") {
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				assert.Equal(t, test.wantCode, body["error"])
			}
		})
	}
}
