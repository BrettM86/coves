package pds

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	covesoauth "Coves/internal/atproto/oauth"
)

// The three PDS client entry points reach their HTTP client by different routes:
//
//	NewFromOAuthSession    sess.APIClient() → Client: sess.Client → app.Client
//	NewFromPasswordAuth    atclient.NewAPIClient(host) + guarded createSession
//	NewFromAccessToken     atclient.NewAPIClient(host)
//
// APIClient.Client is a PUBLIC, SETTABLE *http.Client, documented as "May be
// customized after the overall APIClient struct is created; for example to set a
// default request timeout." NewAPIClient initially installs http.DefaultClient,
// so both constructors replace it before their first request. Password login is
// performed through the generated createSession client instead of
// LoginWithPasswordHost, which would make the request before Coves could replace
// its transport.
//
// # WHY THIS FILE WAS REWRITTEN
//
// It previously asserted that the installed client is not `http.DefaultClient`
// and that its Timeout is not zero. Both are true of
//
//	&http.Client{Timeout: bearerRequestTimeout}
//
// which is what this package actually built — a client with a deadline and NO
// ADDRESS GUARD — so the file was green while the property its name promised was
// absent. The audit that scans for bare `&http.Client{}` construction found it.
// Everything below asserts refusal and reachability instead, because "which
// client object is installed" is not a security property and "which addresses it
// will dial" is.

const (
	factoryGuardDID = "did:plc:factoryguard2222222"

	// factoryGuardHost passes every shape check these constructors apply and is a
	// name rather than an address, so classification is the only thing that can
	// refuse it. `.example` is reserved by RFC 2606, so nothing resolves it for
	// real if the seam is ever bypassed.
	factoryGuardHost = "https://community-pds.example"
)

// countingPDS records whether a request ever reached a listener, and what
// credential it carried.
//
// The token is recorded because for these constructors "the listener was
// reached" and "a live PDS credential left the process" are the same event: the
// client is Bearer-authed, so the Authorization header is on the wire before any
// response exists. A blocked request costs an attacker a retry; a leaked bearer
// token is not recoverable.
type countingPDS struct {
	server   *httptest.Server
	requests atomic.Int64
	tokens   chan string
}

func newCountingPDS(t *testing.T) *countingPDS {
	t.Helper()

	pds := &countingPDS{tokens: make(chan string, 8)}
	pds.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pds.requests.Add(1)
		select {
		case pds.tokens <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		// Serves both shapes these tests provoke: a createSession response for the
		// password login, and a getRecord response for everything after it.
		_, _ = w.Write([]byte(`{"did":"` + factoryGuardDID + `","handle":"test.example",` +
			`"accessJwt":"access-token","refreshJwt":"refresh-token",` +
			`"uri":"at://` + factoryGuardDID + `/app.test/self","cid":"bafyguard",` +
			`"value":{"leaked":"from an internal endpoint"}}`))
	}))
	t.Cleanup(pds.server.Close)
	return pds
}

// assertNoTokenLeaked fails with the credential named, because the severity of
// this site is not legible from a request count alone.
func (p *countingPDS) assertNoTokenLeaked(t *testing.T) {
	t.Helper()
	select {
	case token := <-p.tokens:
		assert.Failf(t, "a PDS access token was handed to the listener",
			"the listener received %q. This is credential exfiltration and not only SSRF: the host "+
				"came from a community record, and the guard must refuse it BEFORE the request "+
				"carrying the token is sent", token)
	default:
	}
}

// ---------------------------------------------------------------------------
// NewFromAccessToken
// ---------------------------------------------------------------------------

// TestNewFromAccessToken_RefusesAPrivateHostWithoutReachingIt is the binding
// contract for database-derived PDS destinations.
//
// # WHAT IS BEING DIALLED
//
// `host` is `community.PDSURL` — a per-community database column, written when
// the community is created or federated in, and read by
// posts.(*postService).deleteCommunityPost, posts.NewCommunityRepoFactory and
// cmd/rematerialize-posts's communityRepoOpener. The AppView shares a network
// with its Postgres, its PDS, its Jetstream and a cloud metadata endpoint, and
// deleteCommunityPost reaches this constructor on a REQUEST path, so the trigger
// is an ordinary API call.
func TestNewFromAccessToken_RefusesAPrivateHostWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	c, err := NewFromAccessToken(pds.server.URL, factoryGuardDID, "secret-pds-access-token",
		PrivateHostOptions(false)...)
	require.NoError(t, err, "constructing a client with valid arguments")

	_, err = c.GetRecord(context.Background(), "app.test", "self")

	// THE REACHABILITY CLAIMS COME FIRST, deliberately. They are the security
	// facts; the error is only how the caller learns about them. Asserting the
	// error first with require would abort on failure and hide whether the token
	// actually left the process.
	assert.Zerof(t, pds.requests.Load(),
		"the listener was reached %d times. The host is a community's PDSURL — a database column, "+
			"not a constant — and the request leaving the process is the SSRF whatever comes back",
		pds.requests.Load())
	pds.assertNoTokenLeaked(t)

	require.Error(t, err,
		"a Bearer-authed PDS client read a record from a loopback address and reported success. "+
			"posts.(*postService).deleteCommunityPost reaches this constructor on a request path")

	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the read failed, but not because the guard refused the address. A PDS that simply could "+
			"not be reached fails identically, so a plain require.Error here cannot tell a refusal "+
			"from an unreachable host — which is how this file was once green while the property its "+
			"name promised was absent, against a build whose client was `&http.Client{Timeout: "+
			"bearerRequestTimeout}`: a deadline and no guard at all; got: %v", err)
}

// TestNewFromAccessToken_ReachesThePDSWhenTheHatchIsOpen is the other direction,
// and the falsifiability control for the case above.
//
// It is also the property the whole integration tier depends on: every test in
// this tree that writes to the CI stack's PDS goes through this constructor
// against loopback, which is exactly what the guard refuses.
func TestNewFromAccessToken_ReachesThePDSWhenTheHatchIsOpen(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	c, err := NewFromAccessToken(pds.server.URL, factoryGuardDID, "secret-pds-access-token",
		PrivateHostOptions(true)...)
	require.NoError(t, err, "constructing a client with valid arguments")

	record, err := c.GetRecord(context.Background(), "app.test", "self")

	require.NoErrorf(t, err,
		"the hatch is what every integration test and every dev stack depends on: a client built "+
			"with PrivateHostOptions(true) must reach a loopback PDS; got: %v", err)
	require.NotNil(t, record, "the fixture serves a record, so one must come back")
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once", pds.requests.Load())
}

// TestNewFromAccessToken_InstallsAClientRatherThanTheStdlibDefault is what this
// file used to assert as its whole contract, kept because it still fences one
// thing the tests above do not.
//
// IT IS NECESSARY AND NOT SUFFICIENT, and the name now says so. A client can
// differ from http.DefaultClient and still dial anything — that is precisely the
// state the audit caught. What this keeps is the NIL route: atclient's
// apiclient.go:142 substitutes http.DefaultClient when Client is nil, so a
// conversion that left the field unset would reach the unguarded default by a
// quieter path than the one everyone reads for.
func TestNewFromAccessToken_InstallsAClientRatherThanTheStdlibDefault(t *testing.T) {
	t.Parallel()

	c, err := NewFromAccessToken("https://pds.example", factoryGuardDID, "token")
	require.NoError(t, err, "constructing a client with valid arguments")

	concrete, ok := c.(*client)
	require.True(t, ok, "NewFromAccessToken must return the concrete *client these tests drive")
	require.NotNil(t, concrete.apiClient, "the PDS client must hold an APIClient")

	require.NotNilf(t, concrete.apiClient.Client,
		"the APIClient's HTTP client is nil. atclient's apiclient.go:142 substitutes "+
			"http.DefaultClient for a nil client, so leaving it unset reaches the unguarded, "+
			"un-timed default without ever naming it")
	assert.NotSamef(t, http.DefaultClient, concrete.apiClient.Client,
		"the PDS client is using http.DefaultClient — unguarded, and with no timeout at all")
}

// TestNewFromAccessToken_PreservesTheRequestTimeout guards the value the shared
// client would otherwise swallow.
//
// NewSSRFSafeHTTPClient ships a 15s ceiling. bearerRequestTimeout is 30s, chosen
// to match the longest deadline anything in this tree allows a PDS because this
// client carries record writes and blob uploads. Adopting the guarded client
// without re-applying it would halve the allowance for every PDS write in the
// AppView, as a silent side effect of an SSRF fix.
func TestNewFromAccessToken_PreservesTheRequestTimeout(t *testing.T) {
	t.Parallel()

	c, err := NewFromAccessToken("https://pds.example", factoryGuardDID, "token")
	require.NoError(t, err, "constructing a client with valid arguments")

	concrete, ok := c.(*client)
	require.True(t, ok, "NewFromAccessToken must return the concrete *client these tests drive")
	require.NotNil(t, concrete.apiClient.Client, "the APIClient must hold an HTTP client")

	assert.Equalf(t, bearerRequestTimeout, concrete.apiClient.Client.Timeout,
		"the PDS client runs on a %v timeout instead of bearerRequestTimeout (%v). This client "+
			"carries record writes and blob uploads, and no other deadline covers this call site",
		concrete.apiClient.Client.Timeout, bearerRequestTimeout)
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed is the
// only place the branch production runs is ever evaluated.
//
// `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` takes the PERMISSIVE branch at
// every call site holding such a boolean. A green merge gate therefore says
// nothing about whether production is guarded.
//
// The claim is not "the options returned are safe". It is that there are NONE, so
// what production gets is exactly the constructor's own defaults — a claim a
// reader can check in one glance.
func TestPrivateHostOptions_ReturnsZeroOptionsWhenPrivateHostsAreDisallowed(t *testing.T) {
	t.Parallel()

	opts := PrivateHostOptions(false)

	assert.Lenf(t, opts, 0,
		"PrivateHostOptions(false) returned %d option(s). The production branch must contribute "+
			"nothing at all", len(opts))
}

// TestPrivateHostOptions_BindTheGateToTheClient pins the other direction through
// behaviour, so a helper that returns nothing in BOTH directions — which
// satisfies the length check above perfectly — is caught here instead.
func TestPrivateHostOptions_BindTheGateToTheClient(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	c, err := NewFromAccessToken(pds.server.URL, factoryGuardDID, "token",
		PrivateHostOptions(true)...)
	require.NoError(t, err, "constructing a client with valid arguments")

	_, err = c.GetRecord(context.Background(), "app.test", "self")

	require.NoErrorf(t, err,
		"a client built from PrivateHostOptions(true) could not reach a loopback PDS, so the dev "+
			"hatch does nothing and the guarded assertions elsewhere in this file prove only that "+
			"the client refuses everything; got: %v", err)
	assert.Equalf(t, int64(1), pds.requests.Load(),
		"the listener was reached %d times rather than once", pds.requests.Load())
}

// ---------------------------------------------------------------------------
// Classification, through the resolver seam
// ---------------------------------------------------------------------------

// resolvingAccessTokenClient builds the client the way production does and then
// replaces only its NAME RESOLUTION, so the client under test is the real one.
//
// withTransportOptions is unexported on purpose: the seam must not be reachable
// from any non-test package, which is what the audit's WithHostResolver category
// enforces.
func resolvingAccessTokenClient(t *testing.T, allowPrivateHosts bool, resolvesTo string) Client {
	t.Helper()

	// Checked, not assumed: isPrivateIP(nil) is false, so a typo'd fixture would
	// classify as PUBLIC and certify the guard against nothing.
	ip := net.ParseIP(resolvesTo)
	require.NotNilf(t, ip, "the test's own answer %q must parse as an IP address", resolvesTo)

	opts := append(PrivateHostOptions(allowPrivateHosts),
		withTransportOptions(covesoauth.WithHostResolver(
			func(context.Context, string) ([]net.IP, error) { return []net.IP{ip}, nil })))

	c, err := NewFromAccessToken(factoryGuardHost, factoryGuardDID, "secret-pds-access-token", opts...)
	require.NoError(t, err, "constructing a client with valid arguments")
	return c
}

// TestNewFromAccessToken_RefusesAWellFormedHostThatResolvesPrivate is the
// assertion a loopback-literal fixture cannot make: the guard's CLASSIFICATION
// pass, on a name that survives every earlier check.
//
// It matters here more than at most sites because a community's PDSURL is a
// HOSTNAME in production — `https://pds.somecommunity.example` — never a literal.
// A guard that only refused literals would pass every test above and refuse
// nothing that would actually be attempted.
func TestNewFromAccessToken_RefusesAWellFormedHostThatResolvesPrivate(t *testing.T) {
	t.Parallel()

	c := resolvingAccessTokenClient(t, false, "169.254.169.254")

	_, err := c.GetRecord(context.Background(), "app.test", "self")

	require.Errorf(t, err,
		"%s is a well-formed https host whose DNS answer was the cloud metadata address, and the "+
			"client read from it anyway. A federated community's PDS URL is chosen by whoever wrote "+
			"the record, and they own the zone", factoryGuardHost)
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the refusal must carry the guard's identity, or a build where this constructor was never "+
			"converted looks identical; got: %v", err)
}

// TestNewFromAccessToken_ControlTheSameHostIsDialledWithTheHatchOpen is the
// falsifiability control for the case above.
//
// Identical constructor, identical seam, identical host — only the hatch
// differs. With it open the address is no longer refused, so the request
// proceeds to a dial. That difference is what pins the refusal above to
// classification rather than to this test being unable to make requests at all.
func TestNewFromAccessToken_ControlTheSameHostIsDialledWithTheHatchOpen(t *testing.T) {
	t.Parallel()

	c := resolvingAccessTokenClient(t, true, "127.0.0.1") // coves:allow-host-literal: with the hatch open this is dialled and refused by the OS

	_, err := c.GetRecord(context.Background(), "app.test", "self")

	require.Error(t, err,
		"nothing listens on loopback:443, so this read must fail — if it succeeded, the seam is not "+
			"answering with the address this test gave it")
	assert.NotErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the hatch was open and the address was still refused by the guard. Either PrivateHostOptions "+
			"is not reaching the client, or the guarded case above proves nothing: a client that "+
			"refuses every address refuses that case too, for a reason unconnected to "+
			"classification; got: %v", err)
}

// ---------------------------------------------------------------------------
// NewFromPasswordAuth
// ---------------------------------------------------------------------------

// TestNewFromPasswordAuth_RefusesAPrivateHostWithoutReachingIt proves the
// password-bearing createSession request uses the guard too. This is the request
// that Indigo's LoginWithPasswordHost would send through http.DefaultClient.
func TestNewFromPasswordAuth_RefusesAPrivateHostWithoutReachingIt(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	_, err := NewFromPasswordAuth(context.Background(), pds.server.URL, "test.example", "password",
		PrivateHostOptions(false)...)

	assert.Zerof(t, pds.requests.Load(),
		"the password-bearing login reached the loopback listener %d time(s)", pds.requests.Load())
	require.Error(t, err, "password login to a loopback PDS must be refused")
	assert.ErrorIsf(t, err, covesoauth.ErrBlockedAddress,
		"the login failed, but not because the guard refused the address; got: %v", err)
}

// TestNewFromPasswordAuth_ReachesThePDSWhenTheHatchIsOpen is the control, and
// the property tests/fixtures and the E2E tier depend on.
func TestNewFromPasswordAuth_ReachesThePDSWhenTheHatchIsOpen(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	c, err := NewFromPasswordAuth(context.Background(), pds.server.URL, "test.example", "password",
		PrivateHostOptions(true)...)
	require.NoErrorf(t, err, "the fake PDS serves a valid createSession response; got: %v", err)

	record, err := c.GetRecord(context.Background(), "app.test", "self")

	require.NoErrorf(t, err,
		"with the hatch open a password-authed client must reach a loopback PDS; got: %v", err)
	require.NotNil(t, record, "the fixture serves a record, so one must come back")
	assert.Equalf(t, int64(2), pds.requests.Load(),
		"the fixture must have been reached twice — once by the login, once by the read; got %d",
		pds.requests.Load())
}

// TestNewFromPasswordAuth_PreservesTheRequestTimeout is the same fence as the
// access-token constructor's, at the other entry point.
func TestNewFromPasswordAuth_PreservesTheRequestTimeout(t *testing.T) {
	t.Parallel()

	pds := newCountingPDS(t)

	c, err := NewFromPasswordAuth(context.Background(), pds.server.URL, "test.example", "password",
		PrivateHostOptions(true)...)
	require.NoErrorf(t, err, "the fake PDS serves a valid createSession response; got: %v", err)

	concrete, ok := c.(*client)
	require.True(t, ok, "NewFromPasswordAuth must return the concrete *client these tests drive")
	require.NotNil(t, concrete.apiClient.Client, "the APIClient must hold an HTTP client")

	assert.Equalf(t, bearerRequestTimeout, concrete.apiClient.Client.Timeout,
		"the client runs on a %v timeout instead of bearerRequestTimeout (%v)",
		concrete.apiClient.Client.Timeout, bearerRequestTimeout)
}

// ---------------------------------------------------------------------------
// NewFromOAuthSession — the chain this package does not own
// ---------------------------------------------------------------------------

// The OAuth path is guarded today by three assignments in two dependencies:
//
//	1. internal/atproto/oauth NewOAuthClient   clientApp.Client = NewSSRFSafeHTTPClient(...)
//	2. indigo   oauth.go:200                  sess := ClientSession{Client: app.Client, ...}
//	3. indigo   session.go:403                c := atclient.APIClient{Client: sess.Client, ...}
//
// All three were re-read in the vendored source at
// v0.0.0-20260202181658-ea3d39eec464 and all three hold. Links 2 and 3 are
// indigo's and can vanish in a dependency bump; link 1 is ours.
//
// TWO OF THE THREE ARE FENCED BELOW. Link 1 is not fenced here and cannot be
// from this package — it is an assignment inside NewOAuthClient, reachable only
// by standing up that constructor with a config, which belongs in
// internal/atproto/oauth's own tests.
//
// It is now fenced THERE. An earlier version of this comment ended "today
// nothing in the tree asserts that clientApp.Client is guarded, so deleting that
// line fails no test", which was true when written and is no longer:
// internal/atproto/oauth/client_guard_test.go stands the constructor up and
// asserts the installed client refuses a private address without reaching a live
// listener. Deleting link 1 now fails three tests there. The line is left
// described rather than deleted because the CHAIN is the thing worth reading in
// one place — this file fences links 2 and 3, and a reader needs to know where
// link 1 lives to know the chain is whole.

// fakeSessionStore is the minimum indigo's ResumeSession needs: a GetSession that
// answers. Every other method panics, which is this repo's convention — a call
// nobody predicted fails immediately rather than returning a zero value that
// quietly changes what the test proves.
type fakeSessionStore struct {
	session *indigooauth.ClientSessionData
}

func (s *fakeSessionStore) GetSession(context.Context, syntax.DID, string) (*indigooauth.ClientSessionData, error) {
	return s.session, nil
}

func (s *fakeSessionStore) SaveSession(context.Context, indigooauth.ClientSessionData) error {
	panic("fakeSessionStore: SaveSession is not part of this test's contract")
}

func (s *fakeSessionStore) DeleteSession(context.Context, syntax.DID, string) error {
	panic("fakeSessionStore: DeleteSession is not part of this test's contract")
}

func (s *fakeSessionStore) GetAuthRequestInfo(context.Context, string) (*indigooauth.AuthRequestData, error) {
	panic("fakeSessionStore: GetAuthRequestInfo is not part of this test's contract")
}

func (s *fakeSessionStore) SaveAuthRequestInfo(context.Context, indigooauth.AuthRequestData) error {
	panic("fakeSessionStore: SaveAuthRequestInfo is not part of this test's contract")
}

func (s *fakeSessionStore) DeleteAuthRequestInfo(context.Context, string) error {
	panic("fakeSessionStore: DeleteAuthRequestInfo is not part of this test's contract")
}

// TestResumeSession_CopiesTheClientAppsClientIntoTheSession fences LINK 2, which
// nothing in this tree previously pinned.
//
// THIS TEST IS A FENCE AND IS EXPECTED TO PASS TODAY. It is here because the
// property is load-bearing and lives in someone else's library: if a dependency
// bump stops ResumeSession copying app.Client, every OAuth-authenticated PDS call
// in the AppView silently reverts to http.DefaultClient — unguarded and with no
// timeout — and no other test in the repository notices.
func TestResumeSession_CopiesTheClientAppsClientIntoTheSession(t *testing.T) {
	t.Parallel()

	// A sentinel with a Timeout nothing else in the tree uses, so an assertion on
	// identity cannot be satisfied by some other client that happens to exist.
	sentinel := &http.Client{Timeout: 41 * time.Second}

	did, err := syntax.ParseDID("did:plc:oauthsessiontest")
	require.NoError(t, err, "the test's own DID must parse")

	// ResumeSession's LAST act is to parse the session's DPoP key, so a session
	// without one never reaches the assignment under test. Generated rather than
	// hard-coded: a real P-256 key is what the parser accepts, and pinning a
	// literal would only pin this test to today's encoding.
	dpopKey, err := atcrypto.GeneratePrivateKeyP256()
	require.NoError(t, err, "the test's own DPoP key must generate")

	app := &indigooauth.ClientApp{
		Client: sentinel,
		Config: &indigooauth.ClientConfig{},
		Store: &fakeSessionStore{session: &indigooauth.ClientSessionData{
			AccountDID:              did,
			SessionID:               "session-1",
			HostURL:                 "https://pds.example",
			DPoPPrivateKeyMultibase: dpopKey.Multibase(),
		}},
	}

	sess, err := app.ResumeSession(context.Background(), did, "session-1")
	require.NoErrorf(t, err, "the fake store answers, so the resume must succeed; got: %v", err)
	require.NotNil(t, sess, "ResumeSession must return a session")

	assert.Samef(t, sentinel, sess.Client,
		"indigo no longer copies ClientApp.Client into the session it builds. That copy is the "+
			"FIRST of the two links that make NewFromOAuthSession guarded — "+
			"internal/atproto/oauth's NewOAuthClient sets ClientApp.Client to a guarded client, and if "+
			"it stops arriving here the guard never reaches the APIClient either")
}

// TestNewFromOAuthSession_InheritsTheGuardedClientFromTheClientApp fences LINK 3.
//
// It drives indigo's own structs rather than standing up a session store,
// because the link that can break is indigo's: APIClient() is the second copy,
// and a ClientSession literal exercises it directly. Building a real session
// would need a DPoP key and would test the fixture more than the contract.
func TestNewFromOAuthSession_InheritsTheGuardedClientFromTheClientApp(t *testing.T) {
	t.Parallel()

	sentinel := &http.Client{Timeout: 41 * time.Second}

	did, err := syntax.ParseDID("did:plc:oauthsessiontest")
	require.NoError(t, err, "the test's own DID must parse")

	sess := &indigooauth.ClientSession{
		Client: sentinel,
		Config: &indigooauth.ClientConfig{},
		Data: &indigooauth.ClientSessionData{
			AccountDID: did,
			HostURL:    "https://pds.example",
		},
	}

	apiClient := sess.APIClient()

	require.NotNil(t, apiClient, "ClientSession.APIClient must return a client")
	assert.Samef(t, sentinel, apiClient.Client,
		"indigo no longer threads ClientSession.Client into the APIClient it builds. That chain is "+
			"the ONLY reason NewFromOAuthSession is guarded today: internal/atproto/oauth's NewOAuthClient "+
			"sets ClientApp.Client to a guarded client, ResumeSession copies it into the session, and "+
			"APIClient copies it again. If this assertion fails after a dependency bump, every "+
			"OAuth-authenticated PDS call in the AppView has silently reverted to http.DefaultClient")
}
