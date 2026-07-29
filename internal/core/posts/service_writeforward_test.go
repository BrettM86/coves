//go:build integration

package posts_test

import (
	"context"

	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What creating and deleting a post actually does to the community's repo, and
// who is allowed to do it.
//
// This is the client-write half of the post domain: the half that
// tests/e2e/post_contract_test.go structurally cannot reach. §3.4b of
// docs/TEST_ARCHITECTURE.md records why — RequireAuth accepts only a sealed
// session token, minted nowhere but the browser OAuth callback, so T2 can prove
// that the write endpoints refuse an unauthenticated client and nothing beyond
// it. Authenticated write BEHAVIOUR is therefore proven here, against a real
// PDS, and these are the assertions that used to live in
// tests/integration/post_e2e_test.go's "Write-Forward to PDS" and
// tests/integration/post_delete_test.go's authorization tests.
//
// # WHAT MAKES THE REPO THE INTERESTING PART
//
// A post record does not live in its author's repo. It lives in the COMMUNITY's
// repo, written with the community's own PDS credentials, carrying an `author`
// field that names the human who wrote it (internal/core/posts/service.go step
// 9, and the reason the Jetstream consumer's first security check is
// repoDID == record.community).
//
// That makes two things testable only from the PDS side. First, that the
// service really writes into the community's repository rather than the
// caller's — a post written to the author's repo would be rejected outright by
// the consumer, so the AppView would simply never index it and every test that
// only reads the service's return value would still pass. Second, that deletion
// is authorized against the RECORD's author field rather than against anything
// the caller supplies: DeletePost fetches the record from the PDS specifically
// to read `author` out of it, because the community's credentials would happily
// delete anyone's post.

const (
	// The instance identity these tests provision communities under. It matches
	// the AppView's default (internal/config: INSTANCE_DID defaults to
	// did:web:coves.social), and the domain must be one of the PDS'
	// PDS_SERVICE_HANDLE_DOMAINS or account creation is refused.
	instanceDID    = "did:web:coves.social"
	instanceDomain = "coves.social"

	postCollection = "social.coves.community.post"
)

// postFixture is the post service wired the way cmd/server wires it, over a
// real community that owns a real PDS repository.
type postFixture struct {
	service   posts.Service
	pds       *testkit.PDS
	community *communities.Community
	author    *testkit.Account
}

// newPostFixture provisions a community on the test PDS and returns the post
// service pointed at it.
//
// The community is provisioned through communities.CreateCommunity rather than
// seeded into the index, because unlike the subscribe/block write-forwards this
// is a write into the COMMUNITY's repo: it needs an account that exists on the
// PDS and credentials the AppView can use, and both are what provisioning
// produces. The optional collaborators (aggregators, blobs, unfurl, bluesky)
// are nil — every one of them is a branch on the record's contents, and what is
// under test here is where the record lands.
func newPostFixture(t *testing.T) *postFixture {
	t.Helper()

	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	communityRepo := postgres.NewCommunityRepository(db)

	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		pdsServer.URL(),
		instanceDID,
		instanceDomain,
		communities.NewPDSAccountProvisioner(instanceDomain, pdsServer.URL()),
		testkit.PasswordAuthFactory(pds.NewFromAccessToken),
		nil,
	)

	author := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("pa"))

	name := testkit.UniqueIDWithPrefix(t, "pw")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := communityService.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:         name,
		DisplayName:  "Write forward",
		Description:  "a community whose repo receives posts",
		Visibility:   "public",
		CreatedByDID: author.DID,
	})
	require.NoError(t, err)

	return &postFixture{
		service: posts.NewPostService(
			postgres.NewPostRepository(db), communityService,
			nil, nil, nil, nil, pdsServer.URL()),
		pds:       pdsServer,
		community: community,
		author:    author,
	}
}

// communityAccount returns a session on the community's own repo, which is how
// a test reads back what the service wrote there.
func (f *postFixture) communityAccount(t *testing.T) *testkit.Account {
	t.Helper()
	return f.pds.Login(t, f.community.Handle, f.community.PDSPassword)
}

// createPost writes a post as authorDID. The DID goes into the context as well
// as the request: the service re-checks the two against each other
// (defence-in-depth against a bypassed handler), so a test that sets only one
// exercises the mismatch guard by accident.
func (f *postFixture) createPost(t *testing.T, authorDID, title, content string) *posts.CreatePostResponse {
	t.Helper()
	resp, err := f.service.CreatePost(
		middleware.SetTestUserDID(context.Background(), authorDID),
		posts.CreatePostRequest{
			Community: f.community.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: authorDID,
		})
	require.NoError(t, err)
	require.NotEmpty(t, resp.URI)
	require.NotEmpty(t, resp.CID)
	return resp
}

// sessionFor builds the OAuth session shape DeletePost takes. Only the DID is
// load-bearing: the delete itself goes out on the COMMUNITY's credentials, and
// the session exists to say who is asking.
func sessionFor(t *testing.T, account *testkit.Account, hostURL string) *oauth.ClientSessionData {
	t.Helper()
	did, err := syntax.ParseDID(account.DID)
	require.NoError(t, err)
	return &oauth.ClientSessionData{
		AccountDID:  did,
		SessionID:   "post-write-forward-test",
		HostURL:     hostURL,
		AccessToken: account.AccessToken,
	}
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

func TestService_CreateWritesThePostIntoTheCommunityRepo(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	resp := f.createPost(t, f.author.DID, "write-forward title", "write-forward body")

	// The authority of the URI is the community, not the author. This is the
	// single most consequential fact about a post record's location: the
	// consumer rejects any post whose repo DID differs from its community
	// field, so a service that wrote to the author's repo would produce posts
	// that never index, with no error anywhere on the write path.
	rkey := rkeyOf(t, resp.URI)
	assert.Equal(t, "at://"+f.community.DID+"/"+postCollection+"/"+rkey, resp.URI)

	record := f.communityAccount(t).GetRecord(t, postCollection, rkey)

	// The CID the service reported is the CID of the record that actually
	// committed. Worth asserting rather than merely checking it is non-empty:
	// the response's CID is what a client uses to build a strongRef — a vote or
	// a comment's parent reference — so a service returning a stale or invented
	// one would produce references that resolve to nothing, and nothing on the
	// write path would notice.
	assert.Equal(t, record.CID, resp.CID,
		"the CID returned to the client must be the committed record's")

	assert.Equal(t, postCollection, record.Value["$type"])
	assert.Equal(t, f.community.DID, record.Value["community"],
		"the record's community field must match the repo it lives in, or the consumer rejects it as a spoof")
	assert.Equal(t, f.author.DID, record.Value["author"],
		"posts live in the community's repo but belong to their author, and this field is the only thing that says so")
	assert.Equal(t, "write-forward title", record.Value["title"])
	assert.Equal(t, "write-forward body", record.Value["content"])
	assert.NotEmpty(t, record.Value["createdAt"])
}

func TestService_DeleteRemovesTheRecordFromTheCommunityRepo(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	ctx := context.Background()
	resp := f.createPost(t, f.author.DID, "to be deleted", "body")
	rkey := rkeyOf(t, resp.URI)

	require.NoError(t, f.service.DeletePost(ctx, sessionFor(t, f.author, f.pds.URL()),
		posts.DeletePostRequest{URI: resp.URI}))

	community := f.communityAccount(t)
	assert.True(t, testkit.IsNotFound(getRecordErr(ctx, community, postCollection, rkey)),
		"the post record is still in the community's repo after its author deleted it")

	// KNOWN DEFECT, pinned as it behaves rather than as it is meant to.
	//
	// DeletePost intends a repeated delete to be idempotent: it checks the
	// record fetch for pds.ErrNotFound and returns nil, commented "Post already
	// deleted or never existed - idempotent success" (service.go step 7). That
	// branch is unreachable against this PDS. com.atproto.repo.getRecord answers
	// a missing record with HTTP 400 and "Could not locate record", and
	// pds/client.go maps 400 to ErrBadRequest — so the not-found check misses,
	// and the delete a client retries after a lost response comes back as an
	// opaque failure the handler renders as a 500.
	//
	// Asserting the intent here would fail the suite over a production bug this
	// task is not fixing; asserting nothing would let the bug become invisible.
	// So the assertion is the current truth, and it is written to FAIL LOUDLY
	// the moment the classification is fixed — at which point this block becomes
	// assert.NoError and the comment goes away.
	err := f.service.DeletePost(ctx, sessionFor(t, f.author, f.pds.URL()),
		posts.DeletePostRequest{URI: resp.URI})
	require.Errorf(t, err, "the idempotent-delete defect appears to be FIXED: "+
		"replace this block with assert.NoError and delete the KNOWN DEFECT comment above it")
	assert.Contains(t, err.Error(), "Could not locate record",
		"the repeated delete failed for a different reason than the known not-found misclassification")
}

func TestService_DeleteRefusesEveryoneButTheAuthor(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	ctx := context.Background()
	resp := f.createPost(t, f.author.DID, "the author's post", "body")
	rkey := rkeyOf(t, resp.URI)

	// The attacker is a fully legitimate account with a real session. What they
	// do not have is the record's author field — and since the delete goes out
	// on the COMMUNITY's credentials rather than on theirs, that field is the
	// ONLY thing standing between them and deleting someone else's post.
	attacker := f.pds.CreateAccount(t, testkit.WithHandlePrefix("atk"))

	err := f.service.DeletePost(ctx, sessionFor(t, attacker, f.pds.URL()),
		posts.DeletePostRequest{URI: resp.URI})
	require.ErrorIs(t, err, posts.ErrNotAuthorized,
		"a user who is not the post's author must be refused")

	// And refused means refused: the record is still there.
	community := f.communityAccount(t)
	record := community.GetRecord(t, postCollection, rkey)
	assert.Equal(t, f.author.DID, record.Value["author"],
		"the rejected delete removed the record anyway")
}

func TestService_DeleteRejectsMalformedRequests(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	ctx := context.Background()
	session := sessionFor(t, f.author, f.pds.URL())

	// Everything that must fail before the service touches the network. Each
	// case is a client mistake, and each must be answerable without the PDS
	// having been asked anything — a validation error, not a 500 from a failed
	// fetch of a nonsense URI.
	for _, tc := range []struct {
		name    string
		session *oauth.ClientSessionData
		uri     string
	}{
		{name: "no session", session: nil, uri: "at://" + f.community.DID + "/" + postCollection + "/abc"},
		{name: "empty URI", session: session, uri: ""},
		{name: "not an AT-URI", session: session, uri: "invalid-uri-format"},
		{name: "wrong collection", session: session, uri: "at://" + f.community.DID + "/social.coves.community.comment/abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := f.service.DeletePost(ctx, tc.session, posts.DeletePostRequest{URI: tc.uri})
			require.Error(t, err)
			assert.Truef(t, posts.IsValidationError(err),
				"expected a validation error the handler can turn into a 400, got: %v", err)
		})
	}
}

func TestService_DeleteReportsAnUnknownCommunityAsNotFound(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)

	// A well-formed URI whose authority is a community the AppView has never
	// indexed. The distinction from the validation errors above is what the
	// handler does with it — 404 rather than 400 — so the sentinel identity is
	// the assertion, not merely that an error came back.
	//
	// The DID is a literal rather than a generated one: did:plc identifiers are
	// 24 base32 characters (a-z, 2-7), which UniqueID does not promise, and a DID
	// that fails the FORMAT check would take the validation path above instead of
	// the lookup path under test. Uniqueness is not needed — the database is a
	// per-test clone in which nothing has ever been indexed.
	//
	// It is spelled at the full 24 characters deliberately. validateDIDFormat
	// (service.go) checks the CHARACTER SET but not the length, so a 23-character
	// identifier would pass today and start failing the moment that omission is
	// corrected — turning this lookup test into a validation test without anyone
	// touching it. Fixing the validator is not this task's business; not depending
	// on the gap is.
	uri := "at://did:plc:aaaaaaaaneverindexedcomm/" + postCollection + "/abc"
	err := f.service.DeletePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
		posts.DeletePostRequest{URI: uri})
	assert.ErrorIs(t, err, posts.ErrCommunityNotFound)
}

// getRecordErr asks the PDS for a record and returns only the error, so a test
// can assert a record's ABSENCE — Account.GetRecord fails the test on a missing
// record, which is the right default and the wrong tool here.
func getRecordErr(ctx context.Context, account *testkit.Account, collection, rkey string) error {
	return account.XRPC().Query(ctx, "com.atproto.repo.getRecord", map[string][]string{
		"repo":       {account.DID},
		"collection": {collection},
		"rkey":       {rkey},
	}, nil)
}
