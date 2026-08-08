package posts

import "context"

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
	return "", nil
}
