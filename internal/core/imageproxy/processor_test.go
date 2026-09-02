package imageproxy

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"runtime"
	"strings"
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

// newTestProcessor builds a processor with the production default pixel budget.
func newTestProcessor(t *testing.T) Processor {
	t.Helper()
	proc, err := NewProcessor(DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	return proc
}

func TestProcessor_Process_CoverFit(t *testing.T) {
	proc := newTestProcessor(t)

	tests := []struct {
		name        string
		srcWidth    int
		srcHeight   int
		preset      Preset
		wantWidth   int
		wantHeight  int
		description string
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
	proc := newTestProcessor(t)

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
	proc := newTestProcessor(t)

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
			// A file that ends inside its own header is malformed input, not
			// a failure of ours; see TestProcessor_Process_MalformedHeaderIsClientError.
			name:    "truncated JPEG header",
			data:    []byte{0xFF, 0xD8, 0xFF, 0xE0}, // Partial JPEG magic
			wantErr: ErrUnsupportedFormat,
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
	proc := newTestProcessor(t)
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
	proc := newTestProcessor(t)
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
	proc := newTestProcessor(t)
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
	proc, err := NewProcessor(DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	require.NotNil(t, proc)

	// Verify it's an *ImageProcessor
	_, ok := proc.(*ImageProcessor)
	assert.True(t, ok, "NewProcessor should return *ImageProcessor")
}

func TestNewProcessor_RejectsNonPositiveMegapixelBudget(t *testing.T) {
	for _, budget := range []int{0, -1} {
		proc, err := NewProcessor(budget)
		require.Error(t, err, "NewProcessor(%d) must refuse a budget that would admit nothing or everything", budget)
		assert.True(t, errors.Is(err, ErrInvalidMaxSourceMegapixels),
			"NewProcessor(%d) error must wrap ErrInvalidMaxSourceMegapixels, got %v", budget, err)
		assert.Nil(t, proc, "NewProcessor(%d) must not hand back a processor alongside an error", budget)
	}
}

func TestNewProcessor_AcceptsSmallestPositiveBudget(t *testing.T) {
	proc, err := NewProcessor(1)
	require.NoError(t, err)
	require.NotNil(t, proc)
}

func TestDefaultMaxSourceMegapixels(t *testing.T) {
	// The audit asked for "a few tens of megapixels"; 50 is the value chosen
	// here. At the 12 B/px worst case (16-bit RGBA PNG plus imaging's NRGBA
	// clone) it is ~600 MB per request, which the concurrency cap in Config
	// is what keeps survivable.
	assert.Equal(t, 50, DefaultMaxSourceMegapixels)
}

// newProcessorWithBudget builds a processor with an explicit megapixel budget.
// Boundary tests use tiny budgets so the "just over" case is a real image of
// about a megapixel rather than the 50 MP default nobody can afford to encode.
func newProcessorWithBudget(t *testing.T, megapixels int) Processor {
	t.Helper()
	proc, err := NewProcessor(megapixels)
	require.NoError(t, err)
	return proc
}

// measureProcessAlloc runs Process once and reports the bytes the whole
// process allocated during the call. TotalAlloc is cumulative and monotonic,
// so the delta counts every allocation made while Process ran regardless of
// GC; the runtime.GC() beforehand only keeps a collection from being triggered
// mid-call and skewing timing, it does not isolate the number. What isolates it
// is scheduling: Go runs sequential top-level tests to completion before
// releasing parallel ones, so a caller that is not t.Parallel (and not a
// subtest of a parallel parent) is the only goroutine allocating.
func measureProcessAlloc(proc Processor, data []byte, preset Preset) ([]byte, uint64, error) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	result, err := proc.Process(data, preset)
	runtime.ReadMemStats(&after)
	return result, after.TotalAlloc - before.TotalAlloc, err
}

// budgetAllocBound is the most a refused image may cost. The smallest bomb in
// these tests would allocate 36 MB (3000×3000×4) if decoded, so 8 MiB leaves
// room for the decoder's own bookkeeping while still catching a decode.
const budgetAllocBound = 8 << 20

// TestProcessor_Process_RefusesOverBudgetWithoutDecoding is the unit-level
// statement of SECURITY_AUDIT_2026-09-01 §1.1: a header that declares more
// pixels than the budget must be refused from the header alone. The sentinel
// matters because the handler maps ErrImageTooManyPixels to a 400 and
// ErrProcessingFailed to a 500, and because ErrImageTooLarge already means the
// fetcher's byte cap, which is a different resource. The error value carries
// the declared dimensions and the budget so the handler's log line can name
// them and an operator can tell a bomb from a corrupt file.
func TestProcessor_Process_RefusesOverBudgetWithoutDecoding(t *testing.T) {
	proc := newProcessorWithBudget(t, 1)
	preset, err := GetPreset("avatar")
	require.NoError(t, err)

	bomb := pngHeader(t, 3000, 3000)

	result, allocated, err := measureProcessAlloc(proc, bomb, preset)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrImageTooManyPixels)
	assert.NotErrorIs(t, err, ErrImageTooLarge,
		"the pixel budget must not borrow the fetcher's byte-cap sentinel; a log reader could not tell which limit tripped")
	assert.NotErrorIs(t, err, ErrProcessingFailed,
		"an over-budget image is a refusal, not a processing failure; the handler turns the latter into a 500")
	assert.Truef(t, strings.Contains(err.Error(), "3000x3000"),
		"error should name the declared dimensions, got %q", err.Error())
	assert.Truef(t, strings.Contains(err.Error(), "1-megapixel budget"),
		"error should state the budget in megapixels, e.g. \"exceeds the 1-megapixel budget\", got %q", err.Error())
	assert.Lessf(t, allocated, uint64(budgetAllocBound),
		"Process allocated %d bytes (%.1f MiB) for a %d-byte input; the declared 3000x3000 frame was decoded",
		allocated, float64(allocated)/(1<<20), len(bomb))
}

// TestProcessor_Process_BudgetBoundary pins the budget to the configured value
// with real encoded images on both sides of it. A hardcoded cap, or an
// off-by-one that rejects exactly-at-budget, fails here while the bomb test
// alone would still pass.
func TestProcessor_Process_BudgetBoundary(t *testing.T) {
	preset, err := GetPreset("avatar")
	require.NoError(t, err)

	tests := []struct {
		name            string
		budgetMegapixel int
		width, height   int
		wantErr         error
	}{
		{name: "exactly at a 1 MP budget succeeds", budgetMegapixel: 1, width: 1000, height: 1000},
		{name: "one pixel column over a 1 MP budget is refused", budgetMegapixel: 1, width: 1001, height: 1000, wantErr: ErrImageTooManyPixels},
		{name: "the same image under a 2 MP budget succeeds", budgetMegapixel: 2, width: 1001, height: 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := newProcessorWithBudget(t, tt.budgetMegapixel)
			src := createTestJPEG(t, tt.width, tt.height)

			result, err := proc.Process(src, preset)

			if tt.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.wantErr)
				assert.NotErrorIs(t, err, ErrImageTooLarge)
				assert.Nil(t, result)
				return
			}
			require.NoError(t, err)
			_, format, decodeErr := image.DecodeConfig(bytes.NewReader(result))
			require.NoError(t, decodeErr)
			assert.Equal(t, "jpeg", format)
		})
	}
}

// TestProcessor_Process_RefusesOverBudgetEveryFormat runs the budget against
// the two decoders the PNG bomb test above does not cover. The decoders fail
// differently when handed a header-only file: PNG and WebP allocate the whole
// frame before noticing there is no data, JPEG hits EOF first and allocates
// nothing. Only a check that reads the header BEFORE decoding produces the
// same sentinel and the same near-zero allocation for every format.
func TestProcessor_Process_RefusesOverBudgetEveryFormat(t *testing.T) {
	proc := newTestProcessor(t)
	preset, err := GetPreset("avatar")
	require.NoError(t, err)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "jpeg", data: jpegHeader(t, 12000, 12000)},
		{name: "webp", data: webpHeader(t, 12000, 12000)},
	}

	for _, tt := range tests {
		// Not t.Parallel: the allocation measurement is process-wide.
		t.Run(tt.name, func(t *testing.T) {
			result, allocated, err := measureProcessAlloc(proc, tt.data, preset)

			require.Error(t, err)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrImageTooManyPixels)
			assert.NotErrorIs(t, err, ErrImageTooLarge)
			assert.NotErrorIs(t, err, ErrProcessingFailed)
			assert.Truef(t, strings.Contains(err.Error(), "12000x12000"),
				"error should name the declared dimensions, got %q", err.Error())
			assert.Truef(t, strings.Contains(err.Error(), "50-megapixel budget"),
				"error should state the default budget in megapixels, got %q", err.Error())
			assert.Lessf(t, allocated, uint64(budgetAllocBound),
				"Process allocated %d bytes (%.1f MiB) for a %d-byte %s input; the declared frame was decoded",
				allocated, float64(allocated)/(1<<20), len(tt.data), tt.name)
		})
	}
}

// TestProcessor_Process_RejectsZeroDimensionAsUnsupported covers the headers
// image/jpeg and x/image/webp accept with a zero side and a nil error. Those
// decoders leave it to the caller, so the processor's own dimension check must
// refuse them, and as a malformed input (400) rather than an internal failure
// (500). PNG is deliberately absent: its decoder rejects a zero dimension
// itself and that pre-existing mapping is left alone.
func TestProcessor_Process_RejectsZeroDimensionAsUnsupported(t *testing.T) {
	proc := newTestProcessor(t)
	preset, err := GetPreset("avatar")
	require.NoError(t, err)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "jpeg with zero height", data: jpegHeader(t, 100, 0)},
		{name: "webp with zero width", data: webpHeader(t, 0, 100)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := proc.Process(tt.data, preset)

			require.Error(t, err)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrUnsupportedFormat)
			assert.NotErrorIs(t, err, ErrProcessingFailed,
				"a zero-dimension header is a malformed input, not a processing failure")
		})
	}
}

// TestProcessor_Process_MalformedHeaderIsClientError pins every way a header
// can be broken to ErrUnsupportedFormat. The handler serves that as a 400 and
// ErrProcessingFailed as a 500, and a 500 is wrong twice over for these: it
// tells the client the server is at fault when the bytes are, and it lands in
// error-rate alerts where a stranger uploading garbage can page someone. The
// decoder's own error strings differ per case, so the processor has to
// classify by error TYPE (png.FormatError, io.ErrUnexpectedEOF, ...) rather
// than by matching text, which is also why the strings are not asserted here.
func TestProcessor_Process_MalformedHeaderIsClientError(t *testing.T) {
	proc := newTestProcessor(t)
	preset, err := GetPreset("avatar")
	require.NoError(t, err)

	tests := []struct {
		name string
		data []byte
	}{
		// DecodeConfig fails: unexpected EOF inside the JPEG segment table.
		{name: "jpeg truncated after SOI and APP0 marker", data: []byte{0xFF, 0xD8, 0xFF, 0xE0}},
		// DecodeConfig fails: unexpected EOF, 12 of 13 IHDR bytes present.
		{name: "png truncated inside IHDR", data: pngTruncatedIHDR(t, 12)},
		// DecodeConfig fails: png.FormatError "invalid checksum".
		{name: "png IHDR with wrong CRC", data: pngBadCRC(t, 100, 100)},
		// DecodeConfig fails: png.FormatError "non-positive dimension".
		{name: "png IHDR declaring width 0", data: pngHeader(t, 0, 100)},
		// DecodeConfig fails: png.UnsupportedError "dimension overflow". The
		// decoder refuses before sizing anything, so this stays cheap; the
		// allocation bound below is what proves that.
		{name: "png IHDR declaring 0x7fffffff x 0x7fffffff", data: pngHeader(t, 0x7fffffff, 0x7fffffff)},
		// DecodeConfig fails: io.EOF inside the VP8 chunk, before the start code.
		{name: "webp truncated inside the VP8 chunk", data: webpTruncated(t)},
		// DecodeConfig succeeds and the budget passes; Decode then fails with
		// png.FormatError "not enough pixel data". Corruption past the header
		// is still the client's bytes, not our fault.
		{name: "png under budget with undecodable IDAT", data: pngHeader(t, 100, 100)},
	}

	for _, tt := range tests {
		// Not t.Parallel: the allocation measurement is process-wide.
		t.Run(tt.name, func(t *testing.T) {
			result, allocated, err := measureProcessAlloc(proc, tt.data, preset)

			require.Error(t, err)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrUnsupportedFormat)
			assert.NotErrorIs(t, err, ErrProcessingFailed,
				"a malformed header is the client's bytes, not a failure of ours")
			assert.Lessf(t, allocated, uint64(budgetAllocBound),
				"Process allocated %d bytes (%.1f MiB) for a %d-byte malformed input", allocated, float64(allocated)/(1<<20), len(tt.data))
		})
	}
}

// TestProcessor_Process_UnknownFitModeIsProcessingFailure keeps
// ErrProcessingFailed meaningful now that malformed input no longer produces
// it: a preset the code cannot handle is OUR bug, and a 500 is the honest
// answer for that.
func TestProcessor_Process_UnknownFitModeIsProcessingFailure(t *testing.T) {
	proc := newTestProcessor(t)
	bogus := Preset{Name: "bogus", Width: 100, Height: 100, Fit: FitMode("bogus"), Quality: 85}

	result, err := proc.Process(createTestJPEG(t, 200, 200), bogus)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrProcessingFailed)
	assert.NotErrorIs(t, err, ErrUnsupportedFormat)
}

// TestNewProcessor_BudgetCeiling: an operator can raise the budget, but not
// past the point where the concurrency cap stops meaning anything. At the
// ceiling the 12 B/px worst case is 12 GB per request.
func TestNewProcessor_BudgetCeiling(t *testing.T) {
	assert.Equal(t, 1000, MaxSourceMegapixelsCeiling)

	proc, err := NewProcessor(MaxSourceMegapixelsCeiling)
	require.NoError(t, err, "the ceiling itself is a legal budget")
	require.NotNil(t, proc)

	proc, err = NewProcessor(MaxSourceMegapixelsCeiling + 1)
	require.Error(t, err, "one over the ceiling must be refused")
	assert.ErrorIs(t, err, ErrInvalidMaxSourceMegapixels)
	assert.Nil(t, proc)
}
