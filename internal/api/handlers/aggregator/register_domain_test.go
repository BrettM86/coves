package aggregator

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/tests/domaincorpus"

	"github.com/stretchr/testify/require"
)

// Registration takes a domain from an unauthenticated caller and builds a URL
// out of it — `https://` + the domain + `/.well-known/atproto-did` — and until
// this change the only thing it checked was that the domain was not the empty
// string. String concatenation into a URL is the whole vulnerability: every
// part of a URL that comes after the host can be smuggled in through the host,
// because the parser does not know the concatenation was supposed to stop.
// Three shapes were confirmed against the handler:
//
//	internal-admin/v1/secrets?x=y#   fetches /v1/secrets?x=y from internal-admin
//	evil.com@internal-host           fetches from internal-host; evil.com is userinfo
//	127.0.0.1:5432                   fetches from a port on the loopback interface
//
// The trailing `#` in the first is what makes this more than an SSRF to one
// fixed path: it turns `/.well-known/atproto-did` into a fragment, which is
// never sent, so the attacker chooses the path AND the query as well as the
// host. Rate limiting is the only other gate and no credential is needed.
//
// The fix these tests pin is a positive allowlist on DNS shape rather than a
// blocklist on addresses; validation.NormalizeDomain's doc comment says why a
// blocklist cannot work here. The tests come in three kinds, and the third is
// the one that would be missing if this were written carelessly:
//
//  1. what the validator refuses,
//  2. what it must go on accepting,
//  3. that the handler consults it BEFORE it touches an HTTP client.
//
// Without (3) an implementation that validates after building and sending the
// request satisfies (1) and (2) completely, while having already resolved and
// dialled the host the attacker named. The refusal is only worth anything if it
// happens first.
//
// # (1) AND (2) HAVE MOVED; (3) IS WHAT STAYS HERE
//
// The validator is being promoted out of this package so the community
// consumer can reach it — its `.well-known/did.json` fetch builds a URL by
// concatenating a federated domain the same way this handler did, and could not
// call `normalizeDomain` because it was unexported here. So the predicate's own
// tables now live in internal/validation, driven from tests/domaincorpus.
//
// What cannot move is (3), because it is a fact about THIS HANDLER: that it
// consults the validator before it reaches for a client. Testing the handler's
// behaviour rather than the private function is also what makes this file
// survive the delegation — it asserts the same thing before and after
// `normalizeDomain` becomes a call into the shared package.


// recordingTransport answers nothing and remembers whether it was asked to.
//
// A RoundTripper rather than a dialer, because RoundTrip is the earliest moment
// the handler can reach the network: http.Client.Do calls it before any name is
// resolved, so a call recorded here means the attacker's host was already on its
// way out whether or not a packet followed. Recording the URL as well is what
// makes a failure readable — the message names the request that would have been
// sent, which for the fragment payload is a path nobody wrote down anywhere.
//
// Plain fields, no mutex: http.Client.Do calls RoundTrip on the caller's
// goroutine, and the handler under test is driven synchronously.
type recordingTransport struct {
	requested []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requested = append(r.requested, req.URL.String())
	return nil, errors.New("recordingTransport never answers: the handler should not have reached it")
}

// newRecordingHandler builds the handler with an HTTP client that cannot talk to
// anything and says so afterwards.
func newRecordingHandler(t *testing.T) (*RegisterHandler, *recordingTransport) {
	t.Helper()

	transport := &recordingTransport{}
	userService := &fakeUserService{registered: map[string]*users.User{}}
	resolver := &fakeIdentityResolver{identities: map[string]*identity.Identity{
		registrantDID: {DID: registrantDID, Handle: registrantHandle, PDSURL: registrantPDS},
	}}

	handler := NewRegisterHandler(userService, resolver)
	handler.setHTTPClient(&http.Client{Transport: transport})
	return handler, transport
}

func postRegister(t *testing.T, handler *RegisterHandler, did, domain string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(RegisterRequest{DID: did, Domain: domain})
	require.NoError(t, err, "marshalling the request")

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.HandleRegister(rec, req)
	return rec
}

// THE ORDERING TEST.
//
// Every case below is one the handler currently sends: each parses as a URL, so
// http.NewRequestWithContext succeeds and Do is reached. That is deliberate —
// an input Go's URL parser rejects would never touch the transport even with no
// validation at all, and would prove nothing here.
//
// What this asserts that no other test in this file can: the refusal happens
// before the client. An implementation that validates the domain after building
// and sending the request passes the whole rejection table and still resolves
// and dials whatever the caller named, which for an SSRF is the entire damage.
func TestRegister_ValidatesTheDomainBeforeTouchingTheNetwork(t *testing.T) {
	for _, tt := range domaincorpus.InjectionPayloads() {
		t.Run(tt.Name, func(t *testing.T) {
			handler, transport := newRecordingHandler(t)

			rec := postRegister(t, handler, registrantDID, tt.Domain)

			require.Emptyf(t, transport.requested,
				"the handler asked its HTTP client for %q before refusing the domain %q. "+
					"Validation must run first: by the time the client is called the host has "+
					"been chosen by the caller, and resolving it is already the SSRF",
				transport.requested, tt.Domain)

			// The status is a second, weaker signal, and it is here to say which
			// mechanism refused. A 401 DomainVerificationFailed means the fetch
			// was attempted and failed; only a 400 means the request never left.
			require.Equalf(t, http.StatusBadRequest, rec.Code,
				"domain %q must be refused as a malformed request, not as a failed verification. Body: %s",
				tt.Domain, rec.Body.String())
			var body XRPCError
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body is not valid JSON")
			require.Equalf(t, "InvalidDID", body.Error, "error code for domain %q", tt.Domain)
		})
	}

	// THE CONTROL, and without it every row above is unfalsifiable.
	//
	// The whole table asserts that a recorder stayed EMPTY. An empty recorder is
	// also what you get when the recorder was never installed — and
	// setHTTPClient is the only thing that installs it. Mutation-proven: make
	// setHTTPClient ignore its argument and every row above stays green, while
	// the handler quietly makes real outbound requests through its own default
	// client for the whole run.
	//
	// So one row has to drive a domain that PASSES validation and assert the
	// recorder was reached. That is the only assertion in this file that can
	// tell "the handler refused before the client" from "the client under
	// observation was not the client the handler uses".
	t.Run("control: a valid domain does reach the injected client", func(t *testing.T) {
		handler, transport := newRecordingHandler(t)

		rec := postRegister(t, handler, registrantDID, stubDomain)

		require.NotEmptyf(t, transport.requested,
			"a domain that passes validation (%q) never reached the recording transport. The recorder is "+
				"what every row above asserts on, so if the handler is not using it those rows prove nothing "+
				"— they would pass just as well against a handler that sends every request through a client "+
				"this test cannot see. Either validation is refusing a legitimate hostname, or setHTTPClient "+
				"is not installing the client it is given",
			stubDomain)
		require.Equalf(t, []string{"https://" + stubDomain + wellKnownPath}, transport.requested,
			"the handler asked for %v; the .well-known fetch must go to the domain the caller named, over "+
				"HTTPS, at the well-known path and nothing else", transport.requested)

		// recordingTransport never answers, so verification fails — which is the
		// 401 branch, and its presence here proves the request was attempted
		// rather than short-circuited.
		require.Equalf(t, http.StatusUnauthorized, rec.Code,
			"a domain that reached the transport must fail verification (the recorder answers nothing), "+
				"not be refused as malformed. Body: %s", rec.Body.String())
	})
}

// The handler normalises before it validates, and that ordering is load-bearing
// in both directions.
//
// `https://example.com` is refused by normalizeDomain — it is not a hostname,
// it is a URL — and yet registration has always accepted it, because the
// handler strips the scheme first. Both facts are true at once and neither is
// an accident: the validator is a predicate about hostnames with no opinion
// about what a client might wrap one in, and the handler is where a small,
// enumerated set of client sloppiness is unwrapped. Nothing dangerous survives
// the unwrapping, because validation still runs on the result —
// `https://evil.com@internal-host` strips to `evil.com@internal-host` and is
// refused.
//
// This is here so that the GREEN change does not delete the trimming as
// redundant. Removing it would break clients that have always been allowed to
// send a scheme, and no other test in the file would notice.
func TestRegister_AcceptsTheClientSloppinessItAlwaysHas(t *testing.T) {
	tests := []struct {
		name   string
		domain string
	}{
		{name: "a bare hostname", domain: stubDomain},
		{name: "an https scheme the handler strips", domain: "https://" + stubDomain},
		{name: "a trailing slash the handler strips", domain: stubDomain + "/"},
		{name: "surrounding whitespace the handler trims", domain: "  " + stubDomain + "  "},
		{name: "uppercase, which DNS does not distinguish", domain: "EXAMPLE.COM"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRegisterFixture(t, wellKnownServing(registrantDID))

			rec := f.register(t, registrantDID, tt.domain)

			require.Equalf(t, http.StatusOK, rec.Code,
				"domain %q used to register successfully and must go on doing so; the new validation "+
					"is meant to refuse hosts the AppView should not fetch, not spellings clients "+
					"have always been allowed to send. Body: %s",
				tt.domain, rec.Body.String())
			require.Lenf(t, f.users.created, 1,
				"domain %q answered 200 without registering the aggregator", tt.domain)
		})
	}
}
