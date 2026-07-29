package testkit

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUniqueID_Shape(t *testing.T) {
	id := UniqueID(t)

	assert.LessOrEqual(t, len(id), MaxIDLength,
		"a longer id is rejected by the PDS as 'Handle too long'")
	assert.NotEmpty(t, id)
	assertHandleSafe(t, id)
	assert.True(t, strings.HasPrefix(id, RunPrefix()),
		"ids must carry the run prefix so leftovers on a persistent PDS are attributable")
}

func TestUniqueID_UniqueUnderConcurrency(t *testing.T) {
	const (
		goroutines = 50
		perRoutine = 200
	)

	ids := make([][]string, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			batch := make([]string, perRoutine)
			for i := range batch {
				batch[i] = UniqueID(t)
			}
			ids[g] = batch
		}(g)
	}
	wg.Wait()

	// Checked by hand rather than through require: 10,000 assertions cost more
	// wall clock than the thing under test.
	seen := make(map[string]struct{}, goroutines*perRoutine)
	for _, batch := range ids {
		for _, id := range batch {
			if _, dup := seen[id]; dup {
				t.Fatalf("UniqueID returned %q twice", id)
			}
			if len(id) > MaxIDLength {
				t.Fatalf("UniqueID returned %q (%d chars), over the handle budget", id, len(id))
			}
			seen[id] = struct{}{}
		}
	}
	assert.Len(t, seen, goroutines*perRoutine)
}

func TestUniqueID_RunPrefixIsStableAndRandom(t *testing.T) {
	assert.Equal(t, RunPrefix(), RunPrefix(), "the prefix identifies the run, so it must not change mid-run")
	assert.Len(t, RunPrefix(), runPrefixLength)
	assertHandleSafe(t, RunPrefix())

	// Two draws from newRunPrefix should differ. This is what makes two
	// consecutive runs against the same persistent PDS not collide — the
	// failure mode a bare counter has.
	distinct := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		distinct[newRunPrefix()] = struct{}{}
	}
	assert.Greater(t, len(distinct), 90, "run prefixes should be drawn at random, not derived from a counter")
}

func TestUniqueIDWithPrefix(t *testing.T) {
	id := UniqueIDWithPrefix(t, "comm")
	assert.True(t, strings.HasPrefix(id, "comm"), "got %q", id)
	assert.LessOrEqual(t, len(id), MaxIDLength)
	assertHandleSafe(t, id)

	t.Run("sanitises the prefix", func(t *testing.T) {
		id := UniqueIDWithPrefix(t, "My Test-Community!")
		assert.True(t, strings.HasPrefix(id, "mytest"), "got %q", id)
		assertHandleSafe(t, id)
	})

	t.Run("clamps an over-long prefix without eating the entropy", func(t *testing.T) {
		id := UniqueIDWithPrefix(t, strings.Repeat("x", 100))
		assert.LessOrEqual(t, len(id), MaxIDLength)
		assertHandleSafe(t, id)
		// The readable prefix is what gets truncated, never the random suffix:
		// a shortened prefix costs a debugging hint, a shortened suffix costs a
		// collision.
		assert.Contains(t, id, RunPrefix(), "the run prefix must survive truncation")
		assert.True(t, strings.HasPrefix(id, "x"), "got %q", id)
	})

	t.Run("keeps a digit-leading prefix legal as a handle label", func(t *testing.T) {
		id := UniqueIDWithPrefix(t, "42")
		assertHandleSafe(t, id)
	})

	t.Run("an empty prefix still yields a usable id", func(t *testing.T) {
		id := UniqueIDWithPrefix(t, "!!!")
		assertHandleSafe(t, id)
		assert.True(t, strings.HasPrefix(id, RunPrefix()))
	})

	t.Run("stays unique", func(t *testing.T) {
		seen := make(map[string]struct{}, 500)
		for i := 0; i < 500; i++ {
			id := UniqueIDWithPrefix(t, "post")
			require.NotContains(t, seen, id)
			seen[id] = struct{}{}
		}
	})
}

// assertHandleSafe checks the invariants a PDS handle label must satisfy:
// lowercase alphanumerics only, and not starting with a digit.
func assertHandleSafe(t *testing.T, id string) {
	t.Helper()
	require.NotEmpty(t, id)
	require.False(t, id[0] >= '0' && id[0] <= '9',
		"%q starts with a digit, which is not a legal handle label", id)
	for i, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		t.Fatalf("%q contains %q at index %d; ids must be lowercase alphanumeric", id, r, i)
	}
}

func TestTestPNG_DecodesAtTheRequestedSize(t *testing.T) {
	data := TestPNG(64, 48)

	img, format, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err, "the bytes must be a real PNG, not a hand-rolled header")
	assert.Equal(t, "png", format)
	assert.Equal(t, 64, img.Bounds().Dx())
	assert.Equal(t, 48, img.Bounds().Dy())
}

func TestTestJPEG_DecodesAtTheRequestedSize(t *testing.T) {
	data := TestJPEG(120, 90)

	img, format, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	assert.Equal(t, 120, img.Bounds().Dx())
	assert.Equal(t, 90, img.Bounds().Dy())
}

func TestTestPNGColor_UsesTheGivenColour(t *testing.T) {
	want := color.RGBA{R: 12, G: 200, B: 90, A: 255}
	img, format, err := image.Decode(bytes.NewReader(TestPNGColor(8, 8, want)))
	require.NoError(t, err)
	require.Equal(t, "png", format)

	r, g, b, a := img.At(3, 5).RGBA()
	assert.Equal(t, [4]uint32{0x0c0c, 0xc8c8, 0x5a5a, 0xffff}, [4]uint32{r, g, b, a})
}

func TestTestJPEGColor_DecodesAtTheRequestedSize(t *testing.T) {
	img, format, err := image.Decode(bytes.NewReader(TestJPEGColor(16, 16, color.White)))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	assert.Equal(t, 16, img.Bounds().Dx())
}

func TestTestImages_AreNotDegenerate(t *testing.T) {
	// A flat image encodes to almost nothing, which makes it a poor stand-in
	// for an upload when the code under test resizes or enforces size limits.
	// The gradient is what buys the difference, so that is what to assert.
	assert.Greater(t, len(TestPNG(200, 200)), len(TestPNGColor(200, 200, color.Black)))
	assert.Greater(t, len(TestJPEG(200, 200)), len(TestJPEGColor(200, 200, color.Black)))
	assert.Greater(t, len(TestPNG(200, 200)), 500)
	assert.Greater(t, len(TestJPEG(200, 200)), 500)
}

func TestTestImages_RejectNonPositiveDimensions(t *testing.T) {
	assert.Panics(t, func() { TestPNG(0, 10) })
	assert.Panics(t, func() { TestJPEG(10, -1) })
}
