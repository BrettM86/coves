package identity

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// wellKnownPath is the second leg of atProto handle verification: a GET here,
// answered with the DID the handle belongs to.
const wellKnownPath = "/.well-known/atproto-did"

// wellKnownRewriteTransport redirects the HTTP leg of handle verification for
// the handle suffixes it was given, and leaves every other request alone.
//
// # WHY A TRANSPORT AND NOT A RESOLVER SETTING
//
// The address to dial and the name to ask about have to come apart, and the
// RoundTripper is the only place they are still separable. Indigo builds
// https://<handle>/.well-known/atproto-did and hands it to a client; by the time
// anything downstream sees it, the handle is doing double duty as the
// destination. Rewriting the URL's host while moving the handle into req.Host
// sends the request to a PDS we operate and still asks that PDS about the
// handle — which is the only question it can answer, since it serves this path
// per handle keyed on the Host header.
//
// # WHY THIS IS NOT A BYPASS
//
// It changes WHERE the question is asked, never WHETHER the answer is checked.
// The DID that comes back still goes through indigo's comparison against the DID
// being looked up, so a PDS answering about somebody else — a handle reassigned,
// a machine in our own network misconfigured or compromised — fails verification
// exactly as it would over the public internet. That comparison is what makes
// this a real bidirectional verification rather than a decision to trust the
// PDS, and well_known_hosts_test.go asserts it directly.
//
// The rewrite is also SCOPED. Only a handle under a configured suffix is
// touched; anything else reaches the base transport unmodified and resolves the
// way it always did. An implementation that sent every well-known fetch to the
// single configured host would satisfy the happy path and quietly make the
// override a global one.
type wellKnownRewriteTransport struct {
	// base is the SSRF-safe transport this WRAPS rather than replaces. The
	// rewritten address is still dialled through the guard — it reaches a
	// loopback PDS only because WithPrivateHostsAllowed was passed alongside,
	// which is the gate the constructors enforce.
	base http.RoundTripper

	// routes are the configured suffixes, sorted LONGEST FIRST, so the match
	// below is deterministic and takes the most specific entry.
	routes []wellKnownRoute
}

// wellKnownRoute is one configured suffix and the host:port that answers for it.
type wellKnownRoute struct {
	suffix string // already lowercased by WithWellKnownHosts
	host   string
}

// newWellKnownRewriteTransport wraps base and orders the routes so that lookup
// is both deterministic and specific-first.
//
// SORTED, because a map would not be. The suffixes can overlap — `.test` and
// `.pds2.test` both match alice.pds2.test — and Go randomises map iteration
// order deliberately, so a first-match-wins walk over a map picks a host afresh
// on every request. That is the worst failure available here: not wrong, but
// wrong sometimes, with handle verification succeeding and failing alternately
// for one identity and the difference vanishing under any attempt to reproduce
// it. Config parsing refuses overlapping suffixes, but the option is exported and
// anything in-process may call it with any map.
//
// LONGEST FIRST is the right rule and not merely a deterministic one: a more
// specific suffix is a deliberate carve-out from a broader one — the only reason
// to configure both — so it is the operator's actual intent for the names it
// covers.
//
// FAILS CLOSED on a nil base rather than reaching for http.DefaultTransport.
// Nil here means the client this is wrapping carries no guard, and quietly
// substituting the unguarded default would turn "wrap the SSRF-safe transport"
// into "there isn't one" at the exact spot that dials an operator-named address.
// resolverHTTPClient always supplies one, so this is unreachable in practice and
// is the assertion that keeps it so.
func newWellKnownRewriteTransport(base http.RoundTripper, hosts map[string]string) *wellKnownRewriteTransport {
	if base == nil {
		panic("identity: WithWellKnownHosts needs a guarded transport to wrap, and the client supplied none")
	}

	routes := make([]wellKnownRoute, 0, len(hosts))
	for suffix, host := range hosts {
		routes = append(routes, wellKnownRoute{suffix: suffix, host: host})
	}
	// The suffix tiebreak keeps the order total, so two suffixes of equal length
	// still sort the same way every time rather than in map order.
	sort.Slice(routes, func(i, j int) bool {
		if len(routes[i].suffix) != len(routes[j].suffix) {
			return len(routes[i].suffix) > len(routes[j].suffix)
		}
		return routes[i].suffix < routes[j].suffix
	})

	return &wellKnownRewriteTransport{base: base, routes: routes}
}

// RoundTrip sends a mapped handle's verification fetch to the host configured for
// it, in that handle's own name, and passes every other request through.
func (t *wellKnownRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, ok := t.target(req)
	if !ok {
		return t.base.RoundTrip(req) // coves:allow-bare-client: base IS the guarded transport, delegated to unchanged
	}

	// RoundTrip must not mutate the request it is handed, so the rewrite lands
	// on a clone. The handle moves to Host, which is what the client puts in the
	// Host header and therefore what the PDS keys its answer on.
	rewritten := req.Clone(req.Context())
	handle := req.URL.Hostname()
	rewritten.Host = handle
	rewritten.URL.Scheme = "http"
	rewritten.URL.Host = target

	resp, err := t.base.RoundTrip(rewritten) // coves:allow-bare-client: base IS the guarded transport, and it vets the rewritten address too
	if err != nil {
		// SAID OUT LOUD, once, because nothing downstream will say it. Indigo
		// collapses any failure of this leg into the handle.invalid placeholder,
		// so a mistyped port in HANDLE_WELL_KNOWN_HOSTS presents as "handles do
		// not verify in this environment" with no mention of the address that
		// was dialled or the handle it was dialled for. Logging is affordable
		// here precisely because this path exists only in dev and CI: no
		// configured hosts, no rewrite, no line.
		err = fmt.Errorf("well-known verification of %q redirected to %s: %w", handle, target, err)
		log.Printf("WARNING: %v", err)
		return nil, err
	}
	return resp, nil
}

// target reports the host:port to dial instead, and whether this request is one
// of ours at all.
//
// The path and scheme are both part of the match, not just the suffix: this must
// redirect handle verification and nothing else, and a resolver reaches plenty of
// other hosts — the PLC directory, a did:web document, a PDS endpoint read out of
// a DID document — that may perfectly well sit under a mapped suffix.
func (t *wellKnownRewriteTransport) target(req *http.Request) (string, bool) {
	if req.URL == nil || req.URL.Scheme != "https" || req.URL.Path != wellKnownPath {
		return "", false
	}

	// Lowered because DNS names are case-insensitive and the routes were lowered
	// once at construction; the first match wins and the routes are ordered
	// longest-first, so that is the most specific one.
	handle := strings.ToLower(req.URL.Hostname())
	for _, route := range t.routes {
		if strings.HasSuffix(handle, route.suffix) {
			return route.host, true
		}
	}
	return "", false
}

// requireWellKnownHostsGate refuses, at construction, a well-known override that
// was not accompanied by WithPrivateHostsAllowed.
//
// The override's entire purpose is to dial an address inside our own network, so
// a caller who has not declared that this resolver points at their own machine
// has no business enabling it: a resolver that redirects handle verification to
// an operator-named host without that declaration is an SSRF primitive wearing a
// config flag. Production never reaches the combination — cmd/server passes
// PrivateHostOptions(cfg.IsDevEnv) and its false branch returns no options at
// all — so this fires only for a misconfiguration.
//
// It PANICS rather than dropping the option, and rather than returning an error,
// for two reasons. Neither constructor has an error to return; and an override
// silently ignored fails much later and somewhere else, as handles that will not
// verify in an environment somebody believed they had configured, with nothing
// pointing back to the cause. A construction-time misconfiguration that must
// never be recoverable in a running server is what a panic is for.
func requireWellKnownHostsGate(config Config) {
	if len(config.wellKnownHosts) > 0 && !config.allowPrivateHosts {
		panic(fmt.Sprintf("identity: WithWellKnownHosts redirects handle verification to %d operator-named host(s), "+
			"so WithPrivateHostsAllowed is required alongside it", len(config.wellKnownHosts)))
	}
}

// wellKnownSuffixes returns the mapped suffixes, for indigo's
// SkipDNSDomainSuffixes.
//
// Indigo tries DNS BEFORE the well-known fetch, and the stack this option exists
// for has no DNS server at all — so for a mapped suffix that attempt is a wasted
// round trip at best and a hang on every resolution at worst. Skipping it is
// what makes the path local rather than merely redirected.
//
// These are the SAME KEYS the transport routes on, already lowercased by
// WithWellKnownHosts, so the set indigo skips DNS for and the set the rewrite
// fires for cannot diverge. Sorted only for a stable order, since this ends up on
// a struct field a test reads.
func wellKnownSuffixes(hosts map[string]string) []string {
	if len(hosts) == 0 {
		return nil
	}
	suffixes := make([]string, 0, len(hosts))
	for suffix := range hosts {
		suffixes = append(suffixes, suffix)
	}
	sort.Strings(suffixes)
	return suffixes
}
