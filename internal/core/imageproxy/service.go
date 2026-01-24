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
)

// cacheWriteErrors tracks the number of async cache write failures.
// This provides observability for cache write issues until proper metrics are implemented.
var cacheWriteErrors atomic.Int64

// CacheWriteErrorCount returns the total number of async cache write errors.
// This is useful for monitoring and alerting on cache health.
func CacheWriteErrorCount() int64 {
	return cacheWriteErrors.Load()
}

// Service defines the interface for the image proxy service.
type Service interface {
	// GetImage retrieves an image for the given preset, DID, and CID.
	// It checks the cache first, then fetches from the PDS if not cached,
	// processes the image according to the preset, and stores in cache.
	GetImage(ctx context.Context, preset, did, cid string, pdsURL string) ([]byte, error)
}

// ImageProxyService implements the Service interface and orchestrates
// caching, fetching, and processing of images.
type ImageProxyService struct {
	cache     Cache
	processor Processor
	fetcher   Fetcher
	config    Config
}

// NewService creates a new ImageProxyService with the provided dependencies.
// Returns an error if any required dependency is nil.
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

	return &ImageProxyService{
		cache:     cache,
		processor: processor,
		fetcher:   fetcher,
		config:    config,
	}, nil
}

// GetImage retrieves an image for the given preset, DID, and CID.
// The service flow is:
//  1. Validate preset exists
//  2. Check cache for (preset, did, cid) - return if hit
//  3. Fetch blob from PDS using pdsURL
//  4. Process image with preset
//  5. Store in cache (async, don't block response)
//  6. Return processed image
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

	// Step 3: Fetch blob from PDS
	rawData, err := s.fetcher.Fetch(ctx, pdsURL, did, cid)
	if err != nil {
		return nil, err
	}

	// Step 4: Process image with preset
	processedData, err := s.processor.Process(rawData, preset)
	if err != nil {
		return nil, err
	}

	// Step 5: Store in cache (async, don't block response)
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

	// Step 6: Return processed image
	return processedData, nil
}
