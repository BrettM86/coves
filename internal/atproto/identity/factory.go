package identity

import (
	"database/sql"
	"net/http"
	"os"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
)

// Config holds configuration for the identity resolver
type Config struct {
	PLCURL   string
	CacheTTL time.Duration

	// httpClient is the client every resolver built from this config dials
	// through. UNEXPORTED, because an exported one is a guard-replacing seam
	// with nothing left to grep for: `identity.Config{HTTPClient: unguarded}`
	// from any package in the tree, honoured by NewResolver, with no option and
	// no coves:allow-ssrf-hatch marker. withHTTPClient is the way in, and
	// factory_seam_test.go is the fence. See withHTTPClient.
	httpClient *http.Client

	// allowPrivateHosts disables the SSRF guard that refuses private, loopback
	// and link-local addresses on the client this config builds.
	//
	// IT IS PER-CONSTRUCTION, AND THAT IS THE WHOLE DESIGN. cmd/server builds
	// two resolvers from this same function with OPPOSITE requirements:
	// a.identityResolver points at a PLC that in dev is on loopback and needs
	// the hatch there, while productionPLCResolver is always aimed at
	// plc.directory and must stay guarded in every environment — dev most of
	// all, since a dev machine is exactly where the loopback it would otherwise
	// dial is a real PLC, a real Postgres and a real PDS.
	//
	// So this must never become an ambient read of IS_DEV_ENV inside this
	// package. An ambient hatch satisfies every obvious test and silently
	// unguards the production resolver;
	// TestTwoResolvers_OppositeRequirementsInTheSameProcess is the one
	// assertion that notices.
	allowPrivateHosts bool
}

// ConfigOption configures a resolver Config.
type ConfigOption func(*Config)

// WithPrivateHostsAllowed disables the SSRF address guard on the resolver's
// HTTP client.
//
// THE NAME IS THE CONTRACT: only a caller that knows it is pointing this
// resolver at its own machine may use it. It is greppable, which is how "which
// resolvers have the guard off" stays an answerable question.
func WithPrivateHostsAllowed() ConfigOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(c *Config) { c.allowPrivateHosts = true }
}

// withHTTPClient substitutes the client a resolver dials through.
//
// # IT IS UNEXPORTED, AND THAT IS THE POINT
//
// This replaces the guard wholesale — not the address policy, which
// WithPrivateHostsAllowed carries and which is greppable so that "which
// resolvers have the guard off" stays an answerable question, but the entire
// vetted transport. There is no policy left to grep for afterwards, so the only
// safe number of packages able to do it is one: this one.
//
// It exists because a guard test that builds its own client proves only that
// internal/atproto/oauth works. The seam has to point at the client production
// actually constructs, and every peer package that needed the same thing kept it
// unexported for the same reason — jetstream's withWellKnownHTTPClient, blobs'
// setHTTPClient, the shared transport's withTransportOptions.
//
// It was an EXPORTED FIELD on Config until factory_seam_test.go removed it:
// `identity.Config{HTTPClient: unguarded}` from any package in the tree, honoured
// by NewResolver, with no option and no marker.
func withHTTPClient(client *http.Client) ConfigOption {
	return func(c *Config) { c.httpClient = client }
}

// PrivateHostOptions returns the options a caller holding an allow-private
// boolean should pass to DefaultConfig: the hatch when it is set, and NOTHING
// when it is not.
//
// It mirrors oauth.PrivateAddressOptions, and it is a function rather than an
// `if` in cmd/server/wiring.go for the reason documented there: `.env.ci:140`
// sets IS_DEV_ENV=true, so `make ci` takes the PERMISSIVE branch at every call
// site holding such a boolean. A unit test against this function is the only
// place in the repository where the branch production actually runs is ever
// evaluated. Do not inline it back.
//
// FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT — not "options that are
// safe", but none, so that what production gets is exactly DefaultConfig's own
// defaults.
func PrivateHostOptions(allowPrivate bool) []ConfigOption {
	if !allowPrivate {
		return nil
	}
	return []ConfigOption{WithPrivateHostsAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

// resolverHTTPClient builds the client every resolver in this package dials
// through: the SSRF-safe transport of internal/atproto/oauth, which resolves
// the host, refuses private, loopback and link-local addresses, and then dials
// only the address it vetted — closing the check-then-dial window a naive guard
// leaves open.
//
// THIS PACKAGE IS REACHED WITH NO CREDENTIAL. The /img route resolves a DID
// before it fetches anything, and a did:web carries its own hostname — the
// resolver fetches https://<hostname>/.well-known/did.json, so the destination
// is a string in the URL a stranger typed. Indigo's AllowedTLD check refuses
// eight reserved TLDs — local, arpa, invalid, localhost, internal, example,
// onion and alt — which is a list of NAMES and stops nothing else: a public
// hostname with a 127.0.0.1 A record has a perfectly ordinary TLD, and the TLD
// is the only thing that check looks at.
//
// The 10s timeout is this package's own and predates the guard; the shared
// client ships a 15s ceiling, so it is re-applied here rather than inherited.
// factory_test.go pins it with the reason — an unbounded client here hangs an
// ingestion worker on an unresponsive PDS.
func resolverHTTPClient(allowPrivateHosts bool) *http.Client {
	client := covesoauth.NewSSRFSafeHTTPClient(covesoauth.PrivateAddressOptions(allowPrivateHosts)...)
	client.Timeout = 10 * time.Second
	return client
}

// DefaultConfig returns a configuration with sensible defaults.
//
// PLCURL honors PLC_DIRECTORY_URL when set, falling back to the production
// directory otherwise. This matters for tests: the Makefile exports .env.dev
// (PLC_DIRECTORY_URL=http://localhost:3002), so every resolver built from
// DefaultConfig resolves against the local PLC instead of quietly issuing
// lookups against production for DIDs that only exist locally.
//
// Callers that deliberately need the production directory - resolving real
// Bluesky handles, for instance - must set PLCURL explicitly after calling
// this, as cmd/server does for its read-only Bluesky resolver.
//
// THE CLIENT IT BUILDS IS GUARDED UNLESS THE CALLER SAYS OTHERWISE, so that
// forgetting is safe: DefaultConfig() with no arguments is what the next caller
// will write, and what productionPLCResolver already writes.
func DefaultConfig(opts ...ConfigOption) Config {
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "https://plc.directory"
	}
	config := Config{
		PLCURL:   plcURL,
		CacheTTL: 24 * time.Hour, // Cache for 24 hours
	}
	for _, opt := range opts {
		opt(&config)
	}
	// After the options, because the hatch one of them may have set is what this
	// client is built from — and only when none of them supplied a client, so
	// that withHTTPClient's substitution is not immediately overwritten. The two
	// options are exclusive in practice: nothing wants a substituted client and
	// an address policy applied to a client it is not using.
	if config.httpClient == nil {
		config.httpClient = resolverHTTPClient(config.allowPrivateHosts)
	}
	return config
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
	// GUARDED ON THE SAME TERMS AS DefaultConfig's. `NewResolver(db,
	// Config{PLCURL: ...})` is an ordinary spelling in this tree, and a caller
	// who writes it has said nothing about private hosts — so they get the
	// guard, and the safety of a resolver does not depend on which of two
	// equally ordinary constructions its author happened to reach for. A
	// caller that does want the hatch sets it through DefaultConfig's option,
	// which fills HTTPClient and skips this branch entirely.
	if config.httpClient == nil {
		config.httpClient = resolverHTTPClient(config.allowPrivateHosts)
	}

	// Create base resolver using Indigo
	base := newBaseResolver(config.PLCURL, config.httpClient)

	// Wrap with caching using PostgreSQL
	cache := NewPostgresCache(db, config.CacheTTL)
	caching := newCachingResolver(base, cache)

	// Future: could add rate limiting here if needed
	// if config.MaxConcurrent > 0 {
	//     return newRateLimitedResolver(caching, config.MaxConcurrent)
	// }

	return caching
}
