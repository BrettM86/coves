package oauth

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PrivateAddressOptions is the gate: the one place in this tree that decides,
// from a boolean a caller is holding, whether a client gets the hatch option or
// nothing at all. Seven production call sites hold such a boolean
// (api.allowPrivateHost, s.allowPrivateHosts, config.AllowPrivateIPs,
// allowPrivateIPs) and each is about to route through here.
//
// # WHY THIS IS A FUNCTION AND NOT AN `if` IN WIRING
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` — the hermetic merge gate,
// T0+T1+T2 — runs the PERMISSIVE branch at every one of those call sites. A
// green merge gate therefore cannot prove that production is guarded: the
// guarded branch is evaluated in exactly one place in this repository, and that
// place is this file.
//
// An inline `if cfg.IsDevEnv { ... }` in wiring would be reachable only by
// standing up wiring with a production config, and nothing in this tree does
// that. Pulled out as a pure function, the decision is testable without wiring,
// without a config, and without an environment — which is the only way the
// production branch gets tested at all.

// TestPrivateAddressOptions_ReturnsZeroOptionsWhenPrivateAddressesAreDisallowed
// is the key production-polarity assertion for this helper.
//
// The claim is not "the returned options are safe". It is that there ARE no
// returned options: length zero, nothing to apply, the constructor's own struct
// literal left untouched. That distinction is what makes the guarantee
// auditable. "Safe options" is a property of whatever the slice happens to hold
// today and has to be re-argued every time the slice changes; "no options" is a
// property a reader can check in one glance and a test can pin exactly.
//
// It is written as an exact length and not as a behavioural check on purpose. A
// later edit that appends a diagnostic option, or that returns a one-element
// slice holding a no-op "explicitly deny" closure, would keep every behavioural
// test in this file green — and would move the production branch from "provably
// applies nothing" to "applies something we believe is harmless", which is a
// different and much weaker claim about the only branch CI never runs.
//
// So: if this assertion is ever in the way, the answer is not to relax it.
func TestPrivateAddressOptions_ReturnsZeroOptionsWhenPrivateAddressesAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivateAddressOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivateAddressOptions(false) returned %d option(s). The production branch — the one "+
			"IS_DEV_ENV=true keeps `make ci` from ever evaluating — must contribute NOTHING to the "+
			"constructor, so that what production gets is exactly the constructor's own defaults. An "+
			"option here that is believed harmless is not the same guarantee as no option at all, and "+
			"nothing downstream of this line would catch the difference", len(opts))
}

// TestPrivateAddressOptions_DisallowedClientIsGuarded is the behavioural half of
// the assertion above: zero options has to also MEAN a guarded client.
//
// The length check alone would still pass if the constructor's defaults ever
// regressed to permissive — the helper would be returning nothing, correctly,
// onto a base that no longer refuses anything. Both halves together say: the
// helper adds nothing, and nothing is what keeps the guard on.
//
// Every row of privateHatchGates runs, because allowPrivate gates three separate
// refusals in RoundTrip (the IP-literal check, the zoned-literal check inside it,
// and the classification pass over the resolved answers). A helper that somehow
// re-opened one of the three would be invisible to a single-row test.
func TestPrivateAddressOptions_DisallowedClientIsGuarded(t *testing.T) {
	t.Parallel()

	for _, tt := range privateHatchGates {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probe := newHatchProbe(t, tt.hostname, tt.resolves, PrivateAddressOptions(false)...)

			resp, err := probe.client.Get("http://" + tt.urlHost + "/")
			if err == nil {
				_ = resp.Body.Close()
			}

			require.Errorf(t, err,
				"GET http://%s/ succeeded on a client built from PrivateAddressOptions(false). %s",
				tt.urlHost, tt.why)
			assert.ErrorIsf(t, err, ErrBlockedAddress,
				"the refusal must be the guard's, matchable by identity: a request that failed for some "+
					"other reason is not the same control and would not hold in production; got: %v", err)

			assert.Zerof(t, probe.invocations.Load(),
				"the listener was reached %d times for http://%s/. This probe's dialler sends every "+
					"connection to that one server whatever address it was handed, so any invocation means "+
					"the packet left the transport — and for a destination a stranger named, the packet "+
					"leaving IS the SSRF, whatever error came back afterwards",
				probe.invocations.Load(), tt.urlHost)
		})
	}
}

// TestPrivateAddressOptions_AllowedClientOpensEveryGate pins the other
// direction, and pins it through OBSERVED BEHAVIOUR rather than through the
// shape of the returned slice.
//
// A length check here would be worthless: `[]Option{WithMaxResponseBytes(1024)}`
// has length one just as `[]Option{WithPrivateAddressesAllowed()}` does, and a
// helper that returned the wrong option would satisfy it while leaving every dev
// environment and every httptest fixture in this tree unable to reach loopback.
// So the assertion is that a client built from these options actually reaches a
// listener at an address the guard would otherwise refuse.
//
// All three rows again, for the same reason as the guarded direction: the hatch
// has to open all three gates, or the migrated call sites lose part of their dev
// hatch in a way that only shows up on a developer's machine.
func TestPrivateAddressOptions_AllowedClientOpensEveryGate(t *testing.T) {
	t.Parallel()

	for _, tt := range privateHatchGates {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probe := newHatchProbe(t, tt.hostname, tt.resolves, PrivateAddressOptions(true)...)

			resp, err := probe.client.Get("http://" + tt.urlHost + "/")
			if err == nil {
				defer func() { _ = resp.Body.Close() }()
			}

			require.NoErrorf(t, err,
				"GET http://%s/ was refused on a client built from PrivateAddressOptions(true). The "+
					"permissive branch is what every developer and every fixture in this tree runs, so a "+
					"helper that returns the wrong option — or none — breaks local development everywhere "+
					"at once — %s", tt.urlHost, tt.why)
			assert.Equalf(t, http.StatusOK, resp.StatusCode,
				"GET http://%s/ reached the listener but did not complete", tt.urlHost)

			assert.Equalf(t, int64(1), probe.invocations.Load(),
				"the listener was reached %d times for http://%s/. Every connection this probe makes "+
					"lands there regardless of destination, so anything but exactly one means the request "+
					"never got out of the transport", probe.invocations.Load(), tt.urlHost)
		})
	}
}

// TestPrivateAddressOptions_SpreadsAlongsideOtherOptions pins that the helper's
// result is usable the way call sites will actually use it — spread into a
// constructor that is also passing settings of its own — and that neither
// setting eats the other.
//
// This is not hypothetical composition. The image proxy has an
// operator-configurable size limit (IMAGE_PROXY_MAX_SOURCE_SIZE_MB) that has to
// arrive as WithMaxResponseBytes, and it is one of the call sites holding an
// allow-private boolean. It will pass both. Options are applied by iterating a
// slice of closures over one transport struct, so an implementation that
// returned a whole constructor-worth of options — rather than only the ones its
// own concern owns — would silently reset the cap to the package default and the
// operator's setting would vanish with no error anywhere.
//
// Both effects are asserted in one request: the listener is reached (so the
// hatch is open at an address the guard would otherwise refuse) AND the read
// fails with ErrResponseTooLarge (so the caller's cap, not the 32 MiB default,
// is what bounded it).
func TestPrivateAddressOptions_SpreadsAlongsideOtherOptions(t *testing.T) {
	t.Parallel()

	// Comfortably under the body below and far under DefaultMaxResponseBytes, so
	// a failure to apply this option cannot be mistaken for the default doing the
	// work: at the default, 4 KiB is not remarkable.
	const capBytes = 64
	body := bytes.Repeat([]byte("a"), 4096)

	// The private row: the hatch has to be open for this request to leave the
	// transport at all, so reaching the listener is itself the evidence that
	// PrivateAddressOptions(true) survived being spread next to another option.
	const (
		hostname = "private.test"
		resolves = "127.0.0.1"
	)

	opts := append(PrivateAddressOptions(true), WithMaxResponseBytes(capBytes))
	probe := newHatchProbeWithBody(t, hostname, resolves, body, opts...)

	resp, err := probe.client.Get("http://" + hostname + "/")
	if err == nil {
		// The cap has two enforcement points — a declared Content-Length larger
		// than the allowance is refused in RoundTrip, and anything else is caught
		// by the body wrapper mid-read — and which one fires depends on whether
		// the test server chose to declare a length. Reading through covers both
		// without pinning an implementation detail neither the caller nor this
		// test has an opinion about.
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	assert.Equalf(t, int64(1), probe.invocations.Load(),
		"the listener was reached %d times. The address is loopback, so the request only leaves the "+
			"transport with the hatch open: anything but exactly one means WithMaxResponseBytes "+
			"displaced the option PrivateAddressOptions(true) contributed",
		probe.invocations.Load())

	require.ErrorIsf(t, err, ErrResponseTooLarge,
		"a %d-byte body came back whole under a %d-byte cap. The caller's cap was passed alongside "+
			"PrivateAddressOptions(true) and has to survive it — an image proxy whose operator raises "+
			"IMAGE_PROXY_MAX_SOURCE_SIZE_MB, or lowers it, gets no error when the setting is silently "+
			"replaced by the package default; got: %v", len(body), capBytes, err)
}
