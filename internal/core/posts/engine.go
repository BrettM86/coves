package posts

import (
	"context"
	"errors"
	"fmt"

	"Coves/internal/atproto/pds"
)

// The acceptance engine: the single decision point of
// docs/PRD_AUTHOR_OWNED_POSTS.md §5.6.
//
// Its input is one admission row — a (community, post) subject the AppView has
// indexed and this AppView hosts the community for. It runs the admission
// policy over the content the row holds and then makes the community's repo
// agree with the answer: an acceptance record, a removal commit, or an
// AppView-local rejection that writes no record at all.
//
// It is the ONLY writer of community-repo records in the post system, which is
// what makes "every write is idempotent" a property of the system rather than a
// convention each call site has to remember.

// EngineOutcome reports what one pass over one subject DID.
//
// It is a value rather than an error for the same reason AdmissionOutcome is:
// most of the ways a pass ends without writing anything are the engine working
// — a decision that could not be made yet, a row another writer already
// settled — and routing those into the dead-letter queue would bury the
// failures the queue exists to surface.
type EngineOutcome string

const (
	// EngineAccepted means a community acceptance now stands, pinning the CID
	// the AppView has indexed.
	EngineAccepted EngineOutcome = "accepted"

	// EngineRejected means the AppView recorded a local refusal. No community
	// record was written: §3.3 is explicit that a submission refused before it
	// was ever accepted must not bloat the community's repo.
	EngineRejected EngineOutcome = "rejected"

	// EngineRemoved means a removal record now stands and the acceptance is
	// gone, committed together.
	EngineRemoved EngineOutcome = "removed"

	// EngineRepinned means a standing acceptance moved onto new content with no
	// re-decision — the bridgedStats exception of §5.5.
	EngineRepinned EngineOutcome = "repinned"

	// EngineDeferred means NOTHING was written anywhere and the subject is
	// still owed a decision. It covers an undecided policy answer, a row whose
	// content CID is not yet known, a row already in a terminal state, and a
	// credential failure. In every one of those cases the correct next step is
	// to look again later, never to record a verdict.
	EngineDeferred EngineOutcome = "deferred"
)

// AdmissionDecider is the policy half of admitPost, without its ledger
// reservation.
//
// The split matters. admitPost's dedupe step INSERTS a ledger row, and that
// insert is a submission-time gate: it exists so two concurrent submissions of
// identical content cannot both become posts. The engine is not a submission —
// it is deciding about a post that already exists, often one it has decided
// about before — so reserving a ledger slot here would charge an author quota
// for a firehose redelivery and refuse the redecision as its own duplicate.
type AdmissionDecider interface {
	// DecideAdmission evaluates one indexed post against one community's
	// policy. A refusal is a code on the decision; a decision that could not be
	// made is a non-nil error, and AdmissionDecision.Admitted() reports false
	// for both.
	DecideAdmission(ctx context.Context, communityDID, postURI string) (AdmissionDecision, error)
}

// CredentialRefresher forces a community's stored PDS token to be renewed.
//
// The engine holds this so that a stale access token costs one retry rather
// than a verdict. A community's token expires on a schedule that has nothing to
// do with moderation, and an engine that treated the resulting 401 as anything
// other than "try again with fresh credentials" would either lose the decision
// or — far worse — record a refusal for a post it never managed to ask about.
type CredentialRefresher interface {
	RefreshCommunityCredentials(ctx context.Context, communityDID string) error
}

// AcceptanceEngine settles one subject at a time.
type AcceptanceEngine struct {
	admissions  AdmissionRepository
	decider     AdmissionDecider
	writer      CommunityRecordWriter
	credentials CredentialRefresher
}

// NewAcceptanceEngine wires the engine.
func NewAcceptanceEngine(
	admissions AdmissionRepository,
	decider AdmissionDecider,
	writer CommunityRecordWriter,
	credentials CredentialRefresher,
) *AcceptanceEngine {
	return &AcceptanceEngine{
		admissions:  admissions,
		decider:     decider,
		writer:      writer,
		credentials: credentials,
	}
}

// ProcessAdmission settles one (community, post) subject.
//
// ROUTING IS KEYED ON THE ROW'S STATUS AND THE DECISION TOGETHER, because the
// same verdict means different writes depending on what already stands:
//
//	pending              + admitted  → acceptance created
//	pending              + refused   → AppView-local rejection, NO repo write
//	pending_reacceptance + admitted  → acceptance updated in place, new CID
//	pending_reacceptance + refused   → removal commit (§5.5: a failed
//	                                   re-acceptance is a removal, never a
//	                                   rejection — a rejection would suppress
//	                                   the acceptance that is standing)
//	anything             + undecided → nothing, deferred
//
// A row already in a terminal state — accepted, rejected, removed — is a
// defensive skip. The engine's queue can hand it the same subject twice, and
// re-deciding a settled row is how a removal gets laundered back into a feed.
//
// THE REPOSITORY UPDATE IS OPTIMISTIC AND ITS SKIPS ARE SUCCESS. After a write
// commits, the engine stamps the admission row with the commit rev itself
// rather than waiting for the firehose copy of its own event. If the firehose
// got there first, the repository answers skipped_stale; if the row moved on,
// skipped_terminal. Neither is a failure to report — the firehose is the
// authority on what the repo says, and this write is the AppView catching up
// with itself.
func (e *AcceptanceEngine) ProcessAdmission(ctx context.Context, communityDID, postURI string) (EngineOutcome, error) {
	// THE ROW IS READ FIRST, and the status half of the routing decision comes
	// from it rather than from whoever queued the subject. A queue entry is a
	// hint that something happened; the row is what the AppView believes, and
	// only one of those can be trusted to say a post has already been removed.
	row, err := e.admissions.Get(ctx, communityDID, postURI)
	if err != nil {
		return EngineDeferred, fmt.Errorf("reading the admission of %s in %s: %w", postURI, communityDID, err)
	}
	if row == nil {
		return EngineDeferred, fmt.Errorf("reading the admission of %s in %s: %w", postURI, communityDID, ErrNotFound)
	}

	switch row.Status {
	case AdmissionStatusPending, AdmissionStatusPendingReacceptance:
		// The two states a decision is owed for.
	default:
		// A DEFENSIVE SKIP, and `removed` is why it has to be here rather than
		// in the queue. Removal is terminal against everything except a
		// moderator restore at a strictly greater watermark (§5.5), so an engine
		// that re-ran policy on a settled row would launder a removed post
		// straight back into the feeds it was removed from — and the queue hands
		// it the same subject constantly: redrives, overlapping feeds, a notify
		// racing the firehose.
		return EngineDeferred, nil
	}

	// An acceptance's subject is a strongRef, and a strongRef without a CID pins
	// nothing. A row with no evaluated CID is one whose content this AppView has
	// not decoded yet, so there is nothing to judge and nothing to pin.
	if row.EvaluatedCID == nil || *row.EvaluatedCID == "" {
		return EngineDeferred, nil
	}
	evaluatedCID := *row.EvaluatedCID

	decision, err := e.decider.DecideAdmission(ctx, communityDID, postURI)
	if err != nil {
		// UNDECIDED, which is neither an admission nor a refusal. A community
		// lookup or a ban lookup that could not be reached must never become a
		// verdict: recording one would turn a Postgres blip into a permanent
		// decision about someone's post, and set redrivable=false on it.
		return EngineDeferred, fmt.Errorf("deciding %s for %s: %w", postURI, communityDID, err)
	}
	if !decision.Admitted() && decision.Code == "" {
		// The same state arriving as a value rather than an error. Admitted() is
		// false for both a refusal and an undecided answer, so the code is what
		// tells them apart — and an answer with neither is one nothing may be
		// written from.
		return EngineDeferred, fmt.Errorf("deciding %s for %s: the policy returned no verdict: %w",
			postURI, communityDID, decision.Cause)
	}

	if decision.Admitted() {
		return e.accept(ctx, communityDID, postURI, evaluatedCID)
	}

	if row.Status == AdmissionStatusPending {
		return e.reject(ctx, communityDID, postURI, evaluatedCID, decision.Code)
	}

	// pending_reacceptance + refused. §5.5 is explicit that a failed
	// re-acceptance is a REMOVAL: an acceptance is standing in the community's
	// repo and was published to the firehose, so an AppView-local rejection
	// would leave it in place and federated peers would keep rendering a post
	// this AppView had hidden.
	return e.remove(ctx, communityDID, postURI, evaluatedCID, decision.Code)
}

// accept makes the community's acceptance stand and then stamps the row.
//
// THE ORDER IS THE POINT. The repo write happens first and the row is stamped
// only after it commits, so the AppView can never claim an acceptance the PDS
// never took.
func (e *AcceptanceEngine) accept(ctx context.Context, communityDID, postURI, evaluatedCID string) (EngineOutcome, error) {
	written, err := e.write(ctx, communityDID, func() (CommunityWriteResult, error) {
		return e.writer.WriteAcceptance(ctx, CommunityWriteCommand{
			CommunityDID: communityDID,
			PostURI:      postURI,
			// The CID the AppView has INDEXED. Pinning anything else is an
			// acceptance of content nobody evaluated.
			PostCID: evaluatedCID,
		})
	})
	if err != nil {
		return EngineDeferred, err
	}

	// A SKIPPED WRITE STAMPS NOTHING. The repo already held this exact
	// acceptance, so nothing committed and there is no revision to report —
	// getRecord does not reveal the revision an existing record was written at.
	// The repository refuses an empty rev as a fabricated watermark, and
	// inventing one to get past that would write a clock value no commit had.
	if written.Skipped {
		return EngineAccepted, nil
	}

	if _, err := e.admissions.ApplyAcceptance(ctx, ApplyAcceptanceCommand{
		CommunityDID:   communityDID,
		PostURI:        postURI,
		AcceptanceURI:  written.URI,
		AcceptanceRkey: written.RKey,
		PinnedCID:      evaluatedCID,
		// The rev the write actually COMMITTED in — the §5.2 watermark. Its
		// OpRank is deliberately left zero: the repository derives the rank from
		// the operation itself, because the rank IS the operation's kind.
		Watermark: CommunityWatermark{Rev: written.Rev},
	}); err != nil {
		// The record IS in the community's repo and the row disagrees. The
		// firehose will reconcile it, but a silent divergence is how a post
		// stays invisible for hours with nothing to search for.
		return EngineAccepted, fmt.Errorf("stamping the acceptance of %s in %s: %w", postURI, communityDID, err)
	}

	return EngineAccepted, nil
}

// reject records the AppView's own refusal and writes NO community record.
//
// §3.3: a submission refused before it was ever accepted must not bloat the
// community's repository — spam would otherwise be permanently archived in the
// repo of the community that refused it.
func (e *AcceptanceEngine) reject(ctx context.Context, communityDID, postURI, evaluatedCID string, code DecisionCode) (EngineOutcome, error) {
	if _, err := e.admissions.RecordRejection(ctx, RecordRejectionCommand{
		CommunityDID: communityDID,
		PostURI:      postURI,
		DecisionCode: string(code),
		// The CID the verdict JUDGED. The repository lands the rejection only on
		// a pending row still holding it, so an author who edited between the
		// read and the write gets fresh content judged fresh rather than
		// condemned by a verdict about something else.
		JudgedCID: evaluatedCID,
		// A policy refusal is terminal. Leaving this true would have the
		// dead-letter redrive pass retry a decision that will never change.
		Redrivable: false,
	}); err != nil {
		return EngineDeferred, fmt.Errorf("recording the rejection of %s in %s: %w", postURI, communityDID, err)
	}

	return EngineRejected, nil
}

// remove withdraws a standing acceptance with a removal commit.
func (e *AcceptanceEngine) remove(ctx context.Context, communityDID, postURI, evaluatedCID string, code DecisionCode) (EngineOutcome, error) {
	written, err := e.write(ctx, communityDID, func() (CommunityWriteResult, error) {
		return e.writer.WriteRemoval(ctx, CommunityRemovalCommand{
			CommunityDID: communityDID,
			PostURI:      postURI,
			// Audit metadata: the version present at removal time. The removal
			// itself is URI-scoped and survives later edits (§5.5).
			PostCID: evaluatedCID,
			Code:    code,
		})
	})
	if err != nil {
		return EngineDeferred, err
	}

	if written.Skipped {
		return EngineRemoved, nil
	}

	if _, err := e.admissions.ApplyRemoval(ctx, ApplyRemovalCommand{
		CommunityDID: communityDID,
		PostURI:      postURI,
		DecisionCode: string(code),
		Watermark:    CommunityWatermark{Rev: written.Rev},
	}); err != nil {
		return EngineRemoved, fmt.Errorf("stamping the removal of %s in %s: %w", postURI, communityDID, err)
	}

	return EngineRemoved, nil
}

// write runs one community-repo write, renewing the community's credentials
// once if they turn out to be stale.
//
// ONE FORCED RENEWAL, NEVER MORE. A community's PDS access token expires on a
// schedule that has nothing to do with moderation, so a single retry is the
// difference between a decision that lands and a decision that has to be
// redriven. Retrying persistently would instead have every pass in the queue
// spin against the PDS of a community whose credentials are genuinely gone.
//
// AND NEVER A VERDICT. Whatever comes back, a write failure is returned as an
// error and the caller defers: "I could not write the acceptance" and "this post
// is not acceptable" are opposite facts, and an engine that recorded the second
// when it meant the first would answer the author with a permanent refusal that
// nothing would ever retry.
func (e *AcceptanceEngine) write(ctx context.Context, communityDID string, attempt func() (CommunityWriteResult, error)) (CommunityWriteResult, error) {
	result, err := attempt()
	if err == nil || !errors.Is(err, pds.ErrUnauthorized) {
		return result, err
	}

	if refreshErr := e.credentials.RefreshCommunityCredentials(ctx, communityDID); refreshErr != nil {
		return CommunityWriteResult{}, fmt.Errorf("renewing the credentials of %s after %w: %w",
			communityDID, err, refreshErr)
	}

	return attempt()
}
