//go:build integration

// These are the image-proxy cases that a mock upstream cannot answer, and they
// are the reason this package's integration floor includes a real PDS.
//
// Everything in proxy_serving_test.go states the upstream's response directly:
// here are the bytes, here is the status. That proves what the proxy does with
// an answer, but it assumes the answer's shape — that a PDS really does serve
// com.atproto.sync.getBlob at that path, with those query parameters, for a
// blob uploaded as part of a profile record, and that the community's DID
// really does resolve to that PDS through the directory. Every one of those
// assumptions is a place the proxy could silently stop working against real
// infrastructure while every mock-backed test stayed green.
//
// So this file takes the long way round: provision a community account on the
// PDS with an avatar, let the PDS assign the blob its CID, and then ask the
// proxy for that CID by the community's DID and check the pixels that come
// back. The blob has to be REFERENCED by the community's profile record for the
// PDS to keep it — an unreferenced upload is garbage-collected — which is why
// the community is created through communities.Service rather than by writing a
// row.
//
// The file is in the external test package because it imports
// internal/api/routes and internal/db/postgres, both of which pull in this
// handler package or the domain; in-package that would be an import cycle.
package imageproxy_test

import (
	"context"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provisionedAvatar is a community that exists on the real PDS, together with
// the CID the PDS assigned its avatar blob.
type provisionedAvatar struct {
	communityDID string
	avatarCID    string
	resolver     identity.Resolver
}

// provisionCommunityAvatar creates a community account on the test PDS with an
// avatar of the given size, and returns what the proxy needs to fetch it back.
//
// The identity resolver is the real one, backed by the test stack's PLC
// directory, because DID resolution is half of what this file is proving: the
// proxy is given nothing but a DID and has to find the PDS itself.
func provisionCommunityAvatar(t *testing.T, width, height int, fill color.Color) provisionedAvatar {
	t.Helper()

	db := testkit.DB(t)
	endpoints := testkit.Endpoints()

	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = endpoints.PLC.BaseURL
	resolver := identity.NewResolver(db, identityConfig)

	// The handle domain comes from the stack rather than a literal: the PDS
	// rejects a handle outside its configured service domains, and the
	// provisioner builds the community's handle as c-{name}.{domain}.
	handleDomain := endpoints.PDS.HandleDomain
	communityService := communities.NewCommunityServiceWithPDSFactory(
		postgres.NewCommunityRepository(db),
		endpoints.PDS.BaseURL,
		fixtures.InstanceDID(),
		handleDomain,
		communities.NewPDSAccountProvisioner(handleDomain, endpoints.PDS.BaseURL),
		nil, // no PDS client factory: provisioning uses the password session
		blobs.NewBlobService(endpoints.PDS.BaseURL),
	)

	// The provisioner prefixes "c-", and a PDS handle's local label is capped
	// at 18 characters, so the name has to stay short — testkit.UniqueID is
	// built to that budget.
	name := "ip" + testkit.UniqueID(t)
	community, err := communityService.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "Image proxy avatar community",
		Description:            "Community whose avatar the image proxy serves",
		Visibility:             "public",
		CreatedByDID:           fixtures.DID("creator" + name),
		HostedByDID:            fixtures.InstanceDID(),
		AllowExternalDiscovery: true,
		AvatarBlob:             testkit.TestPNGColor(width, height, fill),
		AvatarMimeType:         "image/png",
	})
	require.NoError(t, err, "provisioning a community with an avatar on the PDS")
	require.NotEmpty(t, community.AvatarCID, "the PDS must have assigned the avatar blob a CID")

	return provisionedAvatar{
		communityDID: community.DID,
		avatarCID:    community.AvatarCID,
		resolver:     resolver,
	}
}

// TestImageProxy_ServesRealPDSAvatar fetches an avatar the test PDS actually
// holds, through DID resolution, and checks both the image and the caching
// contract the CDN in front of this endpoint depends on.
func TestImageProxy_ServesRealPDSAvatar(t *testing.T) {
	t.Parallel()

	avatar := provisionCommunityAvatar(t, 200, 200, color.RGBA{R: 100, G: 150, B: 200, A: 255})
	server := newProxyServer(t, avatar.resolver, defaultFetchTimeout)
	url := proxyURL(server, "avatar_small", avatar.communityDID, avatar.avatarCID)

	t.Run("the avatar comes back re-encoded at the preset size", func(t *testing.T) {
		resp, body := fetch(t, url, nil)

		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
		assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
		assertImageSize(t, body, 360, 360)
	})

	t.Run("the response is immutably cacheable", func(t *testing.T) {
		resp, _ := fetch(t, url, nil)

		// A preset plus a content-addressed CID names bytes that can never
		// change, so the response is safe to cache forever — that is the whole
		// economic argument for the proxy, and a weakened header here would
		// quietly send every view back to the PDS.
		assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"))
		assert.Equal(t, fmt.Sprintf(`"avatar_small-%s"`, avatar.avatarCID), resp.Header.Get("ETag"))
	})

	t.Run("a matching ETag is answered with 304 and no body", func(t *testing.T) {
		resp, _ := fetch(t, url, nil)
		etag := resp.Header.Get("ETag")
		require.NotEmpty(t, etag, "the first response must carry an ETag to revalidate against")

		conditional, body := fetch(t, url, http.Header{"If-None-Match": []string{etag}})

		assert.Equal(t, http.StatusNotModified, conditional.StatusCode)
		assert.Empty(t, body, "a 304 must not carry a body")
	})

	t.Run("a stale ETag is answered with the full image", func(t *testing.T) {
		resp, body := fetch(t, url, http.Header{"If-None-Match": []string{`"wrong-etag-value"`}})

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotEmpty(t, body, "a revalidation miss must return the image")
	})

	t.Run("a CID the community's PDS does not hold is a 404", func(t *testing.T) {
		// A well-formed CIDv1 (raw codec, sha256) that passes validation and
		// then fails to exist, so the 404 comes from the PDS rather than from
		// the parser.
		absent := "bafkreiemeosfdll427qzow5tipvctigjebyvi6ketznqrau2ydhzyggt7i"

		resp, _ := fetch(t, proxyURL(server, "avatar_small", avatar.communityDID, absent), nil)

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// TestImageProxy_SecondFetchIsServedFromCache checks that a repeated request
// for the same preset and CID is answered from the on-disk cache instead of
// going back to the PDS.
//
// It asserts on the CACHE, not on the clock. The obvious version of this test
// times both requests and expects the second to be faster, which is a coin flip
// under CI load and says nothing useful when it fails. Cutting the upstream off
// after the first request states the property directly: if a later response
// still arrives intact, it cannot have come from the PDS.
//
// The retry loop is not defensive padding. ImageProxyService.GetImage writes to
// the cache in a goroutine so the response is not held up by disk IO, which
// means the entry is not guaranteed to exist the instant the first response
// lands. WaitFor is how the suite expresses "eventually, and say so if not"
// without a sleep.
func TestImageProxy_SecondFetchIsServedFromCache(t *testing.T) {
	t.Parallel()

	avatar := provisionCommunityAvatar(t, 150, 150, color.RGBA{R: 50, G: 100, B: 150, A: 255})

	// A resolver that can be pointed at a dead address, so later requests have
	// no working upstream to fall back on.
	resolver := &switchableResolver{delegate: avatar.resolver}
	server := newProxyServer(t, resolver, defaultFetchTimeout)
	url := proxyURL(server, "avatar", avatar.communityDID, avatar.avatarCID)

	first, firstBody := fetch(t, url, nil)
	require.Equal(t, http.StatusOK, first.StatusCode, "the cache-filling request must succeed: %s", firstBody)
	assertImageSize(t, firstBody, 1000, 1000)

	resolver.redirectTo(unreachableURL(t))

	var cachedStatus int
	var cachedBody []byte
	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			return false, fmt.Errorf("building the request for the cached image: %w", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, fmt.Errorf("requesting the cached image: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, fmt.Errorf("reading the cached image: %w", err)
		}
		cachedStatus, cachedBody = resp.StatusCode, body
		// 502 means the cache write has not landed yet and the request fell
		// through to the upstream that is now dead — the one status worth
		// retrying. Anything else is the answer, right or wrong.
		return resp.StatusCode != http.StatusBadGateway, nil
	}, testkit.WithDescription("the processed avatar to be served from the on-disk cache"))

	assert.Equal(t, http.StatusOK, cachedStatus,
		"a cached request must succeed with the PDS unreachable")
	assert.Equal(t, firstBody, cachedBody, "a cache hit must return the same bytes as the miss did")
}

// switchableResolver resolves through a real resolver until it is redirected,
// after which every DID resolves to one fixed host.
type switchableResolver struct {
	delegate identity.Resolver
	override string
}

// redirectTo makes every subsequent resolution point at pdsURL.
//
// There is no lock because the redirect happens between two sequential
// requests in one goroutine, with no request in flight.
func (r *switchableResolver) redirectTo(pdsURL string) { r.override = pdsURL }

func (r *switchableResolver) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	if r.override == "" {
		return r.delegate.ResolveDID(ctx, did)
	}
	return (&fixedPDSResolver{pdsURL: r.override}).ResolveDID(ctx, did)
}

func (r *switchableResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	return r.delegate.Resolve(ctx, identifier)
}

func (r *switchableResolver) ResolveHandle(ctx context.Context, handle string) (did, pdsURL string, err error) {
	return r.delegate.ResolveHandle(ctx, handle)
}

func (r *switchableResolver) Purge(ctx context.Context, identifier string) error {
	return r.delegate.Purge(ctx, identifier)
}
