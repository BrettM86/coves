package imageproxy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/tests/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	moderationTestOwner = "did:plc:moderationimageowner"
	moderationTestCID   = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
)

func TestImageProxyService_BlocksBeforeReadingCache(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			cache := NewMockCache()
			if warm {
				cache.SetCacheData("avatar", moderationTestOwner, moderationTestCID, []byte("cached secret"))
			}
			fetcher := NewMockFetcher([]byte("fetched secret"), nil)
			var checks atomic.Int32
			checker := blockCheckFunc(func(_ context.Context, did, cid string) (bool, error) {
				assert.Equal(t, moderationTestOwner, did)
				assert.Equal(t, moderationTestCID, cid)
				checks.Add(1)
				return true, nil
			})
			service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), fetcher, checker, DefaultConfig())
			require.NoError(t, err)
			data, err := service.GetImageResolvingPDS(t.Context(), "avatar", moderationTestOwner, moderationTestCID, resolvedPDS("https://pds.example.com"))
			assert.ErrorIs(t, err, ErrBlobBlocked)
			assert.Empty(t, data)
			assert.EqualValues(t, 1, checks.Load())
			assert.Zero(t, cache.GetCalls(), "even a warm entry must not be read")
			assert.Zero(t, fetcher.Calls())
		})
	}
}

// The handler refuses these first; the service refuses them too, so a second
// caller cannot miss an owner-scoped block or read another spelling's cache.
func TestImageProxyService_RefusesNoncanonicalOwnerDID(t *testing.T) {
	for _, test := range []struct{ name, owner string }{
		{name: "uppercase plc identifier", owner: "did:plc:Moderationimageowner"},
		{name: "path-based web DID sharing a cache directory", owner: "did:web:example.test:a:b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := NewMockCache()
			cache.SetCacheData("avatar", test.owner, moderationTestCID, []byte("cached secret"))
			fetcher := NewMockFetcher([]byte("fetched secret"), nil)
			var checks, resolutions atomic.Int32
			checker := blockCheckFunc(func(context.Context, string, string) (bool, error) {
				checks.Add(1)
				return false, nil
			})
			service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), fetcher, checker, DefaultConfig())
			require.NoError(t, err)

			data, err := service.GetImageResolvingPDS(t.Context(), "avatar", test.owner, moderationTestCID, func(context.Context) (string, error) {
				resolutions.Add(1)
				return "https://pds.example.com", nil
			})
			assert.ErrorIs(t, err, ErrInvalidDID)
			assert.Empty(t, data)
			blocked, err := service.IsBlobBlocked(t.Context(), test.owner, moderationTestCID)
			assert.ErrorIs(t, err, ErrInvalidDID)
			assert.False(t, blocked)

			assert.Zero(t, checks.Load(), "no block lookup for a noncanonical owner")
			assert.Zero(t, cache.GetCalls(), "a warm entry under a noncanonical owner must not be read")
			assert.Zero(t, resolutions.Load(), "no PDS resolution for a noncanonical owner")
			assert.Zero(t, fetcher.Calls())
		})
	}
}

func TestImageProxyService_BlockCheckFailureFailsClosed(t *testing.T) {
	cache := NewMockCache()
	cache.SetCacheData("avatar", moderationTestOwner, moderationTestCID, []byte("cached secret"))
	fetcher := NewMockFetcher([]byte("fetched secret"), nil)
	lookupError := errors.New("block lookup unavailable")
	checker := blockCheckFunc(func(context.Context, string, string) (bool, error) { return false, lookupError })
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), fetcher, checker, DefaultConfig())
	require.NoError(t, err)
	data, err := service.GetImageResolvingPDS(t.Context(), "avatar", moderationTestOwner, moderationTestCID, resolvedPDS("https://pds.example.com"))
	assert.ErrorIs(t, err, ErrBlockCheckFailed)
	assert.Empty(t, data)
	assert.Zero(t, cache.GetCalls())
	assert.Zero(t, fetcher.Calls())
}

func TestImageProxyService_BlockArrivesWhileFetching(t *testing.T) {
	cache := NewMockCache()
	fetcher := newBlockingFetcher([]byte("source image"))
	t.Cleanup(fetcher.Release)
	var blocked atomic.Bool
	var checks atomic.Int32
	checker := blockCheckFunc(func(context.Context, string, string) (bool, error) {
		checks.Add(1)
		return blocked.Load(), nil
	})
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), fetcher, checker, DefaultConfig())
	require.NoError(t, err)
	done := callGetImageAsync(t.Context(), service, moderationTestCID)
	fetcher.waitForEntries(t, 1, 5*time.Second)
	require.EqualValues(t, 1, checks.Load(), "pre-read check must finish before fetching")
	blocked.Store(true)
	fetcher.Release()
	select {
	case result := <-done:
		assert.ErrorIs(t, result.err, ErrBlobBlocked)
		assert.Empty(t, result.data)
	case <-time.After(5 * time.Second):
		t.Fatal("GetImage did not finish after the fetch was released")
	}
	assert.GreaterOrEqual(t, checks.Load(), int32(2), "the post-fetch check must see the new block")
	assert.Zero(t, cache.SetCalls(), "a blocked in-flight image must never be cached")
}

func TestImageProxyService_PurgeSerializesWithCachePublication(t *testing.T) {
	cache := NewMockCache()
	var checks atomic.Int32
	checkingAfterFetch := make(chan struct{})
	releaseCheck := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCheck) }) }
	defer release()
	checker := blockCheckFunc(func(context.Context, string, string) (bool, error) {
		if checks.Add(1) == 2 {
			close(checkingAfterFetch)
			<-releaseCheck
		}
		return false, nil
	})
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), NewMockFetcher([]byte("raw"), nil), checker, DefaultConfig())
	require.NoError(t, err)
	imageDone := callGetImageAsync(t.Context(), service, moderationTestCID)
	select {
	case <-checkingAfterFetch:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never reached the post-fetch block check")
	}
	purgeStarted := make(chan struct{})
	purgeDone := make(chan error, 1)
	go func() {
		close(purgeStarted)
		purgeDone <- service.PurgeOwnerBlob(t.Context(), "did:plc:test123", moderationTestCID)
	}()
	<-purgeStarted
	testkit.Holds(t, 200*time.Millisecond, func() (bool, error) {
		select {
		case <-purgeDone:
			return false, nil
		default:
			return true, nil
		}
	}, testkit.WithDescription("purge waiting for the in-flight cache publication"))
	release()
	select {
	case result := <-imageDone:
		require.NoError(t, result.err)
		assert.Equal(t, []byte("processed"), result.data)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not finish after releasing the block check")
	}
	select {
	case err := <-purgeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("purge did not finish after the writer")
	}
	assert.Equal(t, 1, cache.SetCalls(), "the writer must have cached bytes before purge completed")
	data, found, err := cache.Get("avatar", "did:plc:test123", moderationTestCID)
	require.NoError(t, err)
	assert.False(t, found, "purge must remove the writer's cache entry across the race")
	assert.Empty(t, data)
}

func TestImageProxyService_PurgeBlobClearsAllOwners(t *testing.T) {
	cache := NewMockCache()
	cache.SetCacheData("avatar", moderationTestOwner, moderationTestCID, []byte("first"))
	cache.SetCacheData("banner", "did:plc:otherowner", moderationTestCID, []byte("second"))
	cache.SetCacheData("avatar", moderationTestOwner, "bafyreiunrelatedimage", []byte("unrelated"))
	service, err := NewService(cache, NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), allowAllBlockChecker(), DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, service.PurgeBlob(t.Context(), moderationTestCID))
	for _, entry := range []struct{ preset, did string }{{"avatar", moderationTestOwner}, {"banner", "did:plc:otherowner"}} {
		_, found, err := cache.Get(entry.preset, entry.did, moderationTestCID)
		require.NoError(t, err)
		assert.False(t, found)
	}
	_, found, err := cache.Get("avatar", moderationTestOwner, "bafyreiunrelatedimage")
	require.NoError(t, err)
	assert.True(t, found)
}

func TestImageProxyService_IsBlobBlockedUsesChecker(t *testing.T) {
	checker := blockCheckFunc(func(_ context.Context, did, cid string) (bool, error) {
		assert.Equal(t, moderationTestOwner, did)
		assert.Equal(t, moderationTestCID, cid)
		return true, nil
	})
	service, err := NewService(NewMockCache(), NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), checker, DefaultConfig())
	require.NoError(t, err)
	blocked, err := service.IsBlobBlocked(t.Context(), moderationTestOwner, moderationTestCID)
	require.NoError(t, err)
	assert.True(t, blocked)
}

// cidOnOtherStripe returns a CID whose publication lock differs from cid's,
// so a test can show that work on one stripe does not stall the other.
func cidOnOtherStripe(t *testing.T, service *ImageProxyService, cid string) string {
	t.Helper()
	for _, suffix := range "abcdefghijklmnopqrstuvwxyz" {
		candidate := cid[:len(cid)-1] + string(suffix)
		if service.publicationLock(candidate) != service.publicationLock(cid) {
			return candidate
		}
	}
	t.Fatal("no candidate CID landed on another publication stripe")
	return ""
}

// parkingPurgeCache parks DeleteCID until released, standing in for a purge
// whose directory walk is slow.
type parkingPurgeCache struct {
	*MockCache
	entered chan struct{}
	release chan struct{}
}

func (c *parkingPurgeCache) DeleteCID(cid string) error {
	close(c.entered)
	<-c.release
	return c.MockCache.DeleteCID(cid)
}

func receiveImage(t *testing.T, done <-chan getImageResult, what string) getImageResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", what)
		return getImageResult{}
	}
}

func TestImageProxyService_BlockedRequestPurgesItsCacheEntry(t *testing.T) {
	cache := NewMockCache()
	cache.SetCacheData("avatar", moderationTestOwner, moderationTestCID, []byte("cached secret"))
	cache.SetCacheData("avatar", "did:plc:otherowner", moderationTestCID, []byte("other owner"))
	checker := blockCheckFunc(func(_ context.Context, did, _ string) (bool, error) {
		return did == moderationTestOwner, nil
	})
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), NewMockFetcher(nil, nil), checker, DefaultConfig())
	require.NoError(t, err)

	_, err = service.GetImageResolvingPDS(t.Context(), "avatar", moderationTestOwner, moderationTestCID, resolvedPDS("https://pds.example.com"))
	require.ErrorIs(t, err, ErrBlobBlocked)
	assert.Zero(t, cache.GetCalls(), "the blocked entry must never be read")
	_, found, err := cache.Get("avatar", moderationTestOwner, moderationTestCID)
	require.NoError(t, err)
	assert.False(t, found, "a blocked request must remove the cached bytes a lost purge left behind")
	_, found, err = cache.Get("avatar", "did:plc:otherowner", moderationTestCID)
	require.NoError(t, err)
	assert.True(t, found, "another owner's unblocked entry must stay")
}

func TestImageProxyService_PurgeDiskWorkDoesNotHoldPublicationLock(t *testing.T) {
	cache := &parkingPurgeCache{MockCache: NewMockCache(), entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releasePurge := func() { releaseOnce.Do(func() { close(cache.release) }) }
	t.Cleanup(releasePurge)
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), NewMockFetcher([]byte("raw"), nil), allowAllBlockChecker(), DefaultConfig())
	require.NoError(t, err)

	purgeDone := make(chan error, 1)
	go func() { purgeDone <- service.PurgeBlob(t.Context(), moderationTestCID) }()
	select {
	case <-cache.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("purge never reached the cache walk")
	}
	result := receiveImage(t, callGetImageAsync(t.Context(), service, moderationTestCID),
		"cache publication on the purged CID's stripe while the purge walks the cache")
	require.NoError(t, result.err)
	releasePurge()
	select {
	case err := <-purgeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("purge did not finish after release")
	}
}

func TestImageProxyService_ProcessingSlotFreeDuringPublication(t *testing.T) {
	parkedCheck := make(chan struct{})
	releaseCheck := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCheck) }) }
	t.Cleanup(release)
	var parkedCID string
	var checksForParked atomic.Int32
	checker := blockCheckFunc(func(_ context.Context, _, cid string) (bool, error) {
		if cid == parkedCID && checksForParked.Add(1) == 2 {
			close(parkedCheck)
			<-releaseCheck
		}
		return false, nil
	})
	cache := NewMockCache()
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), NewMockFetcher([]byte("raw"), nil), checker, semaphoreTestConfig(1))
	require.NoError(t, err)
	parkedCID = moderationTestCID
	otherCID := cidOnOtherStripe(t, service, parkedCID)

	parkedDone := callGetImageAsync(t.Context(), service, parkedCID)
	select {
	case <-parkedCheck:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached its post-fetch block check")
	}
	result := receiveImage(t, callGetImageAsync(t.Context(), service, otherCID), "request on another stripe")
	require.NoError(t, result.err, "a request parked in publication must not hold the only processing slot")
	release()
	require.NoError(t, receiveImage(t, parkedDone, "parked request").err)
}

// TestImageProxyService_AdmissionHeldDuringPublication: a processed output
// waiting for publication is transient memory the in-flight cap must still
// count, or a slow recheck lets outputs pile up beyond the documented bound.
func TestImageProxyService_AdmissionHeldDuringPublication(t *testing.T) {
	parkedCheck := make(chan struct{})
	releaseCheck := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCheck) }) }
	t.Cleanup(release)
	var checksForParked atomic.Int32
	checker := blockCheckFunc(func(_ context.Context, _, cid string) (bool, error) {
		if cid == moderationTestCID && checksForParked.Add(1) == 2 {
			close(parkedCheck)
			<-releaseCheck
		}
		return false, nil
	})
	fetcher := NewMockFetcher([]byte("raw"), nil)
	service, err := NewService(NewMockCache(), NewMockProcessor([]byte("processed"), nil), fetcher, checker, admissionTestConfig(1))
	require.NoError(t, err)
	otherCID := cidOnOtherStripe(t, service, moderationTestCID)

	parkedDone := callGetImageAsync(t.Context(), service, moderationTestCID)
	select {
	case <-parkedCheck:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached its post-fetch block check")
	}
	refusalsBefore := ProcessorBusyRefusalCount()
	result := receiveImage(t, callGetImageAsync(t.Context(), service, otherCID), "request over the in-flight cap")
	require.ErrorIs(t, result.err, ErrProcessorBusy, "a request parked in publication must still hold its admission slot")
	assert.Equal(t, int64(1), ProcessorBusyRefusalCount()-refusalsBefore)
	assert.Equal(t, 1, fetcher.Calls(), "the refused request must never fetch")

	release()
	require.NoError(t, receiveImage(t, parkedDone, "parked request").err)
	result = receiveImage(t, callGetImageAsync(t.Context(), service, otherCID), "request after publication finished")
	require.NoError(t, result.err, "publication must hand the admission slot back when it finishes")
}

func TestImageProxyService_CallerDisconnectAfterProcessingStillCaches(t *testing.T) {
	cache := NewMockCache()
	processor := newBlockingProcessor([]byte("processed"))
	t.Cleanup(processor.Release)
	checker := blockCheckFunc(func(ctx context.Context, _, _ string) (bool, error) {
		return false, ctx.Err()
	})
	service, err := NewService(cache, processor, NewMockFetcher([]byte("raw"), nil), checker, DefaultConfig())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := callGetImageAsync(ctx, service, moderationTestCID)
	processor.waitForEntries(t, 1, 5*time.Second)
	cancel()
	processor.Release()
	receiveImage(t, done, "request whose caller disconnected during processing")
	data, found := cache.GetSetData("avatar", "did:plc:test123", moderationTestCID)
	assert.True(t, found, "processed bytes must be cached even though the caller went away")
	assert.Equal(t, []byte("processed"), data)
}

func TestImageProxyService_PurgeReturnsWhileFetchIsParked(t *testing.T) {
	cache := NewMockCache()
	fetcher := newBlockingFetcher([]byte("raw"))
	t.Cleanup(fetcher.Release)
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), fetcher, allowAllBlockChecker(), DefaultConfig())
	require.NoError(t, err)
	done := callGetImageAsync(t.Context(), service, moderationTestCID)
	fetcher.waitForEntries(t, 1, 5*time.Second)

	purged := make(chan error, 2)
	go func() {
		purged <- service.PurgeBlob(t.Context(), moderationTestCID)
		purged <- service.PurgeOwnerBlob(t.Context(), "did:plc:test123", moderationTestCID)
	}()
	for range 2 {
		select {
		case err := <-purged:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("purge waited on a fetch that holds no publication lock")
		}
	}
	fetcher.Release()
	require.NoError(t, receiveImage(t, done, "released fetch").err)
}

func TestImageProxyService_PublicationWaiterReleasedWhenContextEnds(t *testing.T) {
	cache := NewMockCache()
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), NewMockFetcher([]byte("raw"), nil), allowAllBlockChecker(), DefaultConfig())
	require.NoError(t, err)
	release, err := service.lockPublication(t.Context(), moderationTestCID)
	require.NoError(t, err)
	t.Cleanup(release)

	t.Run("cancelled waiter", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		waiterDone := make(chan error, 1)
		go func() {
			_, err := service.lockPublication(ctx, moderationTestCID)
			waiterDone <- err
		}()
		cancel()
		select {
		case err := <-waiterDone:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("a waiter on the publication stripe ignored its cancelled context")
		}
	})

	t.Run("publication gives up at its bound and fails closed", func(t *testing.T) {
		service.publicationTimeout = 20 * time.Millisecond
		result := receiveImage(t, callGetImageAsync(t.Context(), service, moderationTestCID), "publication waiting on a held stripe")
		assert.ErrorIs(t, result.err, ErrBlockCheckFailed)
		assert.ErrorIs(t, result.err, context.DeadlineExceeded)
		assert.Empty(t, result.data, "bytes that were never rechecked must not be served")
		assert.Zero(t, cache.SetCalls())
	})
}

type blockedBlobListerFunc func(context.Context) ([]BlockedBlob, error)

func (f blockedBlobListerFunc) ListActiveBlockedBlobs(ctx context.Context) ([]BlockedBlob, error) {
	return f(ctx)
}

func TestImageProxyService_PurgeActiveBlocksRemovesBlockedEntries(t *testing.T) {
	const (
		ownerA       = "did:plc:sweepownera"
		ownerB       = "did:plc:sweepownerb"
		ownerBlocked = "bafyreiownerblocked"
		everyOwner   = "bafyreieveryownerblocked"
		unrelated    = "bafyreiunrelated"
	)
	cache, err := NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	type entry struct{ preset, did, cid string }
	removed := []entry{{"avatar", ownerA, ownerBlocked}, {"banner", ownerA, ownerBlocked}, {"avatar", ownerA, everyOwner}, {"banner", ownerB, everyOwner}}
	kept := []entry{{"avatar", ownerB, ownerBlocked}, {"avatar", ownerA, unrelated}}
	for _, e := range append(append([]entry{}, removed...), kept...) {
		require.NoError(t, cache.Set(e.preset, e.did, e.cid, []byte("image")))
	}
	lister := blockedBlobListerFunc(func(context.Context) ([]BlockedBlob, error) {
		return []BlockedBlob{{OwnerDID: ownerA, CID: ownerBlocked}, {CID: everyOwner}, {OwnerDID: ownerB, CID: everyOwner}}, nil
	})
	service, err := NewService(cache, NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), allowAllBlockChecker(), DefaultConfig())
	require.NoError(t, err)

	require.NoError(t, service.PurgeActiveBlocks(t.Context(), lister))
	for _, e := range removed {
		_, found, err := cache.Get(e.preset, e.did, e.cid)
		require.NoError(t, err)
		assert.False(t, found, "blocked entry %v must be purged", e)
	}
	for _, e := range kept {
		_, found, err := cache.Get(e.preset, e.did, e.cid)
		require.NoError(t, err)
		assert.True(t, found, "unblocked entry %v must stay", e)
	}

	listError := errors.New("block list unavailable")
	err = service.PurgeActiveBlocks(t.Context(), blockedBlobListerFunc(func(context.Context) ([]BlockedBlob, error) { return nil, listError }))
	assert.ErrorIs(t, err, listError)
}
