// Package communitysignals delivers community NSFW flag change alerts and the
// weekly NSFW share digest to operators.
//
// The community repository records a flag event in the same transaction that
// indexes a community whose contentWarnings gains or loses "nsfw". This
// package sends each event after commit, retries failed sends with a sweep,
// and once a week sends the communities whose share of nsfw self-labelled
// posts rose. It takes no moderation action.
package communitysignals

import (
	"context"
	"errors"
	"time"
)

// ErrLeaseLost means an event's lease expired and another claim took it, or
// it was delivered, before the claim's outcome was recorded. The outcome is
// not recorded.
var ErrLeaseLost = errors.New("community flag event lease lost")

// ErrorCodeSendFailed is the last_error_code of an event whose send failed.
// The Telegram adapter scrubs its errors, so no finer class is reliable.
const ErrorCodeSendFailed = "send_failed"

// Logged error codes. Logs carry these and event IDs only, never a community
// DID or an underlying error's text.
const (
	// errorCodeLeaseLost: the claim's lease expired before its outcome was
	// recorded.
	errorCodeLeaseLost = "lease_lost"
	// errorCodeClaimFailed: claiming a due event failed.
	errorCodeClaimFailed = "claim_failed"
	// errorCodeRecordFailed: recording a claimed event's outcome failed.
	errorCodeRecordFailed = "record_failed"
)

// The errors Sweep returns. They are fixed, so a returned error never carries
// repository text that could name a community.
var (
	errClaimFailed  = errors.New("community signal: claiming a due flag event failed")
	errRecordFailed = errors.New("community signal: recording a flag event outcome failed")
	// The errors RunWeeklyDigest returns, fixed for the same reason.
	errDigestMarkerReadFailed  = errors.New("community signal: reading the digest marker failed")
	errDigestRowsFailed        = errors.New("community signal: querying the digest rows failed")
	errDigestSendFailed        = errors.New("community signal: sending the digest failed")
	errDigestMarkerWriteFailed = errors.New("community signal: persisting the digest marker failed")
)

// Event is one recorded change of a community's nsfw flag.
type Event struct {
	ID              int64
	CommunityDID    string
	CommunityHandle string // joined from communities at read time
	OriginHost      string // "" for a native community
	OldNSFW         bool   // false for a community first indexed with nsfw
	NewNSFW         bool
	OccurredAt      time.Time
	DeliveredAt     *time.Time
	Attempts        int
	LastErrorCode   string
	NextAttemptAt   time.Time
}

// DigestRow is one community in the weekly NSFW share digest.
type DigestRow struct {
	CommunityDID    string
	CommunityHandle string
	OriginHost      string  // "" for a native community
	RecentPosts     int     // posts in [now-7d, now)
	RecentShare     float64 // fraction of RecentPosts with an nsfw self-label
	PriorShare      float64 // the same over [now-35d, now-7d); 0 with no prior posts
}

// RisePoints is the absolute rise of the nsfw share, in percentage points.
func (r DigestRow) RisePoints() float64 { return (r.RecentShare - r.PriorShare) * 100 }

// Repository stores flag change events and the digest marker.
type Repository interface {
	// ClaimDueEvent claims the undelivered event below attemptLimit whose next
	// attempt is at or before now, oldest due first; communityDID "" means any
	// community. The claimed event's next attempt moves to leaseUntil, so no
	// other claim returns it until then; an event another claim is taking at
	// the same moment is skipped. The returned event carries its attempts
	// before the claim and NextAttemptAt = leaseUntil as stored, which is the
	// lease the Mark methods match. ok is false when nothing is due.
	ClaimDueEvent(ctx context.Context, communityDID string, now, leaseUntil time.Time, attemptLimit int) (event Event, ok bool, err error)
	// MarkEventDelivered records the delivery of a claimed event. It returns
	// ErrLeaseLost when the event no longer holds the claim's lease.
	MarkEventDelivered(ctx context.Context, id int64, lease, at time.Time) error
	// MarkEventFailed increments the attempt count and records the failure of
	// a claimed event. It returns ErrLeaseLost when the event no longer holds
	// the claim's lease.
	MarkEventFailed(ctx context.Context, id int64, lease time.Time, errorCode string, nextAttemptAt time.Time) error
	// DigestRows returns the communities with at least minPosts posts in the
	// week before now whose nsfw share rose by more than riseThresholdPoints,
	// ordered by rise descending.
	DigestRows(ctx context.Context, now time.Time, minPosts int, riseThresholdPoints float64) ([]DigestRow, error)
	// DigestLastSent returns the slot of the last digest run, if any.
	DigestLastSent(ctx context.Context) (time.Time, bool, error)
	SetDigestLastSent(ctx context.Context, slot time.Time) error
}
