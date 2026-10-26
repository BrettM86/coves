package media_test

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"slices"
	"testing"
)

// pngFixChunk is one PNG chunk: its type and its data, without length or CRC.
type pngFixChunk struct {
	chunkType string
	data      []byte
}

// pngFixChunks splits a PNG into its chunks, in order.
func pngFixChunks(t *testing.T, data []byte) []pngFixChunk {
	t.Helper()
	var chunks []pngFixChunk
	for offset := 8; offset < len(data); {
		if offset+12 > len(data) {
			t.Fatal("truncated PNG chunk")
		}
		end := offset + 12 + int(binary.BigEndian.Uint32(data[offset:]))
		if end > len(data) {
			t.Fatal("PNG chunk exceeds input")
		}
		chunks = append(chunks, pngFixChunk{string(data[offset+4 : offset+8]), data[offset+8 : end-4]})
		offset = end
	}
	return chunks
}

// pngFixEncode encodes img and returns its IHDR chunk and the concatenated
// data of its IDAT chunks.
func pngFixEncode(t *testing.T, img image.Image) (ihdr, imageData []byte) {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	for _, chunk := range pngFixChunks(t, encoded.Bytes()) {
		switch chunk.chunkType {
		case "IHDR":
			ihdr = pngChunk("IHDR", chunk.data)
		case "IDAT":
			imageData = append(imageData, chunk.data...)
		}
	}
	return ihdr, imageData
}

// pngFixFrameControl is an APNG fcTL chunk for a full-canvas frame shown for
// a tenth of a second.
func pngFixFrameControl(sequence uint32, width, height int) []byte {
	payload := binary.BigEndian.AppendUint32(nil, sequence)
	payload = binary.BigEndian.AppendUint32(payload, uint32(width))
	payload = binary.BigEndian.AppendUint32(payload, uint32(height))
	payload = binary.BigEndian.AppendUint32(payload, 0) // x offset
	payload = binary.BigEndian.AppendUint32(payload, 0) // y offset
	payload = append(payload, 0, 1, 0, 10, 0, 0)        // delay 1/10, dispose none, blend source
	return pngChunk("fcTL", payload)
}

// pngFixSignature is the eight-byte PNG signature.
var pngFixSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// pngFixAnimatedPNG is a two-frame 300x200 APNG whose default image is the
// first frame, with extra chunks inserted after IHDR.
func pngFixAnimatedPNG(t *testing.T, extra ...[]byte) []byte {
	t.Helper()
	ihdr, firstFrame := pngFixEncode(t, quadrantImage(300, 200, false))
	solid := image.NewNRGBA(image.Rect(0, 0, 300, 200))
	for i := range solid.Pix {
		solid.Pix[i] = 0xff
	}
	_, secondFrame := pngFixEncode(t, solid)
	parts := [][]byte{pngFixSignature, ihdr}
	parts = append(parts, extra...)
	parts = append(parts,
		pngChunk("acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0}), // two frames, loop forever
		pngFixFrameControl(0, 300, 200),
		pngChunk("IDAT", firstFrame),
		pngFixFrameControl(1, 300, 200),
		pngChunk("fdAT", append(binary.BigEndian.AppendUint32(nil, 2), secondFrame...)),
		pngChunk("IEND", nil),
	)
	return bytes.Join(parts, nil)
}

func TestUploadKeepsAnimatedPNGsOverTheLongEdgeLimit(t *testing.T) {
	input := pngFixAnimatedPNG(t, pngChunk("tEXt", []byte("Author\x00person")))
	want := pngFixAnimatedPNG(t) // the input without its tEXt chunk
	expectHeaderDimensions(t, input, "image/png", 300, 200)

	uploaded, uploader := upload(t, downscalePipeline(t), input, "image/png")

	decoded := decodeStored(t, uploaded, uploader, "image/png")
	if imageBounds(decoded) != image.Rect(0, 0, 300, 200) {
		t.Errorf("stored bounds: got %v, want the original 300x200", imageBounds(decoded))
	}
	types := pngChunkTypes(t, uploader.data)
	for _, animationChunk := range []string{"acTL", "fcTL", "fdAT"} {
		if !slices.Contains(types, animationChunk) {
			t.Errorf("stored PNG lacks %s; chunks %v", animationChunk, types)
		}
	}
	if !bytes.Equal(uploader.data, want) {
		t.Errorf("stored PNG is not the input with only its metadata removed; chunks %v", types)
	}
}

func TestUploadDownscalesAPNGWhoseAnimationControlFollowsImageData(t *testing.T) {
	// An acTL after IDAT does not make an APNG, so the image is downscaled.
	ihdr, imageData := pngFixEncode(t, quadrantImage(300, 200, false))
	input := bytes.Join([][]byte{
		pngFixSignature, ihdr,
		pngChunk("IDAT", imageData),
		pngChunk("acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0}),
		pngChunk("IEND", nil),
	}, nil)
	expectHeaderDimensions(t, input, "image/png", 300, 200)

	uploaded, uploader := upload(t, downscalePipeline(t), input, "image/png")

	decoded := decodeStored(t, uploaded, uploader, "image/png")
	if imageBounds(decoded) != image.Rect(0, 0, 120, 80) {
		t.Errorf("stored bounds: got %v, want 120x80", imageBounds(decoded))
	}
	if types := pngChunkTypes(t, uploader.data); slices.Contains(types, "acTL") {
		t.Errorf("downscaled PNG retains acTL; chunks %v", types)
	}
}

func TestUploadDownscaledPNGKeepsColorChunks(t *testing.T) {
	gamma := pngChunk("gAMA", []byte{0, 0, 0xb1, 0x8f})
	chromaticities := pngChunk("cHRM", []byte{
		0, 0, 0x7a, 0x26, 0, 0, 0x80, 0x84, 0, 0, 0xfa, 0, 0, 0, 0x80, 0xe8,
		0, 0, 0x75, 0x30, 0, 0, 0xea, 0x60, 0, 0, 0x3a, 0x98, 0, 0, 0x17, 0x70,
	})
	standardRGB := pngChunk("sRGB", []byte{0})
	iccProfile := pngChunk("iCCP", []byte("profile\x00\x00x\x9c\x03\x00\x00\x00\x00\x01"))
	codingPoints := pngChunk("cICP", []byte{9, 16, 0, 1})
	masteringDisplay := pngChunk("mDCv", []byte{
		0x8a, 0x48, 0x39, 0x08, 0x2e, 0xe0, 0x9a, 0xc4, 0x19, 0x96, 0x08, 0xfc,
		0x3d, 0x13, 0x40, 0x42, 0x00, 0x98, 0x96, 0x80, 0x00, 0x00, 0x00, 0x01,
	})
	contentLightLevel := pngChunk("cLLi", []byte{0, 0x0f, 0x42, 0x40, 0, 0x06, 0x1a, 0x80})
	significantBits := pngChunk("sBIT", []byte{8, 8, 8})
	text := pngChunk("tEXt", []byte("Author\x00person"))
	rows := []struct {
		name  string
		extra [][]byte // inserted after IHDR, in this order
		want  [][]byte // the colour chunks the output must carry after IHDR
	}{
		{
			"every colour chunk among sBIT and tEXt",
			[][]byte{gamma, significantBits, chromaticities, iccProfile, text, standardRGB, codingPoints, masteringDisplay, contentLightLevel},
			[][]byte{gamma, chromaticities, iccProfile, standardRGB, codingPoints, masteringDisplay, contentLightLevel},
		},
		{"gAMA only", [][]byte{gamma}, [][]byte{gamma}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ihdr, imageData := pngFixEncode(t, quadrantImage(300, 200, false))
			parts := append([][]byte{pngFixSignature, ihdr}, row.extra...)
			parts = append(parts, pngChunk("IDAT", imageData), pngChunk("IEND", nil))
			input := bytes.Join(parts, nil)
			expectHeaderDimensions(t, input, "image/png", 300, 200)

			uploaded, uploader := upload(t, downscalePipeline(t), input, "image/png")

			decoded := decodeStored(t, uploaded, uploader, "image/png")
			if imageBounds(decoded) != image.Rect(0, 0, 120, 80) {
				t.Errorf("stored bounds: got %v, want 120x80", imageBounds(decoded))
			}
			if got := decoded.At(10, 10); !pngFixColorsEqual(got, color.RGBA{0xff, 0, 0, 0xff}) {
				t.Errorf("top-left pixel: got %v, want red", got)
			}
			const ihdrEnd = 8 + 12 + 13
			want := bytes.Join(row.want, nil)
			if len(uploader.data) < ihdrEnd+len(want) || !bytes.Equal(uploader.data[ihdrEnd:ihdrEnd+len(want)], want) {
				t.Errorf("stored PNG does not carry the input's colour chunks verbatim and in order after IHDR; chunks %v", pngChunkTypes(t, uploader.data))
			}
			types := pngChunkTypes(t, uploader.data)
			for _, dropped := range []string{"sBIT", "tEXt"} {
				if slices.Contains(types, dropped) {
					t.Errorf("stored PNG retains %s; chunks %v", dropped, types)
				}
			}
		})
	}
}

// pngFixColorsEqual compares two colours by their 16-bit premultiplied components.
func pngFixColorsEqual(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}
