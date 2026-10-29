package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/handlers/defaultavatars"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const defaultAvatarImmutableCacheControl = "public, max-age=31536000, immutable"

var defaultAvatarV1Slugs = []string{
	"anchor", "beach-bucket-and-spade", "branch-of-coral", "clownfish", "compass",
	"dolphin", "eel", "hermit-crab-in-a-shell", "jellyfish", "kelp-frond",
	"life-ring", "lighthouse", "message-in-a-bottle", "narwhal", "nudibranch-sea-slug",
	"ocean-wave", "octopus", "oyster-with-a-pearl", "pelican", "penguin",
	"pufferfish", "puffin", "sand-dollar", "sandcastle", "scallop-shell",
	"sea-anemone", "sea-otter", "sea-turtle", "sea-urchin", "seahorse",
	"seal", "ships-wheel", "snorkel-mask", "spiral-sea-snail-shell", "squid",
	"starfish", "swordfish", "treasure-chest", "whale",
}

func serveDefaultAvatar(t *testing.T, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	RegisterDefaultAvatarRoutes(router, defaultavatars.NewHandler())
	request := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestDefaultAvatarGetServesEveryV1Slug(t *testing.T) {
	for _, slug := range defaultAvatarV1Slugs {
		t.Run(slug, func(t *testing.T) {
			response := serveDefaultAvatar(t, http.MethodGet, "/img/default-avatars/v1/"+slug+".webp", nil)
			require.Equalf(t, http.StatusOK, response.Code, "slug %q status", slug)
			assert.Equalf(t, "image/webp", response.Header().Get("Content-Type"), "slug %q Content-Type", slug)
			assert.NotEmptyf(t, response.Header().Get("ETag"), "slug %q ETag", slug)
			assert.Equalf(t, defaultAvatarImmutableCacheControl, response.Header().Get("Cache-Control"), "slug %q Cache-Control", slug)
			embedded, ok := defaultavatars.Image(slug)
			require.Truef(t, ok, "slug %q has no embedded image", slug)
			assert.Equalf(t, embedded, response.Body.Bytes(), "slug %q body", slug)
		})
	}
}

func TestDefaultAvatarETagsDifferBetweenSlugs(t *testing.T) {
	octopus := serveDefaultAvatar(t, http.MethodGet, "/img/default-avatars/v1/octopus.webp", nil)
	anchor := serveDefaultAvatar(t, http.MethodGet, "/img/default-avatars/v1/anchor.webp", nil)
	require.Equal(t, http.StatusOK, octopus.Code)
	require.Equal(t, http.StatusOK, anchor.Code)
	assert.NotEqual(t, octopus.Header().Get("ETag"), anchor.Header().Get("ETag"))
}

func TestDefaultAvatarHeadMatchesGetHeadersWithEmptyBody(t *testing.T) {
	const target = "/img/default-avatars/v1/octopus.webp"
	get := serveDefaultAvatar(t, http.MethodGet, target, nil)
	head := serveDefaultAvatar(t, http.MethodHead, target, nil)

	require.Equal(t, http.StatusOK, head.Code)
	assert.Equal(t, "image/webp", head.Header().Get("Content-Type"))
	assert.Equal(t, defaultAvatarImmutableCacheControl, head.Header().Get("Cache-Control"))
	assert.NotEmpty(t, head.Header().Get("ETag"))
	assert.Equal(t, get.Header().Get("ETag"), head.Header().Get("ETag"))
	assert.Empty(t, head.Body.Bytes())
}

func TestDefaultAvatarIfNoneMatch(t *testing.T) {
	const target = "/img/default-avatars/v1/octopus.webp"
	first := serveDefaultAvatar(t, http.MethodGet, target, nil)
	etag := first.Header().Get("ETag")
	require.NotEmpty(t, etag)

	t.Run("matching ETag returns 304", func(t *testing.T) {
		response := serveDefaultAvatar(t, http.MethodGet, target, map[string]string{"If-None-Match": etag})
		require.Equal(t, http.StatusNotModified, response.Code)
		assert.Equal(t, defaultAvatarImmutableCacheControl, response.Header().Get("Cache-Control"))
		assert.Empty(t, response.Body.Bytes())
	})

	t.Run("another slug's ETag returns 200", func(t *testing.T) {
		anchor := serveDefaultAvatar(t, http.MethodGet, "/img/default-avatars/v1/anchor.webp", nil)
		anchorETag := anchor.Header().Get("ETag")
		require.NotEmpty(t, anchorETag)
		response := serveDefaultAvatar(t, http.MethodGet, target, map[string]string{"If-None-Match": anchorETag})
		require.Equal(t, http.StatusOK, response.Code)
		assert.NotEmpty(t, response.Body.Bytes())
	})
}

func TestDefaultAvatarNotFoundIsNoStore(t *testing.T) {
	targets := []string{
		"/img/default-avatars/v1/not-a-slug.webp",
		"/img/default-avatars/v2/octopus.webp",
		"/img/default-avatars/v1/ship-s-wheel.webp",
		"/img/default-avatars/v1/octopus.png",
		"/img/default-avatars/v1/octopus",
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, target := range targets {
			t.Run(method+" "+target, func(t *testing.T) {
				response := serveDefaultAvatar(t, method, target, nil)
				assert.Equal(t, http.StatusNotFound, response.Code)
				// chi's default not-found sets no Cache-Control, so no-store
				// proves the handler answered.
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			})
		}
	}
}

func TestDefaultAvatarIgnoresOriginAndQuery(t *testing.T) {
	const target = "/img/default-avatars/v1/octopus.webp"
	plain := serveDefaultAvatar(t, http.MethodGet, target, nil)
	require.Equal(t, http.StatusOK, plain.Code)
	require.Equal(t, defaultAvatarImmutableCacheControl, plain.Header().Get("Cache-Control"))

	t.Run("Origin header", func(t *testing.T) {
		response := serveDefaultAvatar(t, http.MethodGet, target, map[string]string{"Origin": "https://example.test"})
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, defaultAvatarImmutableCacheControl, response.Header().Get("Cache-Control"))
	})

	t.Run("query string", func(t *testing.T) {
		response := serveDefaultAvatar(t, http.MethodGet, target+"?x=1", nil)
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, plain.Header().Get("ETag"), response.Header().Get("ETag"))
		assert.Equal(t, defaultAvatarImmutableCacheControl, response.Header().Get("Cache-Control"))
		assert.Equal(t, plain.Body.Bytes(), response.Body.Bytes())
	})
}
