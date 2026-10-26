package media

import (
	"encoding/binary"
	"fmt"
	"image"
)

// stripWebPMetadata keeps the RIFF chunks and their padding intact, apart from
// EXIF, XMP and C2PA (content credentials). It returns the orientation recorded
// in the removed EXIF chunk and the first ICCP chunk's profile, which stays in
// the output.
func stripWebPMetadata(data []byte) (output []byte, orientation int, iccProfile []byte, err error) {
	if len(data) < 12 {
		return nil, 1, nil, fmt.Errorf("truncated WebP RIFF header")
	}
	extent := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if extent < 12 || extent > uint64(len(data)) {
		return nil, 1, nil, fmt.Errorf("invalid WebP RIFF size")
	}
	output = make([]byte, 12, int(extent))
	copy(output, data[:12])
	orientation = 1
	for pos := 12; uint64(pos) < extent; {
		if extent-uint64(pos) < 8 {
			return nil, 1, nil, fmt.Errorf("truncated WebP chunk header")
		}
		length := uint64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		padded := length + length%2
		if padded > extent-uint64(pos)-8 {
			return nil, 1, nil, fmt.Errorf("WebP chunk exceeds RIFF extent")
		}
		end := pos + 8 + int(padded)
		switch string(data[pos : pos+4]) {
		case "EXIF":
			orientation = exifOrientation(data[pos+8 : pos+8+int(length)])
		case "XMP ", "C2PA":
		case "ICCP":
			if iccProfile == nil {
				iccProfile = data[pos+8 : pos+8+int(length)]
			}
			output = append(output, data[pos:end]...)
		case "VP8X":
			if length < 1 {
				return nil, 1, nil, fmt.Errorf("truncated WebP VP8X flags")
			}
			start := len(output)
			output = append(output, data[pos:end]...)
			output[start+8] &^= 0x0c
		default:
			output = append(output, data[pos:end]...)
		}
		pos = end
	}
	binary.LittleEndian.PutUint32(output[4:8], uint32(len(output)-8))
	return output, orientation, iccProfile, nil
}

// encodeOrientedWebP re-encodes oriented pixels, which stdlib cannot write as
// WebP: PNG when any pixel is not fully opaque, JPEG otherwise. Both keep the
// ICC profile.
func encodeOrientedWebP(decoded image.Image, orientation int, iccProfile []byte) (Result, error) {
	oriented := OrientImage(decoded, orientation)
	result := Result{Width: oriented.Bounds().Dx(), Height: oriented.Bounds().Dy()}
	var err error
	if !oriented.Opaque() {
		result.Format = FormatPNG
		result.Data, err = encodePNGWithICCProfile(oriented, iccProfile)
		return result, err
	}
	segments, err := iccProfileSegments(iccProfile)
	if err != nil {
		return Result{}, err
	}
	result.Format = FormatJPEG
	result.Data, err = encodeJPEGWithSegments(oriented, segments, orientedJPEGQuality)
	return result, err
}
