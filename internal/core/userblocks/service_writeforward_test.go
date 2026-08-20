//go:build integration

package userblocks_test

import (
	"Coves/internal/atproto/pds"
	"Coves/internal/core/userblocks"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
	"context"
	"net/url"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What blocking and unblocking a user actually write to a repo.
//
// This is what survives of tests/integration/userblock_e2e_test.go. That file
// billed itself as a full pipeline proof — "Client -> XRPC -> PDS Write ->
// Jetstream -> Consumer -> AppView" — but the Jetstream half was a JetstreamEvent
// the test hand-built and fed to a consumer the test constructed, so it proved
// the consumer against a fixture rather than against the firehose. Those events
// are now internal/atproto/jetstream/user_consumer_block_test.go, where they are
// honestly labelled, and the real pipeline is a tests/e2e contract.
//
// What only a real PDS can show is here: the block is a RECORD in the blocker's
// own repository, and unblocking removes it. Everything upstream of that — the
// self-block guard, handle resolution, the PDS conflict paths — is covered
// against mocks in service_test.go, because those decisions are made before the
// PDS is ever called.
//
// # WHY THE REPO THE RECORD LANDS IN IS THE POINT
//
// A block written into the wrong repository still returns a URI and a CID, and
// the AppView would still index it: the consumer takes the blocker from the
// commit's repo DID, so a record in the BLOCKED user's repo indexes as that user
// blocking themselves. Nothing downstream can detect the mistake, because
// everything downstream is fed by the record. Only reading it back can.

// writeForwardFixture is the service under test, wired the way production wires
// it except for the PDS client factory: password auth instead of OAuth/DPoP, so
// a test can hold a session without an authorization-code flow.
type writeForwardFixture struct {
	service userblocks.Service
	repo    userblocks.Repository
	blocker *testkit.Account
	blocked *testkit.Account
	session *oauth.ClientSessionData
}

func newWriteForwardFixture(t *testing.T) *writeForwardFixture {
	t.Helper()

	repo := postgres.NewUserBlockRepository(testkit.DB(t))
	pdsServer := testkit.NewPDS(t)

	// Both parties are real accounts. The blocked user's repo is never written
	// to, but a subject that names an account the PDS actually knows is what
	// makes "the record went into the blocker's repo" a claim with two possible
	// answers rather than one.
	blocker := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("ubb"))
	blocked := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("ubt"))

	did, err := syntax.ParseDID(blocker.DID)
	require.NoError(t, err)

	// The handle resolver is nil: every identifier these tests pass is already a
	// DID, and resolveIdentifier short-circuits before touching it. Handle
	// resolution is service_test.go's subject.
	service := userblocks.NewServiceWithPDSFactory(repo, nil,
		testkit.PasswordAuthFactory(pds.NewFromAccessToken, pds.PrivateHostOptions(true)...))

	return &writeForwardFixture{
		service: service,
		repo:    repo,
		blocker: blocker,
		blocked: blocked,
		session: &oauth.ClientSessionData{
			AccountDID:  did,
			SessionID:   "write-forward-test",
			HostURL:     pdsServer.URL(),
			AccessToken: blocker.AccessToken,
		},
	}
}

// getRecordErr asks the PDS for a record and returns only the error, so a test
// can assert a record's ABSENCE — Account.GetRecord fails the test on a missing
// record, which is the right default and the wrong tool here.
func getRecordErr(ctx context.Context, account *testkit.Account, collection, rkey string) error {
	return account.XRPC().Query(ctx, "com.atproto.repo.getRecord", url.Values{
		"repo":       {account.DID},
		"collection": {collection},
		"rkey":       {rkey},
	}, nil)
}

// rkeyOf returns the record key an AT-URI ends with.
func rkeyOf(t *testing.T, uri string) string {
	t.Helper()
	parsed, err := syntax.ParseATURI(uri)
	require.NoErrorf(t, err, "the service returned an unparseable record URI %q", uri)
	rkey := parsed.RecordKey().String()
	require.NotEmptyf(t, rkey, "the record URI %q has no record key", uri)
	return rkey
}

func TestService_BlockWritesABlockRecordToTheBlockersRepo(t *testing.T) {
	t.Parallel()

	f := newWriteForwardFixture(t)

	result, err := f.service.BlockUser(context.Background(), f.session, f.blocked.DID)
	require.NoError(t, err)
	require.NotEmpty(t, result.RecordCID, "the CID is what pins the version a client just wrote")

	rkey := rkeyOf(t, result.RecordURI)
	assert.Equal(t, "at://"+f.blocker.DID+"/social.coves.actor.block/"+rkey, result.RecordURI,
		"the block record belongs to the blocker, not the blocked user")

	record := f.blocker.GetRecord(t, "social.coves.actor.block", rkey)
	assert.Equal(t, "social.coves.actor.block", record.Value["$type"])
	assert.Equal(t, f.blocked.DID, record.Value["subject"],
		"atProto convention: the blocked user is referenced by a subject field")
	assert.NotEmpty(t, record.Value["createdAt"])
}

func TestService_UnblockDeletesTheBlockRecord(t *testing.T) {
	t.Parallel()

	f := newWriteForwardFixture(t)
	ctx := context.Background()

	result, err := f.service.BlockUser(ctx, f.session, f.blocked.DID)
	require.NoError(t, err)
	rkey := rkeyOf(t, result.RecordURI)

	// Unblock finds the record key through the AppView's index, so the block has
	// to be indexed first — which the firehose would have done by now in
	// production, and which this test does directly because the consumer is not
	// what is under test.
	_, err = f.repo.BlockUser(ctx, &userblocks.UserBlock{
		BlockerDID: f.blocker.DID,
		BlockedDID: f.blocked.DID,
		RecordURI:  result.RecordURI,
		RecordCID:  result.RecordCID,
	})
	require.NoError(t, err)

	require.NoError(t, f.service.UnblockUser(ctx, f.session, f.blocked.DID))

	assert.True(t, testkit.IsNotFound(getRecordErr(ctx, f.blocker, "social.coves.actor.block", rkey)),
		"the block record is still in the blocker's repo after unblocking")
}
