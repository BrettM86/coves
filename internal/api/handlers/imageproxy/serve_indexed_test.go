package imageproxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/atproto/identity"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
)

type indexedPair struct{ ownerDID, cid string }

type indexedReferences struct {
	active map[indexedPair]bool
	err    error
}

func (references *indexedReferences) HasActiveReference(_ context.Context, ownerDID, cid string) (bool, error) {
	if references.err != nil {
		return false, references.err
	}
	return references.active[indexedPair{ownerDID, cid}], nil
}

func requestIndexedImage(handler *Handler, method, requestedCID, ifNoneMatch string) *httptest.ResponseRecorder {
	request := createTestRequest(method, "/img/content_full/plain/"+validTestDID+"/"+requestedCID, map[string]string{
		"preset": "content_full", "did": validTestDID, "cid": requestedCID,
	})
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}
	response := httptest.NewRecorder()
	handler.HandleImage(response, request)
	return response
}

func TestHandler_UnreferencedImageReturnsUncacheable404(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			source := formatGIF(t, true)
			requestedCID := rawCIDForHandlerFixture(t, source)
			references := &indexedReferences{active: map[indexedPair]bool{}}
			handler := newFormatServingHandler(t, t.TempDir(), formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
				return source, nil
			}), references)

			response := requestIndexedImage(handler, method, requestedCID, "")
			if response.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", response.Code)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestHandler_ReferenceLookupFailureReturns503(t *testing.T) {
	for _, test := range []struct {
		name        string
		warm        bool
		conditional bool
	}{
		{name: "cold cache GET"},
		{name: "warm cache GET", warm: true},
		{name: "matching If-None-Match GET", warm: true, conditional: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := formatGIF(t, true)
			requestedCID := rawCIDForHandlerFixture(t, source)
			references := &indexedReferences{active: map[indexedPair]bool{{validTestDID, requestedCID}: true}}
			handler := newFormatServingHandler(t, t.TempDir(), formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
				return source, nil
			}), references)
			var etag string
			if test.warm {
				served := requestIndexedImage(handler, http.MethodGet, requestedCID, "")
				if served.Code != http.StatusOK {
					t.Fatalf("initial status = %d, want 200", served.Code)
				}
				etag = served.Header().Get("ETag")
				if etag == "" {
					t.Fatal("initial response missing ETag")
				}
			}
			references.err = errors.New("reference storage unavailable")
			if !test.conditional {
				etag = ""
			}
			response := requestIndexedImage(handler, http.MethodGet, requestedCID, etag)
			if response.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", response.Code)
			}
		})
	}
}

func TestHandler_MatchingETagRechecksActiveReference(t *testing.T) {
	for _, test := range []struct {
		name, method string
		remove       bool
	}{
		{name: "GET unreferenced", method: http.MethodGet, remove: true},
		{name: "HEAD unreferenced", method: http.MethodHead, remove: true},
		{name: "GET referenced guard", method: http.MethodGet},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := formatGIF(t, true)
			requestedCID := rawCIDForHandlerFixture(t, source)
			pair := indexedPair{validTestDID, requestedCID}
			references := &indexedReferences{active: map[indexedPair]bool{pair: true}}
			fetchCalls := 0
			handler := newFormatServingHandler(t, t.TempDir(), formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
				fetchCalls++
				return source, nil
			}), references)
			served := requestIndexedImage(handler, http.MethodGet, requestedCID, "")
			if served.Code != http.StatusOK {
				t.Fatalf("initial status = %d, want 200", served.Code)
			}
			etag := served.Header().Get("ETag")
			if etag == "" {
				t.Fatal("initial response missing ETag")
			}
			if test.remove {
				delete(references.active, pair)
			}
			resolverCalls := 0
			resolver := resolverForPDS("https://pds.example.com")
			resolveDID := resolver.resolveDIDFunc
			resolver.resolveDIDFunc = func(ctx context.Context, did string) (*identity.DIDDocument, error) {
				resolverCalls++
				return resolveDID(ctx, did)
			}
			handler.identityResolver = resolver
			fetchCalls = 0
			response := requestIndexedImage(handler, test.method, requestedCID, etag)
			want := http.StatusNotFound
			if !test.remove {
				want = http.StatusNotModified
			}
			if response.Code != want {
				t.Errorf("status = %d, want %d", response.Code, want)
			}
			if !test.remove {
				if fetchCalls != 0 || resolverCalls != 0 {
					t.Errorf("conditional fetch calls = %d, resolver calls = %d; want neither contacted", fetchCalls, resolverCalls)
				}
			}
		})
	}
}

func TestHandler_Base58CIDUsesCanonicalActiveReference(t *testing.T) {
	source := formatGIF(t, true)
	canonicalCID := rawCIDForHandlerFixture(t, source)
	parsed, err := cid.Parse(canonicalCID)
	if err != nil {
		t.Fatalf("parse fixture CID: %v", err)
	}
	base58CID, err := parsed.StringOfBase(multibase.Base58BTC)
	if err != nil {
		t.Fatalf("encode fixture CID: %v", err)
	}
	references := &indexedReferences{active: map[indexedPair]bool{{validTestDID, canonicalCID}: true}}
	handler := newFormatServingHandler(t, t.TempDir(), formatFetcher(func(context.Context, string, string, string) ([]byte, error) {
		return source, nil
	}), references)

	response := requestIndexedImage(handler, http.MethodGet, base58CID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if !bytes.Equal(response.Body.Bytes(), source) {
		t.Errorf("served bytes differ from source image")
	}
}
