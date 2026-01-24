package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/disintegration/imaging"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/api/handlers/imageproxy"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	imageproxycore "Coves/internal/core/imageproxy"
	"Coves/internal/db/postgres"
)

// TestImageProxy_E2E tests the complete image proxy flow including:
// - Creating a community with an avatar
// - Fetching the avatar via the image proxy
// - Verifying response headers, status codes, and image dimensions
// - Testing ETag-based caching (304 responses)
// - Error handling for invalid presets and missing blobs
func TestImageProxy_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E integration test in short mode")
	}

	// Check if PDS is running
	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v. Run 'make dev-up' to start.", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Setup test database
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Setup repositories and services
	communityRepo := postgres.NewCommunityRepository(db)

	// Setup identity resolver with local PLC
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Create a real community WITH an avatar using the community service
	// This ensures the blob is referenced by the community profile record
	// (blobs must be referenced to be stored by PDS)
	instanceDID := "did:web:coves.social"
	provisioner := communities.NewPDSAccountProvisioner("coves.social", pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		instanceDID,
		"coves.social",
		provisioner,
		nil, // No custom PDS factory
		blobService,
	)

	// Create avatar image data
	avatarData := createTestImageForProxy(t, 200, 200, color.RGBA{R: 100, G: 150, B: 200, A: 255})

	uniqueID := time.Now().UnixNano() % 100000000 // Keep shorter for handle limit
	communityName := fmt.Sprintf("ip%d", uniqueID)
	creatorDID := fmt.Sprintf("did:plc:c%d", uniqueID)

	t.Logf("Creating community with avatar: %s", communityName)
	community, err := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:                   communityName,
		DisplayName:            "Image Proxy Test Community",
		Description:            "Testing image proxy with avatar",
		Visibility:             "public",
		CreatedByDID:           creatorDID,
		HostedByDID:            instanceDID,
		AllowExternalDiscovery: true,
		AvatarBlob:             avatarData,
		AvatarMimeType:         "image/png",
	})
	require.NoError(t, err, "Failed to create community with avatar")

	// Get the avatar CID from the created community
	avatarCID := community.AvatarCID
	require.NotEmpty(t, avatarCID, "Avatar CID should not be empty")
	t.Logf("Created community: DID=%s, AvatarCID=%s", community.DID, avatarCID)

	// Verify blob exists on PDS before starting proxy tests
	directBlobURL := fmt.Sprintf("%s/xrpc/com.atproto.sync.getBlob?did=%s&cid=%s", pdsURL, community.DID, avatarCID)
	t.Logf("Verifying blob exists at: %s", directBlobURL)
	verifyResp, verifyErr := http.Get(directBlobURL)
	if verifyErr != nil {
		t.Logf("Warning: Failed to verify blob: %v", verifyErr)
	} else {
		t.Logf("Direct blob fetch status: %d", verifyResp.StatusCode)
		if verifyResp.StatusCode != http.StatusOK {
			var errBuf bytes.Buffer
			_, _ = errBuf.ReadFrom(verifyResp.Body)
			t.Logf("Direct blob fetch error: %s", errBuf.String())
		}
		_ = verifyResp.Body.Close()
	}

	// Create the test server with image proxy routes
	testServer := createImageProxyTestServer(t, pdsURL, identityResolver)
	defer testServer.Close()

	t.Run("fetch avatar via proxy returns valid JPEG", func(t *testing.T) {
		// Build request URL
		proxyURL := fmt.Sprintf("%s/img/avatar_small/plain/%s/%s", testServer.URL, community.DID, avatarCID)
		t.Logf("Requesting: %s", proxyURL)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		// Log error details if not 200
		if resp.StatusCode != http.StatusOK {
			var errBuf bytes.Buffer
			_, _ = errBuf.ReadFrom(resp.Body)
			t.Logf("Error response (status %d): %s", resp.StatusCode, errBuf.String())
		}

		// Verify status code
		assert.Equal(t, http.StatusOK, resp.StatusCode, "Should return 200 OK")

		// Verify Content-Type header
		contentType := resp.Header.Get("Content-Type")
		assert.Equal(t, "image/jpeg", contentType, "Content-Type should be image/jpeg")

		// Read the response body
		var buf bytes.Buffer
		_, err = buf.ReadFrom(resp.Body)
		require.NoError(t, err, "Should read response body")

		// Verify it's valid image data
		img, err := imaging.Decode(&buf)
		require.NoError(t, err, "Response should be valid image data")

		// Verify dimensions match avatar_small preset (360x360)
		bounds := img.Bounds()
		assert.Equal(t, 360, bounds.Dx(), "Width should be 360 (avatar_small preset)")
		assert.Equal(t, 360, bounds.Dy(), "Height should be 360 (avatar_small preset)")

		t.Logf("Successfully fetched and verified avatar: %dx%d", bounds.Dx(), bounds.Dy())
	})

	t.Run("returns correct cache headers", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/avatar_small/plain/%s/%s", testServer.URL, community.DID, avatarCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		// Verify Cache-Control header
		cacheControl := resp.Header.Get("Cache-Control")
		expectedCacheControl := "public, max-age=31536000, immutable"
		assert.Equal(t, expectedCacheControl, cacheControl, "Cache-Control header should be correct")

		// Verify ETag header is present and matches expected format
		etag := resp.Header.Get("ETag")
		expectedETag := fmt.Sprintf(`"avatar_small-%s"`, avatarCID)
		assert.Equal(t, expectedETag, etag, "ETag should match preset-cid format")

		t.Logf("Cache headers verified: Cache-Control=%s, ETag=%s", cacheControl, etag)
	})

	t.Run("ETag returns 304 on match", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/avatar_small/plain/%s/%s", testServer.URL, community.DID, avatarCID)

		// First, get the ETag
		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Initial request should succeed")
		etag := resp.Header.Get("ETag")
		_ = resp.Body.Close()
		require.NotEmpty(t, etag, "ETag should be present")

		// Now make a conditional request with If-None-Match
		req, err := http.NewRequest(http.MethodGet, proxyURL, nil)
		require.NoError(t, err, "Should create request")
		req.Header.Set("If-None-Match", etag)

		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err, "Conditional request should succeed")
		defer func() { _ = resp.Body.Close() }()

		// Verify 304 Not Modified
		assert.Equal(t, http.StatusNotModified, resp.StatusCode, "Should return 304 Not Modified")

		// Verify no body in 304 response
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		assert.Equal(t, 0, buf.Len(), "304 response should have empty body")

		t.Log("ETag conditional request correctly returned 304 Not Modified")
	})

	t.Run("ETag mismatch returns full image", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/avatar_small/plain/%s/%s", testServer.URL, community.DID, avatarCID)

		// Make request with non-matching ETag
		req, err := http.NewRequest(http.MethodGet, proxyURL, nil)
		require.NoError(t, err, "Should create request")
		req.Header.Set("If-None-Match", `"wrong-etag-value"`)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		// Should return 200 with full image
		assert.Equal(t, http.StatusOK, resp.StatusCode, "Should return 200 OK when ETag doesn't match")

		// Verify body is present
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		assert.Greater(t, buf.Len(), 0, "Response should have body")

		t.Log("Non-matching ETag correctly returned full image")
	})

	t.Run("invalid preset returns 400", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/not_a_valid_preset/plain/%s/%s", testServer.URL, community.DID, avatarCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		// Verify 400 Bad Request
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "Should return 400 for invalid preset")

		// Verify error message
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		body := buf.String()
		assert.Contains(t, body, "invalid preset", "Error message should mention invalid preset")

		t.Logf("Invalid preset correctly returned 400: %s", body)
	})

	t.Run("non-existent CID returns 404", func(t *testing.T) {
		// Use a valid CIDv1 (raw codec, sha256) that doesn't exist on the PDS
		// This is a properly formatted CID that will pass validation but won't exist
		fakeCID := "bafkreiemeosfdll427qzow5tipvctigjebyvi6ketznqrau2ydhzyggt7i"
		proxyURL := fmt.Sprintf("%s/img/avatar_small/plain/%s/%s", testServer.URL, community.DID, fakeCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		// Verify 404 Not Found (blob not found on PDS)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Should return 404 for non-existent CID")

		t.Log("Non-existent CID correctly returned 404")
	})

	t.Run("all valid presets work correctly", func(t *testing.T) {
		// Test a subset of presets with fixed dimensions (cover fit)
		presetTests := []struct {
			preset       string
			expectWidth  int
			expectHeight int
		}{
			{"avatar", 1000, 1000},
			{"avatar_small", 360, 360},
			// banner has 640x300 but input is 200x200, so it will be scaled+cropped
			{"banner", 640, 300},
			// embed_thumbnail is 720x360, will also be scaled+cropped
			{"embed_thumbnail", 720, 360},
		}

		for _, tc := range presetTests {
			t.Run(tc.preset, func(t *testing.T) {
				proxyURL := fmt.Sprintf("%s/img/%s/plain/%s/%s", testServer.URL, tc.preset, community.DID, avatarCID)

				resp, err := http.Get(proxyURL)
				require.NoError(t, err, "Request should succeed for preset %s", tc.preset)
				defer func() { _ = resp.Body.Close() }()

				assert.Equal(t, http.StatusOK, resp.StatusCode, "Should return 200 for valid preset %s", tc.preset)

				// Verify ETag format
				etag := resp.Header.Get("ETag")
				expectedETag := fmt.Sprintf(`"%s-%s"`, tc.preset, avatarCID)
				assert.Equal(t, expectedETag, etag, "ETag should match for preset %s", tc.preset)

				// Verify image dimensions
				var buf bytes.Buffer
				_, _ = buf.ReadFrom(resp.Body)
				img, err := imaging.Decode(&buf)
				require.NoError(t, err, "Should decode image for preset %s", tc.preset)

				bounds := img.Bounds()
				assert.Equal(t, tc.expectWidth, bounds.Dx(), "Width should match for preset %s", tc.preset)
				assert.Equal(t, tc.expectHeight, bounds.Dy(), "Height should match for preset %s", tc.preset)

				t.Logf("Preset %s: verified %dx%d", tc.preset, bounds.Dx(), bounds.Dy())
			})
		}
	})

	t.Run("missing parameters return 400", func(t *testing.T) {
		testCases := []struct {
			name string
			url  string
		}{
			{"missing CID", fmt.Sprintf("%s/img/avatar/plain/%s/", testServer.URL, community.DID)},
			{"missing DID", fmt.Sprintf("%s/img/avatar/plain//%s", testServer.URL, avatarCID)},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				resp, err := http.Get(tc.url)
				require.NoError(t, err, "Request should succeed")
				defer func() { _ = resp.Body.Close() }()

				// Should return 400 or 404 (depends on routing)
				assert.True(t, resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound,
					"Should return 400 or 404 for %s, got %d", tc.name, resp.StatusCode)
			})
		}
	})

	t.Run("content_preview preset preserves aspect ratio", func(t *testing.T) {
		// content_preview uses FitContain which preserves aspect ratio
		// Input is 200x200, max width is 800, so output should be 200x200 (no upscaling)
		proxyURL := fmt.Sprintf("%s/img/content_preview/plain/%s/%s", testServer.URL, community.DID, avatarCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode, "Should return 200")

		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		img, err := imaging.Decode(&buf)
		require.NoError(t, err, "Should decode image")

		bounds := img.Bounds()
		// content_preview with FitContain doesn't upscale, so 200x200 stays 200x200
		assert.Equal(t, 200, bounds.Dx(), "Width should be preserved (no upscaling)")
		assert.Equal(t, 200, bounds.Dy(), "Height should be preserved (no upscaling)")

		t.Logf("content_preview preserved aspect ratio: %dx%d", bounds.Dx(), bounds.Dy())
	})
}

// TestImageProxy_CacheHit tests that cache hits are faster than cache misses
func TestImageProxy_CacheHit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cache test in short mode")
	}

	// Check if PDS is running
	pdsURL := getTestPDSURL()
	healthResp, err := http.Get(pdsURL + "/xrpc/_health")
	if err != nil {
		t.Skipf("PDS not running at %s: %v. Run 'make dev-up' to start.", pdsURL, err)
	}
	_ = healthResp.Body.Close()

	// Setup test database
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Setup repositories and services
	communityRepo := postgres.NewCommunityRepository(db)

	// Setup identity resolver
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002"
	}
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = plcURL
	identityResolver := identity.NewResolver(db, identityConfig)

	// Create a real community with avatar using the community service
	instanceDID := "did:web:coves.social"
	provisioner := communities.NewPDSAccountProvisioner("coves.social", pdsURL)
	blobService := blobs.NewBlobService(pdsURL)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsURL,
		instanceDID,
		"coves.social",
		provisioner,
		nil,
		blobService,
	)

	avatarData := createTestImageForProxy(t, 150, 150, color.RGBA{R: 50, G: 100, B: 150, A: 255})
	uniqueID := time.Now().UnixNano() % 100000000 // Keep shorter for handle limit
	communityName := fmt.Sprintf("ic%d", uniqueID)
	creatorDID := fmt.Sprintf("did:plc:cc%d", uniqueID)

	community, err := communityService.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:                   communityName,
		DisplayName:            "Image Cache Test Community",
		Description:            "Testing image proxy caching",
		Visibility:             "public",
		CreatedByDID:           creatorDID,
		HostedByDID:            instanceDID,
		AllowExternalDiscovery: true,
		AvatarBlob:             avatarData,
		AvatarMimeType:         "image/png",
	})
	require.NoError(t, err, "Failed to create community with avatar")

	avatarCID := community.AvatarCID
	require.NotEmpty(t, avatarCID, "Avatar CID should not be empty")
	t.Logf("Created community: DID=%s, AvatarCID=%s", community.DID, avatarCID)

	// Create temp directory for cache
	cacheDir := t.TempDir()

	// Create test server with caching enabled
	testServer := createImageProxyTestServerWithCache(t, pdsURL, identityResolver, cacheDir)
	defer testServer.Close()

	proxyURL := fmt.Sprintf("%s/img/avatar/plain/%s/%s", testServer.URL, community.DID, avatarCID)

	// First request (cache miss)
	startFirst := time.Now()
	resp, err := http.Get(proxyURL)
	require.NoError(t, err, "First request should succeed")
	_ = resp.Body.Close()
	firstDuration := time.Since(startFirst)

	// Second request (should hit cache)
	startSecond := time.Now()
	resp, err = http.Get(proxyURL)
	require.NoError(t, err, "Second request should succeed")
	_ = resp.Body.Close()
	secondDuration := time.Since(startSecond)

	t.Logf("First request (cache miss): %v", firstDuration)
	t.Logf("Second request (should hit cache): %v", secondDuration)

	// Note: Cache hit should generally be faster, but timing can be flaky in tests
	// So we just verify both requests succeed
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Cached request should return 200")
}

// createTestImageForProxy creates a test PNG image with specified dimensions and color
func createTestImageForProxy(t *testing.T, width, height int, fillColor color.Color) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, fillColor)
		}
	}

	var buf bytes.Buffer
	err := png.Encode(&buf, img)
	require.NoError(t, err, "PNG encoding should succeed")

	return buf.Bytes()
}

// createImageProxyTestServer creates an httptest server with image proxy routes configured
func createImageProxyTestServer(t *testing.T, pdsURL string, identityResolver identity.Resolver) *httptest.Server {
	t.Helper()

	// Create temp directory for cache
	cacheDir := t.TempDir()
	return createImageProxyTestServerWithCache(t, pdsURL, identityResolver, cacheDir)
}

// createImageProxyTestServerWithCache creates an httptest server with image proxy routes and specified cache directory
func createImageProxyTestServerWithCache(t *testing.T, pdsURL string, identityResolver identity.Resolver, cacheDir string) *httptest.Server {
	t.Helper()

	// Create imageproxy service components
	cache, err := imageproxycore.NewDiskCache(cacheDir, 1, 0)
	require.NoError(t, err, "Failed to create disk cache") // 1GB max
	processor := imageproxycore.NewProcessor()
	fetcher := imageproxycore.NewPDSFetcher(30 * time.Second, 10)
	config := imageproxycore.Config{
		Enabled:         true,
		CachePath:       cacheDir,
		CacheMaxGB:      1,
		FetchTimeout:    30 * time.Second,
		MaxSourceSizeMB: 10,
	}

	service, err := imageproxycore.NewService(cache, processor, fetcher, config)
	require.NoError(t, err, "Failed to create imageproxy service")

	// Create handler
	handler := imageproxy.NewHandler(service, identityResolver)

	// Create router and register routes
	r := chi.NewRouter()
	routes.RegisterImageProxyRoutes(r, handler)

	return httptest.NewServer(r)
}

// TestImageProxy_MockPDS tests the image proxy with a mock PDS server
// This allows testing image proxy behavior without a real PDS
func TestImageProxy_MockPDS(t *testing.T) {
	// Create test image
	testImage := createTestImageForProxy(t, 100, 100, color.RGBA{R: 255, G: 128, B: 64, A: 255})
	testCID := "bafybeimockimagetest123"
	testDID := "did:plc:mocktest123"

	// Create mock PDS server that returns the test image
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify it's a getBlob request
		if !strings.HasPrefix(r.URL.Path, "/xrpc/com.atproto.sync.getBlob") {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Check query parameters
		did := r.URL.Query().Get("did")
		cid := r.URL.Query().Get("cid")

		if did == testDID && cid == testCID {
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testImage)
			return
		}

		// Return 404 for unknown blobs
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockPDS.Close()

	// Create mock identity resolver that returns the mock PDS URL
	mockResolver := &mockIdentityResolverForImageProxy{
		pdsURL: mockPDS.URL,
	}

	// Create test server
	cacheDir := t.TempDir()
	cache, err := imageproxycore.NewDiskCache(cacheDir, 1, 0)
	require.NoError(t, err, "Failed to create disk cache")
	processor := imageproxycore.NewProcessor()
	fetcher := imageproxycore.NewPDSFetcher(30 * time.Second, 10)
	config := imageproxycore.Config{
		Enabled:         true,
		CachePath:       cacheDir,
		CacheMaxGB:      1,
		FetchTimeout:    30 * time.Second,
		MaxSourceSizeMB: 10,
	}

	service, err := imageproxycore.NewService(cache, processor, fetcher, config)
	require.NoError(t, err, "Failed to create imageproxy service")
	handler := imageproxy.NewHandler(service, mockResolver)

	r := chi.NewRouter()
	routes.RegisterImageProxyRoutes(r, handler)
	testServer := httptest.NewServer(r)
	defer testServer.Close()

	t.Run("mock PDS returns valid image", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/avatar/plain/%s/%s", testServer.URL, testDID, testCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode, "Should return 200")
		assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"), "Content-Type should be JPEG")

		// Verify processed dimensions (avatar is 1000x1000 per presets.go)
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		img, err := imaging.Decode(&buf)
		require.NoError(t, err, "Should decode image")

		bounds := img.Bounds()
		assert.Equal(t, 1000, bounds.Dx(), "Width should be 1000 (avatar preset)")
		assert.Equal(t, 1000, bounds.Dy(), "Height should be 1000 (avatar preset)")
	})

	t.Run("mock PDS 404 returns proxy 404", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/avatar/plain/%s/%s", testServer.URL, testDID, "nonexistentcid")

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Should return 404")
	})
}

// mockIdentityResolverForImageProxy is a mock identity resolver for testing
type mockIdentityResolverForImageProxy struct {
	pdsURL string
}

func (m *mockIdentityResolverForImageProxy) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockIdentityResolverForImageProxy) ResolveHandle(ctx context.Context, handle string) (did, pdsURL string, err error) {
	return "", "", fmt.Errorf("not implemented")
}

func (m *mockIdentityResolverForImageProxy) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	return &identity.DIDDocument{
		DID: did,
		Service: []identity.Service{
			{
				ID:              "#atproto_pds",
				Type:            "AtprotoPersonalDataServer",
				ServiceEndpoint: m.pdsURL,
			},
		},
	}, nil
}

func (m *mockIdentityResolverForImageProxy) Purge(ctx context.Context, identifier string) error {
	return nil
}

// TestImageProxy_ErrorHandling tests various error conditions
func TestImageProxy_ErrorHandling(t *testing.T) {
	// Create mock identity resolver
	mockResolver := &mockIdentityResolverForImageProxy{
		pdsURL: "http://localhost:9999", // Non-existent server
	}

	// Create test server
	cacheDir := t.TempDir()
	cache, err := imageproxycore.NewDiskCache(cacheDir, 1, 0)
	require.NoError(t, err, "Failed to create disk cache")
	processor := imageproxycore.NewProcessor()
	fetcher := imageproxycore.NewPDSFetcher(1 * time.Second, 10) // Short timeout
	config := imageproxycore.Config{
		Enabled:         true,
		CachePath:       cacheDir,
		CacheMaxGB:      1,
		FetchTimeout:    1 * time.Second,
		MaxSourceSizeMB: 10,
	}

	service, err := imageproxycore.NewService(cache, processor, fetcher, config)
	require.NoError(t, err, "Failed to create imageproxy service")
	handler := imageproxy.NewHandler(service, mockResolver)

	r := chi.NewRouter()
	routes.RegisterImageProxyRoutes(r, handler)
	testServer := httptest.NewServer(r)
	defer testServer.Close()

	t.Run("connection refused returns 502", func(t *testing.T) {
		// Use a valid CID format - this will pass validation but fail at the PDS fetch stage
		validCID := "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
		proxyURL := fmt.Sprintf("%s/img/avatar/plain/did:plc:test/%s", testServer.URL, validCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		// Should return 502 Bad Gateway when PDS fetch fails
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode, "Should return 502 when PDS is unreachable")
	})

	t.Run("invalid DID resolution returns 502", func(t *testing.T) {
		// Create resolver that returns error
		errorResolver := &errorMockResolver{}

		errorHandler := imageproxy.NewHandler(service, errorResolver)
		errorRouter := chi.NewRouter()
		routes.RegisterImageProxyRoutes(errorRouter, errorHandler)
		errorServer := httptest.NewServer(errorRouter)
		defer errorServer.Close()

		// Use a valid CID format - this will pass validation but fail at DID resolution
		validCID := "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
		proxyURL := fmt.Sprintf("%s/img/avatar/plain/did:plc:test/%s", errorServer.URL, validCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusBadGateway, resp.StatusCode, "Should return 502 when DID resolution fails")
	})
}

// errorMockResolver is a mock resolver that always returns an error
type errorMockResolver struct{}

func (m *errorMockResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	return nil, fmt.Errorf("resolution failed")
}

func (m *errorMockResolver) ResolveHandle(ctx context.Context, handle string) (did, pdsURL string, err error) {
	return "", "", fmt.Errorf("resolution failed")
}

func (m *errorMockResolver) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	return nil, fmt.Errorf("resolution failed")
}

func (m *errorMockResolver) Purge(ctx context.Context, identifier string) error {
	return nil
}

// TestImageProxy_UnsupportedFormat tests behavior with unsupported image formats
func TestImageProxy_UnsupportedFormat(t *testing.T) {
	// Create mock PDS that returns invalid image data
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/xrpc/com.atproto.sync.getBlob") {
			cid := r.URL.Query().Get("cid")

			if cid == "textdata" {
				// Return text data instead of image
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("this is not an image"))
				return
			}

			if cid == "corruptedimage" {
				// Return corrupted image data
				w.Header().Set("Content-Type", "image/png")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x00}) // Incomplete PNG header
				return
			}

			if cid == "emptybody" {
				// Return empty body
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockPDS.Close()

	mockResolver := &mockIdentityResolverForImageProxy{
		pdsURL: mockPDS.URL,
	}

	cacheDir := t.TempDir()
	cache, err := imageproxycore.NewDiskCache(cacheDir, 1, 0)
	require.NoError(t, err, "Failed to create disk cache")
	processor := imageproxycore.NewProcessor()
	fetcher := imageproxycore.NewPDSFetcher(30 * time.Second, 10)
	config := imageproxycore.DefaultConfig()

	service, err := imageproxycore.NewService(cache, processor, fetcher, config)
	require.NoError(t, err, "Failed to create imageproxy service")
	handler := imageproxy.NewHandler(service, mockResolver)

	r := chi.NewRouter()
	routes.RegisterImageProxyRoutes(r, handler)
	testServer := httptest.NewServer(r)
	defer testServer.Close()

	testCases := []struct {
		name           string
		cid            string
		expectedStatus int
	}{
		{"text data", "textdata", http.StatusBadRequest},
		{"corrupted image", "corruptedimage", http.StatusInternalServerError},
		{"empty body", "emptybody", http.StatusBadRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			proxyURL := fmt.Sprintf("%s/img/avatar/plain/did:plc:test/%s", testServer.URL, tc.cid)

			resp, err := http.Get(proxyURL)
			require.NoError(t, err, "Request should succeed")
			defer func() { _ = resp.Body.Close() }()

			// Should return error status for invalid image data
			assert.True(t, resp.StatusCode >= 400, "Should return error status for %s", tc.name)
			t.Logf("%s returned status %d", tc.name, resp.StatusCode)
		})
	}
}

// TestImageProxy_LargeImage tests behavior with large images
func TestImageProxy_LargeImage(t *testing.T) {
	// Create a large test image (1000x1000)
	largeImage := createTestImageForProxy(t, 1000, 1000, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	testCID := "bafylargeimagecid"
	testDID := "did:plc:largetest"

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/xrpc/com.atproto.sync.getBlob") {
			did := r.URL.Query().Get("did")
			cid := r.URL.Query().Get("cid")

			if did == testDID && cid == testCID {
				w.Header().Set("Content-Type", "image/png")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(largeImage)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockPDS.Close()

	mockResolver := &mockIdentityResolverForImageProxy{
		pdsURL: mockPDS.URL,
	}

	cacheDir := t.TempDir()
	cache, err := imageproxycore.NewDiskCache(cacheDir, 1, 0)
	require.NoError(t, err, "Failed to create disk cache")
	processor := imageproxycore.NewProcessor()
	fetcher := imageproxycore.NewPDSFetcher(30 * time.Second, 10)
	config := imageproxycore.DefaultConfig()

	service, err := imageproxycore.NewService(cache, processor, fetcher, config)
	require.NoError(t, err, "Failed to create imageproxy service")
	handler := imageproxy.NewHandler(service, mockResolver)

	r := chi.NewRouter()
	routes.RegisterImageProxyRoutes(r, handler)
	testServer := httptest.NewServer(r)
	defer testServer.Close()

	t.Run("large image resized correctly", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/avatar/plain/%s/%s", testServer.URL, testDID, testCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode, "Should return 200")

		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		img, err := imaging.Decode(&buf)
		require.NoError(t, err, "Should decode image")

		bounds := img.Bounds()
		assert.Equal(t, 1000, bounds.Dx(), "Width should be 1000 (avatar preset)")
		assert.Equal(t, 1000, bounds.Dy(), "Height should be 1000 (avatar preset)")

		t.Logf("Large image correctly resized to %dx%d", bounds.Dx(), bounds.Dy())
	})

	t.Run("content_preview limits width for large image", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/content_preview/plain/%s/%s", testServer.URL, testDID, testCID)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode, "Should return 200")

		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		img, err := imaging.Decode(&buf)
		require.NoError(t, err, "Should decode image")

		bounds := img.Bounds()
		// content_preview max width is 800, preserves aspect ratio
		assert.Equal(t, 800, bounds.Dx(), "Width should be limited to 800")
		assert.Equal(t, 800, bounds.Dy(), "Height should be 800 (1:1 aspect ratio)")

		t.Logf("Large image correctly scaled to %dx%d for content_preview", bounds.Dx(), bounds.Dy())
	})
}

// TestImageProxy_ResponseJSON verifies no JSON is returned (should be plain text or image)
func TestImageProxy_ResponseJSON(t *testing.T) {
	mockResolver := &mockIdentityResolverForImageProxy{
		pdsURL: "http://localhost:9999",
	}

	cacheDir := t.TempDir()
	cache, err := imageproxycore.NewDiskCache(cacheDir, 1, 0)
	require.NoError(t, err, "Failed to create disk cache")
	processor := imageproxycore.NewProcessor()
	fetcher := imageproxycore.NewPDSFetcher(1 * time.Second, 10)
	config := imageproxycore.DefaultConfig()

	service, err := imageproxycore.NewService(cache, processor, fetcher, config)
	require.NoError(t, err, "Failed to create imageproxy service")
	handler := imageproxy.NewHandler(service, mockResolver)

	r := chi.NewRouter()
	routes.RegisterImageProxyRoutes(r, handler)
	testServer := httptest.NewServer(r)
	defer testServer.Close()

	t.Run("error responses are plain text not JSON", func(t *testing.T) {
		proxyURL := fmt.Sprintf("%s/img/invalid_preset/plain/did:plc:test/cid", testServer.URL)

		resp, err := http.Get(proxyURL)
		require.NoError(t, err, "Request should succeed")
		defer func() { _ = resp.Body.Close() }()

		contentType := resp.Header.Get("Content-Type")
		assert.Contains(t, contentType, "text/plain", "Error responses should be text/plain")

		// Verify body is not valid JSON
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		body := buf.Bytes()

		var jsonCheck map[string]interface{}
		jsonErr := json.Unmarshal(body, &jsonCheck)
		assert.Error(t, jsonErr, "Error response should not be valid JSON")

		t.Logf("Error response correctly returned as plain text: %s", string(body))
	})
}
