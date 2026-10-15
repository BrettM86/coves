package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
)

const debouncedUserDID = "did:plc:identityrefreshdebounce"

func debounceIdentityEvent() *JetstreamEvent {
	return &JetstreamEvent{
		Kind:     "identity",
		Did:      debouncedUserDID,
		Identity: &IdentityEvent{Did: debouncedUserDID, Handle: "event.example.com"},
	}
}

// scriptedRefreshResolver is the uncached resolver for identity refresh. In
// hang mode it never answers, so the refresh runs into identityResolveTimeout
// the way a handle domain an attacker makes hang would. Otherwise it answers at
// once with err, or with answer when err is nil.
type scriptedRefreshResolver struct {
	mu     sync.Mutex
	calls  int
	hang   bool
	err    error
	answer *identity.Identity
}

func (r *scriptedRefreshResolver) set(hang bool, err error, answer *identity.Identity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hang, r.err, r.answer = hang, err, answer
}

func (r *scriptedRefreshResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *scriptedRefreshResolver) Resolve(ctx context.Context, did string) (*identity.Identity, error) {
	r.mu.Lock()
	r.calls++
	hang, err, answer := r.hang, r.err, r.answer
	r.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, &identity.ErrResolutionFailed{Identifier: did, Reason: "handle domain never answers", Err: ctx.Err()}
	}
	if err != nil {
		return nil, err
	}
	return answer, nil
}

func (r *scriptedRefreshResolver) ResolveHandle(context.Context, string) (string, string, error) {
	return "", "", nil
}

func (r *scriptedRefreshResolver) ResolveDID(context.Context, string) (*identity.DIDDocument, error) {
	return nil, nil
}

func (r *scriptedRefreshResolver) Purge(context.Context, string) error { return nil }

// ResolveDeclaredIdentity is never reached by identity refresh, which verifies
// through Resolve alone.
func (r *scriptedRefreshResolver) ResolveDeclaredIdentity(_ context.Context, did string) (*identity.DeclaredIdentity, error) {
	return nil, &identity.ErrResolutionFailed{Identifier: did, Reason: "identity refresh does not read declared documents"}
}

// purgeRecordingResolver is the caching resolver; it records every purge so a
// test can see that cache invalidation ran. It also holds cached entries: a
// seeded entry stays until a purge of that identifier removes it.
type purgeRecordingResolver struct {
	mockIdentityResolverForUser
	mu     sync.Mutex
	purged []string
	cached map[string]bool
}

func (r *purgeRecordingResolver) Purge(_ context.Context, identifier string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purged = append(r.purged, identifier)
	delete(r.cached, identifier)
	return nil
}

// seedCacheEntry stores a cached entry for identifier.
func (r *purgeRecordingResolver) seedCacheEntry(identifier string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached == nil {
		r.cached = make(map[string]bool)
	}
	r.cached[identifier] = true
}

// hasCacheEntry reports whether a cached entry for identifier is still held.
func (r *purgeRecordingResolver) hasCacheEntry(identifier string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cached[identifier]
}

// passThroughIdentityRefreshLock is the in-process IdentityRefreshLock for unit
// tests: it runs fn under a mutex and has no cross-process reach.
type passThroughIdentityRefreshLock struct {
	mu sync.Mutex
}

func (l *passThroughIdentityRefreshLock) WithDIDLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fn(ctx)
}

func (r *purgeRecordingResolver) purgeCount(identifier string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, purged := range r.purged {
		if purged == identifier {
			count++
		}
	}
	return count
}

type debounceFixture struct {
	consumer        *UserEventConsumer
	service         *mockUserService
	cachingResolver *purgeRecordingResolver
	advance         func(time.Duration)
}

// newDebouncedUserConsumer returns a consumer for a known user whose identity
// refresh clock is controlled by the fixture's advance function.
func newDebouncedUserConsumer(uncachedResolver identity.UncachedResolver, opts ...ConsumerOption) debounceFixture {
	service := newMockUserService()
	service.users[debouncedUserDID] = &users.User{DID: debouncedUserDID, Handle: "stored.example.com"}
	cachingResolver := &purgeRecordingResolver{}
	opts = append([]ConsumerOption{
		WithUserUncachedIdentityResolver(uncachedResolver),
		WithUserIdentityRefreshLock(&passThroughIdentityRefreshLock{}),
	}, opts...)
	consumer := NewUserEventConsumer(service, cachingResolver, opts...)

	var clockMu sync.Mutex
	current := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	consumer.identityRefreshFailures.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return current
	}
	advance := func(elapsed time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		current = current.Add(elapsed)
	}
	return debounceFixture{consumer: consumer, service: service, cachingResolver: cachingResolver, advance: advance}
}

func renamedIdentity() *identity.Identity {
	return &identity.Identity{DID: debouncedUserDID, Handle: "renamed.example.com"}
}

func TestUserConsumer_IdentityRefreshTimeoutIsDebouncedButStillPurgesAndDefers(t *testing.T) {
	t.Parallel()

	resolver := &scriptedRefreshResolver{hang: true}
	fixture := newDebouncedUserConsumer(resolver)

	err := fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent())
	require.ErrorIs(t, err, ErrUnresolvedReference, "a timed-out fresh resolution must be redrivable")
	require.NotErrorIs(t, err, errRedriveDeferred,
		"the event that actually ran the resolution did real work and must spend its attempt")
	require.Equal(t, 1, resolver.callCount())
	require.Equal(t, 1, fixture.cachingResolver.purgeCount(debouncedUserDID))

	fixture.advance(time.Second)
	err = fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent())
	require.ErrorIs(t, err, ErrUnresolvedReference, "a debounced refresh must still be redrivable rather than dropped")
	assert.ErrorIs(t, err, errRedriveDeferred,
		"a debounced refresh ran no resolution, so it must not spend one of the three unresolved redrive attempts")
	assert.Equal(t, 1, resolver.callCount(),
		"a second identity event within the TTL after a timeout must not reach the network: a handle domain that hangs would otherwise cost the serial user lane 5 seconds per event")
	assert.Equal(t, 2, fixture.cachingResolver.purgeCount(debouncedUserDID),
		"the cached DID answer must be purged on every identity event, debounced or not")
}

func TestUserConsumer_IdentityRefreshFastFailureIsNotDebounced(t *testing.T) {
	t.Parallel()

	fastFailures := []struct {
		name string
		err  error
		ans  *identity.Identity
	}{
		{name: "resolution error such as NXDOMAIN", err: &identity.ErrResolutionFailed{
			Identifier: debouncedUserDID, Reason: "no such host while DNS propagates",
		}},
		{name: "handle.invalid", ans: &identity.Identity{DID: debouncedUserDID, Handle: identity.InvalidHandle}},
	}
	for _, failure := range fastFailures {
		t.Run(failure.name, func(t *testing.T) {
			t.Parallel()

			resolver := &scriptedRefreshResolver{}
			resolver.set(false, failure.err, failure.ans)
			fixture := newDebouncedUserConsumer(resolver)

			require.ErrorIs(t, fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent()), ErrUnresolvedReference)
			require.Equal(t, 1, resolver.callCount())

			resolver.set(false, nil, renamedIdentity())
			fixture.advance(time.Second)
			require.NoError(t, fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent()),
				"once the user fixes their handle, the next identity event must succeed even within the TTL")
			assert.Equal(t, 2, resolver.callCount(),
				"a fast failure does not stall the lane, so it must never suppress the next event's fresh resolution (PRD §8)")
			assert.Equal(t, []string{"renamed.example.com"}, fixture.service.updatedHandles,
				"the fixed handle must be stored on the next identity event")
		})
	}
}

func TestUserConsumer_IdentityRefreshTimeoutDebounceExpires(t *testing.T) {
	t.Parallel()

	const failureTTL = 30 * time.Second
	resolver := &scriptedRefreshResolver{hang: true}
	fixture := newDebouncedUserConsumer(resolver, WithUserIdentityRefreshFailureTTL(failureTTL))

	require.ErrorIs(t, fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent()), ErrUnresolvedReference)
	require.Equal(t, 1, resolver.callCount())

	resolver.set(false, nil, renamedIdentity())
	fixture.advance(failureTTL - time.Second)
	require.ErrorIs(t, fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent()), errRedriveDeferred)
	require.Equal(t, 1, resolver.callCount(), "the timeout must be remembered for the configured TTL")

	fixture.advance(2 * time.Second)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent()))
	assert.Equal(t, 2, resolver.callCount(),
		"after the configured TTL the refresh must reach the network again, or a redrive could never observe recovery")
	assert.Equal(t, []string{"renamed.example.com"}, fixture.service.updatedHandles)
}

func TestUserConsumer_IdentityRefreshSuccessIsNotDebounced(t *testing.T) {
	t.Parallel()

	resolver := &scriptedRefreshResolver{answer: &identity.Identity{DID: debouncedUserDID, Handle: "stored.example.com"}}
	fixture := newDebouncedUserConsumer(resolver)

	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent()))
	require.Equal(t, 1, resolver.callCount())

	fixture.advance(time.Second)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent()))
	assert.Equal(t, 2, resolver.callCount(),
		"a successful refresh must never be remembered: every identity event for a healthy identity resolves fresh")
}

func TestUserConsumer_IdentityRefreshParentContextEndIsNotDebounced(t *testing.T) {
	t.Parallel()

	parentEnds := []struct {
		name   string
		parent func() (context.Context, context.CancelFunc)
	}{
		{name: "cancelled", parent: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
		{name: "parent deadline shorter than the resolve timeout", parent: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}},
	}
	for _, parentEnd := range parentEnds {
		t.Run(parentEnd.name, func(t *testing.T) {
			t.Parallel()

			resolver := &scriptedRefreshResolver{hang: true}
			fixture := newDebouncedUserConsumer(resolver)

			parent, cancel := parentEnd.parent()
			defer cancel()
			require.Error(t, fixture.consumer.HandleEvent(parent, debounceIdentityEvent()))
			require.Equal(t, 1, resolver.callCount())

			resolver.set(false, nil, renamedIdentity())
			fixture.advance(time.Second)
			require.NoError(t, fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent()))
			assert.Equal(t, 2, resolver.callCount(),
				"a failure caused by the caller's own context says nothing about the identity and must not suppress the next refresh")
		})
	}
}

// TestUserConsumer_DebouncedIdentityDeadLettersDoNotSpendAttempts pins
// docs/SECURITY_MODEL.md: a replay that did no new work does not spend an
// attempt. Without it, a live event that times out just before a redrive pass
// would use up the three unresolved attempts of every dead letter for that DID
// without one fresh lookup.
func TestUserConsumer_DebouncedIdentityDeadLettersDoNotSpendAttempts(t *testing.T) {
	t.Parallel()

	const failureTTL = 30 * time.Second
	ctx := context.Background()
	resolver := &scriptedRefreshResolver{hang: true}
	fixture := newDebouncedUserConsumer(resolver, WithUserIdentityRefreshFailureTTL(failureTTL))

	require.ErrorIs(t, fixture.consumer.HandleEvent(ctx, debounceIdentityEvent()), ErrUnresolvedReference)
	require.Equal(t, 1, resolver.callCount())

	queue := newFakeDeadLetterQueue()
	const startingAttempts = MaxRedriveAttempts - UnresolvedRedriveAttempts
	const deadLetters = 3
	for i := int64(1); i <= deadLetters; i++ {
		event := debounceIdentityEvent()
		event.TimeUS = i * 1_000
		data, err := json.Marshal(event)
		require.NoError(t, err)
		require.NoError(t, queue.AddDeadLetter(ctx, "user-consumer", event.TimeUS, data, "unresolved reference", startingAttempts))
	}
	redriver := NewDeadLetterRedriver(fakeRedriveStore(queue), map[string]EventHandler{"user-consumer": fixture.consumer})

	for pass := 0; pass < UnresolvedRedriveAttempts+1; pass++ {
		redriver.redriveAll(ctx)
	}
	require.Equal(t, 1, resolver.callCount(), "debounced replays must not reach the network")
	require.Equal(t, deadLetters, queue.rowCount())
	for i := 0; i < queue.rowCount(); i++ {
		assert.Equal(t, startingAttempts, queue.row(i).Attempts,
			"dead letter %d spent an attempt on a debounced replay that ran no resolution", queue.row(i).ID)
	}

	resolver.set(false, nil, renamedIdentity())
	fixture.advance(failureTTL + time.Second)
	redriver.redriveAll(ctx)
	assert.Equal(t, 0, queue.rowCount(), "once the TTL expires each dead letter must resolve fresh and be deleted")
}

// hangableServer is a loopback httptest server that either answers with respond
// or, when hang is set, holds every request open until the client gives up.
type hangableServer struct {
	server   *httptest.Server
	requests atomic.Int64
}

func newHangableServer(t *testing.T, hang bool, respond http.HandlerFunc) *hangableServer {
	t.Helper()
	release := make(chan struct{})
	hangable := &hangableServer{}
	hangable.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hangable.requests.Add(1)
		if hang {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		respond(w, r)
	}))
	// Cleanups run last-in first-out, so release unblocks any held request
	// before Close waits for it.
	t.Cleanup(hangable.server.Close)
	t.Cleanup(func() { close(release) })
	return hangable
}

// TestUserConsumer_IdentityRefreshTimeoutThroughRealResolverIsDebounced drives
// the timeout through identity.NewUncachedResolver against local servers. A
// hanging handle domain does not make indigo's DID lookup fail: it spends the
// whole identityResolveTimeout on the well-known fetch and then reports the
// handle as handle.invalid with a nil error. The scripted resolver above always
// returns an error on timeout, so it cannot see that shape. A hanging PLC
// directory does surface as an error and is pinned here through the same real
// resolver.
func TestUserConsumer_IdentityRefreshTimeoutThroughRealResolverIsDebounced(t *testing.T) {
	t.Parallel()

	const handleSuffix = ".hang.test"
	const handle = "renamed" + handleSuffix
	hangs := []struct {
		name               string
		plcHangs           bool
		handleDomainHangs  bool
		wantHandleRequests int64
	}{
		{name: "handle domain hangs behind a prompt PLC", handleDomainHangs: true, wantHandleRequests: 1},
		{name: "PLC directory hangs", plcHangs: true, wantHandleRequests: 0},
	}
	for _, hang := range hangs {
		t.Run(hang.name, func(t *testing.T) {
			t.Parallel()

			handleDomain := newHangableServer(t, hang.handleDomainHangs, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/atproto-did" || r.Host != handle {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = fmt.Fprint(w, debouncedUserDID)
			})
			plc := newHangableServer(t, hang.plcHangs, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/"+debouncedUserDID {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":%q,"alsoKnownAs":["at://%s"],`+
					`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.invalid"}]}`,
					debouncedUserDID, handle)
			})

			config := identity.DefaultConfig(identity.WithPrivateHostsAllowed(), identity.WithWellKnownHosts(map[string]string{
				handleSuffix: strings.TrimPrefix(handleDomain.server.URL, "http://"),
			}))
			config.PLCURL = plc.server.URL
			fixture := newDebouncedUserConsumer(identity.NewUncachedResolver(config))

			started := time.Now()
			err := fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent())
			require.ErrorIs(t, err, ErrUnresolvedReference, "a timed-out fresh resolution must be redrivable")
			require.NotErrorIs(t, err, errRedriveDeferred,
				"the event that actually ran the resolution did real work and must spend its attempt")
			require.GreaterOrEqual(t, time.Since(started), identityResolveTimeout,
				"the first event must take the timeout path, not a fast failure")
			require.Equal(t, int64(1), plc.requests.Load())
			require.Equal(t, hang.wantHandleRequests, handleDomain.requests.Load())

			fixture.advance(time.Second)
			err = fixture.consumer.HandleEvent(context.Background(), debounceIdentityEvent())
			require.ErrorIs(t, err, ErrUnresolvedReference, "a debounced refresh must still be redrivable rather than dropped")
			assert.ErrorIs(t, err, errRedriveDeferred,
				"the timeout must be recorded so the next event within the TTL defers without resolving")
			assert.Equal(t, int64(1), plc.requests.Load(), "a debounced refresh must not reach the PLC directory")
			assert.Equal(t, hang.wantHandleRequests, handleDomain.requests.Load(),
				"a debounced refresh must not reach the handle domain")
			assert.Equal(t, 2, fixture.cachingResolver.purgeCount(debouncedUserDID),
				"the cached DID answer must be purged on every identity event, debounced or not")
		})
	}
}

// A user consumer without an identity refresh lock cannot serialize refreshes
// of one DID across processes, so it must refuse to refresh at all, the way it
// refuses without an uncached resolver. The refusal comes before any cache
// purge or handle write: a redrive is the only safe outcome.
func TestUserConsumer_IdentityRefreshWithoutLockFailsClosed(t *testing.T) {
	t.Parallel()

	const did = "did:plc:identityrefreshwithoutlock"
	service := newMockUserService()
	service.users[did] = &users.User{DID: did, Handle: "alice.stored.test"}
	cachingResolver := &purgeRecordingResolver{}
	cachingResolver.seedCacheEntry(did)
	uncachedResolver := &scriptedRefreshResolver{answer: &identity.Identity{DID: did, Handle: "alice.renamed.test"}}
	consumer := NewUserEventConsumer(service, cachingResolver,
		WithUserUncachedIdentityResolver(uncachedResolver))

	err := consumer.HandleEvent(context.Background(), &JetstreamEvent{
		Kind:     "identity",
		Did:      did,
		Identity: &IdentityEvent{Did: did, Handle: "alice.renamed.test"},
	})

	require.Error(t, err, "a consumer without a refresh lock must refuse the refresh, not apply it unserialized")
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"a missing lock is a wiring fault, not a fact about the event; dropping it would lose the rename")
	assert.Empty(t, service.updatedHandles, "no handle may be written without the refresh lock")
	assert.Equal(t, "alice.stored.test", service.users[did].Handle)
	assert.True(t, cachingResolver.hasCacheEntry(did),
		"the cached DID entry must survive: the lock check comes before any purge")
}

// A refresh without a caching resolver cannot purge the stale DID answer, so it
// must refuse before resolving, reading or writing anything, with an error that
// a redrive can still fix once the wiring is corrected. A consumer built with a
// nil resolver (as many test constructors are) reaches this path.
func TestRefreshIdentityHandleWithoutCachingResolverFailsClosed(t *testing.T) {
	t.Parallel()

	const did = "did:plc:identityrefreshwithoutcache"
	uncachedResolver := &scriptedRefreshResolver{answer: &identity.Identity{DID: did, Handle: "renamed.example.com"}}
	reads, writes := 0, 0

	var (
		verifiedHandle string
		changed        bool
		err            error
	)
	require.NotPanics(t, func() {
		verifiedHandle, changed, err = RefreshIdentityHandle(context.Background(), did,
			&passThroughIdentityRefreshLock{}, nil, uncachedResolver,
			newIdentityRefreshFailureDebounce(identity.DefaultNegativeCacheTTL),
			func(context.Context) (string, error) {
				reads++
				return "stored.example.com", nil
			},
			func(context.Context, string) error {
				writes++
				return nil
			})
	}, "a missing caching resolver is a wiring fault and must be reported, not crash the consumer")

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"a missing caching resolver is a wiring fault, not a fact about the event; dropping it would lose the rename")
	assert.Empty(t, verifiedHandle)
	assert.False(t, changed)
	assert.Zero(t, uncachedResolver.callCount(), "no resolution may run without the purge that precedes it")
	assert.Zero(t, reads, "the stored handle must not be read")
	assert.Zero(t, writes, "no handle may be written")
}
