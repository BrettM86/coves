package testkit

import (
	"bytes"
	"compress/lzw"
	"encoding/binary"
	"image"
)

// GIFTrailer is the byte that ends a GIF data stream.
const GIFTrailer byte = 0x3b

// gifColourTable packs palette entries for a global or local colour table and
// returns the table-size bits for the packed field. The palette length must
// be a power of two between 2 and 256.
func gifColourTable(palette [][3]byte) (table []byte, sizeBits byte) {
	for sizeBits = 0; sizeBits < 8; sizeBits++ {
		if len(palette) == 2<<sizeBits {
			for _, entry := range palette {
				table = append(table, entry[:]...)
			}
			return table, sizeBits
		}
	}
	panic("GIF colour table length must be a power of two between 2 and 256")
}

// GIFLogicalScreen is a GIF header (version "GIF87a" or "GIF89a"), a logical
// screen descriptor and, when palette is non-empty, a global colour table.
func GIFLogicalScreen(version string, width, height uint16, palette [][3]byte) []byte {
	if version != "GIF87a" && version != "GIF89a" {
		panic("GIF version must be GIF87a or GIF89a")
	}
	out := []byte(version)
	out = binary.LittleEndian.AppendUint16(out, width)
	out = binary.LittleEndian.AppendUint16(out, height)
	if len(palette) == 0 {
		return append(out, 0x70, 0, 0) // colour resolution 8, no global table
	}
	table, sizeBits := gifColourTable(palette)
	out = append(out, 0x80|0x70|sizeBits, 0, 0) // background index 0, no aspect ratio
	return append(out, table...)
}

// GIFSubBlocks frames data as length-prefixed sub-blocks of at most 255 bytes
// followed by the zero-length block terminator.
func GIFSubBlocks(data []byte) []byte {
	var out []byte
	for len(data) > 0 {
		n := min(len(data), 255)
		out = append(out, byte(n))
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return append(out, 0)
}

// GIFLZW compresses palette indices with the GIF variant of LZW (LSB first,
// a clear code first, end-of-information last) at the given minimum code size.
func GIFLZW(minCodeSize byte, indices []byte) []byte {
	var out bytes.Buffer
	writer := lzw.NewWriter(&out, lzw.LSB, int(minCodeSize))
	if _, err := writer.Write(indices); err != nil {
		panic("GIF LZW: " + err.Error())
	}
	if err := writer.Close(); err != nil {
		panic("GIF LZW: " + err.Error())
	}
	return out.Bytes()
}

// GIFImage is an image descriptor for bounds, an optional local colour table,
// the LZW minimum code size and lzwData framed as sub-blocks.
func GIFImage(bounds image.Rectangle, localPalette [][3]byte, minCodeSize byte, lzwData []byte) []byte {
	out := []byte{0x2c}
	for _, value := range []int{bounds.Min.X, bounds.Min.Y, bounds.Dx(), bounds.Dy()} {
		out = binary.LittleEndian.AppendUint16(out, uint16(value))
	}
	if len(localPalette) == 0 {
		out = append(out, 0)
	} else {
		table, sizeBits := gifColourTable(localPalette)
		out = append(out, 0x80|sizeBits)
		out = append(out, table...)
	}
	out = append(out, minCodeSize)
	return append(out, GIFSubBlocks(lzwData)...)
}

// GIFGraphicControl is a graphic control extension. A negative
// transparentIndex leaves the transparency flag clear.
func GIFGraphicControl(disposal byte, delay uint16, transparentIndex int) []byte {
	packed := disposal << 2
	index := byte(0)
	if transparentIndex >= 0 {
		packed |= 1
		index = byte(transparentIndex)
	}
	out := []byte{0x21, 0xf9, 4, packed}
	out = binary.LittleEndian.AppendUint16(out, delay)
	return append(out, index, 0)
}

// GIFComment is a comment extension holding text.
func GIFComment(text string) []byte {
	return append([]byte{0x21, 0xfe}, GIFSubBlocks([]byte(text))...)
}

// GIFApplication is an application extension with an 11-byte identifier and
// authentication code followed by each of subBlocks as its own sub-block.
func GIFApplication(identifier string, subBlocks ...[]byte) []byte {
	if len(identifier) != 11 {
		panic("GIF application identifier and authentication code must be 11 bytes")
	}
	out := append([]byte{0x21, 0xff, 11}, identifier...)
	for _, block := range subBlocks {
		if len(block) == 0 || len(block) > 255 {
			panic("GIF application sub-block must hold 1 to 255 bytes")
		}
		out = append(out, byte(len(block)))
		out = append(out, block...)
	}
	return append(out, 0)
}

// GIFFrame is one frame of AnimatedGIFWithMetadata as written to the file.
type GIFFrame struct {
	Bounds   image.Rectangle
	Indices  []byte // row-major palette indices, Bounds.Dx()×Bounds.Dy()
	Delay    int    // hundredths of a second
	Disposal byte
}

// AnimatedGIFFrames are the frames of AnimatedGIFWithMetadata, in order.
// Frame 0 is a 2×2 sub-rectangle with transparent index 3, frame 1 has its
// own 8-colour local colour table, frame 2 uses the global table.
var AnimatedGIFFrames = []GIFFrame{
	{Bounds: image.Rect(1, 1, 3, 3), Indices: []byte{1, 2, 3, 0}, Delay: 10, Disposal: 2},
	{Bounds: image.Rect(0, 0, 4, 3), Indices: []byte{7, 6, 5, 4, 3, 2, 1, 0, 7, 5, 3, 1}, Delay: 20, Disposal: 1},
	{Bounds: image.Rect(0, 0, 4, 3), Indices: []byte{0, 1, 2, 3, 3, 2, 1, 0, 1, 1, 2, 2}, Delay: 30, Disposal: 1},
}

// AnimatedGIFSecrets are hidden in the dirty AnimatedGIFWithMetadata input:
// an extra NETSCAPE2.0 sub-block, a comment, an XMP application extension and
// bytes after the trailer. None may survive sanitisation.
var AnimatedGIFSecrets = []string{
	"SECRET-NETSCAPE-SUBBLOCK-7f3a2c",
	"SECRET-GIF-COMMENT-91c2d4",
	"SECRET-XMP-PACKET-4e8d0b",
	"SECRET-AFTER-TRAILER-b6e01f",
}

var (
	animatedGIFGlobalPalette = [][3]byte{{0, 0, 0}, {255, 0, 0}, {0, 255, 0}, {0, 0, 255}}
	animatedGIFLocalPalette  = [][3]byte{
		{10, 10, 10},
		{20, 40, 60},
		{30, 60, 90},
		{40, 80, 120},
		{50, 100, 150},
		{60, 120, 180},
		{70, 140, 210},
		{80, 160, 240},
	}
)

// AnimatedGIFWithMetadata is a 4×3 GIF89a of AnimatedGIFFrames looping five
// times. dirty also carries every AnimatedGIFSecrets entry; clean is the same
// blocks, built here independently, with the comment and XMP extensions
// removed, NETSCAPE2.0 reduced to its loop sub-block and nothing after the
// trailer.
func AnimatedGIFWithMetadata() (dirty, clean []byte) {
	screen := GIFLogicalScreen("GIF89a", 4, 3, animatedGIFGlobalPalette)
	dirtyLoop := GIFApplication("NETSCAPE2.0", []byte{1, 5, 0}, []byte(AnimatedGIFSecrets[0]))
	cleanLoop := []byte("\x21\xff\x0bNETSCAPE2.0\x03\x01\x05\x00\x00")
	comment := GIFComment(AnimatedGIFSecrets[1])
	xmp := GIFApplication("XMP DataXMP", []byte(AnimatedGIFSecrets[2]))

	first, second, third := AnimatedGIFFrames[0], AnimatedGIFFrames[1], AnimatedGIFFrames[2]
	frame1 := append(GIFGraphicControl(first.Disposal, uint16(first.Delay), 3),
		GIFImage(first.Bounds, nil, 2, GIFLZW(2, first.Indices))...)
	frame2 := append(GIFGraphicControl(second.Disposal, uint16(second.Delay), -1),
		GIFImage(second.Bounds, animatedGIFLocalPalette, 3, GIFLZW(3, second.Indices))...)
	frame3 := append(GIFGraphicControl(third.Disposal, uint16(third.Delay), -1),
		GIFImage(third.Bounds, nil, 2, GIFLZW(2, third.Indices))...)

	for _, block := range [][]byte{screen, dirtyLoop, frame1, comment, frame2, frame3, xmp, {GIFTrailer}, []byte(AnimatedGIFSecrets[3])} {
		dirty = append(dirty, block...)
	}
	for _, block := range [][]byte{screen, cleanLoop, frame1, frame2, frame3, {GIFTrailer}} {
		clean = append(clean, block...)
	}
	return dirty, clean
}

// StaticGIF87a is a single-frame 2×3 GIF87a with a two-colour global table and
// no extensions: nothing in it needs removing.
func StaticGIF87a() []byte {
	out := GIFLogicalScreen("GIF87a", 2, 3, [][3]byte{{0, 0, 0}, {255, 255, 255}})
	out = append(out, GIFImage(image.Rect(0, 0, 2, 3), nil, 2, GIFLZW(2, []byte{0, 1, 1, 0, 0, 1}))...)
	return append(out, GIFTrailer)
}
