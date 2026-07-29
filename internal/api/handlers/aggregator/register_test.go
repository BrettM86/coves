package aggregator

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Registration is how an aggregator tells this instance it exists, and the only
// thing standing between "I claim this DID" and a row in the users table is the
// .well-known/atproto-did check: the caller must control a domain that publishes
// the DID it is registering. Everything here is about that boundary — which
// requests are refused, and what is written when one is not.
//
// The two collaborators are faked. The user service is faked because what
// matters is WHICH CreateUserRequest the handler builds (the handle and PDS URL
// have to come from DID resolution, never from the request body), and a real
// repository would only let the test read back what it already asserted. The
// identity resolver is faked because the alternative is the public PLC
// directory, which the suite may never touch.
//
// These assertions used to live in tests/integration/aggregator_registration_test.go,
// where every one of them held a Postgres clone open to check a row the handler
// writes through a service that has its own tests.

const (
	// The aggregator under test: a DID, the handle and PDS its DID document
	// resolves to, and a domain it will prove ownership of.
	// The handle uses a real TLD rather than .example: the user service these
	// tests fake validates handle syntax and rejects the reserved TLDs, so a
	// .example handle here would model a CreateUserRequest that can never
	// succeed against the real service (register_users_row_test.go is the test
	// that would have caught it).
	registrantDID    = "did:plc:rssaggregator"
	registrantHandle = "rss.aggregator.dev"
	registrantPDS    = "https://pds.example.com"

	wellKnownPath = "/.well-known/atproto-did"
)

// fakeUserService answers the two calls on the registration path and records
// what it was asked to create.
//
// The embedded interface is nil on purpose: any other method the handler starts
// calling panics immediately instead of returning a zero value that quietly
// changes what this test proves.
type fakeUserService struct {
	users.UserService

	registered map[string]*users.User
	created    []users.CreateUserRequest
}

func (f *fakeUserService) GetUserByDID(_ context.Context, did string) (*users.User, error) {
	if user, ok := f.registered[did]; ok {
		return user, nil
	}
	return nil, users.ErrUserNotFound
}

func (f *fakeUserService) CreateUser(_ context.Context, req users.CreateUserRequest) (*users.User, error) {
	f.created = append(f.created, req)
	user := &users.User{DID: req.DID, Handle: req.Handle, PDSURL: req.PDSURL, CreatedAt: time.Now()}
	f.registered[req.DID] = user
	return user, nil
}

// fakeIdentityResolver stands in for the PLC directory. Like the user service
// above, only the method the handler calls is implemented.
type fakeIdentityResolver struct {
	identity.Resolver

	identities map[string]*identity.Identity
}

func (f *fakeIdentityResolver) Resolve(_ context.Context, identifier string) (*identity.Identity, error) {
	if resolved, ok := f.identities[identifier]; ok {
		return resolved, nil
	}
	return nil, fmt.Errorf("%s is not in the directory", identifier)
}

// registerFixture is the handler wired as cmd/server wires it, pointed at a
// stub domain.
type registerFixture struct {
	handler  *RegisterHandler
	users    *fakeUserService
	resolver *fakeIdentityResolver

	// domain is the stub's host:port — the shape a client actually sends, with
	// no scheme.
	domain string
}

// newRegisterFixture starts a stub domain serving wellKnown over TLS and builds
// the handler against it.
func newRegisterFixture(t *testing.T, wellKnown http.Handler) *registerFixture {
	t.Helper()

	// TLS rather than plaintext because the handler builds an https:// URL and
	// refuses to do otherwise — a domain that cannot serve HTTPS cannot prove
	// ownership. stub.Client() trusts exactly this server's certificate, which
	// is narrower than disabling verification altogether.
	stub := httptest.NewTLSServer(wellKnown)
	t.Cleanup(stub.Close)

	userService := &fakeUserService{registered: map[string]*users.User{}}
	resolver := &fakeIdentityResolver{
		identities: map[string]*identity.Identity{
			registrantDID: {
				DID:        registrantDID,
				Handle:     registrantHandle,
				PDSURL:     registrantPDS,
				ResolvedAt: time.Now(),
				Method:     identity.MethodHTTPS,
			},
		},
	}

	handler := NewRegisterHandler(userService, resolver)
	handler.SetHTTPClient(stub.Client())

	return &registerFixture{
		handler:  handler,
		users:    userService,
		resolver: resolver,
		domain:   strings.TrimPrefix(stub.URL, "https://"),
	}
}

// register posts a well-formed registration request.
func (f *registerFixture) register(t *testing.T, did, domain string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(RegisterRequest{DID: did, Domain: domain})
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	return f.send(t, http.MethodPost, body)
}

// send posts an arbitrary body, for the cases where the body is the thing under
// test.
func (f *registerFixture) send(t *testing.T, method string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, "/xrpc/social.coves.aggregator.register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.handler.HandleRegister(rec, req)
	return rec
}

// wellKnownServing answers the .well-known path with body, and 404s everything
// else the way a real domain would.
func wellKnownServing(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wellKnownPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	})
}

func assertRegisterError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) XRPCError {
	t.Helper()

	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d. Body: %s", rec.Code, wantStatus, rec.Body.String())
	}
	var body XRPCError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Error != wantCode {
		t.Errorf("error code = %q, want %q", body.Error, wantCode)
	}
	return body
}

func TestRegister_RegistersAnAggregatorThatProvesItsDomain(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	rec := f.register(t, registrantDID, f.domain)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	var resp RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if resp.DID != registrantDID {
		t.Errorf("did = %q, want %q", resp.DID, registrantDID)
	}
	if resp.Handle != registrantHandle {
		t.Errorf("handle = %q, want the resolved %q", resp.Handle, registrantHandle)
	}
	// Registration is step one of two: the aggregator is a user here, but it is
	// not an aggregator until it publishes a service declaration. The message is
	// where a client learns that, so it is part of the contract.
	if !strings.Contains(resp.Message, "service declaration") {
		t.Errorf("message = %q, want it to name the next step", resp.Message)
	}

	if len(f.users.created) != 1 {
		t.Fatalf("created %d users, want exactly 1", len(f.users.created))
	}
	created := f.users.created[0]
	if created.DID != registrantDID {
		t.Errorf("created DID = %q, want %q", created.DID, registrantDID)
	}
	// The handle and PDS URL come from resolving the DID, never from the
	// request. A caller who could name its own handle here could register under
	// someone else's.
	if created.Handle != registrantHandle {
		t.Errorf("created handle = %q, want the resolved %q", created.Handle, registrantHandle)
	}
	if created.PDSURL != registrantPDS {
		t.Errorf("created PDS URL = %q, want the resolved %q", created.PDSURL, registrantPDS)
	}
}

// did:web is the other supported method, and it reaches registration by a
// different branch of the format check than did:plc.
func TestRegister_AcceptsDIDWeb(t *testing.T) {
	const webDID = "did:web:aggregator.example.com"

	f := newRegisterFixture(t, wellKnownServing(webDID))
	f.resolver.identities[webDID] = &identity.Identity{
		DID:    webDID,
		Handle: "aggregator.example.com",
		PDSURL: registrantPDS,
	}

	rec := f.register(t, webDID, f.domain)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
}

// The check that makes registration mean anything: the domain must publish the
// DID being registered, not merely publish something.
func TestRegister_RefusesADomainServingSomeoneElsesDID(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing("did:plc:somebodyelse"))

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusUnauthorized, "DomainVerificationFailed")

	if len(f.users.created) != 0 {
		t.Errorf("a failed domain check still registered %d users", len(f.users.created))
	}
}

func TestRegister_RefusesADomainWithNoWellKnown(t *testing.T) {
	f := newRegisterFixture(t, http.NotFoundHandler())

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusUnauthorized, "DomainVerificationFailed")
}

// A malicious domain can answer with as many bytes as the AppView is willing to
// read, so the handler caps the response at maxWellKnownSize.
//
// The body below is that cap plus one byte, and it ENDS WITH THE CORRECT DID
// behind padding that TrimSpace removes. That shape is the whole point: reading
// it in full would verify the domain successfully, so the only way this request
// can be refused is if the handler stopped reading. The two tests this replaces
// padded with "A" instead, which fails the DID comparison whether the cap exists
// or not — deleting maxWellKnownSize left both of them green.
func TestRegister_RefusesAnOversizedWellKnownResponse(t *testing.T) {
	body := strings.Repeat(" ", maxWellKnownSize+1-len(registrantDID)) + registrantDID
	if len(body) <= maxWellKnownSize {
		t.Fatalf("the test body is %d bytes, which does not exceed the %d-byte cap", len(body), maxWellKnownSize)
	}

	f := newRegisterFixture(t, wellKnownServing(body))

	rec := f.register(t, registrantDID, f.domain)
	assertRegisterError(t, rec, http.StatusUnauthorized, "DomainVerificationFailed")

	if len(f.users.created) != 0 {
		t.Errorf("an over-long .well-known response still registered %d users", len(f.users.created))
	}
}

func TestRegister_RefusesADIDThatIsAlreadyRegistered(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))
	f.users.registered[registrantDID] = &users.User{DID: registrantDID, Handle: registrantHandle, PDSURL: registrantPDS}

	rec := f.register(t, registrantDID, f.domain)
	body := assertRegisterError(t, rec, http.StatusConflict, "AlreadyRegistered")
	if !strings.Contains(body.Message, "already registered") {
		t.Errorf("message = %q, want it to say the aggregator is already registered", body.Message)
	}

	if len(f.users.created) != 0 {
		t.Errorf("a conflicting registration still created %d users", len(f.users.created))
	}
}

// Domain ownership is proven but the DID itself does not resolve, so there is no
// handle or PDS to register it under. That is the caller's problem, not a server
// fault: 400, not 500.
func TestRegister_RefusesADIDTheDirectoryDoesNotKnow(t *testing.T) {
	const unknownDID = "did:plc:neverpublished"

	f := newRegisterFixture(t, wellKnownServing(unknownDID))

	rec := f.register(t, unknownDID, f.domain)
	assertRegisterError(t, rec, http.StatusBadRequest, "DIDResolutionFailed")

	if len(f.users.created) != 0 {
		t.Errorf("an unresolvable DID still registered %d users", len(f.users.created))
	}
}

// Everything that must be refused before the handler makes a single outbound
// request. Each case is answered with the same code, so the table is about
// coverage of the shapes rather than about distinguishing them.
func TestRegister_RejectsMalformedRequests(t *testing.T) {
	tests := []struct {
		name   string
		did    string
		domain string
	}{
		{name: "no DID", did: "", domain: "example.com"},
		{name: "not a DID at all", did: "not-a-did", domain: "example.com"},
		{name: "missing the did: prefix", did: "plc:test123", domain: "example.com"},
		{name: "unsupported DID method", did: "did:key:test123", domain: "example.com"},
		{name: "no domain", did: registrantDID, domain: ""},
		// Whitespace and a bare scheme both normalize to the empty domain, which
		// the handler re-checks after trimming — the reason that second check
		// exists.
		{name: "whitespace domain", did: registrantDID, domain: "   "},
		{name: "scheme with no host", did: registrantDID, domain: "https://"},
		// HTTPS is not decoration here: a .well-known fetched over plaintext
		// proves nothing about who controls the domain.
		{name: "plaintext scheme", did: registrantDID, domain: "http://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRegisterFixture(t, wellKnownServing(registrantDID))

			rec := f.register(t, tt.did, tt.domain)
			assertRegisterError(t, rec, http.StatusBadRequest, "InvalidDID")

			if len(f.users.created) != 0 {
				t.Errorf("an invalid request still registered %d users", len(f.users.created))
			}
		})
	}
}

func TestRegister_RejectsABodyThatIsNotJSON(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	rec := f.send(t, http.MethodPost, []byte(`{"did": `))
	assertRegisterError(t, rec, http.StatusBadRequest, "InvalidDID")
}

// Registration writes; a GET must not reach the body-parsing path at all.
func TestRegister_RejectsAnythingButPOST(t *testing.T) {
	f := newRegisterFixture(t, wellKnownServing(registrantDID))

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := f.send(t, method, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405. Body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
