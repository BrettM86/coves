package jetstream

import (
	"Coves/internal/core/bridgedvotes"
	"log"
	"time"
)

// maxBridgedCount aliases the shared ceiling both ingestion channels enforce on a
// bridge-asserted count; see bridgedvotes.MaxBridgedCount for the rationale and
// the tidepool constant it chases.
const maxBridgedCount = bridgedvotes.MaxBridgedCount

// BridgeTrust is the provenance gate for bridge-asserted vote aggregates
// (bridgedStats). bridgedStats let a record declare origin-platform vote counts that
// the consumers fold into the displayed counts, denormalized score, and hot-rank.
// Because posts live in community repos and comments live in user repos, ANY native
// repo could otherwise self-assert bridgedStats{upvotes: 10^9} and inflate its own
// ranking at zero cost. BridgeTrust default-denies that: bridgedStats are honoured
// only when the repo that carried them is hosted on a configured trusted bridge PDS.
//
// Why PDS-host allowlist (and not a DID allowlist or live resolution):
//   - Coves already resolves and persists each repo's PDS host at index time
//     (users.pds_url for a commenter, communities.pds_url for a post's community),
//     derived from identity resolution. The gate reuses that stored value, so it needs
//     NO new live PLC/identity lookups on the hot Jetstream path.
//   - A host allowlist is O(number of bridge instances) config, not O(number of
//     bridged actors) — the bridge mints a new DID per federated community/user, so a
//     DID allowlist would be unmaintainable. Trusting the bridge's PDS host(s) trusts
//     exactly the infrastructure the operator controls.
//   - The set is operator config (TRUSTED_BRIDGE_PDS_HOSTS), mirroring the existing
//     COMMUNITY_CREATORS allowlist convention (comma-separated env var).
//
// Trade-off / trust assumption (documented deliberately): this trusts that the stored
// pds_url faithfully reflects where the repo is hosted. A hostile actor who could get
// their repo hosted on (or spoof) the bridge PDS host would be trusted — but that
// requires compromising the operator's own bridge infrastructure, which is outside the
// self-assertion threat this gate closes. An empty allowlist (no bridge configured)
// means bridgedStats are universally ignored, which is the safe default.
type BridgeTrust struct {
	// hosts holds normalized (scheme+host, lowercased) trusted PDS host keys.
	hosts map[string]struct{}
}

// NewBridgeTrust builds a provenance gate from a list of trusted bridge PDS host URLs
// (typically parsed from the TRUSTED_BRIDGE_PDS_HOSTS env var). Blank/garbage entries
// are dropped. A gate with no usable hosts trusts nothing (default-deny).
func NewBridgeTrust(pdsHosts []string) *BridgeTrust {
	m := make(map[string]struct{}, len(pdsHosts))
	for _, h := range pdsHosts {
		if n := normalizePDSHost(h); n != "" {
			m[n] = struct{}{}
		}
	}
	return &BridgeTrust{hosts: m}
}

// TrustsPDS reports whether pdsURL is a configured trusted bridge PDS host.
// Default-deny: a nil gate, an empty allowlist, or an empty/unparseable pdsURL all
// return false so bridgedStats are ignored unless provenance is affirmatively proven.
func (b *BridgeTrust) TrustsPDS(pdsURL string) bool {
	if b == nil || len(b.hosts) == 0 {
		return false
	}
	n := normalizePDSHost(pdsURL)
	if n == "" {
		return false
	}
	_, ok := b.hosts[n]
	return ok
}

// normalizePDSHost delegates to bridgedvotes.NormalizeHost, the shared trust and
// polling comparison rule. It lowercases scheme+host, removes matching default
// HTTP(S) ports and tolerantly normalizes schemeless stored values.
func normalizePDSHost(raw string) string {
	return bridgedvotes.NormalizeHost(raw)
}

// validatedBridgedStats applies input hygiene to a bridgedStats aggregate and parses
// its asOf timestamp. It returns ok=false — meaning the caller must ignore the WHOLE
// aggregate — when either count is negative, either count exceeds maxBridgedCount, or
// asOf is unparseable. Treating asOf as part of the atomic trio (fix for the create
// path) ensures counts are never applied with a NULL/unknown asOf, which would defeat
// the regression guard. This hygiene runs regardless of provenance; the trust gate is
// a separate, prior check the caller performs first.
func validatedBridgedStats(stats *BridgedStatsFromJetstream, uri string) (up, down int, asOf time.Time, ok bool) {
	if stats.Upvotes < 0 || stats.Downvotes < 0 {
		log.Printf("Warning: ignoring bridgedStats with negative counts for %s (up=%d down=%d)",
			uri, stats.Upvotes, stats.Downvotes)
		return 0, 0, time.Time{}, false
	}
	if stats.Upvotes > maxBridgedCount || stats.Downvotes > maxBridgedCount {
		log.Printf("Warning: ignoring bridgedStats exceeding cap %d for %s (up=%d down=%d)",
			maxBridgedCount, uri, stats.Upvotes, stats.Downvotes)
		return 0, 0, time.Time{}, false
	}
	t, err := parseBridgedAsOf(stats.AsOf, uri)
	if err != nil {
		// parseBridgedAsOf already logs the parse failure.
		return 0, 0, time.Time{}, false
	}
	return stats.Upvotes, stats.Downvotes, t, true
}
