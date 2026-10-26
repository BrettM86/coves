package media_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image/jpeg"
	"testing"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

// Go's encoder writes neither restart intervals nor progressive scans, so these
// fixtures come from libjpeg-turbo 3.2 cjpeg. Each holds only a thumbnail-free
// JFIF APP0 and structural segments, all of which the metadata allowlist keeps.
const (
	// cjpeg -quality 75 -optimize -restart 1B: 64x32 baseline, DRI of one MCU, seven RST markers.
	restartIntervalJPEGBase64 = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/2wBDAQkJCQwLDBgNDRgyIRwhMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjL/wAARCAAgAEADASIAAhEBAxEB/8QAFgABAQEAAAAAAAAAAAAAAAAAAAYH/8QAFhAAAwAAAAAAAAAAAAAAAAAAABVi/8QAFgEBAQEAAAAAAAAAAAAAAAAABAIH/8QAFxEBAAMAAAAAAAAAAAAAAAAAABMVYf/dAAQAAf/aAAwDAQACEQMRAD8A0prQa0RLWg1ozemwyR//0NKa0GtES1oNaM3psMkf/9HSmtBrREtaDWjN6bDJH//S0prQa0RLWg1ozemwyR//05xrQa0RDWg1ofTYmR//1JxrQa0RDWg1ofTYmR//1ZxrQa0RDWg1ofTYmR//1pxrQa0RDWg1ofTYmR//2Q=="
	// cjpeg -quality 75 -progressive: 32x32, ten scans with DHT segments between them.
	progressiveJPEGBase64 = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/2wBDAQkJCQwLDBgNDRgyIRwhMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjL/wgARCAAgACADASIAAhEBAxEB/8QAFgABAQEAAAAAAAAAAAAAAAAAAAMF/8QAFgEBAQEAAAAAAAAAAAAAAAAAAAYB/9oADAMBAAIQAxAAAAHCRZUWREUVPO2RH//EABUQAQEAAAAAAAAAAAAAAAAAAAAR/9oACAEBAAEFAqqqqqqqqqqq/8QAFREBAQAAAAAAAAAAAAAAAAAAABT/2gAIAQMBAT8BpUqVL//EABURAQEAAAAAAAAAAAAAAAAAAAAU/9oACAECAQE/AZEiRI//xAAUEAEAAAAAAAAAAAAAAAAAAABA/9oACAEBAAY/Agf/xAAUEAEAAAAAAAAAAAAAAAAAAABA/9oACAEBAAE/IQAAB//aAAwDAQACAAMAAAAQ99DD/8QAFREBAQAAAAAAAAAAAAAAAAAAAHH/2gAIAQMBAT8Qta1v/8QAFREBAQAAAAAAAAAAAAAAAAAAAHH/2gAIAQIBAT8QhCEP/8QAFBABAAAAAAAAAAAAAAAAAAAAQP/aAAgBAQABPxAH/wD/AP/Z"
)

func decodeFixture(t *testing.T, encoded string, width, height int) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode fixture base64: %v", err)
	}
	expectHeaderDimensions(t, data, "image/jpeg", width, height)
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	return data
}

// jpegScanBounds returns the offsets of every SOS marker and the end of its
// entropy-coded data, which ends at the first marker other than a stuffed
// FF00 or an RST marker.
func jpegScanBounds(t *testing.T, data []byte) (starts, ends []int) {
	t.Helper()
	for offset := 2; offset+2 <= len(data); {
		if data[offset] != 0xff {
			t.Fatalf("missing JPEG marker at %d", offset)
		}
		marker := data[offset+1]
		if marker == 0xd9 {
			return starts, ends
		}
		if offset+4 > len(data) {
			break
		}
		next := offset + 2 + int(binary.BigEndian.Uint16(data[offset+2:offset+4]))
		if marker == 0xda {
			starts = append(starts, offset)
			for next+1 < len(data) && (data[next] != 0xff || data[next+1] == 0x00 || data[next+1] >= 0xd0 && data[next+1] <= 0xd7) {
				next++
			}
			ends = append(ends, next)
		}
		offset = next
	}
	t.Fatal("missing JPEG EOI")
	return nil, nil
}

func insertAt(data []byte, offset int, segments ...[]byte) []byte {
	output := append([]byte(nil), data[:offset]...)
	for _, segment := range segments {
		output = append(output, segment...)
	}
	return append(output, data[offset:]...)
}

func TestPipelineKeepsRestartAndProgressiveScanStructure(t *testing.T) {
	restartInterval := decodeFixture(t, restartIntervalJPEGBase64, 64, 32)
	progressive := decodeFixture(t, progressiveJPEGBase64, 32, 32)
	if !bytes.Contains(restartInterval, []byte{0xff, 0xdd}) || !bytes.Contains(restartInterval, []byte{0xff, 0xd0}) {
		t.Fatal("restart fixture must have a DRI segment and RST markers in its scan")
	}
	progressiveScanStarts, progressiveScanEnds := jpegScanBounds(t, progressive)
	if len(progressiveScanStarts) < 2 {
		t.Fatalf("progressive fixture has %d scans, want several", len(progressiveScanStarts))
	}
	gps, comment := testkit.JPEGExifGPSegment(1), testkit.JPEGCommentSegment()

	rows := []struct {
		name          string
		fixture       []byte
		input         []byte
		width, height int
	}{
		{"baseline with restart interval and metadata before the scan", restartInterval, testkit.JPEGWithSegments(restartInterval, gps, comment), 64, 32},
		{"progressive with Exif GPS and COM between scans", progressive, insertAt(progressive, progressiveScanEnds[0], gps, comment), 32, 32},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			result, err := ingest(t, pipeline(t, 1), row.input, "image/jpeg", allFormats)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if result.Format != media.FormatJPEG || result.Width != row.width || result.Height != row.height {
				t.Fatalf("result: got %q %dx%d, want image/jpeg %dx%d", result.Format, result.Width, result.Height, row.width, row.height)
			}
			if !bytes.Equal(result.Data, row.fixture) {
				t.Fatalf("output: got %d bytes, want the input minus exactly the Exif GPS and COM segments (%d bytes)", len(result.Data), len(row.fixture))
			}
		})
	}
}

// progressiveJPEGWithScans repeats the progressive fixture's first AC scan
// (spectral start above zero, no successive-approximation high bit) until the
// file has the requested number of scans. Go's decoder accepts the repeats;
// each costs work proportional to the pixel count.
func progressiveJPEGWithScans(t *testing.T, scans int) []byte {
	t.Helper()
	progressive := decodeFixture(t, progressiveJPEGBase64, 32, 32)
	starts, ends := jpegScanBounds(t, progressive)
	if scans < len(starts) {
		t.Fatalf("fixture already has %d scans, more than %d", len(starts), scans)
	}
	for i, start := range starts {
		header := progressive[start+4:]
		componentCount := int(header[0])
		spectralStart, approximation := header[1+2*componentCount], header[3+2*componentCount]
		if spectralStart == 0 || approximation>>4 != 0 {
			continue
		}
		scan := progressive[start:ends[i]]
		return insertAt(progressive, ends[i], bytes.Repeat(scan, scans-len(starts)))
	}
	t.Fatal("progressive fixture has no first-pass AC scan")
	return nil
}

func TestPipelineRejectsJPEGWithTooManyScansBeforeDecodeSlot(t *testing.T) {
	t.Run("at the cap is accepted", func(t *testing.T) {
		input := progressiveJPEGWithScans(t, media.MaxJPEGScans)
		if starts, _ := jpegScanBounds(t, input); len(starts) != media.MaxJPEGScans {
			t.Fatalf("fixture has %d scans, want %d", len(starts), media.MaxJPEGScans)
		}
		result, err := ingest(t, pipeline(t, 1), input, "image/jpeg", allFormats)
		if err != nil {
			t.Fatalf("ingest %d-scan JPEG: %v", media.MaxJPEGScans, err)
		}
		if result.Width != 32 || result.Height != 32 || !bytes.Equal(result.Data, input) {
			t.Fatalf("result: got %dx%d, %d bytes, want the unchanged 32x32 input (%d bytes)", result.Width, result.Height, len(result.Data), len(input))
		}
	})
	t.Run("one over the cap is invalid without waiting for a slot", func(t *testing.T) {
		input := progressiveJPEGWithScans(t, media.MaxJPEGScans+1)
		p, ctx, _ := heldSlots(t)
		_, done := startIngest(p, ctx, input, allFormats)
		expectCode(t, waitIngest(t, done).err, media.ErrorCodeInvalidImage)
	})
}
