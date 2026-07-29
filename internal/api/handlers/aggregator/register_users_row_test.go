//go:build integration

package aggregator

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The one registration test that keeps a database, and the reason it exists is
// narrow enough to state exactly.
//
// register_test.go proves the whole endpoint against a fake user service: which
// requests are refused, and which CreateUserRequest the handler builds when one
// is not. What a fake cannot show is that the request the handler builds is one
// the users table will actually accept. Registration is the aggregator domain's
// only synchronous write — every other row in the domain arrives over the
// firehose — and it is the row a later aggregator post depends on
// (posts.author_did references users.did, migrations/011), so "the handler
// called CreateUser with the right arguments" and "the aggregator now exists as
// far as the rest of the product is concerned" are different claims. This makes
// the second one.
//
// It is one test rather than a mirror of the T0 suite on purpose: every refusal
// path returns before the service is reached, so running it against a real
// repository would prove nothing a fake has not already proven, at the cost of
// a database clone each.
func TestRegister_WritesTheAggregatorIntoTheUsersTable(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)

	// A run-scoped DID, because the assertion below is that THIS registration
	// created the row: a fixed literal would be satisfied by a leftover.
	//
	// The handle is a real-looking one under a real TLD, which is not
	// decoration: the users service validates handle syntax on the way in and
	// rejects the reserved TLDs (.example, .invalid, .local), so a handle that
	// reads fine in a fake-service test fails here with a 500. That difference
	// is most of why this test is worth a database.
	did := "did:plc:" + testkit.UniqueIDWithPrefix(t, "agg")
	handle := "aggregator-" + testkit.UniqueID(t) + ".test.coves.dev"
	pdsURL := "https://pds.example.invalid"

	stub := httptest.NewTLSServer(wellKnownServing(did))
	t.Cleanup(stub.Close)

	// The resolver is still a fake: what it stands in for is the PLC directory,
	// which the suite may never touch (§3.7). Its job here is to be the ONLY
	// source of the handle and PDS URL, so that a handler taking either from
	// the request body would write something this test can see is wrong.
	resolver := &fakeIdentityResolver{identities: map[string]*identity.Identity{
		did: {DID: did, Handle: handle, PDSURL: pdsURL, ResolvedAt: time.Now(), Method: identity.MethodHTTPS},
	}}

	userRepo := postgres.NewUserRepository(db)
	// The real service over the real repository. defaultPDS is what a signup
	// would provision against and is irrelevant to this path; nil turnstile and
	// an empty admin password are likewise unreachable from registration, which
	// creates no PDS account of its own.
	userService := users.NewUserService(userRepo, resolver, pdsURL, nil, "")

	handler := NewRegisterHandler(userService, resolver)
	handler.SetHTTPClient(stub.Client())

	body, err := json.Marshal(RegisterRequest{
		DID: did,
		// The client sends a bare host:port, and the handler is what turns it
		// into an https:// URL.
		Domain: strings.TrimPrefix(stub.URL, "https://"),
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.aggregator.register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.HandleRegister(rec, req)

	require.Equalf(t, http.StatusOK, rec.Code, "registration failed: %s", rec.Body.String())

	stored, err := userRepo.GetByDID(context.Background(), did)
	require.NoError(t, err,
		"registration answered 200 and the users table has no row for the DID: the aggregator "+
			"cannot author a post, because posts.author_did references users.did")
	require.Equal(t, did, stored.DID)
	require.Equal(t, handle, stored.Handle,
		"the handle must come from DID resolution, not from anything the caller sent")
	require.Equal(t, pdsURL, stored.PDSURL,
		"the PDS URL must come from DID resolution: it is where the AppView will later fetch "+
			"this aggregator's records from")
}
