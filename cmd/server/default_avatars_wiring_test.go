package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	imageproxyhandlers "Coves/internal/api/handlers/imageproxy"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaultAvatarWiringProxyService answers every image request with fixed bytes
// so a response body proves the request reached the image proxy handler.
type defaultAvatarWiringProxyService struct{ body []byte }

func (service defaultAvatarWiringProxyService) GetImageResolvingPDS(context.Context, string, string, string, func(context.Context) (string, error)) ([]byte, error) {
	return service.body, nil
}

func (defaultAvatarWiringProxyService) CheckServable(context.Context, string, string) error {
	return nil
}

func TestRegisterImageRoutesServesDefaultAvatars(t *testing.T) {
	proxyBody := []byte("image proxy wiring bytes")
	for _, test := range []struct {
		name  string
		proxy *imageproxyhandlers.Handler
	}{
		{name: "image proxy disabled"},
		{name: "image proxy enabled", proxy: imageproxyhandlers.NewHandler(defaultAvatarWiringProxyService{body: proxyBody}, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := chi.NewRouter()
			registerImageRoutes(router, &application{imageProxyHandler: test.proxy})

			avatar := httptest.NewRecorder()
			router.ServeHTTP(avatar, httptest.NewRequest(http.MethodGet, "/img/default-avatars/v1/octopus.webp", nil))
			require.Equal(t, http.StatusOK, avatar.Code)
			assert.Equal(t, "image/webp", avatar.Header().Get("Content-Type"))

			if test.proxy == nil {
				return
			}
			proxied := httptest.NewRecorder()
			router.ServeHTTP(proxied, httptest.NewRequest(http.MethodGet,
				"/img/avatar/plain/did:plc:ewvi7nxzyoun6zhxrhs64oiz/bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", nil))
			require.Equal(t, http.StatusOK, proxied.Code)
			assert.Equal(t, proxyBody, proxied.Body.Bytes())
		})
	}
}
