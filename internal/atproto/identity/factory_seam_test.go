package identity

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
)

// The client-replacing seam on Config, and why it may not be exported.
//
// # WHAT THIS PACKAGE'S GUARD IS WORTH
//
// resolverHTTPClient is the only client a resolver in this package dials
// through, and every constructor routes to it: DefaultConfig fills the field
// after applying options, and NewResolver fills it when a caller left it nil, so
// that `NewResolver(db, Config{PLCURL: ...})` — an ordinary spelling in this
// tree — is guarded rather than accidentally safe. That is a closed set of
// constructions.
//
// An EXPORTED *http.Client field on Config reopens it. Any package in the tree
// can write `identity.Config{HTTPClient: unguarded}` and NewResolver will honour
// it: no option, no coves:allow-ssrf-hatch marker, nothing for the audit to
// grep, and a resolver that fetches whatever a did:web's hostname points at
// with no address check. The hatch this package DOES offer —
// WithPrivateHostsAllowed — is greppable precisely so "which resolvers have the
// guard off" stays an answerable question; an exported field makes the answer
// "unknown".
//
// Every peer package that needed the same test seam made it unexported for this
// reason: jetstream's withWellKnownHTTPClient, blobs' setHTTPClient, the shared
// transport's withTransportOptions. This package was the one that did not.

// TestConfig_ExposesNoClientReplacingField is the fence, and it is stated over
// the TYPE rather than over any particular field name so that it also catches
// the next one somebody adds.
//
// Reflection is the only way to assert this: the property is "no package outside
// this one can substitute a client", and a test written in this package can
// reach the unexported field regardless. What reflection can see is exactly what
// an outside caller can reach.
func TestConfig_ExposesNoClientReplacingField(t *testing.T) {
	t.Parallel()

	configType := reflect.TypeOf(Config{})
	clientTypes := map[reflect.Type]bool{
		reflect.TypeOf(&http.Client{}):                   true,
		reflect.TypeOf(http.Client{}):                    true,
		reflect.TypeOf((*http.RoundTripper)(nil)).Elem(): true,
	}

	for i := 0; i < configType.NumField(); i++ {
		field := configType.Field(i)
		if !field.IsExported() {
			continue
		}
		assert.Falsef(t, clientTypes[field.Type],
			"Config.%s is an exported %s, so any package in the tree can hand this resolver an "+
				"UNGUARDED client — no option, no coves:allow-ssrf-hatch marker, nothing to grep. This "+
				"package resolves did:web documents, which means it fetches https://<hostname>/.well-known/"+
				"did.json where the hostname is a string a stranger typed; the address guard is the only "+
				"thing between that and a loopback PLC, Postgres or PDS on a dev machine. Every peer "+
				"package made the same seam unexported (withWellKnownHTTPClient, setHTTPClient, "+
				"withTransportOptions) — see withHTTPClient in this package",
			field.Name, field.Type)
	}
}

// TestWithHTTPClient_IsHonouredByEveryConstructor keeps the seam WORKING once it
// is unexported, because a seam that is merely hidden is one the next author
// re-exports.
//
// Both constructions are covered: through DefaultConfig, where the option runs
// before the client is built, and through NewResolver's own nil branch, which is
// what a hand-built Config degrades to.
func TestWithHTTPClient_IsHonouredByEveryConstructor(t *testing.T) {
	t.Parallel()

	// A timeout no default in this package uses, so the assertion cannot be
	// satisfied by the guarded client this option is meant to displace:
	// resolverHTTPClient's is 10s.
	const marker = 3 * time.Second
	require.NotEqual(t, marker, 10*time.Second, "the marker must differ from resolverHTTPClient's own timeout")

	t.Run("through DefaultConfig", func(t *testing.T) {
		t.Parallel()

		config := DefaultConfig(withHTTPClient(&http.Client{Timeout: marker}))
		config.PLCURL = "http://plc.invalid:3002"
		config.CacheTTL = 90 * time.Second

		dir := baseDirectoryOf(t, NewResolver(nil, config))
		assert.Equal(t, marker, dir.HTTPClient.Timeout,
			"the option was applied but the client it names did not reach the directory. DefaultConfig "+
				"fills the client AFTER options run, so an implementation that overwrites unconditionally "+
				"leaves this seam dead and every test using it silently exercising the default client")
		assert.Equal(t, "http://plc.invalid:3002", dir.PLCURL,
			"the explicit configuration around the option must survive it")
	})

	t.Run("through NewResolver", func(t *testing.T) {
		t.Parallel()

		config := Config{PLCURL: "http://plc.invalid:3002"}
		withHTTPClient(&http.Client{Timeout: marker})(&config)

		dir := baseDirectoryOf(t, NewResolver(nil, config))
		assert.Equal(t, marker, dir.HTTPClient.Timeout,
			"NewResolver replaced a client the caller supplied. Its nil branch exists to guard callers "+
				"who said NOTHING about a client; one that fires regardless would also overwrite the seam")
	})
}

// baseDirectoryOf digs out the indigo directory a resolver ended up dialling
// through, which is the only place the client is observable.
func baseDirectoryOf(t *testing.T, resolver Resolver) *indigoIdentity.BaseDirectory {
	t.Helper()

	caching, ok := resolver.(*cachingResolver)
	require.Truef(t, ok, "NewResolver must return the caching resolver these tests drive, got %T", resolver)
	base, ok := caching.base.(*baseResolver)
	require.Truef(t, ok, "the caching resolver must wrap the base resolver, got %T", caching.base)
	dir, ok := base.directory.(*indigoIdentity.BaseDirectory)
	require.Truef(t, ok, "the base resolver must hold an indigo BaseDirectory, got %T", base.directory)
	return dir
}
