package media

import (
	"bytes"
	"encoding/binary"
	"image"
)

// JPEGOrientation finds the Exif APP1, ignoring other APP1 payloads such as XMP.
func JPEGOrientation(data []byte) int {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	for pos := 2; pos < len(data); {
		if data[pos] != 0xff {
			return 1
		}
		for pos < len(data) && data[pos] == 0xff {
			pos++
		}
		if pos == len(data) {
			return 1
		}
		marker := data[pos]
		pos++
		if marker == 0xda || marker == 0xd9 {
			break // image data begins at SOS; it is not a segment table.
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd8 {
			continue // Standalone markers have no length field.
		}
		if len(data)-pos < 2 {
			return 1
		}
		length := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		if length < 2 || length > len(data)-pos {
			return 1
		}
		payload := data[pos+2 : pos+length : pos+length]
		if marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			return exifOrientation(payload)
		}
		pos += length
	}
	return 1
}

// exifOrientation accepts the raw TIFF form used by WebP as well as the
// Exif-prefixed form used by JPEG APP1 segments.
func exifOrientation(payload []byte) int {
	if bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
		payload = payload[6:]
	}
	return tiffOrientation(payload)
}

func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	offset := uint64(order.Uint32(tiff[4:8]))
	if offset > uint64(len(tiff)) || uint64(len(tiff))-offset < 2 {
		return 1
	}
	ifd := tiff[int(offset):]
	count := int(order.Uint16(ifd[:2]))
	if count > (len(ifd)-2)/12 {
		return 1
	}
	for i := 0; i < count; i++ {
		entry := ifd[2+i*12 : 2+(i+1)*12]
		if order.Uint16(entry[:2]) != 0x0112 {
			continue
		}
		if order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
			return 1
		}
		orientation := int(order.Uint16(entry[8:10]))
		if orientation >= 1 && orientation <= 8 {
			return orientation
		}
		return 1
	}
	return 1
}

// OrientImage writes the final display pixels. The scaled path copies directly
// between RGBA buffers; an unscaled source is copied only once.
func OrientImage(img image.Image, orientation int) *image.RGBA {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	outputWidth, outputHeight := w, h
	if orientation >= 5 {
		outputWidth, outputHeight = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, outputWidth, outputHeight))
	rgba, fast := img.(*image.RGBA)
	for y := 0; y < outputHeight; y++ {
		for x := 0; x < outputWidth; x++ {
			sourceX, sourceY := x, y
			switch orientation {
			case 2:
				sourceX = w - 1 - x
			case 3:
				sourceX, sourceY = w-1-x, h-1-y
			case 4:
				sourceY = h - 1 - y
			case 5:
				sourceX, sourceY = y, x
			case 6:
				sourceX, sourceY = y, h-1-x
			case 7:
				sourceX, sourceY = w-1-y, h-1-x
			case 8:
				sourceX, sourceY = w-1-y, x
			}
			destinationOffset := y*dst.Stride + x*4
			if fast {
				sourceOffset := sourceY*rgba.Stride + sourceX*4
				copy(dst.Pix[destinationOffset:destinationOffset+4], rgba.Pix[sourceOffset:sourceOffset+4])
			} else {
				r, g, b, a := img.At(bounds.Min.X+sourceX, bounds.Min.Y+sourceY).RGBA()
				dst.Pix[destinationOffset+0] = uint8(r >> 8)
				dst.Pix[destinationOffset+1] = uint8(g >> 8)
				dst.Pix[destinationOffset+2] = uint8(b >> 8)
				dst.Pix[destinationOffset+3] = uint8(a >> 8)
			}
		}
	}
	return dst
}
