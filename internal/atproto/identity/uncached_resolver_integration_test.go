//go:build integration

package identity

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"Coves/tests/testkit"
)

func assertNoUncachedIdentityRows(t *testing.T, did, handle string) {
	t.Helper()
	db := testkit.DB(t)
	var rows int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM identity_cache WHERE identifier = $1 OR identifier = $2`, did, handle).Scan(&rows); err != nil {
		t.Fatalf("read identity_cache: %v", err)
	}
	if rows != 0 {
		t.Errorf("identity_cache contains %d rows for the resolved DID or handle, want 0", rows)
	}
}

func TestUncachedResolverVerifiesTwiceWithoutCaching(t *testing.T) {
	const did = "did:plc:z72i7hdynmk6r22z27h6tvur"
	const handle = "alice.freshb9twice.pds2.test"
	pds := newWellKnownPDS(t, handle, did)
	var requests atomic.Int64
	plc := declaringPLC(t, did, handle, "https://pds.example.invalid", func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/"+did {
			requests.Add(1)
		}
		return false
	})
	config := DefaultConfig(WithPrivateHostsAllowed(), WithWellKnownHosts(map[string]string{".pds2.test": pds.hostPort()}))
	config.PLCURL = plc.URL
	resolver := NewUncachedResolver(config)
	if resolver == nil {
		t.Fatal("NewUncachedResolver returned nil")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		got, err := resolver.Resolve(context.Background(), did)
		if err != nil {
			t.Fatalf("resolve attempt %d: %v", attempt, err)
		}
		if got == nil || got.DID != did || got.Handle != handle {
			t.Errorf("resolve attempt %d = %+v, want verified DID and handle", attempt, got)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("PLC DID requests = %d, want 2", got)
	}
	assertNoUncachedIdentityRows(t, did, handle)
}

func TestUncachedResolverDoesNotNegativeCacheNotFound(t *testing.T) {
	const did = "did:plc:q72i7hdynmk6r22z27h6tvus"
	const handle = "alice.freshb9retry.pds2.test"
	pds := newWellKnownPDS(t, handle, did)
	var requests atomic.Int64
	plc := declaringPLC(t, did, handle, "https://pds.example.invalid", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/"+did && requests.Add(1) == 1 {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	})
	config := DefaultConfig(WithPrivateHostsAllowed(), WithWellKnownHosts(map[string]string{".pds2.test": pds.hostPort()}))
	config.PLCURL = plc.URL
	resolver := NewUncachedResolver(config)
	if resolver == nil {
		t.Fatal("NewUncachedResolver returned nil")
	}
	if _, err := resolver.Resolve(context.Background(), did); err == nil {
		t.Error("first not-found DID lookup succeeded, want error")
	}
	got, err := resolver.Resolve(context.Background(), did)
	if err != nil {
		t.Fatalf("second DID lookup after not-found: %v", err)
	}
	if got == nil || got.DID != did || got.Handle != handle {
		t.Errorf("second DID lookup = %+v, want verified DID and handle", got)
	}
	if count := requests.Load(); count != 2 {
		t.Errorf("PLC DID requests = %d, want 2", count)
	}
}

func TestUncachedResolverRejectsWellKnownHostsWithoutPrivateHatch(t *testing.T) {
	plc := newCountingPLC(t)
	config := Config{PLCURL: plc.server.URL}
	WithWellKnownHosts(map[string]string{".pds2.test": plc.server.Listener.Addr().String()})(&config)
	defer func() {
		if recover() == nil {
			t.Error("NewUncachedResolver did not panic for well-known hosts without private-hosts hatch")
		}
	}()
	NewUncachedResolver(config)
}

func TestUncachedResolverHandBuiltConfigGuardsLoopback(t *testing.T) {
	plc := newCountingPLC(t)
	resolver := NewUncachedResolver(Config{PLCURL: plc.server.URL})
	if resolver == nil {
		t.Fatal("NewUncachedResolver returned nil")
	}
	if _, err := resolver.Resolve(context.Background(), testDID); err == nil {
		t.Error("uncached resolver reached loopback without private-hosts hatch, want error")
	}
	if got := plc.requests.Load(); got != 0 {
		t.Errorf("PLC listener received %d requests, want 0", got)
	}
}
