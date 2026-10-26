package imageproxy

import (
	"bytes"
	"errors"
	"testing"

	cidlib "github.com/ipfs/go-cid"
	multihash "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type processorFunc func([]byte, Preset) ([]byte, error)

func (f processorFunc) Process(data []byte, preset Preset) ([]byte, error) {
	return f(data, preset)
}

func TestImageProxyService_GIFFrameBudgetAndCache(t *testing.T) {
	for _, test := range []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{"two real frames", animatedGIF(t, 1000, 1000, 2), ErrImageTooManyPixels},
		{"two framed undecodable images", framedJunkGIF(1000, 1000, 2), ErrImageTooManyPixels},
		{"one frame at budget", animatedGIF(t, 1000, 1000, 1), nil},
	} {
		for _, preset := range []string{"content_preview", "content_full", "avatar"} {
			t.Run(test.name+"/"+preset, func(t *testing.T) {
				cache, err := NewDiskCache(t.TempDir(), 1, 0)
				require.NoError(t, err)
				service := mustNewService(t, cache, newProcessorWithBudget(t, 1), NewMockFetcher(test.data, nil), DefaultConfig())
				cid := rawCIDForTest(t, test.data)
				const did = "did:plc:test123"
				output, err := service.GetImageResolvingPDS(t.Context(), preset, did, cid, resolvedPDS("https://pds.example.com"))
				if test.wantErr != nil {
					require.True(t, errors.Is(err, test.wantErr), "got %v, want %v", err, test.wantErr)
					assert.Empty(t, output)
					cached, found, cacheErr := cache.Get(preset, did, cid)
					require.NoError(t, cacheErr)
					assert.False(t, found)
					assert.Empty(t, cached)
					return
				}
				require.NoError(t, err)
				assert.NotEmpty(t, output)
			})
		}
	}
}

func TestImageProxyService_RejectsUnverifiedFetchedBytes(t *testing.T) {
	data := literalGreenPNG(t)
	sha256Hash, err := multihash.Sum(data, multihash.SHA2_256, -1)
	require.NoError(t, err)
	blake2bHash, err := multihash.Sum(data, multihash.BLAKE2B_MIN+31, -1)
	require.NoError(t, err)
	computed := rawCIDForTest(t, data)
	for _, test := range []struct{ name, cid, reason string }{
		{"different raw digest", rawCIDForTest(t, []byte("different bytes")), "digest_mismatch"},
		{"dag-cbor codec with matching digest", "bafyreigeuexa7agavc2emh3jirquiqqub3idxyr2tjvxotlaorm62ao44a", "unsupported_codec"},
		{"dag-pb codec with matching digest", cidlib.NewCidV1(cidlib.DagProtobuf, sha256Hash).String(), "unsupported_codec"},
		{"CIDv0 with matching digest", cidlib.NewCidV0(sha256Hash).String(), "unsupported_version"},
		{"raw SHA2-512 with matching bytes", "bafkrgqcdlqyg3dbub2wedubcrj4zgk5vslxiyha52bwkrmg3y3uzr7mdmkgbgpdkmozpbldqlgrvzqytorm7xip7z4ivpb6gpa673gmivr4cy", "unsupported_hash"},
		{"raw BLAKE2b-256 with matching bytes", cidlib.NewCidV1(cidlib.Raw, blake2bHash).String(), "unsupported_hash"},
		// The service runs no CID validation before the fetch (the handler
		// canonicalizes first), so a CID that does not decode reaches the check.
		{"undecodable CID", "notacidatall", "undecodable_cid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache, err := NewDiskCache(t.TempDir(), 1, 0)
			require.NoError(t, err)
			fetcher := NewMockFetcher(data, nil)
			processor := processorFunc(func([]byte, Preset) ([]byte, error) {
				t.Errorf("unverified bytes reached processor")
				return nil, nil
			})
			service := mustNewService(t, cache, processor, fetcher, DefaultConfig())
			const did = "did:plc:test123"
			for request := 1; request <= 2; request++ {
				output, err := service.GetImageResolvingPDS(t.Context(), "content_preview", did, test.cid, resolvedPDS("https://pds.example.com"))
				require.Error(t, err, "request %d", request)
				assert.True(t, errors.Is(err, ErrCIDMismatch), "request %d: got %v", request, err)
				assert.Contains(t, err.Error(), test.reason, "request %d", request)
				assert.Contains(t, err.Error(), "(computed "+computed+")", "request %d", request)
				assert.Empty(t, output)
				_, found, cacheErr := cache.Get("content_preview", did, test.cid)
				require.NoError(t, cacheErr)
				assert.False(t, found)
			}
			assert.Equal(t, 2, fetcher.Calls())
		})
	}
}

func TestImageProxyService_AcceptsLiteralRawSHA256CID(t *testing.T) {
	data := literalGreenPNG(t)
	cache, err := NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	service := mustNewService(t, cache, newTestProcessor(t), NewMockFetcher(data, nil), DefaultConfig())
	output, err := service.GetImageResolvingPDS(t.Context(), "content_preview", "did:plc:test123", "bafkreigeuexa7agavc2emh3jirquiqqub3idxyr2tjvxotlaorm62ao44a", resolvedPDS("https://pds.example.com"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(output, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}))
}
