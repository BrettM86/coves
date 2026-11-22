package classification_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"sync"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/classification"
	"Coves/internal/core/imageproxy"

	cidlib "github.com/ipfs/go-cid"
	multihash "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "golang.org/x/image/webp" // registers the WebP decoder for the fetched preview
)

// recordingPDSFetcher is an imageproxy.Fetcher that serves one blob and
// records the PDS URL, DID and CID it was asked for.
type recordingPDSFetcher struct {
	data []byte

	mu   sync.Mutex
	asks []string
}

func (f *recordingPDSFetcher) Fetch(_ context.Context, pdsURL, did, cid string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks = append(f.asks, pdsURL+" "+did+" "+cid)
	return f.data, nil
}

// allowAllMedia is a block and reference checker that serves every blob.
type allowAllMedia struct{}

func (allowAllMedia) IsBlocked(context.Context, string, string) (bool, error) { return false, nil }

func (allowAllMedia) HasActiveReference(context.Context, string, string) (bool, error) {
	return true, nil
}

// ownerPDSResolver is an identity.Resolver that puts every DID on one PDS and
// records the DIDs it was asked to resolve.
type ownerPDSResolver struct {
	pdsURL string

	mu   sync.Mutex
	dids []string
}

func (r *ownerPDSResolver) asked(did string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dids = append(r.dids, did)
}

func (r *ownerPDSResolver) ResolveDID(_ context.Context, did string) (*identity.DIDDocument, error) {
	r.asked(did)
	return &identity.DIDDocument{DID: did, Service: []identity.Service{{
		ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: r.pdsURL,
	}}}, nil
}

func (r *ownerPDSResolver) Resolve(_ context.Context, identifier string) (*identity.Identity, error) {
	r.asked(identifier)
	return &identity.Identity{DID: identifier, PDSURL: r.pdsURL}, nil
}

func (r *ownerPDSResolver) ResolveHandle(context.Context, string) (string, string, error) {
	return "", "", errors.New("ownerPDSResolver: handles are not part of the image path")
}

func (r *ownerPDSResolver) Purge(context.Context, string) error { return nil }

// twoFrameGIF is a 1600x100 animation whose first frame is red and second
// blue.
func twoFrameGIF(t *testing.T) []byte {
	t.Helper()
	animation := &gif.GIF{Config: image.Config{Width: 1600, Height: 100}}
	for _, shade := range []color.RGBA{{R: 255, A: 255}, {B: 255, A: 255}} {
		frame := image.NewPaletted(image.Rect(0, 0, 1600, 100), color.Palette{color.RGBA{A: 255}, shade})
		for index := range frame.Pix {
			frame.Pix[index] = 1
		}
		animation.Image = append(animation.Image, frame)
		animation.Delay = append(animation.Delay, 10)
	}
	var encoded bytes.Buffer
	require.NoError(t, gif.EncodeAll(&encoded, animation))
	return encoded.Bytes()
}

// rawCID is the raw sha2-256 CIDv1 of data, as a PDS names a blob.
func rawCID(t *testing.T, data []byte) string {
	t.Helper()
	hash, err := multihash.Sum(data, multihash.SHA2_256, -1)
	require.NoError(t, err)
	return cidlib.NewCidV1(cidlib.Raw, hash).String()
}

// Images are read through the image proxy at the content_preview size, from
// the owner's PDS as the configured resolver finds it, and an animated GIF
// is sent as its first frame only (R4).
func TestProxyImageFetcher_SendsFirstFrameOfAnimatedGIF(t *testing.T) {
	const owner = "did:plc:galleryowner"
	data := twoFrameGIF(t)
	cid := rawCID(t, data)

	cache, err := imageproxy.NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	pds := &recordingPDSFetcher{data: data}
	service, err := imageproxy.NewService(cache, processor, pds, allowAllMedia{}, allowAllMedia{}, imageproxy.DefaultConfig())
	require.NoError(t, err)
	resolver := &ownerPDSResolver{pdsURL: "https://pds.galleryowner.test"}

	fetched, err := classification.NewProxyImageFetcher(service, resolver).
		FetchImage(context.Background(), classification.ImageRef{OwnerDID: owner, CID: cid})
	require.NoError(t, err)

	assert.Equal(t, "image/webp", fetched.ContentType, "the image proxy renders every preview as WebP")
	assert.Equal(t, []string{owner}, resolver.dids, "the owner's PDS must be found through the configured resolver")
	assert.Equal(t, []string{"https://pds.galleryowner.test " + owner + " " + cid}, pds.asks,
		"the blob must be fetched from the PDS the resolver gave")
	decoded, format, err := image.Decode(bytes.NewReader(fetched.Data))
	require.NoError(t, err, "the fetched image does not decode")
	assert.Equal(t, "webp", format)
	assert.Equal(t, image.Rect(0, 0, 800, 50), decoded.Bounds(), "content_preview is 800 wide")
	red, green, blue, _ := decoded.At(400, 25).RGBA()
	assert.Greater(t, red>>8, uint32(200), "centre pixel is not the first frame's red")
	assert.Less(t, green>>8, uint32(60), "centre pixel is not the first frame's red")
	assert.Less(t, blue>>8, uint32(60), "centre pixel is the second frame's blue, not the first frame's red")
}
