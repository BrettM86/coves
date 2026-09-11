package pds_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T0 HTTP seam: the nonce challenge must complete before the real resource
// retry, and that retry must see credentials already saved in the session store.
func TestSessionRefreshNonceChallengePersistsRotationBeforeResourceRetry(t *testing.T) {
	var refreshes atomic.Int32
	fixture := newSessionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch refreshes.Add(1) {
		case 1:
			w.Header().Set("DPoP-Nonce", "refresh-challenge")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
		case 2:
			parts := strings.Split(r.Header.Get("DPoP"), ".")
			if !assert.Len(t, parts, 3, "refresh retry must carry a DPoP proof") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var claims struct {
				Nonce string `json:"nonce"`
			}
			if !assert.NoError(t, json.Unmarshal(payload, &claims)) || !assert.Equal(t, "refresh-challenge", claims.Nonce) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			successfulRotation(w, r)
		default:
			t.Error("nonce challenge must require exactly one token retry")
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	var sequence []string
	fixture.app.Client.Transport = nonceObservationTransport(func(r *http.Request) (*http.Response, error) {
		sequence = append(sequence, r.URL.Path)
		if r.URL.Path == "/xrpc/com.atproto.repo.deleteRecord" && r.Header.Get("Authorization") == "DPoP fresh-access" {
			saved, err := fixture.store.GetSession(r.Context(), fixture.data.AccountDID, fixture.data.SessionID)
			require.NoError(t, err)
			require.True(t, saved.AccessToken == "fresh-access" && saved.RefreshToken == "fresh-refresh", "rotation must be persisted before sending the resource retry")
		}
		return http.DefaultTransport.RoundTrip(r)
	})

	err := fixture.client(t).DeleteRecord(context.Background(), "social.coves.actor.block", "record")
	require.NoError(t, err, "a legitimate nonce challenge must recover successfully")
	require.Equal(t, []string{
		"/xrpc/com.atproto.repo.deleteRecord", "/token", "/token", "/xrpc/com.atproto.repo.deleteRecord",
	}, sequence)
	require.Equal(t, int32(2), refreshes.Load())
	require.Equal(t, int32(1), fixture.successfulWrites.Load())
	saved, err := fixture.store.GetSession(context.Background(), fixture.data.AccountDID, fixture.data.SessionID)
	require.NoError(t, err)
	require.True(t, saved.AccessToken == "fresh-access" && saved.RefreshToken == "fresh-refresh", "successful refresh must preserve the rotated credentials")
}

type nonceObservationTransport func(*http.Request) (*http.Response, error)

func (transport nonceObservationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
