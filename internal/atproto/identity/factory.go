package identity

import (
	"database/sql"
	"net/http"
	"time"
)

// Config holds configuration for the identity resolver
type Config struct {
	// PLCURL is the URL of the PLC directory (default: https://plc.directory)
	PLCURL string

	// CacheTTL is how long to cache resolved identities
	CacheTTL time.Duration

	// HTTPClient for making HTTP requests (optional, will use default if nil)
	HTTPClient *http.Client
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() Config {
	return Config{
		PLCURL:     "https://plc.directory",
		CacheTTL:   24 * time.Hour, // Cache for 24 hours
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// NewResolver creates a new identity resolver with caching
func NewResolver(db *sql.DB, config Config) Resolver {
	// Apply defaults if not set
	if config.PLCURL == "" {
		config.PLCURL = "https://plc.directory"
	}
	if config.CacheTTL == 0 {
		config.CacheTTL = 24 * time.Hour
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}

	// Create base resolver using Indigo
	base := newBaseResolver(config.PLCURL, config.HTTPClient)

	// Wrap with caching using PostgreSQL
	cache := NewPostgresCache(db, config.CacheTTL)
	caching := newCachingResolver(base, cache)

	// Future: could add rate limiting here if needed
	// if config.MaxConcurrent > 0 {
	//     return newRateLimitedResolver(caching, config.MaxConcurrent)
	// }

	return caching
}
