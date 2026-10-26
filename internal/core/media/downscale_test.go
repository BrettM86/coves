package media_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"slices"
	"testing"
	"time"

	"golang.org/x/image/webp"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

// downscalePipeline stores uploads with a long edge of at most 120 pixels, so
// fixtures stay small.
func downscalePipeline(t *testing.T) *media.Pipeline {
	t.Helper()
	p, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: 120})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// pipelineWithMaxLongEdge is a pipeline with the given decode slots that
// stores uploads with a long edge of at most maxLongEdge pixels.
func pipelineWithMaxLongEdge(t *testing.T, slots, maxLongEdge int) *media.Pipeline {
	t.Helper()
	p, err := media.NewPipeline(media.Config{MaxConcurrent: slots, MaxLongEdge: maxLongEdge})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// quadrantImage is red (top left), green or fully transparent (top right),
// blue (bottom left) and yellow (bottom right), split at width/2 and height/2.
func quadrantImage(width, height int, transparentTopRight bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			chosen := color.NRGBA(red)
			switch {
			case x >= width/2 && y < height/2 && transparentTopRight:
				chosen = color.NRGBA{}
			case x >= width/2 && y < height/2:
				chosen = color.NRGBA(green)
			case x < width/2 && y >= height/2:
				chosen = color.NRGBA(blue)
			case x >= width/2 && y >= height/2:
				chosen = color.NRGBA(yellow)
			}
			img.SetNRGBA(x, y, chosen)
		}
	}
	return img
}

// quadrantJPEGOfSize encodes at quality 100, whose quantization table is all
// ones, so an untouched input is never mistaken for a quality-90 re-encode.
func quadrantJPEGOfSize(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, quadrantImage(width, height, false), &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode %dx%d quadrant JPEG: %v", width, height, err)
	}
	return encoded.Bytes()
}

func jpegICCProfileSegment(profile []byte) []byte {
	return testkit.JPEGMarkerSegment(0xe2, append([]byte("ICC_PROFILE\x00\x01\x01"), profile...))
}

// quadrantPNGWithMetadata is a 200x300 portrait PNG of quadrantImage with a
// transparent top-right quadrant, an iCCP chunk holding profile and a tEXt
// chunk, both inserted after IHDR.
func quadrantPNGWithMetadata(t *testing.T, profile []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, quadrantImage(200, 300, true)); err != nil {
		t.Fatalf("encode quadrant PNG: %v", err)
	}
	base := encoded.Bytes()
	const ihdrEnd = 8 + 12 + 13
	if len(base) < ihdrEnd || string(base[12:16]) != "IHDR" {
		t.Fatal("encoded PNG does not start with IHDR")
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(profile); err != nil {
		t.Fatalf("compress ICC profile: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("compress ICC profile: %v", err)
	}
	iccp := pngChunk("iCCP", append([]byte("test profile\x00\x00"), compressed.Bytes()...))
	text := pngChunk("tEXt", []byte("Author\x00person"))
	return bytes.Join([][]byte{base[:ihdrEnd], iccp, text, base[ihdrEnd:]}, nil)
}

// Lossless WebPs of quadrantImage generated once with PIL 12.3.0; webpFixture
// checks the decoded pixels so a fixture cannot silently become another image.
const (
	opaqueQuadrantWebP300x200Base64      = "UklGRkIAAABXRUJQVlA4TDUAAAAvK8ExACCTtpl/03Owa5D5n//+T0AatYzKf/zHf/zHf/zHf1cgjtpG5T/+4z/+4z/+478rAAA="
	transparentQuadrantWebP300x200Base64 = testkit.TransparentQuadrantWebP300x200Base64
	opaqueQuadrantWebP120x80Base64       = "UklGRjAAAABXRUJQVlA4TCMAAAAvd8ATAFATMA2Df9MIyAcd8z///Z8AhTbo91+QSLRDv/+CBAA="
)

// webpFixtureImageChunk decodes a base64 WebP fixture, checks its size and
// quadrant colours and alpha, and returns its VP8L chunk.
func webpFixtureImageChunk(t *testing.T, encoded string, width, height int, transparentTopRight bool) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode %dx%d WebP fixture: %v", width, height, err)
	}
	decoded, err := webp.Decode(bytes.NewReader(data))
	if err != nil || imageBounds(decoded) != image.Rect(0, 0, width, height) {
		t.Fatalf("%dx%d WebP fixture: bounds %v, error %v", width, height, imageBounds(decoded), err)
	}
	if transparentTopRight {
		assertQuadrantAlpha(t, decoded, [4]uint32{255, 0, 255, 255})
		assertQuadrants(t, decoded, [4]color.RGBA{red, {}, blue, yellow})
	} else {
		assertQuadrantAlpha(t, decoded, [4]uint32{255, 255, 255, 255})
		assertQuadrants(t, decoded, [4]color.RGBA{red, green, blue, yellow})
	}
	return webpImageChunk(t, data)
}

// quality90Luminance is the IJG standard luminance table (JPEG Annex K.1)
// scaled to quality 90, in the zigzag order a DQT segment stores it.
var quality90Luminance = []byte{
	3, 2, 2, 3, 2, 2, 3, 3, 3, 3, 4, 3, 3, 4, 5, 8,
	5, 5, 4, 4, 5, 10, 7, 7, 6, 8, 12, 10, 12, 12, 11, 10,
	11, 11, 13, 14, 18, 16, 13, 14, 17, 14, 11, 11, 16, 22, 16, 17,
	19, 20, 21, 21, 21, 12, 15, 23, 24, 22, 20, 24, 18, 20, 21, 20,
}

// jpegLuminanceQuantization returns the 8-bit quantization table with
// destination 0 from the JPEG's DQT segments.
func jpegLuminanceQuantization(t *testing.T, data []byte) []byte {
	t.Helper()
	segments, _, err := testkit.WalkJPEGSegments(data)
	if err != nil {
		t.Fatalf("parse JPEG: %v", err)
	}
	for _, segment := range segments {
		if segment.Marker != 0xdb {
			continue
		}
		for tables := segment.Bytes[4:]; len(tables) > 0; {
			precision, destination := tables[0]>>4, tables[0]&0x0f
			size := 64 << precision
			if 1+size > len(tables) {
				t.Fatal("truncated DQT table")
			}
			if destination == 0 {
				if precision != 0 {
					t.Fatalf("luminance quantization table precision: got %d, want 8-bit", precision)
				}
				return tables[1 : 1+size]
			}
			tables = tables[1+size:]
		}
	}
	t.Fatal("JPEG has no luminance quantization table")
	return nil
}

// assertJPEGMetadataIsOnlyICC fails on any APPn or COM segment other than an
// APP2 ICC_PROFILE chunk, so no EXIF (APP1) can survive.
func assertJPEGMetadataIsOnlyICC(t *testing.T, data []byte) {
	t.Helper()
	segments, _, err := testkit.WalkJPEGSegments(data)
	if err != nil {
		t.Fatalf("parse JPEG: %v", err)
	}
	for _, segment := range segments {
		metadata := segment.Marker >= 0xe0 && segment.Marker <= 0xef || segment.Marker == 0xfe
		if metadata && (segment.Marker != 0xe2 || !bytes.HasPrefix(segment.Bytes[4:], []byte("ICC_PROFILE\x00"))) {
			t.Errorf("stored JPEG retains metadata marker 0x%02x: %q", segment.Marker, segment.Bytes[4:min(len(segment.Bytes), 20)])
		}
	}
	if bytes.Contains(data, []byte("Exif\x00")) {
		t.Error("stored JPEG contains Exif")
	}
}

// pngChunkTypes lists the PNG's chunk types in order.
func pngChunkTypes(t *testing.T, data []byte) []string {
	t.Helper()
	var types []string
	for offset := 8; offset < len(data); {
		if offset+12 > len(data) {
			t.Fatal("truncated PNG chunk")
		}
		end := offset + 12 + int(binary.BigEndian.Uint32(data[offset:]))
		if end > len(data) {
			t.Fatal("PNG chunk exceeds output")
		}
		types = append(types, string(data[offset+4:offset+8]))
		offset = end
	}
	return types
}

func upload(t *testing.T, p *media.Pipeline, data []byte, declared string) (media.UploadedImage, *recordingUploader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uploader := &recordingUploader{}
	uploaded, err := p.Upload(ctx, uploader, media.Input{Data: data, DeclaredContentType: declared})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if uploader.data == nil {
		t.Fatal("upload stored nothing")
	}
	return uploaded, uploader
}

// decodeStored decodes the stored bytes with the decoder for wantMimeType and
// checks that the uploader and the UploadedImage both describe those bytes.
func decodeStored(t *testing.T, uploaded media.UploadedImage, uploader *recordingUploader, wantMimeType string) image.Image {
	t.Helper()
	if uploader.mimeType != wantMimeType || uploaded.MimeType != wantMimeType {
		t.Fatalf("MIME type: stored as %q, reported %q, want %q", uploader.mimeType, uploaded.MimeType, wantMimeType)
	}
	var decoded image.Image
	var err error
	switch wantMimeType {
	case "image/jpeg":
		decoded, err = jpeg.Decode(bytes.NewReader(uploader.data))
	case "image/png":
		decoded, err = png.Decode(bytes.NewReader(uploader.data))
	case "image/webp":
		decoded, err = webp.Decode(bytes.NewReader(uploader.data))
	}
	if err != nil {
		t.Fatalf("decode stored %s: %v", wantMimeType, err)
	}
	if uploaded.Size != len(uploader.data) {
		t.Errorf("Size: got %d, want the %d stored bytes", uploaded.Size, len(uploader.data))
	}
	bounds := decoded.Bounds()
	if uploaded.AspectRatio != (media.AspectRatio{Width: bounds.Dx(), Height: bounds.Dy()}) {
		t.Errorf("AspectRatio: got %dx%d, want the stored %dx%d", uploaded.AspectRatio.Width, uploaded.AspectRatio.Height, bounds.Dx(), bounds.Dy())
	}
	return decoded
}

func TestUploadDownscalesImagesOverTheLongEdgeLimit(t *testing.T) {
	profile := iccTestProfile(3000)
	jpegWithMetadata := func(width, height int, orientation uint16) []byte {
		return testkit.JPEGWithSegments(quadrantJPEGOfSize(t, width, height),
			testkit.JPEGExifGPSegment(orientation), testkit.JPEGXMPSegment(), jpegICCProfileSegment(profile), testkit.JPEGCommentSegment())
	}
	opaqueWebP := webpFixtureImageChunk(t, opaqueQuadrantWebP300x200Base64, 300, 200, false)
	transparentWebP := webpFixtureImageChunk(t, transparentQuadrantWebP300x200Base64, 300, 200, true)
	upright := [4]color.RGBA{red, green, blue, yellow}
	rotated := [4]color.RGBA{blue, red, yellow, green} // orientation 6
	transparent := [4]color.RGBA{red, {}, blue, yellow}
	var plainPNG bytes.Buffer
	if err := png.Encode(&plainPNG, quadrantImage(200, 300, true)); err != nil {
		t.Fatalf("encode plain quadrant PNG: %v", err)
	}
	rows := []struct {
		name         string
		input        []byte
		inputFormat  string
		inputW       int
		inputH       int
		wantMimeType string
		width        int
		heights      []int          // accepted stored heights
		quadrants    *[4]color.RGBA // nil when the image is too thin to have quadrants
		alpha        *[4]uint32     // nil for JPEG output
		profile      []byte         // the ICC profile the output must carry; nil for none
	}{
		{"JPEG 300x200", jpegWithMetadata(300, 200, 1), "image/jpeg", 300, 200, "image/jpeg", 120, []int{80}, &upright, nil, profile},
		{"JPEG 121x80 one pixel over", jpegWithMetadata(121, 80, 1), "image/jpeg", 121, 80, "image/jpeg", 120, []int{79, 80}, &upright, nil, profile},
		{"JPEG stored 300x200 with EXIF orientation 6", jpegWithMetadata(300, 200, 6), "image/jpeg", 300, 200, "image/jpeg", 80, []int{120}, &rotated, nil, profile},
		{"JPEG stored 300x200 with EXIF orientation 2", jpegWithMetadata(300, 200, 2), "image/jpeg", 300, 200, "image/jpeg", 120, []int{80}, &[4]color.RGBA{green, red, yellow, blue}, nil, profile},
		{"JPEG stored 300x200 with EXIF orientation 3", jpegWithMetadata(300, 200, 3), "image/jpeg", 300, 200, "image/jpeg", 120, []int{80}, &[4]color.RGBA{yellow, blue, green, red}, nil, profile},
		{"JPEG stored 300x200 with EXIF orientation 4", jpegWithMetadata(300, 200, 4), "image/jpeg", 300, 200, "image/jpeg", 120, []int{80}, &[4]color.RGBA{blue, yellow, red, green}, nil, profile},
		{"JPEG stored 300x200 with EXIF orientation 5", jpegWithMetadata(300, 200, 5), "image/jpeg", 300, 200, "image/jpeg", 80, []int{120}, &[4]color.RGBA{red, blue, green, yellow}, nil, profile},
		{"JPEG stored 300x200 with EXIF orientation 7", jpegWithMetadata(300, 200, 7), "image/jpeg", 300, 200, "image/jpeg", 80, []int{120}, &[4]color.RGBA{yellow, green, blue, red}, nil, profile},
		{"JPEG stored 300x200 with EXIF orientation 8", jpegWithMetadata(300, 200, 8), "image/jpeg", 300, 200, "image/jpeg", 80, []int{120}, &[4]color.RGBA{green, yellow, red, blue}, nil, profile},
		{"portrait JPEG 100x300 with the short edge within the limit", jpegWithMetadata(100, 300, 1), "image/jpeg", 100, 300, "image/jpeg", 40, []int{120}, &upright, nil, profile},
		{"JPEG stored 300x100 with EXIF orientation 6 displays as portrait 100x300", jpegWithMetadata(300, 100, 6), "image/jpeg", 300, 100, "image/jpeg", 40, []int{120}, &rotated, nil, profile},
		{"JPEG 600x2 keeps a one-pixel short edge", jpegWithMetadata(600, 2, 1), "image/jpeg", 600, 2, "image/jpeg", 120, []int{1}, nil, nil, profile},
		{"PNG 200x300 with transparency, iCCP and tEXt", quadrantPNGWithMetadata(t, profile), "image/png", 200, 300, "image/png", 80, []int{120}, &transparent, &[4]uint32{255, 0, 255, 255}, profile},
		{"PNG 200x300 with transparency and no iCCP", plainPNG.Bytes(), "image/png", 200, 300, "image/png", 80, []int{120}, &transparent, &[4]uint32{255, 0, 255, 255}, nil},
		{"opaque WebP 300x200 with ICCP to JPEG", webpContainer(webpVP8X(300, 200, 0x20), webpChunk("ICCP", profile), opaqueWebP), "image/webp", 300, 200, "image/jpeg", 120, []int{80}, &upright, nil, profile},
		{"opaque WebP 300x200 with ICCP and EXIF orientation 6 to upright JPEG", webpContainer(webpVP8X(300, 200, 0x28), webpChunk("ICCP", profile), opaqueWebP, webpChunk("EXIF", webpEXIF(6, true, false))), "image/webp", 300, 200, "image/jpeg", 80, []int{120}, &rotated, nil, profile},
		{"WebP 300x200 with transparency and ICCP to PNG", webpContainer(webpVP8X(300, 200, 0x30), webpChunk("ICCP", profile), transparentWebP), "image/webp", 300, 200, "image/png", 120, []int{80}, &transparent, &[4]uint32{255, 0, 255, 255}, profile},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			expectHeaderDimensions(t, row.input, row.inputFormat, row.inputW, row.inputH)

			uploaded, uploader := upload(t, downscalePipeline(t), row.input, row.inputFormat)

			decoded := decodeStored(t, uploaded, uploader, row.wantMimeType)
			bounds := decoded.Bounds()
			if bounds.Dx() != row.width || !slices.Contains(row.heights, bounds.Dy()) {
				t.Errorf("stored dimensions: got %dx%d, want width %d and height in %v", bounds.Dx(), bounds.Dy(), row.width, row.heights)
			}
			if row.quadrants != nil {
				assertQuadrants(t, decoded, *row.quadrants)
			}
			if row.alpha != nil {
				assertQuadrantAlpha(t, decoded, *row.alpha)
			}
			switch row.wantMimeType {
			case "image/jpeg":
				if got := jpegICCProfile(t, uploader.data); !bytes.Equal(got, row.profile) {
					t.Errorf("APP2 ICC profile: got %d bytes, want the %d input profile bytes", len(got), len(row.profile))
				}
				assertJPEGMetadataIsOnlyICC(t, uploader.data)
				if got := jpegLuminanceQuantization(t, uploader.data); !bytes.Equal(got, quality90Luminance) {
					t.Errorf("luminance quantization table: got %v, want the quality-90 table %v", got, quality90Luminance)
				}
			case "image/png":
				got, beforeImageData := pngICCProfile(t, uploader.data)
				if row.profile == nil {
					if slices.Contains(pngChunkTypes(t, uploader.data), "iCCP") {
						t.Error("stored PNG has an iCCP chunk the input lacked")
					}
				} else if !bytes.Equal(got, row.profile) {
					t.Errorf("iCCP profile: got %d bytes, want the %d input profile bytes", len(got), len(row.profile))
				} else if !beforeImageData {
					t.Error("iCCP chunk must precede IDAT")
				}
				for _, chunkType := range pngChunkTypes(t, uploader.data) {
					if slices.Contains([]string{"tEXt", "iTXt", "zTXt", "eXIf", "tIME"}, chunkType) {
						t.Errorf("stored PNG retains %s chunk", chunkType)
					}
				}
			}
		})
	}
}

func TestUploadKeepsImagesAtTheLongEdgeLimitLossless(t *testing.T) {
	t.Run("JPEG 120x80 keeps its scan data", func(t *testing.T) {
		icc := jpegICCProfileSegment(iccTestProfile(3000))
		input := testkit.JPEGWithSegments(quadrantJPEGOfSize(t, 120, 80), testkit.JPEGExifGPSegment(1), icc)

		uploaded, uploader := upload(t, downscalePipeline(t), input, "image/jpeg")

		decoded := decodeStored(t, uploaded, uploader, "image/jpeg")
		if decoded.Bounds() != image.Rect(0, 0, 120, 80) {
			t.Errorf("stored dimensions: got %v, want 120x80", decoded.Bounds())
		}
		assertSanitizedSegments(t, input, uploader.data, icc, true)
	})
	t.Run("WebP 120x80 with EXIF orientation 1 stays WebP", func(t *testing.T) {
		imageChunk := webpFixtureImageChunk(t, opaqueQuadrantWebP120x80Base64, 120, 80, false)
		input := webpContainer(webpVP8X(120, 80, 0x08), imageChunk, webpChunk("EXIF", webpEXIF(1, true, false)))

		uploaded, uploader := upload(t, downscalePipeline(t), input, "image/webp")

		decoded := decodeStored(t, uploaded, uploader, "image/webp")
		if decoded.Bounds() != image.Rect(0, 0, 120, 80) {
			t.Errorf("stored dimensions: got %v, want 120x80", decoded.Bounds())
		}
		if !bytes.Contains(uploader.data, imageChunk) {
			t.Error("stored WebP lacks the input's unchanged VP8L chunk")
		}
	})
}

// Ingest alone serves avatars and banners, which are never downscaled.
func TestIngestDoesNotDownscaleOversizedImages(t *testing.T) {
	input := testkit.JPEGWithSegments(quadrantJPEGOfSize(t, 300, 200), testkit.JPEGExifGPSegment(1))

	result, err := ingest(t, downscalePipeline(t), input, "image/jpeg", []media.Format{media.FormatJPEG})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if result.Format != media.FormatJPEG || result.Width != 300 || result.Height != 200 {
		t.Fatalf("result: got %q %dx%d, want image/jpeg 300x200", result.Format, result.Width, result.Height)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(result.Data))
	if err != nil || imageBounds(decoded) != image.Rect(0, 0, 300, 200) {
		t.Fatalf("decode ingested JPEG: bounds %v, error %v", imageBounds(decoded), err)
	}
}

func TestUploadKeepsAnimatedGIFsOverTheLongEdgeLimit(t *testing.T) {
	dirty, _ := testkit.AnimatedGIFWithMetadata()
	if config, err := gif.DecodeConfig(bytes.NewReader(dirty)); err != nil || config.Width != 4 || config.Height != 3 {
		t.Fatalf("GIF fixture logical screen: %dx%d, error %v; want 4x3", config.Width, config.Height, err)
	}

	uploaded, uploader := upload(t, pipelineWithMaxLongEdge(t, 1, 2), dirty, "image/gif")

	if uploader.mimeType != "image/gif" || uploaded.MimeType != "image/gif" {
		t.Fatalf("MIME type: stored as %q, reported %q, want image/gif", uploader.mimeType, uploaded.MimeType)
	}
	decoded, err := gif.DecodeAll(bytes.NewReader(uploader.data))
	if err != nil {
		t.Fatalf("decode stored GIF: %v", err)
	}
	if decoded.Config.Width != 4 || decoded.Config.Height != 3 {
		t.Errorf("stored logical screen: got %dx%d, want 4x3", decoded.Config.Width, decoded.Config.Height)
	}
	if len(decoded.Image) != 3 {
		t.Errorf("stored frames: got %d, want 3", len(decoded.Image))
	}
	if uploaded.AspectRatio != (media.AspectRatio{Width: 4, Height: 3}) || uploaded.Size != len(uploader.data) {
		t.Errorf("uploaded image: aspect ratio %dx%d size %d, want 4x3 and the %d stored bytes", uploaded.AspectRatio.Width, uploaded.AspectRatio.Height, uploaded.Size, len(uploader.data))
	}
}

func TestUploadRefusesDownscaledReEncodeOverTenMegabytes(t *testing.T) {
	input := testkit.JPEGThatGrowsPastTenMegabytesWhenDownscaled()
	if len(input) > 10_000_000 {
		t.Fatalf("fixture input is %d bytes, want at most 10,000,000", len(input))
	}
	expectHeaderDimensions(t, input, "image/jpeg", 1024, 1024)
	uploader := &recordingUploader{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := pipelineWithMaxLongEdge(t, 1, 1000).Upload(ctx, uploader, media.Input{Data: input, DeclaredContentType: "image/jpeg"})

	expectCode(t, err, media.ErrorCodeImageTooLarge)
	if uploader.uploads != 0 {
		t.Errorf("uploads: got %d, want 0", uploader.uploads)
	}
}

// The rotated re-encode of this fixture alone is over 10,000,000 bytes, so
// the size limit must apply to the downscaled output, not an intermediate.
func TestUploadDownscalesARotatedImageWhoseFullSizeReEncodeIsTooLarge(t *testing.T) {
	input := testkit.JPEGThatGrowsPastTenMegabytesWhenRotated()

	uploaded, uploader := upload(t, pipelineWithMaxLongEdge(t, 1, 64), input, "image/jpeg")

	if uploader.uploads != 1 {
		t.Errorf("uploads: got %d, want 1", uploader.uploads)
	}
	decoded := decodeStored(t, uploaded, uploader, "image/jpeg")
	if decoded.Bounds() != image.Rect(0, 0, 64, 64) {
		t.Errorf("stored dimensions: got %v, want 64x64", decoded.Bounds())
	}
	assertJPEGMetadataIsOnlyICC(t, uploader.data)
	// 147 padding chunks of 65,519 zero bytes each.
	if got := jpegICCProfile(t, uploader.data); !bytes.Equal(got, make([]byte, 9_631_293)) {
		t.Errorf("APP2 ICC profile: got %d bytes, want the input's 9,631,293 zero bytes", len(got))
	}
}

func TestPipelineRejectsNonpositiveMaxLongEdge(t *testing.T) {
	_, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: 0})
	if !errors.Is(err, media.ErrInvalidConfig) {
		t.Fatalf("NewPipeline(MaxLongEdge 0): error %v, want ErrInvalidConfig", err)
	}
}
