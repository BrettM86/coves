package imageproxy

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	// ErrEmptyParameter is returned when a required parameter is empty
	ErrEmptyParameter = errors.New("required parameter is empty")
	// ErrInvalidCacheBasePath is returned when the cache base path is empty
	ErrInvalidCacheBasePath = errors.New("cache base path cannot be empty")
	// ErrInvalidCacheMaxSize is returned when maxSizeGB is not positive
	ErrInvalidCacheMaxSize = errors.New("cache max size must be positive")
)

// Cache defines the interface for image proxy caching
type Cache interface {
	// Get retrieves cached image data for the given preset, DID, and CID.
	// Returns the data, whether it was found, and any error.
	Get(preset, did, cid string) ([]byte, bool, error)

	// Set stores image data in the cache for the given preset, DID, and CID.
	Set(preset, did, cid string, data []byte) error

	// Delete removes cached image data for the given preset, DID, and CID.
	Delete(preset, did, cid string) error

	// Cleanup runs both LRU eviction and TTL cleanup.
	// Returns the number of entries removed and any error.
	Cleanup() (int, error)
}

// DiskCache implements Cache using the filesystem for storage.
// Cache key format: {basePath}/{preset}/{did_safe}/{cid}
// where did_safe has colons replaced with underscores for filesystem safety.
type DiskCache struct {
	basePath  string
	maxSizeGB int
	ttlDays   int
}

// NewDiskCache creates a new DiskCache with the specified base path, maximum size, and TTL.
// Returns an error if basePath is empty or maxSizeGB is not positive.
// ttlDays of 0 disables TTL-based cleanup (only LRU eviction applies).
func NewDiskCache(basePath string, maxSizeGB int, ttlDays int) (*DiskCache, error) {
	if basePath == "" {
		return nil, ErrInvalidCacheBasePath
	}
	if maxSizeGB <= 0 {
		return nil, ErrInvalidCacheMaxSize
	}
	if ttlDays < 0 {
		return nil, errors.New("ttlDays cannot be negative")
	}
	return &DiskCache{
		basePath:  basePath,
		maxSizeGB: maxSizeGB,
		ttlDays:   ttlDays,
	}, nil
}

// makeDIDSafe converts a DID to a filesystem-safe directory name.
// It sanitizes the input to prevent path traversal attacks by:
// - Replacing colons with underscores
// - Removing path separators (/ and \)
// - Removing path traversal sequences (..)
// - Removing null bytes
func makeDIDSafe(did string) string {
	// Replace colons with underscores
	s := strings.ReplaceAll(did, ":", "_")

	// Remove path separators to prevent directory escape
	s = strings.ReplaceAll(s, "/", "")
	s = strings.ReplaceAll(s, "\\", "")

	// Remove path traversal sequences
	s = strings.ReplaceAll(s, "..", "")

	// Remove null bytes (could be used to terminate strings early)
	s = strings.ReplaceAll(s, "\x00", "")

	return s
}

// makeCIDSafe sanitizes a CID for use in filesystem paths.
// It removes characters that could be used for path traversal attacks.
func makeCIDSafe(cid string) string {
	// Remove path separators to prevent directory escape
	s := strings.ReplaceAll(cid, "/", "")
	s = strings.ReplaceAll(s, "\\", "")

	// Remove path traversal sequences
	s = strings.ReplaceAll(s, "..", "")

	// Remove null bytes
	s = strings.ReplaceAll(s, "\x00", "")

	return s
}

// makePresetSafe sanitizes a preset name for use in filesystem paths.
func makePresetSafe(preset string) string {
	// Remove path separators
	s := strings.ReplaceAll(preset, "/", "")
	s = strings.ReplaceAll(s, "\\", "")

	// Remove path traversal sequences
	s = strings.ReplaceAll(s, "..", "")

	// Remove null bytes
	s = strings.ReplaceAll(s, "\x00", "")

	return s
}

// cachePath constructs the full filesystem path for a cached item.
// All components are sanitized to prevent path traversal attacks.
func (c *DiskCache) cachePath(preset, did, cid string) string {
	presetSafe := makePresetSafe(preset)
	didSafe := makeDIDSafe(did)
	cidSafe := makeCIDSafe(cid)
	return filepath.Join(c.basePath, presetSafe, didSafe, cidSafe)
}

// validateParams checks that all required parameters are non-empty.
func validateParams(preset, did, cid string) error {
	if preset == "" || did == "" || cid == "" {
		return ErrEmptyParameter
	}
	return nil
}

// Get retrieves cached image data for the given preset, DID, and CID.
// Returns the data, whether it was found, and any error.
// If the item is not in cache, returns (nil, false, nil).
// Updates the file's modification time on access for LRU tracking.
func (c *DiskCache) Get(preset, did, cid string) ([]byte, bool, error) {
	if err := validateParams(preset, did, cid); err != nil {
		return nil, false, err
	}

	path := c.cachePath(preset, did, cid)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	// Update mtime for LRU tracking
	// Log errors as warnings since failed mtime updates degrade LRU accuracy
	now := time.Now()
	if chtimesErr := os.Chtimes(path, now, now); chtimesErr != nil {
		slog.Warn("[IMAGE-PROXY] failed to update mtime for LRU tracking",
			"path", path,
			"error", chtimesErr,
		)
	}

	return data, true, nil
}

// Set stores image data in the cache for the given preset, DID, and CID.
// Creates necessary directories if they don't exist.
func (c *DiskCache) Set(preset, did, cid string, data []byte) error {
	if err := validateParams(preset, did, cid); err != nil {
		return err
	}

	path := c.cachePath(preset, did, cid)
	dir := filepath.Dir(path)

	// Create directory structure if it doesn't exist
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Write the file atomically by writing to a temp file first
	// then renaming (to avoid partial writes on crash)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

// Delete removes cached image data for the given preset, DID, and CID.
// Returns nil if the item doesn't exist (idempotent delete).
func (c *DiskCache) Delete(preset, did, cid string) error {
	if err := validateParams(preset, did, cid); err != nil {
		return err
	}

	path := c.cachePath(preset, did, cid)

	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// cacheEntry represents a cached file with its metadata.
type cacheEntry struct {
	path    string
	size    int64
	modTime time.Time
}

// scanCache walks the cache directory and returns all cache entries.
func (c *DiskCache) scanCache() ([]cacheEntry, int64, error) {
	var entries []cacheEntry
	var totalSize int64

	err := filepath.WalkDir(c.basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			slog.Warn("[IMAGE-PROXY] failed to stat file during cache scan, cache size may be inaccurate",
				"path", path,
				"error", err,
			)
			return nil // Skip files we can't stat
		}

		entries = append(entries, cacheEntry{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		totalSize += info.Size()

		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, 0, err
	}

	return entries, totalSize, nil
}

// GetCacheSize returns the current cache size in bytes.
func (c *DiskCache) GetCacheSize() (int64, error) {
	_, totalSize, err := c.scanCache()
	return totalSize, err
}

// EvictLRU removes the least recently used entries until the cache is under the size limit.
// Returns the number of entries removed.
func (c *DiskCache) EvictLRU() (int, error) {
	entries, totalSize, err := c.scanCache()
	if err != nil {
		return 0, err
	}

	maxSizeBytes := int64(c.maxSizeGB) * 1024 * 1024 * 1024
	if totalSize <= maxSizeBytes {
		return 0, nil // Under limit, nothing to do
	}

	// Sort by modification time (oldest first for LRU)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.Before(entries[j].modTime)
	})

	removed := 0
	for _, entry := range entries {
		if totalSize <= maxSizeBytes {
			break
		}

		if err := os.Remove(entry.path); err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("[IMAGE-PROXY] failed to remove cache entry during LRU eviction",
					"path", entry.path,
					"error", err,
				)
			}
			continue
		}

		totalSize -= entry.size
		removed++

		slog.Debug("[IMAGE-PROXY] evicted cache entry (LRU)",
			"path", entry.path,
			"size_bytes", entry.size,
		)
	}

	if removed > 0 {
		slog.Info("[IMAGE-PROXY] LRU eviction completed",
			"entries_removed", removed,
			"new_size_bytes", totalSize,
			"max_size_bytes", maxSizeBytes,
		)
	}

	return removed, nil
}

// CleanExpired removes cache entries older than the configured TTL.
// Returns the number of entries removed.
// If TTL is 0 (disabled), returns 0 without scanning.
func (c *DiskCache) CleanExpired() (int, error) {
	if c.ttlDays <= 0 {
		return 0, nil // TTL disabled
	}

	entries, _, err := c.scanCache()
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().AddDate(0, 0, -c.ttlDays)
	removed := 0

	for _, entry := range entries {
		if entry.modTime.After(cutoff) {
			continue // Not expired
		}

		if err := os.Remove(entry.path); err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("[IMAGE-PROXY] failed to remove expired cache entry",
					"path", entry.path,
					"mod_time", entry.modTime,
					"error", err,
				)
			}
			continue
		}

		removed++

		slog.Debug("[IMAGE-PROXY] removed expired cache entry",
			"path", entry.path,
			"mod_time", entry.modTime,
			"ttl_days", c.ttlDays,
		)
	}

	if removed > 0 {
		slog.Info("[IMAGE-PROXY] TTL cleanup completed",
			"entries_removed", removed,
			"ttl_days", c.ttlDays,
		)
	}

	return removed, nil
}

// Cleanup runs both TTL cleanup and LRU eviction.
// TTL cleanup runs first (removes definitely stale entries),
// then LRU eviction runs if still over size limit.
// Returns the total number of entries removed.
func (c *DiskCache) Cleanup() (int, error) {
	totalRemoved := 0

	// First, remove expired entries (definitely stale)
	ttlRemoved, err := c.CleanExpired()
	if err != nil {
		return 0, err
	}
	totalRemoved += ttlRemoved

	// Then, run LRU eviction if still over limit
	lruRemoved, err := c.EvictLRU()
	if err != nil {
		return totalRemoved, err
	}
	totalRemoved += lruRemoved

	return totalRemoved, nil
}

// cleanEmptyDirs removes empty directories under the cache base path.
// This is useful after eviction/cleanup to remove orphaned preset/DID directories.
func (c *DiskCache) cleanEmptyDirs() error {
	// Walk in reverse depth order to clean leaf directories first
	var dirs []string

	var walkErrors []error
	err := filepath.WalkDir(c.basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Log WalkDir errors but continue scanning to clean as much as possible
			slog.Warn("[IMAGE-PROXY] error during empty dir cleanup scan",
				"path", path,
				"error", err,
			)
			walkErrors = append(walkErrors, err)
			return nil // Continue scanning despite errors
		}
		if d.IsDir() && path != c.basePath {
			dirs = append(dirs, path)
		}
		return nil
	})

	if err != nil {
		return err
	}

	if len(walkErrors) > 0 {
		slog.Warn("[IMAGE-PROXY] encountered errors during empty dir cleanup scan",
			"error_count", len(walkErrors),
		)
	}

	// Sort by length descending (deepest paths first)
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})

	var removeErrors int
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			slog.Warn("[IMAGE-PROXY] failed to read directory during cleanup",
				"path", dir,
				"error", err,
			)
			continue
		}
		if len(entries) == 0 {
			if removeErr := os.Remove(dir); removeErr != nil {
				slog.Warn("[IMAGE-PROXY] failed to remove empty directory",
					"path", dir,
					"error", removeErr,
				)
				removeErrors++
			}
		}
	}

	if removeErrors > 0 {
		slog.Warn("[IMAGE-PROXY] some empty directories could not be removed",
			"failed_count", removeErrors,
		)
	}

	return nil
}

// StartCleanupJob starts a background goroutine that periodically runs cache cleanup.
// Returns a cancel function that should be called during graceful shutdown.
// If interval is 0 or negative, no cleanup job is started and the cancel function is a no-op.
func (c *DiskCache) StartCleanupJob(interval time.Duration) context.CancelFunc {
	if interval <= 0 {
		slog.Info("[IMAGE-PROXY] cache cleanup job disabled (interval=0)")
		return func() {} // No-op cancel
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("[IMAGE-PROXY] CRITICAL: cache cleanup job panicked",
					"panic", r,
				)
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		slog.Info("[IMAGE-PROXY] cache cleanup job started",
			"interval", interval,
			"ttl_days", c.ttlDays,
			"max_size_gb", c.maxSizeGB,
		)

		cycleCount := 0
		for {
			select {
			case <-ctx.Done():
				slog.Info("[IMAGE-PROXY] cache cleanup job stopped")
				return
			case <-ticker.C:
				cycleCount++

				removed, err := c.Cleanup()
				if err != nil {
					slog.Error("[IMAGE-PROXY] cache cleanup error",
						"error", err,
						"cycle", cycleCount,
					)
					continue
				}

				// Also clean up empty directories after removing files
				if removed > 0 {
					if err := c.cleanEmptyDirs(); err != nil {
						slog.Warn("[IMAGE-PROXY] failed to clean empty directories",
							"error", err,
						)
					}
				}

				// Log activity or heartbeat every 6 cycles (6 hours if interval is 1h)
				if removed > 0 {
					slog.Info("[IMAGE-PROXY] cache cleanup completed",
						"entries_removed", removed,
						"cycle", cycleCount,
					)
				} else if cycleCount%6 == 0 {
					// Get cache size for heartbeat log
					size, _ := c.GetCacheSize()
					slog.Debug("[IMAGE-PROXY] cache cleanup heartbeat",
						"cycle", cycleCount,
						"cache_size_bytes", size,
					)
				}
			}
		}
	}()

	return cancel
}
