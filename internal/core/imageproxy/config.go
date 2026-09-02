package imageproxy

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config validation errors
var (
	// ErrInvalidCacheMaxGB is returned when CacheMaxGB is not positive
	ErrInvalidCacheMaxGB = errors.New("CacheMaxGB must be positive")
	// ErrInvalidFetchTimeout is returned when FetchTimeout is not positive
	ErrInvalidFetchTimeout = errors.New("FetchTimeout must be positive")
	// ErrInvalidMaxSourceSize is returned when MaxSourceSizeMB is not positive
	ErrInvalidMaxSourceSize = errors.New("MaxSourceSizeMB must be positive")
	// ErrInvalidMaxSourceMegapixels is returned when MaxSourceMegapixels is not positive
	ErrInvalidMaxSourceMegapixels = errors.New("MaxSourceMegapixels must be positive")
	// ErrInvalidMaxConcurrentProcesses is returned when MaxConcurrentProcesses is not positive
	ErrInvalidMaxConcurrentProcesses = errors.New("MaxConcurrentProcesses must be positive")
	// ErrInvalidProcessQueueWait is returned when ProcessQueueWait is not positive
	ErrInvalidProcessQueueWait = errors.New("ProcessQueueWait must be positive")
	// ErrInvalidMaxInFlightRequests is returned when MaxInFlightRequests is not positive
	ErrInvalidMaxInFlightRequests = errors.New("MaxInFlightRequests must be positive")
	// ErrMissingCachePath is returned when CachePath is empty while Enabled is true
	ErrMissingCachePath = errors.New("CachePath is required when proxy is enabled")
	// ErrInvalidCacheTTL is returned when CacheTTLDays is negative
	ErrInvalidCacheTTL = errors.New("CacheTTLDays cannot be negative")
)

// Processing budget defaults. DefaultMaxSourceMegapixels lives next to the
// processor that enforces it; these two bound the service around it.
const (
	// DefaultMaxConcurrentProcesses caps how many decodes run at once so a
	// burst of worst-case images cannot multiply the per-request memory cost
	// past what the host can absorb.
	DefaultMaxConcurrentProcesses = 4

	// DefaultProcessQueueWait is how long a request may wait for a free
	// processing slot before the service refuses it as busy.
	DefaultProcessQueueWait = 5 * time.Second

	// DefaultMaxInFlightRequests caps how many cache-miss requests may be
	// past the cache check at once; see Config.MaxInFlightRequests.
	DefaultMaxInFlightRequests = 64
)

// Config holds the configuration for the image proxy service.
type Config struct {
	// Enabled determines whether the image proxy service is active.
	Enabled bool

	// BaseURL is the origin/domain for the image proxy service (e.g., "https://coves.social").
	// Empty string generates relative URLs (e.g., "/img/avatar/plain/did/cid").
	// The /img path prefix is added automatically by the URL generation function.
	BaseURL string

	// CachePath is the filesystem path where cached images are stored.
	CachePath string

	// CacheMaxGB is the maximum cache size in gigabytes.
	CacheMaxGB int

	// CacheTTLDays is the maximum age in days for cached entries.
	// Entries older than this are eligible for cleanup regardless of cache size.
	// Set to 0 to disable TTL-based cleanup (only LRU eviction applies).
	CacheTTLDays int

	// CleanupInterval is how often to run cache cleanup (TTL + LRU eviction).
	// Set to 0 to disable background cleanup.
	CleanupInterval time.Duration

	// CDNURL is the optional CDN URL prefix for serving cached images.
	CDNURL string

	// FetchTimeout is the maximum time allowed for fetching images from PDS.
	FetchTimeout time.Duration

	// MaxSourceSizeMB is the maximum allowed size for source images in megabytes.
	MaxSourceSizeMB int

	// MaxSourceMegapixels is the maximum pixel count (width × height, in
	// millions) a source image may declare before the processor refuses to
	// decode it.
	MaxSourceMegapixels int

	// MaxConcurrentProcesses is the number of image decodes allowed in flight
	// at once across the whole service.
	MaxConcurrentProcesses int

	// ProcessQueueWait is the longest a request waits for a processing slot
	// before it is refused.
	ProcessQueueWait time.Duration

	// MaxInFlightRequests caps the cold, cache-miss requests admitted past the
	// cache check at once. Each one holds up to MaxSourceSizeMB of fetched
	// blob while it waits for a decode slot, so without this cap the queue in
	// front of the decode semaphore is itself an unbounded memory sink. Excess
	// requests are refused as busy.
	MaxInFlightRequests int
}

// Validate checks the configuration for invalid values.
// Returns nil if the configuration is valid, or an error describing the problem.
// When Enabled is false, only numeric constraints are validated (for safety).
// When Enabled is true, all required fields must be set.
func (c Config) Validate() error {
	// Always validate numeric constraints regardless of enabled state
	if c.CacheMaxGB <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidCacheMaxGB, c.CacheMaxGB)
	}
	if c.FetchTimeout <= 0 {
		return fmt.Errorf("%w: got %v", ErrInvalidFetchTimeout, c.FetchTimeout)
	}
	if c.MaxSourceSizeMB <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidMaxSourceSize, c.MaxSourceSizeMB)
	}
	// The ceiling is checked here as well as in NewProcessor so a bad env value
	// is refused at boot rather than when the first image arrives.
	if c.MaxSourceMegapixels <= 0 || c.MaxSourceMegapixels > MaxSourceMegapixelsCeiling {
		return fmt.Errorf("%w: got %d, want 1..%d",
			ErrInvalidMaxSourceMegapixels, c.MaxSourceMegapixels, MaxSourceMegapixelsCeiling)
	}
	if c.MaxConcurrentProcesses <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidMaxConcurrentProcesses, c.MaxConcurrentProcesses)
	}
	if c.ProcessQueueWait <= 0 {
		return fmt.Errorf("%w: got %v", ErrInvalidProcessQueueWait, c.ProcessQueueWait)
	}
	if c.MaxInFlightRequests <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidMaxInFlightRequests, c.MaxInFlightRequests)
	}
	if c.CacheTTLDays < 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidCacheTTL, c.CacheTTLDays)
	}

	// When enabled, validate required fields
	if c.Enabled {
		if c.CachePath == "" {
			return ErrMissingCachePath
		}
		// BaseURL can be empty for relative URLs
	}

	return nil
}

// DefaultConfig returns a Config with sensible default values.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		BaseURL:         "",
		CachePath:       "/var/cache/coves/images",
		CacheMaxGB:      10,
		CacheTTLDays:    30,
		CleanupInterval: 1 * time.Hour,
		CDNURL:          "",
		FetchTimeout:    30 * time.Second,
		MaxSourceSizeMB: 10,

		MaxSourceMegapixels:    DefaultMaxSourceMegapixels,
		MaxConcurrentProcesses: DefaultMaxConcurrentProcesses,
		ProcessQueueWait:       DefaultProcessQueueWait,
		MaxInFlightRequests:    DefaultMaxInFlightRequests,
	}
}

// ConfigFromEnv creates a Config from environment variables.
// Uses defaults for any missing environment variables.
//
// Environment variables:
//   - IMAGE_PROXY_ENABLED: "true"/"1" to enable, "false"/"0" to disable (default: true)
//   - IMAGE_PROXY_BASE_URL: origin URL for image proxy (default: "" for relative URLs)
//   - IMAGE_PROXY_CACHE_PATH: filesystem cache path (default: "/var/cache/coves/images")
//   - IMAGE_PROXY_CACHE_MAX_GB: max cache size in GB (default: 10)
//   - IMAGE_PROXY_CACHE_TTL_DAYS: max age for cache entries in days, 0 to disable (default: 30)
//   - IMAGE_PROXY_CLEANUP_INTERVAL_MINUTES: cleanup job interval in minutes, 0 to disable (default: 60)
//   - IMAGE_PROXY_CDN_URL: optional CDN URL prefix (default: "")
//   - IMAGE_PROXY_FETCH_TIMEOUT_SECONDS: PDS fetch timeout in seconds (default: 30)
//   - IMAGE_PROXY_MAX_SOURCE_SIZE_MB: max source image size in MB (default: 10)
//   - IMAGE_PROXY_MAX_SOURCE_MEGAPIXELS: max declared source pixel count in megapixels (default: 50)
//   - IMAGE_PROXY_MAX_CONCURRENT_PROCESSES: max image decodes in flight at once (default: 4)
//   - IMAGE_PROXY_MAX_IN_FLIGHT_REQUESTS: max cache-miss requests admitted at once (default: 64).
//     It bounds how many fetched blobs can be held in memory waiting for a decode slot.
//
// ProcessQueueWait is deliberately not configurable: it is coupled to the
// handler's Retry-After and to how long a waiter may hold a fetched blob.
func ConfigFromEnv() Config {
	cfg := DefaultConfig()

	if v := os.Getenv("IMAGE_PROXY_ENABLED"); v != "" {
		cfg.Enabled = v == "true" || v == "1"
	}

	if v := os.Getenv("IMAGE_PROXY_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}

	if v := os.Getenv("IMAGE_PROXY_CACHE_PATH"); v != "" {
		cfg.CachePath = v
	}

	if v := os.Getenv("IMAGE_PROXY_CACHE_MAX_GB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.CacheMaxGB = n
		} else {
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_CACHE_MAX_GB value, using default",
				"value", v,
				"default", cfg.CacheMaxGB,
				"error", err,
			)
		}
	}

	if v := os.Getenv("IMAGE_PROXY_CACHE_TTL_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.CacheTTLDays = n
		} else {
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_CACHE_TTL_DAYS value, using default",
				"value", v,
				"default", cfg.CacheTTLDays,
				"error", err,
			)
		}
	}

	if v := os.Getenv("IMAGE_PROXY_CLEANUP_INTERVAL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.CleanupInterval = time.Duration(n) * time.Minute
		} else {
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_CLEANUP_INTERVAL_MINUTES value, using default",
				"value", v,
				"default_minutes", int(cfg.CleanupInterval.Minutes()),
				"error", err,
			)
		}
	}

	if v := os.Getenv("IMAGE_PROXY_CDN_URL"); v != "" {
		cfg.CDNURL = v
	}

	if v := os.Getenv("IMAGE_PROXY_FETCH_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.FetchTimeout = time.Duration(n) * time.Second
		} else {
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_FETCH_TIMEOUT_SECONDS value, using default",
				"value", v,
				"default_seconds", int(cfg.FetchTimeout.Seconds()),
				"error", err,
			)
		}
	}

	if v := os.Getenv("IMAGE_PROXY_MAX_SOURCE_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxSourceSizeMB = n
		} else {
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_MAX_SOURCE_SIZE_MB value, using default",
				"value", v,
				"default", cfg.MaxSourceSizeMB,
				"error", err,
			)
		}
	}

	if v := os.Getenv("IMAGE_PROXY_MAX_SOURCE_MEGAPIXELS"); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_MAX_SOURCE_MEGAPIXELS value, using default",
				"value", v,
				"default", cfg.MaxSourceMegapixels,
				"error", err,
			)
		case n <= 0:
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_MAX_SOURCE_MEGAPIXELS value, using default",
				"value", v,
				"default", cfg.MaxSourceMegapixels,
				"reason", "must be a positive integer",
			)
		default:
			cfg.MaxSourceMegapixels = n
		}
	}

	if v := os.Getenv("IMAGE_PROXY_MAX_CONCURRENT_PROCESSES"); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_MAX_CONCURRENT_PROCESSES value, using default",
				"value", v,
				"default", cfg.MaxConcurrentProcesses,
				"error", err,
			)
		case n <= 0:
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_MAX_CONCURRENT_PROCESSES value, using default",
				"value", v,
				"default", cfg.MaxConcurrentProcesses,
				"reason", "must be a positive integer",
			)
		default:
			cfg.MaxConcurrentProcesses = n
		}
	}

	if v := os.Getenv("IMAGE_PROXY_MAX_IN_FLIGHT_REQUESTS"); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_MAX_IN_FLIGHT_REQUESTS value, using default",
				"value", v,
				"default", cfg.MaxInFlightRequests,
				"error", err,
			)
		case n <= 0:
			slog.Warn("[IMAGE-PROXY] invalid IMAGE_PROXY_MAX_IN_FLIGHT_REQUESTS value, using default",
				"value", v,
				"default", cfg.MaxInFlightRequests,
				"reason", "must be a positive integer",
			)
		default:
			cfg.MaxInFlightRequests = n
		}
	}

	return cfg
}
