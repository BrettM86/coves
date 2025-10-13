package integration

import (
	"Coves/internal/atproto/identity"
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// uniqueID generates a unique identifier for test isolation
func uniqueID() string {
	return fmt.Sprintf("test-%d", time.Now().UnixNano())
}

// TestIdentityCache tests the PostgreSQL identity cache operations
func TestIdentityCache(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	cache := identity.NewPostgresCache(db, 5*time.Minute)
	ctx := context.Background()

	// Generate unique test prefix for parallel safety
	testID := fmt.Sprintf("test-%d", time.Now().UnixNano())

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

		// Set identity in cache
		if err := cache.Set(ctx, ident); err != nil {
			t.Fatalf("Failed to cache identity: %v", err)
		}

		// Get by handle
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
		// Should be able to retrieve by DID as well (bidirectional cache)
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
		// Update with new PDS URL
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

		// Should now be a cache miss
		_, err := cache.Get(ctx, "alice.test")
		if err == nil {
			t.Error("Expected cache miss after deletion, got nil error")
		}
	})

	t.Run("Purge Removes Both Handle and DID Entries", func(t *testing.T) {
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

		// Verify both entries exist
		if _, err := cache.Get(ctx, "purge.test"); err != nil {
			t.Errorf("Handle entry should exist: %v", err)
		}
		if _, err := cache.Get(ctx, "did:plc:purgetest"); err != nil {
			t.Errorf("DID entry should exist: %v", err)
		}

		// Purge by handle
		if err := cache.Purge(ctx, "purge.test"); err != nil {
			t.Fatalf("Failed to purge: %v", err)
		}

		// Both should be gone
		if _, err := cache.Get(ctx, "purge.test"); err == nil {
			t.Error("Handle entry should be purged")
		}
		if _, err := cache.Get(ctx, "did:plc:purgetest"); err == nil {
			t.Error("DID entry should be purged")
		}
	})

	t.Run("Handle Normalization - Case Insensitive", func(t *testing.T) {
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

		// Should be retrievable with different casing
		cached, err := cache.Get(ctx, "ALICE.TEST")
		if err != nil {
			t.Fatalf("Failed to get identity with different casing: %v", err)
		}

		if cached.DID != "did:plc:casetest" {
			t.Errorf("Expected DID did:plc:casetest, got %s", cached.DID)
		}

		// Cleanup
		if delErr := cache.Delete(ctx, "alice.test"); delErr != nil {
			t.Logf("Failed to delete cache entry: %v", delErr)
		}
	})

	t.Run("DID is Case Sensitive", func(t *testing.T) {
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

		// Should retrieve with exact case
		if _, err := cache.Get(ctx, "did:plc:CaseSensitive"); err != nil {
			t.Errorf("Should retrieve DID with exact case: %v", err)
		}

		// Different case should miss (DIDs are case-sensitive)
		if _, err := cache.Get(ctx, "did:plc:casesensitive"); err == nil {
			t.Error("Should NOT retrieve DID with different case")
		}

		// Cleanup
		if delErr := cache.Delete(ctx, "did:plc:CaseSensitive"); delErr != nil {
			t.Logf("Failed to delete cache entry: %v", delErr)
		}
	})
}

// TestIdentityCacheTTL tests that expired cache entries are not returned
func TestIdentityCacheTTL(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Create cache with very short TTL (reduced from 1s to 100ms for faster, less flaky tests)
	ttl := 100 * time.Millisecond
	cache := identity.NewPostgresCache(db, ttl)
	ctx := context.Background()

	// Use unique ID for test isolation
	testID := uniqueID()

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

	// Should be retrievable immediately
	if _, err := cache.Get(ctx, ident.Handle); err != nil {
		t.Errorf("Should retrieve fresh cache entry: %v", err)
	}

	// Wait for TTL to expire (1.5x TTL for safety margin on slow systems)
	waitTime := time.Duration(float64(ttl) * 1.5)
	t.Logf("Waiting %s for cache entry to expire (TTL=%s)...", waitTime, ttl)
	time.Sleep(waitTime)

	// Should now be a cache miss
	_, err := cache.Get(ctx, ident.Handle)
	if err == nil {
		t.Error("Expected cache miss after TTL expiration, got nil error")
	}
}

// TestIdentityResolverWithCache tests the caching resolver behavior
func TestIdentityResolverWithCache(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	cache := identity.NewPostgresCache(db, 5*time.Minute)

	// Clean slate
	if _, err := db.Exec("TRUNCATE identity_cache"); err != nil {
		t.Logf("Warning: failed to truncate identity_cache: %v", err)
	}

	// Create resolver with caching
	resolver := identity.NewResolver(db, identity.Config{
		PLCURL:   "https://plc.directory",
		CacheTTL: 5 * time.Minute,
	})

	ctx := context.Background()

	t.Run("Resolve Invalid Identifier", func(t *testing.T) {
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
		// Pre-populate cache with known identity
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
		// Pre-populate cache
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

		// Verify it's cached
		if _, err := cache.Get(ctx, "purgetest.test"); err != nil {
			t.Fatalf("Identity should be cached: %v", err)
		}

		// Purge via resolver
		if err := resolver.Purge(ctx, "purgetest.test"); err != nil {
			t.Fatalf("Failed to purge: %v", err)
		}

		// Should be gone from cache
		if _, err := cache.Get(ctx, "purgetest.test"); err == nil {
			t.Error("Identity should be purged from cache")
		}
	})
}

// TestIdentityResolverRealHandles tests resolution with real atProto handles
// This is an optional integration test that requires network access
func TestIdentityResolverRealHandles(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping real handle resolution test in short mode")
	}

	// Skip if environment variable is not set (opt-in for real network tests)
	if os.Getenv("TEST_REAL_HANDLES") != "1" {
		t.Skip("Skipping real handle resolution - set TEST_REAL_HANDLES=1 to enable")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	resolver := identity.NewResolver(db, identity.Config{
		PLCURL:   "https://plc.directory",
		CacheTTL: 10 * time.Minute,
	})

	ctx := context.Background()

	testCases := []struct {
		name           string
		handle         string
		expectedMethod identity.ResolutionMethod
		expectError    bool
	}{
		{
			name:           "Resolve bsky.app (well-known handle)",
			handle:         "bsky.app",
			expectError:    false,
			expectedMethod: identity.MethodHTTPS,
		},
		{
			name:        "Resolve nonexistent handle",
			handle:      "this-handle-definitely-does-not-exist-12345.bsky.social",
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ident, err := resolver.Resolve(ctx, tc.handle)

			if tc.expectError {
				if err == nil {
					t.Error("Expected error for nonexistent handle")
				}
				return
			}

			if err != nil {
				t.Fatalf("Failed to resolve handle %s: %v", tc.handle, err)
			}

			if ident.Handle != tc.handle {
				t.Errorf("Expected handle %s, got %s", tc.handle, ident.Handle)
			}

			if ident.DID == "" {
				t.Error("Expected non-empty DID")
			}

			if ident.PDSURL == "" {
				t.Error("Expected non-empty PDS URL")
			}

			t.Logf("✅ Resolved %s → %s (PDS: %s, Method: %s)",
				ident.Handle, ident.DID, ident.PDSURL, ident.Method)

			// Second resolution should hit cache
			ident2, err := resolver.Resolve(ctx, tc.handle)
			if err != nil {
				t.Fatalf("Failed second resolution: %v", err)
			}

			if ident2.Method != identity.MethodCache {
				t.Errorf("Second resolution should be from cache, got method: %s", ident2.Method)
			}

			t.Logf("✅ Second resolution from cache: %s (Method: %s)", tc.handle, ident2.Method)
		})
	}
}

// TestResolveDID tests DID document resolution
func TestResolveDID(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping DID resolution test in short mode")
	}

	if os.Getenv("TEST_REAL_HANDLES") != "1" {
		t.Skip("Skipping DID resolution - set TEST_REAL_HANDLES=1 to enable")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	resolver := identity.NewResolver(db, identity.Config{
		PLCURL:   "https://plc.directory",
		CacheTTL: 10 * time.Minute,
	})

	ctx := context.Background()

	t.Run("Resolve Real DID Document", func(t *testing.T) {
		// First resolve a handle to get a real DID
		ident, err := resolver.Resolve(ctx, "bsky.app")
		if err != nil {
			t.Skipf("Failed to resolve handle for DID test: %v", err)
		}

		// Now resolve the DID document
		doc, err := resolver.ResolveDID(ctx, ident.DID)
		if err != nil {
			t.Fatalf("Failed to resolve DID document: %v", err)
		}

		if doc.DID != ident.DID {
			t.Errorf("Expected DID %s, got %s", ident.DID, doc.DID)
		}

		// Should have at least PDS service
		if len(doc.Service) == 0 {
			t.Error("Expected at least one service in DID document")
		}

		// Find PDS service
		foundPDS := false
		for _, svc := range doc.Service {
			if svc.Type == "AtprotoPersonalDataServer" {
				foundPDS = true
				if svc.ServiceEndpoint == "" {
					t.Error("PDS service endpoint should not be empty")
				}
				t.Logf("✅ PDS Service: %s", svc.ServiceEndpoint)
			}
		}

		if !foundPDS {
			t.Error("Expected to find AtprotoPersonalDataServer service in DID document")
		}
	})

	t.Run("Resolve Invalid DID", func(t *testing.T) {
		_, err := resolver.ResolveDID(ctx, "not-a-did")
		if err == nil {
			t.Error("Expected error for invalid DID format")
		}
	})
}
