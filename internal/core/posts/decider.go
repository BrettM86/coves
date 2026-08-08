package posts

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
)

// The production AdmissionDecider: the adapter that turns "decide about this
// indexed post" into the AdmissionRequest evaluateAdmissionPolicy already
// answers (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6).
//
// The engine's input is an admission row — a community DID and a post URI, and
// nothing about who wrote the post or what class of actor they are. Everything
// the policy needs beyond that has to be recovered from the index, and the two
// recoveries have opposite failure rules, which is most of what this component
// is:
//
//   - THE POST. Absent or tombstoned means there is nothing to decide, and the
//     answer must NOT be an admission. This is the second half of the
//     resurrection guard: the driver already excludes tombstoned subjects, but a
//     post can be deleted between the listing and the decision, and a decider
//     that admitted it would write an acceptance for content the tombstone sweep
//     is about to delete — each component correct, the pair looping.
//   - THE ACTOR CLASS. A misclassification is a privilege decision. Trusted
//     aggregators skip visibility, ban and authorization entirely, so guessing
//     UPWARD on a failed lookup would hand the widest privileges in the system
//     to whoever made the lookup fail. Every uncertain path therefore falls to
//     the STRICTER class, matching CreatePost's existing behaviour (service.go
//     step 3 treats a failed IsAggregator lookup as an ordinary user).
//
// It reuses evaluateAdmissionPolicy rather than admitPost, and that is the split
// task 3 built for: admitPost RESERVES a ledger slot, and the engine is not a
// submission. Reserving here would charge an author's quota for a firehose
// redelivery and then refuse the redecision as a duplicate of the very post it
// is redeciding.

// TrustedAggregatorDIDs reads the trusted-actor allowlist out of the process
// environment: TRUSTED_AGGREGATOR_DIDS, comma-separated, falling back to the
// legacy single-DID KAGI_AGGREGATOR_DID.
//
// It exists so the WRITE path and the ENGINE cannot disagree about who is
// trusted. Those two decide about the same post at different moments, and a
// trusted actor skips visibility, ban and authorization entirely — so two
// spellings of "read this variable" drifting apart would mean the same author
// is privileged on one path and not the other, which is the least debuggable
// shape a permission bug can take.
//
// It is called ONCE, at wiring time, and its result handed to whoever needs it.
// Reading the environment inside a decision would hide the most consequential
// input to a security decision from the place that makes it, and would make the
// trusted branch untestable alongside t.Parallel — Go's testing package refuses
// t.Setenv there.
func TrustedAggregatorDIDs() map[string]bool {
	raw := os.Getenv("TRUSTED_AGGREGATOR_DIDS")
	if raw == "" {
		raw = os.Getenv("KAGI_AGGREGATOR_DID")
	}

	trusted := map[string]bool{}
	for _, did := range strings.Split(raw, ",") {
		if did = strings.TrimSpace(did); did != "" {
			trusted[did] = true
		}
	}
	return trusted
}

// PostLookup reads the indexed post a decision is about. Satisfied by
// Repository.
type PostLookup interface {
	GetByURI(ctx context.Context, uri string) (*Post, error)
}

// AggregatorLookup reports whether a DID is a registered aggregator. Satisfied
// by aggregators.Service.
type AggregatorLookup interface {
	IsAggregator(ctx context.Context, did string) (bool, error)
}

// DeciderDeps is everything the production decider reads.
//
// A struct rather than six positional parameters, and not only for readability:
// two of these are aggregator collaborators with very different jobs —
// Authorizer answers "may this aggregator post here", Aggregators answers "is
// this DID an aggregator at all" — and in production both are the same object.
// Positionally they would be adjacent, same-shaped, and silently swappable.
type DeciderDeps struct {
	// Posts reads the indexed post the decision is about.
	Posts PostLookup

	// Communities resolves the community the admission row names.
	Communities CommunityLookup

	// Authorizer checks a registered aggregator's authorization and quota. May
	// be nil on a deployment with no aggregator support, in which case no author
	// is ever classified as a registered aggregator.
	Authorizer AggregatorAuthorizer

	// Aggregators classifies an author. May be nil, with the same consequence.
	Aggregators AggregatorLookup

	// Policy is the ban lookup, ledger, limits and clock admitPost already uses.
	Policy AdmissionPolicy

	// TrustedAggregatorDIDs is the set from TRUSTED_AGGREGATOR_DIDS, resolved
	// ONCE at construction rather than read per decision.
	//
	// Reading the environment inside the decision would repeat the mistake
	// ActorClass's doc comment describes: it hides the most consequential input
	// to a security decision from the place that makes it, and it makes the
	// trusted branch untestable alongside t.Parallel, since Go's testing package
	// refuses t.Setenv there.
	TrustedAggregatorDIDs map[string]bool
}

// AdmissionEngineDecider is the production AdmissionDecider.
type AdmissionEngineDecider struct {
	deps DeciderDeps
}

// NewAdmissionEngineDecider wires the decider.
func NewAdmissionEngineDecider(deps DeciderDeps) *AdmissionEngineDecider {
	return &AdmissionEngineDecider{deps: deps}
}

// ErrSubjectGone reports that the post an admission row names no longer stands:
// it was tombstoned by its author, or was never indexed at all.
//
// It travels as an ERROR rather than as a DecisionCode, and the choice is the
// difference between a correct record and a defamatory one. The engine turns a
// code on a `pending_reacceptance` row into a REMOVAL — a signed, portable
// moderation act published to the firehose — and an author deleting their own
// post is not the community removing it. There is no code that can be minted
// here without risking that, so the decider declines to decide instead: nothing
// is written, and the subject leaves the backlog on its own, because
// ListPendingSubjects excludes tombstoned and unindexed posts.
var ErrSubjectGone = errors.New("the post this admission names no longer stands")

// DecideAdmission implements AdmissionDecider.
func (d *AdmissionEngineDecider) DecideAdmission(ctx context.Context, communityDID, postURI string) (AdmissionDecision, error) {
	// THE POST FIRST, and everything else after it. Two things come out of this
	// lookup and both gate the policy: whether there is any content to judge,
	// and who wrote it — and the author is what the actor class is derived
	// from, so nothing about privilege can be decided before this returns.
	post, err := d.deps.Posts.GetByURI(ctx, postURI)
	switch {
	case err != nil && IsNotFound(err):
		// Absent. An admission row can legitimately exist with no post — an
		// acceptance that arrived before its subject (§5.4) — so this is a
		// normal state rather than a corruption, and there is simply nothing to
		// judge yet.
		return undecided(fmt.Errorf("deciding %s for %s: %w: it was never indexed",
			postURI, communityDID, ErrSubjectGone))
	case err != nil:
		// A lookup that FAILED is not a post that is absent. The two look
		// identical from here and mean opposite things: the first clears, the
		// second does not, and collapsing them would let a Postgres blip refuse
		// somebody's post.
		return undecided(fmt.Errorf("deciding %s for %s: reading the post: %w", postURI, communityDID, err))
	case post == nil:
		return undecided(fmt.Errorf("deciding %s for %s: %w: it was never indexed",
			postURI, communityDID, ErrSubjectGone))
	case post.DeletedAt != nil:
		// Tombstoned. The driver already excludes these, but a post can be
		// deleted between the listing and the decision, and admitting one would
		// write an acceptance for content that no longer exists — which the
		// host-side tombstone sweep would then delete, once per pass, forever.
		return undecided(fmt.Errorf("deciding %s for %s: %w: its author deleted it",
			postURI, communityDID, ErrSubjectGone))
	}

	return evaluateAdmissionPolicy(ctx, admissionDeps{
		communities: d.deps.Communities,
		bans:        d.deps.Policy.Bans,
		aggregators: d.deps.Authorizer,
		ledger:      d.deps.Policy.Ledger,
		limits:      d.deps.Policy.Limits,
		now:         d.deps.Policy.Now,
	}, AdmissionRequest{
		Actor:     d.classify(ctx, post.AuthorDID),
		AuthorDID: post.AuthorDID,
		// The community DID, which resolves to itself. The engine's input is an
		// admission row, and its key is already the resolved DID — there is no
		// client-typed handle anywhere on this path to resolve.
		Community: communityDID,
		// EMPTY, and it must stay empty. Fingerprint is the dedupe key, read
		// only by reserveSubmission, and this path deliberately never reserves:
		// the engine re-decides posts that already exist, so a ledger row here
		// would charge an author's quota for a firehose redelivery and then
		// refuse the redecision as a duplicate of the very post it is
		// redeciding.
		Fingerprint: "",
	})
}

// classify decides what class of actor the author is.
//
// EVERY UNCERTAIN PATH FALLS TO ActorUser, the stricter class. A trusted
// aggregator skips visibility, ban and authorization entirely, so resolving a
// failed lookup UPWARD would hand the widest privileges in the system to
// whoever managed to make the lookup fail. Guessing downward costs an
// aggregator some refused posts until the lookup recovers — and CreatePost
// already made exactly this choice (service.go step 3), so the engine agreeing
// with it is also what keeps the write path and the ingestion path from
// disagreeing about who someone is.
func (d *AdmissionEngineDecider) classify(ctx context.Context, authorDID string) ActorClass {
	// The trusted set is checked FIRST, which is both the cheaper path and the
	// only one that costs nothing: it is an in-memory set resolved at
	// construction, so a trusted actor never pays for a database lookup to
	// learn what the process already knew.
	if d.deps.TrustedAggregatorDIDs[authorDID] {
		return ActorTrustedAggregator
	}

	// With no aggregator collaborators wired — a deployment with no aggregator
	// support at all — nobody can be classified as one, which is the strict
	// answer rather than a degraded one.
	if d.deps.Aggregators == nil || d.deps.Authorizer == nil {
		return ActorUser
	}

	registered, err := d.deps.Aggregators.IsAggregator(ctx, authorDID)
	if err != nil {
		log.Printf("[ADMISSION-DECIDER] Warning: classifying %s fell back to the user class, IsAggregator failed: %v",
			authorDID, err)
		return ActorUser
	}
	if registered {
		return ActorRegisteredAggregator
	}
	return ActorUser
}
