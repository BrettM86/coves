//go:build integration

package posts_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

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

// What creating and deleting a post actually does to a repository, and who is
// allowed to do it.
//
// This is the client-write half of the post domain: the half that
// tests/e2e/post_contract_test.go structurally cannot reach. §3.4b of
// docs/TEST_ARCHITECTURE.md records why — RequireAuth accepts only a sealed
// session token, minted nowhere but the browser OAuth callback, so T2 can prove
// that the write endpoints refuse an unauthenticated client and nothing beyond
// it. Authenticated write BEHAVIOUR is therefore proven here, against a real
// PDS.
//
// # THE REPO MOVED, AND THAT IS WHAT THIS FILE IS NOW ABOUT
//
// Until task 6 a post record did NOT live in its author's repo. It lived in the
// COMMUNITY's, written with the community's own PDS credentials, carrying an
// `author` field that named the human who wrote it. Every assertion in this file
// used to pin that arrangement in place.
//
// docs/PRD_AUTHOR_OWNED_POSTS.md §3.1 and §4.2 reverse it. A post is a
// social.coves.community.postv2 record in the AUTHOR's repository, signed with
// the author's own credentials, with NO author field at all — authorship is the
// repository, which a verifying relay or a DID-resolved fetch can check, rather
// than a claim one party signed about another. The community's answer is a
// separate record it publishes itself (§3.2).
//
// That reversal is why the assertions here read the way they do:
//
//   - The record's location is still the most consequential fact about it, so it
//     is still asserted from the PDS side rather than from the service's return
//     value. What changed is which repo has to hold it — and, just as load-
//     bearing, which repo must NOT.
//   - DELETE AUTHORIZATION IS NOW A LOCAL DECISION. It used to require fetching
//     the record to read its `author` field, because the delete went out on the
//     COMMUNITY's credentials and that field was the only thing standing between
//     an attacker and someone else's post. A postv2 record is in the author's own
//     repo, so the URI's authority IS the owner: the check is the session DID
//     against the URI, decided before anything is fetched, and the credentials
//     the delete goes out on cannot reach another author's repo even if the check
//     were wrong.

const (
	// The instance identity these tests provision communities under. It matches
	// the AppView's default (internal/config: INSTANCE_DID defaults to
	// did:web:coves.social), and the domain must be one of the PDS'
	// PDS_SERVICE_HANDLE_DOMAINS or account creation is refused.
	instanceDID    = "did:web:coves.social"
	instanceDomain = "coves.social"

	// postCollection is the DEPRECATED community-repo collection. It survives in
	// this file only where a test is about the pre-flip records still standing
	// in production repos — task 8 re-materializes them and retires it.
	postCollection = "social.coves.community.post"
)

// postFixture is the post service wired the way cmd/server wires it, over a real
// community that owns a real PDS repository and a real author who owns another.
//
// db and communityService are the provisioned halves of that wiring, kept so a
// neighbouring test can build a differently-wired post service over the SAME
// community rather than provision a second one — see service_aggregator_test.go
// and service_admission_test.go, which need collaborators this fixture
// deliberately leaves nil. Those neighbours reuse authorRepos and the acceptance
// wiring through writePathOptions.
type postFixture struct {
	service          posts.Service
	pds              *testkit.PDS
	db               *sql.DB
	communityService communities.Service
	community        *communities.Community
	author           *testkit.Account

	// admissions is the real table the fast path seeds and the engine settles.
	admissions posts.AdmissionRepository

	// acceptor is the real engine behind a switch a test can throw, so the
	// "acceptance failed" branch can be reached without breaking the PDS.
	acceptor *acceptorSpy

	// authorRepos hands out credentials per author DID — the seam that replaces
	// the community's service token on the write path.
	authorRepos *authorRepoRegistry
}

// authorRepoRegistry is the integration stand-in for the production
// AuthorRepoFactory.
//
// Production resolves an author's OAuth session (or an aggregator's stored
// tokens) and builds a DPoP client; here every author is a real PDS account with
// a password session, which is the same substitution comments' PDSClientFactory
// makes (testkit.PasswordAuthFactory) and for the same reason: OAuth's browser
// callback cannot be driven from a Go test, and what is under test is which repo
// the write lands in, not how the token was obtained.
//
// An author it does not know answers ErrNoAuthorCredentials, which is exactly
// what production answers for an aggregator whose stored session is gone.
type authorRepoRegistry struct {
	pds *testkit.PDS

	mu       sync.Mutex
	accounts map[string]*testkit.Account

	// afterRead runs immediately after a repo read, per author DID. It is the
	// only way to open the read-then-write window an update's swap guard exists
	// to close: a competing commit has to land BETWEEN the service's pre-read
	// and its put, and no amount of ordering from outside the service can
	// arrange that. engine_contract_test.go forces a lost swapRecord race the
	// same way.
	afterRead map[string]func()

	// hosts overrides the PDS an author's repo client talks to. It is how a
	// write FAILURE is injected now: the write goes to the AUTHOR's repo, so
	// pointing the community's stored pds_url at a broken server — which is how
	// the reservation-release tests used to do it — breaks nothing the write
	// path touches any more.
	hosts map[string]string
}

func newAuthorRepoRegistry(pdsServer *testkit.PDS) *authorRepoRegistry {
	return &authorRepoRegistry{
		pds:       pdsServer,
		accounts:  map[string]*testkit.Account{},
		afterRead: map[string]func(){},
		hosts:     map[string]string{},
	}
}

// pointAt sends an author's repo writes to host instead of the test PDS. An
// empty host restores the real one.
func (r *authorRepoRegistry) pointAt(authorDID, host string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if host == "" {
		delete(r.hosts, authorDID)
		return
	}
	r.hosts[authorDID] = host
}

// raceAfterRead arranges for fn to run once, right after the next read of
// authorDID's repo, so the service's next write meets a record that changed
// under it.
func (r *authorRepoRegistry) raceAfterRead(authorDID string, fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var once sync.Once
	r.afterRead[authorDID] = func() { once.Do(fn) }
}

// racingAuthorRepo is an author repo that lets a competing write land between a
// read and the write shaped from it.
type racingAuthorRepo struct {
	posts.AuthorRepo
	afterRead func()
}

func (r *racingAuthorRepo) GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	record, err := r.AuthorRepo.GetRecord(ctx, collection, rkey)
	if r.afterRead != nil {
		r.afterRead()
	}
	return record, err
}

// register makes an account's repo reachable by its DID.
func (r *authorRepoRegistry) register(account *testkit.Account) *testkit.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[account.DID] = account
	return account
}

// factory is the AuthorRepoFactory the post service is wired with.
func (r *authorRepoRegistry) factory() posts.AuthorRepoFactory {
	return func(_ context.Context, authorDID string, _ *oauth.ClientSessionData) (posts.AuthorRepo, error) {
		r.mu.Lock()
		account := r.accounts[authorDID]
		race := r.afterRead[authorDID]
		host := r.hosts[authorDID]
		r.mu.Unlock()

		if account == nil {
			return nil, fmt.Errorf("%w: no session is stored for %s", posts.ErrNoAuthorCredentials, authorDID)
		}

		if host == "" {
			host = r.pds.URL()
		}
		client, err := pds.NewFromAccessToken(host, account.DID, account.AccessToken)
		if err != nil {
			return nil, fmt.Errorf("opening the repo of %s: %w", authorDID, err)
		}
		repo, ok := client.(posts.AuthorRepo)
		if !ok {
			return nil, fmt.Errorf("opening the repo of %s: the PDS client does not implement the "+
				"author-repo write surface (guarded put + commit rev)", authorDID)
		}
		if race != nil {
			return &racingAuthorRepo{AuthorRepo: repo, afterRead: race}, nil
		}
		return repo, nil
	}
}

// acceptorSpy delegates to the real acceptance engine, or fails on demand.
//
// The failure switch is not a convenience. The whole promise of the fast path is
// that its failure is INVISIBLE to the author — the record stands, the response
// succeeds, the post is merely pending — and a fixture that could not make the
// acceptance fail could not prove any of that. Breaking the PDS instead would
// break the author-repo write too, which is the case this is trying to hold
// still.
type acceptorSpy struct {
	delegate posts.SubmissionAcceptor

	mu       sync.Mutex
	failWith error
	calls    int
}

func (s *acceptorSpy) AcceptSubmission(ctx context.Context, communityDID, postURI, postCID string) (posts.EngineOutcome, error) {
	s.mu.Lock()
	s.calls++
	failure := s.failWith
	s.mu.Unlock()

	if failure != nil {
		return posts.EngineDeferred, failure
	}
	return s.delegate.AcceptSubmission(ctx, communityDID, postURI, postCID)
}

// failWith makes every subsequent acceptance fail with err.
func (s *acceptorSpy) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith = err
}

// callCount reports how many times the write path reached for the fast path.
func (s *acceptorSpy) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newPostFixture provisions a community and an author on the test PDS and
// returns the post service pointed at both.
//
// The community is provisioned through communities.CreateCommunity rather than
// seeded into the index, because the community still owns a real repo: the
// ACCEPTANCE goes there, written with the credentials provisioning produces. The
// optional content collaborators (aggregators, blobs, unfurl, bluesky) are nil —
// every one of them is a branch on the record's contents, and what is under test
// here is where the record lands.
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

	authorRepos := newAuthorRepoRegistry(pdsServer)
	author := authorRepos.register(pdsServer.CreateAccount(t, testkit.WithHandlePrefix("pa")))

	name := testkit.UniqueIDWithPrefix(t, "pw")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := communityService.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:         name,
		DisplayName:  "Write forward",
		Description:  "a community whose repo receives acceptances",
		Visibility:   "public",
		CreatedByDID: author.DID,
	})
	require.NoError(t, err)

	// The real engine, over the real production community-repo factory. Nothing
	// here is a double: the factory's hosting test is credential presence, which
	// is what the unhosted case in service_writeflip_test.go turns off.
	admissions := postgres.NewAdmissionRepository(db)
	engine := posts.NewAcceptanceEngine(
		admissions,
		// The fast path does not re-decide — CreatePost has already run
		// admission — so the decider is present only to satisfy the engine's
		// constructor. A decider that ADMITTED here would hide a fast path that
		// wrongly re-ran policy; one that refused would make every acceptance
		// fail. It is scripted to refuse, so a fast path that consulted it at
		// all fails these tests loudly.
		&scriptedDecider{code: posts.DecisionRuleViolation},
		posts.NewCommunityRecordWriter(posts.NewCommunityRepoFactory(communityService), time.Now),
		posts.NewCommunityCredentialRefresher(communityService),
	)
	acceptor := &acceptorSpy{delegate: engine}

	f := &postFixture{
		pds:              pdsServer,
		db:               db,
		communityService: communityService,
		community:        community,
		author:           author,
		admissions:       admissions,
		acceptor:         acceptor,
		authorRepos:      authorRepos,
	}
	f.service = posts.NewPostService(
		postgres.NewPostRepository(db), communityService,
		nil, nil, nil, nil, pdsServer.URL(),
		append(f.writePathOptions(),
			// Write-forward is not about admission; the opt-out is explicit.
			posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()))...)
	return f
}

// writePathOptions are the collaborators every post service in this package
// needs now that a post is written to its author's repo: the credential seam and
// the local-community fast path.
//
// Exposed so the neighbouring fixtures (admission, aggregator) wire the SAME
// author registry and the SAME engine as this one. They used to need neither,
// because a post was written with the community's service token that
// CreateCommunity had already produced.
func (f *postFixture) writePathOptions() []posts.PostServiceOption {
	return []posts.PostServiceOption{
		posts.WithAuthorRepoFactory(f.authorRepos.factory()),
		posts.WithSyncAcceptance(f.admissions, f.acceptor),
	}
}

// communityAccount returns a session on the community's own repo, which is how a
// test reads back the acceptance the service wrote there — and how it proves the
// community's repo did NOT receive a post.
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
	resp, err := f.submitPost(t, authorDID, title, content)
	require.NoError(t, err)
	require.NotEmpty(t, resp.URI)
	require.NotEmpty(t, resp.CID)
	return resp
}

// submitPost is createPost without the success requirement, for the cases that
// are about what a failure leaves behind.
func (f *postFixture) submitPost(t *testing.T, authorDID, title, content string) (*posts.CreatePostResponse, error) {
	t.Helper()
	return f.service.CreatePost(
		middleware.SetTestUserDID(context.Background(), authorDID),
		f.sessionForDID(t, authorDID),
		posts.CreatePostRequest{
			Community: f.community.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: authorDID,
		})
}

// sessionForDID builds the session of a registered author, or a session with no
// repo behind it for a DID the registry has never heard of.
func (f *postFixture) sessionForDID(t *testing.T, did string) *oauth.ClientSessionData {
	t.Helper()

	f.authorRepos.mu.Lock()
	account := f.authorRepos.accounts[did]
	f.authorRepos.mu.Unlock()

	if account == nil {
		parsed, err := syntax.ParseDID(did)
		require.NoError(t, err)
		return &oauth.ClientSessionData{
			AccountDID: parsed,
			SessionID:  "post-write-flip-test",
			HostURL:    f.pds.URL(),
		}
	}
	return sessionFor(t, account, f.pds.URL())
}

// authorAccount returns the fixture author's own PDS session, which is how a
// test reads back what the service wrote into their repo.
func (f *postFixture) authorAccount() *testkit.Account { return f.author }

// sessionFor builds the OAuth session shape the write endpoints take. The DID is
// what says who is asking; the account behind it is what the author-repo factory
// resolves into credentials.
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

func TestService_CreateWritesThePostIntoTheAuthorRepo(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	resp := f.createPost(t, f.author.DID, "write-forward title", "write-forward body")

	// THE AUTHORITY OF THE URI IS THE AUTHOR. This is the single most
	// consequential fact about where a post record lives, and it is the exact
	// reverse of what this assertion said before the flip. A service that still
	// wrote into the community's repo would produce records the postv2 consumer
	// attributes to the community — every post in an instance authored by the
	// community itself — with no error anywhere on the write path.
	rkey := rkeyOf(t, resp.URI)
	assert.Equal(t, "at://"+f.author.DID+"/"+posts.PostV2Collection+"/"+rkey, resp.URI)

	// And the rkey is a TID, because the lexicon declares `key: tid`. It is
	// derived rather than minted (§4.2), which is what the retry case below
	// proves; that it is WELL-FORMED is asserted here because a PDS running a
	// stricter build refuses anything else outright.
	_, err := syntax.ParseTID(rkey)
	require.NoErrorf(t, err, "the post's record key %q is not a TID", rkey)

	record := f.authorAccount().GetRecord(t, posts.PostV2Collection, rkey)

	// The CID the service reported is the CID of the record that actually
	// committed. Worth asserting rather than merely checking it is non-empty:
	// the response's CID is what a client uses to build a strongRef — a vote or
	// a comment's parent reference — so a service returning a stale or invented
	// one would produce references that resolve to nothing, and nothing on the
	// write path would notice.
	assert.Equal(t, record.CID, resp.CID,
		"the CID returned to the client must be the committed record's")

	assert.Equal(t, posts.PostV2Collection, record.Value["$type"])
	assert.Equal(t, f.community.DID, record.Value["community"],
		"the record names the community it was submitted to, as a DID — the client may have typed a handle")
	assert.Equal(t, "write-forward title", record.Value["title"])
	assert.Equal(t, "write-forward body", record.Value["content"])
	assert.NotEmpty(t, record.Value["createdAt"])

	// NO AUTHOR FIELD. The repository is the attribution now; a record carrying
	// the field as well would give consumers two answers to one question, one of
	// them unverifiable, with no rule for which wins.
	assert.NotContains(t, record.Value, "author",
		"a postv2 record must not carry an author field — authorship is the repo it lives in")

	// AND THE COMMUNITY'S REPO RECEIVED NO POST. Asserted by listing rather than
	// by a single get, because a service that wrote to BOTH repos would satisfy
	// every assertion above while doubling every post in the network.
	assert.Emptyf(t, listRecordKeys(t, f.communityAccount(t), postCollection),
		"the community's repo holds a %s record; posts belong to their authors now", postCollection)
	assert.Emptyf(t, listRecordKeys(t, f.communityAccount(t), posts.PostV2Collection),
		"the community's repo holds a %s record; the post belongs in the AUTHOR's repo",
		posts.PostV2Collection)
}

func TestService_DeleteRemovesTheRecordFromTheAuthorRepo(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	ctx := context.Background()
	resp := f.createPost(t, f.author.DID, "to be deleted", "body")
	rkey := rkeyOf(t, resp.URI)

	// Asserted before the delete, and not merely for symmetry with its
	// neighbour: without it this test would pass against the PRE-FLIP write
	// path, which puts the record in the community's repo — the author's repo
	// would then be empty for the whole test, and the absence checks below would
	// hold trivially. The delete has to be shown removing something.
	require.Equal(t, "at://"+f.author.DID+"/"+posts.PostV2Collection+"/"+rkey, resp.URI)
	f.author.GetRecord(t, posts.PostV2Collection, rkey)

	require.NoError(t, f.service.DeletePost(ctx, sessionFor(t, f.author, f.pds.URL()),
		posts.DeletePostRequest{URI: resp.URI}))

	assert.True(t, testkit.IsNotFound(getRecordErr(ctx, f.author, posts.PostV2Collection, rkey)),
		"the post record is still in the author's repo after they deleted it")

	// The idempotent-delete path is real: the PDS answers a missing record with
	// HTTP 400 named RecordNotFound, and the client's name-before-status mapping
	// turns that into pds.ErrNotFound, so DeletePost's not-found branch is
	// reachable. Previously pinned as a known defect (p3 from the test-refactor
	// loop); fixed by task 4's PDS error mapping.
	assert.NoError(t, f.service.DeletePost(ctx, sessionFor(t, f.author, f.pds.URL()),
		posts.DeletePostRequest{URI: resp.URI}),
		"a repeated delete is idempotent — the retried delete after a lost response succeeds")

	// And idempotent means the record STAYED gone: a second delete that somehow
	// resurrected or re-wrote the record would also return success, so the
	// absence has to be re-asserted, not assumed.
	assert.True(t, testkit.IsNotFound(getRecordErr(ctx, f.author, posts.PostV2Collection, rkey)),
		"the record must still be absent after the idempotent re-delete")
}

func TestService_DeleteRefusesEveryoneButTheAuthor(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	ctx := context.Background()
	resp := f.createPost(t, f.author.DID, "the author's post", "body")
	rkey := rkeyOf(t, resp.URI)

	// The attacker is a fully legitimate account with a real session and a real
	// repo of their own. What they do not own is the repo the URI names.
	//
	// THE RATIONALE CHANGED WITH THE REPO. Before the flip, the delete went out
	// on the COMMUNITY's credentials — which could delete anyone's post — so the
	// record's `author` field was the only thing standing between an attacker
	// and someone else's post, and it had to be fetched to be checked. Now the
	// URI's authority IS the owner: the refusal is decided locally, before
	// anything is fetched, and the credentials the delete would go out on cannot
	// reach the author's repo in the first place. The check is defence in depth
	// over a boundary the PDS already enforces — which is exactly why it must be
	// proven to exist, since removing it would look harmless right up until an
	// author-supplied repo DID reached the factory.
	attacker := f.authorRepos.register(f.pds.CreateAccount(t, testkit.WithHandlePrefix("atk")))

	err := f.service.DeletePost(ctx, sessionFor(t, attacker, f.pds.URL()),
		posts.DeletePostRequest{URI: resp.URI})
	require.ErrorIs(t, err, posts.ErrNotAuthorized,
		"a user who is not the post's author must be refused")

	// And refused means refused: the record is still there.
	record := f.authorAccount().GetRecord(t, posts.PostV2Collection, rkey)
	assert.Equal(t, f.community.DID, record.Value["community"],
		"the rejected delete removed the record anyway")
}

func TestService_DeleteRefusesAURIWhoseAuthorityIsNotTheCaller(t *testing.T) {
	t.Parallel()

	// This REPLACES the old "an unknown community is reported as not found".
	//
	// That test existed because DeletePost's first act was to look the URI's
	// authority up as a COMMUNITY, so an authority nobody had indexed was a 404
	// and the sentinel identity was the contract. A postv2 URI's authority is an
	// AUTHOR, and there is no lookup to miss: the only question is whether it is
	// the caller's own DID. An authority that is not is refused as
	// unauthorized — never as "not found", which would tell an attacker that
	// the DID they aimed at is one the AppView has never seen, and would answer
	// 404 to a probe that deserves 403.
	f := newPostFixture(t)

	// A well-formed DID that owns nothing here. It is a literal rather than a
	// generated one: did:plc identifiers are 24 base32 characters (a-z, 2-7),
	// which UniqueID does not promise, and a DID that failed the FORMAT check
	// would take the validation path instead of the authorization path under
	// test. Spelled at the full 24 characters deliberately — validateDIDFormat
	// checks the character set but not the length, so a short one would start
	// failing differently the day that omission is corrected.
	uri := "at://did:plc:aaaaaaaasomeoneelsesrepo/" + posts.PostV2Collection + "/3lrc77gmww4nc"

	err := f.service.DeletePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
		posts.DeletePostRequest{URI: uri})
	assert.ErrorIs(t, err, posts.ErrNotAuthorized)
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
		session *oauth.ClientSessionData
		name    string
		uri     string
	}{
		{name: "no session", session: nil, uri: "at://" + f.author.DID + "/" + posts.PostV2Collection + "/3lrc77gmww4nc"},
		{name: "empty URI", session: session, uri: ""},
		{name: "not an AT-URI", session: session, uri: "invalid-uri-format"},
		{
			// A collection that is neither post collection. The old spelling of
			// this case used the DEPRECATED community.post NSID, because delete
			// was narrowed to postv2's predecessor and postv2 itself was refused;
			// both collections are accepted now (see the next test), so the
			// malformed case has to be something that genuinely is not a post.
			name: "a collection that is not a post at all", session: session,
			uri: "at://" + f.author.DID + "/social.coves.community.comment/3lrc77gmww4nc",
		},
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

func TestService_DeleteStillReachesPreFlipPostsInTheCommunityRepo(t *testing.T) {
	t.Parallel()

	// The other half of the adjudicated wrong-collection row: BOTH collections
	// are accepted, and the deprecated one still takes the OLD path.
	//
	// This is not backwards compatibility for its own sake. Every post written
	// before the flip is a social.coves.community.post record standing in a
	// community's repo right now, and its author's delete button has to keep
	// working until task 8 re-materializes them. The two paths are genuinely
	// different — this one authenticates as the COMMUNITY and reads the record's
	// `author` field, because the caller has no credentials on that repo at all —
	// which is precisely why refusing the collection outright is not an option
	// and why the routing needs a test that watches the record disappear.
	f := newPostFixture(t)
	ctx := context.Background()

	// Written directly with the community's own credentials, which is exactly
	// how the pre-flip write path produced it.
	community := f.communityAccount(t)
	legacy := community.CreateRecord(t, postCollection, map[string]any{
		"$type":     postCollection,
		"community": f.community.DID,
		"author":    f.author.DID,
		"title":     "written before the flip",
		"content":   "and still deletable by its author",
		"createdAt": "2026-07-01T12:00:00Z",
	})
	rkey := rkeyOf(t, legacy.URI)

	require.NoError(t, f.service.DeletePost(ctx, sessionFor(t, f.author, f.pds.URL()),
		posts.DeletePostRequest{URI: legacy.URI}),
		"an author must still be able to delete a post written before the write path flipped")

	assert.True(t, testkit.IsNotFound(getRecordErr(ctx, community, postCollection, rkey)),
		"the pre-flip record is still in the community's repo after its author deleted it")

	// And the old path's authorization still comes from the RECORD, because the
	// caller has no credentials on the community's repo to be checked against.
	second := community.CreateRecord(t, postCollection, map[string]any{
		"$type":     postCollection,
		"community": f.community.DID,
		"author":    f.author.DID,
		"title":     "someone else's pre-flip post",
		"content":   "body",
		"createdAt": "2026-07-01T12:00:00Z",
	})
	attacker := f.authorRepos.register(f.pds.CreateAccount(t, testkit.WithHandlePrefix("atl")))
	assert.ErrorIs(t,
		f.service.DeletePost(ctx, sessionFor(t, attacker, f.pds.URL()),
			posts.DeletePostRequest{URI: second.URI}),
		posts.ErrNotAuthorized,
		"the deprecated path's author check is the record's field, and it must still refuse a stranger")
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
