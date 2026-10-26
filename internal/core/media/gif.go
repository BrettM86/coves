package media

import (
	"bytes"
	"compress/lzw"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	gifExtensionIntroducer = 0x21
	gifImageSeparator      = 0x2c
	gifTrailer             = 0x3b

	gifLabelPlainText      = 0x01
	gifLabelGraphicControl = 0xf9
	gifLabelComment        = 0xfe
	gifLabelApplication    = 0xff
)

var (
	errGIFTooMuchImageData = errors.New("GIF frame holds more pixel data than its bounds")
	errGIFNotEnoughData    = errors.New("GIF frame holds less pixel data than its bounds")
	errGIFIndexOutOfRange  = errors.New("GIF frame uses a colour index outside its colour table")
)

// gifFrameCount walks the whole GIF container without inflating any LZW data
// and returns its number of frames. It rejects a truncated or malformed
// container, a frame outside the logical screen and a frame without a colour
// table, and any plain text or unknown extension. Bytes after the trailer are
// allowed; sanitizeGIF drops them.
func gifFrameCount(data []byte) (int, error) {
	frames, _, err := walkGIF(data, false)
	return frames, err
}

// sanitizeGIF validates every frame's pixel data and rewrites the container
// without metadata: comment extensions, application extensions other than
// NETSCAPE2.0 looping, any NETSCAPE2.0 sub-block besides the loop count, and
// bytes after the trailer are dropped. The logical screen, colour tables,
// graphic control extensions, image descriptors and LZW data are copied byte
// for byte; a plain text extension is rejected, as gifFrameCount does. Frames are streamed, never retained.
func sanitizeGIF(data []byte) ([]byte, error) {
	_, clean, err := walkGIF(data, true)
	return clean, err
}

func walkGIF(data []byte, sanitize bool) (frames int, clean []byte, err error) {
	if len(data) < 13 || (!bytes.HasPrefix(data, []byte("GIF87a")) && !bytes.HasPrefix(data, []byte("GIF89a"))) {
		return 0, nil, fmt.Errorf("invalid GIF header")
	}
	screenWidth := int(binary.LittleEndian.Uint16(data[6:8]))
	screenHeight := int(binary.LittleEndian.Uint16(data[8:10]))
	walker := gifWalker{data: data, position: 13}
	globalTableLength := 0
	if data[10]&0x80 != 0 {
		globalTableLength = 2 << (data[10] & 0x07)
		if _, ok := walker.take(3 * globalTableLength); !ok {
			return 0, nil, fmt.Errorf("truncated GIF global colour table")
		}
	}

	var frameChecker *gifFrameChecker
	if sanitize {
		clean = make([]byte, 0, len(data))
		clean = append(clean, data[:walker.position]...)
		frameChecker = &gifFrameChecker{}
	}
	loopWritten := false
	transparentIndex := -1 // from the graphic control extension for the next frame
	for {
		start := walker.position
		introducer, ok := walker.take(1)
		if !ok {
			return 0, nil, fmt.Errorf("missing GIF trailer")
		}
		switch introducer[0] {
		case gifTrailer:
			if frames == 0 {
				return 0, nil, fmt.Errorf("GIF has no frames")
			}
			if sanitize {
				clean = append(clean, gifTrailer)
			}
			return frames, clean, nil

		case gifExtensionIntroducer:
			label, ok := walker.take(1)
			if !ok {
				return 0, nil, fmt.Errorf("truncated GIF extension")
			}
			keep := true
			switch label[0] {
			case gifLabelGraphicControl:
				// Block size 4, packed fields, delay, transparent index, terminator.
				body, ok := walker.take(6)
				if !ok || body[0] != 4 || body[5] != 0 {
					return 0, nil, fmt.Errorf("invalid GIF graphic control extension")
				}
				transparentIndex = -1
				if body[1]&0x01 != 0 {
					transparentIndex = int(body[4])
				}
			case gifLabelPlainText:
				// Browsers do not render plain text, so it could only carry
				// hidden text; no common tool writes it.
				return 0, nil, fmt.Errorf("GIF plain text extensions are not accepted")
			case gifLabelComment:
				keep = false
				if !walker.skipSubBlocks() {
					return 0, nil, fmt.Errorf("truncated GIF comment extension")
				}
			case gifLabelApplication:
				keep = false
				loop, ok := walker.netscapeLoopCount()
				if !ok {
					return 0, nil, fmt.Errorf("truncated GIF application extension")
				}
				if sanitize && loop != nil && !loopWritten {
					clean = append(clean, gifExtensionIntroducer, gifLabelApplication, 11)
					clean = append(clean, "NETSCAPE2.0"...)
					clean = append(clean, 3, 1, loop[0], loop[1], 0)
					loopWritten = true
				}
			default:
				return 0, nil, fmt.Errorf("unknown GIF extension 0x%02x", label[0])
			}
			if sanitize && keep {
				clean = append(clean, data[start:walker.position]...)
			}

		case gifImageSeparator:
			descriptor, ok := walker.take(9)
			if !ok {
				return 0, nil, fmt.Errorf("truncated GIF image descriptor")
			}
			left := int(binary.LittleEndian.Uint16(descriptor[0:2]))
			top := int(binary.LittleEndian.Uint16(descriptor[2:4]))
			width := int(binary.LittleEndian.Uint16(descriptor[4:6]))
			height := int(binary.LittleEndian.Uint16(descriptor[6:8]))
			if left+width > screenWidth || top+height > screenHeight {
				return 0, nil, fmt.Errorf("GIF frame bounds larger than logical screen")
			}
			paletteLength := globalTableLength
			if packed := descriptor[8]; packed&0x80 != 0 {
				paletteLength = 2 << (packed & 0x07)
				if _, ok := walker.take(3 * paletteLength); !ok {
					return 0, nil, fmt.Errorf("truncated GIF local colour table")
				}
			}
			if paletteLength == 0 {
				return 0, nil, fmt.Errorf("GIF frame has no colour table")
			}
			minimumCodeSize, ok := walker.take(1)
			if !ok {
				return 0, nil, fmt.Errorf("truncated GIF image data")
			}
			if minimumCodeSize[0] < 2 || minimumCodeSize[0] > 8 {
				return 0, nil, fmt.Errorf("GIF LZW minimum code size out of range: %d", minimumCodeSize[0])
			}
			imageData := walker.position
			if !walker.skipSubBlocks() {
				return 0, nil, fmt.Errorf("truncated GIF image data")
			}
			if sanitize {
				// As image/gif does, a transparent index past the table
				// extends it with transparent entries.
				if transparentIndex >= paletteLength {
					paletteLength = transparentIndex + 1
				}
				err := frameChecker.check(data[imageData:walker.position], int(minimumCodeSize[0]), width*height, paletteLength)
				if err != nil {
					return 0, nil, fmt.Errorf("GIF frame %d: %w", frames+1, err)
				}
				clean = append(clean, data[start:walker.position]...)
			}
			transparentIndex = -1
			frames++

		default:
			return 0, nil, fmt.Errorf("invalid GIF block 0x%02x", introducer[0])
		}
	}
}

type gifWalker struct {
	data     []byte
	position int
}

func (w *gifWalker) take(length int) ([]byte, bool) {
	if length > len(w.data)-w.position {
		return nil, false
	}
	taken := w.data[w.position : w.position+length]
	w.position += length
	return taken, true
}

// subBlock returns the next data sub-block, or an empty slice at the block
// terminator.
func (w *gifWalker) subBlock() ([]byte, bool) {
	length, ok := w.take(1)
	if !ok {
		return nil, false
	}
	return w.take(int(length[0]))
}

func (w *gifWalker) skipSubBlocks() bool {
	for {
		block, ok := w.subBlock()
		if !ok {
			return false
		}
		if len(block) == 0 {
			return true
		}
	}
}

// netscapeLoopCount walks an application extension's sub-blocks and returns
// the little-endian loop count when it is a NETSCAPE2.0 extension with a loop
// sub-block (ID 1), or nil otherwise.
func (w *gifWalker) netscapeLoopCount() (loop []byte, ok bool) {
	identifier, ok := w.subBlock()
	if !ok {
		return nil, false
	}
	if len(identifier) == 0 {
		return nil, true
	}
	netscape := string(identifier) == "NETSCAPE2.0"
	for {
		block, ok := w.subBlock()
		if !ok {
			return nil, false
		}
		if len(block) == 0 {
			return loop, true
		}
		if netscape && loop == nil && len(block) == 3 && block[0] == 1 {
			loop = block[1:3]
		}
	}
}

// gifFrameChecker inflates frames one at a time into a counter, reusing its
// LZW reader and buffer across frames.
type gifFrameChecker struct {
	reader  lzw.Reader
	indices [4096]byte
}

// check inflates one frame's LZW data (subBlocks, its sub-blocks up to and
// including the terminator) and requires exactly pixels indices, each below
// paletteLength. Like image/gif it tolerates a missing end code once every
// pixel is read, and at most one sub-block's worth of bytes after the end code.
func (c *gifFrameChecker) check(subBlocks []byte, minimumCodeSize, pixels, paletteLength int) error {
	source := gifSubBlockReader{remaining: subBlocks}
	c.reader.Reset(&source, lzw.LSB, minimumCodeSize)
	defer func() { _ = c.reader.Close() }()
	read := 0
	for {
		n, err := c.reader.Read(c.indices[:])
		if paletteLength < 256 {
			for _, index := range c.indices[:n] {
				if int(index) >= paletteLength {
					return errGIFIndexOutOfRange
				}
			}
		}
		read += n
		if read > pixels {
			return errGIFTooMuchImageData
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("inflating GIF image data: %w", err)
		}
	}
	if read < pixels {
		return errGIFNotEnoughData
	}
	return source.finish()
}

// gifSubBlockReader yields the bytes of a well-formed sub-block sequence that
// the container walk has already bounds-checked.
type gifSubBlockReader struct {
	remaining  []byte // from the next sub-block's length byte
	block      []byte // unread bytes of the current sub-block
	terminated bool
}

func (r *gifSubBlockReader) nextBlock() {
	length := int(r.remaining[0])
	if length == 0 {
		r.terminated = true
		r.block = nil
		return
	}
	r.block = r.remaining[1 : 1+length]
	r.remaining = r.remaining[1+length:]
}

func (r *gifSubBlockReader) ReadByte() (byte, error) {
	for len(r.block) == 0 {
		if r.terminated {
			return 0, io.EOF
		}
		r.nextBlock()
	}
	b := r.block[0]
	r.block = r.block[1:]
	return b, nil
}

func (r *gifSubBlockReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.block) == 0 {
		if r.terminated {
			return 0, io.EOF
		}
		r.nextBlock()
	}
	n := copy(p, r.block)
	r.block = r.block[n:]
	return n, nil
}

// finish allows what image/gif allows after the LZW end code: the rest of the
// current sub-block, or one more one-byte sub-block when the end code
// finished a sub-block, then the terminator.
func (r *gifSubBlockReader) finish() error {
	if r.terminated {
		return nil
	}
	if len(r.block) == 0 {
		r.nextBlock()
		if r.terminated {
			return nil
		}
		if len(r.block) > 1 {
			return errGIFTooMuchImageData
		}
	}
	r.nextBlock()
	if r.terminated {
		return nil
	}
	return errGIFTooMuchImageData
}
