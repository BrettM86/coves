package oauth

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestWebOAuth_DevLoginCanonicalizesInitiatingHostBeforeBinding(t *testing.T) {
	handler, store, _, _ := newCompletionHandler(t)
	handler.client.Config.DevMode = true
	handler.client.Config.PublicURL = "http://127.0.0.1:8080" // coves:allow-host-literal: configured local browser origin, no network request
	handler.client.ClientApp.Config.CallbackURL = handler.client.Config.PublicURL + "/oauth/callback"
	query := url.Values{"handle": {"https://example.com"}, "redirect": {"/c/orchids?sort=new#reply"}, "redirect_uri": {"https://attacker.example/callback"}}
	response := httptest.NewRecorder()
	handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, "http://localhost:8080/oauth/login?"+query.Encode(), nil)) // coves:allow-host-literal: request host input, never dialled
	require.Equal(t, http.StatusFound, response.Code)
	target, err := url.Parse(response.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "http", target.Scheme)
	assert.Equal(t, "127.0.0.1:8080", target.Host, "login must first move to configured callback origin so its host-only cookie reaches callback") // coves:allow-host-literal: expected configured browser origin, never dialled
	assert.Equal(t, "/oauth/login", target.Path)
	assert.Equal(t, query.Get("handle"), target.Query().Get("handle"))
	assert.Equal(t, query.Get("redirect"), target.Query().Get("redirect"))
	assert.Empty(t, store.states, "cross-host redirect must happen before creating a transaction")
	assert.Empty(t, response.Result().Cookies(), "do not bind a login to the wrong hostname")
}
