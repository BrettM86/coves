package pds_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// T0 HTTP-seam acceptance: real Indigo DPoP/refresh transport, with an in-process
// resource/authorization server and a copying store. This is not a PDS E2E test.
func TestDeadSessionAcceptance(t *testing.T) {
	for _, nonce := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_nonce", true: "with_nonce"}[nonce], func(t *testing.T) {
			fixture := newSessionFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if nonce {
					w.Header().Set("DPoP-Nonce", "rotated-nonce")
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"revoked"}`))
			})
			client := fixture.client(t)
			err := client.DeleteRecord(context.Background(), "social.coves.actor.block", "record")
			require.Error(t, err)
			if !errors.Is(err, pds.ErrSessionExpired) {
				t.Error("definitive refresh rejection must classify as ErrSessionExpired")
			}
			mapping, _ := xrpc.NewMapper("session-test").Resolve(err)
			assert.Equal(t, http.StatusUnauthorized, mapping.Status, "actual write failure must tell the frontend to authenticate")
			_, err = fixture.store.GetSession(context.Background(), fixture.data.AccountDID, fixture.data.SessionID)
			assert.ErrorIs(t, err, covesoauth.ErrSessionNotFound, "dead session must no longer satisfy subsequent authentication lookups")
			_, err = fixture.store.GetSession(context.Background(), fixture.data.AccountDID, "other-browser")
			require.NoError(t, err, "another session for the same account survives")
		})
	}
}

func TestSessionRefreshNonterminalFailures(t *testing.T) {
	for _, testcase := range []struct {
		name   string
		status int
		body   string
	}{
		{"authorization_server_unavailable", 503, `{"error":"temporarily_unavailable"}`},
		{"invalid_client_is_configuration_failure", 400, `{"error":"invalid_client"}`},
		{"invalid_grant_text_is_not_an_error_code", 400, `{"error":"server_error","error_description":"invalid_grant"}`},
		{"server_failure_with_invalid_grant_body", 503, `{"error":"invalid_grant"}`},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			fixture := newSessionFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testcase.status)
				_, _ = w.Write([]byte(testcase.body))
			})
			err := fixture.client(t).DeleteRecord(context.Background(), "social.coves.actor.block", "record")
			require.Error(t, err)
			require.False(t, pds.IsReauthRequired(err), "transient/configuration failure must not sign the user out")
			_, err = fixture.store.GetSession(context.Background(), fixture.data.AccountDID, fixture.data.SessionID)
			require.NoError(t, err)
		})
	}
}

func TestSessionPermissionFailurePreservesSession(t *testing.T) {
	fixture := newSessionFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("permission denial must not refresh")
		w.WriteHeader(500)
	})
	fixture.permissionDenied.Store(true)
	err := fixture.client(t).DeleteRecord(context.Background(), "social.coves.actor.block", "record")
	require.ErrorIs(t, err, pds.ErrForbidden)
	require.False(t, pds.IsReauthRequired(err))
	_, err = fixture.store.GetSession(context.Background(), fixture.data.AccountDID, fixture.data.SessionID)
	require.NoError(t, err)
}

func TestSessionRotationIsPersistedBeforeWriteRetry(t *testing.T) {
	fixture := newSessionFixture(t, successfulRotation)
	fixture.store.saveErr = errors.New("session database unavailable")
	err := fixture.client(t).DeleteRecord(context.Background(), "social.coves.actor.block", "record")
	assert.Error(t, err, "failed rotation persistence must fail the write")
	require.False(t, pds.IsReauthRequired(err))
	assert.Zero(t, fixture.successfulWrites.Load(), "must not retry the mutation after rotation persistence fails")
	_, err = fixture.store.GetSession(context.Background(), fixture.data.AccountDID, fixture.data.SessionID)
	require.NoError(t, err, "persistence outage must not delete the session")
}

func TestSessionClientsReusePersistedRotation(t *testing.T) {
	var refreshes atomic.Int32
	fixture := newSessionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if refreshes.Add(1) != 1 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		successfulRotation(w, r)
	})
	first, second := fixture.client(t), fixture.client(t)
	require.NoError(t, first.DeleteRecord(context.Background(), "social.coves.actor.block", "first"))
	require.NoError(t, second.DeleteRecord(context.Background(), "social.coves.actor.block", "second"), "a previously constructed client must not reuse the consumed refresh token")
	require.Equal(t, int32(1), refreshes.Load())
	saved, err := fixture.store.GetSession(context.Background(), fixture.data.AccountDID, fixture.data.SessionID)
	require.NoError(t, err)
	require.True(t, saved.AccessToken == "fresh-access" && saved.RefreshToken == "fresh-refresh", "rotated credentials must persist")
}

func successfulRotation(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"access_token":"fresh-access","refresh_token":"fresh-refresh","token_type":"DPoP","expires_in":300}`))
}

type sessionFixture struct {
	app              *oauth.ClientApp
	data             oauth.ClientSessionData
	store            *sessionStore
	successfulWrites atomic.Int32
	permissionDenied atomic.Bool
}

func newSessionFixture(t *testing.T, refresh http.HandlerFunc) *sessionFixture {
	t.Helper()
	fixture := &sessionFixture{store: &sessionStore{sessions: make(map[string]oauth.ClientSessionData)}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			if r.Method != http.MethodPost || r.Header.Get("DPoP") == "" {
				t.Error("refresh must be a DPoP authenticated POST")
			}
			refresh(w, r)
		case "/xrpc/com.atproto.repo.deleteRecord":
			if r.Method != http.MethodPost || r.Header.Get("DPoP") == "" {
				t.Error("write must be a DPoP authenticated POST")
			}
			if fixture.permissionDenied.Load() {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
				return
			}
			if r.Header.Get("Authorization") != "DPoP fresh-access" {
				w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token"`)
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"ExpiredToken"}`))
				return
			}
			fixture.successfulWrites.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Error("unexpected HTTP path")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	key, err := atcrypto.GeneratePrivateKeyP256()
	require.NoError(t, err)
	fixture.data = oauth.ClientSessionData{AccountDID: syntax.DID("did:plc:sessiontest"), SessionID: "browser", HostURL: server.URL, AuthServerURL: server.URL, AuthServerTokenEndpoint: server.URL + "/token", AccessToken: "old-access", RefreshToken: "old-refresh", DPoPPrivateKeyMultibase: key.Multibase()}
	require.NoError(t, fixture.store.SaveSession(context.Background(), fixture.data))
	other := fixture.data
	other.SessionID = "other-browser"
	require.NoError(t, fixture.store.SaveSession(context.Background(), other))
	fixture.app = oauth.NewClientApp(&oauth.ClientConfig{ClientID: server.URL + "/client.json"}, fixture.store)
	fixture.app.Client = &http.Client{Timeout: 3 * time.Second}
	return fixture
}

func (f *sessionFixture) client(t *testing.T) pds.CommitClient {
	t.Helper()
	client, err := pds.NewFromOAuthSession(context.Background(), f.app, &f.data)
	require.NoError(t, err)
	return client
}

// Copies emulate independent database loads; sharing a pointer would conceal
// the stale-client rotation bug. The unused auth-request interface is embedded.
type sessionStore struct {
	oauth.ClientAuthStore
	mutex    sync.Mutex
	sessions map[string]oauth.ClientSessionData
	saveErr  error
}

func (s *sessionStore) GetSession(_ context.Context, did syntax.DID, id string) (*oauth.ClientSessionData, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	data, ok := s.sessions[did.String()+"/"+id]
	if !ok {
		return nil, covesoauth.ErrSessionNotFound
	}
	return &data, nil
}

func (s *sessionStore) SaveSession(_ context.Context, data oauth.ClientSessionData) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.sessions[data.AccountDID.String()+"/"+data.SessionID] = data
	return nil
}

func (s *sessionStore) DeleteSession(_ context.Context, did syntax.DID, id string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete(s.sessions, did.String()+"/"+id)
	return nil
}

// A stored key that cannot be parsed can never sign a request, so the session
// is dead: it must be deleted and the user told to sign in again, whether the
// corruption is found when the client is built or when it first writes.
func TestCorruptSessionKeyIsSessionExpiration(t *testing.T) {
	for _, atConstruction := range []bool{true, false} {
		t.Run(map[bool]string{true: "at_construction", false: "at_write"}[atConstruction], func(t *testing.T) {
			fixture := newSessionFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("a corrupt key must not reach the authorization server")
				w.WriteHeader(500)
			})
			corrupt := func() {
				damaged := fixture.data
				damaged.DPoPPrivateKeyMultibase = "not-a-multibase-key"
				require.NoError(t, fixture.store.SaveSession(context.Background(), damaged))
			}
			var err error
			if atConstruction {
				corrupt()
				_, err = pds.NewFromOAuthSession(context.Background(), fixture.app, &fixture.data)
			} else {
				client := fixture.client(t)
				corrupt()
				err = client.DeleteRecord(context.Background(), "social.coves.actor.block", "record")
			}
			require.ErrorIs(t, err, pds.ErrSessionExpired)
			require.True(t, pds.IsReauthRequired(err))
			assert.Zero(t, fixture.successfulWrites.Load())
			_, err = fixture.store.GetSession(context.Background(), fixture.data.AccountDID, fixture.data.SessionID)
			assert.ErrorIs(t, err, covesoauth.ErrSessionNotFound, "corrupt session must be deleted")
			_, err = fixture.store.GetSession(context.Background(), fixture.data.AccountDID, "other-browser")
			require.NoError(t, err, "another session for the same account survives")
		})
	}
}
