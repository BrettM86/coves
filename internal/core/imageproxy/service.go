// Package imageproxy provides image proxy functionality for AT Protocol applications.
// It handles fetching, caching, and transforming images from Personal Data Servers (PDS).
//
// The package implements a multi-tier architecture:
//   - Service: Orchestrates caching, fetching, and processing
//   - Cache: Disk-based LRU cache with TTL-based expiration
//   - Fetcher: Retrieves blobs from AT Protocol PDSes
//   - Processor: Transforms images according to preset configurations
//
// Presets define image transformation parameters (dimensions, fit mode, quality)
// for common use cases like avatars, banners, and feed thumbnails.
package imageproxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// cacheWriteErrors tracks the number of cache write failures.
// This provides observability for cache write issues until proper metrics are implemented.
var cacheWriteErrors atomic.Int64

// CacheWriteErrorCount returns the total number of cache write errors.
// This is useful for monitoring and alerting on cache health.
func CacheWriteErrorCount() int64 {
	return cacheWriteErrors.Load()
}

// processorBusyRefusals counts requests refused because every processing slot
// stayed occupied for the whole queue wait. Callers that abandoned their own
// request are not counted: the number is meant to say whether the slot count
// is too small for the load, and a client hanging up says nothing about that.
var processorBusyRefusals atomic.Int64

// ProcessorBusyRefusalCount returns the total number of busy refusals so far.
// Like CacheWriteErrorCount it stands in for a metric until proper metrics
// exist: a rising rate means the processing slots are saturated, which is
// either an attack (many worst-case images at once) or under-provisioning,
// and the signal to raise MaxConcurrentProcesses or add capacity.
func ProcessorBusyRefusalCount() int64 {
	return processorBusyRefusals.Load()
}

// Service defines the interface for the image proxy service.
type Service interface {
	// GetImageResolvingPDS retrieves an image for the given preset, DID, and
	// CID. It checks moderation before the cache, then the cache; on a miss it
	// resolves the DID's PDS with resolvePDS, fetches the blob, waits for a
	// processing slot, processes the image according to the preset, and
	// stores it in the cache. resolvePDS runs only on a cache miss, so a
	// cached image is served without a DID lookup, which may be a PLC round
	// trip. Its error is returned unwrapped so the caller can classify it.
	GetImageResolvingPDS(ctx context.Context, preset, did, cid string, resolvePDS func(context.Context) (string, error)) ([]byte, error)
}

// ImageProxyService implements the Service interface and orchestrates
// caching, fetching, and processing of images.
type ImageProxyService struct {
	cache     Cache
	processor Processor
	fetcher   Fetcher
	blocks    BlockChecker
	config    Config
	// A fixed number of stripes serializes cache publication with purges for
	// the same CID without retaining locks for every blob ever requested.
	// Each stripe is a weight-1 semaphore rather than a mutex so a waiter can
	// give up when its context ends.
	publicationLocks [256]*semaphore.Weighted
	// publicationTimeout bounds the wait for the stripe and the moderation
	// recheck under it. The cache write that follows is not bounded by it.
	publicationTimeout time.Duration

	// Two bounds together cap the transient memory a burst of cold requests
	// can demand, which is what a decompression-bomb flood attacks:
	//
	//   admissionSlots bounds how MANY cache-miss requests are past the cache
	//   check at once, each holding up to MaxSourceSizeMB of fetched blob or,
	//   once processed, its output until publication finishes;
	//   processSlots bounds how many of those DECODE at once, each costing up
	//   to the pixel budget × ~19 B/px (see the processor's cost model).
	//
	// Total transient memory is therefore about
	//   MaxInFlightRequests × MaxSourceSizeMB + MaxConcurrentProcesses × budget × 19 B/px
	// rather than either term × however many connections an attacker opens.
	admissionSlots *semaphore.Weighted
	processSlots   *semaphore.Weighted

	// processQueueWait is how long a request may wait for either kind of slot
	// before it is refused as busy.
	processQueueWait time.Duration
}

// NewService creates a new ImageProxyService with the provided dependencies.
// Returns an error if any required dependency is nil or if the processing
// budgets the service enforces itself are not positive.
func NewService(cache Cache, processor Processor, fetcher Fetcher, blocks BlockChecker, config Config) (*ImageProxyService, error) {
	if cache == nil {
		return nil, fmt.Errorf("%w: cache", ErrNilDependency)
	}
	if processor == nil {
		return nil, fmt.Errorf("%w: processor", ErrNilDependency)
	}
	if fetcher == nil {
		return nil, fmt.Errorf("%w: fetcher", ErrNilDependency)
	}
	if blocks == nil {
		return nil, fmt.Errorf("%w: block checker", ErrNilDependency)
	}
	// A zero-slot semaphore would refuse every request and a zero wait would
	// refuse any request that did not find a free slot on its first try.
	if config.MaxConcurrentProcesses <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidMaxConcurrentProcesses, config.MaxConcurrentProcesses)
	}
	if config.ProcessQueueWait <= 0 {
		return nil, fmt.Errorf("%w: got %v", ErrInvalidProcessQueueWait, config.ProcessQueueWait)
	}
	if config.MaxInFlightRequests <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidMaxInFlightRequests, config.MaxInFlightRequests)
	}

	service := &ImageProxyService{
		cache:              cache,
		processor:          processor,
		fetcher:            fetcher,
		blocks:             blocks,
		config:             config,
		publicationTimeout: defaultPublicationTimeout,
		admissionSlots:     semaphore.NewWeighted(int64(config.MaxInFlightRequests)),
		processSlots:       semaphore.NewWeighted(int64(config.MaxConcurrentProcesses)),
		processQueueWait:   config.ProcessQueueWait,
	}
	for index := range service.publicationLocks {
		service.publicationLocks[index] = semaphore.NewWeighted(1)
	}
	return service, nil
}

// defaultPublicationTimeout bounds the publication stripe wait and the
// post-fetch moderation recheck, which run after the caller may already have
// gone away.
const defaultPublicationTimeout = 10 * time.Second

// GetImageResolvingPDS implements Service. The service flow is:
//  1. Validate preset exists and the owner DID is canonical
//  2. Check moderation, then cache for (preset, did, cid) - return if hit
//  3. Resolve the DID's PDS with resolvePDS (misses only)
//  4. Acquire an admission slot, waiting at most ProcessQueueWait
//  5. Fetch blob from the PDS
//  6. Acquire a processing slot, waiting at most ProcessQueueWait
//  7. Process image with preset
//  8. Release the processing slot, then under the CID lock recheck
//     moderation and store in cache, on a context detached from the caller
//  9. Release the admission slot and return the processed image
//
// The PDS is resolved only once the cache has missed: before, every request —
// hits included, which are nearly all of them — paid a DID resolution first.
func (s *ImageProxyService) GetImageResolvingPDS(
	ctx context.Context,
	presetName, did, cid string,
	resolvePDS func(context.Context) (string, error),
) ([]byte, error) {
	// Step 1: Validate preset exists
	preset, err := GetPreset(presetName)
	if err != nil {
		return nil, err
	}
	// A second spelling of the owner would miss its block and could read
	// another spelling's cache directory; see ValidateOwnerDID.
	if err := ValidateOwnerDID(did); err != nil {
		return nil, err
	}

	// Step 2: Check moderation before reading even a warm cache entry.
	blocked, err := s.IsBlobBlocked(ctx, did, cid)
	if err != nil {
		return nil, err
	}
	if blocked {
		// A purge lost to a crash or a disk error leaves bytes behind; any
		// request for them removes this entry.
		s.purgeBlockedEntry(ctx, presetName, did, cid)
		return nil, ErrBlobBlocked
	}

	// Check cache for (preset, did, cid)
	cachedData, found, err := s.cache.Get(presetName, did, cid)
	if err != nil {
		// Log cache read error but continue - cache miss is acceptable
		slog.Warn("[IMAGE-PROXY] cache read error, falling back to fetch",
			"preset", presetName,
			"did", did,
			"cid", cid,
			"error", err,
		)
	}
	if found {
		slog.Debug("[IMAGE-PROXY] cache hit",
			"preset", presetName,
			"did", did,
			"cid", cid,
		)
		return cachedData, nil
	}

	// Step 3: A miss needs the PDS that hosts the blob. Resolved before the
	// admission slot: a slow DID lookup holds no blob, so it must not
	// occupy one of the slots that bound blobs held in memory.
	pdsURL, err := resolvePDS(ctx)
	if err != nil {
		return nil, err
	}

	// Step 4: Acquire an admission slot. This sits AFTER the cache check so a
	// hit, which costs a file read and holds no blob, is never refused under
	// load; and BEFORE the fetch so the number of fetched blobs held in memory
	// is bounded. The slot spans fetch, the processing-slot wait, decode and
	// publication: a processed output waiting on a slow recheck is transient
	// memory too, and content presets bound it only by the source pixel budget.
	// The deferred release runs on every exit.
	err = s.acquireSlot(ctx, s.admissionSlots, "admission", func(waitErr error) error {
		slog.Warn("[IMAGE-PROXY] in-flight request cap reached, shedding request",
			"preset", presetName,
			"did", did,
			"cid", cid,
			"in_flight_cap", s.config.MaxInFlightRequests,
			"waited", s.processQueueWait,
			"total_busy_refusals", processorBusyRefusals.Load(),
		)
		return fmt.Errorf("%w: waited %v for admission (in-flight cap %d): %w",
			ErrProcessorBusy, s.processQueueWait, s.config.MaxInFlightRequests, waitErr)
	})
	if err != nil {
		return nil, err
	}
	defer s.admissionSlots.Release(1)

	processedData, err := s.fetchAndProcess(ctx, preset, presetName, did, cid, pdsURL)
	if err != nil {
		return nil, err
	}

	// Step 8: The check and the cache publication must be atomic against
	// purges; a purge after this write removes the bytes before returning.
	if err := s.publish(ctx, presetName, did, cid, processedData); err != nil {
		return nil, err
	}

	// Step 9: Return processed image
	return processedData, nil
}

// fetchAndProcess runs steps 5 to 7 of GetImageResolvingPDS under the caller's admission
// slot. The processing slot is released when it returns, so it is not held
// across the moderation recheck and cache write that follow: those wait on a
// database and a stripe lock, not on CPU, and a decode slot held there would
// refuse unrelated requests as busy.
func (s *ImageProxyService) fetchAndProcess(ctx context.Context, preset Preset, presetName, did, cid, pdsURL string) ([]byte, error) {
	// Step 5: Fetch blob from PDS
	rawData, err := s.fetcher.Fetch(ctx, pdsURL, did, cid)
	if err != nil {
		return nil, err
	}

	// Step 6: Acquire a processing slot. This happens AFTER the fetch so a slow
	// or hostile PDS cannot pin a slot for the whole network round-trip; slots
	// are only ever held while CPU and memory are actually being spent. The
	// wait is bounded because a waiter is holding a fetched blob of up to
	// MaxSourceSizeMB: the bound limits how LONG each waiter holds that memory,
	// while how MANY waiters exist is bounded by the admission slot above.
	err = s.acquireSlot(ctx, s.processSlots, "processing", func(waitErr error) error {
		slog.Warn("[IMAGE-PROXY] processing slots exhausted, shedding request",
			"preset", presetName,
			"did", did,
			"cid", cid,
			"waited", s.processQueueWait,
			"total_busy_refusals", processorBusyRefusals.Load(),
		)
		return fmt.Errorf("%w: waited %v for a processing slot: %w", ErrProcessorBusy, s.processQueueWait, waitErr)
	})
	if err != nil {
		return nil, err
	}
	defer s.processSlots.Release(1)

	// Step 7: Process image with preset
	return s.processor.Process(rawData, preset)
}

// publish rechecks moderation and writes the processed image to the cache
// under the CID's publication lock. It runs on a context detached from the
// caller, so a client that hangs up after processing still leaves the bytes
// cached, and bounded by publicationTimeout, so a slow database cannot pin the
// stripe. A stripe that cannot be taken in time fails closed: the recheck has
// not run, so the bytes are not served.
func (s *ImageProxyService) publish(ctx context.Context, presetName, did, cid string, processedData []byte) error {
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.publicationTimeout)
	defer cancel()
	release, err := s.lockPublication(publishCtx, cid)
	if err != nil {
		return fmt.Errorf("%w: waiting for the publication lock: %w", ErrBlockCheckFailed, err)
	}
	defer release()
	blocked, err := s.IsBlobBlocked(publishCtx, did, cid)
	if err != nil {
		return err
	}
	if blocked {
		return ErrBlobBlocked
	}
	if cacheErr := s.cache.Set(presetName, did, cid, processedData); cacheErr != nil {
		cacheWriteErrors.Add(1)
		slog.Error("[IMAGE-PROXY] cache write failed",
			"preset", presetName,
			"did", did,
			"cid", cid,
			"error", cacheErr,
			"total_cache_write_errors", cacheWriteErrors.Load(),
		)
	} else {
		slog.Debug("[IMAGE-PROXY] cached processed image",
			"preset", presetName,
			"did", did,
			"cid", cid,
			"size_bytes", len(processedData),
		)
	}
	return nil
}

// acquireSlot takes one unit of sem on the caller's behalf, waiting at most
// processQueueWait, and classifies a failure so that both bounds report the
// same way. stage names the slot in abandonment messages; onBusy builds the
// log line and error for a genuine capacity refusal, and is only called once
// it is known that the CALLER is still live and only our wait expired.
//
// A caller that has already gone away must not spend a slot: Acquire can
// succeed without blocking when a slot is free even if its context is done,
// so the caller's context is checked explicitly first. After a failed Acquire
// the caller's context is checked again, because Acquire returns the context's
// own error and that does not say whose deadline fired. A done caller context
// means a disconnect or the caller's own deadline, reported exactly as the
// fetcher reports it (ErrPDSTimeout) so a disconnect reads the same at every
// step and says nothing about capacity.
func (s *ImageProxyService) acquireSlot(ctx context.Context, sem *semaphore.Weighted, stage string, onBusy func(waitErr error) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: request abandoned before %s: %w", ErrPDSTimeout, stage, err)
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, s.processQueueWait)
	err := sem.Acquire(waitCtx, 1)
	cancelWait()
	if err == nil {
		return nil
	}
	if callerErr := ctx.Err(); callerErr != nil {
		return fmt.Errorf("%w: request abandoned while waiting for %s: %w", ErrPDSTimeout, stage, callerErr)
	}
	processorBusyRefusals.Add(1)
	return onBusy(err)
}

// BlockChecker reports whether moderation blocks serving a blob.
type BlockChecker interface {
	IsBlocked(ctx context.Context, ownerDID, cid string) (bool, error)
}

// IsBlobBlocked reports whether moderation blocks serving the blob. A
// noncanonical owner DID returns ErrInvalidDID: blocks match one spelling.
func (s *ImageProxyService) IsBlobBlocked(ctx context.Context, did, cid string) (bool, error) {
	if err := ValidateOwnerDID(did); err != nil {
		return false, err
	}
	blocked, err := s.blocks.IsBlocked(ctx, did, cid)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrBlockCheckFailed, err)
	}
	return blocked, nil
}

// PurgeOwnerBlob removes the owner's cached copies of a blocked blob. The
// caller must have committed the block first.
func (s *ImageProxyService) PurgeOwnerBlob(did, cid string) error {
	if err := s.awaitPublications(context.Background(), cid); err != nil {
		return err
	}
	return s.cache.DeleteOwner(did, cid)
}

// PurgeBlob removes every owner's cached copies of a blocked blob. The caller
// must have committed the block first.
func (s *ImageProxyService) PurgeBlob(cid string) error {
	if err := s.awaitPublications(context.Background(), cid); err != nil {
		return err
	}
	return s.cache.DeleteCID(cid)
}

// awaitPublications waits until every cache publication of cid that holds or
// is queued for the stripe has finished, and does not keep the stripe for the
// disk work that follows. That is enough once the block is committed: a
// publication that took the stripe before this call has finished writing by
// the time it returns, so the caller's deletion sees its file, and one that
// takes the stripe afterwards rechecks moderation after the commit and sees
// the block, so it writes nothing.
func (s *ImageProxyService) awaitPublications(ctx context.Context, cid string) error {
	release, err := s.lockPublication(ctx, cid)
	if err != nil {
		return err
	}
	release()
	return nil
}

// purgeBlockedEntry removes one blocked cache entry on behalf of a request the
// pre-cache check refused. A failure is logged and otherwise ignored: the
// request is refused either way, and the periodic sweep retries. A caller
// that went away while waiting for the stripe is not a failure worth logging.
func (s *ImageProxyService) purgeBlockedEntry(ctx context.Context, presetName, did, cid string) {
	if err := s.awaitPublications(ctx, cid); err != nil {
		return
	}
	if err := s.cache.Delete(presetName, did, cid); err != nil {
		slog.Error("[IMAGE-PROXY] failed to purge a blocked cache entry",
			"preset", presetName,
			"did", did,
			"cid", cid,
			"error", err,
		)
	}
}

// BlockedBlob identifies a blob with an active moderation block: one owner's
// copy, or every owner's copy when OwnerDID is empty.
type BlockedBlob struct {
	OwnerDID string
	CID      string
}

// BlockedBlobLister lists every blob with an active moderation block.
type BlockedBlobLister interface {
	ListActiveBlockedBlobs(ctx context.Context) ([]BlockedBlob, error)
}

// PurgeActiveBlocks removes the cached bytes of every blob with an active
// block. The purge a moderation action runs after commit is lost if the
// process exits first or the disk refuses it; this sweep makes that purge
// restart-safe. It keeps going past a failed blob and returns every failure
// joined.
func (s *ImageProxyService) PurgeActiveBlocks(ctx context.Context, lister BlockedBlobLister) error {
	blobs, err := lister.ListActiveBlockedBlobs(ctx)
	if err != nil {
		return fmt.Errorf("listing blocked blobs: %w", err)
	}
	everyOwner := make(map[string]bool)
	for _, blob := range blobs {
		if blob.OwnerDID == "" {
			everyOwner[blob.CID] = true
		}
	}
	var errs []error
	for _, blob := range blobs {
		if blob.OwnerDID != "" && everyOwner[blob.CID] {
			continue // the every-owner purge below covers this owner
		}
		if err := s.awaitPublications(ctx, blob.CID); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if blob.OwnerDID == "" {
			err = s.cache.DeleteCID(blob.CID)
		} else {
			err = s.cache.DeleteOwner(blob.OwnerDID, blob.CID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("purging blob %s: %w", blob.CID, err))
		}
	}
	return errors.Join(errs...)
}

// StartActiveBlockPurgeJob runs PurgeActiveBlocks once straight away, which
// completes any purge a restart interrupted, and then every interval. An
// interval of zero or less runs only the startup sweep. The returned function
// stops the job.
func (s *ImageProxyService) StartActiveBlockPurgeJob(lister BlockedBlobLister, interval time.Duration) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("[IMAGE-PROXY] CRITICAL: blocked media purge job panicked", "panic", r)
			}
		}()
		s.runActiveBlockPurge(ctx, lister)
		if interval <= 0 {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runActiveBlockPurge(ctx, lister)
			}
		}
	}()
	return cancel
}

func (s *ImageProxyService) runActiveBlockPurge(ctx context.Context, lister BlockedBlobLister) {
	if err := s.PurgeActiveBlocks(ctx, lister); err != nil && ctx.Err() == nil {
		slog.Error("[IMAGE-PROXY] blocked media purge failed", "error", err)
	}
}

// lockPublication takes cid's publication stripe, giving up when ctx ends.
func (s *ImageProxyService) lockPublication(ctx context.Context, cid string) (release func(), err error) {
	lock := s.publicationLock(cid)
	if err := lock.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return func() { lock.Release(1) }, nil
}

func (s *ImageProxyService) publicationLock(cid string) *semaphore.Weighted {
	digest := sha256.Sum256([]byte(cid))
	return s.publicationLocks[digest[0]]
}
