package imageproxy

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // Register PNG decoder

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp" // Register WebP decoder
)

// Processor defines the interface for image processing operations.
type Processor interface {
	// Process transforms image data according to the preset configuration.
	// Returns the processed image as JPEG bytes, or an error if processing fails.
	Process(data []byte, preset Preset) ([]byte, error)
}

// ImageProcessor implements the Processor interface using the imaging library.
type ImageProcessor struct{}

// NewProcessor creates a new ImageProcessor instance.
func NewProcessor() Processor {
	return &ImageProcessor{}
}

// Process transforms the input image data according to the preset configuration.
// It handles both cover fit (crops to exact dimensions) and contain fit (preserves
// aspect ratio within bounds). Output is always JPEG format.
func (p *ImageProcessor) Process(data []byte, preset Preset) ([]byte, error) {
	// Check for empty or nil data
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty image data", ErrUnsupportedFormat)
	}

	// Decode the source image
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// Determine if this is a format issue or a corruption issue
		if isUnsupportedFormatError(err) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
		}
		return nil, fmt.Errorf("%w: failed to decode image: %v", ErrProcessingFailed, err)
	}

	// Validate that we decoded a supported format
	if format != "jpeg" && format != "png" && format != "webp" {
		return nil, fmt.Errorf("%w: format %s", ErrUnsupportedFormat, format)
	}

	// Process the image based on fit mode
	var processed image.Image
	switch preset.Fit {
	case FitCover:
		processed = processCover(img, preset.Width, preset.Height)
	case FitContain:
		processed = processContain(img, preset.Width, preset.Height)
	default:
		return nil, fmt.Errorf("%w: unknown fit mode", ErrProcessingFailed)
	}

	// Encode as JPEG
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, processed, &jpeg.Options{Quality: preset.Quality}); err != nil {
		return nil, fmt.Errorf("%w: failed to encode JPEG: %v", ErrProcessingFailed, err)
	}

	return buf.Bytes(), nil
}

// processCover scales and crops the image to exactly fill the target dimensions.
// The image is scaled to cover the entire target area, then cropped to exact size.
func processCover(img image.Image, width, height int) image.Image {
	// Use imaging.Fill which scales to cover and crops to exact dimensions
	return imaging.Fill(img, width, height, imaging.Center, imaging.Lanczos)
}

// processContain scales the image to fit within the target width while preserving
// aspect ratio. If the source image is smaller than the target, it is not upscaled.
// Height of 0 means scale proportionally based on width only.
func processContain(img image.Image, maxWidth, maxHeight int) image.Image {
	bounds := img.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()

	// Don't upscale images smaller than target
	if srcWidth <= maxWidth {
		return img
	}

	// Calculate new dimensions preserving aspect ratio
	newWidth := maxWidth
	newHeight := int(float64(srcHeight) * (float64(maxWidth) / float64(srcWidth)))

	// If maxHeight is specified and calculated height exceeds it,
	// scale based on height instead
	if maxHeight > 0 && newHeight > maxHeight {
		newHeight = maxHeight
		newWidth = int(float64(srcWidth) * (float64(maxHeight) / float64(srcHeight)))
	}

	return imaging.Resize(img, newWidth, newHeight, imaging.Lanczos)
}

// isUnsupportedFormatError checks if the error indicates an unsupported image format.
func isUnsupportedFormatError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return errStr == "image: unknown format" ||
		errStr == "invalid JPEG format: missing SOI marker" ||
		errStr == "invalid JPEG format: short segment" ||
		bytes.Contains([]byte(errStr), []byte("unknown format"))
}
