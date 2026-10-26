package media_test

import (
	"bytes"
	"image"
	"image/gif"
	"testing"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

// gifFormats is the post-image accept set, which includes GIF; allFormats
// (the avatar/banner set) does not.
var gifFormats = []media.Format{media.FormatJPEG, media.FormatPNG, media.FormatWebP, media.FormatGIF}

var twoGreys = [][3]byte{{0, 0, 0}, {255, 255, 255}}

// gifFile concatenates a logical screen and blocks into a GIF data stream.
func gifFile(screen []byte, blocks ...[]byte) []byte {
	out := append([]byte(nil), screen...)
	for _, block := range blocks {
		out = append(out, block...)
	}
	return out
}

// validFrame is a frame of the given bounds whose LZW data holds exactly
// Dx×Dy indices from a two-colour table.
func validFrame(bounds image.Rectangle) []byte {
	indices := make([]byte, bounds.Dx()*bounds.Dy())
	for i := range indices {
		indices[i] = byte(i % 2)
	}
	return testkit.GIFImage(bounds, nil, 2, testkit.GIFLZW(2, indices))
}

func TestGIFFixtureCleanBytesDecode(t *testing.T) {
	_, clean := testkit.AnimatedGIFWithMetadata()
	decoded, err := gif.DecodeAll(bytes.NewReader(clean))
	if err != nil {
		t.Fatalf("expected clean GIF must decode: %v", err)
	}
	if decoded.Config.Width != 4 || decoded.Config.Height != 3 {
		t.Errorf("logical screen: got %dx%d, want 4x3", decoded.Config.Width, decoded.Config.Height)
	}
	if len(decoded.Image) != 3 || decoded.LoopCount != 5 {
		t.Fatalf("frames/loop count: got %d frames loop %d, want 3 frames loop 5", len(decoded.Image), decoded.LoopCount)
	}
	if want := []int{10, 20, 30}; !equalInts(decoded.Delay, want) {
		t.Errorf("delays: got %v, want %v", decoded.Delay, want)
	}
	if want := []byte{2, 1, 1}; !bytes.Equal(decoded.Disposal, want) {
		t.Errorf("disposals: got %v, want %v", decoded.Disposal, want)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPipelineSanitizesGIF(t *testing.T) {
	t.Run("animated GIF89a loses comment, foreign application extension, extra loop sub-block and trailing bytes", func(t *testing.T) {
		dirty, clean := testkit.AnimatedGIFWithMetadata()

		result, err := ingest(t, pipeline(t, 2), dirty, "image/gif", gifFormats)
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if result.Format != "image/gif" || result.Width != 4 || result.Height != 3 {
			t.Fatalf("result: got %q %dx%d, want image/gif 4x3", result.Format, result.Width, result.Height)
		}
		for _, secret := range testkit.AnimatedGIFSecrets {
			if bytes.Contains(result.Data, []byte(secret)) {
				t.Errorf("output retains %q", secret)
			}
		}
		if !bytes.Equal(result.Data, clean) {
			t.Errorf("output: got %d bytes %x, want the expected clean GIF (%d bytes %x)", len(result.Data), result.Data, len(clean), clean)
		}
		decoded, err := gif.DecodeAll(bytes.NewReader(result.Data))
		if err != nil {
			t.Fatalf("decode output: %v", err)
		}
		if len(decoded.Image) != 3 || decoded.LoopCount != 5 {
			t.Fatalf("output frames/loop count: got %d frames loop %d, want 3 frames loop 5", len(decoded.Image), decoded.LoopCount)
		}
		if want := []int{10, 20, 30}; !equalInts(decoded.Delay, want) {
			t.Errorf("output delays: got %v, want %v", decoded.Delay, want)
		}
		wantFrames := []struct {
			bounds  image.Rectangle
			indices []byte
		}{
			{image.Rect(1, 1, 3, 3), []byte{1, 2, 3, 0}},
			{image.Rect(0, 0, 4, 3), []byte{7, 6, 5, 4, 3, 2, 1, 0, 7, 5, 3, 1}},
			{image.Rect(0, 0, 4, 3), []byte{0, 1, 2, 3, 3, 2, 1, 0, 1, 1, 2, 2}},
		}
		for i, want := range wantFrames {
			frame := decoded.Image[i]
			if frame.Bounds() != want.bounds || !bytes.Equal(frame.Pix, want.indices) {
				t.Errorf("frame %d: got bounds %v indices %v, want %v %v", i, frame.Bounds(), frame.Pix, want.bounds, want.indices)
			}
		}
	})

	t.Run("static GIF87a with nothing to strip passes through", func(t *testing.T) {
		input := testkit.StaticGIF87a()

		result, err := ingest(t, pipeline(t, 2), input, "image/gif", gifFormats)
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if result.Format != "image/gif" || result.Width != 2 || result.Height != 3 {
			t.Fatalf("result: got %q %dx%d, want image/gif 2x3", result.Format, result.Width, result.Height)
		}
		if !bytes.Equal(result.Data, input) {
			t.Errorf("output: got %x, want the input unchanged %x", result.Data, input)
		}
	})
}

// Every row runs while both decode slots are held, so a GIF that reaches the
// decode step waits and fails the row instead of returning its code.
func TestPipelineRejectsGIFBeforeDecodeSlot(t *testing.T) {
	fiftyOneFullScreenFrames := testkit.GIFLogicalScreen("GIF89a", 1000, 1000, twoGreys)
	for range 51 {
		fiftyOneFullScreenFrames = append(fiftyOneFullScreenFrames, testkit.GIFImage(image.Rect(0, 0, 1000, 1000), nil, 2, []byte{0xff})...)
	}
	fiftyOneFullScreenFrames = append(fiftyOneFullScreenFrames, testkit.GIFTrailer)

	frame2 := validFrame(image.Rect(0, 0, 4, 3))
	if len(frame2) != 13+int(frame2[11]) || frame2[11] < 3 {
		t.Fatalf("fixture: frame 2 needs one LZW sub-block of at least 3 bytes to truncate inside it, got % x", frame2)
	}

	// A plain text extension: 12-byte text grid header, then the text.
	hiddenPlainText := append([]byte{0x21, 0x01, 12, 0, 0, 0, 0, 4, 0, 3, 0, 1, 1, 0, 1},
		testkit.GIFSubBlocks([]byte("GPS 51.5007N 0.1246W owner=alice@example.com"))...)

	rows := []struct {
		name string
		data []byte
		code media.ErrorCode
	}{
		{"1000x1000 screen with 51 full-screen frames exceeds 50 million frame pixels", fiftyOneFullScreenFrames, media.ErrorCodeImageDimensionsTooLarge},
		{
			"10000x5000 screen with two 1x1 frames counts screen pixels per frame",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 10000, 5000, twoGreys), validFrame(image.Rect(0, 0, 1, 1)), validFrame(image.Rect(0, 0, 1, 1)), []byte{testkit.GIFTrailer}),
			media.ErrorCodeImageDimensionsTooLarge,
		},
		{
			"second frame extends beyond the logical screen",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 4, 3, twoGreys), validFrame(image.Rect(0, 0, 4, 3)), validFrame(image.Rect(2, 1, 5, 3)), []byte{testkit.GIFTrailer}),
			media.ErrorCodeInvalidImage,
		},
		{
			"valid first frame then no trailer",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 4, 3, twoGreys), validFrame(image.Rect(0, 0, 4, 3))),
			media.ErrorCodeInvalidImage,
		},
		{
			"image data sub-block of frame 2 truncated",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 4, 3, twoGreys), validFrame(image.Rect(0, 0, 4, 3)), frame2[:len(frame2)-3]),
			media.ErrorCodeInvalidImage,
		},
		{
			"comment extension truncated after the first frame",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 4, 3, twoGreys), validFrame(image.Rect(0, 0, 4, 3)), []byte{0x21, 0xfe, 16, 'a', 'b', 'c'}),
			media.ErrorCodeInvalidImage,
		},
		{
			"frame extends below the logical screen",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 4, 3, twoGreys), validFrame(image.Rect(0, 2, 4, 5)), []byte{testkit.GIFTrailer}),
			media.ErrorCodeInvalidImage,
		},
		{
			"plain text extension hiding text before the frame",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 4, 3, twoGreys), hiddenPlainText, validFrame(image.Rect(0, 0, 4, 3)), []byte{testkit.GIFTrailer}),
			media.ErrorCodeInvalidImage,
		},
		{
			"unknown extension label 0x99 before the frame",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 4, 3, twoGreys), append([]byte{0x21, 0x99}, testkit.GIFSubBlocks([]byte("SECRET-UNKNOWN-EXTENSION"))...), validFrame(image.Rect(0, 0, 4, 3)), []byte{testkit.GIFTrailer}),
			media.ErrorCodeInvalidImage,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p, ctx, _ := heldSlots(t)
			_, done := startIngest(p, ctx, row.data, gifFormats)
			expectCode(t, waitIngest(t, done).err, row.code)
		})
	}
}

func TestPipelineRejectsCorruptGIFFrameData(t *testing.T) {
	screen := testkit.GIFLogicalScreen("GIF89a", 4, 3, [][3]byte{{0, 0, 0}, {255, 0, 0}, {0, 255, 0}, {0, 0, 255}})
	rows := []struct {
		name   string
		frame2 []byte
	}{
		{"frame 2 LZW data ends after 5 of 12 indices", testkit.GIFImage(image.Rect(0, 0, 4, 3), nil, 2, testkit.GIFLZW(2, []byte{0, 1, 2, 3, 0}))},
		{"frame 2 index 3 outside its two-colour local table", testkit.GIFImage(image.Rect(0, 0, 4, 3), twoGreys, 2, testkit.GIFLZW(2, []byte{0, 1, 0, 1, 0, 1, 3, 1, 0, 1, 0, 1}))},
		{"1x1 frame 2 LZW data encodes 100,000 indices", testkit.GIFImage(image.Rect(0, 0, 1, 1), nil, 2, testkit.GIFLZW(2, make([]byte, 100_000)))},
		{"frame 2 has a 21-byte sub-block after its LZW end code", frameWithSubBlockAfterEndCode([]byte("SECRET-AFTER-END-CODE"))},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			data := gifFile(screen, validFrame(image.Rect(0, 0, 4, 3)), row.frame2, []byte{testkit.GIFTrailer})

			_, err := ingest(t, pipeline(t, 2), data, "image/gif", gifFormats)
			expectCode(t, err, media.ErrorCodeInvalidImage)
		})
	}
}

func TestPipelineAcceptsGIFAtFrameBudget(t *testing.T) {
	data := gifFile(testkit.GIFLogicalScreen("GIF89a", 10000, 5000, twoGreys), validFrame(image.Rect(0, 0, 1, 1)), []byte{testkit.GIFTrailer})

	result, err := ingest(t, pipeline(t, 2), data, "image/gif", gifFormats)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Format != "image/gif" || result.Width != 10000 || result.Height != 5000 {
		t.Fatalf("result: got %q %dx%d, want image/gif 10000x5000", result.Format, result.Width, result.Height)
	}
}

// frameWithSubBlockAfterEndCode is a valid 4×3 frame whose last LZW sub-block
// ends with the end code, followed by extra as one more sub-block before the
// terminator.
func frameWithSubBlockAfterEndCode(extra []byte) []byte {
	frame := validFrame(image.Rect(0, 0, 4, 3))
	frame = append(frame[:len(frame)-1], byte(len(extra)))
	frame = append(frame, extra...)
	return append(frame, 0)
}

func TestPipelineAcceptsGIFThatImageGIFAccepts(t *testing.T) {
	rows := []struct {
		name string
		data []byte
	}{
		{
			"one 1-byte sub-block after the LZW end code",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 4, 3, twoGreys), frameWithSubBlockAfterEndCode([]byte{0}), []byte{testkit.GIFTrailer}),
		},
		{
			"transparent index 3 past a two-colour table, used by a pixel",
			gifFile(testkit.GIFLogicalScreen("GIF89a", 2, 1, twoGreys), testkit.GIFGraphicControl(0, 0, 3),
				testkit.GIFImage(image.Rect(0, 0, 2, 1), nil, 2, testkit.GIFLZW(2, []byte{0, 3})), []byte{testkit.GIFTrailer}),
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := gif.DecodeAll(bytes.NewReader(row.data)); err != nil {
				t.Fatalf("fixture: image/gif must accept the input: %v", err)
			}

			result, err := ingest(t, pipeline(t, 2), row.data, "image/gif", gifFormats)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if !bytes.Equal(result.Data, row.data) {
				t.Errorf("output: got %x, want the input unchanged %x", result.Data, row.data)
			}
		})
	}
}
