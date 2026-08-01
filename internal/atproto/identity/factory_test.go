package identity

import (
	"net/http"
	"testing"
	"time"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// How a resolver gets built, and which PLC directory it ends up pointing at.
//
// This is a small amount of code guarding a large mistake. Every resolver in
// the AppView comes from one of these two functions, and the one field that
// matters most — PLCURL — has a production default. A resolver that silently
// falls back to https://plc.directory does not fail; it succeeds against the
// wrong network, resolving DIDs that only exist locally as absent and leaking
// which local DIDs we asked about. The hermetic test suite's egress block would
// catch it in CI, which is exactly why the behaviour deserves a test that says
// what is supposed to happen rather than relying on a network to refuse.
//
// These tests do not run in parallel with each other: DefaultConfig reads a
// process-wide environment variable, and t.Setenv is incompatible with
// t.Parallel by design.

func TestDefaultConfig_PrefersTheConfiguredPLCDirectory(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "http://plc-directory.invalid:3002")

	config := DefaultConfig()

	assert.Equal(t, "http://plc-directory.invalid:3002", config.PLCURL,
		"the Makefile exports .env.dev, so this is how every locally-built resolver reaches the "+
			"stack's own PLC instead of the public one")
	assert.Equal(t, 24*time.Hour, config.CacheTTL)
	require.NotNil(t, config.HTTPClient)
	assert.Equal(t, 10*time.Second, config.HTTPClient.Timeout,
		"an unbounded client here hangs an ingestion worker on an unresponsive PDS")
}

func TestDefaultConfig_FallsBackToTheProductionDirectory(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "")

	config := DefaultConfig()

	// Pinned deliberately. This default is correct for production and wrong for
	// every test, which is why the fallback is stated here rather than left
	// implicit: a reader who changes it needs to see that cmd/server's
	// read-only Bluesky resolver depends on being able to ask for it.
	assert.Equal(t, "https://plc.directory", config.PLCURL) // coves:allow-public-host: asserting the documented production default, not an endpoint any test dials
}

func TestNewResolver_FillsInMissingConfiguration(t *testing.T) {
	t.Parallel()

	// A zero Config is what a caller writes when they only mean to say "use the
	// defaults", and it must not produce a resolver with a zero TTL (every
	// entry expired on write) or a nil HTTP client (a panic on first use).
	resolver := NewResolver(nil, Config{})
	require.NotNil(t, resolver)

	caching, ok := resolver.(*cachingResolver)
	require.Truef(t, ok, "NewResolver must return the caching wrapper, not the bare base resolver: "+
		"callers rely on Purge clearing the identity_cache table")

	base, ok := caching.base.(*baseResolver)
	require.True(t, ok)
	dir, ok := base.directory.(*indigoIdentity.BaseDirectory)
	require.True(t, ok)
	assert.Equal(t, "https://plc.directory", dir.PLCURL) // coves:allow-public-host: asserting the documented production default, not an endpoint any test dials
	assert.Equal(t, 10*time.Second, dir.HTTPClient.Timeout,
		"a nil HTTPClient in the config must be replaced before it reaches Indigo, which dereferences it")

	cache, ok := caching.cache.(*postgresCache)
	require.True(t, ok)
	assert.Equal(t, 24*time.Hour, cache.ttl,
		"a zero TTL would expire every entry the moment it was written, turning the cache into pure "+
			"overhead — and silently, because reads would still work")
}

func TestNewResolver_HonoursAnExplicitConfiguration(t *testing.T) {
	t.Parallel()

	resolver := NewResolver(nil, Config{
		PLCURL:     "http://plc.invalid:3002",
		CacheTTL:   90 * time.Second,
		HTTPClient: &http.Client{Timeout: 3 * time.Second},
	})

	caching := resolver.(*cachingResolver)
	dir := caching.base.(*baseResolver).directory.(*indigoIdentity.BaseDirectory)
	assert.Equal(t, "http://plc.invalid:3002", dir.PLCURL)
	assert.Equal(t, 3*time.Second, dir.HTTPClient.Timeout)
	assert.Equal(t, 90*time.Second, caching.cache.(*postgresCache).ttl)
}
