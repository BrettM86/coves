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
//   - a handle change has to update the users row AND purge the identity cache,
//     in that order. Both live in Postgres, and the ordering is what stops a
//     concurrent resolution from refilling the cache with the old handle.
//
// The tests therefore drive the real jetstream.UserEventConsumer over the real
// service, repository and Postgres-backed identity cache. They are in an
// external test package because Coves/internal/db/postgres and
// Coves/internal/atproto/jetstream both import this package.
package users_test

import (
	"context"
	"database/sql"
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
// The database handle is returned alongside the service because one case
// inspects the identity cache directly, which is a second reader of the same
// clone.
func newUserService(t *testing.T) (users.UserService, identity.Resolver, *sql.DB) {
	t.Helper()
	db := testkit.DB(t)
	resolver := identity.NewResolver(db, identity.Config{
		PLCURL:   testkit.Endpoints().PLC.BaseURL,
		CacheTTL: 24 * time.Hour,
	})
	service := users.NewUserService(postgres.NewUserRepository(db), resolver, testkit.Endpoints().PDS.BaseURL, nil, "")
	return service, resolver, db
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
		// Jetstream guarantees duplicates: the connector rewinds its cursor by
		// five seconds on every reconnect, so the same identity event is
		// replayed routinely. The event is therefore delivered TWICE here, and
		// it carries a handle CHANGE — a same-handle event never reaches the
		// update branch at all, so replaying one would prove nothing about the
		// path a replay actually takes.
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

		consumer := jetstream.NewUserEventConsumer(userService, resolver)

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

	t.Run("Out-of-order identity events are last-write-wins, not seq-ordered", func(t *testing.T) {
		// PINNED BEHAVIOUR, not an endorsement. Identity events are not repo
		// commits and carry no rev, so the per-record rev gate that orders
		// commits across feeds cannot see them: handleIdentityEvent applies
		// whatever arrives, whenever it arrives. Its own header records this as
		// a KNOWN LIMITATION (internal/atproto/jetstream/user_consumer.go) —
		// a lagging feed can transiently revert a handle until the next
		// identity event for that DID lands.
		//
		// The seq field below is what a fix would key on, and this assertion is
		// what announces the fix: the moment ordering is enforced the final
		// handle becomes final.handle (seq 300) and this test goes red, which is
		// the only way an accepted limitation stops being invisible.
		did := "did:plc:outoforder123"
		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    did,
			Handle: "initial.handle",
			PDSURL: pdsURL,
		})
		if err != nil {
			t.Fatalf("failed to create initial user: %v", err)
		}

		identityEvent := func(handle string, seq int64, at time.Time) jetstream.JetstreamEvent {
			return jetstream.JetstreamEvent{
				Did:  did,
				Kind: "identity",
				Identity: &jetstream.IdentityEvent{
					Did:    did,
					Handle: handle,
					Seq:    seq,
					Time:   at.Format(time.RFC3339),
				},
			}
		}

		now := time.Now()
		// Arrival order 3, 1, 2 — every one of them out of seq order, and the
		// NEWEST (seq 300) arrives first, so a consumer that ignored ordering
		// and one that enforced it end on different handles.
		arrivals := []struct {
			label string
			event jetstream.JetstreamEvent
		}{
			{"seq 300", identityEvent("final.handle", 300, now.Add(2*time.Minute))},
			{"seq 100", identityEvent("first.handle", 100, now)},
			{"seq 200", identityEvent("middle.handle", 200, now.Add(1*time.Minute))},
		}

		consumer := jetstream.NewUserEventConsumer(userService, resolver)
		for _, arrival := range arrivals {
			if err := consumer.HandleIdentityEventPublic(ctx, &arrival.event); err != nil {
				t.Fatalf("out-of-order identity event %s must not fail the consumer: %v", arrival.label, err)
			}
		}

		user, err := userService.GetUserByDID(ctx, did)
		if err != nil {
			t.Fatalf("failed to get user: %v", err)
		}
		if user.Handle != "middle.handle" {
			t.Errorf("expected the LAST-DELIVERED handle middle.handle (seq 200), got %s.\n"+
				"If this is now final.handle, identity events have become seq-ordered: delete this pin "+
				"and the KNOWN LIMITATION note on handleIdentityEvent in "+
				"internal/atproto/jetstream/user_consumer.go", user.Handle)
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

		consumer := jetstream.NewUserEventConsumer(userService, resolver)

		for _, u := range testUsers {
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

		// Missing handle: a VALID event (handle invalidated/tombstoned) with
		// nothing to apply — must be skipped without error, or every such
		// event network-wide dead-letters as a permanent failure.
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
			t.Errorf("handle-less identity event must be skipped without error, got: %v", err)
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

	t.Run("Handle change updates database and purges cache", func(t *testing.T) {
		testID := "handlechange"
		oldHandle := "old." + testID + ".test"
		newHandle := "new." + testID + ".test"
		did := "did:plc:" + testID

		_, err := userService.CreateUser(ctx, users.CreateUserRequest{
			DID:    did,
			Handle: oldHandle,
			PDSURL: pdsURL,
		})
		if err != nil {
			t.Fatalf("failed to create initial user: %v", err)
		}

		// Stand in for a previous resolution having warmed the cache. The cache
		// is bidirectional, so this writes a handle entry and a DID entry.
		cache := identity.NewPostgresCache(db, 24*time.Hour)
		err = cache.Set(ctx, &identity.Identity{
			DID:        did,
			Handle:     oldHandle,
			PDSURL:     pdsURL,
			Method:     identity.MethodDNS,
			ResolvedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("failed to cache identity: %v", err)
		}

		cachedByHandle, err := cache.Get(ctx, oldHandle)
		if err != nil {
			t.Fatalf("expected old handle to be cached, got error: %v", err)
		}
		if cachedByHandle.DID != did {
			t.Errorf("expected cached DID %s, got %s", did, cachedByHandle.DID)
		}

		cachedByDID, err := cache.Get(ctx, did)
		if err != nil {
			t.Fatalf("expected DID to be cached, got error: %v", err)
		}
		if cachedByDID.Handle != oldHandle {
			t.Errorf("expected cached handle %s, got %s", oldHandle, cachedByDID.Handle)
		}

		event := jetstream.JetstreamEvent{
			Did:  did,
			Kind: "identity",
			Identity: &jetstream.IdentityEvent{
				Did:    did,
				Handle: newHandle,
				Seq:    99999,
				Time:   time.Now().Format(time.RFC3339),
			},
		}

		consumer := jetstream.NewUserEventConsumer(userService, resolver)
		err = consumer.HandleIdentityEventPublic(ctx, &event)
		if err != nil {
			t.Fatalf("failed to handle handle change event: %v", err)
		}

		user, err := userService.GetUserByDID(ctx, did)
		if err != nil {
			t.Fatalf("failed to get user by DID: %v", err)
		}
		if user.Handle != newHandle {
			t.Errorf("expected database to have new handle %s, got %s", newHandle, user.Handle)
		}

		// Both cache entries must go. Leaving the handle entry would keep
		// resolving the abandoned handle to this account — which is how a
		// handle handed to somebody else ends up pointing at the wrong DID.
		_, err = cache.Get(ctx, oldHandle)
		if err == nil {
			t.Error("expected old handle to be purged from cache, but it's still cached")
		}
		if _, isCacheMiss := err.(*identity.ErrCacheMiss); !isCacheMiss {
			t.Errorf("expected ErrCacheMiss for old handle, got: %v", err)
		}

		_, err = cache.Get(ctx, did)
		if err == nil {
			t.Error("expected DID to be purged from cache, but it's still cached")
		}
		if _, isCacheMiss := err.(*identity.ErrCacheMiss); !isCacheMiss {
			t.Errorf("expected ErrCacheMiss for DID, got: %v", err)
		}

		userByHandle, err := userService.GetUserByHandle(ctx, newHandle)
		if err != nil {
			t.Fatalf("failed to get user by new handle: %v", err)
		}
		if userByHandle.DID != did {
			t.Errorf("expected DID %s when looking up by new handle, got %s", did, userByHandle.DID)
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
