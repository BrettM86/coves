package imageproxy

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestJPEG creates a test JPEG image with the specified dimensions.
func createTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	// Fill with a solid color
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 128, B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	require.NoError(t, err)
	return buf.Bytes()
}

// createTestPNG creates a test PNG image with the specified dimensions.
func createTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	// Fill with a solid color
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 64, G: 128, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	err := png.Encode(&buf, img)
	require.NoError(t, err)
	return buf.Bytes()
}

func TestProcessor_Process_CoverFit(t *testing.T) {
	proc := NewProcessor()

	tests := []struct {
		name         string
		srcWidth     int
		srcHeight    int
		preset       Preset
		wantWidth    int
		wantHeight   int
		description  string
	}{
		{
			name:        "landscape image to square avatar",
			srcWidth:    800,
			srcHeight:   600,
			preset:      Preset{Name: "avatar", Width: 1000, Height: 1000, Fit: FitCover, Quality: 85},
			wantWidth:   1000,
			wantHeight:  1000,
			description: "landscape cropped to square",
		},
		{
			name:        "portrait image to square avatar",
			srcWidth:    600,
			srcHeight:   800,
			preset:      Preset{Name: "avatar", Width: 1000, Height: 1000, Fit: FitCover, Quality: 85},
			wantWidth:   1000,
			wantHeight:  1000,
			description: "portrait cropped to square",
		},
		{
			name:        "square image to smaller square",
			srcWidth:    500,
			srcHeight:   500,
			preset:      Preset{Name: "avatar_small", Width: 360, Height: 360, Fit: FitCover, Quality: 80},
			wantWidth:   360,
			wantHeight:  360,
			description: "square scaled down",
		},
		{
			name:        "landscape to banner dimensions",
			srcWidth:    1920,
			srcHeight:   1080,
			preset:      Preset{Name: "banner", Width: 640, Height: 300, Fit: FitCover, Quality: 85},
			wantWidth:   640,
			wantHeight:  300,
			description: "banner crop",
		},
		{
			name:        "embed thumbnail dimensions",
			srcWidth:    1600,
			srcHeight:   900,
			preset:      Preset{Name: "embed_thumbnail", Width: 720, Height: 360, Fit: FitCover, Quality: 80},
			wantWidth:   720,
			wantHeight:  360,
			description: "embed thumbnail crop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srcData := createTestJPEG(t, tt.srcWidth, tt.srcHeight)

			result, err := proc.Process(srcData, tt.preset)
			require.NoError(t, err)
			require.NotNil(t, result)

			// Decode the result to verify dimensions
			img, _, err := image.Decode(bytes.NewReader(result))
			require.NoError(t, err)

			bounds := img.Bounds()
			assert.Equal(t, tt.wantWidth, bounds.Dx(), "width mismatch for %s", tt.description)
			assert.Equal(t, tt.wantHeight, bounds.Dy(), "height mismatch for %s", tt.description)
		})
	}
}

func TestProcessor_Process_ContainFit(t *testing.T) {
	proc := NewProcessor()

	tests := []struct {
		name          string
		srcWidth      int
		srcHeight     int
		preset        Preset
		wantMaxWidth  int
		wantMaxHeight int
		description   string
	}{
		{
			name:          "landscape image scaled to content_preview width",
			srcWidth:      1600,
			srcHeight:     900,
			preset:        Preset{Name: "content_preview", Width: 800, Height: 0, Fit: FitContain, Quality: 80},
			wantMaxWidth:  800,
			wantMaxHeight: 450, // 800 * (900/1600) = 450 (aspect ratio preserved)
			description:   "landscape scaled proportionally",
		},
		{
			name:          "portrait image scaled to content_preview width",
			srcWidth:      900,
			srcHeight:     1600,
			preset:        Preset{Name: "content_preview", Width: 800, Height: 0, Fit: FitContain, Quality: 80},
			wantMaxWidth:  800,
			wantMaxHeight: 1422, // 800 * (1600/900) ~= 1422
			description:   "portrait scaled proportionally",
		},
		{
			name:          "wide panorama to content_full",
			srcWidth:      3200,
			srcHeight:     800,
			preset:        Preset{Name: "content_full", Width: 1600, Height: 0, Fit: FitContain, Quality: 90},
			wantMaxWidth:  1600,
			wantMaxHeight: 400, // 1600 * (800/3200) = 400
			description:   "panorama scaled proportionally",
		},
		{
			name:          "image smaller than target width stays same size",
			srcWidth:      400,
			srcHeight:     300,
			preset:        Preset{Name: "content_preview", Width: 800, Height: 0, Fit: FitContain, Quality: 80},
			wantMaxWidth:  400, // Don't upscale
			wantMaxHeight: 300,
			description:   "small image not upscaled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srcData := createTestJPEG(t, tt.srcWidth, tt.srcHeight)

			result, err := proc.Process(srcData, tt.preset)
			require.NoError(t, err)
			require.NotNil(t, result)

			// Decode the result to verify dimensions
			img, _, err := image.Decode(bytes.NewReader(result))
			require.NoError(t, err)

			bounds := img.Bounds()
			// For contain fit, verify width doesn't exceed max and aspect ratio is preserved
			assert.LessOrEqual(t, bounds.Dx(), tt.wantMaxWidth, "width should not exceed max for %s", tt.description)
			assert.Equal(t, tt.wantMaxWidth, bounds.Dx(), "width mismatch for %s", tt.description)
			assert.Equal(t, tt.wantMaxHeight, bounds.Dy(), "height mismatch for %s", tt.description)
		})
	}
}

func TestProcessor_Process_InvalidImageData(t *testing.T) {
	proc := NewProcessor()

	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{
			name:    "empty data",
			data:    []byte{},
			wantErr: ErrUnsupportedFormat,
		},
		{
			name:    "nil data",
			data:    nil,
			wantErr: ErrUnsupportedFormat,
		},
		{
			name:    "random garbage data",
			data:    []byte("not an image at all"),
			wantErr: ErrUnsupportedFormat,
		},
		{
			name:    "truncated JPEG header",
			data:    []byte{0xFF, 0xD8, 0xFF, 0xE0}, // Partial JPEG magic
			wantErr: ErrProcessingFailed,
		},
	}

	preset, _ := GetPreset("avatar")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := proc.Process(tt.data, preset)
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
			assert.Nil(t, result)
		})
	}
}

func TestProcessor_Process_SupportsJPEG(t *testing.T) {
	proc := NewProcessor()
	srcData := createTestJPEG(t, 500, 500)
	preset, _ := GetPreset("avatar")

	result, err := proc.Process(srcData, preset)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify output is valid JPEG
	img, format, err := image.Decode(bytes.NewReader(result))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	assert.Equal(t, 1000, img.Bounds().Dx())
	assert.Equal(t, 1000, img.Bounds().Dy())
}

func TestProcessor_Process_SupportsPNG(t *testing.T) {
	proc := NewProcessor()
	srcData := createTestPNG(t, 500, 500)
	preset, _ := GetPreset("avatar")

	result, err := proc.Process(srcData, preset)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify output is valid JPEG (always output JPEG)
	img, format, err := image.Decode(bytes.NewReader(result))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	assert.Equal(t, 1000, img.Bounds().Dx())
	assert.Equal(t, 1000, img.Bounds().Dy())
}

func TestProcessor_Process_AlwaysOutputsJPEG(t *testing.T) {
	proc := NewProcessor()
	preset, _ := GetPreset("avatar")

	// Test with PNG input
	pngData := createTestPNG(t, 300, 300)
	result, err := proc.Process(pngData, preset)
	require.NoError(t, err)

	// Verify output is JPEG even when input is PNG
	_, format, err := image.Decode(bytes.NewReader(result))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format, "output should always be JPEG")
}

func TestProcessor_Interface(t *testing.T) {
	// Compile-time check that ImageProcessor implements Processor
	var _ Processor = (*ImageProcessor)(nil)
}

func TestNewProcessor(t *testing.T) {
	proc := NewProcessor()
	require.NotNil(t, proc)

	// Verify it's an *ImageProcessor
	_, ok := proc.(*ImageProcessor)
	assert.True(t, ok, "NewProcessor should return *ImageProcessor")
}
