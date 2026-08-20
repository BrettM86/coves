package main

import (
	"testing"

	"Coves/internal/config"

	"github.com/stretchr/testify/assert"
)

// The single expression that decides whether this tool's SSRF guard is on.
//
// The cutover tool is the one binary in the tree that DELETES production posts,
// and every outbound call it makes is aimed at an address that came out of the
// database: a community's pds_url column, an author repo's serviceEndpoint. Five
// separate expressions used to spell out whether the guard covering those dials
// was on, and no test evaluated any of them — the same defect cmd/server carried
// at thirteen sites. `.env.ci:140` sets IS_DEV_ENV=true, so the merge gate ran
// the permissive branch at all five.

// TestAllowPrivateHosts_IsOffUnderAProductionConfig is the binding contract.
// The assert.False is the assertion that fails on inversion.
//
// IS_DEV_ENV is set to the dev value on purpose: the gate must be a property of
// the parsed configuration, never an ambient read.
func TestAllowPrivateHosts_IsOffUnderAProductionConfig(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	t.Setenv("IS_DEV_ENV", "true") // the guard must not be ambient

	assert.False(t, allowPrivateHosts(&config.Config{IsDevEnv: false}),
		"the SSRF hatch is OPEN under a production config. With this one expression inverted, the "+
			"blob fetch and PDS upload, both community PDS client paths, the blob copy the "+
			"Rematerializer makes from an author repo's serviceEndpoint, and the OAuth client all "+
			"dial private addresses during a run that irreversibly deletes user data — and `make ci` "+
			"cannot see it, because .env.ci:140 sets IS_DEV_ENV=true and runs the other branch")
}

// TestAllowPrivateHosts_IsOnUnderADevConfig is the falsifiability twin: an
// accessor hardcoded to false passes the test above while making the tool
// unusable against a local stack, whose PDS is on loopback.
func TestAllowPrivateHosts_IsOnUnderADevConfig(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	t.Setenv("IS_DEV_ENV", "false") // the hatch must not be ambient either

	assert.True(t, allowPrivateHosts(&config.Config{IsDevEnv: true}),
		"the SSRF hatch is SHUT under a dev config, so a rehearsal run against a local stack cannot "+
			"reach its own PDS — which is on loopback, exactly the address class the guard refuses. "+
			"-dry-run is the only way this tool is ever practised before it deletes anything")
}
