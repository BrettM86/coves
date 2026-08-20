package oauth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// privateHatchGates are the three distinct behaviours the private-address hatch
// controls. ONE TABLE, shared by the option test and the guarded-by-default
// fence below, so the two directions cannot drift: a spelling the hatch opens
// but the default does not close is a hole, and a spelling the default closes
// but the hatch does not open is an outage in every dev environment and every
// httptest fixture in this tree.
//
// They are three behaviours and not three views of one check, which is the
// reason a named option is worth more here than at any other setting on this
// transport. `allowPrivate` gates a literal refusal, a zoned-literal refusal AND
// a classification pass, in three separate places in RoundTrip — so
// `NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed())` is one unlabelled positional bool standing for
// three decisions, none of which its spelling names.
//
// Rows 2 and 3 carry addresses that classify as PUBLIC, which is what makes
// them independent of row 1. Classification would wave both
// through; only the literal check refuses them. If they used loopback instead,
// all three rows would be one behaviour wearing three spellings and the table
// would prove a third of what it claims.
var privateHatchGates = []struct {
	name string
	// urlHost is what goes in the URL: brackets and the RFC 6874 `%25` zone
	// escape included, because that is what an attacker sends and what url.Parse
	// is written against.
	urlHost string
	// hostname is what url.Hostname() yields from urlHost, which is what the
	// guard actually inspects — and for the zoned row the two differ in more than
	// punctuation.
	hostname string
	// resolves is the answer the fake resolver gives for hostname. For the two
	// literal rows it is the literal itself, which is what a real resolver does
	// for a literal — so a missing literal check does not merely fail the row, it
	// lets the request proceed exactly as it would in production.
	resolves string
	why      string
}{
	{
		name:     "a private address reached through a hostname",
		urlHost:  "private.test",
		hostname: "private.test",
		resolves: "127.0.0.1",
		why: "the classification pass: a name a stranger chose, resolving to an address inside this host. " +
			"This is the behaviour the hatch exists for — every httptest fixture in this tree is served " +
			"from loopback",
	},
	{
		name:     "an IP literal written where a hostname belongs",
		urlHost:  "8.8.8.8",
		hostname: "8.8.8.8",
		resolves: "8.8.8.8",
		why: "the literal refusal, which is a SEPARATE gate on the same boolean: this address classifies as " +
			"public, so classification cannot account for either direction of this row",
	},
	{
		name:     "a zoned IPv6 literal",
		urlHost:  "[2600::1%25eth0]",
		hostname: "2600::1%eth0",
		resolves: "2600::1",
		why: "the zoned-literal refusal, the spelling net.ParseIP misses and netip.ParseAddr catches. Also " +
			"public space, so again only the literal check moves this row — and a zoned " +
			"address is the one form an operator legitimately reaches for with the hatch open, since " +
			"fe80::1%en0 needs its interface to be reachable at all",
	},
}

// hatchProbe is a client every one of whose connections lands on a single
// loopback listener, whatever address it thinks it is dialling.
//
// That is what makes both directions observable from one fixture. "The request
// proceeded" is a 200 from a real server rather than the absence of an error,
// and "the request was refused" is a listener with zero invocations rather than
// a failure that might have come from the network — and since the dialler
// ignores the destination entirely, an invocation count of zero cannot be
// explained by an address that merely happened to be unreachable.
//
// Substituting the base transport discards the dials-only-vetted-addresses
// property for the duration of these tests, which is safe only because
// TestSSRFTransport_DialsOnlyTheAddressItVetted owns that property outright.
// What it buys is that no packet can leave the machine even in the cases where
// the guard is expected NOT to refuse.
type hatchProbe struct {
	client      *http.Client
	resolver    *hostRoutedResolver
	invocations *atomic.Int64
}

func newHatchProbe(t *testing.T, hostname, resolves string, opts ...Option) *hatchProbe {
	t.Helper()

	return newHatchProbeWithBody(t, hostname, resolves, nil, opts...)
}

// newHatchProbeWithBody is the same probe with a reply body, for the callers
// that need the response to be big enough to trip a byte cap. The empty-body
// form above is the common case and stays the one most tests read.
func newHatchProbeWithBody(t *testing.T, hostname, resolves string, body []byte, opts ...Option) *hatchProbe {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) returns false, so a typo'd fixture
	// would classify as public and the row would pass or fail for reasons
	// unconnected to its subject.
	answer := net.ParseIP(resolves)
	require.NotNil(t, answer, "the test's own answer %q must parse as an IP address", resolves)

	var invocations atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		invocations.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	resolver := &hostRoutedResolver{answers: map[string][]net.IP{hostname: {answer}}}

	// The boolean stays false in every construction here. The option is the only
	// thing that may open the hatch, which is the whole subject of this file.
	client := NewSSRFSafeHTTPClient(opts...)
	transport, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "NewSSRFSafeHTTPClient must install an ssrfSafeTransport, got %T", client.Transport)
	transport.lookupIP = resolver.lookup

	serverAddr := server.Listener.Addr().String()
	dialer := &net.Dialer{}
	transport.base = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, serverAddr)
		},
	}

	return &hatchProbe{client: client, resolver: resolver, invocations: &invocations}
}

// TestWithPrivateAddressesAllowed_OpensEveryGateTheBooleanOpens pins that the
// named option is equivalent to `allowPrivate = true`, at all three of the
// places that boolean is read.
//
// # WHY A NAMED OPTION AND NOT THE BOOLEAN
//
// `NewSSRFSafeHTTPClient(WithPrivateAddressesAllowed())` reads as nothing at a call site. The reader has
// to open this package to learn that the argument is the difference between a
// guarded client and an unguarded one, and nine call sites are about to be
// written against it. The byte ceiling already got a self-documenting name
// (WithMaxResponseBytes); the setting that disables the guard ENTIRELY did not,
// which is exactly backwards — the more dangerous switch is the one wearing no
// label.
//
// It also has to be greppable. The regression fence needs a way to find
// "this client has the hatch open", and `true` is not a thing you can grep for.
func TestWithPrivateAddressesAllowed_OpensEveryGateTheBooleanOpens(t *testing.T) {
	t.Parallel()

	for _, tt := range privateHatchGates {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probe := newHatchProbe(t, tt.hostname, tt.resolves, WithPrivateAddressesAllowed())

			resp, err := probe.client.Get("http://" + tt.urlHost + "/")
			if err == nil {
				defer func() { _ = resp.Body.Close() }()
			}

			require.NoErrorf(t, err,
				"GET http://%s/ was refused with WithPrivateAddressesAllowed() passed. The option has to open "+
					"the same gate the boolean opens, or the seven call sites migrating onto it silently lose "+
					"their dev hatch — %s", tt.urlHost, tt.why)
			assert.Equalf(t, http.StatusOK, resp.StatusCode,
				"GET http://%s/ reached the listener but did not complete", tt.urlHost)

			assert.Equalf(t, int64(1), probe.invocations.Load(),
				"the listener was reached %d times for http://%s/. Every connection this probe makes lands "+
					"there regardless of destination, so anything but exactly one means the request never got "+
					"out of the transport", probe.invocations.Load(), tt.urlHost)
		})
	}
}

// TestNewSSRFSafeHTTPClient_IsGuardedWithoutTheHatchOption is the other half of
// the equivalence, and it is the assertion `make ci` can never make.
//
// `.env.ci:140` sets `IS_DEV_ENV=true`, so every call site about to be built on
// this option runs its PERMISSIVE branch under the merge gate. The guarded
// branch — the one production runs, the one the whole remediation is for — is
// exercised nowhere in CI except here. T0 is not one tier among several for this
// property; it is the only tier that has it.
//
// The two constructions are both needed. "No options at all" is the default
// every un-migrated caller gets and the state a new caller falls into by
// omission. "An unrelated option" is the leak test: options are applied by
// iterating a slice of closures over one struct, so an implementation that
// opened the hatch from the wrong closure — or from the constructor, before the
// options run — would pass the first case and fail the second.
func TestNewSSRFSafeHTTPClient_IsGuardedWithoutTheHatchOption(t *testing.T) {
	t.Parallel()

	constructions := []struct {
		name string
		opts []Option
	}{
		{name: "no options at all", opts: nil},
		{name: "an unrelated option", opts: []Option{WithMaxResponseBytes(1024)}},
	}

	for _, construction := range constructions {
		t.Run(construction.name, func(t *testing.T) {
			t.Parallel()

			for _, tt := range privateHatchGates {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()

					probe := newHatchProbe(t, tt.hostname, tt.resolves, construction.opts...)

					resp, err := probe.client.Get("http://" + tt.urlHost + "/")
					if err == nil {
						_ = resp.Body.Close()
					}

					require.Errorf(t, err,
						"GET http://%s/ succeeded on a client built with %s. %s",
						tt.urlHost, construction.name, tt.why)
					assert.ErrorIsf(t, err, ErrBlockedAddress,
						"the refusal must be the guard's, matchable by identity: a request that failed for some "+
							"other reason is not the same control and would not hold in production; got: %v", err)

					assert.Zerof(t, probe.invocations.Load(),
						"the listener was reached %d times for http://%s/. The dialler here sends every "+
							"connection to that one server whatever address it was handed, so any invocation "+
							"means the packet left the transport — and for a destination a stranger named, the "+
							"packet leaving IS the SSRF, whatever error came back afterwards",
						probe.invocations.Load(), tt.urlHost)
				})
			}
		})
	}
}
