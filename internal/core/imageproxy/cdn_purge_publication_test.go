package imageproxy

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/tests/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parkedPublicationCache holds the service's cache write until the test has
// observed the concurrent purge waiting for it.
type parkedPublicationCache struct {
	*DiskCache
	entered          chan struct{}
	release          chan struct{}
	writeHeld        atomic.Bool
	earlyEnumeration chan struct{}
}

func (cache *parkedPublicationCache) Set(preset, owner, cid string, data []byte) error {
	cache.writeHeld.Store(true)
	close(cache.entered)
	<-cache.release
	cache.writeHeld.Store(false)
	return cache.DiskCache.Set(preset, owner, cid, data)
}

func (cache *parkedPublicationCache) OwnerDirectories(cid string) ([]string, error) {
	if cache.writeHeld.Load() {
		select {
		case cache.earlyEnumeration <- struct{}{}:
		default:
		}
	}
	return cache.DiskCache.OwnerDirectories(cid)
}

func TestImageProxyService_CDNPurgeAwaitsInFlightPublicationBeforeNamingOwners(t *testing.T) {
	const ownerB = "did:plc:test123"
	const preset = "avatar"
	const cid = moderationTestCID

	disk, err := NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	cache := &parkedPublicationCache{
		DiskCache:        disk,
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
		earlyEnumeration: make(chan struct{}, 1),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(cache.release) }) }
	t.Cleanup(release)

	var blocked atomic.Bool
	checker := blockCheckFunc(func(context.Context, string, string) (bool, error) {
		return blocked.Load(), nil
	})
	recorder := &observingCDNPurgeRecorder{}
	service, err := NewService(cache, NewMockProcessor([]byte("processed"), nil), NewMockFetcher([]byte("raw"), nil), checker, DefaultConfig(), WithCDNPurgeRecorder(recorder))
	require.NoError(t, err)

	// The writer has already rechecked moderation when it enters Set.
	imageDone := callGetImageAsync(t.Context(), service, cid)
	select {
	case <-cache.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached its cache write")
	}
	blocked.Store(true) // The ownerless block on cid has committed.

	purgeStarted := make(chan struct{})
	purgeDone := make(chan error, 1)
	go func() {
		close(purgeStarted)
		purgeDone <- service.PurgeBlob(t.Context(), cid)
	}()
	<-purgeStarted
	testkit.Holds(t, 200*time.Millisecond, func() (bool, error) {
		select {
		case <-purgeDone:
			return false, nil
		default:
			return true, nil
		}
	}, testkit.WithDescription("ownerless purge waiting for the in-flight cache write"))

	release()
	result := receiveImage(t, imageDone, "request publishing the image")
	require.NoError(t, result.err)
	assert.Equal(t, []byte("processed"), result.data)
	select {
	case err := <-purgeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ownerless purge did not finish after the cache write")
	}
	select {
	case <-cache.earlyEnumeration:
		t.Error("owner directories were enumerated while the cache write was held")
	default:
	}
	assert.Equal(t, [][]BlockedBlob{{{OwnerDID: ownerB, CID: cid}}}, recorder.called)
	_, statErr := os.Stat(disk.cachePath(preset, ownerB, cid))
	assert.True(t, os.IsNotExist(statErr), "owner B's published entry must be gone from disk: %v", statErr)
}
