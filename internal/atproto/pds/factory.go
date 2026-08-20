package pds

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	covesoauth "Coves/internal/atproto/oauth"

	atprotoapi "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// NewFromOAuthSession creates a PDS client from an OAuth session.
// This uses DPoP authentication - the correct method for OAuth tokens.
//
// The oauthClient is used to resume the session and get a properly configured
// APIClient that handles DPoP proof generation and nonce rotation automatically.
//
// # IT TAKES NO ClientOption, AND THE REASON IS A CHAIN THIS PACKAGE DOES NOT OWN
//
// The other two constructors build their own HTTP client (see
// newBearerHTTPClient). This one never touches the field: the client arrives
// already installed, copied twice by indigo from the ClientApp the caller hands
// in. Re-read at v0.0.0-20260202181658-ea3d39eec464, the version go.mod pins:
//
//	link 1  ours    internal/atproto/oauth, in NewOAuthClient
//	                clientApp.Client = NewSSRFSafeHTTPClient(PrivateAddressOptions(config.AllowPrivateIPs)...)
//	link 2  indigo  atproto/auth/oauth/oauth.go:200, in ResumeSession
//	                sess := ClientSession{Client: app.Client, ...}
//	link 3  indigo  atproto/auth/oauth/session.go:403, in ClientSession.APIClient
//	                c := atclient.APIClient{Client: sess.Client, ...}
//
// So the OAuth path IS guarded today, and the guard is also correctly gated —
// AllowPrivateIPs at link 1 is the same dev switch PrivateHostOptions carries
// here.
//
// # WHAT THE CHAIN DEGRADES TO, WHICH IS THE PART WORTH KNOWING
//
// Link 1 is an OVERRIDE, not an initialisation: indigo's NewClientApp sets
// `Client: http.DefaultClient` at oauth.go:55. Deleting or skipping our line
// therefore does not produce a nil client that fails loudly — it produces the
// unguarded, un-timed stdlib default, and every OAuth-authenticated PDS call in
// the AppView reverts silently.
//
// Links 2 and 3 are indigo's and can change in a dependency bump;
// factory_guard_test.go fences both. Link 1 is ours and is NOT fenced anywhere
// today — nothing in this tree asserts that clientApp.Client is guarded, so
// deleting that line fails no test. Fencing it means standing up oauth.NewClient
// with a config, which belongs in internal/atproto/oauth's own tests. That is a
// known gap, recorded here rather than assumed away.
func NewFromOAuthSession(ctx context.Context, oauthClient *oauth.ClientApp, sessionData *oauth.ClientSessionData) (Client, error) {
	if oauthClient == nil {
		return nil, fmt.Errorf("oauthClient is required")
	}
	if sessionData == nil {
		return nil, fmt.Errorf("sessionData is required")
	}

	// ResumeSession reconstructs the OAuth session with DPoP key
	// and returns a ClientSession that can generate authenticated requests.
	// Common failure modes:
	// - Expired access/refresh tokens → User needs to re-authenticate
	// - Session revoked on PDS → User needs to re-authenticate
	// - DPoP nonce mismatch → Retry may help (transient)
	// - DPoP key mismatch → Session data corrupted, re-authenticate
	sess, err := oauthClient.ResumeSession(ctx, sessionData.AccountDID, sessionData.SessionID)
	if err != nil {
		return nil, classifyResumeFailure(err,
			sessionData.AccountDID.String(), sessionData.SessionID)
	}

	// APIClient() returns an *atclient.APIClient configured with DPoP auth
	apiClient := sess.APIClient()

	return &client{
		apiClient: apiClient,
		did:       sessionData.AccountDID.String(),
		host:      sessionData.HostURL,
	}, nil
}

// classifyResumeFailure decides whether a failed session resume means the user
// must sign in again.
//
// Tag ONLY a session that is genuinely gone. ResumeSession is a session-store
// read and nothing more, so its failures split two ways: the row is absent or
// past its expiry (terminal — signing in again is the fix), or the store itself
// failed (a database outage, an exhausted pool, a cancelled request).
//
// The distinction has to be made here because the API boundary checks
// re-authentication ahead of every other rule, so anything tagged expired
// answers 401. Tagging the whole class would turn a few seconds of database
// trouble into a sign-out for every user with a request in flight, and would
// hide the outage from 5xx alerting at the same time.
//
// Either way the cause stays wrapped, so it reaches the logs and — for a
// cancelled or timed-out request — still matches the boundary's lifecycle rules.
func classifyResumeFailure(err error, did, sessionID string) error {
	if errors.Is(err, covesoauth.ErrSessionNotFound) {
		return fmt.Errorf("failed to resume OAuth session for DID=%s, sessionID=%s: %w: %w",
			did, sessionID, ErrSessionExpired, err)
	}
	return fmt.Errorf("failed to resume OAuth session for DID=%s, sessionID=%s: %w",
		did, sessionID, err)
}

// NewFromPasswordAuth creates a PDS client using password authentication.
// This uses Bearer token authentication from com.atproto.server.createSession.
//
// Primarily used for:
// - E2E tests with local PDS
// - Development/debugging tools
// - Non-OAuth clients
//
// Note: This establishes a new session with the PDS. For repeated calls,
// consider using NewFromAccessToken if you already have a valid access token.
func NewFromPasswordAuth(ctx context.Context, host, handle, password string, opts ...ClientOption) (Client, error) {
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if handle == "" {
		return nil, fmt.Errorf("handle is required")
	}
	if password == "" {
		return nil, fmt.Errorf("password is required")
	}

	// Build the API client before createSession so the password-bearing login
	// request uses the same guarded transport as every request after it. Indigo's
	// LoginWithPasswordHost cannot be used here because it creates and uses an
	// http.DefaultClient internally before returning the APIClient to its caller.
	// coves:allow-bare-client: NewAPIClient installs http.DefaultClient (indigo atclient/apiclient.go:43); the next line replaces it before any request is made
	apiClient := atclient.NewAPIClient(host)
	apiClient.Client = newBearerHTTPClient(opts...)

	session, err := atprotoapi.ServerCreateSession(ctx, apiClient, &atprotoapi.ServerCreateSession_Input{
		Identifier: handle,
		Password:   password,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to login with password: %w", err)
	}

	did, err := syntax.ParseDID(session.Did)
	if err != nil {
		return nil, fmt.Errorf("parsing DID returned by password login: %w", err)
	}
	apiClient.Auth = &atclient.PasswordAuth{Session: atclient.PasswordSessionData{
		AccessToken:  session.AccessJwt,
		RefreshToken: session.RefreshJwt,
		AccountDID:   did,
		Host:         host,
	}}
	apiClient.AccountDID = &did

	return &client{
		apiClient: apiClient,
		did:       did.String(),
		host:      host,
	}, nil
}

// NewFromAccessToken creates a PDS client from an existing access token.
// This is useful when you already have a valid Bearer token (e.g., from createSession)
// and don't want to re-authenticate.
//
// WARNING: This creates a client with Bearer auth only. Do NOT use this with
// OAuth access tokens - those require DPoP proofs. Use NewFromOAuthSession instead.
func NewFromAccessToken(host, did, accessToken string, opts ...ClientOption) (Client, error) {
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if did == "" {
		return nil, fmt.Errorf("did is required")
	}
	if accessToken == "" {
		return nil, fmt.Errorf("accessToken is required")
	}

	// Create APIClient with Bearer auth
	// coves:allow-bare-client: NewAPIClient installs http.DefaultClient (indigo atclient/apiclient.go:43); the next line replaces it before any request is made
	apiClient := atclient.NewAPIClient(host)
	apiClient.Client = newBearerHTTPClient(opts...)
	apiClient.Auth = &bearerAuth{token: accessToken}

	return &client{
		apiClient: apiClient,
		did:       did,
		host:      host,
	}, nil
}

// bearerRequestTimeout bounds a single PDS request made through a Bearer-authed
// client.
//
// The value it replaces is http.DefaultClient's, which is ZERO — and zero in
// net/http means "wait forever". posts/service.go reaches this constructor on a
// request path, so a PDS that accepts a connection and then stops answering used
// to hold that goroutine for the life of the process. 30s matches the longest
// deadline anything else in this tree allows a PDS (blobs' upload POST), because
// this client carries record writes whose size is not bounded as tightly as a
// getRecord's.
const bearerRequestTimeout = 30 * time.Second

// bearerClientConfig is what the ClientOptions accumulate into.
type bearerClientConfig struct {
	// allowPrivateHosts opens the SSRF hatch. NEVER set in production.
	allowPrivateHosts bool

	// transportOptions is the TEST SEAM, and it is unexported deliberately: the
	// resolver seam these tests need must not be reachable from any non-test
	// package, which is the rule the new audit category enforces.
	transportOptions []covesoauth.Option
}

// ClientOption configures a Bearer-authed PDS client.
type ClientOption func(*bearerClientConfig)

// WithPrivateHostsAllowed disables the SSRF address guard on a Bearer-authed
// PDS client.
//
// THE NAME IS THE CONTRACT: production must not call this. The hosts these
// constructors dial are `community.PDSURL` — a per-community database field — and
// the AppView shares a network with its Postgres, its PDS, its Jetstream and a
// cloud metadata endpoint. Tests that drive a PDS on loopback pass it because
// loopback is exactly what the guard refuses, and a local dev stack runs its PDS
// on the developer's own machine.
func WithPrivateHostsAllowed() ClientOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(c *bearerClientConfig) { c.allowPrivateHosts = true }
}

// withTransportOptions is the test seam, unexported so production cannot reach
// it. See bearerClientConfig.transportOptions.
func withTransportOptions(opts ...covesoauth.Option) ClientOption {
	return func(c *bearerClientConfig) { c.transportOptions = append(c.transportOptions, opts...) }
}

// PrivateHostOptions returns the options a caller holding an allow-private
// boolean should pass to these constructors: the hatch when it is set, and
// NOTHING when it is not.
//
// It mirrors oauth.PrivateAddressOptions and the same helper in imageproxy,
// blobs, unfurl and jetstream, and it is a function rather than an `if` at the
// call site for the reason documented there: `.env.ci:140` sets IS_DEV_ENV=true,
// so `make ci` takes the PERMISSIVE branch at every call site holding such a
// boolean. A unit test against this function is the only place in the repository
// where the branch production actually runs is ever evaluated.
//
// FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT — not "options that are
// safe", but none, so that what production gets is exactly the constructor's own
// defaults.
func PrivateHostOptions(allowPrivate bool) []ClientOption {
	if !allowPrivate {
		return nil
	}
	return []ClientOption{WithPrivateHostsAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

// newBearerHTTPClient builds the HTTP client the Bearer-authed PDS constructors
// install on their APIClient.
//
// # WHAT IT FIXES
//
// atclient.NewAPIClient leaves APIClient.Client as http.DefaultClient, and
// atclient's apiclient.go substitutes http.DefaultClient again when the field is
// nil — so the unguarded, un-timed default was reached two ways. The field is
// public and documented as customisable, so this is an assignment and not a
// wrapper.
//
// # WHAT THE ADDRESS GUARD IS FOR HERE
//
// The host these constructors dial is `community.PDSURL` — a per-community
// database column, written when a community is created or federated in, and read
// by posts.(*postService).deleteCommunityPost, posts.NewCommunityRepoFactory and
// cmd/rematerialize-posts's communityRepoOpener. deleteCommunityPost reaches this
// constructor on a REQUEST path, so the trigger is an ordinary API call.
//
// This client is Bearer-authed, which makes it the worse half of the two. The
// Authorization header is on the wire before any response exists, so "the
// address was dialled" and "a live PDS credential left the process" are the same
// event. A refused DNS answer costs an attacker a retry; a leaked bearer token
// is not recoverable.
//
// # THE PREVIOUS COMMENT ARGUED THIS COULD NOT BE DONE, AND WAS WRONG
//
// It claimed NewFromAccessToken's shape was pinned by a named function type at
// tests/testkit/pds.go's PasswordAuthFactory, so a variadic option parameter
// would break ~15 integration files. The type is GENERIC over the constructor,
// so widening it carries the option through and every direct call site compiles
// unchanged. What was true in that comment is that the guard cannot be switched
// on without a hatch — every integration and E2E test in this tree drives a PDS
// on loopback through these constructors, which is the address class the guard
// refuses. That is what PrivateHostOptions is for.
//
// # THE TIMEOUT IS THIS SITE'S OWN
//
// bearerRequestTimeout is re-applied over the shared client's 15s ceiling.
// Inheriting the shared value would halve the allowance for every PDS write in
// the AppView as a silent side effect of an SSRF fix.
func newBearerHTTPClient(opts ...ClientOption) *http.Client {
	cfg := &bearerClientConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	client := covesoauth.NewSSRFSafeHTTPClient(
		append(covesoauth.PrivateAddressOptions(cfg.allowPrivateHosts), cfg.transportOptions...)...)
	client.Timeout = bearerRequestTimeout
	return client
}

// bearerAuth implements atclient.AuthMethod for simple Bearer token auth.
// This is used for password-based sessions where DPoP is not required.
type bearerAuth struct {
	token string
}

// Ensure bearerAuth implements atclient.AuthMethod.
var _ atclient.AuthMethod = (*bearerAuth)(nil)

// DoWithAuth adds the Bearer token to the request and executes it.
func (b *bearerAuth) DoWithAuth(c *http.Client, req *http.Request, _ syntax.NSID) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	return c.Do(req)
}
