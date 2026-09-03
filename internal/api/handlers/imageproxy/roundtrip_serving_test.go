//go:build integration

// This file closes the seam between the two halves of the media pipeline, which
// are otherwise only ever tested apart.
//
// One half GENERATES URLs (internal/core/embeds and communities, via
// blobs.HydrateImageURL) and is covered by string comparison. The other half
// SERVES them (/img/{preset}/plain/{did}/{cid}) and is covered — in
// proxy_serving_test.go and avatar_serving_test.go — against paths the tests
// build by hand. Nothing else checks that the first half's output is something
// the second half accepts, so a renamed preset, a changed path shape, or a DID
// that needs escaping would leave both suites green and break every image in
// production.
//
// Here the test takes the URL the AppView actually put in a view — by pointing
// the process-wide image-URL config at the running proxy and letting the real
// projection code emit the URL — and fetches it, against a blob a real PDS
// holds. It is a T1 integration test, not an e2e contract: it exercises the
// generation-plus-serving seam in-process and never touches the firehose or a
// running AppView container.
//
// The file is in the external test package because it imports
// internal/db/postgres and internal/core/communities, which pull in this
// handler package or the domain; in-package that would be an import cycle.
package imageproxy_test

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"image/color"
	"net/http"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageProxy_EmittedURLsAreFetchable provisions a community with a real
// avatar blob on the test PDS, points the image-URL config at a running proxy,
// and then fetches the URLs the projection code emits for every embed shape
// that carries an image — plus the community avatar, which shares the generator
// but a different preset and call sites.
//
// It does not call t.Parallel and it must not: it mutates the process-wide
// blobs image-URL config, which the projection code reads, and a parallel
// sibling touching the same global would race.
func TestImageProxy_EmittedURLsAreFetchable(t *testing.T) {
	db := testkit.DB(t)
	endpoints := testkit.Endpoints()

	// The hatch: the stack's PLC listens on loopback, which the resolver's SSRF
	// guard refuses by default. Per-construction, not ambient — see the note in
	// avatar_serving_test.go, and the guard's own tests in
	// internal/atproto/identity/resolver_guard_test.go.
	identityConfig := identity.DefaultConfig(identity.WithPrivateHostsAllowed())
	identityConfig.PLCURL = endpoints.PLC.BaseURL
	// The proxy resolves the community's DID through the stack's PLC directory to
	// find the blob, so a fabricated DID would never get past resolution.
	resolver := identity.NewResolver(db, identityConfig)

	// A real community on the real test PDS. The blob has to be REFERENCED by the
	// community's profile record for the PDS to keep it — an unreferenced upload
	// is garbage-collected — which is why it goes through communities.Service
	// rather than a row insert.
	handleDomain := endpoints.PDS.HandleDomain
	communityService := communities.NewCommunityServiceWithPDSFactory(
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		endpoints.PDS.BaseURL,
		fixtures.InstanceDID(),
		handleDomain,
		communities.NewPDSAccountProvisioner(handleDomain, endpoints.PDS.BaseURL, communities.PrivateHostOptions(true)...),
		nil, // no PDS client factory: provisioning uses the password session
		blobs.NewBlobService(endpoints.PDS.BaseURL, blobs.PrivateHostOptions(true)...),
		communities.PrivateHostOptions(true)...,
	)

	// The provisioner prefixes "c-", and a PDS handle's local label is capped at
	// 18 characters, so the name stays short — testkit.UniqueID is built to that
	// budget. A 400x300 source sits below the content presets' width caps and is
	// large enough to cover the avatar preset.
	name := "rt" + testkit.UniqueID(t)
	imageData := testkit.TestPNGColor(400, 300, color.RGBA{R: 20, G: 180, B: 90, A: 255})
	community, err := communityService.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "Round Trip Test Community",
		Description:            "Verifies emitted image URLs resolve",
		Visibility:             "public",
		CreatedByDID:           fixtures.DID("creator" + name),
		HostedByDID:            fixtures.InstanceDID(),
		AllowExternalDiscovery: true,
		AvatarBlob:             imageData,
		AvatarMimeType:         "image/png",
	})
	require.NoError(t, err, "provisioning a community with an avatar on the PDS")
	require.NotEmpty(t, community.AvatarCID, "the PDS must have assigned the avatar blob a CID")

	// The real route, the real proxy service, the real PDS behind it.
	server := newProxyServer(t, resolver, defaultFetchTimeout)

	// Point URL generation at that server, exactly as production points it at the
	// media hostname. Everything below flows from this one setting.
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{
		ProxyEnabled: true,
		ProxyBaseURL: server.URL,
	})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)

	blobRef := func(cid string) map[string]interface{} {
		return map[string]interface{}{
			"$type":    "blob",
			"ref":      map[string]interface{}{"$link": cid},
			"mimeType": "image/png",
			"size":     len(imageData),
		}
	}

	t.Run("image embed URLs resolve", func(t *testing.T) {
		postView := &posts.PostView{
			URI:       "at://" + community.DID + "/social.coves.community.post/roundtrip",
			Community: &posts.CommunityRef{DID: community.DID, PDSURL: community.PDSURL},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.images",
				"images": []interface{}{
					map[string]interface{}{"image": blobRef(community.AvatarCID), "alt": "round trip"},
				},
			},
		}

		posts.TransformBlobRefsToURLs(postView)

		embed := postView.Embed.(map[string]interface{})
		require.Equal(t, "social.coves.embed.images#view", embed["$type"],
			"the embed must have been projected, or this test proves nothing")

		img := embed["images"].([]interface{})[0].(map[string]interface{})
		thumb, ok := img["thumb"].(string)
		require.True(t, ok, "thumb should be a URL string")
		fullsize, ok := img["fullsize"].(string)
		require.True(t, ok, "fullsize should be a URL string")

		_, thumbBody := fetch(t, thumb, nil)
		_, fullBody := fetch(t, fullsize, nil)

		// content_preview caps width at 800, content_full at 1600, both
		// preserving aspect ratio. The source is 400x300, under both caps, so
		// neither is upscaled — both decode at the source dimensions.
		assertImageSize(t, thumbBody, 400, 300)
		assertImageSize(t, fullBody, 400, 300)
	})

	t.Run("external embed thumbnail URL resolves", func(t *testing.T) {
		postView := &posts.PostView{
			URI:       "at://" + community.DID + "/social.coves.community.post/roundtrip2",
			Community: &posts.CommunityRef{DID: community.DID, PDSURL: community.PDSURL},
			Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":   "https://example.com/article",
					"thumb": blobRef(community.AvatarCID),
				},
			},
		}

		posts.TransformBlobRefsToURLs(postView)

		external := postView.Embed.(map[string]interface{})["external"].(map[string]interface{})
		thumb, ok := external["thumb"].(string)
		require.True(t, ok, "thumb should be a URL string")

		resp, body := fetch(t, thumb, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
		// embed_thumbnail is 720x360 cover, which crops rather than fitting, so
		// the output is exactly the preset size regardless of source aspect.
		assertImageSize(t, body, 720, 360)
	})

	t.Run("community avatar URL resolves", func(t *testing.T) {
		view := community.ToCommunityViewDetailed()
		require.NotEmpty(t, view.Avatar, "detailed community view should carry an avatar URL")

		resp, body := fetch(t, view.Avatar, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
		// ToCommunityViewDetailed renders the "avatar" preset (1000x1000 cover),
		// so a 400x300 source is upscaled and cropped to the exact preset size.
		assertImageSize(t, body, 1000, 1000)
	})

	// Errors must be uncacheable: this route advertises a one-year immutable
	// lifetime on success and sits behind a CDN, so a cacheable failure would
	// outlive the condition that caused it by a year.
	t.Run("an unresolvable blob returns an uncacheable error", func(t *testing.T) {
		missing := blobs.HydrateImageURL(blobs.GetImageURLConfig(),
			community.PDSURL, community.DID,
			"bafkreiabcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopq", "content_preview")
		require.NotEmpty(t, missing)

		resp, _ := fetch(t, missing, nil)

		assert.GreaterOrEqual(t, resp.StatusCode, 400, "a missing blob must not return 200")
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"),
			"a CDN must never cache this failure")
	})
}
