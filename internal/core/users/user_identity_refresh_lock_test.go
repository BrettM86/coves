//go:build integration

// Handle refreshes of one DID must run one after another in every process that
// shares the database. Every Jetstream consumer runs once per feed, the
// redriver replays through the same handlers and the sweep is a separate
// process, so two refreshes of one DID can overlap: one resolves the old
// handle, stalls, and writes it after the other stored the new one.
//
// Each "process" below is its own *sql.DB pool to the same clone, with its own
// users service, Postgres-backed caching resolver and per-DID refresh lock. The
// uncached resolver is shared: its first call for a DID can block at a barrier,
// and later calls answer differently. The test observes the lock only through
// pg_locks (an ungranted advisory lock whose tag matches one granted to another
// backend) and never computes the lock key.
package users_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
	"Coves/tests/testkit"
)

// refreshPhaseLimit bounds every phase in which a refresh is held at the
// resolver barrier (or behind a row lock), measured from the moment it got
// there. It stays well under the refresh's 5s resolve timeout so the test
// releases the holder before that timeout can decide the outcome.
const refreshPhaseLimit = 3 * time.Second

// refreshFinishLimit bounds the wait for refreshes after they are released.
const refreshFinishLimit = 5 * time.Second

const refreshPollInterval = 10 * time.Millisecond

// resolveStep is one scripted answer of the shared uncached resolver.
type resolveStep struct {
	handle string
	err    error
	// block holds the call at the barrier until the test releases it or the
	// call's context ends.
	block  bool
	panics bool
}

// barrierIdentityResolver answers each DID from its script, one step per call;
// the last step repeats. Answers are verified identities for the asked DID.
type barrierIdentityResolver struct {
	identity.Resolver

	mu      sync.Mutex
	scripts map[string][]resolveStep
	calls   map[string]int

	entered     chan struct{}
	enterOnce   sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newBarrierIdentityResolver(scripts map[string][]resolveStep) *barrierIdentityResolver {
	return &barrierIdentityResolver{
		scripts: scripts, calls: map[string]int{},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
}

func (r *barrierIdentityResolver) next(did string) (resolveStep, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	script := r.scripts[did]
	if len(script) == 0 {
		return resolveStep{}, fmt.Errorf("no scripted answer for %s", did)
	}
	step := script[min(r.calls[did], len(script)-1)]
	r.calls[did]++
	return step, nil
}

func (r *barrierIdentityResolver) Resolve(ctx context.Context, did string) (*identity.Identity, error) {
	step, err := r.next(did)
	if err != nil {
		return nil, err
	}
	if step.block {
		r.enterOnce.Do(func() { close(r.entered) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if step.panics {
		panic("scripted uncached identity resolver panic")
	}
	if step.err != nil {
		return nil, step.err
	}
	return &identity.Identity{
		DID: did, Handle: step.handle, PDSURL: testkit.Endpoints().PDS.BaseURL,
		Method: identity.MethodDNS, ResolvedAt: time.Now().UTC(),
	}, nil
}

func (r *barrierIdentityResolver) Purge(context.Context, string) error { return nil }

func (r *barrierIdentityResolver) ResolveDeclaredIdentity(_ context.Context, did string) (*identity.DeclaredIdentity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	script := r.scripts[did]
	if len(script) == 0 {
		return nil, &identity.ErrNotFound{Identifier: did, Reason: "no scripted document"}
	}
	return &identity.DeclaredIdentity{
		DID: did, DeclaredHandle: script[len(script)-1].handle, PDSURL: testkit.Endpoints().PDS.BaseURL,
	}, nil
}

func (r *barrierIdentityResolver) callCount(did string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[did]
}

func (r *barrierIdentityResolver) releaseBarrier() {
	r.releaseOnce.Do(func() { close(r.release) })
}

// refreshProcess is one process sharing the database: its own pool, service,
// caching resolver and refresh lock. Its backends carry applicationName so
// pg_locks observations can tell the processes apart.
type refreshProcess struct {
	applicationName string
	consumer        *jetstream.UserEventConsumer
}

// refreshRun is one identity refresh running in its own goroutine.
type refreshRun struct {
	applicationName string
	done            chan struct{}
	err             error
	panicked        any
}

func (run *refreshRun) returned() bool {
	select {
	case <-run.done:
		return true
	default:
		return false
	}
}

func (run *refreshRun) wait(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-run.done:
	case <-ctx.Done():
		t.Fatalf("refresh in %s did not return: %v", run.applicationName, ctx.Err())
	}
}

// result waits for the run and fails the test if it panicked.
func (run *refreshRun) result(t *testing.T, ctx context.Context) error {
	t.Helper()
	run.wait(t, ctx)
	if run.panicked != nil {
		t.Fatalf("refresh in %s panicked: %v", run.applicationName, run.panicked)
	}
	return run.err
}

// refreshRace owns the shared choreography: the clone, the observer pool used
// for fixtures and pg_locks polls, the shared resolver, and every goroutine,
// pool and row-lock transaction the test starts. Cleanup releases the barrier,
// cancels contexts, rolls back fixture transactions, waits for every refresh
// goroutine and closes the extra pools, in that order.
type refreshRace struct {
	observer *sql.DB
	service  users.UserService
	resolver *barrierIdentityResolver
	ctx      context.Context

	wg        sync.WaitGroup
	pools     []*sql.DB
	rollbacks []func()
}

func newRefreshRace(t *testing.T, scripts map[string][]resolveStep) *refreshRace {
	t.Helper()
	observer := testkit.DB(t)
	service, _ := newUserServiceOn(observer)
	ctx, cancel := context.WithCancel(context.Background())
	race := &refreshRace{
		observer: observer, service: service,
		resolver: newBarrierIdentityResolver(scripts), ctx: ctx,
	}
	t.Cleanup(func() {
		race.resolver.releaseBarrier()
		cancel()
		for _, rollback := range race.rollbacks {
			rollback()
		}
		race.wg.Wait()
		for _, pool := range race.pools {
			_ = pool.Close()
		}
	})
	return race
}

// process opens an independent pool to the same clone and builds a user
// consumer on it. wrapCaching, when set, wraps that process's caching resolver.
func (r *refreshRace) process(t *testing.T, applicationName string, wrapCaching func(identity.Resolver) identity.Resolver) *refreshProcess {
	t.Helper()
	return r.processWithLockWait(t, applicationName, wrapCaching, jetstream.IdentityRefreshLockWaitTimeout)
}

// processWithLockWait is process with the refresh lock's wait bound set to
// lockWait.
func (r *refreshRace) processWithLockWait(t *testing.T, applicationName string, wrapCaching func(identity.Resolver) identity.Resolver, lockWait time.Duration) *refreshProcess {
	t.Helper()
	var database string
	if err := r.observer.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatalf("read clone name: %v", err)
	}
	pg := testkit.Endpoints().Postgres
	pool, err := sql.Open("postgres", pg.URL(database)+"&application_name="+url.QueryEscape(applicationName))
	if err != nil {
		t.Fatalf("open pool %s on %s: %v", applicationName, pg.Redacted(database), err)
	}
	r.pools = append(r.pools, pool)
	pool.SetMaxOpenConns(3)
	if err := pool.PingContext(context.Background()); err != nil {
		t.Fatalf("ping pool %s on %s: %v", applicationName, pg.Redacted(database), err)
	}
	service, caching := newUserServiceOn(pool)
	if wrapCaching != nil {
		caching = wrapCaching(caching)
	}
	return &refreshProcess{
		applicationName: applicationName,
		consumer: jetstream.NewUserEventConsumer(service, caching,
			jetstream.WithUserUncachedIdentityResolver(r.resolver),
			jetstream.WithUserIdentityRefreshLock(jetstream.NewPostgresIdentityRefreshLock(pool, lockWait))),
	}
}

func (r *refreshRace) seedUser(t *testing.T, did, handle string) {
	t.Helper()
	if _, err := r.service.CreateUser(context.Background(), users.CreateUserRequest{
		DID: did, Handle: handle, PDSURL: testkit.Endpoints().PDS.BaseURL,
	}); err != nil {
		t.Fatalf("seed user %s: %v", did, err)
	}
}

func (r *refreshRace) storedHandle(t *testing.T, did string) string {
	t.Helper()
	user, err := r.service.GetUserByDID(context.Background(), did)
	if err != nil {
		t.Fatalf("read stored user %s: %v", did, err)
	}
	return user.Handle
}

// start delivers an identity event for did through process p in a goroutine,
// recovering a panic into the run.
func (r *refreshRace) start(ctx context.Context, p *refreshProcess, did string) *refreshRun {
	run := &refreshRun{applicationName: p.applicationName, done: make(chan struct{})}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(run.done)
		defer func() { run.panicked = recover() }()
		run.err = p.consumer.HandleIdentityEventPublic(ctx, identityEvent(did, "alice.event.test"))
	}()
	return run
}

// phase starts one absolute deadline for a blocked phase.
func (r *refreshRace) phase(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, refreshPhaseLimit)
	t.Cleanup(cancel)
	return ctx
}

// finish bounds the wait for released refreshes.
func (r *refreshRace) finish(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, refreshFinishLimit)
	t.Cleanup(cancel)
	return ctx
}

// awaitBarrier waits for holder to block at the resolver barrier and returns
// the phase context, whose deadline is measured from that moment.
func (r *refreshRace) awaitBarrier(t *testing.T, holder *refreshRun) context.Context {
	t.Helper()
	timer := time.NewTimer(refreshPhaseLimit)
	defer timer.Stop()
	select {
	case <-r.resolver.entered:
	case <-holder.done:
		t.Fatalf("refresh in %s returned before reaching the resolver barrier: err=%v panic=%v",
			holder.applicationName, holder.err, holder.panicked)
	case <-timer.C:
		t.Fatalf("refresh in %s did not reach the resolver barrier within %s", holder.applicationName, refreshPhaseLimit)
	}
	return r.phase(t)
}

// advisoryWaiting reports whether a backend of applicationName on this clone
// waits for an advisory lock whose tag is granted to a different backend.
func (r *refreshRace) advisoryWaiting(ctx context.Context, applicationName string) (bool, error) {
	var waiting bool
	err := r.observer.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_locks waiter
			JOIN pg_stat_activity activity ON activity.pid = waiter.pid
			JOIN pg_locks holder
				ON holder.locktype = 'advisory' AND holder.granted
				AND holder.pid <> waiter.pid
				AND holder.database = waiter.database
				AND holder.classid = waiter.classid
				AND holder.objid = waiter.objid
				AND holder.objsubid = waiter.objsubid
			WHERE waiter.locktype = 'advisory' AND NOT waiter.granted
				AND waiter.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND activity.application_name = $1)`, applicationName).Scan(&waiting)
	return waiting, err
}

type contenderState int

const (
	contenderUndecided contenderState = iota
	contenderReturned
	contenderWaiting
)

// watchContender polls until contender has returned or is seen waiting on the
// per-DID advisory lock, or the phase ends.
func (r *refreshRace) watchContender(t *testing.T, phase context.Context, contender *refreshRun) contenderState {
	t.Helper()
	ticker := time.NewTicker(refreshPollInterval)
	defer ticker.Stop()
	for {
		if contender.returned() {
			return contenderReturned
		}
		waiting, err := r.advisoryWaiting(phase, contender.applicationName)
		if err != nil {
			if phase.Err() != nil {
				return contenderUndecided
			}
			t.Fatalf("poll pg_locks for %s: %v", contender.applicationName, err)
		}
		if waiting {
			return contenderWaiting
		}
		select {
		case <-ticker.C:
		case <-phase.Done():
			return contenderUndecided
		}
	}
}

// awaitContenderSettled is the release condition for a held refresh: the
// contender returned or waits on the advisory lock. It reports whether the
// contender is waiting.
func (r *refreshRace) awaitContenderSettled(t *testing.T, phase context.Context, contender *refreshRun) bool {
	t.Helper()
	switch r.watchContender(t, phase, contender) {
	case contenderWaiting:
		return true
	case contenderReturned:
		return false
	default:
		t.Fatalf("refresh in %s neither returned nor waited on the per-DID advisory lock within %s",
			contender.applicationName, refreshPhaseLimit)
		return false
	}
}

// requireAdvisoryWaiter fails unless contender is seen waiting on the per-DID
// advisory lock within the phase.
func (r *refreshRace) requireAdvisoryWaiter(t *testing.T, phase context.Context, contender *refreshRun) {
	t.Helper()
	switch r.watchContender(t, phase, contender) {
	case contenderWaiting:
	case contenderReturned:
		t.Fatalf("second refresh never waited on the per-DID advisory lock: refresh in %s returned first (err=%v panic=%v)",
			contender.applicationName, contender.err, contender.panicked)
	default:
		t.Fatalf("second refresh never waited on the per-DID advisory lock: refresh in %s not seen waiting within %s",
			contender.applicationName, refreshPhaseLimit)
	}
}

// lockUserRow holds did's users row with SELECT ... FOR UPDATE in a
// transaction on the observer pool and returns the transaction's backend pid
// and its rollback. Cleanup rolls it back if the test has not.
func (r *refreshRace) lockUserRow(t *testing.T, did string) (int, func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.observer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin row-lock transaction: %v", err)
	}
	var once sync.Once
	rollback := func() {
		once.Do(func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Errorf("roll back row-lock transaction: %v", err)
			}
		})
	}
	r.rollbacks = append(r.rollbacks, rollback)
	var pid int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read row-lock transaction pid: %v", err)
	}
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT did FROM users WHERE did = $1 FOR UPDATE`, did).Scan(&locked); err != nil {
		t.Fatalf("lock users row %s: %v", did, err)
	}
	return pid, rollback
}

// awaitBlockedByRowLock waits until a backend of run's process is blocked by
// the row-lock transaction's backend.
func (r *refreshRace) awaitBlockedByRowLock(t *testing.T, phase context.Context, run *refreshRun, lockPID int) {
	t.Helper()
	ticker := time.NewTicker(refreshPollInterval)
	defer ticker.Stop()
	for {
		if run.returned() {
			t.Fatalf("refresh in %s returned before blocking on the locked users row: err=%v panic=%v",
				run.applicationName, run.err, run.panicked)
		}
		var blocked bool
		err := r.observer.QueryRowContext(phase, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND application_name = $1
					AND $2 = ANY(pg_blocking_pids(pid)))`, run.applicationName, lockPID).Scan(&blocked)
		if err != nil && phase.Err() == nil {
			t.Fatalf("poll pg_blocking_pids for %s: %v", run.applicationName, err)
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-phase.Done():
			t.Fatalf("refresh in %s not seen blocked on the locked users row within %s", run.applicationName, refreshPhaseLimit)
		}
	}
}

func (r *refreshRace) seedIdentityCacheRow(t *testing.T, ctx context.Context, identifier, did, handle string) {
	t.Helper()
	if _, err := r.observer.ExecContext(ctx, `
		INSERT INTO identity_cache (identifier, did, handle, pds_url, resolved_at, resolution_method, expires_at)
		VALUES ($1, $2, $3, $4, NOW(), 'dns', NOW() + INTERVAL '1 hour')`,
		identifier, did, handle, testkit.Endpoints().PDS.BaseURL); err != nil {
		t.Fatalf("seed identity_cache row %s: %v", identifier, err)
	}
}

func (r *refreshRace) identityCacheRows(t *testing.T, ctx context.Context, where string, arg string) int {
	t.Helper()
	var count int
	if err := r.observer.QueryRowContext(ctx, `SELECT count(*) FROM identity_cache WHERE `+where, arg).Scan(&count); err != nil {
		t.Fatalf("count identity_cache rows: %v", err)
	}
	return count
}

func assertNonPermanentRefreshError(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || errors.Is(err, jetstream.ErrPermanentEvent) {
		t.Errorf("%s error = %v, want a non-nil non-permanent error", what, err)
	}
}

// Behavior 1: a refresh that resolved the old handle and stalled must not
// overwrite the newer handle a concurrent refresh in another process stored,
// and the second refresh must not purge before it holds the lock.
func TestUserIdentityRefreshLockStalledOldAnswerDoesNotRevert(t *testing.T) {
	const did = "did:plc:refreshlockrevert"
	race := newRefreshRace(t, map[string][]resolveStep{did: {
		{block: true, handle: "alice.old.test"},
		{handle: "alice.new.test"},
	}})
	race.seedUser(t, did, "alice.seed.test")
	processA := race.process(t, "coves-refresh-a", nil)
	processB := race.process(t, "coves-refresh-b", nil)

	refreshA := race.start(race.ctx, processA, did)
	phase := race.awaitBarrier(t, refreshA)

	// A purged before blocking. The DID row caches a handle other than
	// alice.old.test, so B's initial Purge(did) cannot reach the orphan
	// alice.old.test row; only a purge of alice.old.test itself removes it.
	race.seedIdentityCacheRow(t, phase, did, did, "alice.cached.test")
	race.seedIdentityCacheRow(t, phase, "alice.old.test", did, "alice.old.test")

	refreshB := race.start(race.ctx, processB, did)
	if race.awaitContenderSettled(t, phase, refreshB) {
		if got := race.identityCacheRows(t, phase, `identifier = $1`, did); got != 1 {
			t.Errorf("cached DID row count while B waits on the lock = %d, want 1 (B purged before acquiring the lock)", got)
		}
	}
	race.resolver.releaseBarrier()

	finish := race.finish(t)
	if err := refreshA.result(t, finish); err != nil {
		t.Errorf("refresh A: %v", err)
	}
	if err := refreshB.result(t, finish); err != nil {
		t.Errorf("refresh B: %v", err)
	}
	if got := race.storedHandle(t, did); got != "alice.new.test" {
		t.Errorf("stored handle = %q, want alice.new.test", got)
	}
	if got := race.identityCacheRows(t, finish, `identifier = $1 OR handle = $1`, "alice.old.test"); got != 0 {
		t.Errorf("identity_cache rows mapping alice.old.test = %d, want 0", got)
	}
}

// Behavior 2: the refresh compares against the handle read inside the lock,
// not the one its caller read before waiting.
func TestUserIdentityRefreshLockComparesHandleReadInsideLock(t *testing.T) {
	const did = "did:plc:refreshlockreread"
	race := newRefreshRace(t, map[string][]resolveStep{did: {
		{block: true, handle: "alice.b.test"},
		{handle: "alice.a.test"},
	}})
	race.seedUser(t, did, "alice.a.test")
	processX := race.process(t, "coves-refresh-x", nil)
	processY := race.process(t, "coves-refresh-y", nil)

	refreshX := race.start(race.ctx, processX, did)
	phase := race.awaitBarrier(t, refreshX)
	refreshY := race.start(race.ctx, processY, did)
	race.awaitContenderSettled(t, phase, refreshY)
	race.resolver.releaseBarrier()

	finish := race.finish(t)
	if err := refreshX.result(t, finish); err != nil {
		t.Errorf("refresh X: %v", err)
	}
	if err := refreshY.result(t, finish); err != nil {
		t.Errorf("refresh Y: %v", err)
	}
	if got := race.storedHandle(t, did); got != "alice.a.test" {
		t.Errorf("stored handle = %q, want alice.a.test", got)
	}
}

// Behavior 3 (guard): the lock is per DID, so a refresh of another DID is not
// held up by a held one.
func TestUserIdentityRefreshLockDoesNotSerializeDifferentDIDs(t *testing.T) {
	const didX = "did:plc:refreshlockdidx"
	const didY = "did:plc:refreshlockdidy"
	race := newRefreshRace(t, map[string][]resolveStep{
		didX: {{block: true, handle: "alice.x.new.test"}},
		didY: {{handle: "bob.y.new.test"}},
	})
	race.seedUser(t, didX, "alice.x.seed.test")
	race.seedUser(t, didY, "bob.y.seed.test")
	processX := race.process(t, "coves-refresh-x", nil)
	processY := race.process(t, "coves-refresh-y", nil)

	refreshX := race.start(race.ctx, processX, didX)
	phase := race.awaitBarrier(t, refreshX)
	refreshY := race.start(race.ctx, processY, didY)
	if err := refreshY.result(t, phase); err != nil {
		t.Errorf("refresh of DID Y while DID X is held: %v", err)
	}
	if got := race.storedHandle(t, didY); got != "bob.y.new.test" {
		t.Errorf("stored handle of DID Y while DID X is held = %q, want bob.y.new.test", got)
	}
	if refreshX.returned() {
		t.Fatalf("refresh of DID X returned before release: err=%v panic=%v", refreshX.err, refreshX.panicked)
	}
	race.resolver.releaseBarrier()

	if err := refreshX.result(t, race.finish(t)); err != nil {
		t.Errorf("refresh of DID X: %v", err)
	}
	if got := race.storedHandle(t, didX); got != "alice.x.new.test" {
		t.Errorf("stored handle of DID X = %q, want alice.x.new.test", got)
	}
}

// Behavior 4 (guard): the lock is released on every exit path, so a following
// refresh of the same DID through an independent pool completes.
func TestUserIdentityRefreshLockReleasedOnEveryExitPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		did   string
		steps []resolveStep
		// failCachingPurge makes the first process's caching resolver fail
		// Purge(did) once.
		failCachingPurge bool
		// cancelHolder cancels the first refresh's context while it is held at
		// the resolver barrier.
		cancelHolder bool
		// mayPanic accepts the first refresh panicking instead of returning an
		// error.
		mayPanic bool
	}{
		{
			name:  "resolution error",
			did:   "did:plc:refreshlockresolveerror",
			steps: []resolveStep{{err: errors.New("directory unavailable")}, {handle: "alice.new.test"}},
		},
		{
			name:             "caching resolver Purge(did) error",
			did:              "did:plc:refreshlockpurgeerror",
			steps:            []resolveStep{{handle: "alice.new.test"}},
			failCachingPurge: true,
		},
		{
			name:     "uncached resolver panics",
			did:      "did:plc:refreshlockpanic",
			steps:    []resolveStep{{panics: true}, {handle: "alice.new.test"}},
			mayPanic: true,
		},
		{
			name:         "holder cancelled at the barrier",
			did:          "did:plc:refreshlockholdercancel",
			steps:        []resolveStep{{block: true, handle: "alice.old.test"}, {handle: "alice.new.test"}},
			cancelHolder: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			race := newRefreshRace(t, map[string][]resolveStep{tc.did: tc.steps})
			race.seedUser(t, tc.did, "alice.seed.test")
			var wrap func(identity.Resolver) identity.Resolver
			if tc.failCachingPurge {
				wrap = func(caching identity.Resolver) identity.Resolver {
					return &countingIdentityResolver{Resolver: caching, failPurgeIdentifier: tc.did, failPurgeRemaining: 1}
				}
			}
			first := race.process(t, "coves-refresh-first", wrap)
			successor := race.process(t, "coves-refresh-successor", nil)

			holderCtx, cancelHolder := context.WithCancel(race.ctx)
			t.Cleanup(cancelHolder)
			refreshFirst := race.start(holderCtx, first, tc.did)
			if tc.cancelHolder {
				phase := race.awaitBarrier(t, refreshFirst)
				cancelHolder()
				refreshFirst.wait(t, phase)
			} else {
				refreshFirst.wait(t, race.phase(t))
			}
			switch {
			case refreshFirst.panicked != nil && !tc.mayPanic:
				t.Fatalf("first refresh panicked: %v", refreshFirst.panicked)
			case refreshFirst.panicked == nil && refreshFirst.err == nil:
				t.Fatalf("first refresh returned nil, want it to fail")
			}

			if err := race.start(race.ctx, successor, tc.did).result(t, race.phase(t)); err != nil {
				t.Errorf("following refresh through an independent pool: %v", err)
			}
			if got := race.storedHandle(t, tc.did); got != "alice.new.test" {
				t.Errorf("stored handle after following refresh = %q, want alice.new.test", got)
			}
		})
	}
}

// Behavior 5: waiting for the lock honors the context; a cancelled waiter
// returns a non-permanent error and writes nothing.
func TestUserIdentityRefreshLockWaitHonorsContext(t *testing.T) {
	const did = "did:plc:refreshlockwaitcancel"
	race := newRefreshRace(t, map[string][]resolveStep{did: {
		{block: true, handle: "alice.first.test"},
		{handle: "alice.second.test"},
	}})
	race.seedUser(t, did, "alice.seed.test")
	processOne := race.process(t, "coves-refresh-one", nil)
	processTwo := race.process(t, "coves-refresh-two", nil)

	refreshOne := race.start(race.ctx, processOne, did)
	phase := race.awaitBarrier(t, refreshOne)
	waiterCtx, cancelWaiter := context.WithCancel(race.ctx)
	t.Cleanup(cancelWaiter)
	refreshTwo := race.start(waiterCtx, processTwo, did)
	race.requireAdvisoryWaiter(t, phase, refreshTwo)
	cancelWaiter()

	assertNonPermanentRefreshError(t, "cancelled waiting refresh", refreshTwo.result(t, phase))
	if refreshOne.returned() {
		t.Fatalf("held refresh returned before release: err=%v panic=%v", refreshOne.err, refreshOne.panicked)
	}
	if got := race.storedHandle(t, did); got != "alice.seed.test" {
		t.Errorf("stored handle after the waiter was cancelled = %q, want alice.seed.test", got)
	}
	race.resolver.releaseBarrier()

	if err := refreshOne.result(t, race.finish(t)); err != nil {
		t.Errorf("held refresh: %v", err)
	}
	if got := race.storedHandle(t, did); got != "alice.first.test" {
		t.Errorf("stored handle = %q, want alice.first.test", got)
	}
}

// A live-context waiter whose lock wait runs out must not refresh without the
// lock: it returns the deferred busy error and purges, resolves and writes
// nothing.
func TestUserIdentityRefreshLockBusyWaiterRefreshesNothing(t *testing.T) {
	const did = "did:plc:refreshlockbusy"
	race := newRefreshRace(t, map[string][]resolveStep{did: {
		{block: true, handle: "alice.first.test"},
		{handle: "alice.second.test"},
	}})
	race.seedUser(t, did, "alice.seed.test")
	holder := race.process(t, "coves-refresh-holder", nil)
	waiter := race.processWithLockWait(t, "coves-refresh-waiter", nil, 200*time.Millisecond)

	refreshHolder := race.start(race.ctx, holder, did)
	phase := race.awaitBarrier(t, refreshHolder)
	// The holder purged before blocking. The DID row caches a handle other
	// than the stored one, so only a Purge(did) by the waiter removes it.
	race.seedIdentityCacheRow(t, phase, did, did, "alice.cached.test")

	busyErr := race.start(race.ctx, waiter, did).result(t, phase)
	assertNonPermanentRefreshError(t, "refresh whose lock wait ran out", busyErr)
	if !errors.Is(busyErr, jetstream.ErrIdentityRefreshBusy) || !errors.Is(busyErr, jetstream.ErrUnresolvedReference) {
		t.Errorf("refresh whose lock wait ran out: error = %v, want one wrapping ErrIdentityRefreshBusy and ErrUnresolvedReference", busyErr)
	}
	if got := race.resolver.callCount(did); got != 1 {
		t.Errorf("uncached resolver calls for %s = %d, want 1 (only the holder resolved)", did, got)
	}
	if got := race.identityCacheRows(t, phase, `identifier = $1`, did); got != 1 {
		t.Errorf("cached DID row count after the busy refresh = %d, want 1 (the busy refresh purged)", got)
	}
	if got := race.storedHandle(t, did); got != "alice.seed.test" {
		t.Errorf("stored handle after the busy refresh = %q, want alice.seed.test", got)
	}
	if refreshHolder.returned() {
		t.Fatalf("held refresh returned before release: err=%v panic=%v", refreshHolder.err, refreshHolder.panicked)
	}
	race.resolver.releaseBarrier()

	if err := refreshHolder.result(t, race.finish(t)); err != nil {
		t.Errorf("held refresh: %v", err)
	}
	if got := race.storedHandle(t, did); got != "alice.first.test" {
		t.Errorf("stored handle = %q, want alice.first.test", got)
	}
}

// Behavior 8: the lock is held until the handle write commits, so a second
// refresh waits on it while the first refresh's write is blocked.
func TestUserIdentityRefreshLockHeldUntilHandleWriteCommits(t *testing.T) {
	const did = "did:plc:refreshlockcommit"
	race := newRefreshRace(t, map[string][]resolveStep{did: {
		{handle: "alice.first.test"},
		{handle: "alice.second.test"},
	}})
	race.seedUser(t, did, "alice.seed.test")
	processA := race.process(t, "coves-refresh-a", nil)
	processB := race.process(t, "coves-refresh-b", nil)

	lockPID, rollback := race.lockUserRow(t, did)
	phase := race.phase(t)
	refreshA := race.start(race.ctx, processA, did)
	race.awaitBlockedByRowLock(t, phase, refreshA, lockPID)
	refreshB := race.start(race.ctx, processB, did)
	race.requireAdvisoryWaiter(t, phase, refreshB)
	rollback()

	finish := race.finish(t)
	if err := refreshA.result(t, finish); err != nil {
		t.Errorf("refresh A: %v", err)
	}
	if err := refreshB.result(t, finish); err != nil {
		t.Errorf("refresh B: %v", err)
	}
	if got := race.storedHandle(t, did); got != "alice.second.test" {
		t.Errorf("stored handle = %q, want alice.second.test", got)
	}
}
