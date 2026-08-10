package posts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"Coves/internal/core/aggregators"
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
	// record with createdAt and the client-typed community identifier removed,
	// and the supplied thumbnail URL folded in (see submissionFingerprint). It
	// is the dedupe key, and it must exclude the timestamp or every
	// resubmission of identical content would look new.
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

	// DedupeBucket is the window index the reservation was taken in, reported
	// so the caller derives the post's record key from THE SAME bucket the
	// ledger deduped against (SubmissionRkey).
	//
	// Reading the clock a second time at the call site would work almost
	// always and fail exactly at a bucket boundary — the retry that crossed it
	// would aim at a different rkey than the ledger scoped it to, which is the
	// one case the deterministic key exists for.
	DedupeBucket int64

	// Cause carries the underlying error behind a refusal, when there is one,
	// so the caller can wrap it and keep it matchable.
	//
	// The refusal case is aggregator authorization. The API boundary maps that
	// refusal through aggregators.IsUnauthorized and aggregators.IsRateLimited
	// (internal/api/handlers/post/errors.go), which are predicates over the
	// AGGREGATORS package's sentinels. Collapsing that error into a bare
	// DecisionCode would turn a 403 "stop asking" and a 429 "ask later" into
	// the same answer, and a well-behaved aggregator would retry a permanent
	// refusal forever.
	//
	// It is ALSO set on the decision returned alongside a non-nil error, which
	// is what keeps a decision that could not be made from reading as an
	// admission — see Admitted.
	Cause error
}

// Admitted reports whether the submission may proceed.
//
// There are three states here, not two: admitted, refused with a code, and
// NOT DECIDED — a lookup failed and nothing was concluded either way. The third
// is why this is not simply `Code == ""`. An infrastructure failure must not be
// dressed up as a policy code (the client would be told to stop retrying
// something that will work in a second), so those returns carry an empty Code;
// a bare code test would then read the zero value as an admission, which is the
// most dangerous default available on a security decision. A decision is an
// admission only when there is neither a refusal code nor a cause.
//
// There is still no separate `admitted` bool: two representations of one fact
// drift, and the code is the one that has to be right.
func (d AdmissionDecision) Admitted() bool { return d.Code == "" && d.Cause == nil }

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

// WithAdmissionPolicy supplies the ban check, dedupe and per-author rate limit
// on CreatePost. It is not optional: NewPostService refuses to construct a
// service without a complete policy (see mustCompleteAdmissionPolicy), because
// a post service whose admission policy silently defaulted to no-ops would be
// one whose ban check and quota do not exist and nothing says so.
func WithAdmissionPolicy(policy AdmissionPolicy) PostServiceOption {
	return func(s *postService) { s.admission = &policy }
}

// mustCompleteAdmissionPolicy is NewPostService's guard: a service may not be
// constructed without a complete admission policy.
//
// It panics rather than returning an error, matching how this codebase treats
// every other mandatory collaborator (aggregators.NewAPIKeyService,
// blueskypost.NewService): a missing policy is a wiring bug that must stop the
// process at startup, not a runtime condition to handle. The old alternative —
// silently substituting a no-op ledger and ban lookup — is exactly how the
// pre-§4.1 docstring came to claim "membership/ban validation" that had never
// existed on the write path.
//
// Every limit must be positive for the same reason config.Validate enforces
// it: a quota that silently disappears when a field is left zero is not a
// quota. Tests that are not about admission opt out EXPLICITLY with
// NewAllowAllAdmissionPolicyForTests.
func mustCompleteAdmissionPolicy(policy *AdmissionPolicy) {
	switch {
	case policy == nil:
		panic("posts.NewPostService: an admission policy is required — wire posts.WithAdmissionPolicy " +
			"(cmd/server) or posts.NewAllowAllAdmissionPolicyForTests (fixtures that are not about admission)")
	case policy.Ledger == nil:
		panic("posts.NewPostService: AdmissionPolicy.Ledger cannot be nil")
	case policy.Bans == nil:
		panic("posts.NewPostService: AdmissionPolicy.Bans cannot be nil")
	case policy.Now == nil:
		panic("posts.NewPostService: AdmissionPolicy.Now cannot be nil")
	case policy.Limits.MaxPerAuthorPerCommunity <= 0:
		panic("posts.NewPostService: AdmissionPolicy.Limits.MaxPerAuthorPerCommunity must be positive")
	case policy.Limits.Window <= 0:
		panic("posts.NewPostService: AdmissionPolicy.Limits.Window must be positive")
	case policy.Limits.DedupeWindow <= 0:
		panic("posts.NewPostService: AdmissionPolicy.Limits.DedupeWindow must be positive")
	}
}

// unmeteredLedger is the allow-all test policy's ledger: it reserves nothing,
// so neither dedupe nor the per-author quota applies.
//
// It cannot silently disable a configured limiter — it is only ever reachable
// through NewAllowAllAdmissionPolicyForTests, whose name is the warning.
type unmeteredLedger struct{}

func (unmeteredLedger) Reserve(context.Context, ReserveSubmissionCommand) (SubmissionReservation, error) {
	return SubmissionReservation{}, nil
}

func (unmeteredLedger) Release(context.Context, SubmissionReservation) error { return nil }

// CountSince answers zero, which admits: with no ledger there is nothing to
// count, and refusing on an absent substrate would take a service that never
// asked for a quota and stop it posting at all.
func (unmeteredLedger) CountSince(context.Context, string, string, time.Time) (int, error) {
	return 0, nil
}

// unenforcedBans is the allow-all test policy's ban lookup, answering the way
// an author with no membership row does. It returns the sentinel rather than a
// nil membership so that it travels the same branch a real absent row does —
// the "no membership means not banned" translation stays in one place.
type unenforcedBans struct{}

func (unenforcedBans) GetMembership(context.Context, string, string) (*communities.Membership, error) {
	return nil, communities.ErrMembershipNotFound
}

// NewAllowAllAdmissionPolicyForTests is the explicit opt-out for TEST fixtures
// whose subject is not admission: it admits everything an unconfigured service
// used to — no ban rows to find, no dedupe, no per-author quota — while
// community existence, visibility and aggregator authorization stay enforced.
//
// THE NAME IS THE CONTRACT: this must never be wired in production code.
// cmd/server wires the real policy, and mustCompleteAdmissionPolicy exists
// precisely so that forgetting to do so fails at startup instead of shipping a
// post service whose §8 enforcement quietly does not exist. The limits are
// real (and enormous) only because construction refuses non-positive ones; the
// unmetered ledger never counts against them anyway.
func NewAllowAllAdmissionPolicyForTests() AdmissionPolicy {
	return AdmissionPolicy{
		Ledger: unmeteredLedger{},
		Bans:   unenforcedBans{},
		Limits: SubmissionLimits{
			MaxPerAuthorPerCommunity: 1 << 30,
			Window:                   time.Hour,
			DedupeWindow:             time.Hour,
		},
		Now: time.Now,
	}
}

// admissionDeps assembles the decision's inputs from the service's
// collaborators. s.admission is never nil or incomplete — NewPostService
// refuses to construct without a complete policy — so this cannot silently
// hand admitPost a missing ledger or clock.
func (s *postService) admissionDeps() admissionDeps {
	return admissionDeps{
		communities: s.communityService,
		bans:        s.admission.Bans,
		aggregators: s.aggregatorService,
		ledger:      s.admission.Ledger,
		limits:      s.admission.Limits,
		now:         s.admission.Now,
	}
}

// refusalError translates a refusal into the sentinel the API boundary maps.
//
// The codes and the sentinels are separate vocabularies on purpose: a code is
// what the admissions table stores and what a federated peer is told, while a
// sentinel is what internal/api/handlers/post turns into a status. This is the
// one place they meet.
func refusalError(decision AdmissionDecision) error {
	switch decision.Code {
	case DecisionCommunityNotFound:
		return ErrCommunityNotFound
	case DecisionCommunityPrivate:
		// Unchanged from the pre-§8 behaviour: a private community answers the
		// same 403 to a member-less user it always did.
		return ErrNotAuthorized
	case DecisionAuthorBanned:
		return ErrBanned
	case DecisionAggregatorNotAuthorized:
		// The aggregators-package sentinel has to survive: the boundary tells a
		// permanent 403 from a retryable 429 by matching on it, and the wording
		// is the one CreatePost has always used.
		return fmt.Errorf("aggregator not authorized: %w", decision.Cause)
	case DecisionDuplicateSubmission:
		return ErrDuplicateSubmission
	case DecisionRateLimitExceeded:
		return ErrRateLimitExceeded
	default:
		// A code minted without a mapping. Answering with a bare 500 would be
		// wrong twice over — the submission WAS refused, and the operator would
		// have nothing to search for — so the code itself goes in the error.
		return fmt.Errorf("submission refused: %s", decision.Code)
	}
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
	decision, err := evaluateAdmissionPolicy(ctx, deps, req)
	if err != nil || !decision.Admitted() {
		return decision, err
	}
	return reserveSubmission(ctx, deps, req, decision.Community)
}

// evaluateAdmissionPolicy is admitPost's steps 0-4: everything that decides
// whether this author may post to this community AT ALL, and nothing that
// reserves a slot for the attempt.
//
// THE SPLIT EXISTS FOR THE ACCEPTANCE ENGINE. §5.6's engine decides about a post
// that ALREADY EXISTS — often one it has decided about before, arriving again
// from an overlapping feed or a redrive — so it needs the policy and must not
// have the reservation. Running the ledger insert there would charge an author's
// quota for a firehose redelivery and then refuse the redecision as a duplicate
// of the submission it is redeciding.
//
// On an admission it returns the resolved community and NOTHING else, so a
// caller that goes on to reserve does so explicitly.
func evaluateAdmissionPolicy(ctx context.Context, deps admissionDeps, req AdmissionRequest) (AdmissionDecision, error) {
	// 0. The actor class must be one this decision knows. It gates everything
	// below — including the trusted skip of visibility, ban and authorization —
	// so an unknown value must fail CLOSED before any lookup runs. Falling
	// through would hand the zero value (a caller that forgot to classify) the
	// widest privileges in the system.
	switch req.Actor {
	case ActorUser, ActorRegisteredAggregator, ActorTrustedAggregator:
	default:
		return undecided(fmt.Errorf("unknown actor class %q: the submission cannot be evaluated", req.Actor))
	}

	// 1. Community resolution. Two lookups — the at-identifier to a DID, then
	// the DID to the indexed row — and either failing to find it is the same
	// answer to the client.
	communityDID, err := deps.communities.ResolveCommunityIdentifier(ctx, req.Community)
	if err != nil {
		switch {
		case errors.Is(err, communities.ErrCommunityNotFound):
			return AdmissionDecision{Code: DecisionCommunityNotFound}, nil
		case communities.IsValidationError(err):
			// A malformed identifier is the client's mistake and has to reach
			// the boundary as one: a validation error becomes a 400 naming the
			// bad field, while an unclassified error becomes an opaque 500.
			return undecided(NewValidationError("community", err.Error()))
		default:
			return undecided(fmt.Errorf("failed to resolve community identifier: %w", err))
		}
	}

	community, err := deps.communities.GetByDID(ctx, communityDID)
	if err != nil {
		if errors.Is(err, communities.ErrCommunityNotFound) {
			return AdmissionDecision{Code: DecisionCommunityNotFound}, nil
		}
		return undecided(fmt.Errorf("failed to fetch community: %w", err))
	}

	if req.Actor == ActorUser {
		// 2. The privacy wall, and it stands ahead of the ban lookup rather
		// than beside it: moderation state must not be read at all for a
		// community the submitter cannot see. See the check-order note above.
		if community.Visibility == "private" {
			return AdmissionDecision{Code: DecisionCommunityPrivate}, nil
		}

		// 3. The ban. Looked up against the RESOLVED DID — handles are mutable,
		// and a ban keyed to one stops applying the moment a community renames
		// itself.
		membership, err := deps.bans.GetMembership(ctx, req.AuthorDID, community.DID)
		switch {
		case errors.Is(err, communities.ErrMembershipNotFound):
			// A VALUE meaning "not banned". Posting in a public community has
			// never required joining it, so an absent row is the common case.
		case err != nil:
			// Fail closed. Treating an unreachable database as "not banned"
			// would turn a Postgres blip into a global unban for its duration.
			return undecided(fmt.Errorf("failed to look up community membership: %w", err))
		case membership != nil && membership.IsBanned:
			return AdmissionDecision{Code: DecisionAuthorBanned}, nil
		}
	}

	// 4. Aggregator authorization, which carries the aggregators-package
	// sentinel that caused it: the boundary tells 403 from 429 by matching on
	// it, and a bare code would have a well-behaved aggregator retry a
	// permanent refusal forever.
	//
	// Only the package's POLICY sentinels are refusals. ValidateAggregatorPost
	// also fails when its own lookups do (a wrapped driver error carrying no
	// sentinel), and that is an undecided infrastructure failure like any
	// other — dressing it as DecisionAggregatorNotAuthorized would mint a
	// permanent-sounding 403 out of a Postgres blip.
	if req.Actor == ActorRegisteredAggregator {
		if err := deps.aggregators.ValidateAggregatorPost(ctx, req.AuthorDID, community.DID); err != nil {
			if errors.Is(err, aggregators.ErrNotAuthorized) || errors.Is(err, aggregators.ErrRateLimitExceeded) {
				return AdmissionDecision{Code: DecisionAggregatorNotAuthorized, Cause: err}, nil
			}
			return undecided(fmt.Errorf("failed to validate aggregator post: %w", err))
		}
	}

	return AdmissionDecision{Community: community}, nil
}

// reserveSubmission is admitPost's steps 5-6: the dedupe insert and the
// rolling-window quota, both of which exist only for a NEW submission.
//
// They are one function rather than two because step 6 counts the row step 5
// inserted — the reservation is on the ledger before the quota is measured, so
// the limit is reached when the count EXCEEDS it — and every path out that is
// not an admission has to hand the slot back.
func reserveSubmission(ctx context.Context, deps admissionDeps, req AdmissionRequest, community *communities.Community) (AdmissionDecision, error) {
	// 5. Dedupe, for every actor class. The INSERT is the check: a unique
	// violation means an identical submission is already on the ledger for this
	// window. It runs ahead of the quota so that a client retrying after a lost
	// response is told its post already exists rather than told to slow down.
	now := deps.now()
	bucket := dedupeBucket(now, deps.limits.DedupeWindow)
	reservation, err := deps.ledger.Reserve(ctx, ReserveSubmissionCommand{
		AuthorDID:    req.AuthorDID,
		CommunityDID: community.DID,
		Fingerprint:  req.Fingerprint,
		DedupeBucket: bucket,
	})
	if err != nil {
		if errors.Is(err, ErrDuplicateSubmission) {
			return AdmissionDecision{Code: DecisionDuplicateSubmission}, nil
		}
		return undecided(fmt.Errorf("failed to reserve submission: %w", err))
	}

	// 6. The rolling-window quota, for regular users only. Aggregators are
	// metered by their own limiter (step 4) or, when trusted, not at all.
	//
	// The reservation is already on the ledger, so it is counted here — the
	// limit is reached when the count EXCEEDS it — and every path out of this
	// block that is not an admission has to hand the slot back.
	if req.Actor == ActorUser {
		count, err := deps.ledger.CountSince(ctx, req.AuthorDID, community.DID, now.Add(-deps.limits.Window))
		if err != nil {
			releaseReservation(ctx, deps.ledger, reservation)
			return undecided(fmt.Errorf("failed to count recent submissions: %w", err))
		}
		if count > deps.limits.MaxPerAuthorPerCommunity {
			releaseReservation(ctx, deps.ledger, reservation)
			return AdmissionDecision{Code: DecisionRateLimitExceeded}, nil
		}
	}

	return AdmissionDecision{Community: community, Reservation: &reservation, DedupeBucket: bucket}, nil
}

// undecided reports that the decision could NOT be made.
//
// The error is returned twice — as the error, and on the decision's Cause — and
// the second copy is the load-bearing one: it is what makes Admitted() false
// for a caller that inspects the value. Leaving the decision zero would have a
// caller who checked the decision before the error read a database outage as
// permission to post.
func undecided(err error) (AdmissionDecision, error) {
	return AdmissionDecision{Cause: err}, err
}

// releaseReservation gives back a slot the decision took and then declined to
// use. The error is logged rather than returned: every caller reaches this
// while already reporting a refusal or a failure, and replacing that answer
// with a second one would hide the reason the submission was actually stopped.
//
// The release runs DETACHED from the caller's cancellation (precedent:
// adminreports.raiseAlert), because the most common reason to be here at all
// is that the caller's context is already dead — a client that disconnected
// mid-write is exactly a failed PDS write. A release issued on that context
// would be refused by Postgres as canceled too, and the reservation would
// leak: one quota slot burned and the author's retry refused as a duplicate,
// with nothing but a warning line to say why. Context values (trace IDs)
// survive; only the cancellation signal is dropped, and the fresh timeout
// keeps a wedged database from pinning the goroutine.
func releaseReservation(ctx context.Context, ledger SubmissionLedger, reservation SubmissionReservation) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := ledger.Release(releaseCtx, reservation); err != nil {
		log.Printf("[POST-ADMIT] Warning: failed to release submission reservation %d: %v", reservation.ID, err)
	}
}

// dedupeBucket is the index of the window `now` falls in, so that two
// submissions in the same window collide on the ledger's unique key and two
// submissions a window apart do not.
//
// Buckets are aligned to the epoch, not to the submission, so the effective
// dedupe protection ranges over (0, window] depending on where in the bucket
// a submission lands: content submitted just before a bucket edge can be
// resubmitted the moment the edge passes. That tradeoff is deliberate — the
// epoch-aligned key self-expires without a sweeper, where a per-submission
// window would need a range predicate or a cleanup process to expire. The
// same boundary bounds a leaked reservation: a crash between Reserve and the
// PDS write leaves a row that burns one quota slot and refuses identical
// content as a duplicate until the bucket rolls, then heals on its own.
func dedupeBucket(now time.Time, window time.Duration) int64 {
	// A non-positive window would divide by zero. config.Validate refuses to
	// start a process with one, so reaching this is a wiring bug rather than an
	// operator mistake; collapsing to a single bucket keeps it from panicking
	// on the write path, and the constant bucket makes the misconfiguration
	// loud (every repost is refused) rather than silent.
	if window <= 0 {
		return 0
	}
	return now.UnixNano() / int64(window)
}

// submissionFingerprint hashes what a moderator would judge about a
// submission: everything on the record except createdAt and community, plus
// the thumbnail URL that rides alongside the record.
//
// The timestamp has to go. It is stamped by the server at submission time
// (service.go step 6), so it differs on every attempt — including the retry
// after a lost response, which is the case dedupe exists to catch. A
// fingerprint that included it would never match anything.
//
// The community field has to go too, for the opposite failure. It holds the
// at-identifier as the CLIENT typed it — a handle one time, a DID the next —
// while the ledger's unique key already scopes the fingerprint by the
// RESOLVED community DID. Hashing the client-typed identifier would let the
// same submission to the same community bypass dedupe simply by switching
// spelling between attempts; leaving it out cannot collide submissions to
// DIFFERENT communities, because the ledger key keeps them apart.
//
// The thumbnail URL is IN, even though it is not a record field: a trusted
// aggregator supplies it alongside the record (CreatePostRequest.ThumbnailURL)
// and it changes what readers ultimately see. Two submissions differing only
// in their thumbnail are different posts, and excluding it would refuse the
// second as a repeat of the first.
//
// IT STILL HASHES THE DEPRECATED PostRecord SHAPE, not the postv2 record that
// is actually written. The two describe the same submission — every field a
// CreatePostRequest can populate exists on both — so the fingerprint identifies
// the same posts either way, and keeping this shape keeps every dedupe row
// already on the ledger valid across the deploy that flips the write path.
// Moving it onto PostV2Record is a TASK 8 change, not an outstanding cycle-2
// one, and the milestone is chosen rather than deferred. Retyping repartitions
// every live post_submissions row: an author mid-retry when the binary rolls
// would miss their own reservation and be admitted as a second post, which is
// the exact duplicate the deterministic rkey exists to close. Task 8
// re-materializes these records anyway, so it is the one moment the change
// costs nothing.
func submissionFingerprint(record PostRecord, thumbnailURL *string) string {
	// The record is taken by value, so clearing fields here cannot affect the
	// record the caller goes on to write.
	record.CreatedAt = ""
	record.Community = ""

	material := struct {
		Record       PostRecord `json:"record"`
		ThumbnailURL *string    `json:"thumbnailUrl,omitempty"`
	}{Record: record, ThumbnailURL: thumbnailURL}

	canonical, err := json.Marshal(material)
	if err != nil {
		// Unreachable in practice: every field of a PostRecord either has a
		// concrete marshalable type or holds a value decoded from JSON. Hashing
		// a Go rendering instead of returning an empty string matters anyway —
		// a constant fingerprint would collide every submission with every
		// other, and the second post the instance ever received would be
		// refused as a repeat of the first.
		log.Printf("[POST-ADMIT] Warning: submission fingerprint fell back to a Go rendering, canonical JSON marshal failed: %v", err)
		canonical = []byte(fmt.Sprintf("%#v", material))
	}

	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
