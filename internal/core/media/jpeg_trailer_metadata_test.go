package media_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"image/jpeg"
	"testing"

	"Coves/internal/core/media"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Keep the GPS IFD from the shared fixture but remove its orientation entry.
// The GPS and latitude offsets still point to the same TIFF locations.
func jpegTrailerGPSWithoutOrientation() []byte {
	segment := testkit.JPEGExifGPSegment(1)
	binary.LittleEndian.PutUint16(segment[18:20], 1) // IFD0 now has only the GPS pointer.
	copy(segment[20:32], segment[32:44])
	clear(segment[32:44])
	return segment
}

func TestPipelineStripsJPEGMetadataAfterTheFirstScan(t *testing.T) {
	primary := testkit.TestJPEG(48, 32)
	gps := jpegTrailerGPSWithoutOrientation()
	comment := testkit.JPEGCommentSegment()
	secondary := testkit.JPEGWithSegments(testkit.TestJPEG(16, 12), gps, comment)
	withSecondImage := append(append([]byte(nil), primary...), secondary...)
	withPostScanSegments := append(append([]byte(nil), primary[:len(primary)-2]...), gps...)
	withPostScanSegments = append(withPostScanSegments, comment...)
	withPostScanSegments = append(withPostScanSegments, primary[len(primary)-2:]...)

	for _, test := range []struct {
		name  string
		input []byte
	}{
		{name: "secondary JPEG after first EOI", input: withSecondImage},
		{name: "Exif GPS and COM between scan and EOI", input: withPostScanSegments},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.True(t, bytes.Contains(test.input, gps), "fixture must contain GPS Exif segment")
			require.True(t, bytes.Contains(test.input, comment), "fixture must contain COM segment")
			pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: media.DefaultMaxLongEdge})
			require.NoError(t, err)
			result, err := pipeline.Ingest(context.Background(), media.Input{
				Data: test.input, Accept: []media.Format{media.FormatJPEG},
			})
			require.NoError(t, err)
			assert.Equal(t, media.FormatJPEG, result.Format)
			assert.Equal(t, 48, result.Width)
			assert.Equal(t, 32, result.Height)
			assert.False(t, bytes.Contains(result.Data, []byte("Exif\x00\x00")), "Exif signature survives")
			assert.False(t, bytes.Contains(result.Data, gps), "GPS segment survives")
			assert.False(t, bytes.Contains(result.Data, comment[4:]), "COM payload survives")
			assert.True(t, bytes.HasSuffix(result.Data, []byte{0xff, 0xd9}), "output must end at EOI")
			assert.True(t, bytes.Equal(primary, result.Data), "output must be the metadata-free primary JPEG through its first EOI (got %d bytes, want %d)", len(result.Data), len(primary))
			decoded, err := jpeg.Decode(bytes.NewReader(result.Data))
			require.NoError(t, err)
			assert.Equal(t, 48, decoded.Bounds().Dx())
			assert.Equal(t, 32, decoded.Bounds().Dy())
		})
	}
}
