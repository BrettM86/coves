//go:build integration

// Identity events are the users domain's only unsolicited input: Jetstream
// delivers one whenever an account's handle changes anywhere on the network,
// and this package decides what — if anything — happens to the local row.
//
// The two things that matter here are both invisible to a unit test with a
// stubbed repository:
//
//   - the AppView indexes only accounts that have used Coves, so an event for
//     an unknown DID must be a silent no-op rather than an insert. Getting this
//     wrong means indexing every account on the network;
//   - an identity refresh must purge the identity cache before writing the
//     freshly verified handle, so retries can repeat a failed purge.
//
// The tests therefore drive the real jetstream.UserEventConsumer over the real
// service, repository and Postgres-backed identity cache. They are in an
// external test package because Coves/internal/db/postgres and
// Coves/internal/atproto/jetstream both import this package.
package users_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
)

// newUserService wires the service the consumer talks to.
//
// The identity resolver is real but never resolves anything over the network in
// these tests: the consumer only asks it to purge, which touches the Postgres
// cache alone. The PDS URL is recorded on rows and never dialed, so it comes
// from the test stack's endpoints rather than a literal.
//
// The database handle is returned alongside the service so the cache contract
// inspects the same clone as the consumer.
func newUserService(t *testing.T) (users.UserService, identity.Resolver, *sql.DB) {
	t.Helper()
	db := testkit.DB(t)
	service, resolver := newUserServiceOn(db)
	return service, resolver, db
}

// newUserServiceOn wires the service and its Postgres-backed caching resolver
// on an existing pool, so tests that model several processes sharing one
// database can give each process its own.
func newUserServiceOn(db *sql.DB) (users.UserService, identity.Resolver) {
	resolver := identity.NewResolver(db, identity.Config{
		PLCURL:   testkit.Endpoints().PLC.BaseURL,
		CacheTTL: 24 * time.Hour,
	})
	return users.NewUserService(postgres.NewUserRepository(db), resolver, testkit.Endpoints().PDS.BaseURL, nil, ""), resolver
}

type countingUserService struct {
	users.UserService
	gets int
}

func (s *countingUserService) GetUserByDID(ctx context.Context, did string) (*users.User, error) {
	s.gets++
	return s.UserService.GetUserByDID(ctx, did)
}

type countingIdentityResolver struct {
	identity.Resolver
	resolves            int
	purges              int
	failPurgeIdentifier string
	failPurgeRemaining  int
}

func (r *countingIdentityResolver) Resolve(ctx context.Context, did string) (*identity.Identity, error) {
	r.resolves++
	return r.Resolver.Resolve(ctx, did)
}

func (r *countingIdentityResolver) Purge(ctx context.Context, identifier string) error {
	r.purges++
	if identifier == r.failPurgeIdentifier && r.failPurgeRemaining > 0 {
		r.failPurgeRemaining--
		return errors.New("cache purge unavailable")
	}
	return r.Resolver.Purge(ctx, identifier)
}

type scriptedIdentityResolver struct {
	identity.Resolver
	wantDID  string
	answer   *identity.Identity
	err      error
	resolves int
	purges   int
	// declaredHandle overrides answer.Handle as the handle the document
	// declares, for answers whose handle verification failed.
	declaredHandle string
	documentErr    error
	documentReads  int
}

func (r *scriptedIdentityResolver) Resolve(_ context.Context, identifier string) (*identity.Identity, error) {
	r.resolves++
	if identifier != r.wantDID {
		return nil, errors.New("resolved a different identifier than the event DID")
	}
	return r.answer, r.err
}

func (r *scriptedIdentityResolver) Purge(context.Context, string) error {
	r.purges++
	return nil
}

func (r *scriptedIdentityResolver) ResolveDeclaredIdentity(_ context.Context, did string) (*identity.DeclaredIdentity, error) {
	r.documentReads++
	if did != r.wantDID {
		return nil, errors.New("read the document of a different DID than the event DID")
	}
	if r.documentErr != nil {
		return nil, r.documentErr
	}
	if r.answer == nil {
		return nil, &identity.ErrNotFound{Identifier: did, Reason: "no scripted document"}
	}
	declared := &identity.DeclaredIdentity{DID: r.answer.DID, DeclaredHandle: r.answer.Handle, PDSURL: r.answer.PDSURL}
	if r.declaredHandle != "" {
		declared.DeclaredHandle = r.declaredHandle
	}
	return declared, nil
}

// identityConsumerCase owns the common database, consumer and event choreography.
// Each caller supplies a distinct DID/handles so concurrent integration packages
// cannot collide even if their tests share the same template.
type identityConsumerCase struct {
	service  users.UserService
	counted  *countingUserService
	caching  *countingIdentityResolver
	uncached *scriptedIdentityResolver
	consumer *jetstream.UserEventConsumer
	db       *sql.DB
	did      string
}

func newIdentityConsumerCase(t *testing.T, did, storedHandle string, answer *identity.Identity, resolveErr error) *identityConsumerCase {
	t.Helper()
	service, caching, db := newUserService(t)
	if storedHandle != "" {
		if _, err := service.CreateUser(context.Background(), users.CreateUserRequest{
			DID: did, Handle: storedHandle, PDSURL: testkit.Endpoints().PDS.BaseURL,
		}); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	counted := &countingUserService{UserService: service}
	countingCache := &countingIdentityResolver{Resolver: caching}
	uncached := &scriptedIdentityResolver{wantDID: did, answer: answer, err: resolveErr}
	return &identityConsumerCase{
		service: service, counted: counted, caching: countingCache, uncached: uncached,
		consumer: jetstream.NewUserEventConsumer(counted, countingCache,
			jetstream.WithUserUncachedIdentityResolver(uncached),
			jetstream.WithUserIdentityRefreshLock(jetstream.NewPostgresIdentityRefreshLock(db, jetstream.IdentityRefreshLockWaitTimeout))),
		db: db, did: did,
	}
}

func (c *identityConsumerCase) deliver(handle string) error {
	return c.consumer.HandleIdentityEventPublic(context.Background(), identityEvent(c.did, handle))
}

// identityEvent builds the Jetstream identity event for did carrying handle.
func identityEvent(did, handle string) *jetstream.JetstreamEvent {
	return &jetstream.JetstreamEvent{
		Did: did, Kind: "identity",
		Identity: &jetstream.IdentityEvent{Did: did, Handle: handle, Seq: 12345, Time: time.Now().Format(time.RFC3339)},
	}
}

func (c *identityConsumerCase) storedHandle(t *testing.T) string {
	t.Helper()
	user, err := c.service.GetUserByDID(context.Background(), c.did)
	if err != nil {
		t.Fatalf("read stored user: %v", err)
	}
	return user.Handle
}

func assertTransientIdentityError(t *testing.T, err error) {
	t.Helper()
	if err == nil || errors.Is(err, jetstream.ErrPermanentEvent) || errors.Is(err, jetstream.ErrUnresolvedReference) {
		t.Errorf("identity error = %v, want a plain transient error", err)
	}
}

func TestUserIdentityEventBypassesAndPurgesCachedDID(t *testing.T) {
	for _, tc := range []struct {
		name, did, stored, cached, event, resolved string
	}{
		{"changed handle", "did:plc:freshb3cache", "alice.old.freshb3cache.test", "alice.old.freshb3cache.test", "alice.event.freshb3cache.test", "alice.new.freshb3cache.test"},
		{"unchanged handle with unrelated cached row", "did:plc:freshb3unchanged", "alice.old.freshb3unchanged.test", "alice.cached.freshb3unchanged.test", "alice.event.freshb3unchanged.test", "alice.old.freshb3unchanged.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newIdentityConsumerCase(t, tc.did, tc.stored,
				&identity.Identity{DID: tc.did, Handle: tc.resolved}, nil)
			ctx := context.Background()
			cache := identity.NewPostgresCache(c.db, 24*time.Hour)
			if err := cache.Set(ctx, &identity.Identity{
				DID: tc.did, Handle: tc.cached, PDSURL: testkit.Endpoints().PDS.BaseURL,
				Method: identity.MethodDNS, ResolvedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("seed cached identity: %v", err)
			}
			// Prove this is a usable stale answer on the very resolver passed to the consumer.
			stale, err := c.caching.Resolve(ctx, tc.did)
			if err != nil || stale == nil || stale.Handle != tc.cached {
				t.Fatalf("caching resolver's seeded answer = %+v, %v; want %s", stale, err, tc.cached)
			}
			c.caching.resolves = 0
			if err := c.deliver(tc.event); err != nil {
				t.Fatalf("deliver identity event: %v", err)
			}
			if c.caching.resolves != 0 {
				t.Errorf("caching Resolve calls during event = %d, want 0", c.caching.resolves)
			}
			if c.uncached.resolves != 1 {
				t.Errorf("uncached Resolve calls during event = %d, want 1", c.uncached.resolves)
			}
			if got := c.storedHandle(t); got != tc.resolved {
				t.Errorf("stored handle = %q, want %s", got, tc.resolved)
			}
			for _, identifier := range []string{tc.did, tc.cached} {
				if _, err := cache.Get(ctx, identifier); err == nil {
					t.Errorf("identity_cache still contains %q", identifier)
				} else {
					var miss *identity.ErrCacheMiss
					if !errors.As(err, &miss) {
						t.Errorf("cache lookup %q: want cache miss, got %v", identifier, err)
					}
				}
			}
		})
	}
}

func TestUserIdentityEventDIDPurgeFailureIsRetryable(t *testing.T) {
	const did = "did:plc:freshb6purge"
	c := newIdentityConsumerCase(t, did, "alice.old.freshb6purge.test",
		&identity.Identity{DID: did, Handle: "alice.new.freshb6purge.test"}, nil)
	c.caching.failPurgeIdentifier, c.caching.failPurgeRemaining = did, 1
	assertTransientIdentityError(t, c.deliver("alice.event.freshb6purge.test"))
	if c.uncached.resolves != 0 {
		t.Errorf("uncached Resolve calls after failed DID purge = %d, want 0", c.uncached.resolves)
	}
	if got := c.storedHandle(t); got != "alice.old.freshb6purge.test" {
		t.Errorf("stored handle after failed DID purge = %q, want alice.old.freshb6purge.test", got)
	}
}

func TestUserIdentityEventRetriesFailedOldHandlePurgeBeforeWrite(t *testing.T) {
	const did = "did:plc:freshb8orphan"
	const oldHandle = "alice.old.freshb8orphan.test"
	c := newIdentityConsumerCase(t, did, oldHandle,
		&identity.Identity{DID: did, Handle: "alice.new.freshb8orphan.test"}, nil)
	ctx := context.Background()
	cache := identity.NewPostgresCache(c.db, 24*time.Hour)
	if err := cache.Set(ctx, &identity.Identity{
		DID: did, Handle: oldHandle, PDSURL: testkit.Endpoints().PDS.BaseURL,
		Method: identity.MethodDNS, ResolvedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed cached identity: %v", err)
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM identity_cache WHERE identifier = $1`, did); err != nil {
		t.Fatalf("orphan old handle cache row: %v", err)
	}
	if _, err := cache.Get(ctx, oldHandle); err != nil {
		t.Fatalf("old handle cache row missing before event: %v", err)
	}
	c.caching.failPurgeIdentifier, c.caching.failPurgeRemaining = oldHandle, 1
	assertTransientIdentityError(t, c.deliver("alice.event.freshb8orphan.test"))
	if got := c.storedHandle(t); got != oldHandle {
		t.Errorf("stored handle after failed old-handle purge = %q, want %q", got, oldHandle)
	}
	if err := c.deliver("alice.event.freshb8orphan.test"); err != nil {
		t.Fatalf("replay identity event: %v", err)
	}
	if got := c.storedHandle(t); got != "alice.new.freshb8orphan.test" {
		t.Errorf("stored handle after replay = %q, want alice.new.freshb8orphan.test", got)
	}
	if _, err := cache.Get(ctx, oldHandle); err == nil {
		t.Error("old handle cache row remains after replay")
	} else {
		var miss *identity.ErrCacheMiss
		if !errors.As(err, &miss) {
			t.Errorf("old handle cache lookup after replay: want cache miss, got %v", err)
		}
	}
}

func TestUserIdentityEventUsesFreshVerifiedHandle(t *testing.T) {
	for _, tc := range []struct {
		name, did, stored, event, resolved string
	}{
		{"different event handle", "did:plc:freshb1distinct", "alice.old.freshb1distinct.test", "alice.event.freshb1distinct.test", "alice.new.freshb1distinct.test"},
		{"event agrees with stored handle", "did:plc:freshb1equal", "alice.old.freshb1equal.test", "alice.old.freshb1equal.test", "alice.new.freshb1equal.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newIdentityConsumerCase(t, tc.did, tc.stored, &identity.Identity{DID: tc.did, Handle: tc.resolved}, nil)
			if err := c.deliver(tc.event); err != nil {
				t.Fatalf("deliver identity event: %v", err)
			}
			if got := c.storedHandle(t); got != tc.resolved {
				t.Errorf("stored handle = %q, want verified %q", got, tc.resolved)
			}
			if c.uncached.resolves != 1 {
				t.Errorf("uncached Resolve calls = %d, want 1", c.uncached.resolves)
			}
		})
	}
}

func TestUserIdentityStaleEventDoesNotRewriteCurrentHandle(t *testing.T) {
	const did = "did:plc:freshb2stale"
	c := newIdentityConsumerCase(t, did, "alice.new.freshb2stale.test",
		&identity.Identity{DID: did, Handle: "alice.new.freshb2stale.test"}, nil)
	ctx := context.Background()
	// Fix the starting timestamp so a redundant UPDATE ... SET updated_at=NOW()
	// cannot pass due to clock resolution or two statements in the same instant.
	if _, err := c.db.ExecContext(ctx, `UPDATE users SET updated_at = $1 WHERE did = $2`,
		time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC), did); err != nil {
		t.Fatalf("set starting timestamp: %v", err)
	}
	var before, after time.Time
	if err := c.db.QueryRowContext(ctx, `SELECT updated_at FROM users WHERE did = $1`, did).Scan(&before); err != nil {
		t.Fatalf("read timestamp before event: %v", err)
	}
	if err := c.deliver("alice.old.freshb2stale.test"); err != nil {
		t.Fatalf("deliver stale event: %v", err)
	}
	if got := c.storedHandle(t); got != "alice.new.freshb2stale.test" {
		t.Errorf("stored handle = %q, want current verified handle", got)
	}
	if c.uncached.resolves != 1 {
		t.Errorf("uncached Resolve calls = %d, want 1", c.uncached.resolves)
	}
	if err := c.db.QueryRowContext(ctx, `SELECT updated_at FROM users WHERE did = $1`, did).Scan(&after); err != nil {
		t.Fatalf("read timestamp after event: %v", err)
	}
	if !after.Equal(before) {
		t.Errorf("updated_at changed from %s to %s on stale event", before, after)
	}
}

func TestUserIdentityUnknownDIDReadsOnceWithoutResolving(t *testing.T) {
	c := newIdentityConsumerCase(t, "did:plc:freshb4unknown", "", nil, nil)
	for _, handle := range []string{"alice.event.freshb4unknown.test", ""} {
		before := c.counted.gets
		if err := c.deliver(handle); err != nil {
			t.Errorf("unknown DID event with handle %q: %v", handle, err)
		}
		if calls := c.counted.gets - before; calls != 1 {
			t.Errorf("unknown DID event with handle %q made %d GetUserByDID calls, want 1", handle, calls)
		}
		if c.caching.resolves != 0 || c.caching.purges != 0 || c.uncached.resolves != 0 || c.uncached.purges != 0 {
			t.Errorf("unknown DID event with handle %q called a resolver: caching Resolve/Purge=%d/%d, uncached Resolve/Purge=%d/%d",
				handle, c.caching.resolves, c.caching.purges, c.uncached.resolves, c.uncached.purges)
		}
	}
}

func TestUserIdentityUnverifiedResolutionIsClassified(t *testing.T) {
	for _, tc := range []struct {
		name, did, stored, event string
		answer                   *identity.Identity
		resolveErr, wantErr      error
	}{
		{"resolution error", "did:plc:freshb5error", "alice.old.freshb5error.test", "alice.event.freshb5error.test", nil, errors.New("directory unavailable"), jetstream.ErrUnresolvedReference},
		{"handle invalid on handle-less event", "did:plc:freshb5invalid", "alice.old.freshb5invalid.test", "", &identity.Identity{DID: "did:plc:freshb5invalid", Handle: "handle.invalid"}, nil, jetstream.ErrUnresolvedReference},
		{"different subject", "did:plc:freshb5mismatch", "alice.old.freshb5mismatch.test", "alice.event.freshb5mismatch.test", &identity.Identity{DID: "did:plc:freshb5other", Handle: "alice.new.freshb5mismatch.test"}, nil, jetstream.ErrPermanentEvent},
		{"invalid identifier", "did:plc:freshb5identifier", "alice.old.freshb5identifier.test", "alice.event.freshb5identifier.test", nil, &identity.ErrInvalidIdentifier{Identifier: "did:plc:freshb5identifier", Reason: "invalid"}, jetstream.ErrPermanentEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newIdentityConsumerCase(t, tc.did, tc.stored, tc.answer, tc.resolveErr)
			if err := c.deliver(tc.event); !errors.Is(err, tc.wantErr) {
				t.Errorf("event error = %v, want errors.Is(..., %v)", err, tc.wantErr)
			}
			if got := c.storedHandle(t); got != tc.stored {
				t.Errorf("stored handle = %q, want unchanged %q", got, tc.stored)
			}
			if c.uncached.resolves != 1 {
				t.Errorf("uncached Resolve calls = %d, want 1", c.uncached.resolves)
			}
		})
	}
}

func TestUserIdentityHandlelessEventRefreshesKnownUser(t *testing.T) {
	const did = "did:plc:freshb7handleless"
	c := newIdentityConsumerCase(t, did, "alice.old.freshb7handleless.test",
		&identity.Identity{DID: did, Handle: "alice.new.freshb7handleless.test"}, nil)
	if err := c.deliver(""); err != nil {
		t.Fatalf("deliver handle-less event: %v", err)
	}
	if c.uncached.resolves != 1 {
		t.Errorf("uncached Resolve calls = %d, want 1", c.uncached.resolves)
	}
	if got := c.storedHandle(t); got != "alice.new.freshb7handleless.test" {
		t.Errorf("stored handle = %q, want verified fresh handle", got)
	}
}

func TestUserIndexingFromJetstream(t *testing.T) {
	t.Parallel()
	userService, resolver, db := newUserService(t)
	pdsURL := testkit.Endpoints().PDS.BaseURL

	ctx := context.Background()

	t.Run("Skip identity event for non-existent user", func(t *testing.T) {
		// Identity events for users not in our database are silently skipped:
		// accounts are indexed at OAuth login or signup, never from the
		// firehose, so that the AppView does not accumulate every handle on the
		// network.
		event := jetstream.JetstreamEvent{
			Did:  "did:plc:nonexistent123",
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "did:plc:nonexistent123",
				Handle: "nonexistent.jetstream.test",
				Seq:    12345,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		consumer := jetstream.NewUserEventConsumer(userService, resolver)

		err := consumer.HandleIdentityEventPublic(ctx, &event)
		if err != nil {
			t.Fatalf("expected nil error for non-existent user, got: %v", err)
		}

		_, err = userService.GetUserByDID(ctx, "did:plc:nonexistent123")
		if err == nil {
			t.Fatal("expected user to NOT be created, but found in database")
		}
	})

	t.Run("Idempotent indexing - duplicate event", func(t *testing.T) {
		// The connector rewinds its cursor on reconnect, so the same handle
		// change is routinely delivered twice. Fresh resolution makes the
		// second delivery an equal-handle no-op.
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    "did:plc:duplicate123",
			Handle: "duplicate.old.test",
			PDSURL: pdsURL,
		})
		if err != nil {
			t.Fatalf("failed to create initial user: %v", err)
		}

		event := jetstream.JetstreamEvent{
			Did:  "did:plc:duplicate123",
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "did:plc:duplicate123",
				Handle: "duplicate.test",
				Seq:    12346,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		consumer := jetstream.NewUserEventConsumer(userService, resolver,
			jetstream.WithUserUncachedIdentityResolver(&scriptedIdentityResolver{
				wantDID: "did:plc:duplicate123",
				answer:  &identity.Identity{DID: "did:plc:duplicate123", Handle: "duplicate.test"},
			}),
			jetstream.WithUserIdentityRefreshLock(jetstream.NewPostgresIdentityRefreshLock(db, jetstream.IdentityRefreshLockWaitTimeout)))

		for attempt := 1; attempt <= 2; attempt++ {
			if err := consumer.HandleIdentityEventPublic(ctx, &event); err != nil {
				t.Fatalf("delivery %d of the same identity event should be handled gracefully: %v", attempt, err)
			}

			user, err := userService.GetUserByDID(ctx, "did:plc:duplicate123")
			if err != nil {
				t.Fatalf("failed to get user after delivery %d: %v", attempt, err)
			}
			if user.Handle != "duplicate.test" {
				t.Errorf("after delivery %d: expected handle duplicate.test, got %s", attempt, user.Handle)
			}
		}
	})

	t.Run("Update multiple existing users via identity events", func(t *testing.T) {
		testUsers := []struct {
			did       string
			oldHandle string
			newHandle string
		}{
			{"did:plc:multi1", "user1.old.test", "user1.new.test"},
			{"did:plc:multi2", "user2.old.test", "user2.new.test"},
			{"did:plc:multi3", "user3.old.test", "user3.new.test"},
		}

		for _, u := range testUsers {
			_, err := userService.CreateUser(ctx, users.CreateUserRequest{
				DID:    u.did,
				Handle: u.oldHandle,
				PDSURL: pdsURL,
			})
			if err != nil {
				t.Fatalf("failed to create user %s: %v", u.oldHandle, err)
			}
		}

		for _, u := range testUsers {
			consumer := jetstream.NewUserEventConsumer(userService, resolver,
				jetstream.WithUserUncachedIdentityResolver(&scriptedIdentityResolver{
					wantDID: u.did, answer: &identity.Identity{DID: u.did, Handle: u.newHandle},
				}),
				jetstream.WithUserIdentityRefreshLock(jetstream.NewPostgresIdentityRefreshLock(db, jetstream.IdentityRefreshLockWaitTimeout)))
			event := jetstream.JetstreamEvent{
				Did:  u.did,
				Kind: "identity",
				Identity: &jetstream.IdentityEvent{
					Did:    u.did,
					Handle: u.newHandle,
					Seq:    12345,
					Time:   time.Now().Format(time.RFC3339),
				},
			}

			err := consumer.HandleIdentityEventPublic(ctx, &event)
			if err != nil {
				t.Fatalf("failed to handle identity event for %s: %v", u.newHandle, err)
			}
		}

		for _, u := range testUsers {
			user, err := userService.GetUserByDID(ctx, u.did)
			if err != nil {
				t.Fatalf("user %s not found: %v", u.did, err)
			}

			if user.Handle != u.newHandle {
				t.Errorf("expected handle %s, got %s", u.newHandle, user.Handle)
			}
		}
	})

	t.Run("Skip invalid events", func(t *testing.T) {
		consumer := jetstream.NewUserEventConsumer(userService, resolver)

		// Missing DID: structurally invalid, and a replay would fail
		// identically, so it must be a permanent rejection rather than a retry.
		invalidEvent1 := jetstream.JetstreamEvent{
			Did:  "",
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "",
				Handle: "invalid.test",
				Seq:    12345,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		err := consumer.HandleIdentityEventPublic(ctx, &invalidEvent1)
		if err == nil {
			t.Error("expected error for missing DID, got nil")
		}

		// An unknown DID with no event handle is still ignored after the
		// known-user lookup; handle-less events for known users are refreshed.
		invalidEvent2 := jetstream.JetstreamEvent{
			Did:  "did:plc:invalid",
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    "did:plc:invalid",
				Handle: "",
				Seq:    12345,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		err = consumer.HandleIdentityEventPublic(ctx, &invalidEvent2)
		if err != nil {
			t.Errorf("handle-less identity event for unknown DID must be ignored, got: %v", err)
		}

		// Missing identity data
		invalidEvent3 := jetstream.JetstreamEvent{
			Did:      "did:plc:invalid2",
			Kind:     "identity",
			Identity: nil,
		}

		err = consumer.HandleIdentityEventPublic(ctx, &invalidEvent3)
		if err == nil {
			t.Error("expected error for nil identity data, got nil")
		}
	})
}

// TestUserServiceIdempotency covers the write side of the same seam. Signup and
// OAuth login both call CreateUser unconditionally, so a repeat call for a DID
// already indexed must return the existing account rather than fail — while a
// second account claiming a handle somebody else holds must still be refused.
func TestUserServiceIdempotency(t *testing.T) {
	t.Parallel()
	userService, _, _ := newUserService(t)
	pdsURL := testkit.Endpoints().PDS.BaseURL
	ctx := context.Background()

	t.Run("CreateUser is idempotent for duplicate DID", func(t *testing.T) {
		req := users.CreateUserRequest{
			DID:    "did:plc:idempotent123",
			Handle: "idempotent.test",
			PDSURL: pdsURL,
		}

		user1, err := userService.CreateUser(ctx, req)
		if err != nil {
			t.Fatalf("first creation failed: %v", err)
		}

		user2, err := userService.CreateUser(ctx, req)
		if err != nil {
			t.Fatalf("second creation should be idempotent: %v", err)
		}

		if user1.DID != user2.DID {
			t.Errorf("expected same DID, got %s and %s", user1.DID, user2.DID)
		}

		// Equal created_at is what proves the row was returned rather than
		// replaced; equal DIDs alone would also hold for an overwrite.
		if user1.CreatedAt != user2.CreatedAt {
			t.Errorf("expected same user (same created_at), got different timestamps")
		}
	})

	t.Run("CreateUser fails for duplicate handle with different DID", func(t *testing.T) {
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    "did:plc:handleconflict1",
			Handle: "conflicting.handle",
			PDSURL: pdsURL,
		})
		if err != nil {
			t.Fatalf("first creation failed: %v", err)
		}

		_, err = userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    "did:plc:handleconflict2",
			Handle: "conflicting.handle", // Same handle, different DID
			PDSURL: pdsURL,
		})

		if err == nil {
			t.Fatal("expected error for duplicate handle, got nil")
		}

		if !strings.Contains(err.Error(), "handle already taken") {
			t.Errorf("expected 'handle already taken' error, got: %v", err)
		}
	})
}
