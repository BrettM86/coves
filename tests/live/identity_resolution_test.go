//go:build live

package live

import (
	"Coves/internal/atproto/identity"
	"context"
	"database/sql"
	"testing"
	"time"
)

// purgeIdentityCache removes any cached rows for the given identifiers, so the
// next resolution provably goes to the network.
//
// The identity cache is a table in the shared test database and setupTestDB
// does not clear it, so without this a run inherits rows from every previous
// run: these tests would resolve entirely from Postgres and still pass with
// the public internet unplugged, which is the one thing the live tier exists
// to rule out.
func purgeIdentityCache(t *testing.T, db *sql.DB, identifiers ...string) {
	t.Helper()
	for _, id := range identifiers {
		if _, err := db.Exec(
			`DELETE FROM identity_cache WHERE identifier = $1 OR handle = $1 OR did = $1`, id,
		); err != nil {
			t.Fatalf("Failed to purge identity cache for %q: %v", id, err)
		}
	}
}

// TestIdentityResolverRealHandles resolves real atProto handles against the
// production PLC directory. The `live` build tag is the opt-in — this used to
// carry a TEST_REAL_HANDLES=1 gate, which meant it skipped on every merge-gate
// run and therefore never verified anything. Under -tags live it runs
// unconditionally and fails when the public network is unreachable.
func TestIdentityResolverRealHandles(t *testing.T) {
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
			purgeIdentityCache(t, db, tc.handle)

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

			// The cache was purged above, so this resolution must have gone out
			// to the network. Without the check a stale row would let the whole
			// test pass offline.
			if ident.Method == identity.MethodCache {
				t.Errorf("First resolution came from cache after a purge; it never reached %s", tc.handle)
			}
			if ident.Method != tc.expectedMethod {
				t.Errorf("First resolution method = %s, want %s", ident.Method, tc.expectedMethod)
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

// TestResolveDID resolves a real DID document from the production PLC
// directory. Opt-in via the `live` build tag; see TestIdentityResolverRealHandles.
func TestResolveDID(t *testing.T) {
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
		// TestIdentityResolverRealHandles resolves the same handle and leaves it
		// in the identity cache, which setupTestDB does not clear — so without
		// this purge the resolution below is served from Postgres and the test
		// proves nothing about the production PLC directory.
		purgeIdentityCache(t, db, "bsky.app")

		// First resolve a handle to get a real DID
		ident, err := resolver.Resolve(ctx, "bsky.app")
		if err != nil {
			t.Fatalf("Failed to resolve handle for DID test: %v", err)
		}
		if ident.Method == identity.MethodCache {
			t.Fatal("Resolution came from cache after a purge; this test must reach the real PLC directory")
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
