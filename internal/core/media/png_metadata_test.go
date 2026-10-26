package media_test

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"

	"Coves/internal/core/media"
)

func TestPipelineDropsPNGMetadataChunksLosslessly(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 6, 4))
	colours := [4]image.Image{image.NewUniform(red), image.NewUniform(green), image.NewUniform(blue), image.NewUniform(yellow)}
	for y := 0; y < 4; y++ {
		for x := 0; x < 6; x++ {
			img.Set(x, y, colours[(x/3+2*(y/2))%4].At(0, 0))
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	base := encoded.Bytes()
	if len(base) < 8 || !bytes.Equal(base[:8], []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("encoded PNG has no signature")
	}
	var ihdr, idat, iend []byte
	for offset := 8; offset < len(base); {
		if offset+12 > len(base) {
			t.Fatal("truncated encoded PNG chunk")
		}
		length := int(binary.BigEndian.Uint32(base[offset:]))
		end := offset + 12 + length
		if end > len(base) {
			t.Fatal("encoded PNG chunk exceeds input")
		}
		switch string(base[offset+4 : offset+8]) {
		case "IHDR":
			ihdr = append(ihdr, base[offset:end]...)
		case "IDAT":
			idat = append(idat, base[offset:end]...)
		case "IEND":
			iend = append(iend, base[offset:end]...)
		default:
			t.Fatalf("unexpected encoder PNG chunk %q", base[offset+4:offset+8])
		}
		offset = end
	}
	if len(ihdr) == 0 || len(idat) == 0 || len(iend) == 0 {
		t.Fatal("encoded PNG lacks structural chunks")
	}
	retainedBefore := bytes.Join([][]byte{pngChunk("gAMA", []byte{0, 0, 0xb1, 0x8f}), pngChunk("pHYs", []byte{0, 0, 0x0e, 0xc4, 0, 0, 0x0e, 0xc4, 1}), pngChunk("iCCP", []byte("profile\x00\x00x\x9c\x03\x00\x00\x00\x00\x01"))}, nil)
	retainedAfter := pngChunk("vpAg", []byte("retained"))
	signature := base[:8]
	expected := bytes.Join([][]byte{signature, ihdr, retainedBefore, idat, retainedAfter, iend}, nil)
	input := bytes.Join([][]byte{signature, ihdr, pngChunk("eXIf", []byte("II*\x00gps")), pngChunk("tEXt", []byte("Author\x00person")), retainedBefore, idat, pngChunk("iTXt", []byte("Comment\x00\x00\x00\x00\x00private")), retainedAfter, pngChunk("zTXt", []byte("Comment\x00\x00x\x9c\x03\x00\x00\x00\x00\x01")), pngChunk("tIME", []byte{7, 234, 1, 2, 3, 4, 5}), pngChunk("caBX", []byte("jumbc2pa private manifest")), pngChunk("exIf", []byte("II*\x00legacy gps")), pngChunk("zxIf", []byte("x\x9c\x03\x00\x00\x00\x00\x01")), iend, []byte("trailing private data")}, nil)
	if _, err := png.Decode(bytes.NewReader(input)); err != nil {
		t.Fatalf("PNG fixture must decode: %v", err)
	}
	result, err := ingest(t, pipeline(t, 1), input, "image/png", []media.Format{media.FormatPNG})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Format != media.FormatPNG || result.Width != 6 || result.Height != 4 {
		t.Fatalf("result: got %q %dx%d, want image/png 6x4", result.Format, result.Width, result.Height)
	}
	for _, dropped := range []string{"eXIf", "tEXt", "iTXt", "zTXt", "tIME", "caBX", "exIf", "zxIf"} {
		if bytes.Contains(result.Data, []byte(dropped)) {
			t.Errorf("PNG output retains %s chunk", dropped)
		}
	}
	if !bytes.Equal(result.Data, expected) {
		t.Errorf("PNG output differs from independently assembled metadata-free PNG: got %d bytes, want %d", len(result.Data), len(expected))
	}
	if _, err := png.Decode(bytes.NewReader(result.Data)); err != nil {
		t.Errorf("decode sanitized PNG: %v", err)
	}
}
