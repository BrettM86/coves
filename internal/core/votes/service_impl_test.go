package votes_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/votes"
)

// The vote service's write path: what a tap on an arrow actually does to the
// voter's repository.
//
// # WHY THIS FILE EXISTS, AND WHY IT IS NOT AN INTEGRATION TEST
//
// It replaces the write-path half of tests/integration/vote_e2e_test.go, whose
// four XRPC tests each stood up a real PDS account, a real chi router and a
// fake OAuth middleware in order to assert — in the two places they asserted
// anything about the service at all — that a POST returned 200. The decisions
// this file covers are the ones those tests described in their comments and
// then simulated instead of observing:
//
//   - TestVoteE2E_ToggleSameDirection asserted the second POST was 200, and
//     produced the AppView state change by hand-writing the delete event it
//     expected the service to have caused. Whether the service deleted anything
//     was never checked.
//   - TestVoteE2E_ToggleDifferentDirection went further: its comment stated that
//     the service deletes the old record and creates a new one under a fresh
//     rkey, and then the test simulated exactly that. It did not even assert
//     that the returned URI differed from the first.
//
// Both decisions live entirely in voteService.CreateVote and are visible from a
// fake pds.Client, which is what this file uses. No PDS, no Postgres, no
// router: this is T0, and it runs in milliseconds.
//
// The pipeline half of that file — a real record reaching the AppView's own
// consumers and moving a post's counts — is tests/e2e/vote_contract_test.go.

// fakePDS is a pds.Client that records what the service asked it to do.
//
// Only the four methods the vote service calls do anything; the rest satisfy
// the interface and fail loudly if the service starts using them, because a new
// call to the real PDS from this path is exactly the kind of change these tests
// should notice.
type fakePDS struct {
	t   *testing.T
	did string

	// records is the repo's contents, keyed by rkey.
	records map[string]votes.VoteRecord

	// ops is ONE ordered log of every write, because the interesting claim in
	// this file is a claim about SEQUENCE: a direction change must delete the
	// old record before creating the new one, or there is a window in which the
	// repo holds two live votes for one subject and the consumer's stale-vote
	// cleanup decides arbitrarily which survives.
	//
	// It was two slices (created []string, deleted []string) in the first draft,
	// which cannot express interleaving at all — the ordering comment sat above
	// three final-state assertions that would pass just as happily with the
	// calls reversed. Review caught it. One log, so the assertion can be the one
	// the comment claims.
	ops []voteOp

	// createErr and deleteErr let a test drive the failure branches.
	createErr error
	deleteErr error
}

// voteOp is one write against the fake repo.
type voteOp struct {
	kind string // "create" or "delete"
	rkey string
}

func (o voteOp) String() string { return o.kind + "(" + o.rkey + ")" }

// creates returns the rkeys created, in order.
func (f *fakePDS) creates() []string { return f.rkeysOf("create") }

// deletes returns the rkeys deleted, in order.
func (f *fakePDS) deletes() []string { return f.rkeysOf("delete") }

func (f *fakePDS) rkeysOf(kind string) []string {
	var out []string
	for _, op := range f.ops {
		if op.kind == kind {
			out = append(out, op.rkey)
		}
	}
	return out
}

func newFakePDS(t *testing.T, did string) *fakePDS {
	t.Helper()
	return &fakePDS{t: t, did: did, records: map[string]votes.VoteRecord{}}
}

func (f *fakePDS) CreateRecord(_ context.Context, collection, rkey string, record any) (string, string, error) {
	if f.createErr != nil {
		return "", "", f.createErr
	}
	require.Equal(f.t, "social.coves.feed.vote", collection,
		"the vote service wrote to a collection other than the vote one")
	vote, ok := record.(votes.VoteRecord)
	require.Truef(f.t, ok, "the vote service passed a %T to CreateRecord rather than a votes.VoteRecord", record)
	f.records[rkey] = vote
	f.ops = append(f.ops, voteOp{kind: "create", rkey: rkey})
	return "at://" + f.did + "/" + collection + "/" + rkey, "bafycid" + rkey, nil
}

func (f *fakePDS) DeleteRecord(_ context.Context, collection, rkey string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	require.Equal(f.t, "social.coves.feed.vote", collection)
	delete(f.records, rkey)
	f.ops = append(f.ops, voteOp{kind: "delete", rkey: rkey})
	return nil
}

func (f *fakePDS) ListRecords(_ context.Context, collection string, _ int, cursor string) (*pds.ListRecordsResponse, error) {
	require.Equal(f.t, "social.coves.feed.vote", collection)
	require.Empty(f.t, cursor, "the fake repo is a single page; a cursor means the service is paginating past the end")
	out := &pds.ListRecordsResponse{}
	for rkey, rec := range f.records {
		out.Records = append(out.Records, pds.RecordEntry{
			URI: "at://" + f.did + "/" + collection + "/" + rkey,
			CID: "bafycid" + rkey,
			Value: map[string]any{
				"$type":     rec.Type,
				"direction": rec.Direction,
				"subject":   map[string]any{"uri": rec.Subject.URI, "cid": rec.Subject.CID},
				"createdAt": rec.CreatedAt,
			},
		})
	}
	return out, nil
}

func (f *fakePDS) DID() string     { return f.did }
func (f *fakePDS) HostURL() string { return "https://pds.invalid" }

func (f *fakePDS) GetRecord(context.Context, string, string) (*pds.RecordResponse, error) {
	f.t.Fatal("the vote service called GetRecord, which it did not before: add coverage for the new path")
	return nil, nil
}

func (f *fakePDS) PutRecord(context.Context, string, string, any, string) (string, string, error) {
	f.t.Fatal("the vote service called PutRecord. Votes are created and deleted, never updated in " +
		"place — and the vote consumer ignores update commits entirely (see " +
		"tests/e2e/vote_contract_test.go), so a vote written this way would never reach the index")
	return "", "", nil
}

func (f *fakePDS) UploadBlob(context.Context, []byte, string) (*blobs.BlobRef, error) {
	f.t.Fatal("the vote service uploaded a blob, which makes no sense for a vote")
	return nil, nil
}

// voter is the session the service votes on behalf of.
func voter(t *testing.T, did string) *oauth.ClientSessionData {
	t.Helper()
	parsed, err := syntax.ParseDID(did)
	require.NoError(t, err)
	return &oauth.ClientSessionData{AccountDID: parsed, AccessToken: "test-access-token"}
}

// newService wires the service to a fake PDS.
//
// The cache is nil on purpose in most tests: with a cache the service answers
// "does a vote already exist" from memory, and these tests are about the
// decision made from that answer, not about the cache. The one test that cares
// which source the answer came from builds its own.
func newService(t *testing.T, fake *fakePDS, cache *votes.VoteCache) votes.Service {
	t.Helper()
	return votes.NewServiceWithPDSFactory(nil, cache, nil,
		func(context.Context, *oauth.ClientSessionData) (pds.Client, error) { return fake, nil })
}

const (
	testVoterDID  = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
	testSubject   = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kabc"
	testSubjectCI = "bafyreiasubjectcid"
)

func subject() votes.StrongRef {
	return votes.StrongRef{URI: testSubject, CID: testSubjectCI}
}

func TestCreateVote_FirstVoteWritesTheRecord(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	svc := newService(t, fake, nil)

	resp, err := svc.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	require.Len(t, fake.ops, 1, "a first vote is one CreateRecord and nothing else")
	require.Equal(t, "create", fake.ops[0].kind)

	rkey := fake.ops[0].rkey
	require.Equal(t, "at://"+testVoterDID+"/social.coves.feed.vote/"+rkey, resp.URI,
		"the response must name the record the service actually wrote")
	require.NotEmpty(t, resp.CID)

	// The record's SHAPE is the wire contract with the consumer: the same fields
	// internal/atproto/jetstream's parseVoteRecord reads back out. Asserted here
	// because it is the one place both halves are in scope — the ingestion
	// contract writes its own records and so cannot check what the service
	// writes.
	written := fake.records[rkey]
	assert.Equal(t, "social.coves.feed.vote", written.Type)
	assert.Equal(t, "up", written.Direction)
	assert.Equal(t, testSubject, written.Subject.URI)
	assert.Equal(t, testSubjectCI, written.Subject.CID,
		"the subject CID makes the reference STRONG; dropping it would let a vote follow a post "+
			"through an edit")
	assert.NotEmpty(t, written.CreatedAt)

	// The rkey is a TID, which is what makes vote records sortable and what the
	// consumer builds the vote's URI from.
	_, err = syntax.ParseTID(rkey)
	assert.NoErrorf(t, err, "the vote service used %q as an rkey, which is not a TID", rkey)
}

func TestCreateVote_SameDirectionTogglesOff(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	svc := newService(t, fake, nil)
	ctx := context.Background()

	first, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)
	existing := fake.creates()[0]

	// The same tap again. This is the decision tests/integration's
	// TestVoteE2E_ToggleSameDirection was named after and never checked.
	second, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	require.Equal(t, []string{existing}, fake.deletes(),
		"re-voting the same direction must DELETE the existing record from the voter's repo")
	require.Len(t, fake.creates(), 1,
		"toggling off must not also create a replacement record: the repo would then hold two "+
			"votes for one subject, and the consumer's stale-vote cleanup would silently pick one")
	require.Empty(t, fake.records, "the voter's repo has no vote for this subject any more")

	// The empty response is how the handler tells a client the vote was
	// withdrawn rather than recorded — there is no separate status for it.
	require.Equal(t, "", second.URI, "a toggled-off vote answers with an empty URI")
	require.Equal(t, "", second.CID)
	require.NotEqual(t, first.URI, second.URI)
}

func TestCreateVote_DifferentDirectionReplacesUnderANewRKey(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	svc := newService(t, fake, nil)
	ctx := context.Background()

	up, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)
	oldRKey := fake.creates()[0]

	down, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
	require.NoError(t, err)

	require.Equal(t, []string{oldRKey}, fake.deletes(),
		"changing direction must remove the old record, not leave two votes in the repo")
	require.Len(t, fake.creates(), 2)
	newRKey := fake.creates()[1]

	// THE LOAD-BEARING CLAIM, and the one the deleted integration test asserted
	// nowhere: the replacement is a NEW record under a NEW key, not an update of
	// the old one. It matters beyond tidiness — the vote consumer handles create
	// and delete commits only, so a same-rkey update would never reach the
	// AppView at all (pinned from the outside in tests/e2e/vote_contract_test.go).
	require.NotEqual(t, oldRKey, newRKey,
		"the service reused the old rkey. A putRecord-shaped update is invisible to the vote "+
			"consumer, so the AppView would keep serving the previous direction forever")
	require.NotEqual(t, up.URI, down.URI)
	require.Equal(t, "at://"+testVoterDID+"/social.coves.feed.vote/"+newRKey, down.URI)

	// ORDERING, asserted rather than described. The old record must be deleted
	// BEFORE the new one is created: the reverse leaves a window in which the
	// repo holds two live votes for one subject, and if the firehose carries
	// them in that order the consumer's stale-vote cleanup picks a winner by
	// whichever arrives second rather than by what the user chose.
	//
	// Stated as the whole op log rather than as two positions, so a third write
	// appearing between them also fails.
	require.Equal(t, []voteOp{
		{kind: "create", rkey: oldRKey},
		{kind: "delete", rkey: oldRKey},
		{kind: "create", rkey: newRKey},
	}, fake.ops,
		"a direction change must be exactly delete-old-then-create-new against the voter's repo")

	require.Equal(t, "down", fake.records[newRKey].Direction)
	require.Len(t, fake.records, 1, "exactly one vote for this subject survives")
}

func TestCreateVote_LeavesOtherSubjectsAlone(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	svc := newService(t, fake, nil)
	ctx := context.Background()

	other := votes.StrongRef{
		URI: "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kother",
		CID: "bafyreiotherc",
	}
	_, err := svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: other, Direction: "up"})
	require.NoError(t, err)
	_, err = svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	// Toggling this subject off must not disturb the vote on the other one. The
	// lookup is by subject URI, and a lookup that ignored it would pass every
	// other test in this file.
	_, err = svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	require.Len(t, fake.records, 1)
	for _, rec := range fake.records {
		require.Equal(t, other.URI, rec.Subject.URI,
			"withdrawing a vote on one post removed the voter's vote on a different one")
	}
}

func TestDeleteVote(t *testing.T) {
	t.Parallel()

	t.Run("removes the record from the voter's repo", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, nil)
		ctx := context.Background()

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)
		rkey := fake.creates()[0]

		require.NoError(t, svc.DeleteVote(ctx, voter(t, testVoterDID),
			votes.DeleteVoteRequest{Subject: subject()}))
		require.Equal(t, []string{rkey}, fake.deletes())
		require.Empty(t, fake.records)
	})

	t.Run("is not found when there is no vote to delete", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, nil)

		err := svc.DeleteVote(context.Background(), voter(t, testVoterDID),
			votes.DeleteVoteRequest{Subject: subject()})
		require.ErrorIs(t, err, votes.ErrVoteNotFound,
			"deleting a vote that was never cast must be distinguishable from deleting one that was: "+
				"the handler maps this to a 404 and a silent success would tell a client its "+
				"un-vote worked when nothing happened")
		require.Empty(t, fake.ops)
	})
}

func TestCreateVote_RejectsMalformedInput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		req     votes.CreateVoteRequest
		wantErr error
	}{
		{"sideways direction", votes.CreateVoteRequest{Subject: subject(), Direction: "sideways"}, votes.ErrInvalidDirection},
		{"empty direction", votes.CreateVoteRequest{Subject: subject(), Direction: ""}, votes.ErrInvalidDirection},
		{"empty subject URI", votes.CreateVoteRequest{
			Subject: votes.StrongRef{CID: testSubjectCI}, Direction: "up"}, votes.ErrInvalidSubject},
		{"subject URI that is not an AT-URI", votes.CreateVoteRequest{
			Subject:   votes.StrongRef{URI: "https://example.com/post/1", CID: testSubjectCI},
			Direction: "up"}, votes.ErrInvalidSubject},
		{"subject with no CID", votes.CreateVoteRequest{
			Subject: votes.StrongRef{URI: testSubject}, Direction: "up"}, votes.ErrInvalidSubject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			svc := newService(t, fake, nil)

			_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID), tc.req)
			require.ErrorIs(t, err, tc.wantErr)
			require.Empty(t, fake.ops,
				"a rejected vote must not reach the PDS: validation runs before any repo write")
		})
	}
}

func TestCreateVote_PDSFailureIsReportedRatherThanSwallowed(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	fake.createErr = errors.New("pds is down")
	svc := newService(t, fake, nil)

	_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.Error(t, err, "a vote the PDS refused must not be reported to the client as cast — "+
		"nothing else will ever write that record, so a swallowed error is a vote that silently "+
		"never existed")
	require.Contains(t, strings.ToLower(err.Error()), "pds is down",
		"the underlying failure must survive into the error the handler maps")
}

func TestCreateVote_FailedToggleOffDoesNotReportSuccess(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	svc := newService(t, fake, nil)
	ctx := context.Background()

	_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	// The withdrawal fails at the PDS. The dangerous outcome would be the
	// service returning the empty "toggled off" response anyway: the client
	// would render the arrow as un-pressed while the record — and therefore the
	// AppView's count — still says otherwise, with nothing to correct it.
	fake.deleteErr = errors.New("pds refused the delete")
	_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.Error(t, err)
	require.Len(t, fake.records, 1, "the vote is still in the repo, and the caller was told so")
	require.Len(t, fake.creates(), 1, "a failed toggle-off must not fall through into creating a second vote")
}
