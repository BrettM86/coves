package posts

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The acceptance engine's queue driver (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6,
// §8): what turns "the engine can settle a subject" into "every stranded
// subject eventually gets settled".
//
// T0, because the driver is a scheduling policy over two interfaces and nothing
// it decides needs a database to be true. The backlog QUERY it drives is T1
// (internal/db/postgres/admission_queue_repo_test.go); this is the half that
// decides what to do with the answer.
//
// # THE THREE PROPERTIES, AND WHY EACH IS A REAL FAILURE MODE
//
//   - ONE SUBJECT AT A TIME, GROUPED BY COMMUNITY. swapCommit is repo-global, so
//     two workers on one community's repo starve each other's writes — task 4
//     recorded this before the driver existed. v1 satisfies it trivially with a
//     single goroutine, but the ORDER still matters: the partition a worker pool
//     would eventually shard on has to already be the shape of the data.
//   - A DEFERRAL IS NOT A RETRY SIGNAL. EngineDeferred is the common outcome —
//     an expired token, an undecoded CID, an unreachable lookup — and a driver
//     that re-offered it immediately would hammer a PDS that is already
//     unhappy. This is the difference between a queue and a spin loop.
//   - ONE BAD SUBJECT MUST NOT STOP THE PASS. An error on one row aborting the
//     loop means one poisoned admission freezes every other community's posts,
//     and the symptom appears as "posts stopped becoming visible" with nothing
//     pointing at the row that caused it.

// fakeSubjects is the backlog.
type fakeSubjects struct {
	batches [][]PendingSubject
	err     error

	limits []int
	calls  int
}

func (f *fakeSubjects) ListPendingSubjects(_ context.Context, limit int) ([]PendingSubject, error) {
	f.limits = append(f.limits, limit)
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	batch := f.batches[0]
	if len(f.batches) > 1 {
		f.batches = f.batches[1:]
	}
	return batch, nil
}

// fakeEngine records the subjects it was handed, in order, and answers each
// with a scripted outcome.
type fakeEngine struct {
	mu       sync.Mutex
	seen     []PendingSubject
	outcomes map[string]EngineOutcome
	errs     map[string]error
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		outcomes: map[string]EngineOutcome{},
		errs:     map[string]error{},
	}
}

func (e *fakeEngine) ProcessAdmission(_ context.Context, communityDID, postURI string) (EngineOutcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, PendingSubject{CommunityDID: communityDID, PostURI: postURI})
	if err := e.errs[postURI]; err != nil {
		return EngineDeferred, err
	}
	if outcome, ok := e.outcomes[postURI]; ok {
		return outcome, nil
	}
	return EngineAccepted, nil
}

func (e *fakeEngine) uris() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	uris := make([]string, 0, len(e.seen))
	for _, subject := range e.seen {
		uris = append(uris, subject.PostURI)
	}
	return uris
}

func (e *fakeEngine) communityOrder() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var order []string
	for _, subject := range e.seen {
		if len(order) == 0 || order[len(order)-1] != subject.CommunityDID {
			order = append(order, subject.CommunityDID)
		}
	}
	return order
}

// queueClock is a mutable instant the driver reads through Clock.
type queueClock struct{ at time.Time }

func (c *queueClock) now() Clock              { return func() time.Time { return c.at } }
func (c *queueClock) advance(d time.Duration) { c.at = c.at.Add(d) }
func newQueueClock() *queueClock              { return &queueClock{at: time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)} }
func subject(community, rkey string) PendingSubject {
	return PendingSubject{
		CommunityDID: community,
		PostURI:      "at://did:plc:queueauthor/social.coves.community.postv2/" + rkey,
		CreatedAt:    time.Date(2026, 8, 8, 8, 0, 0, 0, time.UTC),
	}
}

func TestQueueDriver_ProcessesEverySubjectOncePerPass(t *testing.T) {
	t.Parallel()

	clock := newQueueClock()
	first, second := subject("did:plc:qa", "a"), subject("did:plc:qa", "b")
	subjects := &fakeSubjects{batches: [][]PendingSubject{{first, second}}}
	engine := newFakeEngine()

	report, err := NewQueueDriver(subjects, engine, clock.now(), WithQueueBatchSize(10)).
		RunPass(context.Background())

	require.NoError(t, err)
	assert.Equal(t, []string{first.PostURI, second.PostURI}, engine.uris(),
		"every listed subject must be offered to the engine, in the order the backlog returned them")
	assert.Equal(t, 2, report.Listed)
	assert.Equal(t, 2, report.Processed)
	assert.Equal(t, 2, report.Settled)
	assert.Equal(t, []int{10}, subjects.limits,
		"the batch size must reach the query: a driver that listed the whole backlog would hold one transaction open across a table that grows forever")
}

func TestQueueDriver_GroupsWorkByCommunity(t *testing.T) {
	t.Parallel()

	clock := newQueueClock()
	// Interleaved on the way in — the backlog is ordered by age across
	// communities, so this is the shape it genuinely returns.
	subjects := &fakeSubjects{batches: [][]PendingSubject{{
		subject("did:plc:qa", "a1"),
		subject("did:plc:qb", "b1"),
		subject("did:plc:qa", "a2"),
		subject("did:plc:qb", "b2"),
	}}}
	engine := newFakeEngine()

	_, err := NewQueueDriver(subjects, engine, clock.now()).RunPass(context.Background())
	require.NoError(t, err)

	// Each community's subjects arrive contiguously. In v1 the single goroutine
	// already makes this safe, so what the assertion protects is the NEXT
	// version: the moment this grows a worker pool, the shard key has to be the
	// community, and a driver whose output interleaves communities is one that
	// would be sharded on nothing.
	order := engine.communityOrder()
	assert.Len(t, order, 2,
		"a community's subjects must be contiguous; interleaving them (%v) means a future worker pool has no partition to shard on and would run two writers against one repo, where swapCommit is repo-global", order)
	assert.ElementsMatch(t, []string{"did:plc:qa", "did:plc:qb"}, order)
	assert.Len(t, engine.uris(), 4, "grouping must not drop or duplicate work")
}

func TestQueueDriver_DoesNotTouchOneSubjectTwiceInAPass(t *testing.T) {
	t.Parallel()

	clock := newQueueClock()
	hot := subject("did:plc:qa", "hot")
	// The same subject twice in one listing: exactly what a duplicated row or a
	// racing edit produces, and the shape §8's edit-debounce has to survive.
	subjects := &fakeSubjects{batches: [][]PendingSubject{{hot, hot, subject("did:plc:qa", "cold")}}}
	engine := newFakeEngine()
	engine.outcomes[hot.PostURI] = EngineDeferred

	report, err := NewQueueDriver(subjects, engine, clock.now()).RunPass(context.Background())
	require.NoError(t, err)

	seen := 0
	for _, uri := range engine.uris() {
		if uri == hot.PostURI {
			seen++
		}
	}
	assert.Equalf(t, 1, seen,
		"a subject was processed %d times in one pass. §8's edit-debounce is exactly this rule: a post being edited in a storm coalesces "+
			"into one pending_reacceptance row, and a driver that re-decided it per occurrence would re-run the whole policy per keystroke", seen)
	assert.Equal(t, 1, report.Deferred)
}

func TestQueueDriver_HoldsADeferredSubjectBackUntilItsBackoffElapses(t *testing.T) {
	t.Parallel()

	clock := newQueueClock()
	stuck := subject("did:plc:qa", "stuck")
	healthy := subject("did:plc:qb", "healthy")
	// The same backlog every pass — which is what the query genuinely returns
	// while the subject stays undecided.
	subjects := &fakeSubjects{batches: [][]PendingSubject{{stuck, healthy}}}
	engine := newFakeEngine()
	engine.outcomes[stuck.PostURI] = EngineDeferred

	driver := NewQueueDriver(subjects, engine, clock.now(),
		WithQueueBackoff(time.Minute, 10*time.Minute))
	ctx := context.Background()

	first, err := driver.RunPass(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, first.Processed)
	assert.Equal(t, 1, first.Deferred)

	// Immediately again. A community with a dead token defers on every pass, and
	// a driver that re-offered it at once would turn a scheduled job into a spin
	// against a PDS that is already failing.
	clock.advance(time.Second)
	second, err := driver.RunPass(ctx)
	require.NoError(t, err)

	countIn := func(uris []string, target string) int {
		n := 0
		for _, uri := range uris {
			if uri == target {
				n++
			}
		}
		return n
	}
	assert.Equalf(t, 1, countIn(engine.uris(), stuck.PostURI),
		"a subject deferred one second ago was offered again; the backoff exists so one wedged community does not become a hot loop")

	// The healthy subject keeps flowing. This is the half that makes the backoff
	// per-SUBJECT rather than global: one community's dead credentials must not
	// stall everyone else's posts, which is precisely what a global pause does.
	assert.Equalf(t, 2, countIn(engine.uris(), healthy.PostURI),
		"the backoff must be per-subject: a healthy subject stopped being processed because a DIFFERENT one deferred")
	assert.Equal(t, 1, second.Processed, "only the healthy subject was due")

	// Past the backoff, the deferred subject is due again — a deferral is "look
	// again later", and a driver that never looked again would strand it forever.
	clock.advance(2 * time.Minute)
	third, err := driver.RunPass(ctx)
	require.NoError(t, err)
	assert.Equalf(t, 2, countIn(engine.uris(), stuck.PostURI),
		"a deferred subject must be retried once its backoff elapses; holding it forever is how a recovered community's posts stay pending")
	assert.Equal(t, 2, third.Processed)
}

func TestQueueDriver_AFailedSubjectDoesNotAbortThePass(t *testing.T) {
	t.Parallel()

	clock := newQueueClock()
	poison := subject("did:plc:qa", "poison")
	after := subject("did:plc:qb", "after")
	subjects := &fakeSubjects{batches: [][]PendingSubject{{poison, after}}}
	engine := newFakeEngine()
	engine.errs[poison.PostURI] = errors.New("the decision blew up")

	report, err := NewQueueDriver(subjects, engine, clock.now()).RunPass(context.Background())

	require.NoError(t, err,
		"a pass is only an error when the BACKLOG could not be read; a subject that failed is a counted outcome")
	assert.Containsf(t, engine.uris(), after.PostURI,
		"the pass stopped at the first failing subject. One poisoned admission would then freeze every other community's posts, "+
			"and the symptom — 'posts stopped appearing' — points nowhere near the row that caused it")
	assert.Equal(t, 1, report.Failed)
	assert.Equal(t, 1, report.Settled)

	// Failures and deferrals are counted apart because they mean opposite things
	// to whoever is deciding whether to page: a pass that defers everything is
	// usually credentials and will clear, a pass that FAILS everything is a bug.
	assert.Zero(t, report.Deferred, "an error is not a deferral")
}

func TestQueueDriver_ReportsAnUnreadableBacklog(t *testing.T) {
	t.Parallel()

	clock := newQueueClock()
	subjects := &fakeSubjects{err: errors.New("admissions table unreachable")}

	report, err := NewQueueDriver(subjects, newFakeEngine(), clock.now()).RunPass(context.Background())

	require.Error(t, err, "a backlog that could not be read is the one failure a pass has nothing to do about")
	assert.Zero(t, report.Processed)
}

func TestQueueDriver_SnapshotReportsTheBacklogAndTheLastPass(t *testing.T) {
	t.Parallel()

	clock := newQueueClock()
	oldest := subject("did:plc:qa", "oldest")
	oldest.CreatedAt = clock.at.Add(-90 * time.Minute)
	newer := subject("did:plc:qa", "newer")
	newer.CreatedAt = clock.at.Add(-5 * time.Minute)

	subjects := &fakeSubjects{batches: [][]PendingSubject{{oldest, newer}}}
	engine := newFakeEngine()
	engine.outcomes[newer.PostURI] = EngineDeferred

	driver := NewQueueDriver(subjects, engine, clock.now())

	// Before the first pass, LastPassAt is nil — the one field that distinguishes
	// "the driver has never run" from "the driver ran and found nothing". A
	// driver that has died produces no error and no log; it simply stops, and
	// every symptom shows up somewhere else as posts that never become visible.
	require.Nil(t, driver.Snapshot().LastPassAt,
		"before any pass, lastPassAt must be nil: a zero time would read as a pass that happened at the epoch")

	_, err := driver.RunPass(context.Background())
	require.NoError(t, err)

	snapshot := driver.Snapshot()
	assert.Equal(t, 2, snapshot.PendingBacklog)
	require.NotNil(t, snapshot.OldestPendingAt)
	assert.Equal(t, oldest.CreatedAt, *snapshot.OldestPendingAt,
		"the oldest undecided subject's age is the queue's only early warning: a backlog that is merely big is a busy instance, one whose oldest entry keeps ageing is an engine that has stopped settling anything")
	require.NotNil(t, snapshot.LastPassAt)
	assert.Equal(t, clock.at, *snapshot.LastPassAt)
	assert.Equal(t, 1, snapshot.LastPassDeferred)
	assert.Zero(t, snapshot.LastPassFailed)
}

func TestQueueDriver_OverFetchesPastBackedOffSubjects(t *testing.T) {
	t.Parallel()

	// BATCH-PREFIX STARVATION. The backlog is ordered oldest-first, so the
	// subjects most likely to be stuck — a community whose credentials expired
	// weeks ago, a post whose content never decoded — are exactly the ones that
	// sit at the front of it forever. Ask for LIMIT rows, get LIMIT stuck ones,
	// skip all of them for backoff, and the pass does nothing. Every pass. The
	// queue is not empty and the driver is not broken; it simply never sees past
	// its own prefix, and a healthy post behind them is never decided.
	//
	// The fix stays in the DRIVER: over-fetch and keep skipping until the batch
	// is filled. Pushing the backoff into the query would mean persisting
	// retry-not-before, and the backoff is deliberately a disposable in-memory
	// hint that a restart may forget (see QueueDriver.deferrals).
	clock := newQueueClock()

	const batch = 3
	stuck := make([]PendingSubject, batch)
	for i := range stuck {
		stuck[i] = subject("did:plc:qstuck", fmt.Sprintf("stuck%d", i))
	}
	young := subject("did:plc:qyoung", "young")

	subjects := &fakeSubjects{batches: [][]PendingSubject{append(append([]PendingSubject{}, stuck...), young)}}
	engine := newFakeEngine()
	for _, s := range stuck {
		engine.outcomes[s.PostURI] = EngineDeferred
	}

	driver := NewQueueDriver(subjects, engine, clock.now(),
		WithQueueBatchSize(batch), WithQueueBackoff(time.Minute, 10*time.Minute))
	ctx := context.Background()

	// Pass one settles nothing and backs the whole prefix off.
	first, err := driver.RunPass(ctx)
	require.NoError(t, err)
	require.Equal(t, batch, first.Deferred, "fixture: the whole prefix must defer")

	// Pass two, inside the backoff window. Every stuck subject is held back, so
	// a driver that asked for exactly LIMIT rows would process nothing at all.
	clock.advance(time.Second)
	second, err := driver.RunPass(ctx)
	require.NoError(t, err)

	assert.Contains(t, engine.uris(), young.PostURI,
		"the young subject was never reached: the pass asked for exactly the batch size, got a prefix of backed-off "+
			"subjects, and skipped all of them — so a stuck community at the head of the backlog starves everything behind it forever")
	assert.Equalf(t, 1, second.Processed,
		"the pass must fill its batch past the held-back prefix, processing the young subject and nothing else; got %d processed", second.Processed)

	require.GreaterOrEqual(t, len(subjects.limits), 2)
	assert.Greaterf(t, subjects.limits[len(subjects.limits)-1], batch,
		"the query must be asked for MORE than the batch size (limits seen: %v). The driver cannot skip past what it "+
			"never fetched, and the over-fetch factor is what bounds how deep a stuck prefix it can see past", subjects.limits)
}

func TestQueueDriver_PrunesDeferralsForSubjectsThatLeaveTheBacklog(t *testing.T) {
	t.Parallel()

	// A deferral outlives its subject. The row is settled by somebody else — the
	// synchronous fast path, a firehose acceptance, a moderator's removal — and
	// it stops being listed, but its entry stays in the map forever. On a busy
	// instance that map is then an unbounded leak keyed by every subject the
	// driver ever deferred, held for the lifetime of the process.
	//
	// It is also wrong on re-entry: a subject that leaves the backlog and comes
	// back (an edit reopening an accepted post) arrives carrying a stale backoff
	// it did nothing to earn, and waits out a delay that was about a completely
	// different decision.
	clock := newQueueClock()
	leaving := subject("did:plc:qleave", "leaving")
	staying := subject("did:plc:qstay", "staying")

	subjects := &fakeSubjects{batches: [][]PendingSubject{
		{leaving, staying},
		// Second pass: `leaving` was settled elsewhere and is gone from the
		// backlog. `staying` is still owed a decision.
		{staying},
	}}
	engine := newFakeEngine()
	engine.outcomes[leaving.PostURI] = EngineDeferred
	engine.outcomes[staying.PostURI] = EngineDeferred

	driver := NewQueueDriver(subjects, engine, clock.now(), WithQueueBackoff(time.Minute, 10*time.Minute))
	ctx := context.Background()

	_, err := driver.RunPass(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, driver.Snapshot().DeferredSubjects, "fixture: both subjects must be holding a deferral")

	clock.advance(time.Second)
	_, err = driver.RunPass(ctx)
	require.NoError(t, err)

	assert.Equalf(t, 1, driver.Snapshot().DeferredSubjects,
		"the deferral for a subject that has left the backlog was kept. The map is keyed by subject and never swept, so "+
			"it grows for the life of the process — and a subject that comes back (an edit reopening an accepted post) "+
			"inherits a stale backoff it did nothing to earn")
}
