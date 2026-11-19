package imageproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/webp"
)

// webpImagePayloadFourCC returns the FourCC of the chunk holding the image
// bitstream: the first chunk of a simple file, or the first "VP8 " or "VP8L"
// chunk after a VP8X extended header.
func webpImagePayloadFourCC(t *testing.T, data []byte) string {
	t.Helper()
	offset := 12
	for offset+8 <= len(data) {
		fourCC := string(data[offset : offset+4])
		if fourCC == "VP8 " || fourCC == "VP8L" {
			return fourCC
		}
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8 + size + size%2
	}
	t.Fatalf("no VP8 or VP8L image chunk in %d-byte WebP", len(data))
	return ""
}

// assertLossyWebP checks that data is a lossy WebP of exactly width x height
// and returns the decoded image.
func assertLossyWebP(t *testing.T, data []byte, width, height int) image.Image {
	t.Helper()
	require.GreaterOrEqual(t, len(data), 12, "output too short to be WebP")
	require.True(t, string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP",
		"expected WebP magic (RIFF....WEBP), got % x", data[:12])
	require.Equal(t, "VP8 ", webpImagePayloadFourCC(t, data), "expected a lossy VP8 image chunk, not lossless VP8L")
	img, err := webp.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, width, img.Bounds().Dx(), "decoded width")
	require.Equal(t, height, img.Bounds().Dy(), "decoded height")
	return img
}

// texturedTestPNG is a deterministic photographic-style opaque PNG: diagonal
// colour gradients overlaid with seeded noise, which PNG compresses poorly.
func texturedTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	random := rand.New(rand.NewSource(20261117))
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	clamp := func(value int) uint8 { return uint8(max(0, min(255, value))) }
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			noise := random.Intn(41) - 20
			img.SetNRGBA(x, y, color.NRGBA{
				R: clamp(x*255/width + noise),
				G: clamp(y*255/height + random.Intn(41) - 20),
				B: clamp((x+y)*255/(width+height) + noise/2),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

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
			preset:      Preset{Name: "avatar_small", Width: 192, Height: 192, Fit: FitCover, Quality: 80},
			wantWidth:   192,
			wantHeight:  192,
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

			assertLossyWebP(t, result, tt.wantWidth, tt.wantHeight)
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

			assertLossyWebP(t, result, tt.wantMaxWidth, tt.wantMaxHeight)
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
		{
			// Must be exact-capacity: slicing data[8:12] past len but within
			// capacity would not panic, so a []byte("...") literal, which may be
			// over-allocated, would not prove the length guard in Process.
			name: "RIFF blob shorter than the WEBP form type",
			data: func() []byte {
				data := make([]byte, 11)
				copy(data, "RIFF\x00\x00\x00\x00WEB")
				return data
			}(),
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

// TestProcessor_Process_EncodesLossyWebP covers every re-encoded source
// format: each comes out as lossy WebP at the preset's size, for both fits.
func TestProcessor_Process_EncodesLossyWebP(t *testing.T) {
	stillWebP, err := os.ReadFile("testdata/still-lossless-1600x1200.webp")
	require.NoError(t, err)
	sources := []struct {
		name string
		data []byte
	}{
		{name: "jpeg", data: createTestJPEG(t, 1600, 1200)},
		{name: "opaque png", data: createTestPNG(t, 1600, 1200)},
		{name: "still lossless webp", data: stillWebP},
		{name: "still gif", data: animatedGIF(t, 1600, 1200, 1)},
	}
	fits := []struct {
		presetName    string
		width, height int
	}{
		{presetName: "avatar", width: 1000, height: 1000},
		{presetName: "content_preview", width: 800, height: 600},
	}
	proc := newTestProcessor(t)
	for _, source := range sources {
		for _, fit := range fits {
			t.Run(source.name+" "+fit.presetName, func(t *testing.T) {
				preset, err := GetPreset(fit.presetName)
				require.NoError(t, err)
				result, err := proc.Process(source.data, preset)
				require.NoError(t, err)
				assertLossyWebP(t, result, fit.width, fit.height)
			})
		}
	}
}

// TestProcessor_Process_TexturedPNGShrinks re-encodes a photographic-style PNG
// at its own size, so any saving comes from the output format alone.
func TestProcessor_Process_TexturedPNGShrinks(t *testing.T) {
	source := texturedTestPNG(t, 512, 384)
	preset := Preset{Name: "texture", Width: 512, Height: 384, Fit: FitContain, Quality: 80}
	result, err := newTestProcessor(t).Process(source, preset)
	require.NoError(t, err)
	assert.Less(t, len(result), len(source), "output must be smaller than the source PNG")
}

func TestProcessor_Process_QualityControlsSize(t *testing.T) {
	source := texturedTestPNG(t, 512, 384)
	proc := newTestProcessor(t)
	low, err := proc.Process(source, Preset{Name: "texture", Width: 512, Height: 384, Fit: FitContain, Quality: 30})
	require.NoError(t, err)
	high, err := proc.Process(source, Preset{Name: "texture", Width: 512, Height: 384, Fit: FitContain, Quality: 95})
	require.NoError(t, err)
	assert.Less(t, len(low), len(high), "quality 30 must produce a smaller file than quality 95")
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
	// here. The per-request worst case is documented in Process (processor.go);
	// the concurrency cap in Config is what keeps it survivable.
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
// statement of the decode budget (docs/MEDIA.md, Decode budget): a header that declares more
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
			assert.Equal(t, "webp", format)
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

// webpChunk frames payload as a RIFF chunk: FourCC, little-endian size, and the
// pad byte RIFF requires after an odd-sized payload.
func webpChunk(fourCC string, payload []byte) []byte {
	chunk := append([]byte(fourCC), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
	chunk = append(chunk, payload...)
	if len(payload)%2 == 1 {
		chunk = append(chunk, 0)
	}
	return chunk
}

// webpExtended builds a RIFF/WEBP file holding a VP8X header with the given
// flags and canvas size, followed by chunks.
func webpExtended(flags byte, canvasWidth, canvasHeight uint32, chunks ...[]byte) []byte {
	vp8x := make([]byte, 0, 10)
	vp8x = append(vp8x, flags, 0, 0, 0)
	vp8x = append(vp8x, byte(canvasWidth-1), byte((canvasWidth-1)>>8), byte((canvasWidth-1)>>16))
	vp8x = append(vp8x, byte(canvasHeight-1), byte((canvasHeight-1)>>8), byte((canvasHeight-1)>>16))

	body := append([]byte("WEBP"), webpChunk("VP8X", vp8x)...)
	for _, chunk := range chunks {
		body = append(body, chunk...)
	}
	file := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	return append(file, body...)
}

// webpAnimated builds a 150x150 animated WebP with the layout real encoders
// write: VP8X with the animation flag, ANIM, then one ANMF frame.
func webpAnimated() []byte {
	const animationFlag = 0x02

	// Background colour (4 bytes) and loop count (2 bytes, 0 = forever).
	anim := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0, 0}
	// Frame X and Y (3 bytes each), width-1 and height-1 (3 bytes each),
	// duration (3 bytes) and flags (1 byte), then the frame's image chunk.
	anmf := []byte{
		0, 0, 0,
		0, 0, 0,
		149, 0, 0,
		149, 0, 0,
		60, 0, 0,
		0x02,
	}
	anmf = append(anmf, webpChunk("VP8 ", []byte{0x50, 0x53, 0x00, 0x9D, 0x01, 0x2A, 0x96, 0x00, 0x96, 0x00})...)

	return webpExtended(animationFlag, 150, 150, webpChunk("ANIM", anim), webpChunk("ANMF", anmf))
}

// TestProcessor_Process_WebPWithoutDecodableFrameIsClientError pins WebP files
// whose extended header is valid but which hold no frame the decoder can read.
// x/image/webp reads the size from VP8X, so the header check passes, and then
// reports a plain error once pixel decoding runs out of chunks. These are
// bytes we cannot serve, not a failure of ours, so they must not reach the
// handler as a 500.
func TestProcessor_Process_WebPWithoutDecodableFrameIsClientError(t *testing.T) {
	proc := newTestProcessor(t)
	preset, err := GetPreset("avatar")
	require.NoError(t, err)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "animated webp", data: webpAnimated()},
		// No animation flag and no ANIM chunk, so nothing marks this as
		// animated: the file simply has no image chunk after its header.
		{
			name: "non-animated VP8X with only an unknown chunk",
			data: webpExtended(0, 150, 150, webpChunk("JUNK", []byte{1, 2, 3, 4})),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, format, err := image.DecodeConfig(bytes.NewReader(tt.data))
			require.NoError(t, err, "the fixture must pass header parsing so the failure comes from pixel decoding")
			require.Equal(t, "webp", format)
			require.Equal(t, 150, config.Width)
			require.Equal(t, 150, config.Height)

			result, err := proc.Process(tt.data, preset)

			require.Error(t, err)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrUnsupportedFormat)
			assert.NotErrorIs(t, err, ErrProcessingFailed,
				"an undecodable WebP is the client's bytes, not a failure of ours")
		})
	}
}

// webpAnimatedDecodableFrame builds a 1x1 animated WebP whose single ANMF frame
// holds a decodable VP8L image, so a first-frame decoder can read it.
func webpAnimatedDecodableFrame(t *testing.T) []byte {
	t.Helper()
	const animationFlag = 0x02

	lossless, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	require.NoError(t, err)
	require.Equal(t, "VP8L", string(lossless[12:16]), "fixture image chunk")

	// Background colour (4 bytes) and loop count (2 bytes, 0 = forever).
	anim := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0, 0}
	// Frame X and Y (3 bytes each), width-1 and height-1 (3 bytes each),
	// duration (3 bytes) and flags (1 byte), then the frame's image chunk.
	anmf := []byte{
		0, 0, 0,
		0, 0, 0,
		0, 0, 0,
		0, 0, 0,
		60, 0, 0,
		0x02,
	}
	anmf = append(anmf, lossless[12:]...)

	return webpExtended(animationFlag, 1, 1, webpChunk("ANIM", anim), webpChunk("ANMF", anmf))
}

// TestProcessor_Process_AnimatedWebPWithDecodableFrameIsRejected pins WebP
// decoding to x/image/webp. The encoder package registers a "webp" decoder
// that reads an animated file's first frame, so a processor using the global
// registry would accept this file.
func TestProcessor_Process_AnimatedWebPWithDecodableFrameIsRejected(t *testing.T) {
	data := webpAnimatedDecodableFrame(t)
	frame, format, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err, "the global registry must decode the fixture so the rejection comes from the pinned decoder")
	require.Equal(t, "webp", format)
	require.Equal(t, 1, frame.Bounds().Dx())
	require.Equal(t, 1, frame.Bounds().Dy())

	preset, err := GetPreset("avatar")
	require.NoError(t, err)
	result, err := newTestProcessor(t).Process(data, preset)

	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrUnsupportedFormat)
}

// TestProcessor_Process_WebPWithMalformedHeaderIsClientError pins WebP files
// that fail in image.DecodeConfig itself. x/image/webp reports these fixtures as
// plain errors, with no type or io sentinel to recognise, and they describe the
// client's bytes, so they must not reach the handler as a 500.
func TestProcessor_Process_WebPWithMalformedHeaderIsClientError(t *testing.T) {
	proc := newTestProcessor(t)
	preset, err := GetPreset("avatar")
	require.NoError(t, err)

	tests := []struct {
		name string
		data []byte
	}{
		// Canvas width-1 and height-1 are both 0xFFFFFF: the area exceeds what
		// the WebP decoder accepts.
		{name: "VP8X canvas of 2^24 x 2^24", data: webpExtended(0, 1<<24, 1<<24)},
		// A lossless chunk whose first byte is not the 0x2F VP8L signature.
		// The RIFF size is 18: "WEBP", the chunk header, 5 payload bytes, 1 pad.
		{
			name: "VP8L chunk with a bad signature",
			data: append([]byte("RIFF\x12\x00\x00\x00WEBP"), webpChunk("VP8L", []byte{0, 0, 0, 0, 0})...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := image.DecodeConfig(bytes.NewReader(tt.data))
			require.Error(t, err, "the fixture must fail header parsing so the failure comes from DecodeConfig")
			require.NotErrorIs(t, err, image.ErrFormat, "the fixture must be recognised as WebP and reach its decoder")

			result, err := proc.Process(tt.data, preset)

			require.Error(t, err)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrUnsupportedFormat)
			assert.NotErrorIs(t, err, ErrProcessingFailed,
				"a malformed WebP header is the client's bytes, not a failure of ours")
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
