package imageproxy

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// The builders in this file produce header-only image files: every one of them
// declares arbitrary dimensions in a few dozen bytes and carries no pixel data.
// They exist so a test can hand the processor an image that CLAIMS to be
// enormous without the test itself ever allocating that image. Encoding a real
// 12000×12000 frame just to assert it is refused would spend the ~549 MiB the
// refusal is supposed to save, and would OOM the CI runner besides.
//
// Each recipe is the minimum that makes image.DecodeConfig return the declared
// dimensions with a nil error; anything shorter fails at DecodeConfig and the
// test would be exercising the corruption path instead of the budget.

// pngChunk frames one PNG chunk: length, type, data, CRC32 over type+data.
func pngChunk(chunkType string, data []byte) []byte {
	out := make([]byte, 0, 12+len(data))
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, chunkType...)
	out = append(out, data...)
	crc := crc32.NewIEEE()
	crc.Write([]byte(chunkType))
	crc.Write(data)
	return binary.BigEndian.AppendUint32(out, crc.Sum32())
}

// pngHeader builds a PNG whose IHDR declares width×height 8-bit RGBA and whose
// IDAT holds only the two-byte zlib stream header. The IDAT stub is what makes
// an unguarded decode expensive: image/png allocates the full width*height*4
// frame before reading the stream, then fails on "not enough pixel data". An
// IHDR-only file hits EOF first and allocates nothing, which would let a
// missing budget check pass an allocation-bound assertion.
func pngHeader(t *testing.T, width, height uint32) []byte {
	t.Helper()

	ihdr := make([]byte, 0, 13)
	ihdr = binary.BigEndian.AppendUint32(ihdr, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr,
		8, // bit depth
		6, // colour type: truecolour with alpha
		0, // compression method
		0, // filter method
		0, // interlace method
	)

	file := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}
	file = append(file, pngChunk("IHDR", ihdr)...)
	file = append(file, pngChunk("IDAT", []byte{0x78, 0x9C})...)
	return file
}

// jpegHeader builds a baseline JPEG consisting of SOI, a JFIF APP0 segment and
// a SOF0 frame header declaring width×height. The APP0 is load-bearing: without
// it image/jpeg's DecodeConfig reads past SOF0 looking for more segments and
// fails with unexpected EOF instead of reporting the dimensions. JPEG stores
// height before width in SOF0.
func jpegHeader(t *testing.T, width, height uint16) []byte {
	t.Helper()

	file := []byte{0xFF, 0xD8}
	file = append(file,
		0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
	)
	file = append(file, 0xFF, 0xC0, 0x00, 0x11, 0x08)
	file = binary.BigEndian.AppendUint16(file, height)
	file = binary.BigEndian.AppendUint16(file, width)
	file = append(file, 0x03, 0x01, 0x22, 0x00, 0x02, 0x11, 0x01, 0x03, 0x11, 0x01)
	return file
}

// webpHeader builds a lossy WebP: a RIFF container around a single "VP8 " chunk
// holding a keyframe tag, the VP8 start code and width×height as 14-bit
// little-endian fields. Each side is therefore capped at 16383. There is no
// VP8X extended header and no frame data; x/image/webp's DecodeConfig stops at
// the dimensions, while its Decode allocates the whole frame before failing.
func webpHeader(t *testing.T, width, height uint16) []byte {
	t.Helper()
	if width > 0x3FFF || height > 0x3FFF {
		t.Fatalf("webpHeader: VP8 dimensions are 14-bit, %dx%d does not fit", width, height)
	}

	vp8 := []byte{0x10, 0x02, 0x00, 0x9D, 0x01, 0x2A}
	vp8 = binary.LittleEndian.AppendUint16(vp8, width)
	vp8 = binary.LittleEndian.AppendUint16(vp8, height)

	file := []byte("RIFF")
	file = binary.LittleEndian.AppendUint32(file, uint32(4+8+len(vp8)))
	file = append(file, "WEBP"...)
	file = append(file, "VP8 "...)
	file = binary.LittleEndian.AppendUint32(file, uint32(len(vp8)))
	file = append(file, vp8...)
	return file
}

// pngSignature is the 8-byte file header every PNG starts with.
func pngSignature() []byte {
	return []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}
}

// pngIHDRData is the 13-byte IHDR payload for an 8-bit RGBA image of the given
// size, with no filter, compression or interlace options set.
func pngIHDRData(width, height uint32) []byte {
	data := make([]byte, 0, 13)
	data = binary.BigEndian.AppendUint32(data, width)
	data = binary.BigEndian.AppendUint32(data, height)
	return append(data,
		8, // bit depth
		6, // colour type: truecolour with alpha
		0, // compression method
		0, // filter method
		0, // interlace method
	)
}

// pngRawChunk frames a chunk with a caller-supplied CRC instead of the correct
// one, so a test can hand the decoder a file whose IHDR fails its checksum.
func pngRawChunk(chunkType string, data []byte, crc uint32) []byte {
	out := make([]byte, 0, 12+len(data))
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, chunkType...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc)
}

// pngIHDROnly builds a PNG that stops right after a well-formed IHDR. The
// decoder reads the header, then hits EOF before any image data.
func pngIHDROnly(t *testing.T, width, height uint32) []byte {
	t.Helper()
	return append(pngSignature(), pngChunk("IHDR", pngIHDRData(width, height))...)
}

// pngTruncatedIHDR builds a PNG whose IHDR chunk announces 13 bytes of data
// but the file ends after `keep` of them, so the decoder runs out of input in
// the middle of the header it needs to size anything.
func pngTruncatedIHDR(t *testing.T, keep int) []byte {
	t.Helper()
	full := pngChunk("IHDR", pngIHDRData(100, 100))
	// 4-byte length + 4-byte type + keep bytes of data; no CRC.
	return append(pngSignature(), full[:8+keep]...)
}

// pngBadCRC builds a PNG whose IHDR carries a CRC that does not match its
// contents, which the decoder checks before trusting the dimensions.
func pngBadCRC(t *testing.T, width, height uint32) []byte {
	t.Helper()
	data := pngIHDRData(width, height)
	correct := crc32.NewIEEE()
	correct.Write([]byte("IHDR"))
	correct.Write(data)
	return append(pngSignature(), pngRawChunk("IHDR", data, correct.Sum32()^0xFFFFFFFF)...)
}

// webpTruncated builds a RIFF/WEBP container that ends partway through the
// VP8 chunk, before the start code and dimensions.
func webpTruncated(t *testing.T) []byte {
	t.Helper()
	full := webpHeader(t, 100, 100)
	// RIFF(4) size(4) WEBP(4) "VP8 "(4) chunksize(4) = 20 bytes of container,
	// then 3 of the 10 VP8 payload bytes: inside the chunk, before the start code.
	return full[:23]
}
