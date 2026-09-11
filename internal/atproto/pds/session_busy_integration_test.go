//go:build integration

package pds_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// A PDS write that cannot obtain its session within the lock timeout has not
// started, so it is retryable: the session is alive and the client must not be
// signed out or told the server failed. This waits the full production lock
// timeout, since the store exposes no shorter setting.
func TestAuthenticatedPDSOperationReportsSessionBusyAsRetryable(t *testing.T) {
	db := testkit.DB(t)
	store := covesoauth.NewPostgresOAuthStore(db, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a busy session must never reach the PDS or authorization server")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	key, err := atcrypto.GeneratePrivateKeyP256()
	require.NoError(t, err)
	data := oauth.ClientSessionData{AccountDID: syntax.DID("did:plc:sessionbusy"), SessionID: "browser", HostURL: server.URL, AuthServerURL: server.URL, AuthServerTokenEndpoint: server.URL + "/token", AccessToken: "access", RefreshToken: "refresh", DPoPPrivateKeyMultibase: key.Multibase()}
	require.NoError(t, store.SaveSession(context.Background(), data))
	app := oauth.NewClientApp(&oauth.ClientConfig{ClientID: server.URL + "/client.json"}, store)
	app.Client = server.Client()
	client, err := pds.NewFromOAuthSession(context.Background(), app, &data)
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	ownerContext, cancelOwner := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelOwner()
	ownerResult := make(chan error, 1)
	go func() {
		ownerResult <- covesoauth.RunSessionOperation(ownerContext, app, data.AccountDID, data.SessionID, func(*oauth.ClientSession) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ownerContext.Done():
				return ownerContext.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ownerContext.Done():
		t.Fatal("owner never entered operation")
	}

	err = client.DeleteRecord(context.Background(), "social.coves.actor.block", "record")
	require.ErrorIs(t, err, covesoauth.ErrSessionBusy)
	assert.ErrorIs(t, err, pds.ErrSessionBusy, "contention must be reported in the PDS error vocabulary")
	assert.False(t, pds.IsReauthRequired(err), "a busy session is alive; the client must not be signed out")
	mapping, matched := xrpc.NewMapper("session-busy-test").Resolve(err)
	assert.True(t, matched, "contention must not fall through to the generic 500")
	assert.Equal(t, http.StatusServiceUnavailable, mapping.Status)
	assert.Equal(t, "SessionBusy", mapping.Code)

	release <- struct{}{}
	require.NoError(t, <-ownerResult)
	saved, err := store.GetSession(context.Background(), data.AccountDID, data.SessionID)
	require.NoError(t, err)
	assert.Equal(t, "refresh", saved.RefreshToken, "busy waiter must leave the stored session untouched")
}
