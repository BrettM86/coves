//go:build dev

package oauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebOAuth_DevLoginPersistenceFailureStopsProviderNavigation(t *testing.T) {
	handler, store, _, _ := newCompletionHandler(t)
	handler.client.Config.DevMode = true
	handler.devAuthResolver = &DevAuthResolver{Client: handler.client.ClientApp.Client}
	store.bindingError = errors.New("private database details")
	response := httptest.NewRecorder()
	handler.HandleLogin(response, httptest.NewRequest(http.MethodGet, handler.client.Config.PublicURL+"/oauth/login?"+url.Values{"did": {completionDID}, "redirect": {"/saved?sort=new#reply"}}.Encode(), nil))
	require.Len(t, store.states, 1, "dev provider flow must reach persistence")
	assertWebLoginError(t, response, "server_error", "/saved?sort=new#reply")
	for _, cookie := range response.Result().Cookies() {
		assert.Less(t, cookie.MaxAge, 0)
	}
	assert.NotContains(t, response.Body.String(), "private")
}
