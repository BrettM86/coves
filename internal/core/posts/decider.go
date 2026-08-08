package posts

import (
	"context"
)

// RED STUB (task 5, cycle 2). Signatures only; the body is GREEN's.

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

// DecideAdmission implements AdmissionDecider.
func (d *AdmissionEngineDecider) DecideAdmission(ctx context.Context, communityDID, postURI string) (AdmissionDecision, error) {
	return AdmissionDecision{}, nil
}
