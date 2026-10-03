package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/config"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cdnWiringFailProcessor struct{ t *testing.T }

func (processor cdnWiringFailProcessor) Process([]byte, imageproxy.Preset) ([]byte, error) {
	processor.t.Fatal("cached image must not be processed")
	return nil, nil
}

type cdnWiringFailFetcher struct{ t *testing.T }

func (fetcher cdnWiringFailFetcher) Fetch(context.Context, string, string, string) ([]byte, error) {
	fetcher.t.Fatal("cached image must not be fetched")
	return nil, nil
}

type cdnWiringUnblocked struct{}

func (cdnWiringUnblocked) IsBlocked(context.Context, string, string) (bool, error) {
	return false, nil
}

type cdnWiringOwnerlessLister struct{ cid string }

func (lister cdnWiringOwnerlessLister) ListActiveBlockedBlobs(context.Context) ([]imageproxy.BlockedBlob, error) {
	return []imageproxy.BlockedBlob{{OwnerDID: "", CID: lister.cid}}, nil
}

type cdnWiringFailResolver struct {
	identity.Resolver
	t *testing.T
}

func (resolver cdnWiringFailResolver) ResolveDID(context.Context, string) (*identity.DIDDocument, error) {
	resolver.t.Fatal("cached image must not resolve the owner")
	return nil, nil
}

type cdnWiringFakePurger struct{}

func (cdnWiringFakePurger) PurgeBlobs(_ context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
	return imageproxy.CDNPurgeResult{Acknowledged: blobs}
}

type cdnWiringDeleteSignalCache struct {
	*imageproxy.DiskCache
	deleted chan struct{}
}

func (cache *cdnWiringDeleteSignalCache) DeleteCID(cid string) error {
	err := cache.DiskCache.DeleteCID(cid)
	if err == nil {
		select {
		case cache.deleted <- struct{}{}:
		default:
		}
	}
	return err
}

func TestBuildImageProxyServiceStartupSweepAndCacheHeaders(t *testing.T) {
	const (
		ownerDID   = "did:plc:ewvi7nxzyoun6zhxrhs64oiz"
		blockedCID = "bafkreicdmtgb7deaakqrghapqrdjva63vqcieyzzi66q2pokcwyotdermy"
		cachedCID  = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	)
	for _, test := range []struct {
		name      string
		purge     config.CDNPurgeConfig
		wantCache string
	}{
		{
			name: "configured",
			purge: config.CDNPurgeConfig{
				ZoneID: "zone-cdn-wiring", APIToken: "token-cdn-wiring",
				BaseURLs: []string{"https://img.example.test"},
			},
			wantCache: "public, max-age=86400, s-maxage=31536000",
		},
		{name: "unconfigured", wantCache: "public, max-age=86400"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache, err := imageproxy.NewDiskCache(t.TempDir(), 1, 0)
			require.NoError(t, err)
			require.NoError(t, cache.Set("content_preview", ownerDID, blockedCID, []byte("blocked bytes")))
			require.NoError(t, cache.Set("avatar", ownerDID, cachedCID, []byte("cached image bytes")))
			observedCache := &cdnWiringDeleteSignalCache{DiskCache: cache, deleted: make(chan struct{}, 1)}
			cfg := imageproxy.DefaultConfig()
			cfg.CachePath = t.TempDir()
			cfg.CleanupInterval = 0

			var queue *moderation.CDNPurgeQueue
			var targets *cdnWiringPurgeTargetStore
			if test.purge.Enabled() {
				targets = &cdnWiringPurgeTargetStore{recordedSignal: make(chan struct{}, 1)}
				queue = moderation.NewCDNPurgeQueue(targets, cdnWiringFakePurger{}, moderation.CDNPurgeQueueConfig{WriteTimeout: 77 * time.Second})
				t.Cleanup(queue.Close)
			}

			service, handler, cancel, err := buildImageProxyService(cfg, test.purge, queue, imageProxyDependencies{
				cache: observedCache, processor: cdnWiringFailProcessor{t}, fetcher: cdnWiringFailFetcher{t},
				blocks: cdnWiringUnblocked{}, lister: cdnWiringOwnerlessLister{cid: blockedCID},
				resolver: cdnWiringFailResolver{t: t},
			})
			if cancel != nil {
				t.Cleanup(cancel)
			}
			require.NoError(t, err)
			require.NotNil(t, handler)
			require.NotNil(t, service)

			if targets != nil {
				select {
				case <-targets.recordedSignal:
				case <-time.After(10 * time.Second):
					t.Fatal("startup sweep did not record the ownerless block's cached owner")
				}
				assert.Equal(t, []imageproxy.BlockedBlob{{OwnerDID: ownerDID, CID: blockedCID}}, targets.recordedBlobs())
			} else {
				select {
				case <-observedCache.deleted:
				case <-time.After(10 * time.Second):
					t.Fatal("startup sweep did not delete the blocked CID")
				}
				_, found, err := cache.Get("content_preview", ownerDID, blockedCID)
				require.NoError(t, err)
				assert.False(t, found)
			}

			router := chi.NewRouter()
			router.Get("/img/{preset}/plain/{did}/{cid}", handler.HandleImage)
			request := httptest.NewRequest(http.MethodGet, "/img/avatar/plain/"+ownerDID+"/"+cachedCID, nil)
			request.Host = "img.example.test"
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, []byte("cached image bytes"), response.Body.Bytes())
			assert.Equal(t, test.wantCache, response.Header().Get("Cache-Control"))
		})
	}
}

type cdnWiringPanickingLister struct{ calls, withoutDeadline atomic.Int64 }

func (lister *cdnWiringPanickingLister) ListActiveBlockedBlobs(ctx context.Context) ([]imageproxy.BlockedBlob, error) {
	if lister.calls.Add(1) == 1 {
		panic("first blocked media purge cycle explodes")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		lister.withoutDeadline.Add(1)
	}
	return nil, nil
}

// One panicking sweep must cost one cycle, not the job: a dead purge job
// leaves blocked bytes the disk refused to drop in the cache until restart.
func TestBuildImageProxyServiceBlockedMediaPurgeSurvivesAPanickingCycle(t *testing.T) {
	cache, err := imageproxy.NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	cfg := imageproxy.DefaultConfig()
	cfg.CachePath = t.TempDir()
	cfg.CleanupInterval = time.Millisecond
	lister := &cdnWiringPanickingLister{}

	_, _, stop, err := buildImageProxyService(cfg, config.CDNPurgeConfig{}, nil, imageProxyDependencies{
		cache: cache, processor: cdnWiringFailProcessor{t}, fetcher: cdnWiringFailFetcher{t},
		blocks: cdnWiringUnblocked{}, lister: lister, resolver: cdnWiringFailResolver{t: t},
	})
	require.NoError(t, err)
	t.Cleanup(stop)

	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		return lister.calls.Load() >= 3, nil
	}, testkit.WithDescription("the blocked media purge job keeps cycling after a panicking cycle"),
		testkit.WithDiagnostics(func() string { return fmt.Sprintf("list calls: %d", lister.calls.Load()) }))
	assert.Zero(t, lister.withoutDeadline.Load(), "every purge cycle must run under a deadline")
}
