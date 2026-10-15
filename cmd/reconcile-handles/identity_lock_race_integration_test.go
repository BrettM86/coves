//go:build integration

// The sweep and the identity-event consumers refresh handles under one per-DID
// lock. In these races the sweep and a consumer each run on their own pools to
// the same clone, with their own scripted uncached resolver. One side holds the
// lock while blocked at its resolution; the other is released only after it
// has returned or is seen waiting on the advisory lock in pg_locks.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockRacePhaseLimit bounds each phase in which one side is held at its
// resolution barrier. It stays under the refresh's 5s resolve timeout, so the
// test releases the holder before that timeout can decide the outcome.
const lockRacePhaseLimit = 3 * time.Second

// lockRaceFinishLimit bounds the wait for both sides after the release.
const lockRaceFinishLimit = 10 * time.Second

const lockRacePollInterval = 10 * time.Millisecond

// consumerApplicationName tags the consumer's pool in pg_locks observations.
const consumerApplicationName = "reconcile-handles-consumer"

// resolveBarrier holds a resolution until opened or until the resolution's
// context ends. Its hold method fits scriptedIdentityResolver.onFirstResolve.
type resolveBarrier struct {
	entered     chan struct{}
	enterOnce   sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newResolveBarrier() *resolveBarrier {
	return &resolveBarrier{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *resolveBarrier) hold(ctx context.Context, _ string) {
	b.enterOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
	}
}

func (b *resolveBarrier) open() {
	b.releaseOnce.Do(func() { close(b.release) })
}

// lockRaceRun is one side of the race running in its own goroutine.
type lockRaceRun struct {
	name            string
	applicationName string
	done            chan struct{}
	err             error
}

func (run *lockRaceRun) returned() bool {
	select {
	case <-run.done:
		return true
	default:
		return false
	}
}

func (run *lockRaceRun) result(t *testing.T, ctx context.Context) error {
	t.Helper()
	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		require.FailNowf(t, "race side did not return", "%s: %v", run.name, ctx.Err())
		return nil
	}
}

// identityEventHandler is the public event entry point both consumers share.
type identityEventHandler interface {
	HandleEvent(ctx context.Context, event *jetstream.JetstreamEvent) error
}

// newIdentityEventConsumer builds the consumer for kind on its own pool to the
// clone behind db: its own repository, failing-PLC caching resolver and
// per-DID refresh lock, with uncached as its uncached resolver.
func newIdentityEventConsumer(t *testing.T, db *sql.DB, kind string, uncached identity.UncachedResolver) identityEventHandler {
	t.Helper()
	pool := openSecondPool(t, db, consumerApplicationName)
	caching := newFailingPLCCachingResolver(t, pool)
	lock := jetstream.NewPostgresIdentityRefreshLock(pool, jetstream.IdentityRefreshLockWaitTimeout)
	switch kind {
	case kindUser:
		service := users.NewUserService(postgres.NewUserRepository(pool), caching, "", nil, "")
		return jetstream.NewUserEventConsumer(service, caching,
			jetstream.WithUserUncachedIdentityResolver(uncached),
			jetstream.WithUserIdentityRefreshLock(lock))
	case kindCommunity:
		return jetstream.NewCommunityEventConsumer(postgres.NewCommunityRepository(pool, credentialciphertest.Fixed()), "",
			true, caching,
			jetstream.WithCommunityUncachedIdentityResolver(uncached),
			jetstream.WithCommunityIdentityRefreshLock(lock))
	}
	require.FailNowf(t, "unknown row kind", "%q", kind)
	return nil
}

// advisoryLockWaiting reports whether a backend of applicationName on this
// clone waits for an advisory lock whose tag is granted to another backend.
func advisoryLockWaiting(ctx context.Context, observer *sql.DB, applicationName string) (bool, error) {
	var waiting bool
	err := observer.QueryRowContext(ctx, `
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

// awaitContenderSettled polls until contender has returned or one of its
// backends is seen waiting on the per-DID advisory lock. It reports whether
// the contender is waiting, and fails the test when neither happens within
// the phase.
func awaitContenderSettled(t *testing.T, phase context.Context, observer *sql.DB, contender *lockRaceRun) bool {
	t.Helper()
	ticker := time.NewTicker(lockRacePollInterval)
	defer ticker.Stop()
	for {
		if contender.returned() {
			return false
		}
		waiting, err := advisoryLockWaiting(phase, observer, contender.applicationName)
		if err != nil && phase.Err() == nil {
			require.NoError(t, err, "poll pg_locks for %s", contender.applicationName)
		}
		if waiting {
			return true
		}
		select {
		case <-ticker.C:
		case <-phase.Done():
			require.FailNowf(t, "contender not settled",
				"%s neither returned nor waited on the per-DID advisory lock within %s", contender.name, lockRacePhaseLimit)
		}
	}
}

// lockRaceCase is one race between the sweep and an identity-event consumer
// over a single bridged row.
type lockRaceCase struct {
	name string
	kind string
	// sweepHoldsLock: the sweep blocks at its resolution while holding the
	// lock and the consumer contends; otherwise the consumer holds.
	sweepHoldsLock bool
	// contenderMustWait fails the race unless the contender is seen waiting on
	// the advisory lock before the holder is released.
	contenderMustWait bool
	seedHandle        string
	sweepAnswer       string
	consumerAnswer    string
	wantHandle        string
}

// runLockRace seeds the row, runs the race and returns the stored handle once
// both sides returned, together with the sweep's report. Neither side may fail.
func runLockRace(t *testing.T, tc lockRaceCase) (storedHandle string, report sweepReport) {
	t.Helper()
	db := testkit.DB(t)
	did := uniqueDID(t, tc.kind)
	switch tc.kind {
	case kindUser:
		seedUser(t, db, did, tc.seedHandle, bridgeHost)
	case kindCommunity:
		seedCommunity(t, db, did, tc.seedHandle, bridgeHost)
	}

	barrier := newResolveBarrier()
	sweepResolver := newScriptedIdentityResolver(map[string]scriptedAnswer{did: {handle: tc.sweepAnswer}})
	consumerResolver := newScriptedIdentityResolver(map[string]scriptedAnswer{did: {handle: tc.consumerAnswer}})
	if tc.sweepHoldsLock {
		sweepResolver.onFirstResolve = barrier.hold
	} else {
		consumerResolver.onFirstResolve = barrier.hold
	}
	dependencies := newSweepDependencies(t, db, sweepResolver)
	consumer := newIdentityEventConsumer(t, db, tc.kind, consumerResolver)

	ctx, cancel := context.WithCancel(context.Background())
	var sides sync.WaitGroup
	// Registered after every pool, so it runs before they close.
	t.Cleanup(func() {
		barrier.open()
		cancel()
		sides.Wait()
	})
	start := func(name, applicationName string, side func(context.Context) error) *lockRaceRun {
		run := &lockRaceRun{name: name, applicationName: applicationName, done: make(chan struct{})}
		sides.Add(1)
		go func() {
			defer sides.Done()
			defer close(run.done)
			run.err = side(ctx)
		}()
		return run
	}
	var out bytes.Buffer
	startSweep := func() *lockRaceRun {
		return start("sweep", sweepLockApplicationName, func(ctx context.Context) error {
			var err error
			report, err = reconcile(ctx, sweepOptions{Concurrency: 1}, dependencies, &out)
			return err
		})
	}
	startConsumer := func() *lockRaceRun {
		return start(tc.kind+" consumer", consumerApplicationName, func(ctx context.Context) error {
			return consumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
				Did: did, Kind: "identity",
				Identity: &jetstream.IdentityEvent{Did: did, Handle: "event.bridge.test", Seq: 1, Time: time.Now().UTC().Format(time.RFC3339)},
			})
		})
	}

	var holder, contender *lockRaceRun
	if tc.sweepHoldsLock {
		holder = startSweep()
	} else {
		holder = startConsumer()
	}
	phase, cancelPhase := context.WithTimeout(ctx, lockRacePhaseLimit)
	defer cancelPhase()
	select {
	case <-barrier.entered:
	case <-holder.done:
		require.FailNowf(t, "holder returned early", "%s returned before reaching its resolution barrier: %v", holder.name, holder.err)
	case <-phase.Done():
		require.FailNowf(t, "holder not at barrier", "%s did not reach its resolution barrier within %s", holder.name, lockRacePhaseLimit)
	}

	if tc.sweepHoldsLock {
		contender = startConsumer()
	} else {
		contender = startSweep()
	}
	waiting := awaitContenderSettled(t, phase, db, contender)
	if tc.contenderMustWait {
		require.Truef(t, waiting, "%s returned (err=%v) instead of waiting on the per-DID advisory lock", contender.name, contender.err)
	}
	barrier.open()

	finish, cancelFinish := context.WithTimeout(ctx, lockRaceFinishLimit)
	defer cancelFinish()
	require.NoError(t, holder.result(t, finish), holder.name)
	require.NoError(t, contender.result(t, finish), contender.name)

	if tc.kind == kindCommunity {
		return storedCommunityHandle(t, db, did), report
	}
	return storedUserHandle(t, db, did), report
}

// TestReconcile_SweepHoldingLockKeepsConsumerHandle (DoD 9): the sweep resolves
// an old handle and stalls inside the per-DID lock while a user consumer on
// another pool handles an identity event for the same DID. The consumer waits
// for the lock, so its newer handle lands after the sweep's write and stays.
func TestReconcile_SweepHoldingLockKeepsConsumerHandle(t *testing.T) {
	t.Parallel()
	stored, _ := runLockRace(t, lockRaceCase{
		kind:           kindUser,
		sweepHoldsLock: true,
		seedHandle:     "seedalice.bridge.test",
		sweepAnswer:    "oldalice.bridge.test",
		consumerAnswer: "newalice.bridge.test",
	})
	assert.Equal(t, "newalice.bridge.test", stored, "users row handle")
}

// TestReconcile_SweepRereadsHandleInsideLock (DoD 11): a consumer stalls inside
// the per-DID lock after resolving B; the sweep selects the row while it still
// stores A and waits for the lock. The consumer then stores B, and the sweep,
// whose resolution answers A, compares against the handle read inside the lock
// (B), so it writes A back and reports the row updated.
func TestReconcile_SweepRereadsHandleInsideLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []lockRaceCase{
		{
			name: "user", kind: kindUser, contenderMustWait: true,
			seedHandle: "alicea.bridge.test", sweepAnswer: "alicea.bridge.test", consumerAnswer: "aliceb.bridge.test",
			wantHandle: "alicea.bridge.test",
		},
		{
			name: "community", kind: kindCommunity, contenderMustWait: true,
			seedHandle: "gardena.bridge.test", sweepAnswer: "gardena.bridge.test", consumerAnswer: "gardenb.bridge.test",
			wantHandle: "gardena.bridge.test",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stored, report := runLockRace(t, tc)
			assert.Equal(t, tc.wantHandle, stored, "stored handle")
			assert.Equal(t, 1, report.Checked, "Checked")
			assert.Equal(t, 1, report.Updated, "Updated")
			assert.Equal(t, 0, report.Unchanged, "Unchanged")
			assert.Equal(t, 0, report.StillDiffering, "StillDiffering")
		})
	}
}
