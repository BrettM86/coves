package media_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

// PIL-generated 12x8 CMYK JPEG with its original Adobe APP14 declaration.
const cmykJPEGBase64 = "/9j/7gAOQWRvYmUAZAAAAAAA/9sAQwADAgIDAgIDAwMDBAMDBAUIBQUEBAUKBwcGCAwKDAwLCgsLDQ4SEA0OEQ4LCxAWEBETFBUVFQwPFxgWFBgSFBUU/8AAFAgACAAMBEMRAE0RAFkRAEsRAP/EAB8AAAEFAQEBAQEBAAAAAAAAAAABAgMEBQYHCAkKC//EALUQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+v/aAA4EQwBNAFkASwAAPwD9U606+B6/VOiiiiv/2Q=="

// Locate length-bearing JPEG segments before SOS. JPEG fill bytes can precede
// any marker; the returned bytes start at the final FF before the marker code.
func jpegSegmentsWithFill(t *testing.T, data []byte) map[byte][][]byte {
	t.Helper()
	if len(data) < 2 || !bytes.Equal(data[:2], []byte{0xff, 0xd8}) {
		t.Fatal("missing JPEG SOI")
	}
	segments := make(map[byte][][]byte)
	for offset := 2; offset < len(data); {
		if data[offset] != 0xff {
			t.Fatalf("missing JPEG marker at %d", offset)
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset+3 > len(data) {
			t.Fatal("truncated JPEG marker")
		}
		marker := data[offset]
		if marker == 0xda {
			return segments
		}
		length := int(binary.BigEndian.Uint16(data[offset+1 : offset+3]))
		if length < 2 || offset+1+length > len(data) {
			t.Fatalf("invalid JPEG marker 0x%02x length %d", marker, length)
		}
		segments[marker] = append(segments[marker], data[offset-1:offset+1+length])
		offset += 1 + length
	}
	t.Fatal("missing JPEG scan")
	return nil
}

func TestPipelineKeepsJPEGMetadataAfterMarkerFillBytes(t *testing.T) {
	cmyk, err := base64.StdEncoding.DecodeString(cmykJPEGBase64)
	if err != nil {
		t.Fatalf("decode CMYK fixture: %v", err)
	}
	decodedCMYK, err := jpeg.Decode(bytes.NewReader(cmyk))
	if err != nil || imageBounds(decodedCMYK) != image.Rect(0, 0, 12, 8) {
		t.Fatalf("CMYK fixture: bounds %v, error %v", imageBounds(decodedCMYK), err)
	}
	if _, ok := decodedCMYK.(*image.CMYK); !ok {
		t.Fatalf("CMYK fixture type: %T, want *image.CMYK", decodedCMYK)
	}
	adobe := jpegSegmentsWithFill(t, cmyk)[0xee]
	if len(adobe) != 1 || !bytes.HasPrefix(adobe[0][4:], []byte("Adobe")) {
		t.Fatal("CMYK fixture lacks Adobe APP14")
	}
	icc := testkit.JPEGICCSegment()
	rgb := quadrantJPEG(t)
	rows := []struct {
		name          string
		input         []byte
		marker        byte
		wantSegment   []byte
		width, height int
		cmyk          bool
	}{
		{"CMYK Adobe APP14 orientation 1", insertAt(cmyk, 2, []byte{0xff}), 0xee, adobe[0], 12, 8, true},
		{"RGB ICC APP2 orientation 1", insertAt(testkit.JPEGWithSegments(rgb, icc), 2, []byte{0xff, 0xff}), 0xe2, icc, 64, 32, false},
		{"RGB ICC APP2 orientation 6", insertAt(testkit.JPEGWithSegments(rgb, icc, testkit.JPEGExifGPSegment(6)), 2, []byte{0xff, 0xff}), 0xe2, icc, 32, 64, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := jpeg.Decode(bytes.NewReader(row.input)); err != nil {
				t.Fatalf("JPEG fixture with fill bytes must decode: %v", err)
			}
			result, err := ingest(t, pipeline(t, 1), row.input, "image/jpeg", []media.Format{media.FormatJPEG})
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != media.FormatJPEG || result.Width != row.width || result.Height != row.height {
				t.Fatalf("result: got %q %dx%d, want image/jpeg %dx%d", result.Format, result.Width, result.Height, row.width, row.height)
			}
			found := false
			for _, segment := range jpegSegmentsWithFill(t, result.Data)[row.marker] {
				if bytes.Equal(segment, row.wantSegment) {
					found = true
				}
			}
			if !found {
				t.Fatalf("output lacks structurally valid byte-identical APP%d segment", int(row.marker)-0xe0)
			}
			decoded, err := jpeg.Decode(bytes.NewReader(result.Data))
			if err != nil || imageBounds(decoded) != image.Rect(0, 0, row.width, row.height) {
				t.Fatalf("decode output: bounds %v, error %v", imageBounds(decoded), err)
			}
			if row.cmyk {
				if _, ok := decoded.(*image.CMYK); !ok {
					t.Errorf("output decoded as %T, want *image.CMYK", decoded)
				}
			}
		})
	}
}

func TestPipelineAcceptsJPEGFillBytesBeforeRestartMarker(t *testing.T) {
	original := decodeFixture(t, restartIntervalJPEGBase64, 64, 32)
	starts, ends := jpegScanBounds(t, original)
	if len(starts) != 1 {
		t.Fatalf("restart fixture scans: got %d, want one", len(starts))
	}
	rst := -1
	for i := starts[0] + 2; i+1 < ends[0]; i++ {
		if original[i] == 0xff && original[i+1] >= 0xd0 && original[i+1] <= 0xd7 {
			rst = i
			break
		}
	}
	if rst < 0 {
		t.Fatal("restart fixture has no scan RST marker")
	}
	input := insertAt(original, rst, []byte{0xff})
	baseline, err := jpeg.Decode(bytes.NewReader(original))
	if err != nil {
		t.Fatalf("decode original: %v", err)
	}
	withFill, err := jpeg.Decode(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("stdlib must decode restart fill bytes: %v", err)
	}
	if !sameJPEGPixelColours(baseline, withFill) {
		t.Fatal("fill bytes changed fixture pixels")
	}
	result, err := ingest(t, pipeline(t, 1), input, "image/jpeg", []media.Format{media.FormatJPEG})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Format != media.FormatJPEG || result.Width != 64 || result.Height != 32 {
		t.Fatalf("result: got %q %dx%d, want image/jpeg 64x32", result.Format, result.Width, result.Height)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(result.Data))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if !sameJPEGPixelColours(baseline, decoded) {
		t.Error("output pixels differ from original restart fixture")
	}
}

func sameJPEGPixelColours(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			r1, g1, b1, a1 := a.At(x, y).RGBA()
			r2, g2, b2, a2 := b.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				return false
			}
		}
	}
	return true
}
