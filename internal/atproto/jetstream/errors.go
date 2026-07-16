package jetstream

import "errors"

// ErrPermanentEvent marks a handler failure as permanent: the event can never
// succeed no matter how many times it is retried (validation rejection,
// security check failure, structurally invalid record). Consumers wrap such
// rejections with this sentinel:
//
//	return fmt.Errorf("%w: repository DID doesn't match community DID", jetstream.ErrPermanentEvent)
//
// The connector checks errors.Is(err, ErrPermanentEvent) and skips both the
// in-line retries and the redrive budget — the event is dead-lettered already
// exhausted, kept only for forensics. Errors NOT wrapped with this sentinel
// are treated as transient (retried in-line, then redriven).
//
// This matters for availability: before this distinction, an adversary
// emitting invalid records could stall a consumer ~4s per event on pointless
// retries and grow the dead letter queue with rows that redrive ten times.
var ErrPermanentEvent = errors.New("permanent event failure")

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
