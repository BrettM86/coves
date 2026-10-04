package imageproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/core/imageproxy"
	"github.com/go-chi/chi/v5"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandler_CDNPurgeCacheHeadersThroughRouter(t *testing.T) {
	parsed, err := cid.Decode(validTestCID)
	require.NoError(t, err)
	require.Equal(t, validTestCID, parsed.String())
	alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	require.NotEqual(t, validTestCID, alias)

	const long = "public, max-age=86400, s-maxage=31536000"
	const short = "public, max-age=86400"
	const etag = `"avatar-` + validTestCID + `"`
	defaultBases := []string{"https://img.example.test", "https://cdn.example.test"}
	for _, test := range []struct {
		name, host, owner, spelling, suffix, origin, wantCache string
		bases                                                  []string
		blocked                                                bool
		serviceError                                           error
		wantStatus                                             int
	}{
		{name: "purge on primary base", host: "img.example.test", owner: validTestDID, spelling: validTestCID, bases: defaultBases, wantCache: long},
		{name: "purge on second base", host: "cdn.example.test", owner: validTestDID, spelling: validTestCID, bases: defaultBases, wantCache: long},
		{name: "purge on other host", host: "other.example.test", owner: validTestDID, spelling: validTestCID, bases: defaultBases, wantCache: short},
		{name: "purge on query", host: "img.example.test", owner: validTestDID, spelling: validTestCID, suffix: "?w=1", bases: defaultBases, wantCache: "no-store"},
		{name: "purge on bare question mark", host: "img.example.test", owner: validTestDID, spelling: validTestCID, suffix: "?", bases: defaultBases, wantCache: "no-store"},
		{name: "purge on Origin header", host: "img.example.test", owner: validTestDID, spelling: validTestCID, origin: "https://other.example.test", bases: defaultBases, wantCache: short},
		{name: "purge on alternate CID spelling", host: "img.example.test", owner: validTestDID, spelling: alias, bases: defaultBases, wantCache: short},
		{name: "purge on escaped port DID", host: "img.example.test", owner: "did:web:localhost%3A2583", spelling: validTestCID, bases: defaultBases, wantCache: short},
		{name: "purge base with path", host: "img.example.test", owner: validTestDID, spelling: validTestCID, bases: []string{"https://img.example.test/prefix"}, wantCache: short},
		{name: "purge off bare question mark", host: "img.example.test", owner: validTestDID, spelling: validTestCID, suffix: "?", wantCache: "no-store"},
		{name: "guard uppercase DID", host: "img.example.test", owner: "did:plc:Z72i7hdynmk6r22z27h6tvur", spelling: validTestCID, bases: defaultBases, wantCache: "no-store", wantStatus: http.StatusBadRequest},
		{name: "guard blocked", host: "img.example.test", owner: validTestDID, spelling: validTestCID, bases: defaultBases, wantCache: "no-store", wantStatus: http.StatusNotFound, blocked: true},
		{name: "guard service error", host: "img.example.test", owner: validTestDID, spelling: validTestCID, bases: defaultBases, wantCache: "no-store", wantStatus: http.StatusInternalServerError, serviceError: errors.New("image unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &mockService{
				getImageFunc: func(_ context.Context, preset, owner, requestedCID, _ string) ([]byte, error) {
					assert.Equal(t, "avatar", preset)
					assert.Equal(t, test.owner, owner)
					assert.Equal(t, validTestCID, requestedCID)
					if test.blocked {
						return nil, imageproxy.ErrBlobBlocked
					}
					if test.serviceError != nil {
						return nil, test.serviceError
					}
					return []byte("image bytes"), nil
				},
				isBlobBlockedFunc: func(context.Context, string, string) (bool, error) { return test.blocked, nil },
			}
			options := []HandlerOption{}
			if test.bases != nil {
				options = append(options, WithCDNPurgeBaseURLs(test.bases))
			}
			router := chi.NewRouter()
			router.Get("/img/{preset}/plain/{did}/{cid}", NewHandler(service, resolverForPDS("https://pds.example.test"), options...).HandleImage)
			request := func(conditional bool) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/img/avatar/plain/"+test.owner+"/"+test.spelling+test.suffix, nil)
				req.Host = test.host
				if test.origin != "" {
					req.Header.Set("Origin", test.origin)
				}
				if conditional {
					req.Header.Set("If-None-Match", etag)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				return response
			}
			served := request(false)
			if test.wantStatus != 0 {
				require.Equal(t, test.wantStatus, served.Code)
				assert.Equal(t, test.wantCache, served.Header().Get("Cache-Control"))
				assert.Empty(t, served.Header().Get("ETag"))
				if test.blocked {
					conditional := request(true)
					assert.Equal(t, http.StatusNotFound, conditional.Code)
					assert.Equal(t, "no-store", conditional.Header().Get("Cache-Control"))
					assert.Empty(t, conditional.Header().Get("ETag"))
				}
				return
			}
			require.Equal(t, http.StatusOK, served.Code, "body: %s", served.Body.String())
			assert.Equal(t, test.wantCache, served.Header().Get("Cache-Control"))
			assert.NotContains(t, served.Header().Get("Cache-Control"), "immutable")
			assert.Equal(t, etag, served.Header().Get("ETag"))
			conditional := request(true)
			require.Equal(t, http.StatusNotModified, conditional.Code, "body: %s", conditional.Body.String())
			assert.Equal(t, test.wantCache, conditional.Header().Get("Cache-Control"))
			assert.Equal(t, served.Header().Get("Cache-Control"), conditional.Header().Get("Cache-Control"))
			assert.Equal(t, etag, conditional.Header().Get("ETag"))
		})
	}
}
