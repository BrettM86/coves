package media_test

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"runtime"
	"testing"
	"time"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

// scaleFixPhotoJPEG is a width × height 4:2:0 JPEG with detail in every block,
// so decoding it does the work a camera photo's does.
func scaleFixPhotoJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	for y := range height {
		for x := range width {
			img.Y[img.YOffset(x, y)] = uint8(x*7 + y*3 + (x*y)>>5)
		}
	}
	for y := range (height + 1) / 2 {
		for x := range (width + 1) / 2 {
			img.Cb[y*img.CStride+x] = uint8(x + y>>2)
			img.Cr[y*img.CStride+x] = uint8(y - x>>3)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode %dx%d photo JPEG: %v", width, height, err)
	}
	return encoded.Bytes()
}

// scaleFixMeasureUploadAlloc runs Upload once and reports the bytes the whole
// process allocated during the call. TotalAlloc is cumulative and monotonic,
// so the delta counts every allocation made while Upload ran regardless of
// GC; the runtime.GC() beforehand only keeps a collection from being triggered
// mid-call. What isolates the number is scheduling: no test in this package is
// t.Parallel, so the caller is the only goroutine allocating.
func scaleFixMeasureUploadAlloc(t *testing.T, p *media.Pipeline, data []byte) (media.UploadedImage, *recordingUploader, uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	uploader := &recordingUploader{}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	uploaded, err := p.Upload(ctx, uploader, media.Input{Data: data, DeclaredContentType: "image/jpeg"})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return uploaded, uploader, after.TotalAlloc - before.TotalAlloc
}

// A 48 MP JPEG decodes to a 72 MB 4:2:0 frame. Downscaling it to a 4096 long
// edge needs a 50 MB RGBA destination and, in orientation 6, a second 50 MB
// RGBA for the rotated result. The rows measure 150 MB and 200 MB; their
// bounds (192 MiB and 256 MiB) sit 1.34× over.
//
// The bounds catch two regressions, each measured on both rows. A direct
// draw.CatmullRom.Scale of the whole frame allocates a 4096 × 6000 × 32 B =
// 786 MB kernel buffer: 937 MB and 988 MB, 4.6× and 3.7× over. Orienting the
// decoded frame before scaling, rather than the scaled one, builds a 192 MB
// 6000 × 8000 RGBA, plus what its per-pixel colour conversion allocates:
// 495 MB on both rows, 2.5× and 1.8× over.
func TestUploadDownscaleAllocation(t *testing.T) {
	photo := scaleFixPhotoJPEG(t, 8000, 6000)
	rows := []struct {
		name                  string
		input                 []byte
		wantWidth, wantHeight int
		maxAllocated          uint64
	}{
		{name: "8000x6000 orientation 1", input: photo, wantWidth: 4096, wantHeight: 3072, maxAllocated: 192 << 20},
		{name: "8000x6000 orientation 6", input: testkit.JPEGWithSegments(photo, testkit.JPEGExifGPSegment(6)), wantWidth: 3072, wantHeight: 4096, maxAllocated: 256 << 20},
	}
	p := pipelineWithMaxLongEdge(t, 1, media.DefaultMaxLongEdge)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			started := time.Now()
			uploaded, uploader, allocated := scaleFixMeasureUploadAlloc(t, p, row.input)
			t.Logf("allocated %d bytes in %s", allocated, time.Since(started))
			stored := decodeStored(t, uploaded, uploader, "image/jpeg")
			if stored.Bounds() != image.Rect(0, 0, row.wantWidth, row.wantHeight) {
				t.Errorf("stored bounds: got %v, want %dx%d", stored.Bounds(), row.wantWidth, row.wantHeight)
			}
			if allocated >= row.maxAllocated {
				t.Errorf("allocated %d bytes, want under %d", allocated, row.maxAllocated)
			}
		})
	}
}
