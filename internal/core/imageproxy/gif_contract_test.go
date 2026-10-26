package imageproxy

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertRedJPEG(t *testing.T, data []byte, width, height int) {
	t.Helper()
	require.True(t, bytes.HasPrefix(data, []byte{0xFF, 0xD8}), "JPEG SOI missing")
	picture, err := jpeg.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, width, picture.Bounds().Dx())
	assert.Equal(t, height, picture.Bounds().Dy())
	red, green, blue, _ := picture.At(width/2, height/2).RGBA()
	assert.Greater(t, red>>8, uint32(200))
	assert.Less(t, green>>8, uint32(55))
	assert.Less(t, blue>>8, uint32(55))
}

func TestProcessor_AnimatedGIFPreviewUsesFirstFrame(t *testing.T) {
	data := animatedGIF(t, 800, 600, 2)
	processor := newTestProcessor(t)
	for _, test := range []struct {
		preset        string
		width, height int
	}{
		{"avatar", 1000, 1000},
		{"avatar_small", 360, 360},
		{"banner", 640, 300},
		{"content_preview", 800, 600},
		{"embed_thumbnail", 720, 360},
	} {
		t.Run(test.preset, func(t *testing.T) {
			preset, err := GetPreset(test.preset)
			require.NoError(t, err)
			output, err := processor.Process(data, preset)
			require.NoError(t, err)
			assertRedJPEG(t, output, test.width, test.height)
		})
	}
}

func TestProcessor_GIFBlockScanAndMalformedInput(t *testing.T) {
	large := gifWithComment(t, 1000, 1000)
	small := gifWithComment(t, 40, 30)
	globalTruncated := framedJunkGIF(40, 30, 1)[:16] // 13-byte header/LSD + 3 of 6 global-table bytes
	lzwTruncated := framedJunkGIF(40, 30, 1)[:32]    // header/LSD (13) + global table (6) + descriptor (10) + min code (1) + sub-block length (1)
	frameLargerThanScreen := framedJunkGIFWithFrame(1, 1, image.Rect(0, 0, 65535, 65535), 3)
	frameOffsetPastRight := framedJunkGIFWithFrame(40, 30, image.Rect(1, 0, 41, 30), 1)
	frameOffsetPastBottom := framedJunkGIFWithFrame(40, 30, image.Rect(0, 1, 40, 31), 1)
	twoFrames := animatedGIF(t, 40, 30, 2)
	junkAfterTrailer := append(append([]byte{}, twoFrames...), "junk"...)
	missingTrailer := twoFrames[:len(twoFrames)-1]
	trailerWithoutFrames := []byte{'G', 'I', 'F', '8', '9', 'a', 1, 0, 1, 0, 0, 0, 0, 0x3B} // header/LSD (13), no global table
	unrecognisedVersion := []byte{'G', 'I', 'F', '8', '0', 'a', 1, 0, 1, 0, 0, 0, 0, 0x3B}
	for _, test := range []struct {
		name, preset string
		data         []byte
		wantErr      error
		jpegWidth    int
		jpegHeight   int
		unchanged    bool
	}{
		{"local tables and comment over budget preview", "content_preview", large, ErrImageTooManyPixels, 0, 0, false},
		{"local tables and comment over budget full", "content_full", large, ErrImageTooManyPixels, 0, 0, false},
		{"local tables and comment small preview", "content_preview", small, nil, 40, 30, false},
		{"local tables and comment small full", "content_full", small, nil, 0, 0, true},
		{"truncated global table preview", "content_preview", globalTruncated, ErrUnsupportedFormat, 0, 0, false},
		{"truncated global table full", "content_full", globalTruncated, ErrUnsupportedFormat, 0, 0, false},
		{"truncated LZW sub-block preview", "content_preview", lzwTruncated, ErrUnsupportedFormat, 0, 0, false},
		{"truncated LZW sub-block full", "content_full", lzwTruncated, ErrUnsupportedFormat, 0, 0, false},
		{"framed undecodable first frame", "content_preview", framedJunkGIF(40, 30, 1), ErrUnsupportedFormat, 0, 0, false},
		{"frame larger than screen preview", "content_preview", frameLargerThanScreen, ErrUnsupportedFormat, 0, 0, false},
		{"frame larger than screen full", "content_full", frameLargerThanScreen, ErrUnsupportedFormat, 0, 0, false},
		{"frame offset past right edge full", "content_full", frameOffsetPastRight, ErrUnsupportedFormat, 0, 0, false},
		{"frame offset past bottom edge full", "content_full", frameOffsetPastBottom, ErrUnsupportedFormat, 0, 0, false},
		{"unrecognised GIF version full", "content_full", unrecognisedVersion, ErrUnsupportedFormat, 0, 0, false},
		{"bytes after trailer full", "content_full", junkAfterTrailer, ErrUnsupportedFormat, 0, 0, false},
		{"missing trailer full", "content_full", missingTrailer, ErrUnsupportedFormat, 0, 0, false},
		{"trailer without frames full", "content_full", trailerWithoutFrames, ErrUnsupportedFormat, 0, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			processor := newProcessorWithBudget(t, 1)
			preset, err := GetPreset(test.preset)
			require.NoError(t, err)
			output, err := processor.Process(test.data, preset)
			if test.wantErr != nil {
				require.True(t, errors.Is(err, test.wantErr), "got %v, want %v", err, test.wantErr)
				assert.Empty(t, output)
				return
			}
			require.NoError(t, err)
			if test.unchanged {
				assert.Equal(t, test.data, output)
			} else {
				assertRedJPEG(t, output, test.jpegWidth, test.jpegHeight)
			}
		})
	}
}
