package jetstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

type didVerificationTransport func(*http.Request) (*http.Response, error)

func (f didVerificationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func didVerificationResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func didVerificationConsumer(transport http.RoundTripper) *CommunityEventConsumer {
	return NewCommunityEventConsumer(nil, "did:web:coves.social", false, nil,
		withWellKnownHTTPClient(&http.Client{Transport: transport}))
}

func TestVerifyHostedByClaim_ClassifiesDIDDocumentAnswers(t *testing.T) {
	const (
		did    = "did:web:example.com"
		handle = "c-testing.example.com"
	)

	tests := []struct {
		name       string
		response   *http.Response
		transport  error
		permanent  bool
		unresolved bool
	}{
		{
			name:     "valid document",
			response: didVerificationResponse(http.StatusOK, `{"id":"did:web:example.com","alsoKnownAs":["at://example.com"]}`),
		},
		{
			name:      "document identifies another DID",
			response:  didVerificationResponse(http.StatusOK, `{"id":"did:web:attacker.example","alsoKnownAs":["at://example.com"]}`),
			permanent: true,
		},
		{
			name:      "document does not claim the handle domain",
			response:  didVerificationResponse(http.StatusOK, `{"id":"did:web:example.com","alsoKnownAs":["at://elsewhere.example"]}`),
			permanent: true,
		},
		{
			name:       "host answers unavailable",
			response:   didVerificationResponse(http.StatusServiceUnavailable, `{"error":"unavailable"}`),
			unresolved: true,
		},
		{
			name:       "host cannot be reached",
			transport:  errors.New("dial failed"),
			unresolved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			consumer := didVerificationConsumer(didVerificationTransport(func(*http.Request) (*http.Response, error) {
				return tt.response, tt.transport
			}))

			err := consumer.verifyHostedByClaim(context.Background(), handle, did)
			if tt.permanent || tt.unresolved {
				if err == nil {
					t.Fatal("verification succeeded, want a classified refusal")
				}
			} else if err != nil {
				t.Fatalf("valid DID document was refused: %v", err)
			}
			if got := errors.Is(err, ErrPermanentEvent); got != tt.permanent {
				t.Errorf("errors.Is(ErrPermanentEvent)=%v, want %v: %v", got, tt.permanent, err)
			}
			if got := errors.Is(err, ErrUnresolvedReference); got != tt.unresolved {
				t.Errorf("errors.Is(ErrUnresolvedReference)=%v, want %v: %v", got, tt.unresolved, err)
			}
		})
	}
}

func TestVerifyDIDDocument_CacheIsScopedToDIDAndHandleDomain(t *testing.T) {
	const did = "did:web:claimed.example"
	var requests atomic.Int64

	consumer := didVerificationConsumer(didVerificationTransport(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		switch req.URL.Host {
		case "first.example":
			return didVerificationResponse(http.StatusOK,
				`{"id":"did:web:somebody-else.example","alsoKnownAs":["at://first.example"]}`), nil
		case "second.example":
			return didVerificationResponse(http.StatusOK,
				`{"id":"did:web:claimed.example","alsoKnownAs":["at://second.example"]}`), nil
		default:
			return nil, errors.New("unexpected host " + req.URL.Host)
		}
	}))

	if err := consumer.verifyDIDDocument(context.Background(), did, "first.example", "first.example"); err == nil {
		t.Fatal("mismatched DID document unexpectedly verified")
	}
	if err := consumer.verifyDIDDocument(context.Background(), did, "second.example", "second.example"); err != nil {
		t.Fatalf("a failure for another handle domain poisoned this valid verification: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("got %d DID-document requests, want 2 distinct cache keys", got)
	}
}

func TestVerifyDIDDocument_CachedFailureKeepsItsClassificationAndDefersAttempts(t *testing.T) {
	const (
		did    = "did:web:example.com"
		handle = "c-testing.example.com"
	)

	t.Run("permanent", func(t *testing.T) {
		var requests atomic.Int64
		consumer := didVerificationConsumer(didVerificationTransport(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return didVerificationResponse(http.StatusOK,
				`{"id":"did:web:another.example","alsoKnownAs":["at://example.com"]}`), nil
		}))

		for i := 0; i < 2; i++ {
			err := consumer.verifyHostedByClaim(context.Background(), handle, did)
			if !errors.Is(err, ErrPermanentEvent) {
				t.Fatalf("call %d lost the permanent classification: %v", i+1, err)
			}
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("permanent result was fetched %d times, want 1", got)
		}
	})

	t.Run("unresolved", func(t *testing.T) {
		var requests atomic.Int64
		consumer := didVerificationConsumer(didVerificationTransport(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return didVerificationResponse(http.StatusServiceUnavailable, "unavailable"), nil
		}))

		first := consumer.verifyHostedByClaim(context.Background(), handle, did)
		if !errors.Is(first, ErrUnresolvedReference) || errors.Is(first, errRedriveDeferred) {
			t.Fatalf("fresh failed fetch must be unresolved new information: %v", first)
		}
		second := consumer.verifyHostedByClaim(context.Background(), handle, did)
		if !errors.Is(second, ErrUnresolvedReference) || !errors.Is(second, errRedriveDeferred) {
			t.Fatalf("cached unresolved result must defer rather than consume an attempt: %v", second)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("cached unresolved result fetched %d times, want 1", got)
		}
	})
}

func TestVerifyDIDDocument_InFlightRequestSuppressesDuplicateFetch(t *testing.T) {
	const (
		did    = "did:web:example.com"
		domain = "example.com"
		handle = "c-testing.example.com"
	)

	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int64
	consumer := didVerificationConsumer(didVerificationTransport(func(*http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		return didVerificationResponse(http.StatusOK,
			`{"id":"did:web:example.com","alsoKnownAs":["at://example.com"]}`), nil
	}))

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- consumer.verifyDIDDocument(context.Background(), did, domain, handle)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first verification never reached the HTTP transport")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- consumer.verifyHostedByClaim(context.Background(), handle, did)
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrUnresolvedReference) || !errors.Is(err, errRedriveDeferred) {
			t.Fatalf("duplicate in-flight verification returned the wrong classification: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate verification reached the blocked transport instead of using the in-flight marker")
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first verification failed after the transport was released: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("in-flight verification made %d requests, want 1", got)
	}
}

func TestVerifyDIDDocument_RestampsMarkerAfterLimiterWait(t *testing.T) {
	const (
		did    = "did:web:example.com"
		domain = "example.com"
		handle = "c-testing.example.com"
	)
	key := didVerificationCacheKey{did: did, handleDomain: domain}

	started := make(chan struct{})
	release := make(chan struct{})
	consumer := didVerificationConsumer(didVerificationTransport(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return didVerificationResponse(http.StatusOK,
			`{"id":"did:web:example.com","alsoKnownAs":["at://example.com"]}`), nil
	}))
	consumer.wellKnownLimiter = rate.NewLimiter(rate.Every(250*time.Millisecond), 1)
	if !consumer.wellKnownLimiter.Allow() {
		t.Fatal("failed to consume the limiter's initial token")
	}

	done := make(chan error, 1)
	go func() {
		done <- consumer.verifyDIDDocument(context.Background(), did, domain, handle)
	}()
	waitForDIDVerificationCacheEntry(t, consumer, key)
	consumer.cacheVerificationResult(key, didVerificationInFlight, "stale test marker", 5*time.Millisecond)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("verification never passed the limiter")
	}
	assertFreshInFlightMarker(t, consumer, key)

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("verification failed after the transport was released: %v", err)
	}
}

func TestVerifyDIDDocument_RestampsMarkerWhenLimiterWaitIsCanceled(t *testing.T) {
	const (
		did    = "did:web:example.com"
		domain = "example.com"
		handle = "c-testing.example.com"
	)
	key := didVerificationCacheKey{did: did, handleDomain: domain}

	var requests atomic.Int64
	consumer := didVerificationConsumer(didVerificationTransport(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("transport must not be reached")
	}))
	consumer.wellKnownLimiter = rate.NewLimiter(rate.Every(time.Second), 1)
	if !consumer.wellKnownLimiter.Allow() {
		t.Fatal("failed to consume the limiter's initial token")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- consumer.verifyDIDDocument(ctx, did, domain, handle)
	}()
	waitForDIDVerificationCacheEntry(t, consumer, key)
	consumer.cacheVerificationResult(key, didVerificationInFlight, "stale test marker", 5*time.Millisecond)
	cancel()

	if err := <-done; err == nil {
		t.Fatal("canceled limiter wait unexpectedly verified the DID document")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("canceled limiter wait made %d transport requests, want 0", got)
	}
	assertFreshInFlightMarker(t, consumer, key)
}

func assertFreshInFlightMarker(t *testing.T, consumer *CommunityEventConsumer, key didVerificationCacheKey) {
	t.Helper()
	consumer.didCacheMu.Lock()
	cached, ok := consumer.didCache.Get(key)
	consumer.didCacheMu.Unlock()
	if !ok {
		t.Fatal("verification cache marker is missing")
	}
	if cached.outcome != didVerificationInFlight {
		t.Fatalf("cached outcome=%d, want in-flight", cached.outcome)
	}
	if wantAfter := time.Now().Add(wellKnownVerificationTimeout / 2); !cached.expiresAt.After(wantAfter) {
		t.Fatalf("in-flight marker expires at %s, want it re-stamped past %s", cached.expiresAt, wantAfter)
	}
}

func waitForDIDVerificationCacheEntry(t *testing.T, consumer *CommunityEventConsumer, key didVerificationCacheKey) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		consumer.didCacheMu.Lock()
		_, ok := consumer.didCache.Get(key)
		consumer.didCacheMu.Unlock()
		if ok {
			return
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("verification never installed its in-flight cache marker")
		}
	}
}

func TestWellKnownTimeout_RemainsFiveSeconds(t *testing.T) {
	if wellKnownTimeout != 5*time.Second {
		t.Fatalf("wellKnownTimeout=%s, want literal 5s security budget", wellKnownTimeout)
	}
}
