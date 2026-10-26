package media_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"golang.org/x/image/webp"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

func pipeline(t *testing.T, slots int) *media.Pipeline {
	t.Helper()
	p, err := media.NewPipeline(media.Config{MaxConcurrent: slots, MaxLongEdge: media.DefaultMaxLongEdge})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func ingest(t *testing.T, p *media.Pipeline, data []byte, declared string, accept []media.Format) (media.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.Ingest(ctx, media.Input{Data: data, DeclaredContentType: declared, Accept: accept})
}

func expectCode(t *testing.T, err error, code media.ErrorCode) {
	t.Helper()
	var imageError *media.Error
	if !errors.As(err, &imageError) {
		t.Fatalf("error: got %v, want *media.Error with code %s", err, code)
	}
	if imageError.Code != code {
		t.Fatalf("error code: got %s, want %s", imageError.Code, code)
	}
}

var allFormats = []media.Format{media.FormatJPEG, media.FormatPNG, media.FormatWebP}

// A literal lossless VP8L 1×1 image (verified by x/image/webp before ingestion).
func losslessWebP(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatalf("decode VP8L fixture: %v", err)
	}
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("VP8L fixture must decode as 1x1: %v", err)
	}
	if img.Bounds().Dx() != 1 || img.Bounds().Dy() != 1 {
		t.Fatalf("VP8L fixture bounds: got %v, want 1x1", img.Bounds())
	}
	return data
}

func encodedGIF(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := gif.Encode(&output, image.NewPaletted(image.Rect(0, 0, 2, 3), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatalf("encode GIF: %v", err)
	}
	return output.Bytes()
}

func ftyp(brand string) []byte {
	box := binary.BigEndian.AppendUint32(nil, 20)
	box = append(box, "ftyp"...)
	box = append(box, brand...)
	box = binary.BigEndian.AppendUint32(box, 0)
	return append(box, brand...)
}

func pngChunk(name string, payload []byte) []byte {
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	chunk = append(chunk, name...)
	chunk = append(chunk, payload...)
	return binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
}

func pngHeader(width, height uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	file := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	file = append(file, pngChunk("IHDR", ihdr)...)
	return append(file, pngChunk("IDAT", []byte{0x78, 0x9c})...)
}

func jpegHeader(width, height uint16) []byte {
	file := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}
	file = append(file, 0xff, 0xc0, 0, 17, 8)
	file = binary.BigEndian.AppendUint16(file, height)
	file = binary.BigEndian.AppendUint16(file, width)
	return append(file, 3, 1, 0x22, 0, 2, 0x11, 1, 3, 0x11, 1)
}

func webpVP8XHeader(width, height uint32) []byte {
	file := []byte("RIFF")
	file = binary.LittleEndian.AppendUint32(file, 22)
	file = append(file, "WEBPVP8X"...)
	file = binary.LittleEndian.AppendUint32(file, 10)
	file = append(file, 0, 0, 0, 0) // flags and reserved bytes
	for _, size := range []uint32{width - 1, height - 1} {
		file = append(file, byte(size), byte(size>>8), byte(size>>16))
	}
	return file // no pixel chunk
}

func expectHeaderDimensions(t *testing.T, data []byte, format string, width, height int) {
	t.Helper()
	var config image.Config
	var err error
	switch format {
	case "image/jpeg":
		config, err = jpeg.DecodeConfig(bytes.NewReader(data))
	case "image/png":
		config, err = png.DecodeConfig(bytes.NewReader(data))
	case "image/webp":
		config, err = webp.DecodeConfig(bytes.NewReader(data))
	}
	if err != nil || config.Width != width || config.Height != height {
		t.Fatalf("fixture DecodeConfig: got %dx%d, error %v, want %dx%d", config.Width, config.Height, err, width, height)
	}
}

func TestPipelineMagicBytesOverrideDeclaredContentType(t *testing.T) {
	rows := []struct {
		name, declared, wantFormat string
		data                       []byte
		width, height              int
		passThrough                bool
	}{
		{"JPEG declared PNG", "image/png", "image/jpeg", testkit.TestJPEG(48, 32), 48, 32, false},
		{"PNG declared JPEG", "image/jpeg", "image/png", testkit.TestPNG(17, 13), 17, 13, true},
		{"WebP declared JPEG", "image/jpeg", "image/webp", losslessWebP(t), 1, 1, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			result, err := ingest(t, pipeline(t, 2), row.data, row.declared, allFormats)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if string(result.Format) != row.wantFormat || result.Width != row.width || result.Height != row.height {
				t.Fatalf("result format/dimensions: got %q %dx%d, want %q %dx%d", result.Format, result.Width, result.Height, row.wantFormat, row.width, row.height)
			}
			if row.passThrough && !bytes.Equal(result.Data, row.data) {
				t.Fatalf("pass-through data differs from input")
			}
		})
	}
}

func TestPipelineUnsupportedImageFormats(t *testing.T) {
	rows := []struct {
		name   string
		data   []byte
		accept []media.Format
	}{
		{"HEIC", ftyp("heic"), allFormats},
		{"AVIF", ftyp("avif"), allFormats},
		{"SVG", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="2" height="3"><rect width="2" height="3"/></svg>`), allFormats},
		{"plain text", []byte("not an image"), allFormats},
		{"GIF", encodedGIF(t), allFormats},
		{"WebP excluded by caller", losslessWebP(t), []media.Format{media.FormatJPEG, media.FormatPNG}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := ingest(t, pipeline(t, 2), row.data, "image/jpeg", row.accept)
			expectCode(t, err, media.ErrorCodeUnsupportedImageFormat)
		})
	}
}

type pendingIngest struct {
	result media.Result
	err    error
}

func heldSlots(t *testing.T) (*media.Pipeline, context.Context, context.CancelFunc) {
	t.Helper()
	p := pipeline(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var releases []func()
	t.Cleanup(func() {
		cancel()
		for _, release := range releases {
			release()
		}
	})
	for range 2 {
		release, err := media.HoldDecodeSlot(p, ctx)
		if err != nil {
			t.Fatalf("hold decode slot: %v", err)
		}
		releases = append(releases, release)
	}
	return p, ctx, cancel
}

func startIngest(p *media.Pipeline, ctx context.Context, data []byte, accept []media.Format) (<-chan struct{}, <-chan pendingIngest) {
	started := make(chan struct{})
	done := make(chan pendingIngest, 1)
	go func() {
		close(started)
		result, err := p.Ingest(ctx, media.Input{Data: data, Accept: accept})
		done <- pendingIngest{result, err}
	}()
	return started, done
}

func waitIngest(t *testing.T, done <-chan pendingIngest) pendingIngest {
	t.Helper()
	testkit.WaitFor(t, 2*time.Second, func() (bool, error) { return len(done) != 0, nil }, testkit.WithDescription("ingest to finish while decode slots are held"))
	return <-done
}

func TestPipelineOversizedDimensionsBeforeDecodeSlot(t *testing.T) {
	rows := []struct {
		name, format  string
		data          []byte
		width, height int
		atLimit       bool
	}{
		{"PNG 10000x5001", "image/png", pngHeader(10000, 5001), 10000, 5001, false},
		{"JPEG 65500x65500", "image/jpeg", jpegHeader(65500, 65500), 65500, 65500, false},
		{"WebP VP8X 16384x16384", "image/webp", webpVP8XHeader(16384, 16384), 16384, 16384, false},
		{"PNG exactly 50 million pixels waits for slot", "image/png", pngHeader(10000, 5000), 10000, 5000, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			expectHeaderDimensions(t, row.data, row.format, row.width, row.height)
			p, ctx, cancel := heldSlots(t)
			started, done := startIngest(p, ctx, row.data, allFormats)
			if row.atLimit {
				<-started
				testkit.Holds(t, 200*time.Millisecond, func() (bool, error) { return len(done) == 0, nil }, testkit.WithDescription("50 million pixel ingest waits for a decode slot"))
				cancel()
				if result := waitIngest(t, done); !errors.Is(result.err, context.Canceled) {
					t.Fatalf("cancelled slot wait: got %v, want context.Canceled", result.err)
				}
				return
			}
			expectCode(t, waitIngest(t, done).err, media.ErrorCodeImageDimensionsTooLarge)
		})
	}
}

func TestPipelineZeroDimensionsBeforeDecodeSlot(t *testing.T) {
	rows := []struct {
		name string
		data []byte
	}{
		{"JPEG zero height", jpegHeader(64, 0)},
		{"PNG zero width", pngHeader(0, 64)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p, ctx, _ := heldSlots(t)
			_, done := startIngest(p, ctx, row.data, allFormats)
			expectCode(t, waitIngest(t, done).err, media.ErrorCodeInvalidImage)
		})
	}
}

func TestPipelineCorruptPixelsAreInvalid(t *testing.T) {
	jpegData := testkit.TestJPEG(48, 32)
	_, scan, err := testkit.WalkJPEGSegments(jpegData)
	if err != nil {
		t.Fatalf("walk encoded JPEG: %v", err)
	}
	jpegData = jpegData[:len(jpegData)-len(scan)+len(scan)/2]
	pngData := testkit.TestPNG(17, 13)
	idat := bytes.Index(pngData, []byte("IDAT"))
	if idat < 0 || idat+6 >= len(pngData) {
		t.Fatalf("encoded PNG lacks IDAT data")
	}
	pngData[idat+6] ^= 0xff // damage the compressed data, not IHDR
	rows := []struct {
		name string
		data []byte
	}{
		{"JPEG truncated scan", jpegData},
		{"PNG corrupt IDAT", pngData},
		{"WebP truncated after VP8X header", webpVP8XHeader(16, 12)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if row.name == "WebP truncated after VP8X header" {
				expectHeaderDimensions(t, row.data, "image/webp", 16, 12)
			}
			_, err := ingest(t, pipeline(t, 2), row.data, "image/jpeg", allFormats)
			expectCode(t, err, media.ErrorCodeInvalidImage)
		})
	}
}

// jfifSegment is a JFIF APP0 with an uncompressed RGB thumbnail of the given size.
func jfifSegment(thumbnailWidth, thumbnailHeight byte) []byte {
	payload := []byte("JFIF\x00\x01\x02\x01\x00\x48\x00\x48")
	payload = append(payload, thumbnailWidth, thumbnailHeight)
	return testkit.JPEGMarkerSegment(0xe0, append(payload, bytes.Repeat([]byte("THB"), int(thumbnailWidth)*int(thumbnailHeight))...))
}

// jfxxThumbnailSegment is a JFXX APP0 whose JPEG thumbnail carries its own Exif GPS.
func jfxxThumbnailSegment() []byte {
	thumbnail := testkit.JPEGWithSegments(testkit.TestJPEG(8, 8), testkit.JPEGExifGPSegment(1))
	return testkit.JPEGMarkerSegment(0xe0, append([]byte("JFXX\x00\x10"), thumbnail...))
}

// adobeSegment is an Adobe APP14 declaring the YCbCr transform Go's encoder uses.
func adobeSegment() []byte {
	return testkit.JPEGMarkerSegment(0xee, []byte("Adobe\x00\x64\x00\x00\x00\x00\x01"))
}

func multiPictureSegment() []byte {
	return testkit.JPEGMarkerSegment(0xe2, []byte("MPF\x00II*\x00\x08\x00\x00\x00"))
}

func flashPixSegment() []byte {
	return testkit.JPEGMarkerSegment(0xe2, []byte("FPXR\x00\x00\x01 flashpix contents list"))
}

func kodakMetaSegment() []byte {
	return testkit.JPEGMarkerSegment(0xe3, []byte("Meta\x00\x00II*\x00\x08\x00\x00\x00 camera serial"))
}

func goProGPMFSegment() []byte {
	return testkit.JPEGMarkerSegment(0xe6, []byte("GoPro\x00DEVC GPS5 51.5,-0.12"))
}

func jumbfC2PASegment() []byte {
	return testkit.JPEGMarkerSegment(0xeb, []byte("JP\x00\x01\x00\x00\x00\x01\x00\x00\x00\x20jumbc2pa author location"))
}

func duckySegment() []byte {
	return testkit.JPEGMarkerSegment(0xec, []byte("Ducky\x00\x01\x00\x04 photographer"))
}

func app15Segment() []byte {
	return testkit.JPEGMarkerSegment(0xef, []byte("vendor private metadata"))
}

// assertSanitizedSegments fails on any APPn or COM segment outside the metadata
// allowlist. ICC APP2 always survives. The lossless path may also keep a
// thumbnail-free JFIF APP0 and an Adobe APP14, which describe its untouched
// scan data; the re-encoded path keeps neither.
func assertSanitizedSegments(t *testing.T, input, output, icc []byte, lossless bool) {
	t.Helper()
	_, originalScan, err := testkit.WalkJPEGSegments(input)
	if err != nil {
		t.Fatalf("walk input JPEG: %v", err)
	}
	segments, outputScan, err := testkit.WalkJPEGSegments(output)
	if err != nil {
		t.Fatalf("walk output JPEG: %v", err)
	}
	iccPresent := false
	for _, segment := range segments {
		payload := segment.Bytes[4:]
		allowed := segment.Marker < 0xe0 || segment.Marker > 0xef && segment.Marker != 0xfe
		switch {
		case segment.Marker == 0xe2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")):
			allowed = true
		case lossless && segment.Marker == 0xe0 && bytes.HasPrefix(payload, []byte("JFIF\x00")):
			allowed = len(segment.Bytes) == 18 && payload[12] == 0 && payload[13] == 0
		case lossless && segment.Marker == 0xee && bytes.HasPrefix(payload, []byte("Adobe")):
			allowed = true
		}
		if !allowed {
			t.Errorf("output retains metadata marker 0x%02x: %q", segment.Marker, payload[:min(len(payload), 16)])
		}
		if bytes.Equal(segment.Bytes, icc) {
			iccPresent = true
		}
	}
	if !iccPresent {
		t.Error("output lacks byte-identical APP2 ICC segment")
	}
	if lossless && !bytes.Equal(originalScan, outputScan) {
		t.Error("SOS-to-EOI bytes changed without orientation")
	}
}

func TestPipelineSanitizesJPEGWithoutReencodingOrientationOne(t *testing.T) {
	icc := testkit.JPEGICCSegment()
	thumbnailFreeJFIF := jfifSegment(0, 0)
	rows := []struct {
		name     string
		segments [][]byte
		kept     [][]byte // the expected output's segments after SOI, in order
	}{
		{"Exif GPS XMP ICC IPTC COM", [][]byte{testkit.JPEGExifGPSegment(1), testkit.JPEGXMPSegment(), icc, testkit.JPEGIPTCSegment(), testkit.JPEGCommentSegment()}, [][]byte{icc}},
		{"APP0 JFXX thumbnail", [][]byte{jfxxThumbnailSegment(), icc}, [][]byte{icc}},
		{"APP0 JFIF thumbnail rewritten to the 16-byte form", [][]byte{jfifSegment(2, 1), icc}, [][]byte{thumbnailFreeJFIF, icc}},
		{"APP0 JFIF without thumbnail", [][]byte{thumbnailFreeJFIF, icc}, [][]byte{thumbnailFreeJFIF, icc}},
		{"APP2 MPF and FPXR", [][]byte{multiPictureSegment(), flashPixSegment(), icc}, [][]byte{icc}},
		{"APP3 Kodak Meta", [][]byte{kodakMetaSegment(), icc}, [][]byte{icc}},
		{"APP6 GoPro GPMF", [][]byte{goProGPMFSegment(), icc}, [][]byte{icc}},
		{"APP11 JUMBF C2PA", [][]byte{jumbfC2PASegment(), icc}, [][]byte{icc}},
		{"APP12 and APP15", [][]byte{duckySegment(), app15Segment(), icc}, [][]byte{icc}},
		{"APP14 Adobe", [][]byte{adobeSegment(), icc}, [][]byte{adobeSegment(), icc}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			base := testkit.TestJPEG(48, 32)
			input := testkit.JPEGWithSegments(base, row.segments...)
			result, err := ingest(t, pipeline(t, 2), input, "image/jpeg", allFormats)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != "image/jpeg" || result.Width != 48 || result.Height != 32 {
				t.Fatalf("result: got %q %dx%d, want image/jpeg 48x32", result.Format, result.Width, result.Height)
			}
			assertSanitizedSegments(t, input, result.Data, icc, true)
			if want := testkit.JPEGWithSegments(base, row.kept...); !bytes.Equal(result.Data, want) {
				t.Errorf("output: got %d bytes, want the input with exactly the non-allowlisted segments removed (%d bytes)", len(result.Data), len(want))
			}
			decoded, err := jpeg.Decode(bytes.NewReader(result.Data))
			if err != nil {
				t.Fatalf("decode sanitized JPEG: %v", err)
			}
			if decoded.Bounds() != image.Rect(0, 0, 48, 32) {
				t.Errorf("decoded bounds: got %v, want 48x32", decoded.Bounds())
			}
		})
	}
}

var (
	red    = color.RGBA{R: 255, A: 255}
	green  = color.RGBA{G: 255, A: 255}
	blue   = color.RGBA{B: 255, A: 255}
	yellow = color.RGBA{R: 255, G: 255, A: 255}
)

func quadrantJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := range 32 {
		for x := range 64 {
			chosen := red
			switch {
			case x >= 32 && y < 16:
				chosen = green
			case x < 32 && y >= 16:
				chosen = blue
			case x >= 32 && y >= 16:
				chosen = yellow
			}
			img.Set(x, y, chosen)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode quadrant JPEG: %v", err)
	}
	return encoded.Bytes()
}

func assertQuadrants(t *testing.T, img image.Image, expected [4]color.RGBA) {
	t.Helper()
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	centres := [4]image.Point{{w / 4, h / 4}, {3 * w / 4, h / 4}, {w / 4, 3 * h / 4}, {3 * w / 4, 3 * h / 4}}
	for i, point := range centres {
		r, g, b, _ := img.At(point.X, point.Y).RGBA()
		got := [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
		want := [3]int{int(expected[i].R), int(expected[i].G), int(expected[i].B)}
		for channel := range got {
			if diff := got[channel] - want[channel]; diff < -60 || diff > 60 {
				t.Errorf("quadrant %d channel %d at %v: got %d, want %d (±60)", i, channel, point, got[channel], want[channel])
			}
		}
	}
}

func TestPipelineAppliesEveryEXIFOrientation(t *testing.T) {
	rows := []struct {
		orientation   uint16
		width, height int
		quadrants     [4]color.RGBA // TL, TR, BL, BR after display transform
	}{
		{2, 64, 32, [4]color.RGBA{green, red, yellow, blue}},
		{3, 64, 32, [4]color.RGBA{yellow, blue, green, red}},
		{4, 64, 32, [4]color.RGBA{blue, yellow, red, green}},
		{5, 32, 64, [4]color.RGBA{red, blue, green, yellow}},
		{6, 32, 64, [4]color.RGBA{blue, red, yellow, green}},
		{7, 32, 64, [4]color.RGBA{yellow, green, blue, red}},
		{8, 32, 64, [4]color.RGBA{green, yellow, red, blue}},
	}
	source := quadrantJPEG(t)
	for _, row := range rows {
		t.Run(fmt.Sprintf("orientation %d", row.orientation), func(t *testing.T) {
			icc := testkit.JPEGICCSegment()
			input := testkit.JPEGWithSegments(source, jfifSegment(2, 1), testkit.JPEGExifGPSegment(row.orientation), icc,
				testkit.JPEGIPTCSegment(), testkit.JPEGCommentSegment(), jfxxThumbnailSegment(), multiPictureSegment(), kodakMetaSegment(),
				goProGPMFSegment(), jumbfC2PASegment(), duckySegment(), adobeSegment(), app15Segment())
			result, err := ingest(t, pipeline(t, 2), input, "image/jpeg", allFormats)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != "image/jpeg" || result.Width != row.width || result.Height != row.height {
				t.Fatalf("result: got %q %dx%d, want image/jpeg %dx%d", result.Format, result.Width, result.Height, row.width, row.height)
			}
			assertSanitizedSegments(t, input, result.Data, icc, false)
			decoded, err := jpeg.Decode(bytes.NewReader(result.Data))
			if err != nil {
				t.Fatalf("decode oriented JPEG: %v", err)
			}
			if decoded.Bounds() != image.Rect(0, 0, row.width, row.height) {
				t.Fatalf("decoded bounds: got %v, want %dx%d", decoded.Bounds(), row.width, row.height)
			}
			assertQuadrants(t, decoded, row.quadrants)
		})
	}
}

func TestPipelineRejectsSanitizedJPEGOverTenMegabytes(t *testing.T) {
	rows := []struct {
		name  string
		count int
	}{
		{"ICC profile chunks survive sanitation", 153},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			segments := append([][]byte{testkit.JPEGExifGPSegment(1)}, testkit.JPEGICCPaddingSegments(row.count)...)
			input := testkit.JPEGWithSegments(testkit.TestJPEG(48, 32), segments...)
			if len(input)-len(segments[0]) <= 10_000_000 {
				t.Fatalf("ICC-padded fixture should exceed 10,000,000 bytes after sanitation")
			}
			_, err := ingest(t, pipeline(t, 2), input, "image/jpeg", allFormats)
			expectCode(t, err, media.ErrorCodeImageTooLarge)
		})
	}
}
