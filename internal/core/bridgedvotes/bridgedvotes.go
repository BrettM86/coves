// Package bridgedvotes polls the Tidepool bridge's vote-aggregate side channel
// (social.coves.bridge.getVoteAggregates) for content in bridged communities and
// folds the returned fediverse tallies into the bridged_* columns on posts and
// comments. It exists because the record-stamp channel (bridgedStats via
// Jetstream) cannot reach native-authored content: the bridge cannot write into
// a native user's own PDS repo, so without this poller votes cast on Lemmy
// against native posts/comments are aggregated by the bridge and then displayed
// by nobody.
package bridgedvotes

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// MaxBridgedCount is the largest per-direction count either ingestion
	// channel accepts from a bridge. Anything above it is malformed or hostile:
	// it is far larger than any plausible origin-platform score and is the shape
	// a score-inflation attack takes. Keep it in sync with tidepool's
	// MaxSeededCount; the Jetstream adapter aliases this constant rather than
	// carrying its own copy.
	MaxBridgedCount = 1_000_000

	// MaxAsOfSkew bounds how far ahead of this AppView's clock a bridge's asOf
	// stamp may run. A stamp past that is a clock fault or a hostile value, and
	// accepting it would install a bridged_stats_as_of that every later honest
	// aggregate loses the >= guard against, in both ingestion channels, until a
	// repair migration ran. Five minutes tolerates ordinary NTP drift.
	MaxAsOfSkew = 5 * time.Minute
)

// ErrMissingAsOf is returned by a Store asked to apply an aggregate whose AsOf
// is the zero time. Counts and their sampling instant are one atomic trio, so
// an unstamped tally is a caller bug rather than a benign no-op.
var ErrMissingAsOf = errors.New("bridged vote aggregate has no asOf")

// Aggregate is one subject's fediverse-only vote tally as served by the bridge.
type Aggregate struct {
	URI       string
	Upvotes   int
	Downvotes int
	AsOf      time.Time
}

// Candidate is one pollable subject: its at-uri and the community's stored PDS
// URL it was matched under. StoredPDSURL is a MATCH KEY, never a dial target.
// The dial target is always a TrustedHost, which a stored string cannot become
// without passing ParseTrustedHost, the same validation operator config does.
type Candidate struct {
	URI          string
	StoredPDSURL string
}

// Store is the persistence seam the poller sweeps through.
type Store interface {
	// SelectCandidates returns pollable subjects (posts and comments) in
	// communities whose pds_url is one of storedHosts, non-deleted, created
	// within lookback, ordered bridged_polled_at ASC NULLS FIRST, capped at limit.
	// Comments qualify only through an indexed, non-deleted root post — that
	// join is their sole community source, so a deleted or unindexed root
	// excludes its comments.
	SelectCandidates(ctx context.Context, storedHosts []string, lookback time.Duration, limit int) ([]Candidate, error)
	// DistinctCommunityPDSURLs returns every distinct non-empty communities.pds_url.
	DistinctCommunityPDSURLs(ctx context.Context) ([]string, error)
	// ApplyAggregate folds one aggregate into its row under the asOf >= guard,
	// recomputing score in the same statement. Deleted/absent rows are a no-op
	// success; a zero AsOf is ErrMissingAsOf.
	ApplyAggregate(ctx context.Context, agg Aggregate) error
	// MarkPolled advances bridged_polled_at for every named uri (posts and comments).
	MarkPolled(ctx context.Context, uris []string) error
}

// Options tunes a Poller.
type Options struct {
	// Lookback bounds candidate selection by created_at. Non-positive takes the default.
	Lookback time.Duration
	// SweepCap bounds subjects selected per sweep for a single matched host.
	// With several matched hosts each receives max(SweepCap/hosts, 100), so the
	// per-sweep total can exceed SweepCap by up to 100 per host when the cap is
	// small. Non-positive takes the default.
	SweepCap int
}

// Report counts what one Sweep did. It exists so the job can log a working
// poller, an idle one, and a misconfigured one differently: a sweep that
// returns nil and nothing else looks identical whether it folded a thousand
// tallies or matched no community at all.
type Report struct {
	// TrustedHosts is how many operator-configured bridge hosts the poller holds.
	TrustedHosts int
	// StoredHosts is how many distinct community PDS URLs the store reported.
	StoredHosts int
	// MatchedHosts is how many trusted hosts at least one stored URL matched.
	MatchedHosts int
	// Candidates is how many subjects were selected for polling.
	Candidates int
	// Fetched is how many aggregates the bridges returned and the client accepted.
	Fetched int
	// Applied is how many accepted aggregates the store was asked to fold.
	Applied int
	// Marked is how many subjects advanced their poll watermark.
	Marked int
	// PoisonMarked is how many of Marked advanced because their batch failed
	// permanently or exhausted its transient-failure allowance, not because
	// it was fetched.
	PoisonMarked int
	// FailedHosts is how many matched hosts ended the sweep with a fetch or
	// selection failure.
	FailedHosts int
}

// ParseAsOf parses a bridge's RFC 3339 asOf stamp and applies the hygiene both
// ingestion channels share: the zero time is rejected (time.Parse accepts
// "0001-01-01T00:00:00Z", and a zero stamp would defeat the >= guard) and so
// is a stamp more than MaxAsOfSkew ahead of now.
func ParseAsOf(raw string, now time.Time) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	if t.IsZero() {
		return time.Time{}, errors.New("asOf is the zero time")
	}
	if t.After(now.Add(MaxAsOfSkew)) {
		return time.Time{}, fmt.Errorf("asOf %s is more than %s ahead of the AppView clock", t.UTC().Format(time.RFC3339), MaxAsOfSkew)
	}
	return t, nil
}
