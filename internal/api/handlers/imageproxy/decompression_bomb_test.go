package imageproxy

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/imageproxy"
)

// bombFetcher is a Fetcher that hands back the same bytes for every blob,
// standing in for a PDS that serves a hostile image.
type bombFetcher struct {
	payload []byte
	calls   atomic.Int64
}

func (f *bombFetcher) Fetch(_ context.Context, _, _, _ string) ([]byte, error) {
	f.calls.Add(1)
	return f.payload, nil
}

// missCache is a Cache that never has anything and never stores anything, so
// every request is forced through fetch and decode.
type missCache struct{}

func (missCache) Get(_, _, _ string) ([]byte, bool, error) { return nil, false, nil }
func (missCache) Set(_, _, _ string, _ []byte) error       { return nil }
func (missCache) Delete(_, _, _ string) error              { return nil }
func (missCache) Cleanup() (int, error)                    { return 0, nil }

// pngChunk frames one PNG chunk: length, type, data, CRC32 over type+data.
func pngChunk(chunkType string, data []byte) []byte {
	out := make([]byte, 0, 12+len(data))
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, chunkType...)
	out = append(out, data...)
	crc := crc32.NewIEEE()
	crc.Write([]byte(chunkType))
	crc.Write(data)
	return binary.BigEndian.AppendUint32(out, crc.Sum32())
}

// buildPNGDecompressionBomb hand-assembles a PNG whose IHDR declares
// width×height 8-bit RGBA pixels but which carries only a zlib stream header
// in its IDAT. Nothing in the file is a real pixel, so it is under 100 bytes,
// yet Go's png decoder allocates the full width*height*4-byte frame in
// readImagePass before it discovers the stream is empty and fails. The IDAT
// stub matters: an IHDR-only file hits EOF before decoding starts and does
// NOT allocate, which would make a pre-fix run look safe when it is not.
func buildPNGDecompressionBomb(t *testing.T, width, height uint32) []byte {
	t.Helper()

	ihdr := make([]byte, 0, 13)
	ihdr = binary.BigEndian.AppendUint32(ihdr, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr,
		8, // bit depth
		6, // colour type: truecolour with alpha
		0, // compression method
		0, // filter method
		0, // interlace method
	)

	file := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}
	file = append(file, pngChunk("IHDR", ihdr)...)
	file = append(file, pngChunk("IDAT", []byte{0x78, 0x9C})...)

	if len(file) >= 100 {
		t.Fatalf("bomb fixture is %d bytes; the point of the fixture is that it is tiny", len(file))
	}
	return file
}

// TestHandler_RejectsDecompressionBombBeforeDecoding reproduces
// docs/SECURITY_AUDIT_2026-09-01.md §1.1: an image whose header declares a
// pixel count the proxy cannot afford must be refused as ErrImageTooLarge
// BEFORE any pixel buffer is allocated, and that refusal must reach the client
// as a 400 "image too large" rather than a 500.
//
// The alloc bound is the real assertion. Pre-fix, the processor calls
// image.Decode straight away; for a 12000×12000 RGBA PNG that allocates
// ~549 MiB (12000*12000*4) per request for an input of under 100 bytes, then
// fails on "not enough pixel data" and surfaces as a 500 "image processing
// failed". Asserting only on the status would pass with a fix that decodes the
// whole frame and then measures it — which is exactly the attack. Bounding the
// heap delta across the handler call at 32 MiB proves the budget is enforced
// from the header alone.
//
// This test measures process-wide allocation via runtime.MemStats, so it must
// NOT call t.Parallel and must not be nested under a parallel parent: any
// concurrent test's allocations would land in the delta. Go runs sequential
// top-level tests before releasing parallel ones, so a sequential test here is
// isolated from this package's parallel suites.
func TestHandler_RejectsDecompressionBombBeforeDecoding(t *testing.T) {
	const (
		declaredWidth  = 12000
		declaredHeight = 12000
		allocBound     = 32 << 20 // 32 MiB; the bomb would be ~549 MiB
	)

	fetcher := &bombFetcher{payload: buildPNGDecompressionBomb(t, declaredWidth, declaredHeight)}

	processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	service, err := imageproxy.NewService(missCache{}, processor, fetcher, imageproxy.DefaultConfig())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	resolver := &mockIdentityResolver{
		resolveDIDFunc: func(_ context.Context, did string) (*identity.DIDDocument, error) {
			return &identity.DIDDocument{
				DID: did,
				Service: []identity.Service{{
					ID:              "#atproto_pds",
					Type:            "AtprotoPersonalDataServer",
					ServiceEndpoint: "https://pds.example.com",
				}},
			}, nil
		},
	}
	handler := NewHandler(service, resolver)

	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar",
		"did":    validTestDID,
		"cid":    validTestCID,
	})
	recorder := httptest.NewRecorder()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	handler.HandleImage(recorder, req)

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	if calls := fetcher.calls.Load(); calls != 1 {
		t.Errorf("expected the fetcher to be called exactly once, got %d calls", calls)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d (body %q)", http.StatusBadRequest, recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "image too large" {
		t.Errorf("expected body %q, got %q", "image too large", body)
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("expected Cache-Control %q, got %q", "no-store", cacheControl)
	}
	if allocated >= allocBound {
		t.Errorf("handler allocated %d bytes (%.1f MiB) serving a %d-byte input; bound is %d bytes (%d MiB) — the declared %dx%d frame was decoded",
			allocated, float64(allocated)/(1<<20), len(fetcher.payload), allocBound, allocBound>>20, declaredWidth, declaredHeight)
	}
}
