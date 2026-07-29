//go:build live

// Package live holds the opt-in tier of tests that deliberately reach the
// public internet: real Bluesky handles and posts, the production PLC
// directory, and third-party unfurl targets (Streamable, YouTube, Reddit,
// Wikipedia, Kagi Kite).
//
// Everything here is excluded from the merge gate by the `live` build tag.
// `make ci` runs an egress-blocked stack (docker-compose.ci.yml declares its
// network `internal: true`), so these tests could not pass there even if they
// were compiled in — which is the point. Reality checks against third parties
// belong in a run someone chooses to make, not in the gate that decides whether
// a change merges.
//
// Run them with:
//
//	make test-live
//
// They need the test database on localhost:5434 (`make dev-up` provides it) and
// working internet access. When the network is unavailable these tests are
// expected to fail rather than pass quietly.
package live

import (
	"fmt"
)

// generateTestDID builds a valid did:plc-shaped string for fixtures that never
// need PLC registration.
func generateTestDID(suffix string) string {
	return fmt.Sprintf("did:plc:test%s", suffix)
}
