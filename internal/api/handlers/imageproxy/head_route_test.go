package imageproxy_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	imageproxyhandlers "Coves/internal/api/handlers/imageproxy"
	"Coves/internal/api/routes"
	imageproxycore "Coves/internal/core/imageproxy"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	imagePath = "/img/avatar/plain/did:plc:z72i7hdynmk6r22z27h6tvur/bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	imageETag = `"r2-avatar-bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"`
)

type headRouteService struct {
	image    []byte
	fetchErr error
	blocked  bool
}

func (s headRouteService) GetImageResolvingPDS(context.Context, string, string, string, func(context.Context) (string, error)) ([]byte, error) {
	return s.image, s.fetchErr
}

func (s headRouteService) CheckServable(context.Context, string, string) error {
	if s.blocked {
		return imageproxycore.ErrBlobBlocked
	}
	return nil
}

func headRouteRouter(service headRouteService) http.Handler {
	router := chi.NewRouter()
	routes.RegisterImageProxyRoutes(router, imageproxyhandlers.NewHandler(service, nil))
	return router
}

func headRouteRequest(router http.Handler, method, path, ifNoneMatch string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestImageProxyRoute_HEADServesImageHeadersWithoutBody(t *testing.T) {
	t.Parallel()
	image := bytes.Repeat([]byte{0xab}, 3073)
	router := headRouteRouter(headRouteService{image: image})

	get := headRouteRequest(router, http.MethodGet, imagePath, "")
	require.Equal(t, 200, get.Code)
	assert.Equal(t, "image/jpeg", get.Header().Get("Content-Type"))
	assert.Equal(t, "3073", get.Header().Get("Content-Length"))
	assert.Equal(t, "public, max-age=86400", get.Header().Get("Cache-Control"))
	assert.Equal(t, imageETag, get.Header().Get("ETag"))
	assert.Equal(t, image, get.Body.Bytes())

	head := headRouteRequest(router, http.MethodHead, imagePath, "")
	assert.Equal(t, get.Code, head.Code)
	for _, header := range []string{"Content-Type", "Content-Length", "Cache-Control", "ETag"} {
		assert.Equal(t, get.Header().Get(header), head.Header().Get(header), header)
	}
	assert.Empty(t, head.Body.Bytes())
}

func TestImageProxyRoute_HEADErrorsMatchGETWithoutBody(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		service     headRouteService
		path        string
		ifNoneMatch string
		status      int
		body        string
		length      string
	}{
		{"blocked fetch", headRouteService{fetchErr: imageproxycore.ErrBlobBlocked}, imagePath, "", 404, "blob not found", "14"},
		{"blocked conditional", headRouteService{blocked: true}, imagePath, imageETag, 404, "blob not found", "14"},
		{"invalid preset", headRouteService{}, "/img/invalid/plain/did:plc:z72i7hdynmk6r22z27h6tvur/bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "", 400, "invalid preset: invalid", "23"},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := headRouteRouter(test.service)
			get := headRouteRequest(router, http.MethodGet, test.path, test.ifNoneMatch)
			require.Equal(t, test.status, get.Code)
			assert.Equal(t, "text/plain; charset=utf-8", get.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", get.Header().Get("Cache-Control"))
			assert.Equal(t, test.length, get.Header().Get("Content-Length"))
			assert.Equal(t, test.body, get.Body.String())
			assert.NotEmpty(t, get.Body.Bytes())

			head := headRouteRequest(router, http.MethodHead, test.path, test.ifNoneMatch)
			assert.Equal(t, get.Code, head.Code)
			for _, header := range []string{"Content-Type", "Content-Length", "Cache-Control"} {
				assert.Equal(t, get.Header().Get(header), head.Header().Get(header), header)
			}
			assert.Empty(t, head.Body.Bytes())
		})
	}
}

func TestImageProxyRoute_HEADNotModifiedMatchesGETWithoutBody(t *testing.T) {
	t.Parallel()
	router := headRouteRouter(headRouteService{})
	get := headRouteRequest(router, http.MethodGet, imagePath, imageETag)
	require.Equal(t, 304, get.Code)
	assert.Equal(t, imageETag, get.Header().Get("ETag"))
	assert.Equal(t, "public, max-age=86400", get.Header().Get("Cache-Control"))
	assert.Empty(t, get.Body.Bytes())

	head := headRouteRequest(router, http.MethodHead, imagePath, imageETag)
	assert.Equal(t, 304, head.Code)
	assert.Equal(t, get.Header().Get("ETag"), head.Header().Get("ETag"))
	assert.Equal(t, get.Header().Get("Cache-Control"), head.Header().Get("Cache-Control"))
	assert.Empty(t, head.Body.Bytes())
}
