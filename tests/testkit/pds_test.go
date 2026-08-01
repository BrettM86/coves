//go:build integration

package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCollection is a namespace no consumer subscribes to, so records written
// by this file's tests reach Jetstream (which is what firehose_test.go needs)
// without being indexed by a running AppView.
const testCollection = "social.coves.testkit.record"

// testPDS returns the local PDS, failing the test if it is not there.
//
// Not a skip. testkit's own tests are the proof that the harness works against
// real infrastructure, and a harness whose tests quietly pass when the
// infrastructure is missing proves nothing at all — that is the failure mode
// the whole refactor exists to remove.
func testPDS(t *testing.T) *PDS {
	t.Helper()
	p := NewPDS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Anon.Health(ctx); err != nil {
		t.Fatalf("testkit's PDS tests need the local PDS at %s: %v\n"+
			"  start the stack with 'make dev-up', or run the whole suite through 'make ci'", p.URL(), err)
	}
	return p
}

func TestPDS_CreateAccountIssuesAUsableSession(t *testing.T) {
	p := testPDS(t)

	account := p.CreateAccount(t, WithHandlePrefix("kit"))

	require.NotEmpty(t, account.DID)
	assert.True(t, strings.HasPrefix(account.DID, "did:"), "expected a DID, got %q", account.DID)
	assert.NotEmpty(t, account.AccessToken)
	assert.Equal(t, DefaultPassword, account.Password)
	assert.Contains(t, account.Email, "@")

	// The handle has to fit the PDS' 18-character local-label cap and land in a
	// domain the PDS serves — the two things every hand-rolled generator in the
	// old suite got wrong at least once.
	label, domain, found := strings.Cut(account.Handle, ".")
	require.True(t, found, "handle %q has no domain", account.Handle)
	assert.Equal(t, Endpoints().PDS.HandleDomain, domain)
	assert.LessOrEqual(t, len(label), MaxIDLength, "local label %q exceeds the PDS cap", label)
	assert.True(t, strings.HasPrefix(label, "kit"), "prefix should survive into %q", label)
}

func TestPDS_LoginReopensTheSameAccount(t *testing.T) {
	p := testPDS(t)
	created := p.CreateAccount(t)

	reopened := p.Login(t, created.Handle, created.Password)

	assert.Equal(t, created.DID, reopened.DID)
	assert.Equal(t, created.Handle, reopened.Handle)
	assert.NotEmpty(t, reopened.AccessToken)
}

func TestPDS_LoginWithABadPasswordFailsWithTheReason(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	ft := &fakeT{}
	runIsolated(func() { p.Login(ft, account.Handle, "not-the-password") })

	require.True(t, ft.failed())
	msg := ft.message()
	assert.Contains(t, msg, account.Handle)
	assert.Contains(t, msg, p.URL(), "the failure should say which PDS refused")
	assert.Contains(t, msg, "com.atproto.server.createSession")
	assert.NotContains(t, msg, "not-the-password", "a failure message must not echo a credential")
}

func TestAccount_RecordRoundTrip(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	created := account.CreateRecord(t, testCollection, map[string]any{
		"$type": testCollection,
		"text":  "first",
	})

	require.NotEmpty(t, created.URI)
	require.NotEmpty(t, created.CID)
	assert.Equal(t, testCollection, created.Collection)
	assert.Equal(t, "at://"+account.DID+"/"+testCollection+"/"+created.RKey, created.URI)
	_, err := syntax.ParseTID(created.RKey)
	assert.NoError(t, err, "the generated rkey should be a real TID")

	read := account.GetRecord(t, testCollection, created.RKey)
	assert.Equal(t, created.URI, read.URI)
	assert.Equal(t, created.CID, read.CID)
	assert.Equal(t, "first", read.Value["text"])

	updated := account.PutRecord(t, testCollection, created.RKey, map[string]any{
		"$type": testCollection,
		"text":  "second",
	})
	assert.Equal(t, created.URI, updated.URI)
	assert.NotEqual(t, created.CID, updated.CID, "a changed record must have a new CID")
	assert.Equal(t, "second", account.GetRecord(t, testCollection, created.RKey).Value["text"])

	account.DeleteRecord(t, testCollection, created.RKey)

	// Absence is asserted through the error-returning client: the helpers fail
	// the test on error, which is the wrong shape for "this should be gone".
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = account.XRPC().Query(ctx, "com.atproto.repo.getRecord", recordParams(account.DID, testCollection, created.RKey), nil)
	require.Error(t, err)
	assert.True(t, IsNotFound(err), "a deleted record should read as not-found, got %v", err)
}

func TestAccount_CreateRecordAcceptsAnExplicitRKey(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	rkey := UniqueID(t)
	created := account.CreateRecord(t, testCollection, map[string]any{"$type": testCollection}, WithRKey(rkey))

	assert.Equal(t, rkey, created.RKey)
	assert.Equal(t, created.URI, account.GetRecord(t, testCollection, rkey).URI)
}

func TestAccount_CreateRecordFailureNamesTheRecord(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)
	rkey := UniqueID(t)
	account.CreateRecord(t, testCollection, map[string]any{"$type": testCollection}, WithRKey(rkey))

	// Creating the same key twice is a conflict, which is a convenient way to
	// make a real PDS reject a real write.
	ft := &fakeT{}
	runIsolated(func() {
		account.CreateRecord(ft, testCollection, map[string]any{"$type": testCollection}, WithRKey(rkey))
	})

	require.True(t, ft.failed())
	assert.Contains(t, ft.message(), testCollection)
	assert.Contains(t, ft.message(), rkey)
}

func TestAccount_UploadBlobRoundTripsIntoARecord(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	png := TestPNG(64, 48)
	ref := account.UploadBlob(t, png, "image/png")

	assert.Equal(t, "blob", ref.Type)
	assert.Equal(t, "image/png", ref.MimeType)
	assert.Equal(t, int64(len(png)), ref.Size)
	assert.NotEmpty(t, ref.CID())

	// The reference has to survive being embedded in a record and read back:
	// testkit declares its own BlobRef (it may not import internal/core/blobs),
	// so the JSON shape is the thing actually under test here.
	created := account.CreateRecord(t, testCollection, map[string]any{
		"$type": testCollection,
		"image": ref,
	})
	value := account.GetRecord(t, testCollection, created.RKey).Value

	embedded, ok := value["image"].(map[string]any)
	require.True(t, ok, "the blob did not survive as an object: %#v", value["image"])
	assert.Equal(t, "blob", embedded["$type"])
	assert.Equal(t, "image/png", embedded["mimeType"])
	link, ok := embedded["ref"].(map[string]any)
	require.True(t, ok, "the blob ref lost its $link wrapper: %#v", embedded["ref"])
	assert.Equal(t, ref.CID(), link["$link"])
}

func TestAccount_UploadBlobRejectsAWildcardMIMEType(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Rejected locally, before any request: a PDS enforcing the granular
	// blob:*/* scope answers a wildcard content type with a scope error that
	// never mentions content types.
	err := account.XRPC().Upload(ctx, "com.atproto.repo.uploadBlob", "*/*", TestPNG(8, 8), nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "concrete MIME type")
}

func TestPDS_WaitHealthy(t *testing.T) {
	p := testPDS(t)
	p.WaitHealthy(t, 10*time.Second)
}

func TestAccount_PutRecordHonoursASwapCID(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	created := account.CreateRecord(t, testCollection, map[string]any{"$type": testCollection, "text": "v1"})
	updated := account.PutRecord(t, testCollection, created.RKey,
		map[string]any{"$type": testCollection, "text": "v2"}, WithSwapRecord(created.CID))
	require.NotEqual(t, created.CID, updated.CID)

	// The stale CID now names a version that is no longer current, which is the
	// lost-update the compare-and-swap exists to refuse. An option that was
	// accepted and ignored would make this write succeed.
	ft := &fakeT{}
	runIsolated(func() {
		account.PutRecord(ft, testCollection, created.RKey,
			map[string]any{"$type": testCollection, "text": "v3"}, WithSwapRecord(created.CID))
	})
	require.True(t, ft.failed(), "a write against a stale CID must be refused")
	assert.Contains(t, ft.message(), created.RKey)

	assert.Equal(t, "v2", account.GetRecord(t, testCollection, created.RKey).Value["text"],
		"the refused write must not have landed")
}

func TestAccount_InapplicableRecordOptionsFailLoudly(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	// Silently ignoring either of these would leave a test believing it proved
	// something it never asked the PDS to do.
	ft := &fakeT{}
	runIsolated(func() {
		account.CreateRecord(ft, testCollection, map[string]any{"$type": testCollection},
			WithSwapRecord("bafyreiabc"))
	})
	require.True(t, ft.failed())
	assert.Contains(t, ft.message(), "WithSwapRecord does not apply to CreateRecord")

	ft = &fakeT{}
	runIsolated(func() {
		account.PutRecord(ft, testCollection, "the-argument",
			map[string]any{"$type": testCollection}, WithRKey("the-option"))
	})
	require.True(t, ft.failed())
	assert.Contains(t, ft.message(), "WithRKey does not apply to PutRecord")
	assert.Contains(t, ft.message(), "the-option")
}

// TestAccount_DeleteExistingRecordCatchesTheSilentNoOp covers the trap that
// makes a delete look successful when it deleted nothing.
//
// com.atproto.repo.deleteRecord answers 200 for a key that was never there, and
// commits nothing — so the firehose emits nothing, and a test that pairs the
// delete with an Await waits out its whole timeout blaming the pipeline.
func TestAccount_DeleteExistingRecordCatchesTheSilentNoOp(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)

	// The plain delete is happy to do nothing at all.
	account.DeleteRecord(t, testCollection, "never-existed")

	ft := &fakeT{}
	runIsolated(func() { account.DeleteExistingRecord(ft, testCollection, "never-existed") })
	require.True(t, ft.failed(), "deleting a record that is not there should be a failure, not a no-op")
	assert.Contains(t, ft.message(), "never-existed")

	// And it still deletes what is there.
	created := account.CreateRecord(t, testCollection, map[string]any{"$type": testCollection})
	account.DeleteExistingRecord(t, testCollection, created.RKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := account.XRPC().Query(ctx, "com.atproto.repo.getRecord",
		recordParams(account.DID, testCollection, created.RKey), nil)
	assert.True(t, IsNotFound(err), "the record should be gone, got %v", err)
}

func TestAccount_RefreshSessionSwapsBothTokens(t *testing.T) {
	p := testPDS(t)
	account := p.CreateAccount(t)
	originalAccess, originalRefresh := account.AccessToken, account.RefreshToken

	account.RefreshSession(t)

	assert.NotEmpty(t, account.AccessToken)
	assert.NotEmpty(t, account.RefreshToken)
	assert.NotEqual(t, originalRefresh, account.RefreshToken,
		"refreshSession spends the refresh token; keeping the old one would fail the next call")

	// The client has to move with the tokens, or the next write would go out
	// under the previous session.
	created := account.CreateRecord(t, testCollection, map[string]any{"$type": testCollection})
	assert.NotEmpty(t, created.URI)
	_ = originalAccess
}

// TestAccount_DoesNotPrintItsCredentials guards every fmt verb, because the one
// that leaks is the one nobody thought to check.
func TestAccount_DoesNotPrintItsCredentials(t *testing.T) {
	account := &Account{
		Handle:       "alice.local.coves.dev",
		DID:          "did:plc:alice",
		Email:        "alice@test.com",
		Password:     "super-secret-password",
		AccessToken:  "access-token-value",
		RefreshToken: "refresh-token-value",
	}
	secrets := []string{"super-secret-password", "access-token-value", "refresh-token-value"}

	for _, format := range []string{"%v", "%+v", "%s", "%#v", "%q"} {
		rendered := fmt.Sprintf(format, account)
		for _, secret := range secrets {
			assert.NotContains(t, rendered, secret,
				"%s leaked a credential into what may become CI output", format)
		}
		// Still useful: the identity a reader actually needs is all there.
		if format != "%q" {
			assert.Contains(t, rendered, "alice.local.coves.dev", "%s should still identify the account", format)
			assert.Contains(t, rendered, "REDACTED", "%s should say something was withheld", format)
		}
	}

	var missing *Account
	assert.Equal(t, "<nil>", fmt.Sprintf("%v", missing))
}

// ---------------------------------------------------------------------------
// TIDs
// ---------------------------------------------------------------------------

func TestTID_IsAValidStrictlyIncreasingTID(t *testing.T) {
	const n = 2000
	previous := ""
	for i := 0; i < n; i++ {
		tid := TID()
		require.Len(t, tid, 13, "a TID is 13 characters")
		_, err := syntax.ParseTID(tid)
		require.NoError(t, err, "TID() produced %q", tid)
		// Strictly increasing AND lexicographically sortable: rkeys order
		// records in a repo listing, so a TID that only increases numerically
		// would sort wrongly.
		require.Greater(t, tid, previous, "TIDs must increase; %q followed %q", tid, previous)
		previous = tid
	}
}

func TestTID_IsSafeUnderConcurrency(t *testing.T) {
	const goroutines, each = 8, 200
	results := make(chan string, goroutines*each)
	done := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < each; j++ {
				results <- TID()
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	close(results)

	seen := map[string]bool{}
	for tid := range results {
		require.False(t, seen[tid], "TID %q was issued twice", tid)
		seen[tid] = true
	}
	assert.Len(t, seen, goroutines*each)
}

// ---------------------------------------------------------------------------
// Account creation against a PDS the test controls
// ---------------------------------------------------------------------------
//
// These cover what a real PDS cannot be asked for on demand: a malformed
// session response, and the exact request body sent for options whose effect is
// otherwise invisible.

// capturingPDS records the createAccount payload it was sent and answers with a
// canned session.
func capturingPDS(t *testing.T, respond func(http.ResponseWriter)) (*PDS, func() map[string]any) {
	t.Helper()
	var captured atomic.Pointer[map[string]any]
	stub := newStubService(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			captured.Store(&body)
		}
		respond(w)
	})
	return NewPDS(t, WithPDSURL(stub.URL), WithPDSHandleDomain("stub.test")),
		func() map[string]any {
			if got := captured.Load(); got != nil {
				return *got
			}
			return nil
		}
}

func validSession(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"did": "did:plc:stubbed", "handle": "stubbed.stub.test",
		"accessJwt": "access-jwt", "refreshJwt": "refresh-jwt",
	})
}

func TestPDS_WithHandleBypassesGeneration(t *testing.T) {
	pds, payload := capturingPDS(t, validSession)

	// Deliberately over the 18-character budget: WithHandle is the escape hatch
	// for tests that are ABOUT the handle, so it must pass the value through
	// untouched rather than sanitising it into something that no longer tests
	// what was asked.
	const handle = "a-very-long-local-label-indeed.stub.test"
	pds.CreateAccount(t, WithHandle(handle), WithEmail("someone@test.com"), WithPassword("pw"))

	body := payload()
	require.NotNil(t, body)
	assert.Equal(t, handle, body["handle"])
	assert.Equal(t, "someone@test.com", body["email"])
	assert.Equal(t, "pw", body["password"])
	assert.NotContains(t, body, "inviteCode", "an absent invite code must not be sent as an empty string")
}

func TestPDS_WithInviteCodeIsSent(t *testing.T) {
	pds, payload := capturingPDS(t, validSession)

	pds.CreateAccount(t, WithInviteCode("stub-invite-1234"))

	body := payload()
	require.NotNil(t, body)
	assert.Equal(t, "stub-invite-1234", body["inviteCode"])
	// The generated handle still lands in the configured domain.
	assert.Contains(t, body["handle"], ".stub.test")
}

func TestPDS_GeneratedEmailFollowsTheHandle(t *testing.T) {
	pds, payload := capturingPDS(t, validSession)

	account := pds.CreateAccount(t, WithHandlePrefix("trace"))

	body := payload()
	require.NotNil(t, body)
	// Derived from the handle that was REQUESTED — the stub answers with a
	// canned handle of its own, and the account carries that, so the request is
	// the only place the generated pair can be compared.
	requested, ok := body["handle"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(requested, "trace"), "the prefix should reach the PDS: %q", requested)
	assert.Equal(t, handleLabel(requested)+"@test.com", body["email"])
	assert.Equal(t, account.Email, body["email"])
}

func TestPDS_RejectsAMalformedSessionResponse(t *testing.T) {
	// A 200 whose body is not a session. Accepting it would send an
	// unauthenticated client into the next twenty lines of the test, where it
	// fails as a 401 on something unrelated.
	for name, respond := range map[string]func(http.ResponseWriter){
		"no did": func(w http.ResponseWriter) {
			writeJSON(w, http.StatusOK, map[string]any{"accessJwt": "a", "refreshJwt": "r"})
		},
		"no access token": func(w http.ResponseWriter) {
			writeJSON(w, http.StatusOK, map[string]any{"did": "did:plc:x", "refreshJwt": "r"})
		},
		"no refresh token": func(w http.ResponseWriter) {
			writeJSON(w, http.StatusOK, map[string]any{"did": "did:plc:x", "accessJwt": "a"})
		},
		"empty body": func(w http.ResponseWriter) {
			writeJSON(w, http.StatusOK, map[string]any{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			pds, _ := capturingPDS(t, respond)
			ft := &fakeT{}
			runIsolated(func() { pds.CreateAccount(ft) })
			require.True(t, ft.failed(), "a session missing a credential must not be accepted")
			assert.Contains(t, ft.message(), "testkit: creating account")
		})
	}
}

// ---------------------------------------------------------------------------
// The factory adapter
// ---------------------------------------------------------------------------

// fakeClient stands in for internal/atproto/pds.Client, which testkit may not
// import. The real assignability — that PasswordAuthFactory's return type
// satisfies votes.PDSClientFactory and its four siblings — is exercised at the
// call sites in phase 3, where importing a domain package is legal.
type fakeClient struct {
	host, did, token string
}

func newFakeClient(host, did, token string) (*fakeClient, error) {
	return &fakeClient{host: host, did: did, token: token}, nil
}

func TestPasswordAuthFactory_PassesTheSessionThrough(t *testing.T) {
	factory := PasswordAuthFactory(newFakeClient)

	did, err := syntax.ParseDID("did:plc:abc123")
	require.NoError(t, err)
	client, err := factory(context.Background(), &oauth.ClientSessionData{
		AccountDID:  did,
		AccessToken: "token-abc",
		HostURL:     "http://pds.test",
	})

	require.NoError(t, err)
	assert.Equal(t, "http://pds.test", client.host)
	assert.Equal(t, "did:plc:abc123", client.did)
	assert.Equal(t, "token-abc", client.token)
}

func TestPasswordAuthFactory_PropagatesAConstructorRejection(t *testing.T) {
	// The real constructor validates its arguments too. A factory that
	// swallowed that error would hand back a nil client and move the failure to
	// whichever line first used it.
	failing := func(host, did, token string) (*fakeClient, error) {
		return nil, fmt.Errorf("constructor refused host %q", host)
	}
	factory := PasswordAuthFactory(failing)

	did, err := syntax.ParseDID("did:plc:abc123")
	require.NoError(t, err)
	client, err := factory(context.Background(), &oauth.ClientSessionData{
		AccountDID: did, AccessToken: "token-abc", HostURL: "http://pds.test",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "constructor refused host")
	assert.Nil(t, client)
}

func TestPasswordAuthFactory_RejectsUnusableSessions(t *testing.T) {
	did, err := syntax.ParseDID("did:plc:abc123")
	require.NoError(t, err)

	for name, session := range map[string]*oauth.ClientSessionData{
		"no session":      nil,
		"no access token": {AccountDID: did, HostURL: "http://pds.test"},
		"no host":         {AccountDID: did, AccessToken: "token-abc"},
	} {
		t.Run(name, func(t *testing.T) {
			factory := PasswordAuthFactory(newFakeClient)
			client, err := factory(context.Background(), session)
			require.Error(t, err, "an unusable session must not produce a client")
			assert.Nil(t, client)
		})
	}
}

// recordParams builds the query a repo read takes.
func recordParams(repo, collection, rkey string) url.Values {
	return url.Values{"repo": {repo}, "collection": {collection}, "rkey": {rkey}}
}
