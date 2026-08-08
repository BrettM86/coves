package posts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
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
//     to whoever made the lookup fail. But guessing DOWNWARD is not free here
//     either, which is where this path parts company with CreatePost: a lookup
//     that could not be made is reported as UNDECIDED rather than resolved to
//     the stricter class, because this decision gets written into the admission
//     row with redrivable = false. See classify for the full asymmetry.
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

// AdmissionCounter is the narrow slice of AdmissionRepository the quota needs.
type AdmissionCounter interface {
	CountRecentAdmissions(ctx context.Context, communityDID, authorDID string, since time.Time) (int, error)
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
	// Its Limits govern the firehose quota below as well, so a local author and
	// a remote one are held to the same number.
	Policy AdmissionPolicy

	// Admissions counts an author's recent admitted posts in a community — the
	// firehose path's quota substrate (§8). nil disables the quota, which is
	// the right default for a deployment that hosts no communities and therefore
	// decides nothing.
	Admissions AdmissionCounter

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

	actor, err := d.classify(ctx, post.AuthorDID)
	if err != nil {
		return undecided(fmt.Errorf("deciding %s for %s: %w", postURI, communityDID, err))
	}

	decision, err := evaluateAdmissionPolicy(ctx, admissionDeps{
		communities: d.deps.Communities,
		bans:        d.deps.Policy.Bans,
		aggregators: d.deps.Authorizer,
		ledger:      d.deps.Policy.Ledger,
		limits:      d.deps.Policy.Limits,
		now:         d.deps.Policy.Now,
	}, AdmissionRequest{
		Actor:     actor,
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
	if err != nil || !decision.Admitted() {
		return decision, err
	}

	return d.applyQuota(ctx, communityDID, post.AuthorDID, actor, decision)
}

// applyQuota is the firehose path's §8 submission limit, and the last thing
// between an admitted decision and the engine writing an acceptance.
//
// IT COUNTS ADMISSION ROWS, NOT LEDGER ROWS, and that substitution is the whole
// reason it exists separately from admitPost's step 6. post_submissions is
// written by CreatePost, so a post that arrived over the firehose from an author
// on another server has no ledger row and never will — counting it would hold
// LOCAL users to the limit while exempting precisely the remote ones §8 is
// about. Anyone can write unlimited postv2 records naming any community, and the
// admission layer is what absorbs that.
//
// THE LIMIT IS THE SAME NUMBER the write path uses, taken from the same config,
// so an author is held to one quota rather than to two that drift.
//
// ACTOR CLASSES ARE TREATED EXACTLY AS admitPost TREATS THEM: only ActorUser is
// metered. A registered aggregator is already governed by its own hourly quota
// inside ValidateAggregatorPost, and applying this as well would silently halve
// an authorized aggregator's throughput; a trusted one has never had a
// submission limit, and inventing one here would stop the bridge dead at a
// number nobody chose.
func (d *AdmissionEngineDecider) applyQuota(
	ctx context.Context, communityDID, authorDID string, actor ActorClass, decision AdmissionDecision,
) (AdmissionDecision, error) {
	if actor != ActorUser || d.deps.Admissions == nil {
		return decision, nil
	}
	limits := d.deps.Policy.Limits
	if limits.MaxPerAuthorPerCommunity <= 0 || limits.Window <= 0 {
		return decision, nil
	}

	since := d.deps.Policy.Now().Add(-limits.Window)
	count, err := d.deps.Admissions.CountRecentAdmissions(ctx, communityDID, authorDID, since)
	if err != nil {
		// UNDECIDED, never a refusal. The engine persists a refusal code and
		// marks it non-redrivable, so a count that could not be taken must not
		// become a permanent rate-limit verdict on somebody's post.
		return undecided(fmt.Errorf("counting recent admissions for %s in %s: %w", authorDID, communityDID, err))
	}

	// EXCEEDS, not reaches. The subject being decided already has its own
	// admission row — the consumer opens it before the engine ever runs — so it
	// is inside this count, exactly as admitPost's reservation is inside its
	// own. Comparing with >= would refuse the author's very first post.
	if count > limits.MaxPerAuthorPerCommunity {
		return AdmissionDecision{Code: DecisionRateLimitExceeded}, nil
	}
	return decision, nil
}

// classify decides what class of actor the author is, or reports that it could
// not.
//
// A FAILED LOOKUP IS UNDECIDED HERE, AND A DOWNGRADE ON THE WRITE PATH. The two
// answers are deliberate opposites, and the difference is what each caller does
// with the result afterwards. CreatePost is talking to a live client: a
// downgrade to ActorUser applies the strict checks, costs an aggregator a few
// posts until the table recovers, and hands back something the caller can
// retry. Nothing is written down.
//
// The engine writes its verdict INTO the admission row, and a policy refusal is
// stamped redrivable = false — terminal, never revisited by the redrive pass.
// The same downgrade there does not cost a retry; it permanently marks a post
// refused for a reason that was never true, because a table was briefly
// unreachable, and nothing in the system would ever look at it again.
//
// So the rule this encodes is: A DECISION THAT PERSISTS MAY ONLY BE MADE FROM
// AN ANSWER THAT WAS ACTUALLY OBTAINED. Guessing upward is never available
// either — a trusted aggregator skips visibility, ban and authorization
// entirely, so resolving uncertainty in that direction would hand the widest
// privileges in the system to whoever could make a lookup fail.
func (d *AdmissionEngineDecider) classify(ctx context.Context, authorDID string) (ActorClass, error) {
	// The trusted set is checked FIRST, which is both the cheaper path and the
	// only one that costs nothing: it is an in-memory set resolved at
	// construction, so a trusted actor never pays for a database lookup to
	// learn what the process already knew.
	if d.deps.TrustedAggregatorDIDs[authorDID] {
		return ActorTrustedAggregator, nil
	}

	// With no aggregator collaborators wired — a deployment with no aggregator
	// support at all — nobody can be classified as one. That is a CONFIGURED
	// fact rather than a failed lookup, so it answers rather than defers.
	if d.deps.Aggregators == nil || d.deps.Authorizer == nil {
		return ActorUser, nil
	}

	registered, err := d.deps.Aggregators.IsAggregator(ctx, authorDID)
	if err != nil {
		return "", fmt.Errorf("classifying %s: %w", authorDID, err)
	}
	if registered {
		return ActorRegisteredAggregator, nil
	}
	return ActorUser, nil
}
