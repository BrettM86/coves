package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestUncachedResolverDeclaredIdentityMakesNoHandleRequest pins the document
// read that lets a caller reject an identity before its handle is verified: it
// returns what the document declares and never contacts the handle domain, so a
// handle domain that hangs costs nothing.
func TestUncachedResolverDeclaredIdentityMakesNoHandleRequest(t *testing.T) {
	const did = "did:plc:declaredonlyb12aaaaaaaaa"
	const handle = "alice.declaredonly.test"
	var handleRequests atomic.Int64
	handleDomain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handleRequests.Add(1)
	}))
	t.Cleanup(handleDomain.Close)
	var plcRequests atomic.Int64
	plc := declaringPLC(t, did, handle, "https://pds.declaredonly.test", func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/"+did {
			plcRequests.Add(1)
		}
		return false
	})
	config := DefaultConfig(WithPrivateHostsAllowed(), WithWellKnownHosts(map[string]string{
		".declaredonly.test": strings.TrimPrefix(handleDomain.URL, "http://"),
	}))
	config.PLCURL = plc.URL
	resolver := NewUncachedResolver(config)

	got, err := resolver.ResolveDeclaredIdentity(context.Background(), did)
	if err != nil {
		t.Fatalf("ResolveDeclaredIdentity: %v", err)
	}
	want := DeclaredIdentity{DID: did, DeclaredHandle: handle, PDSURL: "https://pds.declaredonly.test"}
	if got == nil || *got != want {
		t.Errorf("ResolveDeclaredIdentity = %+v, want %+v", got, want)
	}
	if n := plcRequests.Load(); n != 1 {
		t.Errorf("PLC DID requests = %d, want 1", n)
	}
	if n := handleRequests.Load(); n != 0 {
		t.Errorf("handle domain requests = %d, want 0", n)
	}
}

func TestUncachedResolverDeclaredIdentityErrors(t *testing.T) {
	plc := declaringPLC(t, "did:plc:declaredonlyb12present", "alice.declaredonly.test", "https://pds.declaredonly.test")
	config := DefaultConfig(WithPrivateHostsAllowed())
	config.PLCURL = plc.URL
	resolver := NewUncachedResolver(config)

	_, err := resolver.ResolveDeclaredIdentity(context.Background(), "not-a-did")
	var invalid *ErrInvalidIdentifier
	if !errors.As(err, &invalid) {
		t.Errorf("malformed DID error = %v, want ErrInvalidIdentifier", err)
	}
	_, err = resolver.ResolveDeclaredIdentity(context.Background(), "did:plc:declaredonlyb12missing")
	var notFound *ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("absent DID error = %v, want ErrNotFound", err)
	}
}

func TestUncachedResolverDeclaredIdentityWithoutHandle(t *testing.T) {
	plc := newCountingPLC(t)
	config := DefaultConfig(WithPrivateHostsAllowed())
	config.PLCURL = plc.server.URL
	got, err := NewUncachedResolver(config).ResolveDeclaredIdentity(context.Background(), testDID)
	if err != nil {
		t.Fatalf("ResolveDeclaredIdentity: %v", err)
	}
	if got == nil || got.DID != testDID || got.DeclaredHandle != "" || got.PDSURL != "https://pds.example.invalid" {
		t.Errorf("ResolveDeclaredIdentity = %+v, want the DID and PDS with no declared handle", got)
	}
}

func TestUncachedResolverDeclaredIdentityGuardsLoopback(t *testing.T) {
	plc := newCountingPLC(t)
	_, err := NewUncachedResolver(Config{PLCURL: plc.server.URL}).ResolveDeclaredIdentity(context.Background(), testDID)
	var failed *ErrResolutionFailed
	if !errors.As(err, &failed) {
		t.Errorf("loopback PLC without the private-hosts hatch: error = %v, want ErrResolutionFailed", err)
	}
	if n := plc.requests.Load(); n != 0 {
		t.Errorf("PLC listener received %d requests, want 0", n)
	}
}
