package jetstream

import (
	"errors"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Canonical consumer names. These key the persisted rows in
// jetstream_cursors and jetstream_dead_letters, so they MUST stay stable
// across releases: renaming one silently orphans its cursor (the consumer
// restarts at live tail — the exact data loss cursors exist to prevent) and
// strands its dead letter backlog under the old name.
const (
	ConsumerUsers       = "users"
	ConsumerCommunities = "communities"
	ConsumerPosts       = "posts"
	ConsumerVotes       = "votes"
	ConsumerComments    = "comments"
	ConsumerAggregators = "aggregators"
)

// The connector sorts every handler failure into one of three classes, and the
// class decides how much of the consumer's lane the failure is allowed to
// spend. A Jetstream consumer is a SERIAL read loop: while one event is being
// retried nothing behind it is indexed, so every class below is a statement
// about availability as much as about correctness.
//
//   - TRANSIENT (the default: any error not wrapped with a sentinel below) —
//     infrastructure hiccups: Postgres blip, a PDS that timed out. Retried
//     in-line (200ms, 1s, 3s), then dead-lettered with the full redrive budget.
//     In-line retries are worth their lane time here because the failure has
//     nothing to do with the event and usually clears within seconds.
//
//   - PERMANENT (ErrPermanentEvent) — the event itself is invalid and a replay
//     of the identical bytes fails identically. No retries, dead-lettered with
//     its redrive budget already exhausted, kept for forensics only.
//
//   - UNRESOLVED REFERENCE (ErrUnresolvedReference) — the event is well-formed
//     but names something this AppView has not indexed: a vote on a post it has
//     never seen, a subscription to a community whose profile has not arrived,
//     or an authorization for an unindexed aggregator. It may converge later
//     (the referenced record arrives) or never (the reference is fabricated),
//     and the consumer cannot tell which. No in-line retries — the referenced
//     record lives in a DIFFERENT repo and will not appear in the next 4.2
//     seconds of this lane — but a small redrive budget, so the timer-driven
//     DeadLetterRedriver can converge genuine ordering cases without letting
//     fabricated references monopolize the shared worker and dead-letter store.
//
// The third class is what keeps the second from being re-opened one consumer
// at a time. Every "the referenced thing may arrive later" argument is right,
// and every one of them, expressed as a plain transient error, handed any
// repo on the network a free 4.2-second stall per event: mint a thousand
// records naming a nonexistent subject and the lane is gone for an hour. See
// docs/CONSUMER_TRUST_AUDIT.md §1.3.

// ErrPermanentEvent marks a handler failure as permanent: the event can never
// succeed no matter how many times it is retried (validation rejection,
// security check failure, structurally invalid record). Consumers wrap such
// rejections with this sentinel:
//
//	return fmt.Errorf("%w: repository DID doesn't match community DID", jetstream.ErrPermanentEvent)
//
// The connector checks errors.Is(err, ErrPermanentEvent) and skips both the
// in-line retries and the redrive budget — the event is dead-lettered already
// exhausted, kept only for forensics.
var ErrPermanentEvent = errors.New("permanent event failure")

// ErrUnresolvedReference marks a handler failure as an ordering failure: the
// event is well-formed but references a record this AppView has not indexed.
// Consumers wrap such refusals with this sentinel:
//
//	return fmt.Errorf("%w: vote subject %s is not indexed", jetstream.ErrUnresolvedReference, uri)
//
// The connector skips the in-line retries (the lane must not wait on another
// repo's delivery) and dead-letters the event with a bounded redrive budget so
// the redriver can replay genuine ordering races without giving fabricated
// references the infrastructure-failure budget.
//
// Use it ONLY for references that are syntactically valid. A subject that is
// not DID-shaped, a URI that names the wrong collection, a value a CHECK
// constraint rejects — those can never resolve and are ErrPermanentEvent.
// Wrapping them here would spend the bounded off-lane budget on a row that a
// single look at the payload could have retired.
var ErrUnresolvedReference = errors.New("unresolved reference")

// errRedriveDeferred marks a replay that acquired no new information because
// another request is already in flight or a recent result is still cached. It
// modifies an existing error class; it is not a fourth connector outcome.
// The row remains retryable, but this pass must not spend one of its attempts.
var errRedriveDeferred = errors.New("redrive deferred")

type redriveDeferredError struct {
	err error
}

func (e *redriveDeferredError) Error() string { return e.err.Error() }
func (e *redriveDeferredError) Unwrap() error { return e.err }
func (e *redriveDeferredError) Is(target error) bool {
	return target == errRedriveDeferred
}

func deferRedrive(err error) error {
	if err == nil || errors.Is(err, errRedriveDeferred) {
		return err
	}
	return &redriveDeferredError{err: err}
}

// requireDIDShaped returns an ErrPermanentEvent rejection when value is not a
// syntactically valid did:plc or did:web identifier. Those are the account DID
// methods the index schema accepts. syntax.ParseDID accepts other methods, so
// parsing alone is not enough to keep an unsupported did:key value from
// reaching a CHECK constraint and being misclassified as transient.
func requireDIDShaped(field, value string) error {
	did, err := syntax.ParseDID(value)
	if err != nil {
		return fmt.Errorf("%w: %s %q is not a DID: %v", ErrPermanentEvent, field, value, err)
	}
	if method := did.Method(); method != "plc" && method != "web" {
		return fmt.Errorf("%w: %s %q uses unsupported DID method %q (only did:plc and did:web are indexable)",
			ErrPermanentEvent, field, value, method)
	}
	return nil
}
