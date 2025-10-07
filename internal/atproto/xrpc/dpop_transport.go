package xrpc

import (
	"fmt"
	"net/http"
	"sync"

	"Coves/internal/atproto/oauth"
	oauthCore "Coves/internal/core/oauth"

	"github.com/lestrrat-go/jwx/v2/jwk"
)

// DPoPTransport is an http.RoundTripper that automatically adds DPoP proofs to requests
// It intercepts HTTP requests and:
// 1. Adds Authorization: DPoP <access_token>
// 2. Creates and adds DPoP proof JWT
// 3. Handles nonce rotation (retries on 401 with new nonce)
// 4. Updates nonces in session store
type DPoPTransport struct {
	base            http.RoundTripper      // Underlying transport (usually http.DefaultTransport)
	session         *oauthCore.OAuthSession // User's OAuth session
	sessionStore    oauthCore.SessionStore  // For updating nonces
	dpopKey         jwk.Key                 // Parsed DPoP private key
	mu              sync.Mutex              // Protects nonce updates
}

// NewDPoPTransport creates a new DPoP-enabled HTTP transport
func NewDPoPTransport(base http.RoundTripper, session *oauthCore.OAuthSession, sessionStore oauthCore.SessionStore) (*DPoPTransport, error) {
	if base == nil {
		base = http.DefaultTransport
	}

	// Parse DPoP private key from session
	dpopKey, err := oauth.ParseJWKFromJSON([]byte(session.DPoPPrivateJWK))
	if err != nil {
		return nil, fmt.Errorf("failed to parse DPoP key: %w", err)
	}

	return &DPoPTransport{
		base:         base,
		session:      session,
		sessionStore: sessionStore,
		dpopKey:      dpopKey,
	}, nil
}

// RoundTrip implements http.RoundTripper
// This is called for every HTTP request made by the client
func (t *DPoPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request (don't modify original)
	req = req.Clone(req.Context())

	// Add Authorization header with DPoP-bound access token
	req.Header.Set("Authorization", "DPoP "+t.session.AccessToken)

	// Determine which nonce to use based on the target URL
	nonce := t.getDPoPNonce(req.URL.String())

	// Create DPoP proof for this specific request
	dpopProof, err := oauth.CreateDPoPProof(
		t.dpopKey,
		req.Method,
		req.URL.String(),
		nonce,
		t.session.AccessToken,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create DPoP proof: %w", err)
	}

	// Add DPoP proof header
	req.Header.Set("DPoP", dpopProof)

	// Execute the request
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// Handle DPoP nonce rotation
	if resp.StatusCode == http.StatusUnauthorized {
		// Check if server provided a new nonce
		newNonce := resp.Header.Get("DPoP-Nonce")
		if newNonce != "" {
			// Update nonce and retry request once
			t.updateDPoPNonce(req.URL.String(), newNonce)

			// Close the 401 response body
			_ = resp.Body.Close()

			// Retry with new nonce
			return t.retryWithNewNonce(req, newNonce)
		}
	}

	// Check for nonce update even on successful responses
	if newNonce := resp.Header.Get("DPoP-Nonce"); newNonce != "" {
		t.updateDPoPNonce(req.URL.String(), newNonce)
	}

	return resp, nil
}

// getDPoPNonce determines which DPoP nonce to use for a given URL
func (t *DPoPTransport) getDPoPNonce(url string) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	// If URL is to the PDS, use PDS nonce
	if contains(url, t.session.PDSURL) {
		return t.session.DPoPPDSNonce
	}

	// If URL is to auth server, use auth server nonce
	if contains(url, t.session.AuthServerIss) {
		return t.session.DPoPAuthServerNonce
	}

	// Default: no nonce (first request to this server)
	return ""
}

// updateDPoPNonce updates the appropriate nonce based on URL
func (t *DPoPTransport) updateDPoPNonce(url, newNonce string) {
	t.mu.Lock()

	// Read DID inside lock to avoid race condition
	did := t.session.DID

	// Update PDS nonce
	if contains(url, t.session.PDSURL) {
		t.session.DPoPPDSNonce = newNonce
		t.mu.Unlock()
		// Persist to database (async, best-effort)
		go func() {
			_ = t.sessionStore.UpdatePDSNonce(did, newNonce)
		}()
		return
	}

	// Update auth server nonce
	if contains(url, t.session.AuthServerIss) {
		t.session.DPoPAuthServerNonce = newNonce
		t.mu.Unlock()
		// Persist to database (async, best-effort)
		go func() {
			_ = t.sessionStore.UpdateAuthServerNonce(did, newNonce)
		}()
		return
	}

	t.mu.Unlock()
}

// retryWithNewNonce retries a request with an updated DPoP nonce
func (t *DPoPTransport) retryWithNewNonce(req *http.Request, newNonce string) (*http.Response, error) {
	// Create new DPoP proof with updated nonce
	dpopProof, err := oauth.CreateDPoPProof(
		t.dpopKey,
		req.Method,
		req.URL.String(),
		newNonce,
		t.session.AccessToken,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create DPoP proof on retry: %w", err)
	}

	// Update DPoP header
	req.Header.Set("DPoP", dpopProof)

	// Retry the request (only once - no infinite loops)
	return t.base.RoundTrip(req)
}

// contains checks if haystack contains needle (case-sensitive)
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && haystack[:len(needle)] == needle ||
		len(haystack) > len(needle) && haystack[len(haystack)-len(needle):] == needle
}

// AuthenticatedClient creates an HTTP client with DPoP transport
// This is what handlers use to make authenticated requests to the user's PDS
func NewAuthenticatedClient(session *oauthCore.OAuthSession, sessionStore oauthCore.SessionStore) (*http.Client, error) {
	transport, err := NewDPoPTransport(nil, session, sessionStore)
	if err != nil {
		return nil, fmt.Errorf("failed to create DPoP transport: %w", err)
	}

	return &http.Client{
		Transport: transport,
	}, nil
}
