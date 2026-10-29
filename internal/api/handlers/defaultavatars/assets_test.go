package defaultavatars

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/webp"
)

// v1Slugs is the v1 default avatar set, sorted ascending.
var v1Slugs = []string{
	"anchor", "beach-bucket-and-spade", "branch-of-coral", "clownfish", "compass",
	"dolphin", "eel", "hermit-crab-in-a-shell", "jellyfish", "kelp-frond",
	"life-ring", "lighthouse", "message-in-a-bottle", "narwhal", "nudibranch-sea-slug",
	"ocean-wave", "octopus", "oyster-with-a-pearl", "pelican", "penguin",
	"pufferfish", "puffin", "sand-dollar", "sandcastle", "scallop-shell",
	"sea-anemone", "sea-otter", "sea-turtle", "sea-urchin", "seahorse",
	"seal", "ships-wheel", "snorkel-mask", "spiral-sea-snail-shell", "squid",
	"starfish", "swordfish", "treasure-chest", "whale",
}

func TestSlugsListsExactlyTheV1Set(t *testing.T) {
	require.Len(t, v1Slugs, 39)
	assert.Equal(t, v1Slugs, Slugs())
}

func TestEveryV1ImageIsATransparent512SquareWebP(t *testing.T) {
	for _, slug := range v1Slugs {
		t.Run(slug, func(t *testing.T) {
			data, ok := Image(slug)
			require.Truef(t, ok, "slug %q has no embedded image", slug)

			decoded, err := webp.Decode(bytes.NewReader(data))
			require.NoErrorf(t, err, "slug %q does not decode as WebP", slug)

			bounds := decoded.Bounds()
			require.Equalf(t, 512, bounds.Dx(), "slug %q width", slug)
			require.Equalf(t, 512, bounds.Dy(), "slug %q height", slug)

			switch decoded.(type) {
			case *image.NRGBA, *image.NYCbCrA:
			default:
				t.Fatalf("slug %q decodes to %T, which has no alpha channel", slug, decoded)
			}

			alphaAt := func(x, y int) uint8 {
				return color.NRGBAModel.Convert(decoded.At(x, y)).(color.NRGBA).A
			}
			corners := [][2]int{
				{bounds.Min.X, bounds.Min.Y},
				{bounds.Max.X - 1, bounds.Min.Y},
				{bounds.Min.X, bounds.Max.Y - 1},
				{bounds.Max.X - 1, bounds.Max.Y - 1},
			}
			for _, corner := range corners {
				assert.Zerof(t, alphaAt(corner[0], corner[1]),
					"slug %q corner (%d,%d) is not transparent", slug, corner[0], corner[1])
			}

			opaque := 0
			for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
				for x := bounds.Min.X; x < bounds.Max.X; x++ {
					if alphaAt(x, y) >= 250 {
						opaque++
					}
				}
			}
			// 1% of 512 x 512 = 2621.44 pixels.
			assert.GreaterOrEqualf(t, opaque*100, 512*512,
				"slug %q has %d opaque pixels, fewer than 1%%", slug, opaque)
		})
	}
}
