package unfurl

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Failures a poster can cause per URL must not open a shared circuit.
//
// A 404 for a bogus video ID or repository path, or NXDOMAIN for a made-up
// subdomain, says nothing about whether the provider is up. The breaker keys
// oEmbed per provider domain and OpenGraph per registrable domain, so counting
// those failures lets three posts disable previews of youtube.com or
// github.com for every user for openDuration.

// fixedResultTransport answers every request with the same status, or fails
// it with dialErr when set, without opening a socket.
type fixedResultTransport struct {
	status  int
	dialErr error
}

func (transport fixedResultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if transport.dialErr != nil {
		return nil, transport.dialErr
	}
	return &http.Response{
		StatusCode: transport.status,
		Status:     fmt.Sprintf("%d %s", transport.status, http.StatusText(transport.status)),
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader("error")),
		Request:    req,
	}, nil
}

func TestUnfurlService_PerURLFailuresDoNotOpenSharedCircuit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		transport   fixedResultTransport
		linkPattern string
		breakerKey  string
		wantOpen    bool
	}{
		{
			name:        "oEmbed 404s for bogus video IDs leave youtube.com closed",
			transport:   fixedResultTransport{status: http.StatusNotFound},
			linkPattern: "https://youtube.com/watch?v=bogus-%d",
			breakerKey:  "youtube.com",
			wantOpen:    false,
		},
		{
			name:        "oEmbed 5xx still opens youtube.com",
			transport:   fixedResultTransport{status: http.StatusServiceUnavailable},
			linkPattern: "https://youtube.com/watch?v=video-%d",
			breakerKey:  "youtube.com",
			wantOpen:    true,
		},
		{
			name:        "OpenGraph 404s for missing repositories leave github.com closed",
			transport:   fixedResultTransport{status: http.StatusNotFound},
			linkPattern: "https://github.com/no-such-repo-%d",
			breakerKey:  "opengraph:github.com",
			wantOpen:    false,
		},
		{
			name: "OpenGraph NXDOMAIN for made-up subdomains leaves nytimes.com closed",
			transport: fixedResultTransport{dialErr: &net.DNSError{
				Err:        "no such host",
				Name:       "zz.nytimes.com",
				IsNotFound: true,
			}},
			linkPattern: "https://zz%d.nytimes.com/",
			breakerKey:  "opengraph:nytimes.com",
			wantOpen:    false,
		},
		{
			name:        "OpenGraph 5xx still opens the site",
			transport:   fixedResultTransport{status: http.StatusBadGateway},
			linkPattern: "https://example.org/page-%d",
			breakerKey:  "opengraph:example.org",
			wantOpen:    true,
		},
		{
			name:        "OpenGraph connection refused still opens the site",
			transport:   fixedResultTransport{dialErr: &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connect: connection refused")}},
			linkPattern: "https://example.net/page-%d",
			breakerKey:  "opengraph:example.net",
			wantOpen:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := NewService(alwaysMissRepo{}, PrivateHostOptions(false)...).(*service)
			svc.httpClient.Transport = tt.transport
			ctx := context.Background()

			threshold := svc.circuitBreaker.failureThreshold
			for i := 0; i < threshold; i++ {
				_, err := svc.UnfurlURL(ctx, fmt.Sprintf(tt.linkPattern, i))
				require.Error(t, err, "request %d must fail", i)
			}

			canAttempt, err := svc.circuitBreaker.canAttempt(tt.breakerKey)
			if tt.wantOpen {
				assert.False(t, canAttempt, "circuit for %s must be open", tt.breakerKey)
				require.Error(t, err)
				return
			}
			assert.True(t, canAttempt, "circuit for %s must stay closed: %v", tt.breakerKey, err)
			assert.NoError(t, err)
		})
	}
}

// TestCircuitBreaker_SuccessPathKeysAreBounded: every successfully unfurled
// distinct host passes through canAttempt and recordSuccess, so the success
// path must not leave per-key entries behind either.
func TestCircuitBreaker_SuccessPathKeysAreBounded(t *testing.T) {
	t.Parallel()

	cb := newCircuitBreaker()
	for i := 0; i < 50000; i++ {
		key := fmt.Sprintf("healthy-%d.example", i)
		canAttempt, err := cb.canAttempt(key)
		require.True(t, canAttempt)
		require.NoError(t, err)
		cb.recordSuccess(key)
	}

	cb.mu.RLock()
	defer cb.mu.RUnlock()
	assert.LessOrEqual(t, len(cb.failures), maxTrackedKeys, "failures map")
	assert.LessOrEqual(t, len(cb.lastFailure), maxTrackedKeys, "lastFailure map")
	assert.LessOrEqual(t, len(cb.state), maxTrackedKeys, "state map")
	assert.LessOrEqual(t, len(cb.lastStateLog), maxTrackedKeys, "lastStateLog map")
	assert.LessOrEqual(t, len(cb.recencyElements), maxTrackedKeys, "recencyElements map")

	_, tracked := cb.state["healthy-0.example"]
	assert.False(t, tracked, "a key that never failed must leave no state entry")
}
