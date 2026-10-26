package imageproxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	_ "golang.org/x/image/webp" // Register WebP decoder

	"Coves/internal/core/media"
)

// Processor defines the interface for image processing operations.
type Processor interface {
	// Process transforms image data according to the preset configuration.
	// Returns the processed image bytes, or an error if processing fails.
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

// ImageProcessor implements the Processor interface.
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
// aspect ratio within bounds). PNG inputs produce PNG; GIF content_full retains
// its original bytes; other inputs produce JPEG.
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
		// image/gif registers the magic "GIF8?a", so any GIF8xa header reaches
		// its DecodeConfig; its errors are malformed input, never our failure.
		if len(data) >= 6 && bytes.HasPrefix(data, []byte("GIF8")) && data[5] == 'a' {
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
		}
		return nil, classifyDecodeError(err)
	}

	// Any package in the binary that imports image/gif (or another decoder)
	// registers it process-wide, so this allowlist is what keeps the proxy to
	// the four formats it budgets for. Decode below sees the same registry, so
	// one check covers both calls.
	if format != "jpeg" && format != "png" && format != "webp" && format != "gif" {
		return nil, fmt.Errorf("%w: format %s", ErrUnsupportedFormat, format)
	}

	// image/jpeg and x/image/webp hand back a zero side with a nil error and
	// leave the judgement to the caller; a zero-area image is malformed input,
	// not a failure of ours.
	if header.Width <= 0 || header.Height <= 0 {
		return nil, fmt.Errorf("%w: declared dimensions %dx%d", ErrUnsupportedFormat, header.Width, header.Height)
	}

	// Live bytes per processor slot, in source pixels (px) at the budget:
	//   - Decode: a progressive 4:4:4 JPEG holds three 4 B/px coefficient
	//     buffers alongside its 3 B/px frame, 15 B/px (750 MB at 50 MP). A
	//     16-bit PNG frame is 8 B/px.
	//   - Scale (media.ScaleImage): the decoded frame (≤ 8 B/px), box pre-reduction
	//     RGBA intermediates (the first ≤ 2 B/px, the next ≤ 1 B/px), the
	//     CatmullRom float buffer (≤ the 256 MiB kernel cap), and the
	//     4 B/px RGBA destination. A Height:0 contain of a source just wider
	//     than the preset is not reduced and its destination is up to about
	//     budget × 4 B (200 MB); that case is over the kernel cap, so it scales
	//     with ApproxBiLinear and allocates no float buffer. Its worst case, a
	//     16-bit PNG, is 12 B/px (600 MB); a pre-reduced source peaks near
	//     8 + 3 B/px plus the kernel cap (about 820 MB).
	//   - Orientation (JPEG only, 3 B/px frame) adds one destination-sized
	//     buffer to the scale phase.
	// So one slot peaks under about 850 MB at 50 MP, and the 4 default slots
	// under about 3.4 GB. int64 avoids header overflow.
	declaredPixels := int64(header.Width) * int64(header.Height)
	if declaredPixels > p.maxSourcePixels {
		return nil, fmt.Errorf("%w: %dx%d exceeds the %d-megapixel budget",
			ErrImageTooManyPixels, header.Width, header.Height, p.maxSourceMegapixels)
	}

	if format == "gif" {
		frames, err := countGIFFrames(data)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
		}
		if int64(frames) > p.maxSourcePixels/declaredPixels {
			return nil, fmt.Errorf("%w: %d GIF frames of %dx%d exceed the %d-megapixel budget",
				ErrImageTooManyPixels, frames, header.Width, header.Height, p.maxSourceMegapixels)
		}
		// content_full serves the source bytes without inflating their LZW
		// data. gif.DecodeAll is not a safe check here: it retains a palette
		// per frame (a 4 KB clone of a 256-entry global table whenever a frame
		// sets a transparent index), so a 1×1 screen with hundreds of
		// thousands of frames passes the pixel budget yet holds gigabytes.
		if preset.Name == "content_full" {
			return data, nil
		}
	}

	// Decode the source image from a fresh reader; DecodeConfig consumed the header.
	var img image.Image
	if format == "gif" {
		img, err = gif.Decode(bytes.NewReader(data))
	} else {
		img, _, err = image.Decode(bytes.NewReader(data))
	}
	if err != nil {
		if format == "gif" {
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
		}
		return nil, classifyDecodeError(err)
	}
	orientation := 1
	if format == "jpeg" {
		orientation = media.JPEGOrientation(data)
	}
	displayWidth, displayHeight := img.Bounds().Dx(), img.Bounds().Dy()
	if orientation >= 5 {
		displayWidth, displayHeight = displayHeight, displayWidth
	}

	// Process the image based on fit mode
	var processed image.Image
	switch preset.Fit {
	case FitCover:
		width, height := preset.Width, preset.Height
		if orientation >= 5 {
			width, height = height, width
		}
		processed = processCover(img, width, height)
	case FitContain:
		width, height := containSize(displayWidth, displayHeight, preset.Width, preset.Height)
		if width == displayWidth && height == displayHeight {
			processed = img
		} else {
			if orientation >= 5 {
				width, height = height, width
			}
			processed = resizeImage(img, width, height)
		}
	default:
		return nil, fmt.Errorf("%w: unknown fit mode", ErrProcessingFailed)
	}
	if orientation != 1 {
		processed = media.OrientImage(processed, orientation)
	}

	var buf bytes.Buffer
	if format == "png" {
		if err := png.Encode(&buf, processed); err != nil {
			return nil, fmt.Errorf("%w: failed to encode PNG: %v", ErrProcessingFailed, err)
		}
		return buf.Bytes(), nil
	}
	if err := jpeg.Encode(&buf, processed, &jpeg.Options{Quality: preset.Quality}); err != nil {
		return nil, fmt.Errorf("%w: failed to encode JPEG: %v", ErrProcessingFailed, err)
	}

	return buf.Bytes(), nil
}

// countGIFFrames walks the entire GIF container without inflating its LZW
// streams. Each image descriptor is a frame; extensions and image data consist
// of length-prefixed sub-blocks terminated by a zero-length block. Like
// image/gif, it rejects a frame that does not fit inside the logical screen,
// so frames × screen pixels bounds every frame's area.
func countGIFFrames(data []byte) (int, error) {
	if len(data) < 13 || (!bytes.HasPrefix(data, []byte("GIF87a")) && !bytes.HasPrefix(data, []byte("GIF89a"))) {
		return 0, fmt.Errorf("invalid GIF header")
	}
	screenWidth := int(binary.LittleEndian.Uint16(data[6:8]))
	screenHeight := int(binary.LittleEndian.Uint16(data[8:10]))
	position := 13 // header and logical screen descriptor
	skip := func(length int) bool {
		if length > len(data)-position {
			return false
		}
		position += length
		return true
	}
	skipSubBlocks := func() bool {
		for position < len(data) {
			length := int(data[position])
			position++
			if length == 0 {
				return true
			}
			if !skip(length) {
				return false
			}
		}
		return false
	}
	if data[10]&0x80 != 0 && !skip(6<<(data[10]&0x07)) {
		return 0, fmt.Errorf("truncated GIF global colour table")
	}
	frames := 0
	for position < len(data) {
		switch data[position] {
		case 0x3b: // trailer
			if frames == 0 || position != len(data)-1 {
				return 0, fmt.Errorf("invalid GIF trailer")
			}
			return frames, nil
		case 0x21: // extension introducer and label
			position++
			if !skip(1) || !skipSubBlocks() {
				return 0, fmt.Errorf("truncated GIF extension")
			}
		case 0x2c: // image descriptor, optional local colour table, LZW data
			position++
			if !skip(9) {
				return 0, fmt.Errorf("truncated GIF image descriptor")
			}
			descriptor := data[position-9 : position]
			left := int(binary.LittleEndian.Uint16(descriptor[0:2]))
			top := int(binary.LittleEndian.Uint16(descriptor[2:4]))
			width := int(binary.LittleEndian.Uint16(descriptor[4:6]))
			height := int(binary.LittleEndian.Uint16(descriptor[6:8]))
			if left+width > screenWidth || top+height > screenHeight {
				return 0, fmt.Errorf("GIF frame bounds larger than logical screen")
			}
			packed := descriptor[8]
			if packed&0x80 != 0 && !skip(6<<(packed&0x07)) {
				return 0, fmt.Errorf("truncated GIF local colour table")
			}
			if !skip(1) || !skipSubBlocks() {
				return 0, fmt.Errorf("truncated GIF image data")
			}
			frames++
		default:
			return 0, fmt.Errorf("invalid GIF block")
		}
	}
	return 0, fmt.Errorf("missing GIF trailer")
}

// processCover crops the source before scaling, avoiding a large intermediate
// image for extreme aspect ratios.
func processCover(img image.Image, width, height int) image.Image {
	bounds := img.Bounds()
	srcWidth, srcHeight := bounds.Dx(), bounds.Dy()
	cropWidth, cropHeight := srcWidth, srcHeight
	if int64(srcWidth)*int64(height) > int64(srcHeight)*int64(width) {
		cropWidth = max(1, int(int64(srcHeight)*int64(width)/int64(height)))
	} else {
		cropHeight = max(1, int(int64(srcWidth)*int64(height)/int64(width)))
	}
	crop := image.Rect(bounds.Min.X+(srcWidth-cropWidth)/2, bounds.Min.Y+(srcHeight-cropHeight)/2,
		bounds.Min.X+(srcWidth+cropWidth)/2, bounds.Min.Y+(srcHeight+cropHeight)/2)
	return media.ScaleImage(img, crop, width, height)
}

// containSize uses display dimensions, even when the JPEG pixels are rotated.
func containSize(srcWidth, srcHeight, maxWidth, maxHeight int) (int, int) {
	// Don't upscale images already within both bounds.
	if srcWidth <= maxWidth && (maxHeight <= 0 || srcHeight <= maxHeight) {
		return srcWidth, srcHeight
	}

	newWidth := min(srcWidth, maxWidth)
	newHeight := max(1, int((int64(srcHeight)*int64(newWidth)+int64(srcWidth)/2)/int64(srcWidth)))

	// If maxHeight is specified and calculated height exceeds it,
	// scale based on height instead
	if maxHeight > 0 && newHeight > maxHeight {
		newHeight = maxHeight
		newWidth = max(1, int((int64(srcWidth)*int64(maxHeight)+int64(srcHeight)/2)/int64(srcHeight)))
	}

	return newWidth, newHeight
}

func resizeImage(img image.Image, width, height int) image.Image {
	return media.ScaleImage(img, img.Bounds(), width, height)
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
