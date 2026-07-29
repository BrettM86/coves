package testkit

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Accounts, sessions, records and blobs on the test PDS.
//
// This replaces four drifted copies of the same three hundred lines: the
// createPDSAccount in tests/integration/helpers.go, the differently-shaped one
// in tests/e2e/user_signup_test.go, the hand-rolled writePDSRecord, and the
// four *PasswordAuthPDSClientFactory adapters. What they had in common was
// net/http plus encoding/json plus a slightly different set of forgotten error
// checks; what they did not have in common was handle generation, which is why
// the suite collided handles between runs.
//
// # Why not internal/atproto/pds
//
// That package is the real client and this one is not a rival to it. testkit
// cannot import it: it returns *blobs.BlobRef, so it imports
// internal/core/blobs, and testkit importing anything under internal/core makes
// `go test ./internal/core/blobs` an import cycle (see the package doc in
// testkit.go). Production code should keep using it. Test code that needs a
// domain's PDS-client seam gets it through PasswordAuthFactory below, which
// wires internal/atproto/pds in at the CALL SITE, where the import is legal.
//
// # Failure model
//
// Every exported helper takes a TestingT and fails the test on error, like
// testkit.DB. A missing PDS is a failed test, never a skip: if the suite was
// invoked, the infrastructure was requested. Tests that need to assert on a
// failure — a rejected record, a bad token — use Account.XRPC(), which returns
// errors instead of consuming them.

// DefaultPassword is the password every generated account gets.
//
// It is a throwaway local credential for a PDS that holds nothing: the same
// value is in .env.dev, .env.ci and docker-compose.dev.yml. It exists as a
// constant so that a test needing a second session for an account does not have
// to guess it.
const DefaultPassword = "test-password-123"

// ---------------------------------------------------------------------------
// TIDs
// ---------------------------------------------------------------------------

// tidClock issues record keys. Its clock id is drawn randomly per process, which
// is what the field is for: two processes writing to one repo in the same
// microsecond produce different TIDs rather than a duplicate-rkey conflict.
//
// syntax.TIDClock is monotonic under concurrency (it holds a mutex and bumps the
// microsecond when time has not moved), so this replaces the old generateTID's
// hand-rolled counter with the real thing — and the result is a valid TID, which
// "3k" + a decimal timestamp never was.
var tidClock = syntax.NewTIDClock(randomClockID())

func randomClockID() uint {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("testkit: reading random bytes for the TID clock id: %v", err))
	}
	// TID clock ids are 10 bits.
	return uint(binary.BigEndian.Uint64(b[:]) % 1024)
}

// TID returns a fresh record key: a valid atProto TID, strictly increasing
// within this process.
//
// Records are created with a client-side TID rather than letting the PDS mint
// one, because the caller needs the rkey BEFORE the write — that is what a
// firehose matcher is built from, and a matcher that can only be built after the
// event has been emitted is the subscribe-after-write race in another costume.
func TID() string { return tidClock.Next().String() }

// ---------------------------------------------------------------------------
// The PDS
// ---------------------------------------------------------------------------

// PDS is the test stack's Personal Data Server: the thing that holds repos,
// mints accounts, and feeds the firehose.
type PDS struct {
	Endpoint PDSEndpoint
	// Anon is an unauthenticated client, for the endpoints that need no session
	// (createAccount, createSession, _health).
	Anon *XRPCClient
}

type pdsConfig struct {
	baseURL      string
	handleDomain string
}

// PDSOption customises NewPDS.
//
// Options configure a private struct rather than mutating the returned client,
// matching the other constructors in the kit.
type PDSOption func(*pdsConfig)

// WithPDSURL overrides the PDS address from Endpoints(), for tests of this file
// that point it at a server they control.
func WithPDSURL(baseURL string) PDSOption {
	return func(c *pdsConfig) { c.baseURL = trimURL(baseURL) }
}

// WithPDSHandleDomain overrides the domain generated handles are issued under.
func WithPDSHandleDomain(domain string) PDSOption {
	return func(c *pdsConfig) { c.handleDomain = strings.TrimPrefix(domain, ".") }
}

// NewPDS returns a handle on the PDS the test stack is running.
func NewPDS(t TestingT, opts ...PDSOption) *PDS {
	t.Helper()
	endpoint := Endpoints().PDS
	cfg := pdsConfig{baseURL: endpoint.BaseURL, handleDomain: endpoint.HandleDomain}
	for _, opt := range opts {
		opt(&cfg)
	}
	resolved := PDSEndpoint{BaseURL: cfg.baseURL, HandleDomain: cfg.handleDomain}
	return &PDS{Endpoint: resolved, Anon: NewXRPCClient(resolved.BaseURL)}
}

// URL is the PDS' base URL.
func (p *PDS) URL() string { return p.Endpoint.BaseURL }

// WaitHealthy blocks until the PDS answers, failing the test if it has not
// within timeout.
//
// A refused connection is "still starting"; an answered 4xx is a finding about
// the address rather than the service. See waitHealthy in appview.go.
func (p *PDS) WaitHealthy(t TestingT, timeout time.Duration) {
	t.Helper()
	waitHealthy(t, timeout, "the PDS", p.Endpoint.BaseURL, p.Anon.Health)
}

// ---------------------------------------------------------------------------
// Accounts and sessions
// ---------------------------------------------------------------------------

// Account is a PDS account together with an authenticated session on it.
//
// The password is kept so a test can open a second session — an OAuth flow, a
// token-expiry case — without inventing one that does not match.
//
// # Session lifetime
//
// AccessToken is a PDS access JWT, which expires (two hours on a stock PDS).
// Nothing in this package refreshes it in the background, and nothing needs to:
// a test that outlives its own session has other problems. A test that
// deliberately spans that boundary calls RefreshSession, which swaps both tokens
// and the authenticated client together.
//
// # Credentials in failure messages
//
// Account carries a password and two bearer tokens, and a test logging
// `t.Logf("%+v", account)` would otherwise print all three into CI output that
// outlives the run. String and GoString redact them, so every fmt verb is safe.
type Account struct {
	Handle       string
	DID          string
	Email        string
	Password     string
	AccessToken  string
	RefreshToken string

	pds *PDS

	// mu guards the credentials and the derived client, so RefreshSession
	// swaps them as one unit rather than leaving a window in which the token
	// and the client disagree.
	mu     sync.Mutex
	client *XRPCClient
}

// String renders the account without its credentials.
//
// This is what makes %v and %+v safe. GoString covers %#v, which would
// otherwise dump the struct field by field.
func (a *Account) String() string {
	if a == nil {
		return "<nil>"
	}
	return fmt.Sprintf("testkit.Account{Handle: %q, DID: %q, Email: %q, Password: %s, AccessToken: %s, RefreshToken: %s}",
		a.Handle, a.DID, a.Email, redacted(a.Password), redacted(a.AccessToken), redacted(a.RefreshToken))
}

// GoString renders the account without its credentials, for %#v.
func (a *Account) GoString() string { return a.String() }

// redacted describes a secret without disclosing it. The length is kept because
// "REDACTED (0 chars)" and "REDACTED (183 chars)" answer different questions,
// and neither answer is the secret.
func redacted(secret string) string {
	if secret == "" {
		return "\"\""
	}
	return fmt.Sprintf("REDACTED(%d chars)", len(secret))
}

type accountConfig struct {
	handle     string
	label      string
	prefix     string
	email      string
	password   string
	inviteCode string
}

// AccountOption customises CreateAccount.
type AccountOption func(*accountConfig)

// WithHandle sets the account's full handle verbatim, bypassing generation.
//
// Use it only when the handle itself is what a test is about. Anything else
// should take the generated one: a hand-written handle on a PDS whose volume
// survives the run is a collision waiting for the second invocation.
func WithHandle(handle string) AccountOption {
	return func(c *accountConfig) { c.handle = handle }
}

// WithHandlePrefix makes the generated handle start with a readable prefix, so
// a leftover account says which test created it.
func WithHandlePrefix(prefix string) AccountOption {
	return func(c *accountConfig) { c.prefix = prefix }
}

// WithEmail overrides the generated email address.
func WithEmail(email string) AccountOption {
	return func(c *accountConfig) { c.email = email }
}

// WithPassword overrides DefaultPassword.
func WithPassword(password string) AccountOption {
	return func(c *accountConfig) { c.password = password }
}

// WithInviteCode supplies an invite code, for a PDS configured to require one.
func WithInviteCode(code string) AccountOption {
	return func(c *accountConfig) { c.inviteCode = code }
}

// CreateAccount registers a new account on the PDS and returns it with a live
// session.
//
// The handle is generated from UniqueID unless WithHandle says otherwise, which
// is the whole point of routing account creation through here: the local label
// is inside the PDS' 18-character cap, starts with a letter, and carries this
// process's random run prefix, so it cannot collide with another run's leftovers
// on a PDS volume that persists (which the dev stack's does).
func (p *PDS) CreateAccount(t TestingT, opts ...AccountOption) *Account {
	t.Helper()

	cfg := accountConfig{password: DefaultPassword}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.handle == "" {
		if cfg.prefix != "" {
			cfg.label = UniqueIDWithPrefix(t, cfg.prefix)
		} else {
			cfg.label = UniqueID(t)
		}
		cfg.handle = p.Endpoint.Handle(cfg.label)
	}
	if cfg.email == "" {
		// Derived from the handle so a stray account in the PDS' user table can
		// be traced back to the run that made it.
		cfg.email = handleLabel(cfg.handle) + "@test.com"
	}

	payload := map[string]string{
		"handle":   cfg.handle,
		"email":    cfg.email,
		"password": cfg.password,
	}
	if cfg.inviteCode != "" {
		payload["inviteCode"] = cfg.inviteCode
	}

	var resp sessionResponse
	ctx, cancel := context.WithTimeout(context.Background(), defaultXRPCTimeout)
	defer cancel()
	if err := p.Anon.Procedure(ctx, "com.atproto.server.createAccount", payload, &resp); err != nil {
		t.Fatalf("testkit: creating account %q on %s: %v", cfg.handle, p.Endpoint.BaseURL, err)
		return nil
	}
	acct, err := p.newAccount(resp, cfg.email, cfg.password)
	if err != nil {
		t.Fatalf("testkit: creating account %q on %s: %v", cfg.handle, p.Endpoint.BaseURL, err)
		return nil
	}
	return acct
}

// Login opens a session on an existing account.
//
// identifier is a handle or a DID — whatever com.atproto.server.createSession
// accepts. This is the path the instance account takes: it exists on the PDS
// already, so there is nothing to create.
//
// The returned Account has an EMPTY Email: createSession does not disclose it,
// and inventing one would put a value in the field that does not match the
// account. Tests that need it should carry it from CreateAccount.
func (p *PDS) Login(t TestingT, identifier, password string) *Account {
	t.Helper()

	var resp sessionResponse
	ctx, cancel := context.WithTimeout(context.Background(), defaultXRPCTimeout)
	defer cancel()
	err := p.Anon.Procedure(ctx, "com.atproto.server.createSession", map[string]string{
		"identifier": identifier,
		"password":   password,
	}, &resp)
	if err != nil {
		t.Fatalf("testkit: authenticating %q against %s: %v", identifier, p.Endpoint.BaseURL, err)
		return nil
	}
	acct, err := p.newAccount(resp, "", password)
	if err != nil {
		t.Fatalf("testkit: authenticating %q against %s: %v", identifier, p.Endpoint.BaseURL, err)
		return nil
	}
	return acct
}

// sessionResponse is the body com.atproto.server.createAccount and
// com.atproto.server.createSession both return.
type sessionResponse struct {
	DID        string `json:"did"`
	Handle     string `json:"handle"`
	AccessJwt  string `json:"accessJwt"`
	RefreshJwt string `json:"refreshJwt"`
}

// newAccount validates a session response before anything depends on it.
//
// A 200 carrying an empty did, accessJwt or refreshJwt means the PDS — or
// something in front of it — answered with a body that is not a session.
// Accepting it would send an unauthenticated client into the next twenty lines
// of the test, where it fails as a 401 on an unrelated call; accepting a missing
// refresh token would defer the same surprise to RefreshSession.
func (p *PDS) newAccount(resp sessionResponse, email, password string) (*Account, error) {
	switch {
	case resp.DID == "":
		return nil, fmt.Errorf("PDS returned a session with no did (handle %q)", resp.Handle)
	case resp.AccessJwt == "":
		return nil, fmt.Errorf("PDS returned a session with no accessJwt (did %q)", resp.DID)
	case resp.RefreshJwt == "":
		return nil, fmt.Errorf("PDS returned a session with no refreshJwt (did %q)", resp.DID)
	}
	return &Account{
		Handle:       resp.Handle,
		DID:          resp.DID,
		Email:        email,
		Password:     password,
		AccessToken:  resp.AccessJwt,
		RefreshToken: resp.RefreshJwt,
		pds:          p,
		client:       p.Anon.WithBearer(resp.AccessJwt),
	}, nil
}

// RefreshSession exchanges the refresh token for a new session and swaps both
// tokens and the authenticated client together.
//
// com.atproto.server.refreshSession authenticates with the REFRESH token, not
// the access token, and returns a new pair — the old refresh token is spent. So
// this is not a retry-safe operation: calling it twice with the same starting
// state fails the second time, which is correct and worth knowing before
// wrapping it in a loop.
func (a *Account) RefreshSession(t TestingT) {
	t.Helper()

	a.mu.Lock()
	refreshToken := a.RefreshToken
	a.mu.Unlock()

	var resp sessionResponse
	ctx, cancel := context.WithTimeout(context.Background(), defaultXRPCTimeout)
	defer cancel()
	err := a.pds.Anon.WithBearer(refreshToken).
		Procedure(ctx, "com.atproto.server.refreshSession", nil, &resp)
	if err != nil {
		t.Fatalf("testkit: refreshing the session for %s: %v", a.DID, err)
		return
	}
	switch {
	case resp.AccessJwt == "":
		t.Fatalf("testkit: refreshing the session for %s: PDS returned no accessJwt", a.DID)
		return
	case resp.RefreshJwt == "":
		t.Fatalf("testkit: refreshing the session for %s: PDS returned no refreshJwt", a.DID)
		return
	case resp.DID != "" && resp.DID != a.DID:
		t.Fatalf("testkit: refreshing the session for %s returned a session for %s", a.DID, resp.DID)
		return
	}

	a.mu.Lock()
	a.AccessToken, a.RefreshToken = resp.AccessJwt, resp.RefreshJwt
	a.client = a.pds.Anon.WithBearer(resp.AccessJwt)
	a.mu.Unlock()
}

// PDS returns the server this account lives on.
func (a *Account) PDS() *PDS { return a.pds }

// XRPC returns an error-returning client authenticated as this account.
//
// This is the escape hatch for the two things the helpers below deliberately do
// not do: call an endpoint the kit does not wrap, and assert that a call FAILS.
// Everything else should use the helpers, which fail the test for you.
//
// The client is a snapshot of the current session. Hold it across a
// RefreshSession and it will still carry the spent access token.
func (a *Account) XRPC() *XRPCClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// handleLabel returns the part of a handle before the first dot.
func handleLabel(handle string) string {
	label, _, _ := strings.Cut(handle, ".")
	return label
}

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// Record identifies a record written to a repo.
type Record struct {
	URI        string
	CID        string
	Collection string
	RKey       string
}

// RecordValue is a record read back from a repo.
type RecordValue struct {
	URI   string
	CID   string
	Value map[string]any
}

type recordConfig struct {
	rkey       string
	swapRecord string
}

// RecordOption customises a record write.
//
// Not every option applies to every write — a key is an argument to PutRecord
// rather than an option, and a compare-and-swap has nothing to compare on a
// create. Passing an inapplicable one FAILS THE TEST rather than being ignored:
// a swapRecord silently dropped from a create is a test that believes it proved
// optimistic locking and proved nothing.
type RecordOption func(*recordConfig)

// WithRKey sets the record key instead of generating a TID. CreateRecord only.
func WithRKey(rkey string) RecordOption {
	return func(c *recordConfig) { c.rkey = rkey }
}

// WithSwapRecord makes the write conditional on the record's current CID, the
// compare-and-swap atProto offers for lost-update protection. PutRecord only.
func WithSwapRecord(cid string) RecordOption {
	return func(c *recordConfig) { c.swapRecord = cid }
}

// CreateRecord writes a new record to this account's repo.
//
// record may be a map or a struct; it is marshalled as JSON, so a struct needs
// its lexicon field names in tags, including "$type" where the lexicon requires
// one.
func (a *Account) CreateRecord(t TestingT, collection string, record any, opts ...RecordOption) Record {
	t.Helper()
	cfg := newRecordConfig(opts)
	if cfg.swapRecord != "" {
		t.Fatalf("testkit: WithSwapRecord does not apply to CreateRecord — there is no existing record " +
			"to compare a CID against; use PutRecord for a conditional write")
		return Record{}
	}
	if cfg.rkey == "" {
		cfg.rkey = TID()
	}
	return a.writeRecord(t, "com.atproto.repo.createRecord", map[string]any{
		"repo":       a.DID,
		"collection": collection,
		"rkey":       cfg.rkey,
		"record":     record,
	}, collection, cfg.rkey)
}

// PutRecord creates or replaces a record at a known key. This is how a contract
// exercises the update half of create/update/delete.
func (a *Account) PutRecord(t TestingT, collection, rkey string, record any, opts ...RecordOption) Record {
	t.Helper()
	cfg := newRecordConfig(opts)
	if cfg.rkey != "" {
		t.Fatalf("testkit: WithRKey does not apply to PutRecord — the record key is the rkey argument, "+
			"and passing %q as an option would silently disagree with %q", cfg.rkey, rkey)
		return Record{}
	}
	payload := map[string]any{
		"repo":       a.DID,
		"collection": collection,
		"rkey":       rkey,
		"record":     record,
	}
	if cfg.swapRecord != "" {
		payload["swapRecord"] = cfg.swapRecord
	}
	return a.writeRecord(t, "com.atproto.repo.putRecord", payload, collection, rkey)
}

func newRecordConfig(opts []RecordOption) recordConfig {
	var cfg recordConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

func (a *Account) writeRecord(t TestingT, nsid string, payload map[string]any, collection, rkey string) Record {
	t.Helper()
	var resp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultXRPCTimeout)
	defer cancel()
	if err := a.XRPC().Procedure(ctx, nsid, payload, &resp); err != nil {
		t.Fatalf("testkit: writing %s/%s to %s: %v", collection, rkey, a.DID, err)
		return Record{}
	}
	// A 200 with no uri means the body was not a record-write response. The
	// caller is about to build a firehose matcher out of that URI.
	if resp.URI == "" || resp.CID == "" {
		t.Fatalf("testkit: writing %s/%s to %s: PDS answered 200 without uri/cid",
			collection, rkey, a.DID)
		return Record{}
	}
	return Record{URI: resp.URI, CID: resp.CID, Collection: collection, RKey: rkey}
}

// GetRecord reads a record back from this account's repo. A missing record
// fails the test; use XRPC() with IsNotFound to assert absence.
func (a *Account) GetRecord(t TestingT, collection, rkey string) RecordValue {
	t.Helper()
	var resp struct {
		URI   string         `json:"uri"`
		CID   string         `json:"cid"`
		Value map[string]any `json:"value"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultXRPCTimeout)
	defer cancel()
	err := a.XRPC().Query(ctx, "com.atproto.repo.getRecord", url.Values{
		"repo":       {a.DID},
		"collection": {collection},
		"rkey":       {rkey},
	}, &resp)
	if err != nil {
		t.Fatalf("testkit: reading %s/%s from %s: %v", collection, rkey, a.DID, err)
		return RecordValue{}
	}
	return RecordValue{URI: resp.URI, CID: resp.CID, Value: resp.Value}
}

// DeleteRecord removes a record from this account's repo.
//
// IT IS SILENT ABOUT A RECORD THAT WAS NOT THERE. com.atproto.repo.deleteRecord
// answers 200 for a key that does not exist, and — because nothing changed — the
// repo commits nothing and the firehose emits NOTHING. A test that deletes the
// wrong rkey and then awaits the delete event therefore waits out its whole
// timeout on a call that looked like it succeeded.
//
// Use DeleteExistingRecord whenever an Await is going to depend on the event.
func (a *Account) DeleteRecord(t TestingT, collection, rkey string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), defaultXRPCTimeout)
	defer cancel()
	err := a.XRPC().Procedure(ctx, "com.atproto.repo.deleteRecord", map[string]any{
		"repo":       a.DID,
		"collection": collection,
		"rkey":       rkey,
	}, nil)
	if err != nil {
		t.Fatalf("testkit: deleting %s/%s from %s: %v", collection, rkey, a.DID, err)
	}
}

// DeleteExistingRecord reads a record and then deletes it, failing the test if
// it was not there to begin with.
//
// The read is what makes the delete observable: a delete that removed nothing
// emits no commit, so pairing DeleteRecord with an Await on the delete event
// turns a wrong rkey into a timeout that blames the firehose. Proving the record
// existed first turns it into a failure that names the record.
func (a *Account) DeleteExistingRecord(t TestingT, collection, rkey string) {
	t.Helper()
	a.GetRecord(t, collection, rkey)
	a.DeleteRecord(t, collection, rkey)
}

// ---------------------------------------------------------------------------
// Blobs
// ---------------------------------------------------------------------------

// BlobRef is a reference to an uploaded blob, in the exact JSON shape a record
// must embed it in.
//
// It is testkit's own type for the same reason firehose.go declares its own
// event structs: the production one lives in internal/core/blobs, which testkit
// may not import. The wire format is what matters and it is fixed by the
// lexicon, so the risk of the two drifting apart is a spec change, not a
// refactor.
type BlobRef struct {
	Type     string   `json:"$type"`
	Ref      BlobLink `json:"ref"`
	MimeType string   `json:"mimeType"`
	Size     int64    `json:"size"`
}

// BlobLink is the CID link inside a BlobRef.
type BlobLink struct {
	Link string `json:"$link"`
}

// CID returns the blob's content identifier.
func (b BlobRef) CID() string { return b.Ref.Link }

// UploadBlob stores bytes in this account's blob store and returns the reference
// a record embeds.
//
// mimeType must be concrete: see XRPCClient.Upload for why a wildcard fails in a
// way that does not mention content types.
func (a *Account) UploadBlob(t TestingT, data []byte, mimeType string) BlobRef {
	t.Helper()
	var resp struct {
		Blob BlobRef `json:"blob"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultXRPCTimeout)
	defer cancel()
	if err := a.XRPC().Upload(ctx, "com.atproto.repo.uploadBlob", mimeType, data, &resp); err != nil {
		t.Fatalf("testkit: uploading a %d-byte %s blob to %s: %v", len(data), mimeType, a.DID, err)
		return BlobRef{}
	}
	if resp.Blob.Ref.Link == "" {
		t.Fatalf("testkit: uploading a %d-byte %s blob to %s: PDS answered 200 without a blob ref",
			len(data), mimeType, a.DID)
		return BlobRef{}
	}
	return resp.Blob
}

// ---------------------------------------------------------------------------
// Domain interfaces
// ---------------------------------------------------------------------------

// PasswordAuthFactory adapts a PDS-client constructor into the PDS-client
// factory a domain service expects.
//
// Five domain packages declare the identical type under five names —
// votes.PDSClientFactory, communities.PDSClientFactory,
// userblocks.PDSClientFactory, comments.PDSClientFactory, and a bare function
// type in the user-profile wiring — all of them
//
//	func(context.Context, *oauth.ClientSessionData) (pds.Client, error)
//
// testkit cannot name pds.Client (internal/atproto/pds imports
// internal/core/blobs; see the package doc), so it cannot return any of them.
// It can, however, be generic over the client type and take the constructor as
// an argument, which moves the illegal import to the call site where it is
// perfectly legal:
//
//	svc := communities.NewService(repo, testkit.PasswordAuthFactory(pds.NewFromAccessToken))
//
// The returned unnamed function type is assignable to each of the five named
// ones, so no conversion is needed. That one line replaces the four adapters in
// tests/integration/helpers.go, and the validation they each did slightly
// differently now happens once, here.
func PasswordAuthFactory[C any](newClient func(host, did, accessToken string) (C, error)) func(context.Context, *oauth.ClientSessionData) (C, error) {
	return func(_ context.Context, session *oauth.ClientSessionData) (C, error) {
		var zero C
		switch {
		case session == nil:
			return zero, fmt.Errorf("testkit: no session")
		case session.AccessToken == "":
			return zero, fmt.Errorf("testkit: session for %s has no access token", session.AccountDID)
		case session.HostURL == "":
			return zero, fmt.Errorf("testkit: session for %s has no host URL", session.AccountDID)
		}
		return newClient(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}
}
