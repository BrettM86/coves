package testkit

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

// MaxIDLength is the hard cap on a generated identifier.
//
// It comes from the PDS: a handle's local label — the part before the first dot
// in "alice.local.coves.dev" — is capped at 18 characters, and exceeding it
// fails account creation with a "Handle too long" error that reads like a test
// bug rather than a naming-scheme bug. Every generated ID is kept inside that
// budget so any of them can be used as a handle label.
const MaxIDLength = 18

// runPrefixLength is the width of the per-process random prefix: one letter
// plus six base36 characters.
//
// Uniqueness across processes is probabilistic, and this is the honest
// statement of it: 26 × 36^6 ≈ 5.7×10^10 possible prefixes, so by the birthday
// bound the chance that any two of 10,000 runs against the same persistent PDS
// share a prefix is under 0.1%. Within a run, collisions are impossible rather
// than unlikely — the atomic counter guarantees it. Seven characters is the
// most the 18-character handle budget affords while leaving room for a readable
// prefix in UniqueIDWithPrefix.
const runPrefixLength = 7

var (
	runPrefixOnce = sync.OnceValue(newRunPrefix)
	idCounter     = newCounter()
)

// counter is a process-wide monotonic sequence. Wrapped rather than used bare
// so the two independent sequences (identifiers, clone names) cannot be
// accidentally shared.
type counter struct{ n atomic.Uint64 }

func newCounter() *counter { return &counter{} }

func (c *counter) next() uint64 { return c.n.Add(1) }

// RunPrefix returns this process's random identifier prefix.
//
// It is also embedded in every clone database name, which is what lets
// scripts/test-db-prepare.sh tell one run's leftovers from another's.
func RunPrefix() string { return runPrefixOnce() }

// newRunPrefix draws a random prefix that starts with a letter.
//
// A counter alone is not enough, which the suite learned the hard way: counters
// restart at 1 in every process, and the local PDS keeps accounts across runs,
// so two `make test-e2e` invocations would try to register the same handle. A
// timestamp alone is not enough either — second granularity collides within a
// single fast run, and a ten-digit Unix timestamp eats most of the 18-character
// budget.
//
// The leading letter matters: a handle's label may not begin with a digit, and
// a purely numeric label is rejected outright.
func newRunPrefix() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the process has no entropy source. There is
		// no sensible fallback and a duplicate-handle failure three layers away
		// would be far harder to diagnose than this.
		panic(fmt.Sprintf("testkit: reading random bytes for the run prefix: %v", err))
	}
	letter := byte('a' + b[0]%26)
	// Reduced to a fixed width, so every generated ID has the same shape and
	// the budget arithmetic below is exact. Drawn from 64 bits rather than 32
	// so the modulo reduction's bias is immeasurable rather than a factor of
	// two across half the range.
	const span = 36 * 36 * 36 * 36 * 36 * 36 // 36^6
	n := binary.BigEndian.Uint64(b[1:9]) % span
	digits := strconv.FormatUint(n, 36)
	return string(letter) + strings.Repeat("0", runPrefixLength-1-len(digits)) + digits
}

// UniqueID returns an identifier unique across every process, machine and run
// that shares this Postgres or PDS.
//
// It is the only handle/ID generator the suite may use. Hand-rolled
// time.Now().Unix() identifiers are why handles collided between runs against
// the persistent dev PDS, and why two tests started in the same second
// occasionally fought over one account.
//
// The result is lowercase alphanumeric, starts with a letter, and is at most
// MaxIDLength characters, so it is directly usable as a PDS handle label, a
// community name, an email local part, or a database-safe suffix.
func UniqueID(t TestingT) string {
	t.Helper()
	id := RunPrefix() + strconv.FormatUint(idCounter.next(), 36)
	if len(id) > MaxIDLength {
		t.Fatalf("testkit.UniqueID: generated %q (%d chars), over the %d-char PDS handle budget",
			id, len(id), MaxIDLength)
		return ""
	}
	return id
}

// UniqueIDWithPrefix returns a unique identifier that starts with a readable
// prefix, so a leftover row or account says which test made it.
//
// The prefix is sanitised to lowercase alphanumerics and truncated as needed:
// uniqueness wins over readability, because a truncated prefix costs a
// debugging hint while a truncated random suffix costs a collision.
func UniqueIDWithPrefix(t TestingT, prefix string) string {
	t.Helper()
	suffix := RunPrefix() + strconv.FormatUint(idCounter.next(), 36)
	if len(suffix) > MaxIDLength {
		t.Fatalf("testkit.UniqueIDWithPrefix: generated %q (%d chars), over the %d-char PDS handle budget",
			suffix, len(suffix), MaxIDLength)
		return ""
	}

	clean := sanitizeLabel(prefix)
	if budget := MaxIDLength - len(suffix); len(clean) > budget {
		clean = clean[:max(budget, 0)]
	}
	return clean + suffix
}

// sanitizeLabel reduces an arbitrary string to lowercase alphanumerics and
// guarantees it starts with a letter, so the result stays a legal handle label.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out != "" && out[0] >= '0' && out[0] <= '9' {
		out = "t" + out
	}
	return out
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

// TestPNG returns a decodable PNG of the requested size.
//
// The pixels are a deterministic gradient rather than a single flat colour:
// flat images compress to a handful of bytes and encode to a nearly empty JPEG,
// which is a poor stand-in for a real upload when the code under test is
// resizing, sniffing, or enforcing a size budget.
func TestPNG(width, height int) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, gradientImage(width, height)); err != nil {
		panic(fmt.Sprintf("testkit.TestPNG(%d, %d): %v", width, height, err))
	}
	return buf.Bytes()
}

// TestPNGColor returns a decodable PNG filled with a single colour, for tests
// that assert on pixel values.
func TestPNGColor(width, height int, c color.Color) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, solidImage(width, height, c)); err != nil {
		panic(fmt.Sprintf("testkit.TestPNGColor(%d, %d): %v", width, height, err))
	}
	return buf.Bytes()
}

// TestJPEG returns a decodable JPEG of the requested size, encoded at quality
// 90 to match what the image pipeline produces.
func TestJPEG(width, height int) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gradientImage(width, height), &jpeg.Options{Quality: 90}); err != nil {
		panic(fmt.Sprintf("testkit.TestJPEG(%d, %d): %v", width, height, err))
	}
	return buf.Bytes()
}

// TestJPEGColor returns a decodable JPEG filled with a single colour.
func TestJPEGColor(width, height int, c color.Color) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, solidImage(width, height, c), &jpeg.Options{Quality: 90}); err != nil {
		panic(fmt.Sprintf("testkit.TestJPEGColor(%d, %d): %v", width, height, err))
	}
	return buf.Bytes()
}

func gradientImage(width, height int) *image.RGBA {
	img := newImage(width, height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(x * 255 / width),
				G: uint8(y * 255 / height),
				B: 128,
				A: 255,
			})
		}
	}
	return img
}

func solidImage(width, height int, c color.Color) *image.RGBA {
	img := newImage(width, height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func newImage(width, height int) *image.RGBA {
	if width <= 0 || height <= 0 {
		// A zero-sized image encodes to bytes that no decoder accepts, so the
		// failure would otherwise surface as an unrelated "invalid image" from
		// whatever consumed it.
		panic(fmt.Sprintf("testkit: image dimensions must be positive, got %dx%d", width, height))
	}
	return image.NewRGBA(image.Rect(0, 0, width, height))
}
