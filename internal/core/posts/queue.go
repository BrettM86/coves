package posts

import (
	"context"
	"time"
)

// RED STUB (task 5, cycle 2). Signatures only; every body returns zero values.

// The acceptance engine's driver: the thing that decides WHEN the engine runs
// and on what (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6, §8).
//
// The engine settles one subject. Nothing until now decided which subjects, in
// what order, or how often — the fast path and the firehose consumer both push
// work at it, and neither can see a subject that was left pending because a
// credential expired or a lookup blipped. This is the pull side: a periodic pass
// over the undecided backlog that eventually reaches every stranded row.
//
// # IT IS A SINGLE GOROUTINE, AND THAT IS THE PER-COMMUNITY SERIALIZATION
//
// Task 4 recorded the requirement: serialize the queue per community DID,
// because swapCommit is repo-global and sibling workers on one busy community
// would starve each other's removals. In v1 that requirement is satisfied
// trivially and completely — one goroutine walks one ordered list, so no two
// subjects of any community are ever in flight together. The ordering matters
// anyway: the list is grouped by community so that when this DOES grow a worker
// pool, the partition it has to shard on is already the shape of the data,
// rather than something a later change has to introduce.
//
// # A DEFERRAL IS NOT A FAILURE, AND MUST NOT BECOME A SPIN
//
// EngineDeferred is the common outcome, not the exceptional one: a community
// whose token expired, a post whose content CID has not been decoded yet, a
// policy lookup that could not be reached. Every one of those is "look again
// later", and a driver that took it as "try again now" would turn one wedged
// subject into a hot loop against a PDS that is already unhappy. Deferred
// subjects therefore carry a per-subject backoff, and the same subject is never
// touched twice in one pass — §8's edit-debounce falls out of the same rule,
// since a post being edited in a storm coalesces into one pending_reacceptance
// row that this pass sees exactly once.

// AdmissionProcessor settles one subject. Satisfied by *AcceptanceEngine.
type AdmissionProcessor interface {
	ProcessAdmission(ctx context.Context, communityDID, postURI string) (EngineOutcome, error)
}

// PendingSubjectLister supplies the backlog. Satisfied by AdmissionRepository.
type PendingSubjectLister interface {
	ListPendingSubjects(ctx context.Context, limit int) ([]PendingSubject, error)
}

// PassReport is what one pass did, for logs and for the health surface.
//
// Deferred and Failed are counted separately because they mean opposite things
// to an operator. A pass that defers everything is usually a credential or
// connectivity problem that will clear; a pass that FAILS everything is a bug.
// Collapsing them into one number would make the first look like the second at
// exactly the moment somebody is deciding whether to page.
type PassReport struct {
	// Listed is how many subjects the backlog query returned.
	Listed int

	// Processed is how many were handed to the engine — Listed minus those the
	// backoff held back.
	Processed int

	// Settled is how many reached a verdict: accepted, rejected, removed or
	// repinned.
	Settled int

	// Deferred is how many the engine declined to decide yet.
	Deferred int

	// Failed is how many returned an error.
	Failed int

	// StartedAt is when the pass began.
	StartedAt time.Time
}

// QueueSnapshot is the driver's health surface: what the backlog looks like and
// when it was last worked.
//
// LastPassAt is the liveness signal and the reason the snapshot exists at all.
// A driver that has stopped running produces no logs and no errors — it simply
// stops, and every symptom of that appears somewhere else, as posts that never
// become visible. An operator needs one number that says the pass is happening.
type QueueSnapshot struct {
	PendingBacklog int

	// OldestPendingAt is the created_at of the oldest subject the last pass
	// listed, or nil when the backlog was empty.
	OldestPendingAt *time.Time

	// LastPassAt is nil until the first pass completes — distinguishing "the
	// driver has never run" from "the driver ran and found nothing", which look
	// identical in every other field.
	LastPassAt *time.Time

	LastPassDeferred int
	LastPassFailed   int
}

// QueueDriverOption configures the driver.
type QueueDriverOption func(*QueueDriver)

// WithQueueBatchSize bounds how many subjects one pass lists.
func WithQueueBatchSize(size int) QueueDriverOption {
	return func(d *QueueDriver) { d.batchSize = size }
}

// WithQueueBackoff sets the delay before a DEFERRED subject is offered to the
// engine again, and the ceiling that delay grows to.
//
// It is per-subject rather than global: one community with dead credentials must
// not slow down the queue for everyone else, which is exactly what a global
// backoff would do.
func WithQueueBackoff(base, max time.Duration) QueueDriverOption {
	return func(d *QueueDriver) { d.backoffBase, d.backoffMax = base, max }
}

// QueueDriver walks the undecided backlog and feeds the engine.
type QueueDriver struct {
	subjects  PendingSubjectLister
	engine    AdmissionProcessor
	now       Clock
	batchSize int

	backoffBase time.Duration
	backoffMax  time.Duration

	// deferrals holds the per-subject retry-not-before times. In-memory on
	// purpose: it is a politeness hint, not state anything is allowed to depend
	// on, so a restart that forgets it costs one extra attempt per subject and
	// nothing else.
	deferrals map[PendingSubject]time.Time

	snapshot QueueSnapshot
}

// NewQueueDriver wires the driver.
func NewQueueDriver(subjects PendingSubjectLister, engine AdmissionProcessor, now Clock, opts ...QueueDriverOption) *QueueDriver {
	d := &QueueDriver{
		subjects:  subjects,
		engine:    engine,
		now:       now,
		deferrals: make(map[PendingSubject]time.Time),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// RunPass processes one batch of the backlog.
//
// It returns an error ONLY when the backlog itself could not be read. A subject
// the engine failed on is counted and the pass continues: one poisonous row
// must not stop every other community's posts from being decided, which is what
// an early return would do — and the row is still in the backlog next pass.
func (d *QueueDriver) RunPass(ctx context.Context) (PassReport, error) {
	return PassReport{}, nil
}

// Snapshot returns the driver's health surface as of the last completed pass.
func (d *QueueDriver) Snapshot() QueueSnapshot {
	return QueueSnapshot{}
}
