//go:build integration

package communities_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
)

// Two community consumers in different processes refreshing one community DID
// must not interleave. Each "process" is its own *sql.DB pool to the same
// clone with its own repository, Postgres-backed caching resolver and per-DID
// refresh lock; the uncached resolver is shared, its first answer held at a
// barrier. The holder is released only once the competitor has returned or is
// seen in pg_locks waiting on an advisory lock another backend holds; the lock
// key itself is never computed here.

// communityRefreshPhaseLimit bounds a phase measured from the moment the holder
// reached the barrier; it stays under the refresh's 5s resolve timeout.
const (
	communityRefreshPhaseLimit   = 3 * time.Second
	communityRefreshFinishLimit  = 5 * time.Second
	communityRefreshPollInterval = 10 * time.Millisecond
)

// communityBarrierResolver answers one DID with answers[n] on its nth call
// (the last answer repeats). The first call blocks until releaseBarrier.
type communityBarrierResolver struct {
	identity.Resolver

	did     string
	answers []string

	mu    sync.Mutex
	calls int

	entered     chan struct{}
	enterOnce   sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func (r *communityBarrierResolver) Resolve(ctx context.Context, did string) (*identity.Identity, error) {
	if did != r.did {
		return nil, errors.New("communityBarrierResolver: resolved a different DID than the event DID")
	}
	r.mu.Lock()
	call := r.calls
	r.calls++
	r.mu.Unlock()
	if call == 0 {
		r.enterOnce.Do(func() { close(r.entered) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return identityOn(did, r.answers[min(call, len(r.answers)-1)], trustedBridgePDS), nil
}

func (r *communityBarrierResolver) ResolveDeclaredIdentity(_ context.Context, did string) (*identity.DeclaredIdentity, error) {
	return &identity.DeclaredIdentity{DID: did, DeclaredHandle: r.answers[len(r.answers)-1], PDSURL: trustedBridgePDS}, nil
}

func (r *communityBarrierResolver) Purge(context.Context, string) error { return nil }

func (r *communityBarrierResolver) releaseBarrier() {
	r.releaseOnce.Do(func() { close(r.release) })
}

// communityRefreshRun is one identity event delivered in its own goroutine.
type communityRefreshRun struct {
	applicationName string
	done            chan struct{}
	err             error
	panicked        any
}

func (run *communityRefreshRun) returned() bool {
	select {
	case <-run.done:
		return true
	default:
		return false
	}
}

func (run *communityRefreshRun) result(t *testing.T, ctx context.Context) error {
	t.Helper()
	select {
	case <-run.done:
	case <-ctx.Done():
		t.Fatalf("refresh in %s did not return: %v", run.applicationName, ctx.Err())
	}
	if run.panicked != nil {
		t.Fatalf("refresh in %s panicked: %v", run.applicationName, run.panicked)
	}
	return run.err
}

// communityRefreshRace owns the harness clone (seeding, reads, pg_locks polls),
// the shared barrier resolver, the extra pools and every refresh goroutine.
type communityRefreshRace struct {
	h        *communityOriginHarness
	resolver *communityBarrierResolver
	ctx      context.Context

	wg    sync.WaitGroup
	pools []*sql.DB
}

func newCommunityRefreshRace(t *testing.T, did, seedHandle string, answers ...string) *communityRefreshRace {
	t.Helper()
	h := newCommunityOriginHarness(t, did, seedHandle, trustedBridgePDS)
	h.seed(t, seededCommunity{name: "gaming", handle: seedHandle, pdsURL: trustedBridgePDS})
	ctx, cancel := context.WithCancel(context.Background())
	race := &communityRefreshRace{
		h: h, ctx: ctx,
		resolver: &communityBarrierResolver{
			did: did, answers: answers,
			entered: make(chan struct{}), release: make(chan struct{}),
		},
	}
	t.Cleanup(func() {
		race.resolver.releaseBarrier()
		cancel()
		race.wg.Wait()
		for _, pool := range race.pools {
			_ = pool.Close()
		}
	})
	return race
}

// process builds a community consumer on its own pool to the harness clone;
// applicationName tags its backends for pg_locks.
func (r *communityRefreshRace) process(t *testing.T, applicationName string) *jetstream.CommunityEventConsumer {
	t.Helper()
	var database string
	if err := r.h.db.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&database); err != nil {
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
	caching := identity.NewResolver(pool, identity.Config{PLCURL: testkit.Endpoints().PLC.BaseURL, CacheTTL: 24 * time.Hour})
	return jetstream.NewCommunityEventConsumer(postgres.NewCommunityRepository(pool, credentialciphertest.Fixed()),
		instanceDID, true, caching,
		jetstream.WithCommunityUncachedIdentityResolver(r.resolver),
		jetstream.WithCommunityIdentityRefreshLock(
			jetstream.NewPostgresIdentityRefreshLock(pool, jetstream.IdentityRefreshLockWaitTimeout)))
}

// start delivers a handle-carrying identity event for the harness DID through
// consumer in a goroutine.
func (r *communityRefreshRace) start(consumer *jetstream.CommunityEventConsumer, applicationName, eventHandle string) *communityRefreshRun {
	run := &communityRefreshRun{applicationName: applicationName, done: make(chan struct{})}
	event := r.h.identityEvent(eventHandle)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(run.done)
		defer func() { run.panicked = recover() }()
		run.err = consumer.HandleEvent(r.ctx, event)
	}()
	return run
}

func (r *communityRefreshRace) deadline(t *testing.T, limit time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, limit)
	t.Cleanup(cancel)
	return ctx
}

// awaitBarrier waits for holder to be held at the resolver barrier and returns
// a phase context measured from then. A holder that returns without resolving
// is reported and the phase starts at once.
func (r *communityRefreshRace) awaitBarrier(t *testing.T, holder *communityRefreshRun) context.Context {
	t.Helper()
	timer := time.NewTimer(communityRefreshPhaseLimit)
	defer timer.Stop()
	select {
	case <-r.resolver.entered:
	case <-holder.done:
		t.Errorf("identity event in %s returned without a fresh resolution (err=%v panic=%v); want it held at the resolver barrier",
			holder.applicationName, holder.err, holder.panicked)
	case <-timer.C:
		t.Fatalf("identity event in %s did not reach the resolver barrier within %s", holder.applicationName, communityRefreshPhaseLimit)
	}
	return r.deadline(t, communityRefreshPhaseLimit)
}

// advisoryWaiting reports whether a backend of applicationName on the clone
// waits for an advisory lock whose tag another backend holds.
func (r *communityRefreshRace) advisoryWaiting(ctx context.Context, applicationName string) (bool, error) {
	var waiting bool
	err := r.h.db.QueryRowContext(ctx, `
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

// awaitContenderSettled polls until contender has returned or waits on the
// per-DID advisory lock; it fails the test when the phase ends first.
func (r *communityRefreshRace) awaitContenderSettled(t *testing.T, phase context.Context, contender *communityRefreshRun) {
	t.Helper()
	ticker := time.NewTicker(communityRefreshPollInterval)
	defer ticker.Stop()
	for {
		if contender.returned() {
			return
		}
		waiting, err := r.advisoryWaiting(phase, contender.applicationName)
		if err != nil && phase.Err() == nil {
			t.Fatalf("poll pg_locks for %s: %v", contender.applicationName, err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-phase.Done():
			t.Fatalf("refresh in %s neither returned nor waited on the per-DID advisory lock within %s",
				contender.applicationName, communityRefreshPhaseLimit)
		}
	}
}

// TestCommunityIdentityRefreshRace_LastFreshAnswerWins: the holder resolves
// OLD and is held; the competitor runs behind it. Whatever the competitor
// resolved last is what stays stored, and it compares against the row read
// inside the lock.
func TestCommunityIdentityRefreshRace_LastFreshAnswerWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		did  string
		seed string
		// answers are the uncached resolver's answers in call order; the first
		// is held at the barrier.
		answers []string
		want    string
	}{
		{
			// Without the lock the competitor stores NEW first and the released
			// holder overwrites it with its stalled OLD answer.
			name: "stalled old answer does not revert the newer handle", did: "did:plc:communityrefreshracenew",
			seed:    "gaming.seed.coves.social",
			answers: []string{"gaming.old.coves.social", "gaming.new.coves.social"},
			want:    "gaming.new.coves.social",
		},
		{
			// The holder stores OLD; the competitor resolves the seed handle
			// again. Compared with a row read before the lock (the seed) it
			// would skip the write and leave OLD.
			name: "competitor compares with the row read inside the lock", did: "did:plc:communityrefreshracereread",
			seed:    "gaming.seed.coves.social",
			answers: []string{"gaming.old.coves.social", "gaming.seed.coves.social"},
			want:    "gaming.seed.coves.social",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			race := newCommunityRefreshRace(t, tc.did, tc.seed, tc.answers...)
			processA := race.process(t, "coves-community-refresh-a")
			processB := race.process(t, "coves-community-refresh-b")
			eventHandle := tc.answers[len(tc.answers)-1]

			holder := race.start(processA, "coves-community-refresh-a", eventHandle)
			phase := race.awaitBarrier(t, holder)
			contender := race.start(processB, "coves-community-refresh-b", eventHandle)
			race.awaitContenderSettled(t, phase, contender)
			race.resolver.releaseBarrier()

			finish := race.deadline(t, communityRefreshFinishLimit)
			if err := holder.result(t, finish); err != nil {
				t.Errorf("holder refresh: %v", err)
			}
			if err := contender.result(t, finish); err != nil {
				t.Errorf("competing refresh: %v", err)
			}
			race.h.assertStoredHandle(t, tc.want)
		})
	}
}
