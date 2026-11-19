package imageproxy

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"golang.org/x/image/webp"

	"Coves/internal/core/imageproxy"
	"Coves/tests/testkit"
)

type formatFetcher func(context.Context, string, string, string) ([]byte, error)

func (fetch formatFetcher) Fetch(ctx context.Context, pdsURL, did, cid string) ([]byte, error) {
	return fetch(ctx, pdsURL, did, cid)
}

func transparentFormatPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1200, 1200))
	draw.Draw(img, image.Rect(600, 0, 1200, 1200), image.NewUniform(color.NRGBA{R: 255, A: 255}), image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode transparent PNG: %v", err)
	}
	return encoded.Bytes()
}

// tallTransparentFormatPNG is a 2x17000 PNG with a transparent left column.
// Its height is over WebP's 16383 side limit, so the proxy serves it as PNG.
func tallTransparentFormatPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 17000))
	draw.Draw(img, image.Rect(1, 0, 2, 17000), image.NewUniform(color.NRGBA{R: 255, A: 255}), image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode tall transparent PNG: %v", err)
	}
	return encoded.Bytes()
}

func rawCIDForHandlerFixture(t *testing.T, data []byte) string {
	t.Helper()
	digest, err := multihash.Sum(data, multihash.SHA2_256, -1)
	if err != nil {
		t.Fatalf("hash fixture: %v", err)
	}
	return cid.NewCidV1(cid.Raw, digest).String()
}

func formatGIF(t *testing.T, animated bool) []byte {
	t.Helper()
	palette := color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	first := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)
	var encoded bytes.Buffer
	if animated {
		second := image.NewPaletted(first.Rect, palette)
		for i := range second.Pix {
			second.Pix[i] = 1
		}
		if err := gif.EncodeAll(&encoded, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{5, 5}}); err != nil {
			t.Fatalf("encode animated GIF: %v", err)
		}
		if !bytes.HasPrefix(encoded.Bytes(), []byte("GIF89a")) {
			t.Fatalf("animated fixture header = %q, want GIF89a", encoded.Bytes()[:6])
		}
		decoded, err := gif.DecodeAll(bytes.NewReader(encoded.Bytes()))
		if err != nil {
			t.Fatalf("decode GIF89a fixture: %v", err)
		}
		if len(decoded.Image) != 2 {
			t.Fatalf("GIF89a fixture frames = %d, want 2", len(decoded.Image))
		}
		return encoded.Bytes()
	}
	if err := gif.Encode(&encoded, first, nil); err != nil {
		t.Fatalf("encode single-frame GIF: %v", err)
	}
	data := encoded.Bytes()
	copy(data[:6], "GIF87a")
	if _, err := gif.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("decode GIF87a fixture: %v", err)
	}
	return data
}

func newFormatServingHandler(t *testing.T, cacheDir string, fetcher imageproxy.Fetcher, referenceCheckers ...imageproxy.ReferenceChecker) *Handler {
	t.Helper()
	references := imageproxy.ReferenceChecker(allowAllReferences{})
	if len(referenceCheckers) > 0 {
		references = referenceCheckers[0]
	}
	cache, err := imageproxy.NewDiskCache(cacheDir, 1, 0)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	config := imageproxy.DefaultConfig()
	config.CachePath = cacheDir
	service, err := imageproxy.NewService(cache, processor, fetcher, allowAllBlockChecker{}, references, config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return NewHandler(service, resolverForPDS("https://pds.example.com"))
}

func serveFormatImage(handler *Handler, method, preset, requestedCID string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := createTestRequest(method, "/img/"+preset+"/plain/"+validTestDID+"/"+requestedCID, map[string]string{
		"preset": preset,
		"did":    validTestDID,
		"cid":    requestedCID,
	})
	handler.HandleImage(recorder, request)
	return recorder
}

// opaqueFormatPNG is a 1000x500 opaque PNG; content_preview contains it at
// 800x400.
func opaqueFormatPNG() []byte {
	return testkit.TestPNG(1000, 500)
}

// assertServedWebP checks that a served body carries the WebP magic and decodes
// at exactly width x height, and returns the decoded image.
func assertServedWebP(t *testing.T, body []byte, width, height int) image.Image {
	t.Helper()
	if len(body) < 12 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WEBP" {
		prefix := body[:min(len(body), 12)]
		t.Fatalf("served body is not WebP: want RIFF....WEBP magic, got % x", prefix)
	}
	img, err := webp.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode served WebP: %v", err)
	}
	if got := img.Bounds().Dx(); got != width {
		t.Errorf("decoded width = %d, want %d", got, width)
	}
	if got := img.Bounds().Dy(); got != height {
		t.Errorf("decoded height = %d, want %d", got, height)
	}
	return img
}

func TestHandler_ServesSourceFormats(t *testing.T) {
	transparentSource := transparentFormatPNG(t)
	webpSource, err := os.ReadFile(filepath.Join("testdata", "source.webp"))
	if err != nil {
		t.Fatalf("read WebP fixture: %v", err)
	}
	cases := []struct {
		name, preset  string
		source        []byte
		width, height int
		checkAlpha    bool
	}{
		{"OpaquePNG/content_preview", "content_preview", opaqueFormatPNG(), 800, 400, false},
		{"TransparentPNG/avatar", "avatar", transparentSource, 1000, 1000, true},
		{"TransparentPNG/avatar_small", "avatar_small", transparentSource, 192, 192, true},
		{"TransparentPNG/banner", "banner", transparentSource, 640, 300, true},
		{"TransparentPNG/content_preview", "content_preview", transparentSource, 800, 800, true},
		{"TransparentPNG/content_full", "content_full", transparentSource, 1200, 1200, true},
		{"TransparentPNG/embed_thumbnail", "embed_thumbnail", transparentSource, 720, 360, true},
		{"JPEG/avatar_small", "avatar_small", testkit.TestJPEGColor(120, 60, color.RGBA{R: 255, A: 255}), 192, 192, false},
		{"WebP/avatar_small", "avatar_small", webpSource, 192, 192, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newFormatServingHandler(t, t.TempDir(), formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
				return tc.source, nil
			}))
			response := serveFormatImage(handler, http.MethodGet, tc.preset, rawCIDForHandlerFixture(t, tc.source))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "image/webp" {
				t.Errorf("Content-Type = %q, want %q", got, "image/webp")
			}
			if got := response.Header().Get("Content-Length"); got != strconv.Itoa(response.Body.Len()) {
				t.Errorf("Content-Length = %q, want %d", got, response.Body.Len())
			}
			if got := response.Header().Get("ETag"); got != `"r3-`+tc.preset+`-`+rawCIDForHandlerFixture(t, tc.source)+`"` {
				t.Errorf("ETag = %q, want the r3 render version", got)
			}
			img := assertServedWebP(t, response.Body.Bytes(), tc.width, tc.height)
			if tc.checkAlpha {
				bounds := img.Bounds()
				middle := bounds.Min.Y + bounds.Dy()/2
				_, _, _, left := img.At(bounds.Min.X+bounds.Dx()/10, middle).RGBA()
				_, _, _, right := img.At(bounds.Max.X-1-bounds.Dx()/10, middle).RGBA()
				if left != 0 {
					t.Errorf("left alpha = %d, want 0", left)
				}
				if right != 0xffff {
					t.Errorf("right alpha = %d, want 65535", right)
				}
			}
		})
	}
}

func TestHandler_HeadMatchesGet(t *testing.T) {
	source := opaqueFormatPNG()
	requestedCID := rawCIDForHandlerFixture(t, source)
	handler := newFormatServingHandler(t, t.TempDir(), formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
		return source, nil
	}))
	get := serveFormatImage(handler, http.MethodGet, "content_preview", requestedCID)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body: %s", get.Code, get.Body.String())
	}
	head := serveFormatImage(handler, http.MethodHead, "content_preview", requestedCID)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200; body: %s", head.Code, head.Body.String())
	}
	if got := get.Header().Get("Content-Type"); got != "image/webp" {
		t.Errorf("GET Content-Type = %q, want %q", got, "image/webp")
	}
	if got := head.Header().Get("Content-Type"); got != "image/webp" {
		t.Errorf("HEAD Content-Type = %q, want %q", got, "image/webp")
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(get.Body.Len()) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, get.Body.Len())
	}
	if head.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", head.Body.Len())
	}
}

func TestHandler_ContentTypeSurvivesDiskCacheRestart(t *testing.T) {
	cases := []struct {
		name, preset, contentType string
		source                    []byte
	}{
		{"OpaquePNG/content_preview", "content_preview", "image/webp", opaqueFormatPNG()},
		{"GIF/content_full", "content_full", "image/gif", formatGIF(t, true)},
		{"TallTransparentPNG/content_full", "content_full", "image/png", tallTransparentFormatPNG(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			requestedCID := rawCIDForHandlerFixture(t, tc.source)
			firstHandler := newFormatServingHandler(t, cacheDir, formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
				return tc.source, nil
			}))
			first := serveFormatImage(firstHandler, http.MethodGet, tc.preset, requestedCID)
			if first.Code != http.StatusOK {
				t.Fatalf("first status = %d, want 200; body: %s", first.Code, first.Body.String())
			}
			if got := first.Header().Get("Content-Type"); got != tc.contentType {
				t.Errorf("first Content-Type = %q, want %q", got, tc.contentType)
			}
			if got := first.Header().Get("Content-Length"); got != strconv.Itoa(first.Body.Len()) {
				t.Errorf("first Content-Length = %q, want %d", got, first.Body.Len())
			}

			restartedHandler := newFormatServingHandler(t, cacheDir, formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
				t.Errorf("fetch invoked after disk cache restart")
				return nil, errors.New("fetch unavailable after restart")
			}))
			second := serveFormatImage(restartedHandler, http.MethodGet, tc.preset, requestedCID)
			if second.Code != http.StatusOK {
				t.Fatalf("cached status = %d, want 200; body: %s", second.Code, second.Body.String())
			}
			if !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) {
				t.Errorf("cached body differs from first response")
			}
			if got := second.Header().Get("Content-Type"); got != tc.contentType {
				t.Errorf("cached Content-Type = %q, want %q", got, tc.contentType)
			}
			if got := second.Header().Get("Content-Length"); got != strconv.Itoa(second.Body.Len()) {
				t.Errorf("cached Content-Length = %q, want %d", got, second.Body.Len())
			}
		})
	}
}

func TestHandler_ContentFullServesOriginalGIF(t *testing.T) {
	for _, tc := range []struct {
		name     string
		animated bool
	}{
		{"animated GIF89a", true},
		{"single-frame GIF87a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := formatGIF(t, tc.animated)
			requestedCID := rawCIDForHandlerFixture(t, source)
			handler := newFormatServingHandler(t, t.TempDir(), formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
				return source, nil
			}))
			response := serveFormatImage(handler, http.MethodGet, "content_full", requestedCID)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", response.Code, response.Body.String())
			}
			if !bytes.Equal(response.Body.Bytes(), source) {
				t.Errorf("served GIF differs from fetched blob")
			}
			if got := response.Header().Get("Content-Type"); got != "image/gif" {
				t.Errorf("Content-Type = %q, want image/gif", got)
			}
			if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len(source)) {
				t.Errorf("Content-Length = %q, want %d", got, len(source))
			}
		})
	}
}

func TestHandler_CIDMismatchReturns502(t *testing.T) {
	source := transparentFormatPNG(t)
	requestedCID := rawCIDForHandlerFixture(t, []byte("different blob"))
	handler := newFormatServingHandler(t, t.TempDir(), formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
		return source, nil
	}))
	response := serveFormatImage(handler, http.MethodGet, "content_full", requestedCID)
	if response.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", response.Code)
	}
}
