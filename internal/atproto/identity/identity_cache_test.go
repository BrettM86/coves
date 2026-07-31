//go:build integration

// The identity cache is a Postgres table, and every property worth asserting
// about it is a property of that table: the bidirectional handle/DID indexing,
// the case folding that applies to handles but must NOT apply to DIDs, the TTL
// that makes a row invisible without deleting it, and the purge that has to
// remove both directions at once. None of those survive being tested against an
// in-memory stand-in, so these run against a real database.
//
// Everything here is hermetic. Resolution is exercised only where the cache
// answers or where the identifier is rejected on syntax before any lookup is
// attempted; the tests that genuinely resolve a handle against the public
// network live in tests/live, which is the only tier allowed to leave the
// machine.
//
// The tests are in an external test package because they import
// Coves/tests/testkit alongside this package, and the fixtures tier that
// testkit anchors imports this package in turn.
package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/tests/testkit"
)

// TestIdentityCache covers the storage contract of the Postgres identity cache.
func TestIdentityCache(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	cache := identity.NewPostgresCache(db, 5*time.Minute)
	ctx := context.Background()

	testID := testkit.UniqueID(t)

	t.Run("Cache Miss on Empty Cache", func(t *testing.T) {
		_, err := cache.Get(ctx, testID+"-nonexistent.test")
		if err == nil {
			t.Error("Expected cache miss error, got nil")
		}
	})

	t.Run("Set and Get Identity by Handle", func(t *testing.T) {
		ident := &identity.Identity{
			DID:        "did:plc:" + testID + "-test123abc",
			Handle:     testID + "-alice.test",
			PDSURL:     "https://pds.alice.test",
			ResolvedAt: time.Now().UTC(),
			Method:     identity.MethodHTTPS,
		}

		if err := cache.Set(ctx, ident); err != nil {
			t.Fatalf("Failed to cache identity: %v", err)
		}

		cached, err := cache.Get(ctx, ident.Handle)
		if err != nil {
			t.Fatalf("Failed to get cached identity by handle: %v", err)
		}

		if cached.DID != ident.DID {
			t.Errorf("Expected DID %s, got %s", ident.DID, cached.DID)
		}
		if cached.Handle != ident.Handle {
			t.Errorf("Expected handle %s, got %s", ident.Handle, cached.Handle)
		}
		if cached.PDSURL != ident.PDSURL {
			t.Errorf("Expected PDS URL %s, got %s", ident.PDSURL, cached.PDSURL)
		}
	})

	t.Run("Get Identity by DID", func(t *testing.T) {
		// One Set writes both directions: callers look an identity up by
		// whichever half they hold, and a one-way cache would silently halve
		// the hit rate on the DID-keyed path the consumers use.
		expectedDID := "did:plc:" + testID + "-test123abc"
		expectedHandle := testID + "-alice.test"

		cached, err := cache.Get(ctx, expectedDID)
		if err != nil {
			t.Fatalf("Failed to get cached identity by DID: %v", err)
		}

		if cached.Handle != expectedHandle {
			t.Errorf("Expected handle %s, got %s", expectedHandle, cached.Handle)
		}
	})

	t.Run("Update Existing Cache Entry", func(t *testing.T) {
		// A re-resolution must overwrite in place. An account that migrates to
		// a new PDS would otherwise keep being sent to the old one until the
		// TTL expired.
		updated := &identity.Identity{
			DID:        "did:plc:test123abc",
			Handle:     "alice.test",
			PDSURL:     "https://new-pds.alice.test",
			ResolvedAt: time.Now(),
			Method:     identity.MethodHTTPS,
		}

		if err := cache.Set(ctx, updated); err != nil {
			t.Fatalf("Failed to update cached identity: %v", err)
		}

		cached, err := cache.Get(ctx, "alice.test")
		if err != nil {
			t.Fatalf("Failed to get updated identity: %v", err)
		}

		if cached.PDSURL != "https://new-pds.alice.test" {
			t.Errorf("Expected updated PDS URL, got %s", cached.PDSURL)
		}
	})

	t.Run("Delete Cache Entry", func(t *testing.T) {
		if err := cache.Delete(ctx, "alice.test"); err != nil {
			t.Fatalf("Failed to delete cache entry: %v", err)
		}

		_, err := cache.Get(ctx, "alice.test")
		if err == nil {
			t.Error("Expected cache miss after deletion, got nil error")
		}
	})

	t.Run("Purge Removes Both Handle and DID Entries", func(t *testing.T) {
		// Purge is what the Jetstream identity consumer calls on a handle
		// change. Leaving either direction behind keeps resolving an abandoned
		// handle — which, once the handle is re-registered by someone else,
		// resolves it to the wrong account.
		ident := &identity.Identity{
			DID:        "did:plc:purgetest",
			Handle:     "purge.test",
			PDSURL:     "https://pds.purge.test",
			ResolvedAt: time.Now(),
			Method:     identity.MethodDNS,
		}

		if err := cache.Set(ctx, ident); err != nil {
			t.Fatalf("Failed to cache identity: %v", err)
		}

		if _, err := cache.Get(ctx, "purge.test"); err != nil {
			t.Errorf("Handle entry should exist: %v", err)
		}
		if _, err := cache.Get(ctx, "did:plc:purgetest"); err != nil {
			t.Errorf("DID entry should exist: %v", err)
		}

		if err := cache.Purge(ctx, "purge.test"); err != nil {
			t.Fatalf("Failed to purge: %v", err)
		}

		if _, err := cache.Get(ctx, "purge.test"); err == nil {
			t.Error("Handle entry should be purged")
		}
		if _, err := cache.Get(ctx, "did:plc:purgetest"); err == nil {
			t.Error("DID entry should be purged")
		}
	})

	t.Run("Handle Normalization - Case Insensitive", func(t *testing.T) {
		// atProto handles are case-insensitive, so a lookup that preserved case
		// would miss every time a client sent "Alice.Test" and re-resolve over
		// the network for an entry already present.
		ident := &identity.Identity{
			DID:        "did:plc:casetest",
			Handle:     "Alice.Test",
			PDSURL:     "https://pds.alice.test",
			ResolvedAt: time.Now(),
			Method:     identity.MethodHTTPS,
		}

		if err := cache.Set(ctx, ident); err != nil {
			t.Fatalf("Failed to cache identity: %v", err)
		}

		cached, err := cache.Get(ctx, "ALICE.TEST")
		if err != nil {
			t.Fatalf("Failed to get identity with different casing: %v", err)
		}

		if cached.DID != "did:plc:casetest" {
			t.Errorf("Expected DID did:plc:casetest, got %s", cached.DID)
		}
	})

	t.Run("DID is Case Sensitive", func(t *testing.T) {
		// DIDs are NOT case-insensitive: did:plc identifiers are base32 and two
		// DIDs differing only in case are two different accounts. Folding them
		// the way handles are folded would collide distinct identities.
		ident := &identity.Identity{
			DID:        "did:plc:CaseSensitive",
			Handle:     "sensitive.test",
			PDSURL:     "https://pds.test",
			ResolvedAt: time.Now(),
			Method:     identity.MethodHTTPS,
		}

		if err := cache.Set(ctx, ident); err != nil {
			t.Fatalf("Failed to cache identity: %v", err)
		}

		if _, err := cache.Get(ctx, "did:plc:CaseSensitive"); err != nil {
			t.Errorf("Should retrieve DID with exact case: %v", err)
		}

		if _, err := cache.Get(ctx, "did:plc:casesensitive"); err == nil {
			t.Error("Should NOT retrieve DID with different case")
		}
	})
}

// TestIdentityCacheTTL proves an entry stops being served once it is older than
// the configured lifetime.
//
// The TTL is what bounds how long the AppView can keep routing to a stale PDS
// after an account migrates, so "expired rows are still returned" is a
// correctness bug and not merely a staleness one.
func TestIdentityCacheTTL(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	// A short lifetime keeps the test fast; the expiry check is a timestamp
	// comparison, so the absolute value carries no meaning. One second rather
	// than something tighter because the fresh-entry assertion below has to
	// happen inside the lifetime, and a hundred-millisecond budget for a query
	// on a loaded runner is a flake waiting to happen.
	const ttl = time.Second
	cache := identity.NewPostgresCache(db, ttl)
	ctx := context.Background()

	testID := testkit.UniqueID(t)

	ident := &identity.Identity{
		DID:        "did:plc:" + testID,
		Handle:     testID + ".ttl.test",
		PDSURL:     "https://pds.ttl.test",
		ResolvedAt: time.Now().UTC(),
		Method:     identity.MethodHTTPS,
	}

	if err := cache.Set(ctx, ident); err != nil {
		t.Fatalf("Failed to cache identity: %v", err)
	}

	// Read it back while it is still fresh. Without this the wait below would
	// pass for a cache that never stored the entry in the first place.
	if _, err := cache.Get(ctx, ident.Handle); err != nil {
		t.Fatalf("Should retrieve fresh cache entry: %v", err)
	}

	// Poll rather than sleep for the TTL: the deadline is generous because a
	// loaded CI runner can take far longer than the lifetime itself to get
	// round to the next query, and the assertion is "it eventually stops being
	// served", not "it stops being served at exactly 100ms".
	testkit.WaitFor(t, 30*time.Second, func() (bool, error) {
		_, err := cache.Get(ctx, ident.Handle)
		if err == nil {
			return false, nil
		}
		var miss *identity.ErrCacheMiss
		if errors.As(err, &miss) {
			return true, nil
		}
		return false, err
	}, testkit.WithDescription("the expired cache entry to stop being served"),
		testkit.WithPollInterval(20*time.Millisecond))
}

// TestIdentityResolverWithCache covers the caching resolver's own behaviour:
// what it does before it would reach the network, and what it does to the cache
// afterwards.
func TestIdentityResolverWithCache(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	cache := identity.NewPostgresCache(db, 5*time.Minute)

	// The PLC address comes from the test stack rather than a literal. Nothing
	// below actually reaches it — each case either hits the pre-populated cache
	// or is rejected on identifier syntax first — but a resolver configured
	// with the public directory would turn any future regression in that
	// ordering into a silent call to the real network.
	resolver := identity.NewResolver(db, identity.Config{
		PLCURL:   testkit.Endpoints().PLC.BaseURL,
		CacheTTL: 5 * time.Minute,
	})

	ctx := context.Background()

	t.Run("Resolve Invalid Identifier", func(t *testing.T) {
		// Both of these are rejected by identifier parsing before any lookup is
		// attempted, which is also what keeps this test hermetic.
		_, err := resolver.Resolve(ctx, "")
		if err == nil {
			t.Error("Expected error for empty identifier")
		}

		_, err = resolver.Resolve(ctx, "invalid format")
		if err == nil {
			t.Error("Expected error for invalid identifier format")
		}
	})

	t.Run("ResolveHandle Returns DID and PDS URL", func(t *testing.T) {
		// Pre-populating the cache is what makes this a cache-hit test: the
		// handle is not registered anywhere, so a resolver that consulted the
		// network would fail rather than return these values.
		ident := &identity.Identity{
			DID:        "did:plc:resolvetest",
			Handle:     "resolve.test",
			PDSURL:     "https://pds.resolve.test",
			ResolvedAt: time.Now(),
			Method:     identity.MethodDNS,
		}

		if err := cache.Set(ctx, ident); err != nil {
			t.Fatalf("Failed to pre-populate cache: %v", err)
		}

		did, pdsURL, err := resolver.ResolveHandle(ctx, "resolve.test")
		if err != nil {
			t.Fatalf("Failed to resolve handle: %v", err)
		}

		if did != "did:plc:resolvetest" {
			t.Errorf("Expected DID did:plc:resolvetest, got %s", did)
		}
		if pdsURL != "https://pds.resolve.test" {
			t.Errorf("Expected PDS URL https://pds.resolve.test, got %s", pdsURL)
		}
	})

	t.Run("Purge Removes from Cache", func(t *testing.T) {
		ident := &identity.Identity{
			DID:        "did:plc:purge123",
			Handle:     "purgetest.test",
			PDSURL:     "https://pds.test",
			ResolvedAt: time.Now(),
			Method:     identity.MethodHTTPS,
		}

		if err := cache.Set(ctx, ident); err != nil {
			t.Fatalf("Failed to cache identity: %v", err)
		}

		if _, err := cache.Get(ctx, "purgetest.test"); err != nil {
			t.Fatalf("Identity should be cached: %v", err)
		}

		if err := resolver.Purge(ctx, "purgetest.test"); err != nil {
			t.Fatalf("Failed to purge: %v", err)
		}

		if _, err := cache.Get(ctx, "purgetest.test"); err == nil {
			t.Error("Identity should be purged from cache")
		}
	})
}
