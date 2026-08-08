package posts

import (
	"context"
	"time"

	"Coves/internal/core/communities"
)

// admitPost: the single decision point for "may this submission become a post
// in this community?" (PRD_AUTHOR_OWNED_POSTS.md §4.1, §5.6, §8).
//
// It is extraction PLUS new policy, and §4.1 is blunt about which is which.
// What CreatePost enforces today is community existence, a private-visibility
// block for regular users, and aggregator authorization + the aggregator's own
// hourly quota — nothing else. The service docstring's "membership/ban
// validation" was aspirational: there is no ban lookup anywhere on the write
// path, and no per-author rate limiting at all. Both arrive here.
//
// WHY A SEPARATE FUNCTION RATHER THAN MORE STEPS IN CreatePost. The same
// decision has to be made from three places: the synchronous local-community
// fast path (§4.2 step 4), the firehose consumer when a post for a community we
// host arrives from someone else's PDS (§5.6), and the notify endpoint (§7).
// A decision that lived inside CreatePost would be reachable only from the
// first, so the other two would silently admit what the first refuses.

// ActorClass is what the CALLER has already established the submitter to be.
//
// It is an INPUT rather than something this decision derives, and that is the
// whole point. Today's classification reads TRUSTED_AGGREGATOR_DIDS (falling
// back to KAGI_AGGREGATOR_DID) out of the process environment inside CreatePost
// (service.go step 3). A decision function that reached for os.Getenv itself
// could not have its trusted-actor branch tested alongside t.Parallel — Go's
// own testing package refuses t.Setenv there — and, worse, would hide "who is
// trusted" from the call site of the security decision it governs.
type ActorClass string

const (
	// ActorUser is a person posting on their own behalf. Every check applies.
	ActorUser ActorClass = "user"

	// ActorRegisteredAggregator is a service the AppView has indexed a
	// social.coves.aggregator.service declaration for. It is held to the
	// community's authorization record and to its OWN hourly quota
	// (aggregators.ValidateAggregatorPost), not to membership or visibility.
	ActorRegisteredAggregator ActorClass = "registered_aggregator"

	// ActorTrustedAggregator is a service named in TRUSTED_AGGREGATOR_DIDS —
	// the temporary env-var mechanism that predates a real authorization
	// endpoint. It skips visibility, ban and authorization checks entirely,
	// which is the existing behaviour and is preserved deliberately.
	ActorTrustedAggregator ActorClass = "trusted_aggregator"
)

// AdmissionRequest is one submission, described in the terms the decision needs
// and no others. There is no record and no blob here: admitPost runs BEFORE any
// of that work, so that a refusal costs a lookup rather than an upload.
type AdmissionRequest struct {
	// Actor is the class the caller resolved. See ActorClass.
	Actor ActorClass

	// AuthorDID is the authenticated author. CreatePost has already proven it
	// matches the DID on the request; this decision trusts that.
	AuthorDID string

	// Community is the at-identifier as the client sent it — a handle
	// (!gardening.communities.coves.social) or a DID. Resolving it is the
	// decision's first step, because a community that does not resolve is the
	// first thing that can refuse a submission.
	Community string

	// Fingerprint identifies WHAT is being submitted: the hash of the canonical
	// record with createdAt removed (see submissionFingerprint). It is the
	// dedupe key, and it must exclude the timestamp or every resubmission of
	// identical content would look new.
	Fingerprint string
}

// AdmissionDecision is the answer: admitted, or refused with a code.
//
// A refusal is a VALUE rather than an error, matching AdmissionOutcome above
// and the project's standing preference for error codes over booleans. The
// caller has to translate the code into whatever its transport speaks — a
// sentinel error for CreatePost, an admissions row for the firehose engine —
// and a refusal returned as an error would push the second of those into the
// dead-letter queue, which is meant to hold genuine failures.
type AdmissionDecision struct {
	// Code is the reason for a refusal, and empty for an admission.
	Code DecisionCode

	// Community is the resolved community, populated on admission so that
	// CreatePost does not fetch it a second time. It is the one piece of state
	// the decision has already paid for that its caller would otherwise re-buy.
	Community *communities.Community

	// Reservation is the ledger row that was inserted for this submission. It
	// is present on admission and must be released if the PDS write that
	// follows fails — see SubmissionLedger.
	Reservation *SubmissionReservation

	// Cause carries the underlying error behind a refusal, when there is one,
	// so the caller can wrap it and keep it matchable.
	//
	// It exists for exactly one case today: aggregator authorization. The API
	// boundary maps that refusal through aggregators.IsUnauthorized and
	// aggregators.IsRateLimited (internal/api/handlers/post/errors.go), which
	// are predicates over the AGGREGATORS package's sentinels. Collapsing that
	// error into a bare DecisionCode would turn a 403 "stop asking" and a 429
	// "ask later" into the same answer, and a well-behaved aggregator would
	// retry a permanent refusal forever.
	Cause error
}

// Admitted reports whether the submission may proceed. There is no separate
// bool field: two representations of one fact drift, and the code is the one
// that has to be right.
func (d AdmissionDecision) Admitted() bool { return d.Code == "" }

// SubmissionReservation identifies the ledger row admitPost inserted for a
// submission, so a caller whose subsequent PDS write failed can release it.
type SubmissionReservation struct {
	ID int64
}

// SubmissionLimits bounds what one author may submit to one community (§8).
//
// Every field is required. There is deliberately no "zero means unlimited"
// reading: a quota that silently disappears when an environment variable is
// missing is not a quota, so config.Validate refuses to start the process with
// any of these unset.
type SubmissionLimits struct {
	// MaxPerAuthorPerCommunity is how many submissions one author may have
	// admitted to one community inside Window.
	MaxPerAuthorPerCommunity int

	// Window is the rolling window the quota is counted over, matching the
	// aggregator limiter's semantics (aggregators.RateLimitWindow): a COUNT of
	// ledger rows newer than now-Window, not a fixed bucket that empties on the
	// hour and lets an author spend twice across the boundary.
	Window time.Duration

	// DedupeWindow is the width of the bucket that scopes dedupe uniqueness.
	// Without it the ledger's unique constraint would forbid an author from
	// ever reposting identical content again, which is a different and much
	// stronger policy than "do not accept the same thing twice right now".
	DedupeWindow time.Duration
}

// Clock is the decision's only source of time.
//
// Injected rather than called directly so that window expiry is testable
// without waiting for one: docs/TEST_ARCHITECTURE.md §3.3 records that
// time.Sleep in a test fails the audit, and that a rate limiter's window is
// crossed through an injected clock.
type Clock func() time.Time

// CommunityLookup resolves an at-identifier and fetches the community behind
// it. Satisfied by communities.Service.
type CommunityLookup interface {
	// ResolveCommunityIdentifier turns a handle or a DID into a DID.
	ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error)

	// GetByDID returns the indexed community.
	GetByDID(ctx context.Context, did string) (*communities.Community, error)
}

// BanLookup answers whether an author is banned from a community, by returning
// the membership row that carries the answer.
//
// It returns the whole membership rather than a bool so that the translation of
// "no membership row" into "not banned" happens in ONE place — inside
// admitPost, next to the comment that explains why an error is not the same
// thing. Satisfied by communities.Service.
type BanLookup interface {
	// GetMembership returns the author's membership of the community, or an
	// error wrapping communities.ErrMembershipNotFound when there is none.
	GetMembership(ctx context.Context, userDID, communityIdentifier string) (*communities.Membership, error)
}

// AggregatorAuthorizer checks a registered aggregator's authorization and its
// own quota. Satisfied by aggregators.Service.
type AggregatorAuthorizer interface {
	ValidateAggregatorPost(ctx context.Context, aggregatorDID, communityDID string) error
}

// ReserveSubmissionCommand is one row of the post_submissions ledger.
type ReserveSubmissionCommand struct {
	AuthorDID    string
	CommunityDID string

	// Fingerprint is the content hash — see AdmissionRequest.Fingerprint.
	Fingerprint string

	// DedupeBucket is the index of the DedupeWindow this submission falls in,
	// derived from the injected clock. It is part of the unique key, which is
	// what makes dedupe expire.
	DedupeBucket int64
}

// SubmissionLedger records admitted submissions, and IS the dedupe gate.
//
// RESERVE-THEN-CONFIRM. The row goes in BEFORE the PDS write and is released if
// that write fails. The alternative — record after a successful write — leaves
// a window in which two concurrent identical submissions both pass the check
// and both get written, which is precisely the double-tap this exists to stop.
// A leaked reservation (process died between the insert and the release) costs
// the author one quota slot until the window rolls; a missed one costs the
// community a duplicate post. The asymmetry decides the direction.
//
// THE INSERT IS THE CHECK. Dedupe is not a SELECT followed by an INSERT: it is
// the INSERT, with the unique constraint as the arbiter. A read-then-write
// would reopen the same race under concurrency, and the database is the only
// participant that can serialize it.
type SubmissionLedger interface {
	// Reserve inserts the ledger row for a submission. A unique-constraint
	// violation means an identical submission is already recorded for this
	// window, and is reported as ErrDuplicateSubmission rather than as a driver
	// error — the caller has to tell "someone already posted this" apart from
	// "the database is unwell".
	Reserve(ctx context.Context, cmd ReserveSubmissionCommand) (SubmissionReservation, error)

	// Release removes a reservation whose submission never became a post. It is
	// idempotent: releasing a row that is already gone is not an error, because
	// the caller reaches this path while already handling a failure and must
	// not be handed a second one.
	Release(ctx context.Context, reservation SubmissionReservation) error

	// CountSince counts one author's submissions to one community at or after
	// `since` — the rolling-window quota query.
	CountSince(ctx context.Context, authorDID, communityDID string, since time.Time) (int, error)
}

// AdmissionPolicy is the collaborator set the new §8 policy needs, over and
// above what postService already holds.
type AdmissionPolicy struct {
	Ledger SubmissionLedger
	Bans   BanLookup
	Limits SubmissionLimits
	Now    Clock
}

// WithAdmissionPolicy enables the ban check, dedupe and per-author rate limit
// on CreatePost.
func WithAdmissionPolicy(policy AdmissionPolicy) PostServiceOption {
	return func(s *postService) { s.admission = &policy }
}

// admissionDeps is everything admitPost reads, gathered so the decision is a
// function of its arguments rather than of a service's field set.
type admissionDeps struct {
	communities CommunityLookup
	bans        BanLookup
	aggregators AggregatorAuthorizer
	ledger      SubmissionLedger
	limits      SubmissionLimits
	now         Clock
}

// admitPost decides whether one submission may become a post.
//
// CHECK ORDER — each step its own refusal, and the order is load-bearing:
//
//  1. Community resolution. Nothing else can be evaluated against a community
//     that does not exist.
//
//  2. Private visibility, for regular users. A banned member of a PRIVATE
//     community is refused with DecisionCommunityPrivate, NOT with
//     DecisionAuthorBanned — the ban lookup is not even consulted. A private
//     community's moderation state is behind the same wall as its content, and
//     answering "you are banned" would confirm to an outsider both that the
//     community exists and that a moderator has acted on them.
//
//  3. Ban, for regular users in public communities. A membership row with no
//     ban, or NO membership row at all, is not a ban — that is the ordinary
//     case, since posting to a public community does not require joining it.
//     Any OTHER lookup error FAILS the request. Failing open here would turn a
//     Postgres blip into a global unban for its duration, which is the one
//     failure mode a ban check exists to prevent.
//
//  4. Aggregator authorization, for registered aggregators. Existing
//     semantics, existing sentinels, carried on the decision's Cause.
//
//  5. Dedupe, for EVERY actor class. An aggregator re-polling an RSS feed and
//     resubmitting an identical item is the canonical case, so exempting
//     trusted actors here would exempt the exact traffic the check is for.
//
//  6. Rate limit, for regular users only.
//
// DEDUPE BEFORE RATE LIMIT. A client whose response was lost retries; if the
// retry burned quota, a flaky connection would rate-limit a user who posted
// once. Dedupe recognises the retry for what it is and refuses it without
// charging for it.
//
// TRUSTED AGGREGATORS skip 2, 3, 4 (existing behaviour) and also skip 6: they
// have no submission limit today, and inventing one here would be a silent
// production behaviour change smuggled in under a refactor. REGISTERED
// aggregators skip 6 for a different reason — they are already governed by
// their own hourly quota inside ValidateAggregatorPost (step 4), and applying
// the new per-author limit as well would silently halve an authorized
// aggregator's throughput.
//
// A REFUSAL CONSUMES NO QUOTA: no refusal leaves a ledger row behind, including
// the rate-limit refusal itself, whose reservation is released before it
// returns. Otherwise an author who kept retrying past their limit would extend
// their own lockout indefinitely.
//
// A non-nil error means the decision could NOT be made — a lookup failed — and
// is distinct from a refusal, which is a decision.
func admitPost(ctx context.Context, deps admissionDeps, req AdmissionRequest) (AdmissionDecision, error) {
	return AdmissionDecision{}, nil
}

// dedupeBucket is the index of the window `now` falls in, so that two
// submissions in the same window collide on the ledger's unique key and two
// submissions a window apart do not.
func dedupeBucket(now time.Time, window time.Duration) int64 {
	return 0
}

// submissionFingerprint hashes what a moderator would judge about a record:
// everything except createdAt.
//
// The timestamp has to go. It is stamped by the server at submission time
// (service.go step 9), so it differs on every attempt — including the retry
// after a lost response, which is the case dedupe exists to catch. A
// fingerprint that included it would never match anything.
func submissionFingerprint(record PostRecord) string {
	return ""
}
