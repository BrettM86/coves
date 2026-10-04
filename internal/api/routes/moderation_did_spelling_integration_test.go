//go:build integration

package routes_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModerationMediaBlockHoldsForEveryDIDSpelling(t *testing.T) {
	const preset = "content_preview"
	h, _ := newModerationMediaHarness(t, false)
	imageX, imageY := mediaImageCID("blocked plc spelling"), mediaImageCID("blocked web spelling")
	webOwner := "did:web:mediaowner.test"
	fixtures.User(t, h.db, testkit.UniqueIDWithPrefix(t, "webowner")+".test", webOwner)
	h.remove(t, h.comment(t, h.ownerA, imageX), "social.coves.moderation.defs#reasonSpam")
	h.remove(t, h.comment(t, webOwner, imageY), "social.coves.moderation.defs#reasonSpam")
	store := postgres.NewModerationRepository(h.db)
	for _, blockedPair := range []mediaBlobKey{{h.ownerA, imageX}, {webOwner, imageY}} {
		blocked, err := store.IsBlocked(t.Context(), blockedPair.did, blockedPair.cid)
		require.NoError(t, err)
		require.True(t, blocked, "removal must have installed an owner-scoped block")
	}
	otherBlocked, err := store.IsBlocked(t.Context(), h.ownerB, imageX)
	require.NoError(t, err)
	require.False(t, otherBlocked, "X must remain fetchable from another owner")
	h.pds.mu.Lock()
	h.pds.known[mediaBlobKey{h.ownerB, imageX}] = true
	h.pds.mu.Unlock()

	requestImage := func(t *testing.T, did, blobCID string, conditional bool) (int, http.Header, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			h.proxy.URL+"/img/"+preset+"/plain/"+did+"/"+blobCID, nil)
		require.NoError(t, err)
		if strings.Contains(did, "%") {
			require.NotEmpty(t, request.URL.RawPath, "escaped owner must reach chi as a raw path")
		}
		if conditional {
			request.Header.Set("If-None-Match", `"`+preset+`-`+blobCID+`"`)
		}
		response, err := h.proxy.Client().Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, response.Header, body
	}
	assertRefused := func(t *testing.T, did, blobCID, stage string, seeded []byte, conditional bool) {
		t.Helper()
		blobRequestsBefore := h.pds.totalBlobRequests()
		status, headers, body := requestImage(t, did, blobCID, conditional)
		assert.Equal(t, http.StatusBadRequest, status, "%s: %s must be refused as an invalid DID", stage, did)
		assert.Equal(t, blobRequestsBefore, h.pds.totalBlobRequests(), "%s: %s must not reach the PDS", stage, did)
		assert.Equal(t, "no-store", headers.Get("Cache-Control"), "%s: rejected DID must not be cached", stage)
		assert.NotContains(t, headers.Get("Content-Type"), "image/", "%s: rejected DID cannot serve image bytes", stage)
		assert.False(t, bytes.Equal(seeded, body), "%s: rejected DID must not serve cached image bytes", stage)
	}

	seededImage := testkit.TestPNG(32, 32)
	for _, spelling := range []struct {
		name, did, blockedDID, blobCID string
	}{
		{name: "uppercase plc identifier", did: strings.Replace(h.ownerA, ":test", ":Test", 1), blockedDID: h.ownerA, blobCID: imageX},
		{name: "uppercase web host", did: "did:web:Mediaowner.test", blockedDID: webOwner, blobCID: imageY},
		{name: "percent-encoded web host letter", did: "did:web:%6dediaowner.test", blockedDID: webOwner, blobCID: imageY},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			require.NotEqual(t, spelling.blockedDID, spelling.did, "the case must request a different spelling, not the blocked DID itself")
			assertRefused(t, spelling.did, spelling.blobCID, "cold", seededImage, false)
			require.NoError(t, h.cache.Set(preset, spelling.did, spelling.blobCID, seededImage))
			cached, found, err := h.cache.Get(preset, spelling.did, spelling.blobCID)
			require.NoError(t, err)
			require.True(t, found, "noncanonical spelling must actually be warm on disk")
			require.Equal(t, seededImage, cached)
			assertRefused(t, spelling.did, spelling.blobCID, "warm", seededImage, false)
			assertRefused(t, spelling.did, spelling.blobCID, "matching If-None-Match", seededImage, true)
		})
	}

	t.Run("distinct web spellings colliding in disk cache", func(t *testing.T) {
		first := "did:web:example.test:a_b"
		second := "did:web:example.test:a:b"
		require.Equal(t, h.cachePath(preset, first, imageY), h.cachePath(preset, second, imageY),
			"the two spellings must actually share a disk-cache directory")
		require.NoError(t, h.cache.Set(preset, first, imageY, seededImage))
		cached, found, err := h.cache.Get(preset, second, imageY)
		require.NoError(t, err)
		require.True(t, found, "the second spelling must be able to see the seeded entry without DID validation")
		require.Equal(t, seededImage, cached)
		assertRefused(t, second, imageY, "cache-key collision", seededImage, false)
	})

	assert.Zero(t, h.pds.totalBlobRequests(), "refused owner spellings must never cause a blob request, even on a cold cache miss")
	status, headers, body := requestImage(t, h.ownerB, imageX, false)
	assert.Equal(t, http.StatusOK, status, "a different owner must still serve the same CID")
	assert.Contains(t, headers.Get("Content-Type"), "image/")
	assert.NotEmpty(t, body, "the positive control must serve image bytes")
	assert.Equal(t, 1, h.pds.totalBlobRequests(), "only the positive control may reach the fake PDS")
}
