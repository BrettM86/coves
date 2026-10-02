package imageproxy

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnerDIDFromCacheDirectory(t *testing.T) {
	for _, test := range []struct {
		name, directory, owner string
		mapped                 bool
	}{
		{"plc", "did_plc_ewvi7nxzyoun6zhxrhs64oiz", "did:plc:ewvi7nxzyoun6zhxrhs64oiz", true},
		{"web", "did_web_example.test", "did:web:example.test", true},
		{"web port", "did_web_localhost%3A2583", "did:web:localhost%3A2583", true},
		{"web path", "did_web_example.test_alice", "", false},
		{"web underscore", "did_web_under_score.test", "", false},
		{"uppercase plc", "did_plc_UPPERCASE1", "", false},
		{"key method", "did_key_z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK", "", false},
		{"empty plc", "did_plc_", "", false},
		{"empty web", "did_web_", "", false},
		{"colon form", "did:plc:abc", "", false},
		{"empty", "", "", false},
		{"junk", "junk", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner, mapped := ownerDIDFromCacheDirectory(test.directory)
			assert.Equal(t, test.mapped, mapped)
			assert.Equal(t, test.owner, owner)
		})
	}
}

type observingCDNPurgeRecorder struct {
	called [][]BlockedBlob
	check  func()
	err    error
}

func (recorder *observingCDNPurgeRecorder) RecordCDNPurgeTargets(_ context.Context, blobs []BlockedBlob) error {
	if recorder.check != nil {
		recorder.check()
	}
	recorder.called = append(recorder.called, append([]BlockedBlob(nil), blobs...))
	return recorder.err
}

type failingOwnerDirectoryCache struct {
	*DiskCache
	err error
}

func (cache *failingOwnerDirectoryCache) OwnerDirectories(string) ([]string, error) {
	return nil, cache.err
}

func TestImageProxyService_CDNRecordsBlockedRequestsBeforeDeletion(t *testing.T) {
	const owner = "did:plc:ewvi7nxzyoun6zhxrhs64oiz"
	const cid = moderationTestCID
	const preset = "content_preview"
	for _, test := range []struct {
		name, seeded string
		recordError  bool
	}{
		{"cached blocked image", "yes", false},
		{"recording failure", "yes", true},
		{"uncached blocked image", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache, err := NewDiskCache(t.TempDir(), 1, 0)
			require.NoError(t, err)
			if test.seeded != "" {
				require.NoError(t, cache.Set(preset, owner, cid, []byte("cached image")))
			}
			recorder := &observingCDNPurgeRecorder{check: func() {
				_, statError := os.Stat(cache.cachePath(preset, owner, cid))
				assert.NoError(t, statError, "the target must be durable before the cached image is deleted")
			}}
			if test.recordError {
				recorder.err = errors.New("recording unavailable")
			}
			checker := blockCheckFunc(func(_ context.Context, checkedOwner, checkedCID string) (bool, error) {
				assert.Equal(t, owner, checkedOwner)
				assert.Equal(t, cid, checkedCID)
				return true, nil
			})
			service, err := NewService(cache, NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), checker, DefaultConfig(), WithCDNPurgeRecorder(recorder))
			require.NoError(t, err)

			image, err := service.GetImageResolvingPDS(t.Context(), preset, owner, cid, resolvedPDS("https://pds.example.test"))
			assert.ErrorIs(t, err, ErrBlobBlocked)
			assert.Empty(t, image)
			if test.seeded != "" {
				assert.Equal(t, [][]BlockedBlob{{{OwnerDID: owner, CID: cid}}}, recorder.called)
			} else {
				assert.Empty(t, recorder.called)
			}
			_, statError := os.Stat(cache.cachePath(preset, owner, cid))
			if test.recordError {
				assert.NoError(t, statError, "failed recording must leave the entry for the next sweep")
			} else {
				assert.True(t, os.IsNotExist(statError), "successful recording must allow deletion")
			}
		})
	}

	t.Run("owner enumeration failure leaves every copy", func(t *testing.T) {
		disk, err := NewDiskCache(t.TempDir(), 1, 0)
		require.NoError(t, err)
		for _, entry := range []struct{ preset, owner string }{
			{"content_preview", owner}, {"content_full", "did:plc:z72i7hdynmk6r22z27h6tvur"},
		} {
			require.NoError(t, disk.Set(entry.preset, entry.owner, cid, []byte("cached image")))
		}
		recorder := &observingCDNPurgeRecorder{}
		listingError := errors.New("owner directory unavailable")
		service, err := NewService(&failingOwnerDirectoryCache{disk, listingError}, NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), allowAllBlockChecker(), DefaultConfig(), WithCDNPurgeRecorder(recorder))
		require.NoError(t, err)
		assert.ErrorIs(t, service.PurgeBlob(t.Context(), cid), listingError)
		assert.Empty(t, recorder.called)
		for _, entry := range []struct{ preset, owner string }{
			{"content_preview", owner}, {"content_full", "did:plc:z72i7hdynmk6r22z27h6tvur"},
		} {
			_, statError := os.Stat(disk.cachePath(entry.preset, entry.owner, cid))
			assert.NoError(t, statError, "enumeration failure must preserve %s for %s", entry.preset, entry.owner)
		}
	})
}

type stallingCDNPurgeRecorder struct {
	deadlineSet chan bool
}

func (recorder *stallingCDNPurgeRecorder) RecordCDNPurgeTargets(ctx context.Context, _ []BlockedBlob) error {
	_, hasDeadline := ctx.Deadline()
	recorder.deadlineSet <- hasDeadline
	<-ctx.Done()
	return ctx.Err()
}

func TestImageProxyService_PurgeBlobBoundsStalledCDNRecording(t *testing.T) {
	const owner = "did:plc:ewvi7nxzyoun6zhxrhs64oiz"
	const cid = moderationTestCID
	const preset = "content_preview"
	const bound = 50 * time.Millisecond
	previous := cdnPurgeRecordTimeout
	cdnPurgeRecordTimeout = bound
	t.Cleanup(func() { cdnPurgeRecordTimeout = previous })

	cache, err := NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	require.NoError(t, cache.Set(preset, owner, cid, []byte("cached image")))
	recorder := &stallingCDNPurgeRecorder{deadlineSet: make(chan bool, 1)}
	service, err := NewService(cache, NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), allowAllBlockChecker(), DefaultConfig(), WithCDNPurgeRecorder(recorder))
	require.NoError(t, err)

	purgeDone := make(chan error, 1)
	go func() { purgeDone <- service.PurgeBlob(t.Context(), cid) }()
	select {
	case purgeErr := <-purgeDone:
		assert.ErrorIs(t, purgeErr, context.DeadlineExceeded)
	case <-time.After(bound + 2*time.Second):
		t.Fatal("PurgeBlob did not return after its CDN recording bound expired")
	}
	select {
	case hasDeadline := <-recorder.deadlineSet:
		assert.True(t, hasDeadline, "the CDN recording must run under a deadline")
	default:
		t.Fatal("the CDN recorder was never called")
	}
	_, statError := os.Stat(cache.cachePath(preset, owner, cid))
	assert.NoError(t, statError, "a timed-out recording must leave the cached image for the sweep")
}
