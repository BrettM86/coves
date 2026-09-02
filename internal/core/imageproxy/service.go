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
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// cacheWriteErrors tracks the number of async cache write failures.
// This provides observability for cache write issues until proper metrics are implemented.
var cacheWriteErrors atomic.Int64

// CacheWriteErrorCount returns the total number of async cache write errors.
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
	// GetImage retrieves an image for the given preset, DID, and CID.
	// It checks the cache first, then fetches from the PDS if not cached,
	// waits for a processing slot, processes the image according to the
	// preset, and stores in cache.
	GetImage(ctx context.Context, preset, did, cid string, pdsURL string) ([]byte, error)
}

// ImageProxyService implements the Service interface and orchestrates
// caching, fetching, and processing of images.
type ImageProxyService struct {
	cache     Cache
	processor Processor
	fetcher   Fetcher
	config    Config

	// Two bounds together cap the transient memory a burst of cold requests
	// can demand, which is what a decompression-bomb flood attacks:
	//
	//   admissionSlots bounds how MANY cache-miss requests are past the cache
	//   check at once, each holding up to MaxSourceSizeMB of fetched blob;
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
func NewService(cache Cache, processor Processor, fetcher Fetcher, config Config) (*ImageProxyService, error) {
	if cache == nil {
		return nil, fmt.Errorf("%w: cache", ErrNilDependency)
	}
	if processor == nil {
		return nil, fmt.Errorf("%w: processor", ErrNilDependency)
	}
	if fetcher == nil {
		return nil, fmt.Errorf("%w: fetcher", ErrNilDependency)
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

	return &ImageProxyService{
		cache:            cache,
		processor:        processor,
		fetcher:          fetcher,
		config:           config,
		admissionSlots:   semaphore.NewWeighted(int64(config.MaxInFlightRequests)),
		processSlots:     semaphore.NewWeighted(int64(config.MaxConcurrentProcesses)),
		processQueueWait: config.ProcessQueueWait,
	}, nil
}

// GetImage retrieves an image for the given preset, DID, and CID.
// The service flow is:
//  1. Validate preset exists
//  2. Check cache for (preset, did, cid) - return if hit
//  3. Acquire an admission slot, waiting at most ProcessQueueWait
//  4. Fetch blob from PDS using pdsURL
//  5. Acquire a processing slot, waiting at most ProcessQueueWait
//  6. Process image with preset
//  7. Store in cache (async, don't block response)
//  8. Return processed image
func (s *ImageProxyService) GetImage(ctx context.Context, presetName, did, cid string, pdsURL string) ([]byte, error) {
	// Step 1: Validate preset exists
	preset, err := GetPreset(presetName)
	if err != nil {
		return nil, err
	}

	// Step 2: Check cache for (preset, did, cid)
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

	// Step 3: Acquire an admission slot. This sits AFTER the cache check so a
	// hit, which costs a file read and holds no blob, is never refused under
	// load; and BEFORE the fetch so the number of fetched blobs held in memory
	// is bounded. The slot spans fetch, the processing-slot wait and decode,
	// and the deferred release runs on every exit.
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

	// Step 4: Fetch blob from PDS
	rawData, err := s.fetcher.Fetch(ctx, pdsURL, did, cid)
	if err != nil {
		return nil, err
	}

	// Step 5: Acquire a processing slot. This happens AFTER the fetch so a slow
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

	// Step 6: Process image with preset
	processedData, err := s.processor.Process(rawData, preset)
	if err != nil {
		return nil, err
	}

	// Step 7: Store in cache (async, don't block response)
	go func() {
		// Use a background context since the original request context may be cancelled
		if cacheErr := s.cache.Set(presetName, did, cid, processedData); cacheErr != nil {
			// Increment error counter for monitoring
			cacheWriteErrors.Add(1)
			slog.Error("[IMAGE-PROXY] async cache write failed",
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
	}()

	// Step 8: Return processed image
	return processedData, nil
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
