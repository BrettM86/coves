package jetstream

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// This file owns the multi-feed Jetstream configuration. The AppView consumes
// N Jetstream endpoints ("feeds") carrying overlapping repos — typically the
// public bsky.network Jetstream (kept for third-party PDS records and
// redundancy) plus the self-hosted relay+Jetstream pair that crawls our own
// PDSes without bsky.network's per-host quotas. Every consumer runs once per
// feed; rev-gating (rev_gate.go) makes the overlap safe.
//
// Configuration is one env var:
//
//	JETSTREAM_FEEDS="bsky=wss://jetstream2.us-east.bsky.network;self=ws://tidepool-prod-jetstream:8080"
//
// Each entry is <feedKey>=<baseURL>. The base URL carries no query string; a
// path is optional (a trailing /subscribe is tolerated). The per-consumer
// collection filters live in consumerWantedCollections (exposed via
// WantedCollections) so adding feed N+1 is pure config and the filters exist
// in exactly one place.

// Feed is one upstream Jetstream endpoint.
type Feed struct {
	Key     string // short name used in consumer names and logs, e.g. "bsky", "self"
	BaseURL string // ws(s)://host[:port], optionally with a path; no query
}

// PrimaryFeedKey is the feed whose consumers keep the bare legacy names
// ("users", "posts", ...) so their persisted cursors and dead letters carry
// over from the single-feed era. Every other feed's consumers are named
// "<consumer>@<feedKey>".
const PrimaryFeedKey = "bsky"

// feedKeyPattern keeps feed keys safe for use inside consumer names (which key
// cursor and dead-letter rows) and log lines.
var feedKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// consumerWantedCollections maps each canonical consumer name to the record
// collections it indexes. The wiring loop appends these as wantedCollections
// query parameters to every feed's subscribe URL. Unexported on purpose: a
// direct map lookup with a mistyped consumer name would silently yield nil,
// and a filterless subscribe URL means consuming the ENTIRE firehose. Use
// WantedCollections, which fails closed on unknown names.
//
// NOTE: social.coves.community.block is listed for the communities consumer —
// its handler has always supported block records, but the old hand-written
// COMMUNITY_JETSTREAM_URL never subscribed to the collection, so firehose
// block events silently never arrived. Deriving URLs from this table fixes
// that class of drift.
var consumerWantedCollections = map[string][]string{
	ConsumerUsers: {
		CovesProfileCollection,
		"social.coves.actor.block",
	},
	ConsumerCommunities: {
		"social.coves.community.profile",
		"social.coves.community.subscription",
		"social.coves.community.block",
	},
	// One consumer for all four post-related collections, because they decide
	// about each other: an acceptance is meaningless without the postv2 it
	// pins, and both write the same admission row. Splitting them across
	// connectors would give the two halves independent cursors and independent
	// dead letters for one conversation.
	//
	// social.coves.community.post stays subscribed even though it is DEPRECATED
	// (§3.0): the records already written to community repos keep arriving, and
	// dropping the filter would silently stop indexing edits and deletes of
	// every post that exists today.
	ConsumerPosts: {
		"social.coves.community.post",
		"social.coves.community.postv2",
		"social.coves.community.acceptance",
		"social.coves.community.removal",
	},
	ConsumerAggregators: {
		"social.coves.aggregator.service",
		"social.coves.aggregator.authorization",
	},
	ConsumerVotes: {
		"social.coves.feed.vote",
	},
	ConsumerComments: {
		"social.coves.community.comment",
	},
}

// WantedCollections returns a copy of the record collections the named
// canonical consumer indexes, for use as wantedCollections filters on a feed's
// subscribe URL. Unknown consumer names return an error rather than an empty
// slice, because an unfiltered subscribe URL would consume the entire firehose.
func WantedCollections(consumer string) ([]string, error) {
	collections, ok := consumerWantedCollections[consumer]
	if !ok {
		return nil, fmt.Errorf("unknown Jetstream consumer %q: no wantedCollections defined (an unfiltered URL would subscribe to the whole firehose)", consumer)
	}
	return append([]string(nil), collections...), nil
}

// ConsumedCollections returns every record collection the AppView indexes,
// mapped to the consumers that index it (sorted, deduplicated).
//
// This is the generated contract inventory of docs/TEST_ARCHITECTURE.md §3.4a.
// cmd/contract-manifest walks it and fails the build when a consumed collection
// has no pipeline contract in tests/e2e — so "every collection the AppView
// ingests is proven end to end" is a compile-time-ish invariant rather than a
// review habit. Reading the same map the subscribe URLs are built from is the
// whole point: a hand-maintained second list was wrong the day it was written
// (it missed both block collections and collapsed the two aggregator types).
//
// The map is rebuilt per call and its slices are copies, so a caller cannot
// reach back into the consumer topology.
func ConsumedCollections() map[string][]string {
	byCollection := make(map[string][]string)
	for consumer, collections := range consumerWantedCollections {
		for _, collection := range collections {
			byCollection[collection] = append(byCollection[collection], consumer)
		}
	}
	for collection := range byCollection {
		sort.Strings(byCollection[collection])
	}
	return byCollection
}

// ParseFeeds parses a JETSTREAM_FEEDS value into an ordered feed list.
// Format: semicolon-separated <key>=<baseURL> entries. Keys must be unique,
// lowercase alphanumeric (plus hyphens); base URLs must be ws:// or wss://
// and carry no query string (collection filters are code-owned).
func ParseFeeds(spec string) ([]Feed, error) {
	var feeds []Feed
	seen := make(map[string]bool)

	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, base, found := strings.Cut(entry, "=")
		key, base = strings.TrimSpace(key), strings.TrimSpace(base)
		if !found || key == "" || base == "" {
			return nil, fmt.Errorf("invalid feed entry %q (expected <key>=<baseURL>)", entry)
		}
		if !feedKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("invalid feed key %q (lowercase alphanumeric and hyphens only)", key)
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate feed key %q", key)
		}
		seen[key] = true

		parsed, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("invalid feed URL %q: %w", base, err)
		}
		if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
			return nil, fmt.Errorf("feed URL %q must use ws:// or wss://", base)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("feed URL %q is missing a host", base)
		}
		if parsed.RawQuery != "" {
			return nil, fmt.Errorf("feed URL %q must not carry a query string (collection filters are derived per consumer)", base)
		}

		feeds = append(feeds, Feed{Key: key, BaseURL: base})
	}

	if len(feeds) == 0 {
		return nil, fmt.Errorf("no feeds configured (expected e.g. %q)",
			"bsky=wss://jetstream2.us-east.bsky.network;self=ws://tidepool-prod-jetstream:8080")
	}
	return feeds, nil
}

// SubscribeURL builds the full WebSocket subscribe URL for one consumer on one
// feed: the base URL with a /subscribe path (appended unless already present)
// and one wantedCollections parameter per collection.
func SubscribeURL(baseURL string, collections []string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid feed URL %q: %w", baseURL, err)
	}
	if !strings.HasSuffix(parsed.Path, "/subscribe") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/subscribe"
	}
	query := parsed.Query()
	for _, collection := range collections {
		query.Add("wantedCollections", collection)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// FeedConsumerName returns the connector name — the key for the persisted
// cursor and dead-letter rows — for a consumer on a feed. The primary feed
// keeps the bare legacy name so live cursors carry over untouched; all other
// feeds get "<consumer>@<feedKey>". A brand-new name starts with no cursor
// row, i.e. the consumer live-tails from now — a newly added feed starts with
// no history, and recovering records it never delivered requires the source
// PDSes to re-emit them through the relay (Tidepool's POST /admin/reemit
// rewrites records with fresh revs so they pass the rev gate).
func FeedConsumerName(consumer, feedKey string) string {
	if feedKey == PrimaryFeedKey {
		return consumer
	}
	return consumer + "@" + feedKey
}
