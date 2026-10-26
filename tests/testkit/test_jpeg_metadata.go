package testkit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"math/rand/v2"
)

// JPEGSegment is a complete marker segment, including its marker and length.
type JPEGSegment struct {
	Marker byte
	Bytes  []byte
}

// JPEGMarkerSegment frames a length-bearing JPEG marker with its payload.
func JPEGMarkerSegment(marker byte, payload []byte) []byte {
	if len(payload) > 65533 {
		panic("JPEG marker payload exceeds 65533 bytes")
	}
	segment := []byte{0xff, marker}
	segment = binary.BigEndian.AppendUint16(segment, uint16(len(payload)+2))
	return append(segment, payload...)
}

// JPEGWithSegments inserts segments immediately after SOI of a stdlib JPEG.
func JPEGWithSegments(encoded []byte, segments ...[]byte) []byte {
	if len(encoded) < 2 || !bytes.Equal(encoded[:2], []byte{0xff, 0xd8}) {
		panic("JPEGWithSegments requires an encoded JPEG")
	}
	out := append([]byte(nil), encoded[:2]...)
	for _, segment := range segments {
		out = append(out, segment...)
	}
	return append(out, encoded[2:]...)
}

// JPEGExifGPSSegment has an IFD0 orientation and a GPS pointer to latitude 51°30′.
func JPEGExifGPSegment(orientation uint16) []byte {
	const gpsOffset = 8 + 2 + 2*12 + 4
	const latitudeOffset = gpsOffset + 2 + 12 + 4
	tiff := make([]byte, latitudeOffset+3*8)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 2)
	// IFD0: SHORT Orientation (one inline value), then LONG GPS IFD pointer.
	binary.LittleEndian.PutUint16(tiff[10:12], 0x0112)
	binary.LittleEndian.PutUint16(tiff[12:14], 3)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint16(tiff[18:20], orientation)
	binary.LittleEndian.PutUint16(tiff[22:24], 0x8825)
	binary.LittleEndian.PutUint16(tiff[24:26], 4)
	binary.LittleEndian.PutUint32(tiff[26:30], 1)
	binary.LittleEndian.PutUint32(tiff[30:34], gpsOffset)
	// GPS IFD: GPSLatitude is three RATIONALs, stored out of line.
	binary.LittleEndian.PutUint16(tiff[gpsOffset:gpsOffset+2], 1)
	binary.LittleEndian.PutUint16(tiff[gpsOffset+2:gpsOffset+4], 0x0002)
	binary.LittleEndian.PutUint16(tiff[gpsOffset+4:gpsOffset+6], 5)
	binary.LittleEndian.PutUint32(tiff[gpsOffset+6:gpsOffset+10], 3)
	binary.LittleEndian.PutUint32(tiff[gpsOffset+10:gpsOffset+14], latitudeOffset)
	for i, value := range []uint32{51, 30, 0} {
		binary.LittleEndian.PutUint32(tiff[latitudeOffset+i*8:], value)
		binary.LittleEndian.PutUint32(tiff[latitudeOffset+i*8+4:], 1)
	}
	return JPEGMarkerSegment(0xe1, append([]byte("Exif\x00\x00"), tiff...))
}

func JPEGXMPSegment() []byte {
	return JPEGMarkerSegment(0xe1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta xmlns:x=\"adobe:ns:meta/\"/>"))
}

func JPEGICCSegment() []byte {
	return JPEGMarkerSegment(0xe2, append([]byte("ICC_PROFILE\x00\x01\x01"), []byte("test ICC profile bytes")...))
}

func JPEGIPTCSegment() []byte {
	payload := append([]byte("Photoshop 3.0\x00"), []byte("8BIM")...)
	payload = binary.BigEndian.AppendUint16(payload, 0x0404) // IPTC resource ID
	payload = append(payload, 0, 0)                          // empty Pascal name, padded
	payload = binary.BigEndian.AppendUint32(payload, 4)
	payload = append(payload, 0x1c, 0x02, 0x05, 0x00)
	return JPEGMarkerSegment(0xed, payload)
}

func JPEGCommentSegment() []byte { return JPEGMarkerSegment(0xfe, []byte("location metadata")) }

// JPEGICCPaddingSegments splits a zero-filled ICC profile across count APP2
// chunks, each the largest length-bearing segment allowed by JPEG. ICC is the
// one metadata payload the media pipeline keeps, so the padding survives it.
func JPEGICCPaddingSegments(count int) [][]byte {
	if count < 1 || count > 255 {
		panic("ICC profiles span 1 to 255 APP2 chunks")
	}
	segments := make([][]byte, 0, count)
	for sequence := 1; sequence <= count; sequence++ {
		payload := make([]byte, 65533)
		copy(payload, "ICC_PROFILE\x00")
		payload[12], payload[13] = byte(sequence), byte(count)
		segments = append(segments, JPEGMarkerSegment(0xe2, payload))
	}
	return segments
}

// JPEGThatGrowsPastTenMegabytesWhenRotated is at most 10,000,000 bytes, but
// the media pipeline's oriented re-encode of it is over 10,000,000 bytes.
//
// The pixels are 1024x1024 seeded noise encoded at quality 30 (about 294 KB)
// with Exif orientation 6, so the pipeline must rotate and re-encode them at
// its own quality 92 (about 735 KB). 147 ICC padding chunks (about 9.63 MB)
// survive that re-encode, putting the input near 9.93 MB and the output near
// 10.37 MB. The noise is seeded so the bytes are the same on every call.
func JPEGThatGrowsPastTenMegabytesWhenRotated() []byte {
	segments := append([][]byte{JPEGExifGPSegment(6)}, JPEGICCPaddingSegments(147)...)
	return JPEGWithSegments(noiseJPEGQuality30(), segments...)
}

// JPEGThatGrowsPastSixMillionBytesWhenRotated is under 6,000,000 bytes, but the
// media pipeline's oriented re-encode of it is over 6,000,000 bytes, the
// lexicon's maxSize for a link card's thumb.
//
// It is JPEGThatGrowsPastTenMegabytesWhenRotated with 86 ICC padding chunks
// (5,636,182 bytes) instead of 147: about 5.93 MB in and 6.36 MB out.
func JPEGThatGrowsPastSixMillionBytesWhenRotated() []byte {
	segments := append([][]byte{JPEGExifGPSegment(6)}, JPEGICCPaddingSegments(86)...)
	return JPEGWithSegments(noiseJPEGQuality30(), segments...)
}

// JPEGThatGrowsPastTenMegabytesWhenDownscaled is at most 10,000,000 bytes, but
// downscaling it to a long edge of 1000 and re-encoding at quality 90 keeps
// its ICC profile and produces over 10,000,000 bytes.
//
// It is the noise of JPEGThatGrowsPastTenMegabytesWhenRotated (about 294 KB)
// with no Exif, so no orientation is applied, and the same 147 ICC padding
// chunks (9,633,939 bytes): about 9.93 MB in. Scaled to 1000x1000 with any
// golang.org/x/image/draw interpolator and encoded at quality 90, the pixels
// take 659 KB to 773 KB, so the output is 10.29 MB to 10.41 MB.
func JPEGThatGrowsPastTenMegabytesWhenDownscaled() []byte {
	return JPEGWithSegments(noiseJPEGQuality30(), JPEGICCPaddingSegments(147)...)
}

// noiseJPEGQuality30 is 1024x1024 seeded noise encoded at quality 30, which
// compresses poorly at any higher quality.
func noiseJPEGQuality30() []byte {
	random := rand.New(rand.NewPCG(19, 10))
	noise := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	for index := range noise.Pix {
		if index%4 == 3 {
			noise.Pix[index] = 0xff
		} else {
			noise.Pix[index] = uint8(random.UintN(256))
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, noise, &jpeg.Options{Quality: 30}); err != nil {
		panic(fmt.Sprintf("testkit noise JPEG: %v", err))
	}
	return encoded.Bytes()
}

// WalkJPEGSegments returns pre-scan segments and the exact SOS-through-EOI tail.
func WalkJPEGSegments(data []byte) ([]JPEGSegment, []byte, error) {
	if len(data) < 4 || !bytes.Equal(data[:2], []byte{0xff, 0xd8}) {
		return nil, nil, fmt.Errorf("missing JPEG SOI")
	}
	var segments []JPEGSegment
	for offset := 2; offset+4 <= len(data); {
		if data[offset] != 0xff {
			return nil, nil, fmt.Errorf("missing JPEG marker at %d", offset)
		}
		marker := data[offset+1]
		if marker == 0xda {
			if !bytes.HasSuffix(data[offset:], []byte{0xff, 0xd9}) {
				return nil, nil, fmt.Errorf("missing JPEG EOI")
			}
			return segments, data[offset:], nil
		}
		length := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if length < 2 || offset+2+length > len(data) {
			return nil, nil, fmt.Errorf("invalid JPEG marker length at %d", offset)
		}
		segments = append(segments, JPEGSegment{Marker: marker, Bytes: data[offset : offset+2+length]})
		offset += 2 + length
	}
	return nil, nil, fmt.Errorf("missing JPEG SOS")
}
