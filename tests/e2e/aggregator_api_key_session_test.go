//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestAggregatorAPIKeyTakesOverTheBrowserSession pins what createApiKey does
// with the operator's browser OAuth session: it MOVES it to the API key.
//
// The key forwards writes to the aggregator's PDS through a stored OAuth
// session. If that session were a copy of the browser's, both would share one
// refresh-token lineage, and the browser's /oauth/logout — which revokes the
// grant at the PDS — would silently kill the key. So once the key exists the
// browser session is over (the operator signs in again to keep browsing), and
// a logout sent with the old cookie afterwards cannot reach the key's session.
//
//	sign in → createApiKey answers 200 with a ckapi_ key
//	the browser token now answers 401 on a RequireAuth endpoint
//	POST /oauth/logout with that token as the coves_session cookie answers 200
//	the key still uploads an image to the aggregator's PDS
func TestAggregatorAPIKeyTakesOverTheBrowserSession(t *testing.T) {
	p := newPipeline(t)

	// Given an indexed aggregator signed in through the real OAuth web flow.
	aggregatorAccount, _ := indexedAggregator(t, p, "keysess")
	browserToken := p.AppView.SignIn(t, aggregatorAccount)
	browser := p.AppView.As(browserToken)

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// The browser session is live before the key exists, so the 401 below is
	// caused by createApiKey and not by a token getApiKey never accepted.
	require.NoError(t, browser.Query(ctx, "social.coves.aggregator.getApiKey", nil, nil),
		"getApiKey must answer 200 to %s's freshly signed-in browser session", aggregatorAccount.Handle)

	// When the operator creates an API key with that session.
	var created struct {
		Key string `json:"key"`
		DID string `json:"did"`
	}
	require.NoError(t, browser.Procedure(ctx, "social.coves.aggregator.createApiKey", map[string]any{}, &created),
		"createApiKey must answer 200 to %s's browser session", aggregatorAccount.Handle)
	require.True(t, strings.HasPrefix(created.Key, "ckapi_"), "createApiKey must return a ckapi_ key")
	require.Equal(t, aggregatorAccount.DID, created.DID)

	// Then (1) the browser session has ended: the key took it over.
	err := browser.Query(ctx, "social.coves.aggregator.getApiKey", nil, nil)
	require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
		"after createApiKey the browser session must be gone, so getApiKey with the old browser token "+
			"must answer 401; answered: %v", err)

	// (2) Logging out with the old browser cookie still answers 200.
	logout, err := http.NewRequestWithContext(ctx, http.MethodPost, p.AppView.BaseURL+"/oauth/logout", nil)
	require.NoError(t, err)
	for name, values := range p.AppView.Headers {
		for _, value := range values {
			logout.Header.Add(name, value)
		}
	}
	logout.AddCookie(&http.Cookie{Name: "coves_session", Value: browserToken})
	resp, err := p.AppView.HTTP.Do(logout)
	require.NoError(t, err, "POST /oauth/logout")
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "POST /oauth/logout with the old browser cookie must answer 200")

	// (3) The key still forwards a write to the aggregator's PDS through its
	// own stored session, which the logout could not revoke.
	var uploaded struct {
		Blob     testkit.BlobRef `json:"blob"`
		MimeType string          `json:"mimeType"`
	}
	require.NoError(t, p.AppView.As(created.Key).Upload(ctx, "social.coves.media.uploadImage", "image/jpeg",
		testkit.TestJPEG(16, 16), &uploaded),
		"after the browser logout, uploadImage with %s's API key must still reach its PDS", aggregatorAccount.Handle)
	require.Equal(t, "image/jpeg", uploaded.MimeType)
	require.NotEmpty(t, uploaded.Blob.Ref.Link, "uploadImage's blob must carry a CID in ref.$link")
}
