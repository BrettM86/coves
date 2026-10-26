package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
)

// stripPNGMetadata preserves each retained chunk, including its CRC, without
// touching the compressed image data.
func stripPNGMetadata(data []byte) ([]byte, error) {
	output := make([]byte, 0, len(data))
	output = append(output, data[:8]...)
	for pos := 8; pos < len(data); {
		if len(data)-pos < 12 {
			return nil, fmt.Errorf("truncated PNG chunk header")
		}
		length := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		if length > uint64(len(data)-pos-12) {
			return nil, fmt.Errorf("PNG chunk exceeds input")
		}
		end := pos + 12 + int(length)
		switch string(data[pos+4 : pos+8]) {
		// caBX holds C2PA content credentials; exIf and zxIf are legacy
		// ImageMagick raw and compressed EXIF.
		case "eXIf", "tEXt", "iTXt", "zTXt", "tIME", "caBX", "exIf", "zxIf":
		default:
			output = append(output, data[pos:end]...)
		}
		if string(data[pos+4:pos+8]) == "IEND" {
			return output, nil
		}
		pos = end
	}
	return nil, fmt.Errorf("missing PNG IEND")
}

// pngChunksBeforeImageData walks the chunks before the first IDAT. It returns
// the colour chunks there (gAMA, cHRM, sRGB, iCCP, cICP, mDCv and cLLi), each
// verbatim with length and CRC, concatenated in input order, and whether an
// acTL there makes the input an animated PNG.
func pngChunksBeforeImageData(data []byte) (colorChunks []byte, animated bool, err error) {
	for pos := 8; pos < len(data); {
		if len(data)-pos < 12 {
			return nil, false, fmt.Errorf("truncated PNG chunk header")
		}
		length := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		if length > uint64(len(data)-pos-12) {
			return nil, false, fmt.Errorf("PNG chunk exceeds input")
		}
		end := pos + 12 + int(length)
		switch string(data[pos+4 : pos+8]) {
		case "gAMA", "cHRM", "sRGB", "iCCP", "cICP", "mDCv", "cLLi":
			colorChunks = append(colorChunks, data[pos:end]...)
		case "acTL":
			animated = true
		case "IDAT", "IEND":
			// Colour chunks and acTL count only before the image data.
			return colorChunks, animated, nil
		}
		pos = end
	}
	return nil, false, fmt.Errorf("missing PNG IEND")
}

// encodePNGWithICCProfile encodes img as PNG and, when profile is not empty,
// inserts it as an iCCP chunk after IHDR, since image/png does not write one.
func encodePNGWithICCProfile(img image.Image, profile []byte) ([]byte, error) {
	if len(profile) == 0 {
		return encodePNGWithChunk(img, nil)
	}
	chunkData := []byte("ICC Profile\x00\x00") // name, NUL, compression method 0
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(profile); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	chunkData = append(chunkData, compressed.Bytes()...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(chunkData)))
	chunk = append(chunk, "iCCP"...)
	chunk = append(chunk, chunkData...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	return encodePNGWithChunk(img, chunk)
}

// encodePNGWithChunk encodes img as PNG and, when chunk is not empty, inserts
// it, one or more complete chunks, directly after IHDR.
func encodePNGWithChunk(img image.Image, chunk []byte) ([]byte, error) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		return nil, err
	}
	if len(chunk) == 0 {
		return encoded.Bytes(), nil
	}
	const headerEnd = 8 + 12 + 13 // signature, then the IHDR chunk
	data := encoded.Bytes()
	if len(data) < headerEnd || string(data[12:16]) != "IHDR" {
		return nil, fmt.Errorf("encoded PNG does not start with IHDR")
	}
	output := make([]byte, 0, len(data)+len(chunk))
	output = append(output, data[:headerEnd]...)
	output = append(output, chunk...)
	return append(output, data[headerEnd:]...), nil
}
