package imageproxy

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp" // Register WebP decoder
)

// Processor defines the interface for image processing operations.
type Processor interface {
	// Process transforms image data according to the preset configuration.
	// Returns the processed image as JPEG bytes, or an error if processing fails.
	Process(data []byte, preset Preset) ([]byte, error)
}

// DefaultMaxSourceMegapixels is the default upper bound on the pixel count
// (width × height) of a source image the processor will agree to decode.
const DefaultMaxSourceMegapixels = 50

// MaxSourceMegapixelsCeiling is the largest budget an operator may configure.
// It exists so a typo in an env var (an extra zero, a value meant as pixels)
// cannot silently re-open the OOM the budget closes: at the ceiling the
// ~19 B/px worst case is already ~19 GB per request, so anything above it is
// a misconfiguration rather than a choice. It also keeps the int64
// megapixel-to-pixel multiplication far from overflow.
const MaxSourceMegapixelsCeiling = 1000

// pixelsPerMegapixel converts the operator-facing megapixel budget into the
// raw pixel count the processor compares declared dimensions against.
const pixelsPerMegapixel int64 = 1_000_000

// ImageProcessor implements the Processor interface using the imaging library.
type ImageProcessor struct {
	// maxSourceMegapixels is the budget as the operator stated it, kept for
	// error messages so a refusal reads in the same unit the config uses.
	maxSourceMegapixels int

	// maxSourcePixels is the largest width × height this processor will
	// decode, held as int64 so the comparison cannot overflow on the
	// dimensions an attacker-controlled header may declare.
	maxSourcePixels int64
}

// NewProcessor creates a new ImageProcessor instance bounded to sources of at
// most maxSourceMegapixels. The budget must lie in 1..MaxSourceMegapixelsCeiling:
// zero would reject every image, a negative value has no meaning as a cap, and
// anything above the ceiling is a misconfiguration (see the const's comment).
func NewProcessor(maxSourceMegapixels int) (Processor, error) {
	if maxSourceMegapixels <= 0 || maxSourceMegapixels > MaxSourceMegapixelsCeiling {
		return nil, fmt.Errorf("%w: got %d, want 1..%d",
			ErrInvalidMaxSourceMegapixels, maxSourceMegapixels, MaxSourceMegapixelsCeiling)
	}
	return &ImageProcessor{
		maxSourceMegapixels: maxSourceMegapixels,
		maxSourcePixels:     int64(maxSourceMegapixels) * pixelsPerMegapixel,
	}, nil
}

// Process transforms the input image data according to the preset configuration.
// It handles both cover fit (crops to exact dimensions) and contain fit (preserves
// aspect ratio within bounds). Output is always JPEG format.
func (p *ImageProcessor) Process(data []byte, preset Preset) ([]byte, error) {
	// Check for empty or nil data
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty image data", ErrUnsupportedFormat)
	}

	// Read only the header first. The dimensions an image DECLARES decide how
	// much memory image.Decode allocates before it has verified a single pixel:
	// image/png sizes the full frame (4 B/px for 8-bit RGBA, 8 B/px for 16-bit)
	// in readImagePass before reading a byte of IDAT, so a header-only file
	// declaring 12000×12000 costs ~576 MB inside the decoder itself, from a few
	// dozen attacker-controlled bytes. The budget therefore has to be enforced
	// on the header, never on the decoded result.
	header, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, classifyDecodeError(err)
	}

	// Any package in the binary that imports image/gif (or another decoder)
	// registers it process-wide, so this allowlist is what keeps the proxy to
	// the three formats it budgets for. Decode below sees the same registry, so
	// one check covers both calls.
	if format != "jpeg" && format != "png" && format != "webp" {
		return nil, fmt.Errorf("%w: format %s", ErrUnsupportedFormat, format)
	}

	// image/jpeg and x/image/webp hand back a zero side with a nil error and
	// leave the judgement to the caller; a zero-area image is malformed input,
	// not a failure of ours.
	if header.Width <= 0 || header.Height <= 0 {
		return nil, fmt.Errorf("%w: declared dimensions %dx%d", ErrUnsupportedFormat, header.Width, header.Height)
	}

	// Cost model the budget is sized against. The worst case is a progressive
	// 4:4:4 JPEG at ~19 B/px: image/jpeg holds three int32 coefficient buffers
	// (3 × 4 B/px) alongside the 3 B/px YCbCr frame until EOI, and cover fit
	// then pays a further 4 B/px when imaging copies the frame for its crop
	// (contain fit allocates only the destination). At the 50 MP default that
	// is ~950 MB per request and ~3.8 GB across the default 4 slots; that is
	// the number the slot count must be read against. int64 arithmetic so two
	// 16-bit sides cannot overflow the comparison.
	declaredPixels := int64(header.Width) * int64(header.Height)
	if declaredPixels > p.maxSourcePixels {
		return nil, fmt.Errorf("%w: %dx%d exceeds the %d-megapixel budget",
			ErrImageTooManyPixels, header.Width, header.Height, p.maxSourceMegapixels)
	}

	// Decode the source image from a fresh reader; DecodeConfig consumed the header.
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, classifyDecodeError(err)
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

// classifyDecodeError maps an error from image.DecodeConfig or image.Decode
// onto this package's sentinels. Anything the decoders report about the BYTES
// (unknown format, truncation, bad CRC, impossible dimensions, missing pixel
// data) is the caller's malformed input and becomes ErrUnsupportedFormat, so
// the handler answers 400 and a stranger uploading garbage cannot page anyone
// through the 500-rate alert. Only errors outside that set are ours.
//
// Classification is by TYPE, not by message text: the decoders' strings differ
// per case and are not part of their API. x/image/webp returns plain errors,
// so its truncations arrive as the io sentinels.
func classifyDecodeError(err error) error {
	if isMalformedInputError(err) {
		return fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	return fmt.Errorf("%w: failed to decode image: %v", ErrProcessingFailed, err)
}

// isMalformedInputError reports whether err is one of the error kinds the
// registered decoders use to describe bad input rather than an internal fault.
func isMalformedInputError(err error) bool {
	if errors.Is(err, image.ErrFormat) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var (
		pngFormat       png.FormatError
		pngUnsupported  png.UnsupportedError
		jpegFormat      jpeg.FormatError
		jpegUnsupported jpeg.UnsupportedError
	)
	return errors.As(err, &pngFormat) ||
		errors.As(err, &pngUnsupported) ||
		errors.As(err, &jpegFormat) ||
		errors.As(err, &jpegUnsupported)
}
