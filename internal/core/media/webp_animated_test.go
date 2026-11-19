package media_test

import (
	"bytes"
	"image"
	"testing"

	// The production binary links the image proxy's WebP encoder, which
	// registers a global "webp" decoder that reads the first frame of an
	// animated WebP. Linking it here gives the test binary the same registry.
	gen2brainwebp "github.com/gen2brain/webp"

	"Coves/internal/core/media"
)

// webpAnimated builds a 1x1 animated WebP with the layout real encoders write:
// VP8X with the animation flag, ANIM, then one ANMF frame holding a decodable
// VP8L image.
func webpAnimated(t *testing.T) []byte {
	t.Helper()
	const animationFlag = 0x02

	// Background colour (4 bytes) and loop count (2 bytes, 0 = forever).
	anim := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0, 0}
	// Frame X and Y (3 bytes each), width-1 and height-1 (3 bytes each),
	// duration (3 bytes) and flags (1 byte), then the frame's image chunk.
	anmf := []byte{
		0, 0, 0,
		0, 0, 0,
		0, 0, 0,
		0, 0, 0,
		60, 0, 0,
		0x02,
	}
	anmf = append(anmf, webpImageChunk(t, losslessWebP(t))...)

	return webpContainer(webpVP8X(1, 1, animationFlag), webpChunk("ANIM", anim), webpChunk("ANMF", anmf))
}

func TestPipelineRejectsAnimatedWebP(t *testing.T) {
	data := webpAnimated(t)
	frame, err := gen2brainwebp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("fixture must decode with the linked first-frame decoder so the rejection comes from the pipeline: %v", err)
	}
	if frame.Bounds().Dx() != 1 || frame.Bounds().Dy() != 1 {
		t.Fatalf("fixture first frame: got %v, want 1x1", frame.Bounds())
	}

	registryFrame, registryFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || registryFormat != "webp" {
		t.Fatalf("fixture must decode through the global image registry so an unpinned decoder would accept it: format %q, error %v", registryFormat, err)
	}
	if registryFrame.Bounds().Dx() != 1 || registryFrame.Bounds().Dy() != 1 {
		t.Fatalf("registry first frame: got %v, want 1x1", registryFrame.Bounds())
	}

	result, err := ingest(t, pipeline(t, 1), data, "image/webp", []media.Format{media.FormatWebP})

	expectCode(t, err, media.ErrorCodeInvalidImage)
	if result.Data != nil {
		t.Fatalf("animated WebP produced %d bytes of output", len(result.Data))
	}
}
