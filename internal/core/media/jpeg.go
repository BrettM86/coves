package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
)

// maxJPEGScans bounds decode work, which image/jpeg does not: every scan costs
// time proportional to the pixel count (about 7.6 ms per duplicated AC scan
// at 49 megapixels; 2,000 scans in a 342 KB file took 15.2 s), and decoding
// ignores the request context. Real encoders emit few scans. Measured with
// libjpeg-turbo 3.2: cjpeg and jpegtran -progressive write 10 scans for YCbCr
// and 6 for grayscale, and ImageMagick's progressive CMYK writes 18; baseline
// files have one per component at most. 100 accepts all of these with room for
// custom scan scripts and bounds a decode at about 0.8 s, well under
// libjpeg-turbo's own 500-scan default limit.
const maxJPEGScans = 100

// walkJPEG visits markers and scan data through the first EOI. Entropy-coded
// bytes include stuffed FF00 and restart markers; all other markers resume
// segment parsing, including metadata placed between progressive scans. It
// rejects files with more than maxJPEGScans scans.
func walkJPEG(data []byte, visit func(marker byte, segment []byte), visitScan func([]byte)) error {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return fmt.Errorf("missing JPEG SOI")
	}
	scans := 0
	for pos := 2; pos < len(data); {
		start := pos
		if data[pos] != 0xff {
			return fmt.Errorf("invalid JPEG marker")
		}
		for pos < len(data) && data[pos] == 0xff {
			pos++
		}
		if pos == len(data) {
			return fmt.Errorf("truncated JPEG marker")
		}
		marker := data[pos]
		pos++
		if marker == 0xd9 {
			if scans == 0 {
				return fmt.Errorf("JPEG EOI before scan")
			}
			visit(marker, data[start:pos])
			return nil // Never retain trailing data, including another JPEG.
		}
		if marker == 0x00 || marker == 0xd8 || marker >= 0xd0 && marker <= 0xd7 {
			return fmt.Errorf("unexpected JPEG marker outside scan")
		}
		if marker != 0x01 {
			if len(data)-pos < 2 {
				return fmt.Errorf("truncated JPEG segment length")
			}
			length := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			if length < 2 || length > len(data)-pos {
				return fmt.Errorf("invalid JPEG segment length")
			}
			pos += length
		}
		if marker == 0xda {
			scans++
			if scans > maxJPEGScans {
				return fmt.Errorf("JPEG has more than %d scans", maxJPEGScans)
			}
		}
		visit(marker, data[start:pos])
		if marker == 0xda {
			scanStart := pos
			for pos < len(data) {
				if data[pos] != 0xff {
					pos++
					continue
				}
				nextPos := pos + 1
				for nextPos < len(data) && data[nextPos] == 0xff {
					nextPos++
				}
				if nextPos == len(data) {
					return fmt.Errorf("truncated JPEG scan marker")
				}
				next := data[nextPos]
				if next == 0x00 || next >= 0xd0 && next <= 0xd7 {
					pos = nextPos + 1
					continue
				}
				break
			}
			if visitScan != nil {
				visitScan(data[scanStart:pos])
			}
		}
	}
	return fmt.Errorf("missing JPEG EOI")
}

// isJPEGMetadataMarker reports whether marker is an APPn or COM segment. Every
// other marker is structural: tables, frame and scan headers, restart interval.
func isJPEGMetadataMarker(marker byte) bool {
	return marker >= 0xe0 && marker <= 0xef || marker == 0xfe
}

// retainedJPEGMetadata is the metadata allowlist both output paths share. It
// returns the bytes to write for an APPn or COM segment, and false to drop it.
//
//   - APP2 ICC_PROFILE survives on both paths: it defines the pixel colours.
//   - APP0 JFIF and APP14 Adobe tell a decoder how to read the scan data, so
//     they survive only when that scan data does (keepScanDecodingSegments).
//     Go's encoder writes its own YCbCr scan, which an original Adobe
//     transform flag would mis-describe. JFIF is rewritten to its 16-byte
//     form so an embedded thumbnail of the uncropped original cannot survive.
//   - Every other APPn (Exif, XMP, JFXX, MPF, FPXR, IPTC, GPMF, JUMBF, ...)
//     and COM is dropped.
func retainedJPEGMetadata(marker byte, segment []byte, keepScanDecodingSegments bool) ([]byte, bool) {
	// walkJPEG includes optional FF fill before the actual marker. Emit a
	// canonical segment so its marker and length remain adjacent.
	for len(segment) > 2 && segment[1] == 0xff {
		segment = segment[1:]
	}
	payload := segment[4:]
	switch {
	case marker == 0xe2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")):
		return segment, true
	case keepScanDecodingSegments && marker == 0xe0 && bytes.HasPrefix(payload, []byte("JFIF\x00")) && len(payload) >= jfifSegmentPayloadSize:
		jfif := make([]byte, 0, 4+jfifSegmentPayloadSize)
		jfif = append(jfif, 0xff, 0xe0)
		jfif = binary.BigEndian.AppendUint16(jfif, 2+jfifSegmentPayloadSize)
		// Keep identifier, version, units and density; zero the thumbnail size.
		jfif = append(jfif, payload[:jfifSegmentPayloadSize-2]...)
		return append(jfif, 0, 0), true
	case keepScanDecodingSegments && marker == 0xee && bytes.HasPrefix(payload, []byte("Adobe")):
		return segment, true
	default:
		return nil, false
	}
}

// jfifSegmentPayloadSize is a JFIF APP0 payload without a thumbnail.
const jfifSegmentPayloadSize = 14

// stripJPEGMetadata copies the structural segments and scan data unchanged and
// applies the metadata allowlist, so the scan data is never re-encoded.
func stripJPEGMetadata(data []byte) ([]byte, error) {
	output := make([]byte, 0, len(data))
	output = append(output, data[:2]...)
	err := walkJPEG(data, func(marker byte, segment []byte) {
		if !isJPEGMetadataMarker(marker) {
			output = append(output, segment...)
		} else if retained, keep := retainedJPEGMetadata(marker, segment, true); keep {
			output = append(output, retained...)
		}
	}, func(scan []byte) {
		output = append(output, scan...)
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}

// orientedJPEGQuality is the JPEG quality of an oriented, full-size re-encode.
const orientedJPEGQuality = 92

func encodeOrientedJPEG(img image.Image, original []byte) ([]byte, error) {
	retainedSegments, err := retainedJPEGReencodeSegments(original)
	if err != nil {
		return nil, err
	}
	return encodeJPEGWithSegments(img, retainedSegments, orientedJPEGQuality)
}

// retainedJPEGReencodeSegments is the original's metadata that survives a
// re-encode of its pixels.
func retainedJPEGReencodeSegments(original []byte) ([][]byte, error) {
	var retainedSegments [][]byte
	err := walkJPEG(original, func(marker byte, segment []byte) {
		if !isJPEGMetadataMarker(marker) {
			return // Go's encoder writes its own structural segments.
		}
		if retained, keep := retainedJPEGMetadata(marker, segment, false); keep {
			retainedSegments = append(retainedSegments, retained)
		}
	}, nil)
	if err != nil {
		return nil, err
	}
	return retainedSegments, nil
}

// encodeJPEGWithSegments encodes img at quality and inserts the given
// complete marker segments directly after SOI.
func encodeJPEGWithSegments(img image.Image, segments [][]byte, quality int) ([]byte, error) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	outputSize := encoded.Len()
	for _, segment := range segments {
		outputSize += len(segment)
	}
	output := make([]byte, 0, outputSize)
	output = append(output, encoded.Bytes()[:2]...)
	for _, segment := range segments {
		output = append(output, segment...)
	}
	output = append(output, encoded.Bytes()[2:]...)
	return output, nil
}

// iccProfileSegments splits an ICC profile into APP2 ICC_PROFILE segments,
// each with a 1-based sequence number and the segment count.
func iccProfileSegments(profile []byte) ([][]byte, error) {
	const header = "ICC_PROFILE\x00"
	const maxChunk = 0xffff - 2 - len(header) - 2
	count := (len(profile) + maxChunk - 1) / maxChunk
	if count > 255 {
		return nil, fmt.Errorf("ICC profile of %d bytes exceeds 255 APP2 segments", len(profile))
	}
	segments := make([][]byte, 0, count)
	for sequence := 1; len(profile) > 0; sequence++ {
		chunk := profile[:min(len(profile), maxChunk)]
		profile = profile[len(chunk):]
		segment := make([]byte, 0, 4+len(header)+2+len(chunk))
		segment = append(segment, 0xff, 0xe2)
		segment = binary.BigEndian.AppendUint16(segment, uint16(2+len(header)+2+len(chunk)))
		segment = append(segment, header...)
		segment = append(segment, byte(sequence), byte(count))
		segments = append(segments, append(segment, chunk...))
	}
	return segments, nil
}
