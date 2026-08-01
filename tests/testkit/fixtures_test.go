package testkit

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"net"
	"strconv"
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

func TestSyntheticClientIP_IsAValidDocumentationAddress(t *testing.T) {
	ip := net.ParseIP(SyntheticClientIP("contract/post"))
	require.NotNil(t, ip, "the value goes into X-Real-IP; it must be a real address")
	assert.Nil(t, ip.To4(), "IPv6, so the 64 hash bits fit without being compressed into an octet")

	// 2001:db8::/32 is RFC 3849's documentation range: reserved, unroutable,
	// and therefore incapable of being mistaken for a real client.
	_, documentation, err := net.ParseCIDR("2001:db8::/32")
	require.NoError(t, err)
	assert.True(t, documentation.Contains(ip), "got %s, which is outside 2001:db8::/32", ip)
}

func TestSyntheticClientIP_IsStablePerLabelAndDistinctBetweenLabels(t *testing.T) {
	assert.Equal(t, SyntheticClientIP("a"), SyntheticClientIP("a"),
		"a caller must land in the same bucket every time, or its own quota is unpredictable")
	assert.NotEqual(t, SyntheticClientIP("a"), SyntheticClientIP("b"),
		"two labels sharing a bucket is the collision this exists to prevent")

	// Length-delimited rather than concatenated: without the delimiter these
	// two would hash identically and share a rate-limit bucket.
	assert.NotEqual(t, SyntheticClientIP("ab"), SyntheticClientIP("a")+"b")
	seen := make(map[string]string, 256)
	for i := range 256 {
		label := "contract/" + strconv.Itoa(i)
		ip := SyntheticClientIP(label)
		if previous, clash := seen[ip]; clash {
			t.Fatalf("%s and %s hash to the same bucket (%s)", previous, label, ip)
		}
		seen[ip] = label
	}
}

func TestSyntheticClientIP_CarriesTheRunPrefix(t *testing.T) {
	// The run-scoping is what makes a kept stack re-runnable: the AppView's
	// rate-limit buckets outlive the test binary, so a second run must not
	// inherit the first run's spent quota.
	// %016x, not %x: the address zero-pads every hextet, so an unpadded hash
	// whose top nibble happens to be zero would not appear in it — a test that
	// fails one run in sixteen for a reason that has nothing to do with the code.
	hashed := func(prefix, label string) string {
		h := fnv.New64a()
		_, _ = fmt.Fprintf(h, "%s\x00%s", prefix, label)
		return fmt.Sprintf("%016x", h.Sum64())
	}
	assert.NotEqual(t, hashed(RunPrefix(), "x"), hashed("other-run", "x"),
		"a different run must produce a different bucket for the same label")

	// Exact rather than a substring match: the four hextets after the 2001:db8
	// prefix ARE the 64-bit hash, so this pins the derivation itself. A
	// "contains the first few hex digits" check passed or failed depending on
	// whether that run's hash happened to start with a zero nibble.
	groups := strings.Split(strings.TrimSuffix(SyntheticClientIP("x"), "::1"), ":")
	require.Len(t, groups, 6, "expected 2001:db8 plus four hash hextets")
	assert.Equal(t, []string{"2001", "db8"}, groups[:2])
	assert.Equal(t, hashed(RunPrefix(), "x"), strings.Join(groups[2:], ""),
		"the address must actually be derived from RunPrefix, not merely documented as such")
}
