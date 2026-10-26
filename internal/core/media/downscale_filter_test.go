package media_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

// scaleFixCheckerboardPNG is a lossless grey PNG of 1-pixel black and white
// squares: all of its detail is at the source Nyquist frequency, so a
// low-pass downscale must render it as flat mid-grey.
func scaleFixCheckerboardPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			if (x+y)%2 == 0 {
				img.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode %dx%d checkerboard PNG: %v", width, height, err)
	}
	return encoded.Bytes()
}

// The kernel buffer for a direct CatmullRom.Scale of this fixture is
// 3000 (destination width) × 3000 (source rows) × 32 B = 288 MB, over the
// 256 MiB cap, and the 1.47× reduction leaves nothing for box pre-reduction,
// the shape of a 6000×4000 photo stored at a 4096 long edge.
//
// Measured on the 3000×2045 output, ignoring a 4-pixel border:
// ApproxBiLinear, which blends the nearest 2×2 source pixels, leaves a moiré
// deviating from mid-grey by up to 119.5 (mean 31.97); CatmullRom, widened to
// the reduction, by up to 2.5 (mean 1.29). The bounds, 16 and 4, sit 6× and
// 3× over CatmullRom and 7× and 8× under ApproxBiLinear.
func TestUploadDownscaleFiltersFineDetailOverTheKernelBufferCap(t *testing.T) {
	const sourceWidth, sourceHeight, maxLongEdge = 4400, 3000, 3000
	uploaded, uploader := upload(t, pipelineWithMaxLongEdge(t, 1, maxLongEdge), scaleFixCheckerboardPNG(t, sourceWidth, sourceHeight), "image/png")
	stored := decodeStored(t, uploaded, uploader, "image/png")
	if stored.Bounds() != image.Rect(0, 0, 3000, 2045) {
		t.Fatalf("stored bounds: got %v, want 3000x2045", stored.Bounds())
	}

	const border = 4
	var maxDeviation, sumDeviation float64
	pixels := 0
	for y := border; y < stored.Bounds().Dy()-border; y++ {
		for x := border; x < stored.Bounds().Dx()-border; x++ {
			grey := color.GrayModel.Convert(stored.At(x, y)).(color.Gray).Y
			deviation := math.Abs(float64(grey) - 127.5)
			maxDeviation = max(maxDeviation, deviation)
			sumDeviation += deviation
			pixels++
		}
	}
	meanDeviation := sumDeviation / float64(pixels)
	t.Logf("deviation from mid-grey: max %.1f, mean %.2f", maxDeviation, meanDeviation)
	if maxDeviation > 16 || meanDeviation > 4 {
		t.Errorf("checkerboard aliases: max deviation from mid-grey %.1f, mean %.2f", maxDeviation, meanDeviation)
	}
}
