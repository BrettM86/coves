//go:build integration

// The image proxy is one handler sitting on top of four collaborators — a URL
// route, a DID resolver, a PDS fetcher and an image processor — and almost
// every interesting behaviour is a property of the assembly rather than of any
// one part. The in-package unit tests in handler_test.go replace the whole
// imageproxy service with a fake and prove the handler maps a service error to
// a status code. These tests keep the real service, the real disk cache, the
// real processor and the real fetcher, and replace only the far side of the
// network: a PDS that is an httptest server rather than a PDS.
//
// That is what lets them assert the things the unit tests cannot — that the
// bytes coming out are a decodable JPEG of the preset's exact dimensions, that
// a preset which preserves aspect ratio really does, that an upstream returning
// HTML or a truncated PNG turns into a 4xx/5xx rather than a corrupt image, and
// that a proxy error is served as plain text so a browser's <img> tag does not
// end up rendering a JSON blob.
//
// The mock PDS is not a compromise here: what these cases care about is what
// the proxy does with the bytes and the status it gets back, and an httptest
// server states those inputs directly instead of contriving them on a real
// server. The one thing it cannot prove — that a real PDS answers
// com.atproto.sync.getBlob the way the fetcher expects — is what
// avatar_serving_test.go is for.
//
// The file is in the external test package because it imports
// internal/api/routes, which imports this handler package; in-package that
// would be an import cycle.
package imageproxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/handlers/imageproxy"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	imageproxycore "Coves/internal/core/imageproxy"
	"Coves/tests/testkit"

	"github.com/disintegration/imaging"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaultFetchTimeout is what the proxy is given for a fetch it is expected to
// complete. It is generous on purpose: these tests are not measuring latency,
// and a tight budget here would turn a loaded CI machine into a 502.
const defaultFetchTimeout = 30 * time.Second

// fixedPDSResolver is an identity.Resolver that sends every DID to one PDS.
//
// Only ResolveDID is implemented because that is the only method the proxy
// calls: it turns the DID in the URL into the host it should fetch the blob
// from. The rest return errors rather than zero values so that a handler which
// started calling one fails loudly here instead of quietly resolving to "".
type fixedPDSResolver struct {
	pdsURL string
}

func (r *fixedPDSResolver) ResolveDID(_ context.Context, did string) (*identity.DIDDocument, error) {
	return &identity.DIDDocument{
		DID: did,
		Service: []identity.Service{{
			ID:              "#atproto_pds",
			Type:            "AtprotoPersonalDataServer",
			ServiceEndpoint: r.pdsURL,
		}},
	}, nil
}

func (r *fixedPDSResolver) Resolve(context.Context, string) (*identity.Identity, error) {
	return nil, fmt.Errorf("fixedPDSResolver: Resolve is not part of the image-proxy path")
}

func (r *fixedPDSResolver) ResolveHandle(context.Context, string) (did, pdsURL string, err error) {
	return "", "", fmt.Errorf("fixedPDSResolver: ResolveHandle is not part of the image-proxy path")
}

func (r *fixedPDSResolver) Purge(context.Context, string) error { return nil }

// failingResolver is an identity.Resolver that cannot resolve anything, for the
// case where the proxy is asked about a DID the directory does not know.
type failingResolver struct{}

func (failingResolver) Resolve(context.Context, string) (*identity.Identity, error) {
	return nil, fmt.Errorf("resolution failed")
}

func (failingResolver) ResolveHandle(context.Context, string) (did, pdsURL string, err error) {
	return "", "", fmt.Errorf("resolution failed")
}

func (failingResolver) ResolveDID(context.Context, string) (*identity.DIDDocument, error) {
	return nil, fmt.Errorf("resolution failed")
}

func (failingResolver) Purge(context.Context, string) error { return nil }

// newProxyServer stands the routed image proxy up over a real service — real
// disk cache in a temp directory, real processor, real fetcher — pointed at
// whatever resolver it is given.
//
// It goes through routes.RegisterImageProxyRoutes rather than calling the
// handler directly because the URL shape (/img/{preset}/plain/{did}/{cid}) is
// part of the contract: a route pattern that stopped capturing the CID would
// leave every handler unit test passing.
func newProxyServer(t *testing.T, resolver identity.Resolver, fetchTimeout time.Duration) *httptest.Server {
	t.Helper()

	cacheDir := t.TempDir()
	cache, err := imageproxycore.NewDiskCache(cacheDir, 1, 0)
	require.NoError(t, err, "creating the disk cache")

	service, err := imageproxycore.NewService(
		cache,
		imageproxycore.NewProcessor(),
		// The hatch, for the same reason the T0 fixtures in
		// core/imageproxy/fetcher_test.go carry it: every PDS these tests point
		// at is an httptest server or the CI stack's own PDS, and both listen on
		// loopback — which is precisely what the fetcher's SSRF guard refuses.
		// Nothing here is a test OF the guard; that lives in
		// core/imageproxy/fetcher_guard_test.go, whose fetchers are built
		// without this option and assert the listener is never reached.
		imageproxycore.NewPDSFetcher(fetchTimeout, 10, imageproxycore.WithPrivateHostsAllowed()),
		imageproxycore.Config{
			Enabled:         true,
			CachePath:       cacheDir,
			CacheMaxGB:      1,
			FetchTimeout:    fetchTimeout,
			MaxSourceSizeMB: 10,
		},
	)
	require.NoError(t, err, "creating the imageproxy service")

	router := chi.NewRouter()
	routes.RegisterImageProxyRoutes(router, imageproxy.NewHandler(service, resolver))

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

// newBlobServer runs an httptest server that answers com.atproto.sync.getBlob
// from a map of CID to response, and 404s everything else — which is what a PDS
// does for a blob it does not hold.
func newBlobServer(t *testing.T, blobs map[string]func(http.ResponseWriter)) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/xrpc/com.atproto.sync.getBlob") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if write, known := blobs[r.URL.Query().Get("cid")]; known {
			write(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server
}

// servePNG answers a getBlob request with the given PNG bytes.
func servePNG(data []byte) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// unreachableURL returns the address of a server that has already been shut
// down, so a connection to it is refused immediately.
//
// This is how the "upstream is down" cases get a dead address without writing a
// port number into the test: a literal like localhost:9999 is a guess that
// something else on the machine might be listening on, and the suite forbids
// endpoint literals for exactly that reason.
func unreachableURL(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	return url
}

// proxyURL builds a request URL for the routed proxy.
func proxyURL(server *httptest.Server, preset, did, cid string) string {
	return fmt.Sprintf("%s/img/%s/plain/%s/%s", server.URL, preset, did, cid)
}

// fetch issues a GET and hands back the response with its body already read, so
// callers never have to remember to close it.
func fetch(t *testing.T, url string, header http.Header) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err, "building the request")
	for name, values := range header {
		req.Header[name] = values
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "GET %s", url)
	defer func() { _ = resp.Body.Close() }()

	var body bytes.Buffer
	_, err = body.ReadFrom(resp.Body)
	require.NoError(t, err, "reading the response body")
	return resp, body.Bytes()
}

// assertImageSize decodes body and asserts its dimensions.
func assertImageSize(t *testing.T, body []byte, wantWidth, wantHeight int) {
	t.Helper()

	img, err := imaging.Decode(bytes.NewReader(body))
	require.NoError(t, err, "the proxy must return decodable image data")

	bounds := img.Bounds()
	assert.Equal(t, wantWidth, bounds.Dx(), "width")
	assert.Equal(t, wantHeight, bounds.Dy(), "height")
}

// TestImageProxy_ServesProcessedBlob covers the success path end to end: a PNG
// on the upstream comes back as a JPEG at the preset's dimensions, and a CID
// the upstream does not hold comes back as a 404 rather than as an empty 200.
func TestImageProxy_ServesProcessedBlob(t *testing.T) {
	t.Parallel()

	const cid = "bafybeimockimagetest123"
	did := "did:plc:" + testkit.UniqueID(t)

	upstream := newBlobServer(t, map[string]func(http.ResponseWriter){
		cid: servePNG(testkit.TestPNGColor(100, 100, color.RGBA{R: 255, G: 128, B: 64, A: 255})),
	})
	server := newProxyServer(t, &fixedPDSResolver{pdsURL: upstream.URL}, defaultFetchTimeout)

	t.Run("a stored blob is re-encoded to the preset", func(t *testing.T) {
		resp, body := fetch(t, proxyURL(server, "avatar", did, cid), nil)

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		// The proxy always re-encodes: whatever the source format was, clients
		// get one predictable format back.
		assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
		// The avatar preset upscales a 100x100 source to its full 1000x1000.
		assertImageSize(t, body, 1000, 1000)
	})

	t.Run("a blob the PDS does not hold is a 404", func(t *testing.T) {
		resp, _ := fetch(t, proxyURL(server, "avatar", did, "nonexistentcid"), nil)

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// TestImageProxy_UpstreamFailuresAreBadGateway covers the two ways the proxy can
// fail to reach the bytes: the DID does not resolve, or the PDS it resolves to
// does not answer.
//
// Both are 502 rather than 404 or 500 on purpose. The request was well formed
// and the resource may well exist; what failed is an upstream the client has no
// way to fix, and a 404 would tell caches and clients the image is gone.
func TestImageProxy_UpstreamFailuresAreBadGateway(t *testing.T) {
	t.Parallel()

	// Well-formed CIDs: these must travel past validation so that the failure
	// under test is the fetch, not the parse.
	const validCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
	did := "did:plc:" + testkit.UniqueID(t)

	t.Run("the resolved PDS refuses the connection", func(t *testing.T) {
		server := newProxyServer(t, &fixedPDSResolver{pdsURL: unreachableURL(t)}, time.Second)

		resp, _ := fetch(t, proxyURL(server, "avatar", did, validCID), nil)

		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	})

	t.Run("the DID does not resolve", func(t *testing.T) {
		server := newProxyServer(t, failingResolver{}, time.Second)

		resp, _ := fetch(t, proxyURL(server, "avatar", did, validCID), nil)

		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	})
}

// TestImageProxy_UndecodableUpstreamBytes covers what happens when the fetch
// succeeds but the bytes are not an image the processor can read.
//
// The proxy must not pass them through: a 200 carrying HTML under an image
// Content-Type is how a proxy becomes an XSS vector, and a truncated image is
// how a cache ends up holding a permanently broken entry.
func TestImageProxy_UndecodableUpstreamBytes(t *testing.T) {
	t.Parallel()

	did := "did:plc:" + testkit.UniqueID(t)
	upstream := newBlobServer(t, map[string]func(http.ResponseWriter){
		"textdata": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("this is not an image"))
		},
		"corruptedimage": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			// A PNG signature with nothing behind it.
			_, _ = w.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x00})
		},
		"emptybody": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusOK)
		},
	})
	server := newProxyServer(t, &fixedPDSResolver{pdsURL: upstream.URL}, defaultFetchTimeout)

	for _, cid := range []string{"textdata", "corruptedimage", "emptybody"} {
		t.Run(cid, func(t *testing.T) {
			resp, _ := fetch(t, proxyURL(server, "avatar", did, cid), nil)

			// The exact code differs by failure mode — a sniffable non-image is
			// a 400, a decoder blowing up mid-stream is a 500 — and pinning
			// each one would make this brittle about which layer noticed
			// first. What must never happen is a 2xx.
			assert.GreaterOrEqual(t, resp.StatusCode, 400,
				"undecodable upstream bytes must not be served as a success")
		})
	}
}

// TestImageProxy_PresetGeometry covers the preset table: each named preset has
// a documented output geometry, and the fit mode decides whether a source is
// cropped to it or merely bounded by it.
func TestImageProxy_PresetGeometry(t *testing.T) {
	t.Parallel()

	const cid = "bafybeipresetgeometry123"
	did := "did:plc:" + testkit.UniqueID(t)

	// 1000x1000 so that both directions are exercised: the cover presets crop
	// it down, and content_preview — the one preset that only bounds — has
	// something to shrink. A source smaller than every preset would make the
	// no-upscaling case below indistinguishable from a no-op.
	upstream := newBlobServer(t, map[string]func(http.ResponseWriter){
		cid: servePNG(testkit.TestPNGColor(1000, 1000, color.RGBA{R: 200, G: 100, B: 50, A: 255})),
	})
	server := newProxyServer(t, &fixedPDSResolver{pdsURL: upstream.URL}, defaultFetchTimeout)

	// The cover presets: the source is scaled and cropped to exactly these.
	for _, preset := range []struct {
		name          string
		width, height int
	}{
		{"avatar", 1000, 1000},
		{"avatar_small", 360, 360},
		{"banner", 640, 300},
		{"embed_thumbnail", 720, 360},
	} {
		t.Run(preset.name, func(t *testing.T) {
			resp, body := fetch(t, proxyURL(server, preset.name, did, cid), nil)

			require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
			assertImageSize(t, body, preset.width, preset.height)
		})
	}

	t.Run("content_preview bounds the width and keeps the aspect ratio", func(t *testing.T) {
		resp, body := fetch(t, proxyURL(server, "content_preview", did, cid), nil)

		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
		// 800 is the preset's maximum width; the square source stays square.
		assertImageSize(t, body, 800, 800)
	})

	t.Run("content_preview does not upscale a small source", func(t *testing.T) {
		const smallCID = "bafybeismallsource123"
		smallUpstream := newBlobServer(t, map[string]func(http.ResponseWriter){
			smallCID: servePNG(testkit.TestPNGColor(200, 200, color.RGBA{R: 100, G: 150, B: 200, A: 255})),
		})
		smallServer := newProxyServer(t, &fixedPDSResolver{pdsURL: smallUpstream.URL}, defaultFetchTimeout)

		resp, body := fetch(t, proxyURL(smallServer, "content_preview", did, smallCID), nil)

		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
		// Bounding, not resizing: a source already inside the bound is left
		// alone rather than blown up into a blurry 800x800.
		assertImageSize(t, body, 200, 200)
	})

	t.Run("an unknown preset is a 400", func(t *testing.T) {
		resp, body := fetch(t, proxyURL(server, "not_a_valid_preset", did, cid), nil)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, string(body), "invalid preset")
	})

	t.Run("a request missing the DID or the CID is refused", func(t *testing.T) {
		for name, url := range map[string]string{
			"missing CID": fmt.Sprintf("%s/img/avatar/plain/%s/", server.URL, did),
			"missing DID": fmt.Sprintf("%s/img/avatar/plain//%s", server.URL, cid),
		} {
			t.Run(name, func(t *testing.T) {
				resp, _ := fetch(t, url, nil)

				// 400 or 404 depending on whether the router matched the
				// pattern at all; either is a refusal, and which one is a
				// routing detail rather than a contract.
				assert.True(t, resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound,
					"expected 400 or 404 for %s, got %d", name, resp.StatusCode)
			})
		}
	})
}

// TestImageProxy_ErrorsAreNotJSON covers the response format of a failure.
//
// Every other endpoint in the AppView answers XRPC and speaks JSON, so it would
// be an easy and invisible mistake to make this one do the same. It must not:
// the proxy is addressed by <img src>, and a browser handed a JSON body under
// an image request shows a broken image with no clue why. Plain text is what a
// developer sees when they open the URL directly.
func TestImageProxy_ErrorsAreNotJSON(t *testing.T) {
	t.Parallel()

	server := newProxyServer(t, &fixedPDSResolver{pdsURL: unreachableURL(t)}, time.Second)
	did := "did:plc:" + testkit.UniqueID(t)

	resp, body := fetch(t, proxyURL(server, "invalid_preset", did, "cid"), nil)

	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")

	var decoded map[string]any
	assert.Error(t, json.Unmarshal(body, &decoded),
		"an error body must not parse as JSON, got: %s", body)
}
