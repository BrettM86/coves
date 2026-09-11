package oauth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Once the authorization server has answered, the previous refresh token is
// consumed. Abandoning the request afterwards must not lose the rotation.
func TestSessionOperationPersistsRotationAfterRequestCancellation(t *testing.T) {
	store := &contextCheckingStore{ClientAuthStore: indigooauth.NewMemStore()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"DPoP"}`))
	}))
	t.Cleanup(server.Close)
	data := operationSessionData(t, server.URL, "")
	require.NoError(t, store.SaveSession(context.Background(), data))
	app := indigooauth.NewClientApp(&indigooauth.ClientConfig{ClientID: server.URL + "/client.json"}, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.Client = &http.Client{Transport: cancelAfterResponse{base: server.Client().Transport, cancel: cancel}}

	err := RunSessionOperation(ctx, app, data.AccountDID, data.SessionID, func(session *indigooauth.ClientSession) error {
		_, err := session.RefreshTokens(ctx)
		return err
	})
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled, "the test must have canceled the request before persistence")
	saved, err := store.GetSession(context.Background(), data.AccountDID, data.SessionID)
	require.NoError(t, err)
	assert.True(t, saved.AccessToken == "rotated-access" && saved.RefreshToken == "rotated-refresh", "rotated credentials must be saved despite the canceled request")
}

func TestSessionOperationDeletesCorruptSession(t *testing.T) {
	store := indigooauth.NewMemStore()
	data := operationSessionData(t, "http://127.0.0.1:1", "not-a-multibase-key")
	require.NoError(t, store.SaveSession(context.Background(), data))
	sibling := data
	sibling.SessionID = "other-browser"
	require.NoError(t, store.SaveSession(context.Background(), sibling))
	app := indigooauth.NewClientApp(&indigooauth.ClientConfig{ClientID: "http://127.0.0.1:1/client.json"}, store)

	ran := false
	err := RunSessionOperation(context.Background(), app, data.AccountDID, data.SessionID, func(*indigooauth.ClientSession) error {
		ran = true
		return nil
	})
	require.ErrorIs(t, err, ErrSessionCorrupt)
	assert.False(t, ran, "an unusable session must not run its operation")
	_, err = store.GetSession(context.Background(), data.AccountDID, data.SessionID)
	require.Error(t, err, "corrupt session must be deleted so authentication lookups stop succeeding")
	_, err = store.GetSession(context.Background(), sibling.AccountDID, sibling.SessionID)
	require.NoError(t, err, "sibling session must survive")
}

func TestSessionOperationStoreFailureStaysTransient(t *testing.T) {
	failure := errors.New("database unavailable")
	store := &contextCheckingStore{ClientAuthStore: indigooauth.NewMemStore(), getFailure: failure}
	app := indigooauth.NewClientApp(&indigooauth.ClientConfig{ClientID: "http://127.0.0.1:1/client.json"}, store)
	err := RunSessionOperation(context.Background(), app, syntax.DID("did:plc:operationtest"), "browser", func(*indigooauth.ClientSession) error { return nil })
	require.ErrorIs(t, err, failure)
	assert.False(t, errors.Is(err, ErrSessionCorrupt) || errors.Is(err, ErrRefreshRejected) || errors.Is(err, ErrSessionNotFound), "store outage must not be classified as a dead session")
}

func operationSessionData(t *testing.T, serverURL, keyMultibase string) indigooauth.ClientSessionData {
	t.Helper()
	if keyMultibase == "" {
		key, err := atcrypto.GeneratePrivateKeyP256()
		require.NoError(t, err)
		keyMultibase = key.Multibase()
	}
	return indigooauth.ClientSessionData{
		AccountDID:              syntax.DID("did:plc:operationtest"),
		SessionID:               "browser",
		HostURL:                 serverURL,
		AuthServerURL:           serverURL,
		AuthServerTokenEndpoint: serverURL + "/token",
		AccessToken:             "initial-access",
		RefreshToken:            "initial-refresh",
		DPoPPrivateKeyMultibase: keyMultibase,
	}
}

// A real database rejects work on a canceled context; the memory store does not.
type contextCheckingStore struct {
	indigooauth.ClientAuthStore
	getFailure error
}

func (s *contextCheckingStore) GetSession(ctx context.Context, did syntax.DID, id string) (*indigooauth.ClientSessionData, error) {
	if s.getFailure != nil {
		return nil, s.getFailure
	}
	return s.ClientAuthStore.GetSession(ctx, did, id)
}

func (s *contextCheckingStore) SaveSession(ctx context.Context, data indigooauth.ClientSessionData) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.ClientAuthStore.SaveSession(ctx, data)
}

// Buffers the token response, then cancels the request context before Indigo
// decodes it and reaches the persistence callback.
type cancelAfterResponse struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (transport cancelAfterResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	transport.cancel()
	return response, nil
}
