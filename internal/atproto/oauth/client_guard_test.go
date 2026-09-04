package oauth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The three clients NewOAuthClient installs on indigo's ClientApp, and the fact
// that all three are installed BY ASSIGNMENT over working defaults.
//
// # WHY ASSIGNMENT IS THE WHOLE PROBLEM
//
// indigo's NewClientApp returns an app that is already complete:
//
//	Client:          http.DefaultClient
//	Resolver.Client: an Indigo PublicOnlyTransport client
//	Dir:             identity.DefaultDirectory()
//
// (atproto/auth/oauth/oauth.go:53-57). Our constructor then overwrites all three. So
// the guard here is not a call that fails when it is missing — it is a
// REPLACEMENT, and deleting any assignment leaves a compiling, running, fully
// functional OAuth client with the guard silently gone. There is no nil to
// panic on and no zero value to notice. Before this file, deleting the Client
// assignment failed ZERO tests in this repository; that was established by
// executed mutation, not by reading.
//
// # WHAT IS ON THE OTHER SIDE OF THESE CLIENTS
//
// ClientApp.Client carries every OAuth-authenticated call this AppView makes to
// a user's PDS, ClientApp.Dir resolves the identity records that produce those
// hosts, and ClientApp.Resolver fetches OAuth metadata during StartAuthFlow. The
// last path is publicly reachable and accepts an https:// identifier directly.
//
// # WHY REACHABILITY, NOT ONLY THE ERROR
//
// Mutation testing produced a guard that classified correctly, emitted
// a byte-identical message, and refused the request AFTER delivering it. For a
// destination a stranger named, the packet leaving IS the SSRF. Every case below
// stands up a real listener and asserts its handler never ran.

// clientGuardTestDID is a syntactically valid did:plc that does not exist on the
// real network.
//
// NOT A REAL DID, DELIBERATELY. The mutation these tests exist to catch —
// deleting `clientApp.Dir = cacheDir` — hands resolution back to indigo's
// DefaultDirectory, which is pointed at the public plc.directory. A well-known
// DID would then RESOLVE, on any machine with egress, and a test asserting only
// "an error came back" would flip from failing to passing depending on whose
// laptop it ran on. The assertions below are about which listener was reached,
// which does not depend on that; this constant keeps the error path from
// depending on it either.
const clientGuardTestDID = "did:plc:abcdefghijklmnopqrstuvwx"

// countingHost is an HTTP listener that records whether anything reached it. It
// listens on loopback — the address class the guard exists to refuse — so its
// counter is the assertion.
type countingHost struct {
	server   *httptest.Server
	requests atomic.Int64
}

// newCountingPDS answers like a PDS: anything at all, since nothing below
// depends on the body.
func newCountingPDS(t *testing.T) *countingHost {
	t.Helper()
	return newCountingHost(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

// newCountingPLC answers a DID lookup with a document indigo will accept.
//
// No alsoKnownAs, deliberately. Indigo verifies a DECLARED handle
// bidirectionally over DNS and HTTPS, and that lookup would leave the machine —
// which the hermetic tiers forbid. With none declared it marks the handle
// invalid and returns, so this fixture stays local.
func newCountingPLC(t *testing.T) *countingHost {
	t.Helper()
	return newCountingHost(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"service":[{"id":"#atproto_pds",`+
			`"type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.invalid"}]}`,
			clientGuardTestDID)
	})
}

func newCountingHost(t *testing.T, handler http.HandlerFunc) *countingHost {
	t.Helper()

	host := &countingHost{}
	host.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(host.server.Close)
	return host
}

// port is the loopback port this listener is on, so a test can address it by a
// NAME on the same port and have the dial actually arrive.
func (h *countingHost) port(t *testing.T) string {
	t.Helper()

	parsed, err := url.Parse(h.server.URL)
	require.NoError(t, err, "the httptest server's own URL must parse")
	return parsed.Port()
}

// guardTestConfig is the smallest OAuthConfig NewOAuthClient accepts, in the
// PRODUCTION shape: DevMode off, so nothing about the construction under test is
// a dev-only path.
func guardTestConfig(plcURL string, allowPrivateIPs bool) *OAuthConfig {
	return &OAuthConfig{
		PublicURL:       "https://coves.example",
		SealSecret:      base64.StdEncoding.EncodeToString(make([]byte, 32)),
		PLCURL:          plcURL,
		Scopes:          []string{"atproto"},
		AllowPrivateIPs: allowPrivateIPs,
	}
}

// resolvesTo builds the seam that makes a HOSTNAME answer with an address the
// test chose, so the address CLASSIFIER runs rather than the shape check that
// refuses IP literals one branch earlier.
func resolvesTo(t *testing.T, addr string) clientOption {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd fixture would
	// classify as PUBLIC and certify the guard against nothing.
	ip := net.ParseIP(addr)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", addr)

	return withTransportOptions(WithHostResolver(func(context.Context, string) ([]net.IP, error) {
		return []net.IP{ip}, nil
	}))
}

// TestNewOAuthClient_InstallsGuardedClientsForEveryEgressComponent is the
// construction fence: Indigo creates three independent clients, so checking
// only ClientApp.Client and the directory leaves the metadata resolver behind.
func TestNewOAuthClient_InstallsGuardedClientsForEveryEgressComponent(t *testing.T) {
	t.Parallel()

	oauthClient, err := NewOAuthClient(
		guardTestConfig("https://plc.example.invalid", false), indigooauth.NewMemStore())
	require.NoError(t, err, "the test's own config must build a client")

	cacheDir, ok := oauthClient.ClientApp.Dir.(*identity.CacheDirectory)
	require.True(t, ok, "ClientApp.Dir must be the configured cache directory, got %T", oauthClient.ClientApp.Dir)
	baseDir, ok := cacheDir.Inner.(*identity.BaseDirectory)
	require.True(t, ok, "the cache must wrap the configured base directory, got %T", cacheDir.Inner)

	clients := []struct {
		name   string
		client *http.Client
	}{
		{name: "ClientApp.Client", client: oauthClient.ClientApp.Client},
		{name: "ClientApp.Resolver.Client", client: oauthClient.ClientApp.Resolver.Client},
		{name: "BaseDirectory.HTTPClient", client: &baseDir.HTTPClient},
	}

	for _, candidate := range clients {
		candidate := candidate
		t.Run(candidate.name, func(t *testing.T) {
			require.NotNil(t, candidate.client, "%s must be installed", candidate.name)
			transport, ok := candidate.client.Transport.(*ssrfSafeTransport)
			require.True(t, ok, "%s uses %T instead of the Coves SSRF transport", candidate.name, candidate.client.Transport)
			assert.False(t, transport.allowPrivate, "%s unexpectedly has the private-address hatch open", candidate.name)
			assert.Nil(t, transport.base.Proxy, "%s must not honor proxy environment variables", candidate.name)
		})
	}

	resolverTransport := oauthClient.ClientApp.Resolver.Client.Transport.(*ssrfSafeTransport)
	assert.Equal(t, 10*time.Second, oauthClient.ClientApp.Resolver.Client.Timeout,
		"the resolver must retain Indigo's ten-second deadline")
	assert.Equal(t, int64(oauthMetadataMaxResponseBytes), resolverTransport.maxResponseBytes,
		"OAuth metadata needs its tighter response cap")
	assert.NotSame(t, oauthClient.ClientApp.Client, oauthClient.ClientApp.Resolver.Client,
		"the resolver must not silently alias the general OAuth client")
}

func TestOAuthResolverClient_RejectsUnsafeRedirectTargets(t *testing.T) {
	t.Parallel()

	client := newOAuthResolverHTTPClient()
	require.NotNil(t, client.CheckRedirect, "the metadata client must validate every redirect")

	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "HTTPS without an explicit port", target: "https://auth.example/metadata"},
		{name: "HTTP downgrade", target: "http://auth.example/metadata", wantErr: true},
		{name: "explicit standard port", target: "https://auth.example:443/metadata", wantErr: true},
		{name: "explicit non-standard port", target: "https://auth.example:8443/metadata", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tt.target, nil)
			require.NoError(t, err, "building the redirect request")

			err = client.CheckRedirect(req, []*http.Request{{}})
			if tt.wantErr {
				assert.Error(t, err, "unsafe metadata redirect %s was accepted", tt.target)
				return
			}
			assert.NoError(t, err, "safe metadata redirect %s was rejected", tt.target)
		})
	}
}

// routeGuardedClientToTLSServer keeps the guard and resolver intact while
// making every allowed socket land on a hermetic TLS listener. The server's
// client already trusts its generated certificate for example.com.
func routeGuardedClientToTLSServer(t *testing.T, client *http.Client, server *httptest.Server) {
	t.Helper()

	guard, ok := client.Transport.(*ssrfSafeTransport)
	require.True(t, ok, "expected the Coves SSRF transport, got %T", client.Transport)
	trustedTransport, ok := server.Client().Transport.(*http.Transport)
	require.True(t, ok, "httptest TLS client uses %T, not *http.Transport", server.Client().Transport)
	base := trustedTransport.Clone()
	dialer := &net.Dialer{}
	listenerAddr := server.Listener.Addr().String()
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, listenerAddr)
	}
	guard.base = base
}

func newOAuthFlowServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	const issuer = "https://example.com"
	requests := &atomic.Int64{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_, _ = fmt.Fprintf(w, `{
				"issuer":%q,
				"authorization_endpoint":%q,
				"token_endpoint":%q,
				"response_types_supported":["code"],
				"grant_types_supported":["authorization_code","refresh_token"],
				"code_challenge_methods_supported":["S256"],
				"token_endpoint_auth_methods_supported":["none","private_key_jwt"],
				"token_endpoint_auth_signing_alg_values_supported":["ES256"],
				"scopes_supported":["atproto"],
				"authorization_response_iss_parameter_supported":true,
				"require_pushed_authorization_requests":true,
				"pushed_authorization_request_endpoint":%q,
				"dpop_signing_alg_values_supported":["ES256"],
				"client_id_metadata_document_supported":true
			}`,
				issuer, issuer+"/authorize", issuer+"/token", issuer+"/par")
		case "/par":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request_uri":"urn:ietf:params:oauth:request_uri:test","expires_in":60}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// TestOAuthClientApp_StartAuthFlowRefusesPrivateMetadataDNSWithoutReachingIt
// covers the public https:// identifier path end to end, then proves the same
// fixture is reachable only when the explicit dev hatch is open.
func TestOAuthClientApp_StartAuthFlowRefusesPrivateMetadataDNSWithoutReachingIt(t *testing.T) {
	t.Parallel()

	const issuer = "https://example.com"
	tests := []struct {
		name         string
		allowPrivate bool
		wantRequests int64
	}{
		{name: "guarded", allowPrivate: false, wantRequests: 0},
		{name: "hatch open", allowPrivate: true, wantRequests: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newOAuthFlowServer(t)
			oauthClient, err := NewOAuthClient(
				guardTestConfig("https://plc.example.invalid", tt.allowPrivate), indigooauth.NewMemStore(),
				resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: the seam's private DNS answer; production must refuse it before dialing
			require.NoError(t, err, "the test's own config must build a client")

			routeGuardedClientToTLSServer(t, oauthClient.ClientApp.Resolver.Client, server)
			routeGuardedClientToTLSServer(t, oauthClient.ClientApp.Client, server)

			redirect, flowErr := oauthClient.ClientApp.StartAuthFlow(t.Context(), issuer)
			assert.Equal(t, tt.wantRequests, requests.Load(),
				"StartAuthFlow reached the private listener %d times", requests.Load())

			if !tt.allowPrivate {
				require.Error(t, flowErr, "private metadata DNS must stop StartAuthFlow")
				assert.ErrorIs(t, flowErr, ErrBlockedAddress,
					"the failure must come from the Coves address guard; got: %v", flowErr)
				return
			}

			require.NoError(t, flowErr, "the hatch-open control must complete StartAuthFlow")
			assert.Contains(t, redirect, issuer+"/authorize?",
				"the control must produce the authorization redirect, got %q", redirect)
		})
	}
}

func TestOAuthResolverClient_CapsMetadataBodies(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		payload := make([]byte, oauthMetadataMaxResponseBytes+1)
		payload[0] = '"'
		for i := 1; i < len(payload); i++ {
			payload[i] = 'a'
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	oauthClient, err := NewOAuthClient(
		guardTestConfig("https://plc.example.invalid", true), indigooauth.NewMemStore(),
		resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: hatch-open body-cap fixture, routed to the TLS test listener
	require.NoError(t, err, "the test's own config must build a client")
	routeGuardedClientToTLSServer(t, oauthClient.ClientApp.Resolver.Client, server)

	_, resolveErr := oauthClient.ClientApp.Resolver.ResolveAuthServerMetadata(t.Context(), "https://example.com")
	require.Error(t, resolveErr, "metadata larger than the resolver cap must fail")
	assert.ErrorIs(t, resolveErr, ErrResponseTooLarge,
		"the resolver consumed an oversized document without the metadata cap; got: %v", resolveErr)
	assert.Equal(t, int64(1), requests.Load(), "the cap test must reach its server exactly once")
}

// TestOAuthClientApp_ClientRefusesAPrivatePDSWithoutReachingIt is the binding
// contract for client.go's `clientApp.Client = ...` line.
//
// The listener is a stand-in for whatever shares a network with this AppView —
// its Postgres, its PDS, its Jetstream, a cloud metadata endpoint. Under the
// deletion this client is http.DefaultClient, which reaches every one of them.
func TestOAuthClientApp_ClientRefusesAPrivatePDSWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	oauthClient, err := NewOAuthClient(guardTestConfig("https://plc.example.invalid", false), nil)
	require.NoError(t, err, "the test's own config must build a client")
	require.NotNil(t, oauthClient.ClientApp.Client, "the ClientApp must carry an HTTP client")

	resp, getErr := oauthClient.ClientApp.Client.Get(pds.server.URL)
	if getErr == nil {
		_ = resp.Body.Close()
	}

	// THE REACHABILITY CLAIM COMES FIRST, deliberately. It is the security fact;
	// the error is only how the caller learns about it. Asserting the error first
	// with require would abort on failure and hide whether the request actually
	// left the process.
	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times. ClientApp.Client is what indigo sends every "+
			"OAuth-authenticated PDS call through, and the PDS host comes from the user's DID "+
			"document — so a stranger picks the destination. Deleting the assignment in client.go "+
			"leaves indigo's own default, http.DefaultClient: no guard, and no timeout either",
		pds.requests.Load())

	require.Error(t, getErr,
		"ClientApp.Client fetched a loopback address and reported success, which means it is not "+
			"the guarded client this constructor is supposed to install")

	assert.ErrorIsf(t, getErr, ErrBlockedAddress,
		"the refusal must carry the guard's own identity. Without it a build where the assignment "+
			"was deleted looks identical — an unreachable address fails too; got: %v", getErr)
}

// TestOAuthClientApp_ClientRefusesAWellFormedHostThatResolvesPrivate is the
// assertion a loopback-literal fixture cannot make.
//
// The case above is refused on SHAPE — an address written where a hostname
// belongs — one branch before the address classifier runs. That branch is real
// and worth having, but it is not what protects this site: a PDS endpoint out of
// a DID document is a NAME, and its owner controls the zone, so the address is
// decided after every shape check has already passed. This drives that path.
func TestOAuthClientApp_ClientRefusesAWellFormedHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	oauthClient, err := NewOAuthClient(
		guardTestConfig("https://plc.example.invalid", false), nil,
		resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: the address the seam answers with; the guard refuses it before any dial
	require.NoError(t, err, "the test's own config must build a client")

	// `.example` is reserved by RFC 2606, so if the seam were ever bypassed this
	// resolves nowhere rather than reaching a real host.
	target := "http://user-pds.example:" + pds.port(t) + "/xrpc/com.atproto.repo.putRecord"

	resp, getErr := oauthClient.ClientApp.Client.Get(target)
	if getErr == nil {
		_ = resp.Body.Close()
	}

	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times through a hostname whose DNS answer was 127.0.0.1. The "+
			"port is the loopback listener's own, so this is a request that arrived", pds.requests.Load())

	require.Error(t, getErr,
		"user-pds.example is a well-formed host whose answer was a loopback address, and the request "+
			"went ahead anyway")

	assert.ErrorIsf(t, getErr, ErrBlockedAddress,
		"the refusal must be the address classifier's. If this client were indigo's http.DefaultClient "+
			"the seam would not exist at all and the failure would be an ordinary DNS error, which is "+
			"the shape this assertion separates; got: %v", getErr)
}

// TestOAuthClientApp_ControlTheSameHostIsDialledWithTheHatchOpen is the
// falsifiability control for the case above.
//
// Identical construction, identical seam, identical host and port — only
// AllowPrivateIPs differs. With the hatch open the address is no longer refused
// and the request ARRIVES at the listener, which is what pins the refusal above
// to classification rather than to this client being unable to make requests at
// all. A client that refused everything would satisfy that test perfectly.
func TestOAuthClientApp_ControlTheSameHostIsDialledWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	oauthClient, err := NewOAuthClient(
		guardTestConfig("https://plc.example.invalid", true), nil,
		resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: with the hatch open this is dialled and the listener answers
	require.NoError(t, err, "the test's own config must build a client")

	target := "http://user-pds.example:" + pds.port(t) + "/xrpc/com.atproto.repo.putRecord"

	resp, getErr := oauthClient.ClientApp.Client.Get(target)
	if getErr == nil {
		_ = resp.Body.Close()
	}

	require.NoErrorf(t, getErr,
		"the hatch is what every dev stack depends on: a client built with AllowPrivateIPs must reach "+
			"a PDS on the developer's own machine; got: %v", getErr)
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once, so the seam is not answering with the "+
			"address this test gave it and the guarded case above proves nothing", pds.requests.Load())
}

// TestOAuthClientApp_DirRefusesAPrivatePLCWithoutReachingIt pins the HTTPClient
// field on the BaseDirectory this constructor builds.
//
// THE FIELD IS A VALUE, NOT A POINTER — `HTTPClient http.Client` — so omitting
// it is not a nil dereference and not a compile error. It is a zero-value
// client: Transport nil, which means http.DefaultTransport, and Timeout zero,
// which means wait forever. Both failure modes are invisible at the
// construction site.
func TestOAuthClientApp_DirRefusesAPrivatePLCWithoutReachingIt(t *testing.T) {
	t.Parallel()

	plc := newCountingPLC(t)

	oauthClient, err := NewOAuthClient(guardTestConfig(plc.server.URL, false), nil)
	require.NoError(t, err, "the test's own config must build a client")
	require.NotNil(t, oauthClient.ClientApp.Dir, "the ClientApp must carry an identity directory")

	did, err := syntax.ParseDID(clientGuardTestDID)
	require.NoError(t, err, "the test's own DID must parse, or no client is ever reached")

	_, lookupErr := oauthClient.ClientApp.Dir.LookupDID(context.Background(), did)

	assert.Zerof(t, plc.requests.Load(),
		"the PLC listener was reached %d times. The directory's HTTPClient field is an http.Client "+
			"VALUE, so deleting it yields a zero-value client that uses http.DefaultTransport and "+
			"never times out — a working directory with no guard on it", plc.requests.Load())

	require.Error(t, lookupErr,
		"the OAuth client's directory resolved a DID against a PLC on loopback")
	assert.Containsf(t, lookupErr.Error(), "SSRF blocked",
		"the refusal must be the guard's and must say so: indigo wraps this in its own resolution "+
			"error, so the sentence is what tells a blocked address from a PLC that is merely down; "+
			"got: %v", lookupErr)
}

// TestOAuthClientApp_DirRefusesAWellFormedHostThatResolvesPrivate is the
// classifier-driving twin of the case above, for the same reason its sibling
// exists on the ClientApp's client: a loopback literal is refused on shape.
func TestOAuthClientApp_DirRefusesAWellFormedHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	plc := newCountingPLC(t)

	oauthClient, err := NewOAuthClient(
		guardTestConfig("http://plc-directory.example:"+plc.port(t), false), nil,
		resolvesTo(t, "127.0.0.1")) // coves:allow-host-literal: the address the seam answers with; the guard refuses it before any dial
	require.NoError(t, err, "the test's own config must build a client")

	did, err := syntax.ParseDID(clientGuardTestDID)
	require.NoError(t, err, "the test's own DID must parse, or no client is ever reached")

	_, lookupErr := oauthClient.ClientApp.Dir.LookupDID(context.Background(), did)

	assert.Zerof(t, plc.requests.Load(),
		"the PLC listener was reached %d times through a hostname whose DNS answer was 127.0.0.1",
		plc.requests.Load())

	require.Error(t, lookupErr, "the directory resolved against a name that answers with a loopback address")
	assert.Containsf(t, lookupErr.Error(), "SSRF blocked",
		"the refusal must be the address classifier's, not a DNS failure — which is what a directory "+
			"built with a zero-value HTTPClient would produce here, since the seam lives on the client "+
			"this construction is supposed to install; got: %v", lookupErr)
}

// TestOAuthClientApp_DirIsTheConfiguredDirectory pins the OTHER assignment,
// `clientApp.Dir = cacheDir`, and it is the only case here that can.
//
// Deleting that line does not produce a broken directory. It produces indigo's
// DefaultDirectory — a real, working, UNGUARDED directory pointed at the public
// plc.directory — so every refusal assertion in this file still passes: nothing
// reaches the test's listener under that mutation either, because resolution
// went somewhere else entirely. Silently resolving against production instead of
// the configured PLC is its own bug, on top of losing the guard.
//
// So this asserts the POSITIVE direction: with the hatch open, the configured
// PLC — and only it — is what answers. The hatch is what makes a loopback
// fixture addressable at all, and it is also the arrangement a developer runs.
func TestOAuthClientApp_DirIsTheConfiguredDirectory(t *testing.T) {
	t.Parallel()

	plc := newCountingPLC(t)

	oauthClient, err := NewOAuthClient(guardTestConfig(plc.server.URL, true), nil)
	require.NoError(t, err, "the test's own config must build a client")

	did, err := syntax.ParseDID(clientGuardTestDID)
	require.NoError(t, err, "the test's own DID must parse, or no client is ever reached")

	ident, lookupErr := oauthClient.ClientApp.Dir.LookupDID(context.Background(), did)

	require.NoErrorf(t, lookupErr,
		"the OAuth client's directory could not resolve a DID against the PLC it was configured with. "+
			"In dev that PLC is on loopback, so this is what every local login depends on; got: %v",
		lookupErr)
	assert.Equalf(t, int64(1), plc.requests.Load(),
		"the configured PLC was reached %d times rather than once. Zero means resolution went "+
			"somewhere this test did not configure — which is exactly what deleting `clientApp.Dir = "+
			"cacheDir` does: indigo's DefaultDirectory answers instead, against the public "+
			"plc.directory, unguarded", plc.requests.Load())
	assert.Equal(t, clientGuardTestDID, ident.DID.String(),
		"the identity must be the one the configured directory served")
	assert.Equal(t, "https://pds.example.invalid", ident.PDSEndpoint(),
		"the PDS endpoint is what every OAuth-authenticated call is subsequently aimed at")
}
