// Package media is the ingest pipeline for uploaded images.
package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
	"golang.org/x/sync/semaphore"
)

type Format string

const (
	FormatJPEG Format = "image/jpeg"
	FormatPNG  Format = "image/png"
	FormatWebP Format = "image/webp"
	FormatGIF  Format = "image/gif"
)

type ErrorCode string

const (
	ErrorCodeUnsupportedImageFormat  ErrorCode = "UnsupportedImageFormat"
	ErrorCodeImageDimensionsTooLarge ErrorCode = "ImageDimensionsTooLarge"
	ErrorCodeInvalidImage            ErrorCode = "InvalidImage"
	ErrorCodeImageTooLarge           ErrorCode = "ImageTooLarge"
)

type Error struct {
	Code ErrorCode
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

const DefaultMaxConcurrent = 4

// DefaultMaxLongEdge is the longest edge, in pixels, Upload stores; a larger
// image is downscaled to it.
const DefaultMaxLongEdge = 4096

var ErrInvalidConfig = errors.New("invalid media upload configuration")

type Config struct {
	MaxConcurrent int
	MaxLongEdge   int
}

type Pipeline struct {
	decodeSlots *semaphore.Weighted
	maxLongEdge int
	// beforeOrientedEncode is a test hook, nil in production, run between
	// orienting a decoded JPEG and re-encoding it.
	beforeOrientedEncode func()
	// beforeDownscaleEncode is a test hook, nil in production, run between
	// downscaling a decoded upload and re-encoding it.
	beforeDownscaleEncode func()
}

func NewPipeline(cfg Config) (*Pipeline, error) {
	if cfg.MaxConcurrent < 1 {
		return nil, fmt.Errorf("%w: max concurrency must be positive: %d", ErrInvalidConfig, cfg.MaxConcurrent)
	}
	if cfg.MaxLongEdge < 1 {
		return nil, fmt.Errorf("%w: max long edge must be positive: %d", ErrInvalidConfig, cfg.MaxLongEdge)
	}
	return &Pipeline{decodeSlots: semaphore.NewWeighted(int64(cfg.MaxConcurrent)), maxLongEdge: cfg.MaxLongEdge}, nil
}

type Input struct {
	Data                []byte
	DeclaredContentType string
	Accept              []Format
}

type Result struct {
	Data   []byte
	Format Format
	Width  int
	Height int
}

const (
	maxSourcePixels int64 = 50_000_000
	maxOutputBytes  int   = 10_000_000
)

// Ingest sanitizes in.Data at its own dimensions; avatars and banners use it.
func (p *Pipeline) Ingest(ctx context.Context, in Input) (Result, error) {
	return p.ingest(ctx, in, 0)
}

// ingest sanitizes in.Data and, when maxLongEdge is positive, downscales an
// image whose long edge after orientation exceeds it, except an animated GIF or
// PNG. maxLongEdge 0 keeps the image's dimensions.
func (p *Pipeline) ingest(ctx context.Context, in Input, maxLongEdge int) (Result, error) {
	format := sniffFormat(in.Data)
	accepted := false
	for _, allowed := range in.Accept {
		if format != "" && allowed == format {
			accepted = true
			break
		}
	}
	if !accepted {
		return Result{}, &Error{Code: ErrorCodeUnsupportedImageFormat}
	}

	header, _, err := image.DecodeConfig(bytes.NewReader(in.Data))
	if err != nil {
		return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
	}
	if header.Width < 1 || header.Height < 1 {
		return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: fmt.Errorf("invalid dimensions %dx%d", header.Width, header.Height)}
	}
	if int64(header.Width) > maxSourcePixels/int64(header.Height) {
		return Result{}, &Error{Code: ErrorCodeImageDimensionsTooLarge}
	}

	if format == FormatJPEG {
		// Walk the marker structure before taking a slot, so a file with more
		// scans than maxJPEGScans never reaches the unbounded decoder.
		if err := walkJPEG(in.Data, func(byte, []byte) {}, nil); err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
	}
	if format == FormatGIF {
		// Walk the container before taking a slot and bound frames × logical
		// screen pixels, so a small file cannot inflate into many full frames.
		frames, err := gifFrameCount(in.Data)
		if err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
		if int64(frames) > maxSourcePixels/(int64(header.Width)*int64(header.Height)) {
			return Result{}, &Error{Code: ErrorCodeImageDimensionsTooLarge}
		}
	}

	release, err := p.acquireDecodeSlot(ctx)
	if err != nil {
		return Result{}, err
	}
	// Hold the slot through orientation, downscaling and re-encode, which
	// allocate further buffers while the decoded image is live. Deferred so a
	// decoder panic cannot leak the slot.
	defer release()
	if format == FormatGIF {
		// Every frame is inflated to validate it, but none is kept; GIFs are
		// not resized, so the result has the logical screen's dimensions.
		clean, err := sanitizeGIF(in.Data)
		if err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
		if len(clean) > maxOutputBytes {
			return Result{}, &Error{Code: ErrorCodeImageTooLarge}
		}
		return Result{Data: clean, Format: FormatGIF, Width: header.Width, Height: header.Height}, nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(in.Data))
	if err != nil {
		return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
	}

	result := Result{Data: in.Data, Format: format, Width: decoded.Bounds().Dx(), Height: decoded.Bounds().Dy()}
	orientation := 1
	var webpICCProfile []byte
	switch format {
	case FormatJPEG:
		orientation = JPEGOrientation(in.Data)
	case FormatWebP:
		result.Data, orientation, webpICCProfile, err = stripWebPMetadata(in.Data)
		if err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
	}
	displayWidth, displayHeight := result.Width, result.Height
	if orientation >= 5 {
		displayWidth, displayHeight = displayHeight, displayWidth
	}

	downscaling := maxLongEdge > 0 && max(displayWidth, displayHeight) > maxLongEdge
	if downscaling && format == FormatPNG {
		// image/png decodes only an APNG's default image, so an animated PNG
		// keeps its dimensions, as a GIF does; the 10 MB cap still bounds it.
		var animated bool
		if _, animated, err = pngChunksBeforeImageData(in.Data); err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
		downscaling = !animated
	}
	if downscaling {
		width, height := downscaledSize(displayWidth, displayHeight, maxLongEdge)
		result, err = p.downscale(decoded, in.Data, format, orientation, webpICCProfile, width, height)
		if err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
	} else if format == FormatJPEG {
		if orientation == 1 {
			result.Data, err = stripJPEGMetadata(in.Data)
		} else {
			oriented := OrientImage(decoded, orientation)
			result.Width, result.Height = oriented.Bounds().Dx(), oriented.Bounds().Dy()
			if p.beforeOrientedEncode != nil {
				p.beforeOrientedEncode()
			}
			result.Data, err = encodeOrientedJPEG(oriented, in.Data)
		}
		if err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
	} else if format == FormatPNG {
		result.Data, err = stripPNGMetadata(in.Data)
		if err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
	} else if format == FormatWebP && orientation != 1 {
		result, err = encodeOrientedWebP(decoded, orientation, webpICCProfile)
		if err != nil {
			return Result{}, &Error{Code: ErrorCodeInvalidImage, Err: err}
		}
	}
	if len(result.Data) > maxOutputBytes {
		return Result{}, &Error{Code: ErrorCodeImageTooLarge}
	}
	return result, nil
}

// downscaledJPEGQuality is the JPEG quality of a downscaled upload.
const downscaledJPEGQuality = 90

// downscaledSize keeps the aspect ratio with the long edge at maxLongEdge,
// rounding the short edge and keeping it at least one pixel.
func downscaledSize(width, height, maxLongEdge int) (int, int) {
	if width >= height {
		return maxLongEdge, max(1, int((int64(height)*int64(maxLongEdge)+int64(width)/2)/int64(width)))
	}
	return max(1, int((int64(width)*int64(maxLongEdge)+int64(height)/2)/int64(height))), maxLongEdge
}

// downscale scales decoded to width × height in display orientation and
// re-encodes it once, keeping only the colour metadata (the ICC profile, and
// for PNG also gAMA, cHRM, sRGB, cICP, mDCv and cLLi): JPEG as quality-90
// JPEG, PNG as PNG, and WebP, which stdlib cannot write, as quality-90 JPEG,
// or PNG when any pixel is not fully opaque.
func (p *Pipeline) downscale(decoded image.Image, original []byte, format Format, orientation int, webpICCProfile []byte, width, height int) (Result, error) {
	// Scale in stored orientation, then orient the smaller image.
	scaledWidth, scaledHeight := width, height
	if orientation >= 5 {
		scaledWidth, scaledHeight = height, width
	}
	var output image.Image = scaleUploadImage(decoded, scaledWidth, scaledHeight)
	if orientation != 1 {
		output = OrientImage(output, orientation)
	}
	if p.beforeDownscaleEncode != nil {
		p.beforeDownscaleEncode()
	}

	result := Result{Format: format, Width: width, Height: height}
	var segments [][]byte
	var err error
	switch {
	case format == FormatJPEG:
		if segments, err = retainedJPEGReencodeSegments(original); err != nil {
			return Result{}, err
		}
		result.Data, err = encodeJPEGWithSegments(output, segments, downscaledJPEGQuality)
	case format == FormatPNG:
		var colorChunks []byte
		if colorChunks, _, err = pngChunksBeforeImageData(original); err != nil {
			return Result{}, err
		}
		result.Data, err = encodePNGWithChunk(output, colorChunks)
	case format == FormatWebP && !isOpaque(decoded):
		result.Format = FormatPNG
		result.Data, err = encodePNGWithICCProfile(output, webpICCProfile)
	case format == FormatWebP:
		if segments, err = iccProfileSegments(webpICCProfile); err != nil {
			return Result{}, err
		}
		result.Format = FormatJPEG
		result.Data, err = encodeJPEGWithSegments(output, segments, downscaledJPEGQuality)
	default:
		return Result{}, fmt.Errorf("cannot downscale %s", format)
	}
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// isOpaque reports whether every decoded pixel is fully opaque. It is judged
// on the decoded image, not the scaled one, whose interpolated alpha could
// round down; every image type x/image/webp returns reports it.
func isOpaque(decoded image.Image) bool {
	opaque, reportsOpacity := decoded.(interface{ Opaque() bool })
	return reportsOpacity && opaque.Opaque()
}

func (p *Pipeline) acquireDecodeSlot(ctx context.Context) (release func(), err error) {
	if err := p.decodeSlots.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return func() { p.decodeSlots.Release(1) }, nil
}

func sniffFormat(data []byte) Format {
	switch {
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		return FormatJPEG
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return FormatPNG
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return FormatWebP
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return FormatGIF
	default:
		return ""
	}
}
