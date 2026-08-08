package posts

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

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

	// deferrals holds the per-subject backoff state. In-memory on purpose: it is
	// a politeness hint, not state anything is allowed to depend on, so a
	// restart that forgets it costs one extra attempt per subject and nothing
	// else.
	//
	// It is keyed by (community, post) rather than by the whole PendingSubject
	// because the third field is a time.Time read back from Postgres, and Go
	// compares those by wall clock AND monotonic reading AND location. Two
	// reads of one unchanged row can therefore produce values that are equal to
	// a human and distinct to a map, which would silently defeat the backoff.
	deferrals map[subjectKey]deferral

	// mu guards deferrals and snapshot. Snapshot is read by the health handler
	// on an HTTP goroutine while RunPass is writing on the job goroutine, so
	// this is a genuine race rather than a defensive one.
	mu       sync.Mutex
	snapshot QueueSnapshot
}

// subjectKey identifies one subject by the two fields that actually name it.
type subjectKey struct {
	communityDID string
	postURI      string
}

func keyOf(subject PendingSubject) subjectKey {
	return subjectKey{communityDID: subject.CommunityDID, postURI: subject.PostURI}
}

// deferral is how long one subject is held back, and until when.
//
// The delay is carried alongside the deadline so it can GROW: a subject that
// defers repeatedly is one whose community is wedged, and re-offering it on a
// fixed interval would keep a steady trickle of doomed requests pointed at a
// PDS that is already failing.
type deferral struct {
	until time.Time
	delay time.Duration
}

// Default queue bounds, applied when the corresponding option is not given.
//
// The batch bound is not a nicety: this query runs on a timer against a table
// that grows with every submission the instance has ever seen, so a driver
// built without one must still not ask for the whole backlog.
const (
	defaultQueueBatchSize   = 100
	defaultQueueBackoffBase = time.Minute
	defaultQueueBackoffMax  = 15 * time.Minute
)

// NewQueueDriver wires the driver.
func NewQueueDriver(subjects PendingSubjectLister, engine AdmissionProcessor, now Clock, opts ...QueueDriverOption) *QueueDriver {
	d := &QueueDriver{
		subjects:    subjects,
		engine:      engine,
		now:         now,
		batchSize:   defaultQueueBatchSize,
		backoffBase: defaultQueueBackoffBase,
		backoffMax:  defaultQueueBackoffMax,
		deferrals:   make(map[subjectKey]deferral),
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
	startedAt := d.now()
	report := PassReport{StartedAt: startedAt}

	subjects, err := d.subjects.ListPendingSubjects(ctx, d.batchSize)
	if err != nil {
		// The one failure a pass has nothing to do about. Every other outcome
		// below is per-subject and counted; this one means there is no work
		// list to count against.
		return report, fmt.Errorf("listing the acceptance backlog: %w", err)
	}
	report.Listed = len(subjects)

	for _, subject := range groupByCommunity(subjects) {
		if d.heldBack(subject) {
			continue
		}

		outcome, err := d.engine.ProcessAdmission(ctx, subject.CommunityDID, subject.PostURI)
		report.Processed++

		// The ERROR is checked before the outcome, because a failing engine
		// returns EngineDeferred alongside it and reading the outcome first
		// would file every failure as a deferral — collapsing the two numbers an
		// operator uses to decide whether this is credentials or a bug.
		switch {
		case err != nil:
			report.Failed++
			log.Printf("[ACCEPTANCE-QUEUE] Warning: %s in %s could not be settled: %v",
				subject.PostURI, subject.CommunityDID, err)
			// NOT backed off. A failure is unexplained, so the driver has no
			// basis for guessing how long to wait; the next pass re-lists it and
			// the row is still there. Deferral is the engine SAYING "later".
		case outcome == EngineDeferred:
			report.Deferred++
			d.deferSubject(subject, startedAt)
		default:
			report.Settled++
			d.clearDeferral(subject)
		}
	}

	d.record(subjects, report, startedAt)
	return report, nil
}

// Snapshot returns the driver's health surface as of the last completed pass.
func (d *QueueDriver) Snapshot() QueueSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot
}

// heldBack reports whether a subject's backoff has yet to elapse.
func (d *QueueDriver) heldBack(subject PendingSubject) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	held, ok := d.deferrals[keyOf(subject)]
	return ok && d.now().Before(held.until)
}

// deferSubject holds a subject back, doubling its wait each consecutive time.
func (d *QueueDriver) deferSubject(subject PendingSubject, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := keyOf(subject)
	delay := d.backoffBase
	if previous, ok := d.deferrals[key]; ok && previous.delay > 0 {
		delay = previous.delay * 2
	}
	if delay > d.backoffMax {
		delay = d.backoffMax
	}
	d.deferrals[key] = deferral{until: at.Add(delay), delay: delay}
}

// clearDeferral forgets a settled subject, so the map tracks the backlog rather
// than the history of everything the driver has ever seen.
func (d *QueueDriver) clearDeferral(subject PendingSubject) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.deferrals, keyOf(subject))
}

// record publishes the pass's health surface.
func (d *QueueDriver) record(subjects []PendingSubject, report PassReport, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	snapshot := QueueSnapshot{
		PendingBacklog:   report.Listed,
		LastPassDeferred: report.Deferred,
		LastPassFailed:   report.Failed,
	}
	// Taken as a MINIMUM rather than as subjects[0], even though the query
	// orders by age. The oldest entry's age is the queue's only early warning,
	// and deriving it from an ordering assumption would make it silently wrong
	// the first time anything reorders the list.
	for i, subject := range subjects {
		if i == 0 || subject.CreatedAt.Before(*snapshot.OldestPendingAt) {
			oldest := subject.CreatedAt
			snapshot.OldestPendingAt = &oldest
		}
	}
	passedAt := at
	snapshot.LastPassAt = &passedAt

	d.snapshot = snapshot
}

// groupByCommunity returns the subjects with each community's contiguous, and
// with duplicates dropped.
//
// GROUPING. swapCommit is repo-global, so two writers on one community's repo
// starve each other — task 4 recorded that before this driver existed. A single
// goroutine satisfies it today whatever the order; what the grouping protects is
// the NEXT version, where a worker pool has to shard on something, and the only
// safe partition is the community. Output that interleaved communities would be
// output with no partition in it.
//
// DEDUPLICATION. A subject appearing twice in one listing is what a racing edit
// produces, and §8's edit-debounce is exactly this rule: a post edited in a
// storm coalesces into one pending_reacceptance row, and a driver that re-decided
// it per occurrence would re-run the whole policy per keystroke.
//
// The order WITHIN a community is preserved, and so is the order BETWEEN them
// (first appearance wins), so the query's oldest-first discipline survives.
func groupByCommunity(subjects []PendingSubject) []PendingSubject {
	var order []string
	byCommunity := make(map[string][]PendingSubject)
	seen := make(map[subjectKey]bool, len(subjects))

	for _, subject := range subjects {
		if seen[keyOf(subject)] {
			continue
		}
		seen[keyOf(subject)] = true

		if _, known := byCommunity[subject.CommunityDID]; !known {
			order = append(order, subject.CommunityDID)
		}
		byCommunity[subject.CommunityDID] = append(byCommunity[subject.CommunityDID], subject)
	}

	grouped := make([]PendingSubject, 0, len(seen))
	for _, communityDID := range order {
		grouped = append(grouped, byCommunity[communityDID]...)
	}
	return grouped
}
