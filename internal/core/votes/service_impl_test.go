package votes_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/votes"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
//
// Its methods are safe for concurrent use. Hooks run without the lock held.
// Tests read the recorded state and set knobs directly between service calls.
type fakePDS struct {
	mutex sync.Mutex
	t     *testing.T
	did   string

	// records is the repo's contents, keyed by rkey.
	records map[string]votes.VoteRecord

	// ops records committed writes; batchCalls distinguishes atomic replacements.
	ops []voteOp

	// createErr, deleteErr and listErr let a test drive the failure branches.
	createErr         error
	deleteErr         error
	listErr           error
	batchErr          error
	batchCalls        int
	commitBeforeError bool
	afterBatch        func()
	getErr            error
	getCalls          int
	getTransform      func(*pds.RecordResponse)
	getHook           func(context.Context, string) error
	batchResult       func(*pds.ApplyWritesResult) *pds.ApplyWritesResult

	// commitBeforeError applies to CreateRecord and DeleteRecord as well: a
	// failing plain write commits and then returns its error, a lost reply.
	//
	// writeHook runs when CreateRecord or DeleteRecord is entered, before the
	// write is applied; operation is "create" or "delete". A non-nil error is
	// returned without committing.
	writeHook func(ctx context.Context, operation, rkey string) error

	// createAttempts records every CreateRecord that got past writeHook,
	// committed or not, so a test can see the record key a failed create
	// used. A create at a record key the repo already holds fails the way a
	// real PDS does (HTTP 500) and commits nothing.
	createAttempts []createAttempt

	// createAnswer, when set, makes a CreateRecord that would succeed commit
	// nothing and answer success with the URI and CID it returns.
	createAnswer func(rkey string) (uri, cid string)

	// afterList runs after ListRecords builds its response, without the lock
	// held, so a test can change the repo between a scan and the write that
	// follows it.
	afterList func()

	// pageSize splits ListRecords into pages of this many records when it is
	// positive. The service's PDS scan keeps asking for the next page until
	// the PDS stops returning a cursor, and a fake that only ever answers one page cannot tell a
	// correctly-paginating reader from one that reads the first hundred votes
	// and gives up. Zero keeps the original single-page behaviour, which the
	// tests that are not about pagination rely on to catch a stray cursor.
	pageSize int

	// listCalls counts ListRecords round trips: the PDS scan runs only after
	// an ambiguous write, never on an ordinary vote.
	listCalls int

	// rawRecords are listed alongside records but never went through
	// CreateRecord, so their values can be any shape at all. A vote repo is
	// user-writable and a third-party client may put anything in it; the
	// readers have to survive that, and a fake that can only produce records
	// the service itself wrote cannot ask them to. Single-page only.
	rawRecords []pds.RecordEntry
}

// voteOp is one write against the fake repo.
type voteOp struct {
	kind string // "create" or "delete"
	rkey string
}

func (o voteOp) String() string { return o.kind + "(" + o.rkey + ")" }

// createAttempt is one CreateRecord call, whether or not it committed.
type createAttempt struct {
	rkey   string
	record votes.VoteRecord
}

// attemptedCreates returns every CreateRecord call so far, in order.
func (f *fakePDS) attemptedCreates() []createAttempt {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]createAttempt(nil), f.createAttempts...)
}

// commitLate commits a create that failed earlier, as a PDS that finishes a
// write after its reply was lost does. Like CreateRecord, it refuses a record
// key the repo already holds: it commits nothing and returns false.
func (f *fakePDS) commitLate(attempt createAttempt) bool {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if _, exists := f.records[attempt.rkey]; exists {
		return false
	}
	f.records[attempt.rkey] = attempt.record
	f.ops = append(f.ops, voteOp{kind: "create", rkey: attempt.rkey})
	return true
}

// creates returns the rkeys created, in order.
func (f *fakePDS) creates() []string { return f.rkeysOf("create") }

// deletes returns the rkeys deleted, in order.
func (f *fakePDS) deletes() []string { return f.rkeysOf("delete") }

func (f *fakePDS) rkeysOf(kind string) []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
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

// runWriteHook runs the write hook, if any, without the lock held.
func (f *fakePDS) runWriteHook(ctx context.Context, operation, rkey string) error {
	f.mutex.Lock()
	writeHook := f.writeHook
	f.mutex.Unlock()
	if writeHook == nil {
		return nil
	}
	return writeHook(ctx, operation, rkey)
}

func (f *fakePDS) CreateRecord(ctx context.Context, collection, rkey string, record any) (string, string, error) {
	if err := f.runWriteHook(ctx, "create", rkey); err != nil {
		return "", "", err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	require.Equal(f.t, "social.coves.feed.vote", collection,
		"the vote service wrote to a collection other than the vote one")
	vote, ok := record.(votes.VoteRecord)
	require.Truef(f.t, ok, "the vote service passed a %T to CreateRecord rather than a votes.VoteRecord", record)
	f.createAttempts = append(f.createAttempts, createAttempt{rkey: rkey, record: vote})
	if f.createErr != nil && !f.commitBeforeError {
		return "", "", f.createErr
	}
	if f.createAnswer != nil {
		uri, cid := f.createAnswer(rkey)
		return uri, cid, nil
	}
	if _, exists := f.records[rkey]; exists {
		// Verified against the local dev PDS: createRecord at an existing
		// record key answers HTTP 500 and commits nothing.
		return "", "", fmt.Errorf("createRecord: %w: Internal Server Error", pds.ErrServerError)
	}
	f.records[rkey] = vote
	f.ops = append(f.ops, voteOp{kind: "create", rkey: rkey})
	if f.createErr != nil {
		return "", "", f.createErr
	}
	return "at://" + f.did + "/" + collection + "/" + rkey, "bafycid" + rkey, nil
}

func (f *fakePDS) DeleteRecord(ctx context.Context, collection, rkey string) error {
	if err := f.runWriteHook(ctx, "delete", rkey); err != nil {
		return err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.deleteErr != nil && !f.commitBeforeError {
		return f.deleteErr
	}
	require.Equal(f.t, "social.coves.feed.vote", collection)
	delete(f.records, rkey)
	f.ops = append(f.ops, voteOp{kind: "delete", rkey: rkey})
	return f.deleteErr
}

func (f *fakePDS) ListRecords(_ context.Context, collection string, _ int, cursor string) (*pds.ListRecordsResponse, error) {
	response, afterList, err := f.listRecords(collection, cursor)
	if err != nil {
		return nil, err
	}
	if afterList != nil {
		afterList()
	}
	return response, nil
}

// listRecords builds one ListRecords page under the lock and returns the
// hook to run after it.
func (f *fakePDS) listRecords(collection, cursor string) (*pds.ListRecordsResponse, func(), error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, nil, f.listErr
	}
	require.Equal(f.t, "social.coves.feed.vote", collection)

	// rkeys are TIDs, so sorting them puts the repo in the order a real PDS
	// lists it, and makes the page boundaries stable.
	rkeys := make([]string, 0, len(f.records))
	for rkey := range f.records {
		rkeys = append(rkeys, rkey)
	}
	sort.Strings(rkeys)

	from := 0
	if cursor != "" {
		require.Positivef(f.t, f.pageSize,
			"the fake repo is a single page; the cursor %q means the caller is paginating past the end", cursor)
		from = sort.SearchStrings(rkeys, cursor)
		require.Lessf(f.t, from, len(rkeys), "the caller resumed from cursor %q, which names no record", cursor)
	}
	to := len(rkeys)
	if f.pageSize > 0 && from+f.pageSize < to {
		to = from + f.pageSize
	}

	out := &pds.ListRecordsResponse{}
	for _, rkey := range rkeys[from:to] {
		rec := f.records[rkey]
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
	if to < len(rkeys) {
		out.Cursor = rkeys[to]
	}
	if len(f.rawRecords) > 0 {
		require.Zerof(f.t, f.pageSize, "rawRecords is single-page only; paginating them is not modelled")
		out.Records = append(out.Records, f.rawRecords...)
	}
	return out, f.afterList, nil
}

func (f *fakePDS) DID() string     { return f.did }
func (f *fakePDS) HostURL() string { return "https://pds.invalid" }

func (f *fakePDS) ApplyWrites(_ context.Context, writes []pds.Write, swapCommit string) (*pds.ApplyWritesResult, error) {
	afterBatch, batchResult, committed, failure := f.commitBatch(writes, swapCommit)
	if !committed {
		return nil, failure
	}
	if afterBatch != nil {
		afterBatch()
	}
	if failure != nil {
		return nil, failure
	}
	result := &pds.ApplyWritesResult{Results: []pds.WriteResult{
		{Op: pds.WriteOpDelete},
		{Op: pds.WriteOpCreate, URI: "at://" + f.did + "/social.coves.feed.vote/" + writes[1].RKey, CID: "bafycid" + writes[1].RKey},
	}}
	if batchResult != nil {
		result = batchResult(result)
	}
	return result, nil
}

// commitBatch applies a replacement under the lock and returns the hooks to
// run after it, so a hook never runs with the lock held.
func (f *fakePDS) commitBatch(writes []pds.Write, swapCommit string) (afterBatch func(), batchResult func(*pds.ApplyWritesResult) *pds.ApplyWritesResult, committed bool, failure error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.batchCalls++
	require.Empty(f.t, swapCommit)
	require.Len(f.t, writes, 2)
	require.Equal(f.t, pds.WriteOpDelete, writes[0].Op)
	require.Equal(f.t, pds.WriteOpCreate, writes[1].Op)
	require.NotEqual(f.t, writes[0].RKey, writes[1].RKey)
	_, err := syntax.ParseTID(writes[1].RKey)
	require.NoError(f.t, err)
	// Batch faults take precedence over legacy delete/create faults.
	// commitBeforeError and afterBatch simulate a committed write with a lost reply.
	failure = f.batchErr
	if failure == nil {
		failure = f.deleteErr
	}
	if failure == nil {
		failure = f.createErr
	}
	if failure != nil && !f.commitBeforeError {
		return nil, nil, false, failure
	}
	for _, write := range writes {
		require.Equal(f.t, "social.coves.feed.vote", write.Collection)
	}
	record, ok := writes[1].Record.(votes.VoteRecord)
	require.True(f.t, ok)
	delete(f.records, writes[0].RKey)
	f.records[writes[1].RKey] = record
	f.ops = append(f.ops, voteOp{kind: "delete", rkey: writes[0].RKey}, voteOp{kind: "create", rkey: writes[1].RKey})
	return f.afterBatch, f.batchResult, true, failure
}

func (f *fakePDS) GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	f.mutex.Lock()
	f.getCalls++
	getHook := f.getHook
	f.mutex.Unlock()
	_, bounded := ctx.Deadline()
	require.True(f.t, bounded, "reconciliation must have a bounded deadline")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if getHook != nil {
		if err := getHook(ctx, rkey); err != nil {
			return nil, err
		}
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	record, exists := f.records[rkey]
	if !exists {
		return nil, pds.ErrNotFound
	}
	result := &pds.RecordResponse{
		URI: "at://" + f.did + "/" + collection + "/" + rkey, CID: "bafycid" + rkey,
		Value: map[string]any{
			"$type": record.Type, "direction": record.Direction,
			"subject": map[string]any{"uri": record.Subject.URI, "cid": record.Subject.CID}, "createdAt": record.CreatedAt,
		},
	}
	if f.getTransform != nil {
		f.getTransform(result)
	}
	return result, nil
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

// newService wires the service to a fake PDS and a fake votes index.
//
// Most tests pass an empty index: the service's own recent writes then decide
// what "the existing vote" is, and these tests are about the decision made
// from that answer, not about where it came from.
func newService(t *testing.T, fake *fakePDS, repository *fakeVoteRepository) votes.Service {
	t.Helper()
	return newServiceForVoters(t, repository, fake)
}

// newServiceForVoters wires the service to one fake PDS per voter. The factory
// hands each session its own voter's repo, chosen by the session's DID, so a
// write made for one voter can never land in another's.
func newServiceForVoters(t *testing.T, repository votes.Repository, fakes ...*fakePDS) votes.Service {
	t.Helper()
	byDID := make(map[string]*fakePDS, len(fakes))
	for _, fake := range fakes {
		byDID[fake.did] = fake
	}
	return votes.NewServiceWithPDSFactory(repository, nil,
		func(_ context.Context, session *oauth.ClientSessionData) (votes.PDSClient, error) {
			fake, exists := byDID[session.AccountDID.String()]
			if !exists {
				return nil, fmt.Errorf("fake PDS: no repo for voter %s", session.AccountDID)
			}
			return fake, nil
		})
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
	svc := newService(t, fake, newFakeVoteRepository(t))

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
	svc := newService(t, fake, newFakeVoteRepository(t))
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
	svc := newService(t, fake, newFakeVoteRepository(t))
	ctx := context.Background()

	up, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)
	oldRKey := fake.creates()[0]

	down, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
	require.NoError(t, err)

	require.Equal(t, 1, fake.batchCalls, "direction replacement must be one atomic PDS batch")
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
	svc := newService(t, fake, newFakeVoteRepository(t))
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
		svc := newService(t, fake, newFakeVoteRepository(t))
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
		svc := newService(t, fake, newFakeVoteRepository(t))

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
			Subject: votes.StrongRef{CID: testSubjectCI}, Direction: "up",
		}, votes.ErrInvalidSubject},
		{"subject URI that is not an AT-URI", votes.CreateVoteRequest{
			Subject:   votes.StrongRef{URI: "https://example.com/post/1", CID: testSubjectCI},
			Direction: "up",
		}, votes.ErrInvalidSubject},
		{"subject with no CID", votes.CreateVoteRequest{
			Subject: votes.StrongRef{URI: testSubject}, Direction: "up",
		}, votes.ErrInvalidSubject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			svc := newService(t, fake, newFakeVoteRepository(t))

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
	svc := newService(t, fake, newFakeVoteRepository(t))

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
	svc := newService(t, fake, newFakeVoteRepository(t))
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
