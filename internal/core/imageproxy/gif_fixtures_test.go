package imageproxy

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
	"image/gif"
	"testing"

	cidlib "github.com/ipfs/go-cid"
	multihash "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

func rawCIDForTest(t *testing.T, data []byte) string {
	t.Helper()
	hash, err := multihash.Sum(data, multihash.SHA2_256, -1)
	require.NoError(t, err)
	return cidlib.NewCidV1(cidlib.Raw, hash).String()
}

func literalGreenPNG(t *testing.T) []byte {
	t.Helper()
	data, err := hex.DecodeString("89504e470d0a1a0a0000000d494844520000000400000004080200000026930929000000144944415478da62616860800326060662388001001aa0008b810113890000000049454e44ae426082")
	require.NoError(t, err)
	require.Len(t, data, 77)
	return data
}

// animatedGIF encodes full-screen solid frames. Each frame owns its palette;
// no global colour model is supplied to gif.EncodeAll.
func animatedGIF(t *testing.T, width, height, frames int) []byte {
	t.Helper()
	animation := &gif.GIF{Config: image.Config{Width: width, Height: height}}
	for frame := range frames {
		shade := color.RGBA{R: 255, A: 255}
		if frame%2 == 1 {
			shade = color.RGBA{B: 255, A: 255}
		}
		palette := color.Palette{color.RGBA{A: 255}, shade}
		picture := image.NewPaletted(image.Rect(0, 0, width, height), palette)
		for index := range picture.Pix {
			picture.Pix[index] = 1
		}
		animation.Image = append(animation.Image, picture)
		animation.Delay = append(animation.Delay, 1)
	}
	var output bytes.Buffer
	require.NoError(t, gif.EncodeAll(&output, animation))
	require.Zero(t, output.Bytes()[10]&0x80, "fixture must use local, not global, colour tables")
	return output.Bytes()
}

func gifWithComment(t *testing.T, width, height int) []byte {
	t.Helper()
	encoded := animatedGIF(t, width, height, 2)
	// GIF header (6) + logical screen descriptor (7), followed by the
	// optional global table announced by the packed descriptor byte.
	insertAt := 13
	if encoded[10]&0x80 != 0 {
		insertAt += 3 * (1 << (1 + int(encoded[10]&7)))
	}
	comment := []byte{0x21, 0xFE, 2, 'o', 'n', 2, 'o', 'k', 0}
	return append(append(append([]byte{}, encoded[:insertAt]...), comment...), encoded[insertAt:]...)
}

// framedJunkGIF has real GIF block framing but deliberately invalid LZW data.
// The caller can truncate it inside the first frame's declared data sub-block.
func framedJunkGIF(width, height, frames int) []byte {
	return framedJunkGIFWithFrame(width, height, image.Rect(0, 0, width, height), frames)
}

// framedJunkGIFWithFrame is framedJunkGIF with each image descriptor declaring
// frame (left, top, width, height) independently of the logical screen size.
func framedJunkGIFWithFrame(screenWidth, screenHeight int, frame image.Rectangle, frames int) []byte {
	file := []byte("GIF89a")
	file = binary.LittleEndian.AppendUint16(file, uint16(screenWidth))
	file = binary.LittleEndian.AppendUint16(file, uint16(screenHeight))
	file = append(file, 0x80, 0, 0) // two-entry global colour table
	file = append(file, 0, 0, 0, 255, 0, 0)
	for range frames {
		file = append(file, 0x2C)
		file = binary.LittleEndian.AppendUint16(file, uint16(frame.Min.X))
		file = binary.LittleEndian.AppendUint16(file, uint16(frame.Min.Y))
		file = binary.LittleEndian.AppendUint16(file, uint16(frame.Dx()))
		file = binary.LittleEndian.AppendUint16(file, uint16(frame.Dy()))
		file = append(file, 0, 2, 1, 0xFF, 0) // descriptor flags, LZW min size, one junk sub-block, terminator
	}
	return append(file, 0x3B)
}
