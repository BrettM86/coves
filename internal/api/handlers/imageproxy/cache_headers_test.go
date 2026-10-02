package imageproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandler_RoutedImageCacheHeaders(t *testing.T) {
	const path = "/img/avatar/plain/" + validTestDID + "/" + validTestCID
	const cacheControl = "public, max-age=86400"
	const etag = `"avatar-` + validTestCID + `"`

	route := func(service Service) http.Handler {
		router := chi.NewRouter()
		router.Get("/img/{preset}/plain/{did}/{cid}", NewHandler(service, resolverForPDS("https://pds.example.com")).HandleImage)
		return router
	}
	request := func(router http.Handler, ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}

	t.Run("200 and matching 304 advertise the same one-day policy and ETag", func(t *testing.T) {
		service := &mockService{getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
			return []byte("image bytes"), nil
		}}
		router := route(service)
		served := request(router, "")
		require.Equal(t, http.StatusOK, served.Code)
		assert.Equal(t, cacheControl, served.Header().Get("Cache-Control"))
		assert.NotContains(t, served.Header().Get("Cache-Control"), "s-maxage")
		assert.NotContains(t, served.Header().Get("Cache-Control"), "immutable")
		assert.Equal(t, etag, served.Header().Get("ETag"))

		conditional := request(router, served.Header().Get("ETag"))
		require.Equal(t, http.StatusNotModified, conditional.Code)
		assert.Equal(t, cacheControl, conditional.Header().Get("Cache-Control"))
		assert.Equal(t, served.Header().Get("Cache-Control"), conditional.Header().Get("Cache-Control"))
		assert.Equal(t, served.Header().Get("ETag"), conditional.Header().Get("ETag"))
	})

	// A purge names only the bare URL, so a query-string variant must never be
	// shared-cacheable: it is served, unchanged, with no-store.
	t.Run("query-string variant is served as no-store on 200 and 304", func(t *testing.T) {
		service := &mockService{getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
			return []byte("image bytes"), nil
		}}
		router := route(service)
		requestPath := func(target, ifNoneMatch string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if ifNoneMatch != "" {
				req.Header.Set("If-None-Match", ifNoneMatch)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			return response
		}

		bare := requestPath(path, "")
		require.Equal(t, http.StatusOK, bare.Code)
		assert.Equal(t, cacheControl, bare.Header().Get("Cache-Control"))

		variant := requestPath(path+"?x=1", "")
		require.Equal(t, http.StatusOK, variant.Code)
		assert.Equal(t, "image bytes", variant.Body.String())
		assert.Equal(t, etag, variant.Header().Get("ETag"))
		assert.Equal(t, "no-store", variant.Header().Get("Cache-Control"))

		conditionalVariant := requestPath(path+"?x=1", etag)
		require.Equal(t, http.StatusNotModified, conditionalVariant.Code)
		assert.Equal(t, "no-store", conditionalVariant.Header().Get("Cache-Control"))

		conditionalBare := requestPath(path, etag)
		require.Equal(t, http.StatusNotModified, conditionalBare.Code)
		assert.Equal(t, cacheControl, conditionalBare.Header().Get("Cache-Control"))
	})

	for _, test := range []struct {
		name        string
		serveFirst  bool
		conditional bool
	}{
		// The mock service returns ErrBlobBlocked itself and never calls the
		// isBlobBlockedFunc stub: this case covers only the handler's mapping.
		{name: "service ErrBlobBlocked maps to an uncacheable 404"},
		// The handler calls IsBlobBlocked itself before it may answer 304.
		{name: "block check refuses a matching conditional after a 200", serveFirst: true, conditional: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			blocked := false
			service := &mockService{
				isBlobBlockedFunc: func(context.Context, string, string) (bool, error) { return blocked, nil },
				getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
					if blocked {
						return nil, imageproxy.ErrBlobBlocked
					}
					return []byte("previously served image"), nil
				},
			}
			router := route(service)
			if test.serveFirst {
				require.Equal(t, http.StatusOK, request(router, "").Code)
			}
			blocked = true
			ifNoneMatch := ""
			if test.conditional {
				ifNoneMatch = etag
			}
			response := request(router, ifNoneMatch)
			require.Equal(t, http.StatusNotFound, response.Code)
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Empty(t, response.Header().Get("ETag"))
			assert.NotContains(t, response.Body.String(), "previously served image")
		})
	}

	t.Run("blocked warm disk cache cannot serve stale bytes", func(t *testing.T) {
		cache, err := imageproxy.NewDiskCache(t.TempDir(), 1, 0)
		require.NoError(t, err)
		require.NoError(t, cache.Set("avatar", validTestDID, validTestCID, []byte("cached image bytes")))
		cached, found, err := cache.Get("avatar", validTestDID, validTestCID)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, []byte("cached image bytes"), cached)

		processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
		require.NoError(t, err)
		service, err := imageproxy.NewService(cache, processor, imageproxy.NewPDSFetcher(time.Second, 1),
			blockedBlobChecker{}, imageproxy.Config{
				MaxConcurrentProcesses: 1, ProcessQueueWait: time.Second, MaxInFlightRequests: 1,
			})
		require.NoError(t, err)
		response := request(route(service), "")
		require.Equal(t, http.StatusNotFound, response.Code)
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		assert.Empty(t, response.Header().Get("ETag"))
		assert.NotContains(t, response.Body.String(), "cached image bytes")
	})

	t.Run("failed fetch is not cacheable", func(t *testing.T) {
		service := &mockService{getImageFunc: func(context.Context, string, string, string, string) ([]byte, error) {
			return nil, imageproxy.ErrPDSFetchFailed
		}}
		response := request(route(service), "")
		require.Equal(t, http.StatusBadGateway, response.Code)
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	})
}

type blockedBlobChecker struct{}

func (blockedBlobChecker) IsBlocked(context.Context, string, string) (bool, error) {
	return true, nil
}
