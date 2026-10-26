package media_test

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"testing"

	"golang.org/x/image/webp"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

func webpChunk(name string, payload []byte) []byte {
	chunk := append([]byte(name), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(chunk[4:], uint32(len(payload)))
	chunk = append(chunk, payload...)
	if len(payload)%2 != 0 {
		chunk = append(chunk, 0)
	}
	return chunk
}

func webpContainer(chunks ...[]byte) []byte {
	data := []byte("RIFF\x00\x00\x00\x00WEBP")
	for _, chunk := range chunks {
		data = append(data, chunk...)
	}
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	return data
}

func webpImageChunk(t *testing.T, data []byte) []byte {
	t.Helper()
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatal("WebP fixture lacks RIFF/WEBP header")
	}
	chunk := data[12:]
	if len(chunk) < 8 || int(binary.LittleEndian.Uint32(chunk[4:8]))+8 > len(chunk) {
		t.Fatal("WebP fixture lacks complete image chunk")
	}
	return chunk
}

func webpVP8X(width, height int, flags byte) []byte {
	payload := []byte{flags, 0, 0, 0, byte(width - 1), byte((width - 1) >> 8), byte((width - 1) >> 16), byte(height - 1), byte((height - 1) >> 8), byte((height - 1) >> 16)}
	return webpChunk("VP8X", payload)
}

func webpEXIF(orientation uint16, includeOrientation, prefixed bool) []byte {
	// Little-endian TIFF with an orientation entry (when requested) and a
	// second entry pointing at a GPS IFD. The latter must never reach output.
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	count := uint16(1)
	if includeOrientation {
		count++
	}
	tiff = binary.LittleEndian.AppendUint16(tiff, count)
	if includeOrientation {
		tiff = binary.LittleEndian.AppendUint16(tiff, 0x0112)
		tiff = binary.LittleEndian.AppendUint16(tiff, 3)
		tiff = binary.LittleEndian.AppendUint32(tiff, 1)
		tiff = binary.LittleEndian.AppendUint16(tiff, orientation)
		tiff = append(tiff, 0, 0)
	}
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x8825)
	tiff = binary.LittleEndian.AppendUint16(tiff, 4)
	tiff = binary.LittleEndian.AppendUint32(tiff, 1)
	tiff = binary.LittleEndian.AppendUint32(tiff, uint32(8+2+int(count)*12+4))
	tiff = binary.LittleEndian.AppendUint32(tiff, 0)
	tiff = binary.LittleEndian.AppendUint16(tiff, 0)
	tiff = binary.LittleEndian.AppendUint32(tiff, 0)
	if prefixed {
		tiff = append([]byte("Exif\x00\x00"), tiff...)
	}
	return tiff
}

func TestPipelineSanitizesWebPMetadataLosslessly(t *testing.T) {
	imageChunk := webpImageChunk(t, losslessWebP(t))
	icc := webpChunk("ICCP", []byte("colour profile"))
	retained := webpChunk("JUNK", []byte("kept"))
	xmp := webpChunk("XMP ", []byte("private caption"))
	contentCredentials := webpChunk("C2PA", []byte("jumbc2pa private manifest"))
	rows := []struct {
		name               string
		includeOrientation bool
	}{
		{"orientation 1", true},
		{"no orientation entry", false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			exifPayload := webpEXIF(1, row.includeOrientation, false)
			if len(exifPayload)%2 == 0 {
				exifPayload = append(exifPayload, 0) // removed odd chunk with a RIFF pad byte
			}
			exif := webpChunk("EXIF", exifPayload)
			input := webpContainer(webpVP8X(1, 1, 0x2c), icc, imageChunk, exif, retained, xmp, contentCredentials)
			expected := webpContainer(webpVP8X(1, 1, 0x20), icc, imageChunk, retained)
			if decoded, err := webp.Decode(bytes.NewReader(expected)); err != nil || imageBounds(decoded) != image.Rect(0, 0, 1, 1) {
				t.Fatalf("expected sanitized WebP fixture: bounds %v, error %v", imageBounds(decoded), err)
			}
			if _, err := webp.Decode(bytes.NewReader(input)); err != nil {
				t.Fatalf("extended WebP fixture must decode: %v", err)
			}
			result, err := ingest(t, pipeline(t, 1), input, "image/webp", []media.Format{media.FormatWebP})
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != media.FormatWebP || result.Width != 1 || result.Height != 1 {
				t.Fatalf("result: got %q %dx%d, want image/webp 1x1", result.Format, result.Width, result.Height)
			}
			if bytes.Contains(result.Data, []byte("C2PA")) {
				t.Error("WebP output retains C2PA chunk")
			}
			if !bytes.Equal(result.Data, expected) {
				t.Errorf("WebP output differs from expected retained chunks: got %d bytes, want %d", len(result.Data), len(expected))
			}
			decoded, err := webp.Decode(bytes.NewReader(result.Data))
			if err != nil || decoded.Bounds() != image.Rect(0, 0, 1, 1) {
				t.Errorf("decode sanitized WebP: bounds %v, error %v", imageBounds(decoded), err)
			}
		})
	}
}

func imageBounds(img image.Image) image.Rectangle {
	if img == nil {
		return image.Rectangle{}
	}
	return img.Bounds()
}

func TestPipelineValidatesWebPContainerFraming(t *testing.T) {
	imageChunk := webpImageChunk(t, losslessWebP(t))
	exif := webpChunk("EXIF", webpEXIF(1, true, false))
	xmp := webpChunk("XMP ", []byte("private"))
	valid := webpContainer(webpVP8X(1, 1, 0x0c), imageChunk, exif, xmp)
	malformedChunk := webpChunk("JUNK", []byte("chunk longer than the RIFF extent"))
	brokenExtent := webpContainer(webpVP8X(1, 1, 0x0c), imageChunk, malformedChunk)
	binary.LittleEndian.PutUint32(brokenExtent[len(brokenExtent)-len(malformedChunk)+4:], uint32(len(malformedChunk)+20))
	trailerCoveredExtent := webpContainer(webpVP8X(1, 1, 0x0c), imageChunk, malformedChunk)
	binary.LittleEndian.PutUint32(trailerCoveredExtent[4:8], uint32(len(trailerCoveredExtent)-8-8))
	oversizedRIFF := append([]byte(nil), valid...)
	binary.LittleEndian.PutUint32(oversizedRIFF[4:8], uint32(len(oversizedRIFF)+20))
	rows := []struct {
		name  string
		input []byte
		want  []byte
	}{
		{"trailing bytes after RIFF", append(append([]byte(nil), valid...), []byte("private trailer")...), webpContainer(webpVP8X(1, 1, 0), imageChunk)},
		{"chunk length past RIFF extent", brokenExtent, nil},
		{"chunk length past RIFF extent with trailing bytes covering chunk", trailerCoveredExtent, nil},
		{"RIFF size past input", oversizedRIFF, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			result, err := ingest(t, pipeline(t, 1), row.input, "image/webp", []media.Format{media.FormatWebP})
			if row.want == nil {
				expectCode(t, err, media.ErrorCodeInvalidImage)
				return
			}
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != media.FormatWebP || result.Width != 1 || result.Height != 1 || !bytes.Equal(result.Data, row.want) {
				t.Errorf("framed WebP: got %q %dx%d (%d bytes), want image/webp 1x1 (%d bytes)", result.Format, result.Width, result.Height, len(result.Data), len(row.want))
			}
		})
	}
}

// Lossless 32x16 quadrants generated once with PIL; check its decoded pixels
// so the fixture cannot silently become a different image.
const quadrantWebPBase64 = "UklGRjgAAABXRUJQVlA4TCwAAAAvH8ADAB8gEEjaH3qN+RcQFPk/2vwHXySAINtmkpOc5AEuENH/2DbVH0DVDw=="

func quadrantWebP(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(quadrantWebPBase64)
	if err != nil {
		t.Fatalf("decode quadrant WebP fixture: %v", err)
	}
	decoded, err := webp.Decode(bytes.NewReader(data))
	if err != nil || imageBounds(decoded) != image.Rect(0, 0, 32, 16) {
		t.Fatalf("quadrant WebP fixture: bounds %v, error %v", imageBounds(decoded), err)
	}
	assertQuadrants(t, decoded, [4]color.RGBA{red, green, blue, yellow})
	return data
}

func TestPipelineOrientsWebPToMetadataFreeJPEG(t *testing.T) {
	imageChunk := webpImageChunk(t, quadrantWebP(t))
	rows := []struct {
		name          string
		orientation   uint16
		prefixed      bool
		accept        []media.Format
		width, height int
		quadrants     [4]color.RGBA
	}{
		{"raw TIFF orientation 6 WebP only", 6, false, []media.Format{media.FormatWebP}, 16, 32, [4]color.RGBA{blue, red, yellow, green}},
		{"Exif prefix orientation 3 JPEG and WebP", 3, true, []media.Format{media.FormatJPEG, media.FormatWebP}, 32, 16, [4]color.RGBA{yellow, blue, green, red}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			input := webpContainer(webpVP8X(32, 16, 0x08), imageChunk, webpChunk("EXIF", webpEXIF(row.orientation, true, row.prefixed)))
			if _, err := webp.Decode(bytes.NewReader(input)); err != nil {
				t.Fatalf("oriented WebP fixture must decode: %v", err)
			}
			result, err := ingest(t, pipeline(t, 1), input, "image/webp", row.accept)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != media.FormatJPEG || result.Width != row.width || result.Height != row.height {
				t.Fatalf("result: got %q %dx%d, want image/jpeg %dx%d", result.Format, result.Width, result.Height, row.width, row.height)
			}
			if len(result.Data) < 2 || !bytes.Equal(result.Data[:2], []byte{0xff, 0xd8}) || bytes.Contains(result.Data, []byte("Exif")) {
				t.Errorf("oriented output must be JPEG without Exif")
			}
			segments, _, err := testkit.WalkJPEGSegments(result.Data)
			if err != nil {
				t.Fatalf("parse oriented JPEG: %v", err)
			}
			for _, segment := range segments {
				if segment.Marker == 0xe1 {
					t.Error("oriented JPEG contains APP1")
				}
			}
			decoded, err := jpeg.Decode(bytes.NewReader(result.Data))
			if err != nil || imageBounds(decoded) != image.Rect(0, 0, row.width, row.height) {
				t.Fatalf("decode oriented JPEG: bounds %v, error %v", imageBounds(decoded), err)
			}
			assertQuadrants(t, decoded, row.quadrants)
		})
	}
}

// Lossless 32x16 quadrants generated once with PIL: red, transparent, blue,
// yellow. Check its decoded pixels so the fixture cannot silently change.
const transparentQuadrantWebPBase64 = "UklGRjQAAABXRUJQVlA4TCcAAAAvH8ADEB8w//MvICjyf7T5D+AmQJBtM8lJTvIAF4jof1QB/FS37QcA"

func transparentQuadrantWebP(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(transparentQuadrantWebPBase64)
	if err != nil {
		t.Fatalf("decode transparent quadrant WebP fixture: %v", err)
	}
	decoded, err := webp.Decode(bytes.NewReader(data))
	if err != nil || imageBounds(decoded) != image.Rect(0, 0, 32, 16) {
		t.Fatalf("transparent quadrant WebP fixture: bounds %v, error %v", imageBounds(decoded), err)
	}
	assertQuadrantAlpha(t, decoded, [4]uint32{255, 0, 255, 255})
	return data
}

func assertQuadrantAlpha(t *testing.T, img image.Image, expected [4]uint32) {
	t.Helper()
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	centres := [4]image.Point{{w / 4, h / 4}, {3 * w / 4, h / 4}, {w / 4, 3 * h / 4}, {3 * w / 4, 3 * h / 4}}
	for i, point := range centres {
		if _, _, _, alpha := img.At(point.X, point.Y).RGBA(); alpha>>8 != expected[i] {
			t.Errorf("quadrant %d alpha at %v: got %d, want %d", i, point, alpha>>8, expected[i])
		}
	}
}

// iccTestProfile is not a valid ICC profile; decoders ignore its contents, and
// the pipeline must carry the bytes unchanged.
func iccTestProfile(size int) []byte {
	profile := make([]byte, size)
	for i := range profile {
		profile[i] = byte(i*7 + i/251)
	}
	return profile
}

// pngICCProfile returns the decompressed iCCP profile and whether iCCP comes
// before the first IDAT, as the PNG specification requires.
func pngICCProfile(t *testing.T, data []byte) (profile []byte, beforeImageData bool) {
	t.Helper()
	seenImageData := false
	for offset := 8; offset+12 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[offset:]))
		end := offset + 12 + length
		if end > len(data) {
			t.Fatal("PNG chunk exceeds output")
		}
		chunk := data[offset+8 : offset+8+length]
		if crc32.ChecksumIEEE(data[offset+4:offset+8+length]) != binary.BigEndian.Uint32(data[offset+8+length:end]) {
			t.Fatalf("PNG chunk %q has a bad CRC", data[offset+4:offset+8])
		}
		switch string(data[offset+4 : offset+8]) {
		case "IDAT":
			seenImageData = true
		case "iCCP":
			separator := bytes.IndexByte(chunk, 0)
			if separator < 1 || separator > 79 || len(chunk) < separator+2 || chunk[separator+1] != 0 {
				t.Fatalf("malformed iCCP header %q", chunk)
			}
			reader, err := zlib.NewReader(bytes.NewReader(chunk[separator+2:]))
			if err != nil {
				t.Fatalf("open iCCP zlib stream: %v", err)
			}
			profile, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("decompress iCCP profile: %v", err)
			}
			return profile, !seenImageData
		}
		offset = end
	}
	return nil, false
}

func TestPipelineOrientsAlphaWebPToPNG(t *testing.T) {
	imageChunk := webpImageChunk(t, transparentQuadrantWebP(t))
	profile := iccTestProfile(3000)
	rows := []struct {
		name  string
		input []byte
		icc   []byte
	}{
		{"alpha without colour profile", webpContainer(webpVP8X(32, 16, 0x18), imageChunk, webpChunk("EXIF", webpEXIF(6, true, false))), nil},
		{"alpha with ICCP colour profile", webpContainer(webpVP8X(32, 16, 0x38), webpChunk("ICCP", profile), imageChunk, webpChunk("EXIF", webpEXIF(6, true, false))), profile},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := webp.Decode(bytes.NewReader(row.input)); err != nil {
				t.Fatalf("oriented alpha WebP fixture must decode: %v", err)
			}
			result, err := ingest(t, pipeline(t, 1), row.input, "image/webp", []media.Format{media.FormatWebP})
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != media.FormatPNG || result.Width != 16 || result.Height != 32 {
				t.Fatalf("result: got %q %dx%d, want image/png 16x32", result.Format, result.Width, result.Height)
			}
			if bytes.Contains(result.Data, []byte("eXIf")) || bytes.Contains(result.Data, []byte("II*\x00")) {
				t.Error("oriented PNG output contains EXIF")
			}
			decoded, err := png.Decode(bytes.NewReader(result.Data))
			if err != nil || imageBounds(decoded) != image.Rect(0, 0, 16, 32) {
				t.Fatalf("decode oriented PNG: bounds %v, error %v", imageBounds(decoded), err)
			}
			// Orientation 6 moves the transparent top-right quadrant to the
			// bottom right; the opaque quadrants keep their colours.
			assertQuadrantAlpha(t, decoded, [4]uint32{255, 255, 255, 0})
			for i, point := range []image.Point{{4, 8}, {12, 8}, {4, 24}} {
				r, g, b, _ := decoded.At(point.X, point.Y).RGBA()
				want := [3]color.RGBA{blue, red, yellow}[i]
				if uint8(r>>8) != want.R || uint8(g>>8) != want.G || uint8(b>>8) != want.B {
					t.Errorf("pixel %v: got %d,%d,%d, want %d,%d,%d", point, r>>8, g>>8, b>>8, want.R, want.G, want.B)
				}
			}
			got, beforeImageData := pngICCProfile(t, result.Data)
			if row.icc == nil {
				if got != nil {
					t.Error("PNG output has an iCCP chunk the input lacked")
				}
				return
			}
			if !bytes.Equal(got, row.icc) {
				t.Errorf("iCCP profile: got %d bytes, want the %d input ICCP bytes", len(got), len(row.icc))
			}
			if !beforeImageData {
				t.Error("iCCP chunk must precede IDAT")
			}
		})
	}
}

// jpegICCProfile reassembles APP2 ICC_PROFILE segments in sequence order.
func jpegICCProfile(t *testing.T, data []byte) []byte {
	t.Helper()
	segments, _, err := testkit.WalkJPEGSegments(data)
	if err != nil {
		t.Fatalf("parse JPEG: %v", err)
	}
	var parts [][]byte
	for _, segment := range segments {
		payload := segment.Bytes[4:]
		if segment.Marker != 0xe2 || !bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")) {
			continue
		}
		if len(payload) < 14 {
			t.Fatal("truncated ICC_PROFILE segment")
		}
		sequence, count := int(payload[12]), int(payload[13])
		if parts == nil {
			parts = make([][]byte, count)
		}
		if count != len(parts) || sequence < 1 || sequence > count || parts[sequence-1] != nil {
			t.Fatalf("ICC_PROFILE segment %d of %d is inconsistent", sequence, count)
		}
		parts[sequence-1] = payload[14:]
	}
	for i, part := range parts {
		if part == nil {
			t.Fatalf("ICC_PROFILE segment %d missing", i+1)
		}
	}
	return bytes.Join(parts, nil)
}

func TestPipelineOrientsWebPKeepingICCProfileInJPEG(t *testing.T) {
	imageChunk := webpImageChunk(t, quadrantWebP(t))
	rows := []struct {
		name    string
		profile []byte
	}{
		{"profile in one APP2 segment", iccTestProfile(3000)},
		{"profile split across APP2 segments", iccTestProfile(140_000)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			input := webpContainer(webpVP8X(32, 16, 0x28), webpChunk("ICCP", row.profile), imageChunk, webpChunk("EXIF", webpEXIF(6, true, false)))
			if _, err := webp.Decode(bytes.NewReader(input)); err != nil {
				t.Fatalf("oriented ICCP WebP fixture must decode: %v", err)
			}
			result, err := ingest(t, pipeline(t, 1), input, "image/webp", []media.Format{media.FormatWebP})
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != media.FormatJPEG || result.Width != 16 || result.Height != 32 {
				t.Fatalf("result: got %q %dx%d, want image/jpeg 16x32", result.Format, result.Width, result.Height)
			}
			if got := jpegICCProfile(t, result.Data); !bytes.Equal(got, row.profile) {
				t.Errorf("APP2 ICC profile: got %d bytes, want the %d input ICCP bytes", len(got), len(row.profile))
			}
			decoded, err := jpeg.Decode(bytes.NewReader(result.Data))
			if err != nil || imageBounds(decoded) != image.Rect(0, 0, 16, 32) {
				t.Fatalf("decode oriented JPEG: bounds %v, error %v", imageBounds(decoded), err)
			}
			assertQuadrants(t, decoded, [4]color.RGBA{blue, red, yellow, green})
		})
	}
}
