package media_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png"
	"strconv"
	"testing"
	"time"

	ipfscid "github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

type missingImageCache struct{}

func (missingImageCache) Get(string, string, string) ([]byte, bool, error) { return nil, false, nil }
func (missingImageCache) Set(string, string, string, []byte) error         { return nil }
func (missingImageCache) Delete(string, string, string) error              { return nil }
func (missingImageCache) Cleanup() (int, error)                            { return 0, nil }
func (missingImageCache) DeleteOwner(string, string) error                 { return nil }
func (missingImageCache) DeleteCID(string) error                           { return nil }
func (missingImageCache) OwnerDirectories(string) ([]string, error)        { return nil, nil }

type staticImageFetcher []byte

func (f staticImageFetcher) Fetch(context.Context, string, string, string) ([]byte, error) {
	return []byte(f), nil
}

type noImageBlocks struct{}

func (noImageBlocks) IsBlocked(context.Context, string, string) (bool, error) { return false, nil }

type allImageReferences struct{}

func (allImageReferences) HasActiveReference(context.Context, string, string) (bool, error) {
	return true, nil
}

func holdImageSlots(t *testing.T, p *media.Pipeline, count int) []func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	releases := make([]func(), 0, count)
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})
	for range count {
		release, err := media.HoldDecodeSlot(p, ctx)
		if err != nil {
			t.Fatalf("hold decode slot: %v", err)
		}
		releases = append(releases, release)
	}
	return releases
}

func TestPipelineDecodeSlotsAndProxyIndependence(t *testing.T) {
	for _, slots := range []int{4, 2} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			jpegData := testkit.TestJPEG(7, 5)
			t.Run("one free slot", func(t *testing.T) {
				p := pipeline(t, slots)
				holdImageSlots(t, p, slots-1)
				result, err := ingest(t, p, jpegData, "image/jpeg", allFormats)
				if err != nil || result.Format != "image/jpeg" || result.Width != 7 || result.Height != 5 {
					t.Fatalf("free slot ingest: result=%+v error=%v, want JPEG 7x5", result, err)
				}
			})
			t.Run("queued upload does not block proxy", func(t *testing.T) {
				p := pipeline(t, slots)
				releases := holdImageSlots(t, p, slots)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				pngData := testkit.TestPNG(16, 12)
				hash, err := multihash.Sum(pngData, multihash.SHA2_256, -1)
				if err != nil {
					t.Fatalf("hash PNG: %v", err)
				}
				cid := ipfscid.NewCidV1(ipfscid.Raw, hash).String()
				processor, err := imageproxy.NewProcessor(50)
				if err != nil {
					t.Fatalf("build proxy processor: %v", err)
				}
				service, err := imageproxy.NewService(missingImageCache{}, processor, staticImageFetcher(pngData), noImageBlocks{}, allImageReferences{}, imageproxy.DefaultConfig())
				if err != nil {
					t.Fatalf("build image proxy: %v", err)
				}
				started, done := startIngest(p, ctx, jpegData, allFormats)
				<-started
				type proxyResult struct {
					data []byte
					err  error
				}
				proxyDone := make(chan proxyResult, 1)
				go func() {
					data, err := service.GetImageResolvingPDS(ctx, "avatar_small", "did:plc:test123", cid, func(context.Context) (string, error) { return "https://pds.example", nil })
					proxyDone <- proxyResult{data, err}
				}()
				testkit.Holds(t, 300*time.Millisecond, func() (bool, error) { return len(done) == 0, nil }, testkit.WithDescription("upload waits while proxy decodes"))
				testkit.WaitFor(t, 5*time.Second, func() (bool, error) { return len(proxyDone) != 0, nil }, testkit.WithDescription("proxy serves an image while every upload decode slot is held"))
				proxy := <-proxyDone
				if proxy.err != nil {
					t.Fatalf("proxy while upload queued: %v", proxy.err)
				}
				served := proxy.data
				decoded, _, err := image.Decode(bytes.NewReader(served))
				if err != nil {
					t.Fatalf("decode served image: %v", err)
				}
				if decoded.Bounds().Dx() != 360 || decoded.Bounds().Dy() != 360 {
					t.Fatalf("served image = %v, want 360x360", decoded.Bounds())
				}
				if len(done) != 0 {
					t.Fatal("upload finished before a decode slot was released")
				}
				releases[0]()
				releases[0] = func() {}
				finished := waitIngest(t, done)
				if finished.err != nil || finished.result.Format != "image/jpeg" {
					t.Fatalf("released upload = %+v, error %v; want JPEG", finished.result, finished.err)
				}
			})
			t.Run("cancel queued upload", func(t *testing.T) {
				p := pipeline(t, slots)
				holdImageSlots(t, p, slots)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				started, done := startIngest(p, ctx, jpegData, allFormats)
				<-started
				testkit.Holds(t, 300*time.Millisecond, func() (bool, error) { return len(done) == 0, nil }, testkit.WithDescription("cancelled upload was queued"))
				cancel()
				if result := waitIngest(t, done); !errors.Is(result.err, context.Canceled) {
					t.Fatalf("queued cancellation error = %v, want context.Canceled", result.err)
				}
			})
		})
	}
}

func TestPipelineCorruptDecodeReleasesSlots(t *testing.T) {
	p := pipeline(t, 2)
	corrupt := testkit.TestJPEG(7, 5)
	_, scan, err := testkit.WalkJPEGSegments(corrupt)
	if err != nil {
		t.Fatalf("walk JPEG: %v", err)
	}
	// Cut the scan short but keep EOI, so the marker walk run before the slot
	// passes and the failure comes from the decoder while it holds a slot.
	corrupt = append(corrupt[:len(corrupt)-len(scan)+len(scan)/2:len(corrupt)-len(scan)+len(scan)/2], 0xff, 0xd9)
	if _, err := jpeg.Decode(bytes.NewReader(corrupt)); err == nil {
		t.Fatal("truncated JPEG fixture must fail to decode")
	}
	for range 4 {
		_, err := ingest(t, p, corrupt, "image/jpeg", allFormats)
		expectCode(t, err, media.ErrorCodeInvalidImage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	for range 2 {
		release, err := media.HoldDecodeSlot(p, ctx)
		if err != nil {
			t.Fatalf("corrupt decode leaked slot: %v", err)
		}
		defer release()
	}
}

func TestPipelineHoldsDecodeSlotThroughOrientedEncode(t *testing.T) {
	p := pipeline(t, 1)
	hookRan := false
	var slotErrorDuringEncode error
	media.SetBeforeOrientedEncode(p, func() {
		hookRan = true
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		release, err := media.HoldDecodeSlot(p, ctx)
		if err == nil {
			release()
		}
		slotErrorDuringEncode = err
	})
	input := testkit.JPEGWithSegments(testkit.TestJPEG(16, 8), testkit.JPEGExifGPSegment(6))
	result, err := ingest(t, p, input, "image/jpeg", allFormats)
	if err != nil || result.Width != 8 || result.Height != 16 {
		t.Fatalf("orientation 6 ingest: result=%dx%d error=%v, want 8x16", result.Width, result.Height, err)
	}
	if !hookRan {
		t.Fatal("orientation 6 ingest did not reach the oriented encode")
	}
	if !errors.Is(slotErrorDuringEncode, context.DeadlineExceeded) {
		t.Fatalf("decode slot during orientation and encode: acquire error %v, want context.DeadlineExceeded (slot still held)", slotErrorDuringEncode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	release, err := media.HoldDecodeSlot(p, ctx)
	if err != nil {
		t.Fatalf("oriented ingest leaked its decode slot: %v", err)
	}
	release()
}

func TestUploadHoldsDecodeSlotThroughDownscaleEncode(t *testing.T) {
	p := pipelineWithMaxLongEdge(t, 1, 120)
	hookRan := false
	var slotErrorDuringEncode error
	media.SetBeforeDownscaleEncode(p, func() {
		hookRan = true
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		release, err := media.HoldDecodeSlot(p, ctx)
		if err == nil {
			release()
		}
		slotErrorDuringEncode = err
	})

	upload(t, p, quadrantJPEGOfSize(t, 300, 200), "image/jpeg")

	if !hookRan {
		t.Fatal("300x200 upload over a 120-pixel limit did not reach the downscale encode")
	}
	if !errors.Is(slotErrorDuringEncode, context.DeadlineExceeded) {
		t.Fatalf("decode slot during downscale encode: acquire error %v, want context.DeadlineExceeded (slot still held)", slotErrorDuringEncode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	release, err := media.HoldDecodeSlot(p, ctx)
	if err != nil {
		t.Fatalf("downscaled upload leaked its decode slot: %v", err)
	}
	release()
}

func TestPipelineRejectsNonpositiveConcurrency(t *testing.T) {
	for _, slots := range []int{0, -1} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			if _, err := media.NewPipeline(media.Config{MaxConcurrent: slots, MaxLongEdge: media.DefaultMaxLongEdge}); err == nil {
				t.Fatalf("MaxConcurrent=%d: want error", slots)
			}
		})
	}
}
