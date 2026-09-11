//go:build integration

package oauth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/tests/testkit"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebOAuthStore_PersistsAndClaimsBindingWithoutDeletingIndigoRequest(t *testing.T) {
	t.Parallel()
	database := testkit.DB(t)
	store := NewPostgresOAuthStore(database, 0)
	bindingStore, ok := store.(WebOAuthStore)
	require.True(t, ok)
	ctx := context.Background()
	const state = "web-persistence"
	request := indigooauth.AuthRequestData{State: state, PKCEVerifier: "pkce-retained", AuthServerURL: "https://auth.example.com", Scopes: []string{"atproto"}}
	require.NoError(t, store.SaveAuthRequestInfo(ctx, request))
	data := WebOAuthData{BrowserNonce: "browser-owner", ReturnURL: "/c/gardening?sort=new#comments", ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Truncate(time.Microsecond)}
	require.NoError(t, bindingStore.SaveWebOAuthData(ctx, state, data))
	// A new instance proves this is database persistence, not process memory.
	restarted := NewPostgresOAuthStore(database, 0).(WebOAuthStore)
	claimed, err := restarted.ClaimWebOAuthData(ctx, state, data.BrowserNonce)
	require.NoError(t, err)
	require.NotNil(t, claimed, "successful claim must return persisted web transaction")
	assert.Equal(t, data, *claimed)
	retained, err := store.GetAuthRequestInfo(ctx, state)
	require.NoError(t, err, "claim must leave the PKCE request readable for Indigo")
	assert.Equal(t, request.PKCEVerifier, retained.PKCEVerifier)
	replay, err := restarted.ClaimWebOAuthData(ctx, state, data.BrowserNonce)
	require.Error(t, err, "one browser binding can be consumed only once")
	assert.Nil(t, replay)
}

func TestWebOAuthStore_InvalidClaimsDoNotConsumeOwnerBinding(t *testing.T) {
	t.Parallel()
	store := NewPostgresOAuthStore(testkit.DB(t), 0)
	bindingStore := store.(WebOAuthStore)
	ctx := context.Background()
	require.NoError(t, store.SaveAuthRequestInfo(ctx, indigooauth.AuthRequestData{State: "owner", PKCEVerifier: "pkce", AuthServerURL: "https://auth.example.com"}))
	require.NoError(t, bindingStore.SaveWebOAuthData(ctx, "owner", WebOAuthData{BrowserNonce: "owner-nonce", ReturnURL: "/saved", ExpiresAt: time.Now().Add(10 * time.Minute)}))
	for _, nonce := range []string{"", "other-browser"} {
		result, err := bindingStore.ClaimWebOAuthData(ctx, "owner", nonce)
		require.Error(t, err, "missing or mismatched browser proof must fail")
		assert.Nil(t, result)
	}
	result, err := bindingStore.ClaimWebOAuthData(ctx, "owner", "owner-nonce")
	require.NoError(t, err, "invalid attempts must not consume the owner's binding")
	require.NotNil(t, result)
	assert.Equal(t, "/saved", result.ReturnURL)
}

func TestWebOAuthStore_ExpiredAndMissingBindingsFailClosed(t *testing.T) {
	t.Parallel()
	store := NewPostgresOAuthStore(testkit.DB(t), 0)
	bindingStore := store.(WebOAuthStore)
	ctx := context.Background()
	require.NoError(t, store.SaveAuthRequestInfo(ctx, indigooauth.AuthRequestData{State: "expired", PKCEVerifier: "pkce", AuthServerURL: "https://auth.example.com"}))
	require.NoError(t, bindingStore.SaveWebOAuthData(ctx, "expired", WebOAuthData{BrowserNonce: "nonce", ReturnURL: "/saved", ExpiresAt: time.Now().Add(-time.Second)}))
	for _, state := range []string{"expired", "missing"} {
		result, err := bindingStore.ClaimWebOAuthData(ctx, state, "nonce")
		require.Error(t, err, "expired or absent server binding must fail regardless of browser cookie lifetime")
		assert.Nil(t, result)
	}
	require.ErrorIs(t, bindingStore.SaveWebOAuthData(ctx, "missing", WebOAuthData{BrowserNonce: "nonce", ReturnURL: "/saved", ExpiresAt: time.Now().Add(time.Minute)}), ErrWebBindingNotSaved, "cannot bind an absent OAuth request")
}

func TestWebOAuthStore_ConcurrentClaimHasExactlyOneWinner(t *testing.T) {
	t.Parallel()
	database := testkit.DB(t)
	store := NewPostgresOAuthStore(database, 0)
	bindingStore := store.(WebOAuthStore)
	ctx := context.Background()
	require.NoError(t, store.SaveAuthRequestInfo(ctx, indigooauth.AuthRequestData{State: "concurrent", PKCEVerifier: "pkce", AuthServerURL: "https://auth.example.com"}))
	require.NoError(t, bindingStore.SaveWebOAuthData(ctx, "concurrent", WebOAuthData{BrowserNonce: "nonce", ReturnURL: "/saved", ExpiresAt: time.Now().Add(time.Minute)}))
	start := make(chan struct{})
	var waiting sync.WaitGroup
	var winners atomic.Int64
	for range 12 {
		waiting.Go(func() {
			<-start
			independent := NewPostgresOAuthStore(database, 0).(WebOAuthStore)
			data, err := independent.ClaimWebOAuthData(ctx, "concurrent", "nonce")
			if err == nil && data != nil {
				winners.Add(1)
			}
		})
	}
	close(start)
	waiting.Wait()
	assert.Equal(t, int64(1), winners.Load(), "atomic claim across independent store instances must have exactly one winner")
	_, err := store.GetAuthRequestInfo(ctx, "concurrent")
	require.NoError(t, err, "the winner still needs Indigo's auth request")
}
