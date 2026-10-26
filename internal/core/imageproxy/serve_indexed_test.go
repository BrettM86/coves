package imageproxy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type indexedReferencePair struct {
	ownerDID string
	cid      string
}

type indexedReferenceChecker struct {
	active map[indexedReferencePair]struct{}
	calls  atomic.Int32
}

func (checker *indexedReferenceChecker) HasActiveReference(_ context.Context, ownerDID, cid string) (bool, error) {
	checker.calls.Add(1)
	_, found := checker.active[indexedReferencePair{ownerDID: ownerDID, cid: cid}]
	return found, nil
}

func TestImageProxyService_RejectsUnindexedOwnerBlobBeforeResolvingPDS(t *testing.T) {
	const owner = "did:plc:z72i7hdynmk6r22z27h6tvur"
	const otherOwner = "did:plc:ewvi7nxzyoun6zhxrhs64oiz"
	raw := []byte("unindexed image source")
	cid := rawCIDForTest(t, raw)
	for _, test := range []struct {
		name   string
		preset string
		active map[indexedReferencePair]struct{}
	}{
		{name: "avatar", preset: "avatar"},
		{name: "avatar_small", preset: "avatar_small"},
		{name: "banner", preset: "banner"},
		{name: "content_preview", preset: "content_preview"},
		{name: "content_full", preset: "content_full"},
		{name: "embed_thumbnail", preset: "embed_thumbnail"},
		{name: "same CID indexed for another owner", preset: "avatar", active: map[indexedReferencePair]struct{}{{ownerDID: otherOwner, cid: cid}: {}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			references := &indexedReferenceChecker{active: test.active}
			fetcher := NewMockFetcher(raw, nil)
			service, err := NewService(NewMockCache(), NewMockProcessor([]byte("processed"), nil), fetcher, allowAllBlockChecker(), references, DefaultConfig())
			require.NoError(t, err)
			var resolutions atomic.Int32
			data, err := service.GetImageResolvingPDS(t.Context(), test.preset, owner, cid, func(context.Context) (string, error) {
				resolutions.Add(1)
				return "https://pds.example.test", nil
			})
			assert.ErrorIs(t, err, ErrBlobNotIndexed)
			assert.Nil(t, data)
			assert.Zero(t, resolutions.Load(), "unindexed blobs must not resolve their PDS")
			assert.Zero(t, fetcher.Calls(), "unindexed blobs must not be fetched")
		})
	}
}

func TestImageProxyService_ServesIndexedOwnerBlob(t *testing.T) {
	const owner = "did:plc:z72i7hdynmk6r22z27h6tvur"
	raw := []byte("indexed image source")
	cid := rawCIDForTest(t, raw)
	references := &indexedReferenceChecker{active: map[indexedReferencePair]struct{}{{ownerDID: owner, cid: cid}: {}}}
	fetcher := NewMockFetcher(raw, nil)
	service, err := NewService(NewMockCache(), NewMockProcessor([]byte("processed image"), nil), fetcher, allowAllBlockChecker(), references, DefaultConfig())
	require.NoError(t, err)
	data, err := service.GetImageResolvingPDS(t.Context(), "avatar", owner, cid, resolvedPDS("https://pds.example.test"))
	require.NoError(t, err)
	assert.Equal(t, []byte("processed image"), data)
	assert.Equal(t, 1, fetcher.Calls())
}

func TestImageProxyService_RejectsUnindexedWarmCacheEntry(t *testing.T) {
	const owner = "did:plc:z72i7hdynmk6r22z27h6tvur"
	cid := rawCIDForTest(t, []byte("cached image source"))
	for _, test := range []struct {
		name       string
		references ReferenceChecker
		wantError  error
	}{
		{name: "not indexed", references: &indexedReferenceChecker{}, wantError: ErrBlobNotIndexed},
		{name: "reference lookup failed", references: referenceCheckFunc(func(context.Context, string, string) (bool, error) {
			return false, errors.New("reference storage unavailable")
		}), wantError: ErrReferenceCheckFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := NewMockCache()
			cache.SetCacheData("avatar", owner, cid, []byte("previously cached image"))
			service, err := NewService(cache, NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), allowAllBlockChecker(), test.references, DefaultConfig())
			require.NoError(t, err)
			data, err := service.GetImageResolvingPDS(t.Context(), "avatar", owner, cid, resolvedPDS("https://pds.example.test"))
			assert.ErrorIs(t, err, test.wantError)
			assert.Nil(t, data)
			_, found, err := cache.Get("avatar", owner, cid)
			require.NoError(t, err)
			assert.True(t, found, "only a block purges a warm entry")
		})
	}
}

func TestImageProxyService_BlockDecisionPrecedesReferenceLookup(t *testing.T) {
	const owner = "did:plc:z72i7hdynmk6r22z27h6tvur"
	cid := rawCIDForTest(t, []byte("blocked image source"))
	for _, test := range []struct {
		name      string
		block     bool
		blockErr  error
		wantError error
	}{
		{name: "blocked cached blob", block: true, wantError: ErrBlobBlocked},
		{name: "block lookup failed", blockErr: errors.New("block store unavailable"), wantError: ErrBlockCheckFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := NewMockCache()
			cache.SetCacheData("avatar", owner, cid, []byte("cached secret"))
			references := &indexedReferenceChecker{}
			blocks := blockCheckFunc(func(context.Context, string, string) (bool, error) {
				return test.block, test.blockErr
			})
			service, err := NewService(cache, NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), blocks, references, DefaultConfig())
			require.NoError(t, err)
			data, err := service.GetImageResolvingPDS(t.Context(), "avatar", owner, cid, resolvedPDS("https://pds.example.test"))
			assert.ErrorIs(t, err, test.wantError)
			assert.NotErrorIs(t, err, ErrBlobNotIndexed)
			assert.Nil(t, data)
			assert.Zero(t, references.calls.Load(), "moderation must decide before reference lookup")
			_, found, err := cache.Get("avatar", owner, cid)
			require.NoError(t, err)
			if test.block {
				assert.False(t, found, "blocked cached entry must be purged")
			} else {
				assert.True(t, found, "a failed block lookup must not purge the cached entry")
			}
		})
	}
}

func TestNewService_RejectsNilReferenceChecker(t *testing.T) {
	service, err := NewService(NewMockCache(), NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), allowAllBlockChecker(), nil, DefaultConfig())
	assert.ErrorIs(t, err, ErrNilDependency)
	if service != nil {
		t.Error("expected nil service")
	}
}
