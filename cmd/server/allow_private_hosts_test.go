package main

import (
	"testing"

	"Coves/internal/config"

	"github.com/stretchr/testify/assert"
)

// The single expression that decides whether this binary's SSRF guard is on.
//
// # WHY THIS FILE EXISTS
//
// Thirteen separate expressions in cmd/server used to spell out the same
// decision — `identity.PrivateHostOptions(a.cfg.IsDevEnv)`,
// `AllowPrivateIPs: a.cfg.IsDevEnv`, `pds.PrivateHostOptions(a.cfg.IsDevEnv)`
// and ten more — and NO TEST ANYWHERE EVALUATED ANY OF THEM. Inverting any one
// of them left `go test ./cmd/server/` green and `make ssrf-audit` at zero,
// because the audit's rule 3 matches only literal hatch spellings
// (`WithPrivateHostsAllowed()`, `PrivateHostOptions(true)`) and a wiring line
// deriving the value from config matches nothing.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` — the hermetic merge gate,
// T0+T1+T2 — takes the PERMISSIVE branch at every one of those sites. A green
// merge gate was therefore compatible with every site being unguarded in
// production. Collapsed into application.allowPrivateHosts, there is one
// polarity to get right and this is the one place that ever evaluates it.
//
// # WHY BOTH DIRECTIONS
//
// The guarded direction is the security property. The hatched direction is what
// keeps the guarded one falsifiable: an accessor hardcoded to `false` satisfies
// every assertion below that matters to production while breaking every
// developer's local stack — a dev-breaking but test-passing mutation that only
// the second test can see.

// TestApplication_AllowPrivateHosts_IsOffUnderAProductionConfig is the binding
// contract, and the assert.False is the assertion that fails on inversion.
//
// The environment is set to the DEV value on purpose. The gate must be a
// property of the parsed configuration and not an ambient read: this same
// process builds productionPLCResolver, which has to stay guarded even in dev,
// so an accessor that consulted os.Getenv would open every construction in the
// binary at once — including the ones that must never open.
func TestApplication_AllowPrivateHosts_IsOffUnderAProductionConfig(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	t.Setenv("IS_DEV_ENV", "true") // the guard must not be ambient

	a := &application{cfg: &config.Config{IsDevEnv: false}}

	assert.False(t, a.allowPrivateHosts(),
		"the SSRF hatch is OPEN under a production config. Every guarded call site in this binary "+
			"derives from this one expression, so with it inverted the image proxy, the identity "+
			"resolver, the unfurl fetch, the blob fetch and upload, both community PDS client paths, "+
			"the profile backfill, the aggregator registration fetch, the community consumer's "+
			".well-known fetch, the direct post fetch, the OAuth client and the service-JWT directory "+
			"all dial private addresses in production — and `make ci` cannot see it, because "+
			".env.ci:140 sets IS_DEV_ENV=true and runs the other branch")
}

// TestApplication_AllowPrivateHosts_IsOnUnderADevConfig is the falsifiability
// twin.
//
// Without it, `func (a *application) allowPrivateHosts() bool { return false }`
// passes the test above. That mutation is not a security bug — it is the
// opposite — but it silently breaks every local stack: the dev PDS, the dev PLC
// and every fixture origin are on loopback, which is precisely what the guard
// refuses. A gate that can only ever be shut is not a gate.
func TestApplication_AllowPrivateHosts_IsOnUnderADevConfig(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	t.Setenv("IS_DEV_ENV", "false") // the hatch must not be ambient either

	a := &application{cfg: &config.Config{IsDevEnv: true}}

	assert.True(t, a.allowPrivateHosts(),
		"the SSRF hatch is SHUT under a dev config, so nothing in a local stack is reachable: the "+
			"dev PDS, the dev PLC and every httptest fixture listen on loopback, which is exactly the "+
			"address class the guard exists to refuse. An accessor that returns false unconditionally "+
			"passes the production test above while breaking every developer's machine")
}
