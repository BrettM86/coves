package imageproxy

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	decodeRed    = color.RGBA{R: 255, A: 255}
	decodeGreen  = color.RGBA{G: 255, A: 255}
	decodeBlue   = color.RGBA{B: 255, A: 255}
	decodeYellow = color.RGBA{R: 255, G: 255, A: 255}
)

func encodeDecodeTestJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}))
	return buf.Bytes()
}

func decodeTestJPEG(t *testing.T, width, height int, pixel func(x, y int) color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, pixel(x, y))
		}
	}
	return encodeDecodeTestJPEG(t, img)
}

func assertDecodeTestColor(t *testing.T, img image.Image, x, y int, want color.RGBA) {
	t.Helper()
	r, g, b, _ := img.At(x, y).RGBA()
	got := [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
	expected := [3]int{int(want.R), int(want.G), int(want.B)}
	for channel := range got {
		assert.InDeltaf(t, expected[channel], got[channel], 60, "pixel (%d,%d), RGB channel %d", x, y, channel)
	}
}

func TestProcessor_Process_CoverCropsFromCentre(t *testing.T) {
	// Red, green and blue thirds along the long axis: a centred square crop
	// sees only the green third.
	bands := func(position int) color.Color {
		switch {
		case position < 100:
			return decodeRed
		case position < 200:
			return decodeGreen
		default:
			return decodeBlue
		}
	}
	rows := []struct {
		name          string
		width, height int
		pixel         func(x, y int) color.Color
	}{
		{name: "wide vertical bands", width: 300, height: 100, pixel: func(x, _ int) color.Color { return bands(x) }},
		{name: "tall horizontal bands", width: 100, height: 300, pixel: func(_, y int) color.Color { return bands(y) }},
	}
	preset, err := GetPreset("avatar_small")
	require.NoError(t, err)
	proc := newTestProcessor(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			result, err := proc.Process(decodeTestJPEG(t, row.width, row.height, row.pixel), preset)
			require.NoError(t, err)
			img := assertLossyWebP(t, result, 192, 192)
			for _, point := range []image.Point{{10, 10}, {182, 10}, {96, 96}, {10, 182}, {182, 182}} {
				assertDecodeTestColor(t, img, point.X, point.Y, decodeGreen)
			}
		})
	}
}

// The APP1 length includes its two length bytes; this is inserted immediately
// after the SOI of a JPEG encoded by image/jpeg, ahead of its JFIF APP0.
func decodeTestAPP1(payload []byte) []byte {
	segment := []byte{0xff, 0xe1}
	segment = binary.BigEndian.AppendUint16(segment, uint16(len(payload)+2))
	return append(segment, payload...)
}

func decodeTestEXIF(tiff []byte) []byte {
	return decodeTestAPP1(append([]byte("Exif\x00\x00"), tiff...))
}

func decodeTestWithAPP1(jpegBytes []byte, segments ...[]byte) []byte {
	out := append([]byte(nil), jpegBytes[:2]...)
	for _, segment := range segments {
		out = append(out, segment...)
	}
	return append(out, jpegBytes[2:]...)
}

// A TIFF header followed by IFD0, one SHORT Orientation entry whose value is
// inline, and a zero next-IFD offset. Padding allows a non-default IFD0 offset.
func decodeTestOrientationTIFF(littleEndian bool, ifdOffset uint32, orientation uint16) []byte {
	var order binary.ByteOrder = binary.BigEndian
	tiff := make([]byte, int(ifdOffset)+18)
	if littleEndian {
		order = binary.LittleEndian
		copy(tiff, "II")
	} else {
		copy(tiff, "MM")
	}
	order.PutUint16(tiff[2:4], 42)
	order.PutUint32(tiff[4:8], ifdOffset)
	ifd := tiff[ifdOffset:]
	order.PutUint16(ifd[0:2], 1)
	order.PutUint16(ifd[2:4], 0x0112)
	order.PutUint16(ifd[4:6], 3)
	order.PutUint32(ifd[6:10], 1)
	order.PutUint16(ifd[10:12], orientation)
	return tiff
}

// decodeTestCameraOrientationTIFF lays IFD0 out as camera firmware does, in
// ascending tag order: ASCII Make (0x010F) and Model (0x0110) precede the
// Orientation entry (0x0112) and a RATIONAL XResolution (0x011A) follows it.
// Make, Model and XResolution do not fit in four bytes, so their entries hold
// offsets to values stored after the IFD.
func decodeTestCameraOrientationTIFF(littleEndian bool, orientation uint16) []byte {
	const (
		ifdOffset    = 8
		entryCount   = 4
		typeASCII    = 2
		typeShort    = 3
		typeRational = 5
	)
	var order binary.AppendByteOrder = binary.BigEndian
	header := []byte("MM")
	if littleEndian {
		order = binary.LittleEndian
		header = []byte("II")
	}
	makeValue := []byte("Coves Test Camera\x00")
	modelValue := []byte("Model 1\x00")
	makeOffset := uint32(ifdOffset + 2 + entryCount*12 + 4)
	modelOffset := makeOffset + uint32(len(makeValue))
	resolutionOffset := modelOffset + uint32(len(modelValue))

	tiff := order.AppendUint16(header, 42)
	tiff = order.AppendUint32(tiff, ifdOffset)
	tiff = order.AppendUint16(tiff, entryCount)
	appendEntry := func(tag, fieldType uint16, count uint32, value []byte) {
		tiff = order.AppendUint16(tiff, tag)
		tiff = order.AppendUint16(tiff, fieldType)
		tiff = order.AppendUint32(tiff, count)
		tiff = append(tiff, value...)
	}
	appendEntry(0x010F, typeASCII, uint32(len(makeValue)), order.AppendUint32(nil, makeOffset))
	appendEntry(0x0110, typeASCII, uint32(len(modelValue)), order.AppendUint32(nil, modelOffset))
	appendEntry(0x0112, typeShort, 1, append(order.AppendUint16(nil, orientation), 0, 0))
	appendEntry(0x011A, typeRational, 1, order.AppendUint32(nil, resolutionOffset))
	tiff = order.AppendUint32(tiff, 0) // no IFD1
	tiff = append(tiff, makeValue...)
	tiff = append(tiff, modelValue...)
	tiff = order.AppendUint32(tiff, 72)
	return order.AppendUint32(tiff, 1)
}

func decodeTestQuadrantJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	return decodeTestJPEG(t, width, height, func(x, y int) color.Color {
		switch {
		case x < width/2 && y < height/2:
			return decodeRed
		case y < height/2:
			return decodeGreen
		case x < width/2:
			return decodeBlue
		default:
			return decodeYellow
		}
	})
}

func assertDecodeTestQuadrants(t *testing.T, img image.Image, want [4]color.RGBA) {
	t.Helper()
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	points := [4]image.Point{{w / 4, h / 4}, {3 * w / 4, h / 4}, {w / 4, 3 * h / 4}, {3 * w / 4, 3 * h / 4}}
	for i, point := range points {
		assertDecodeTestColor(t, img, point.X, point.Y, want[i])
	}
}

func TestProcessor_Process_EXIFOrientation(t *testing.T) {
	source := decodeTestQuadrantJPEG(t, 200, 100)
	plain := [4]color.RGBA{decodeRed, decodeGreen, decodeBlue, decodeYellow}
	badOffset := decodeTestOrientationTIFF(true, 8, 6)
	binary.LittleEndian.PutUint32(badOffset[4:8], 1000)
	truncatedEntry := decodeTestOrientationTIFF(false, 8, 6)[:18]
	wrongType := decodeTestOrientationTIFF(true, 8, 6)
	binary.LittleEndian.PutUint16(wrongType[12:14], 4)
	xmp := decodeTestAPP1([]byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta/>"))

	transpose := [4]color.RGBA{decodeRed, decodeBlue, decodeGreen, decodeYellow}
	clockwise := [4]color.RGBA{decodeBlue, decodeRed, decodeYellow, decodeGreen}
	transverse := [4]color.RGBA{decodeYellow, decodeGreen, decodeBlue, decodeRed}
	counterclockwise := [4]color.RGBA{decodeGreen, decodeYellow, decodeRed, decodeBlue}
	large := image.Pt(2000, 1000)

	rows := []struct {
		name       string
		tiff       []byte
		xmpFirst   bool
		presetName string
		width      int
		height     int
		quadrants  [4]color.RGBA // TL, TR, BL, BR, literal EXIF orientation result
		sourceSize image.Point   // zero uses the shared 200x100 source
	}{
		{"1 control II", decodeTestOrientationTIFF(true, 8, 1), false, "content_full", 200, 100, plain, image.Point{}},
		{"2 horizontal flip MM", decodeTestOrientationTIFF(false, 8, 2), false, "content_full", 200, 100, [4]color.RGBA{decodeGreen, decodeRed, decodeYellow, decodeBlue}, image.Point{}},
		{"3 rotate 180 II", decodeTestOrientationTIFF(true, 8, 3), false, "content_full", 200, 100, [4]color.RGBA{decodeYellow, decodeBlue, decodeGreen, decodeRed}, image.Point{}},
		{"4 vertical flip MM", decodeTestOrientationTIFF(false, 8, 4), false, "content_full", 200, 100, [4]color.RGBA{decodeBlue, decodeYellow, decodeRed, decodeGreen}, image.Point{}},
		{"5 transpose II", decodeTestOrientationTIFF(true, 8, 5), false, "content_full", 100, 200, transpose, image.Point{}},
		{"6 rotate clockwise camera IFD0 MM", decodeTestCameraOrientationTIFF(false, 6), false, "content_full", 100, 200, clockwise, image.Point{}},
		{"7 transverse II", decodeTestOrientationTIFF(true, 8, 7), false, "content_full", 100, 200, transverse, image.Point{}},
		{"8 rotate counterclockwise camera IFD0 MM", decodeTestCameraOrientationTIFF(false, 8), false, "content_full", 100, 200, counterclockwise, image.Point{}},
		// Both fits scale a 2000×1000 source, so these rows check that each
		// transposing orientation swaps the target before scaling.
		{"5 contain downscale camera IFD0 II", decodeTestCameraOrientationTIFF(true, 5), false, "content_preview", 800, 1600, transpose, large},
		{"6 contain downscale camera IFD0 MM", decodeTestCameraOrientationTIFF(false, 6), false, "content_preview", 800, 1600, clockwise, large},
		{"7 contain downscale camera IFD0 II", decodeTestCameraOrientationTIFF(true, 7), false, "content_preview", 800, 1600, transverse, large},
		{"8 contain downscale camera IFD0 MM", decodeTestCameraOrientationTIFF(false, 8), false, "content_preview", 800, 1600, counterclockwise, large},
		{"5 cover banner camera IFD0 MM", decodeTestCameraOrientationTIFF(false, 5), false, "banner", 640, 300, transpose, large},
		{"6 cover banner camera IFD0 II", decodeTestCameraOrientationTIFF(true, 6), false, "banner", 640, 300, clockwise, large},
		{"7 cover banner camera IFD0 MM", decodeTestCameraOrientationTIFF(false, 7), false, "banner", 640, 300, transverse, large},
		{"8 cover banner camera IFD0 II", decodeTestCameraOrientationTIFF(true, 8), false, "banner", 640, 300, counterclockwise, large},
		{"XMP APP1 before Exif APP1", decodeTestOrientationTIFF(true, 8, 6), true, "content_full", 100, 200, clockwise, image.Point{}},
		{"non-default IFD0 offset", decodeTestOrientationTIFF(false, 16, 6), false, "content_full", 100, 200, clockwise, image.Point{}},
		{"IFD offset out of range", badOffset, false, "content_full", 200, 100, plain, image.Point{}},
		{"truncated IFD entry", truncatedEntry, false, "content_full", 200, 100, plain, image.Point{}},
		{"wrong tag type", wrongType, false, "content_full", 200, 100, plain, image.Point{}},
		{"orientation zero", decodeTestOrientationTIFF(true, 8, 0), false, "content_full", 200, 100, plain, image.Point{}},
		{"orientation nine", decodeTestOrientationTIFF(false, 8, 9), false, "content_full", 200, 100, plain, image.Point{}},
	}
	proc := newTestProcessor(t)
	sources := map[image.Point][]byte{{}: source}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rowSource, ok := sources[row.sourceSize]
			if !ok {
				rowSource = decodeTestQuadrantJPEG(t, row.sourceSize.X, row.sourceSize.Y)
				sources[row.sourceSize] = rowSource
			}
			segments := [][]byte{decodeTestEXIF(row.tiff)}
			if row.xmpFirst {
				segments = append([][]byte{xmp}, segments...)
			}
			preset, err := GetPreset(row.presetName)
			require.NoError(t, err)
			output, err := proc.Process(decodeTestWithAPP1(rowSource, segments...), preset)
			require.NoError(t, err)
			img := assertLossyWebP(t, output, row.width, row.height)
			assertDecodeTestQuadrants(t, img, row.quadrants)
		})
	}
}

func TestProcessor_Process_ExtremeAspectCoverAllocation(t *testing.T) {
	source := createTestJPEG(t, 2000, 20)
	preset, err := GetPreset("avatar_small")
	require.NoError(t, err)
	output, allocated, err := measureProcessAlloc(newTestProcessor(t), source, preset)
	require.NoError(t, err)
	assertLossyWebP(t, output, 192, 192)
	assert.Less(t, allocated, uint64(16<<20))
}

// draw.CatmullRom.Scale allocates a float64 buffer of destination width ×
// source height × 32 B. Before pre-reduction and the kernel buffer cap, the
// tall row allocated 1.20 GB (the buffer alone is 1600×20000×32 B = 1.02 GB)
// and the photo row 248 MB (its buffer is 1600×4000×32 B = 205 MB). With them
// the rows measure 173 MB and 170 MB. The tall bound (537 MB) is 2.2× under the
// old figure and 3.1× over the new; the photo bound (210 MB) is 18% under the
// old figure and 23% over the new. The tall row's output is taller than 16383,
// WebP's side limit, so it is encoded as JPEG. The large-webp row's output
// (1600x16376, 26 MP) fits the side limit and is encoded as lossy WebP; its
// 1800 MiB bound is the accepted cost of large WebP encodes (a 1393x16383
// encode measured about 1 GB of RSS and 916 MB allocated for the encode alone,
// on top of decode and scale).
func TestProcessor_Process_ContainScaleAllocation(t *testing.T) {
	rows := []struct {
		name                      string
		sourceWidth, sourceHeight int
		wantWidth, wantHeight     int
		wantFormat                string
		maxAllocated              uint64
	}{
		{name: "tall 1700x20000", sourceWidth: 1700, sourceHeight: 20000, wantWidth: 1600, wantHeight: 18824, wantFormat: "jpeg", maxAllocated: 512 << 20},
		{name: "photo 6000x4000", sourceWidth: 6000, sourceHeight: 4000, wantWidth: 1600, wantHeight: 1067, wantFormat: "webp", maxAllocated: 200 << 20},
		{name: "large webp 1700x17400", sourceWidth: 1700, sourceHeight: 17400, wantWidth: 1600, wantHeight: 16376, wantFormat: "webp", maxAllocated: 1800 << 20},
	}
	preset, err := GetPreset("content_full")
	require.NoError(t, err)
	proc := newTestProcessor(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			source := createTestJPEG(t, row.sourceWidth, row.sourceHeight)
			started := time.Now()
			output, allocated, err := measureProcessAlloc(proc, source, preset)
			t.Logf("allocated %d bytes in %s", allocated, time.Since(started))
			require.NoError(t, err)
			assertEncodedImage(t, output, row.wantFormat, row.wantWidth, row.wantHeight)
			assert.Less(t, allocated, row.maxAllocated)
		})
	}
}

// assertEncodedImage checks that data is an image in format ("webp" for lossy
// WebP, "jpeg" or "png") of exactly width x height and returns it decoded.
func assertEncodedImage(t *testing.T, data []byte, format string, width, height int) image.Image {
	t.Helper()
	switch format {
	case "webp":
		return assertLossyWebP(t, data, width, height)
	case "jpeg":
		require.GreaterOrEqual(t, len(data), 2, "output too short to be JPEG")
		require.Equal(t, []byte{0xFF, 0xD8}, data[:2], "expected JPEG SOI marker, got % x", data[:min(len(data), 12)])
		img, err := jpeg.Decode(bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, width, img.Bounds().Dx(), "decoded width")
		require.Equal(t, height, img.Bounds().Dy(), "decoded height")
		return img
	case "png":
		require.GreaterOrEqual(t, len(data), 8, "output too short to be PNG")
		require.Equal(t, []byte("\x89PNG\r\n\x1a\n"), data[:8], "expected PNG signature, got % x", data[:min(len(data), 12)])
		img, err := png.Decode(bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, width, img.Bounds().Dx(), "decoded width")
		require.Equal(t, height, img.Bounds().Dy(), "decoded height")
		return img
	}
	t.Fatalf("unknown expected format %q", format)
	return nil
}

// halfTransparentTestPNG is an opaque red PNG whose left half is fully
// transparent.
func halfTransparentTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if x < width/2 {
				img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 0})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// WebP is used for any output with both sides at most 16383, WebP's format
// maximum, whatever its pixel count; output with a side over 16383 keeps the
// pre-WebP encoder: PNG for a PNG source, JPEG otherwise.
func TestProcessor_Process_WebPSideLimit(t *testing.T) {
	rows := []struct {
		name                  string
		source                func(t *testing.T) []byte
		wantFormat            string
		wantWidth, wantHeight int
		wantTransparentAt     *image.Point
		wantOpaqueAt          *image.Point
	}{
		{name: "jpeg 1600x2501 is webp", source: func(t *testing.T) []byte { return createTestJPEG(t, 1600, 2501) }, wantFormat: "webp", wantWidth: 1600, wantHeight: 2501},
		{name: "opaque png 1600x2501 is webp", source: func(t *testing.T) []byte { return createTestPNG(t, 1600, 2501) }, wantFormat: "webp", wantWidth: 1600, wantHeight: 2501},
		{name: "transparent png 1600x2600 is webp and keeps alpha", source: func(t *testing.T) []byte { return halfTransparentTestPNG(t, 1600, 2600) }, wantFormat: "webp", wantWidth: 1600, wantHeight: 2600, wantTransparentAt: &image.Point{X: 200, Y: 1300}, wantOpaqueAt: &image.Point{X: 1400, Y: 1300}},
		{name: "transparent png taller than 16383 is png and keeps alpha", source: func(t *testing.T) []byte { return halfTransparentTestPNG(t, 200, 17000) }, wantFormat: "png", wantWidth: 200, wantHeight: 17000, wantTransparentAt: &image.Point{X: 50, Y: 8500}, wantOpaqueAt: &image.Point{X: 150, Y: 8500}},
		{name: "jpeg taller than 16383 is jpeg", source: func(t *testing.T) []byte { return createTestJPEG(t, 200, 17000) }, wantFormat: "jpeg", wantWidth: 200, wantHeight: 17000},
		{name: "jpeg 16383 tall is webp", source: func(t *testing.T) []byte { return createTestJPEG(t, 200, 16383) }, wantFormat: "webp", wantWidth: 200, wantHeight: 16383},
		{name: "jpeg 16384 tall is jpeg", source: func(t *testing.T) []byte { return createTestJPEG(t, 200, 16384) }, wantFormat: "jpeg", wantWidth: 200, wantHeight: 16384},
	}
	preset, err := GetPreset("content_full")
	require.NoError(t, err)
	proc := newTestProcessor(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			output, err := proc.Process(row.source(t), preset)
			require.NoError(t, err)
			img := assertEncodedImage(t, output, row.wantFormat, row.wantWidth, row.wantHeight)
			if row.wantTransparentAt != nil {
				pixel := color.NRGBAModel.Convert(img.At(row.wantTransparentAt.X, row.wantTransparentAt.Y)).(color.NRGBA)
				assert.Equal(t, uint8(0), pixel.A, "transparent region alpha")
				opaque := color.NRGBAModel.Convert(img.At(row.wantOpaqueAt.X, row.wantOpaqueAt.Y)).(color.NRGBA)
				assert.Equal(t, uint8(255), opaque.A, "opaque region alpha")
			}
		})
	}
}

func TestProcessor_Process_ContainEdgeDimensions(t *testing.T) {
	rows := []struct {
		name, presetName string
		width, height    int
		preset           Preset
		wantWidth        int
		wantHeight       int
	}{
		{name: "minimum one pixel height", presetName: "content_full", width: 4000, height: 1, wantWidth: 1600, wantHeight: 1},
		{name: "positive height cap", width: 1600, height: 1600, preset: Preset{Name: "height_cap", Width: 800, Height: 200, Fit: FitContain, Quality: 85}, wantWidth: 200, wantHeight: 200},
	}
	proc := newTestProcessor(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			preset := row.preset
			if row.presetName != "" {
				var err error
				preset, err = GetPreset(row.presetName)
				require.NoError(t, err)
			}
			output, err := proc.Process(createTestJPEG(t, row.width, row.height), preset)
			require.NoError(t, err)
			assertLossyWebP(t, output, row.wantWidth, row.wantHeight)
		})
	}
}

func TestProcessor_Process_PNGPreservesAlphaAndColor(t *testing.T) {
	// Three vertical regions remain visible both after width-led containment and
	// after the banner's centre crop of this wide source.
	img := image.NewNRGBA(image.Rect(0, 0, 900, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 900; x++ {
			switch {
			case x < 300:
				img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 0})
			case x < 600:
				img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
			default:
				img.SetNRGBA(x, y, color.NRGBA{B: 255, A: 128})
			}
		}
	}
	var source bytes.Buffer
	require.NoError(t, png.Encode(&source, img))
	rows := []struct {
		name                               string
		width, height                      int
		transparentX, redX, blueX, sampleY int
	}{
		{name: "content_preview contain", width: 800, height: 267, transparentX: 100, redX: 400, blueX: 700, sampleY: 130},
		{name: "banner cover", width: 640, height: 300, transparentX: 60, redX: 320, blueX: 580, sampleY: 150},
	}
	proc := newTestProcessor(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			presetName := "content_preview"
			if row.name == "banner cover" {
				presetName = "banner"
			}
			preset, err := GetPreset(presetName)
			require.NoError(t, err)
			output, err := proc.Process(source.Bytes(), preset)
			require.NoError(t, err)
			result := assertLossyWebP(t, output, row.width, row.height)
			transparent := color.NRGBAModel.Convert(result.At(row.transparentX, row.sampleY)).(color.NRGBA)
			red := color.NRGBAModel.Convert(result.At(row.redX, row.sampleY)).(color.NRGBA)
			blue := color.NRGBAModel.Convert(result.At(row.blueX, row.sampleY)).(color.NRGBA)
			assert.Equal(t, uint8(0), transparent.A)
			assert.InDelta(t, 255, red.R, 16)
			assert.Equal(t, uint8(255), red.A)
			assert.InDelta(t, 255, blue.B, 16)
			assert.InDelta(t, 0, blue.R, 16)
			assert.InDelta(t, 0, blue.G, 16)
			assert.InDelta(t, 128, blue.A, 16)
		})
	}
}
