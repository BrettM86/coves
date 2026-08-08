//go:build integration

package posts_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The re-materialization state machine, against a REAL migration-037 ledger and
// FAKE repos (docs/PRD_AUTHOR_OWNED_POSTS.md §11 the rev-2.8 deploy runbook).
//
// # WHAT THIS TIER PROVES, AND WHY THE REPOS ARE FAKED
//
// The tool's three load-bearing safety properties are all ORDERING and
// IDEMPOTENCE properties — they live in the orchestration, not in the PDS:
//
//   1. rkey stability (pinned purely at T0 in rematerialize_rkey_test.go) — a
//      re-run computes the SAME postv2 key and createAuthorRecord converges by
//      read instead of minting a second post.
//   2. VERIFY BEFORE DELETE — the old community.post record is deleted only after
//      the postv2 AND its acceptance are confirmed to pin the same CID. Delete
//      one instant too early and a crash loses a live post.
//   3. THE FALLBACK NEVER FORGES — a post whose author credentials cannot be
//      restored is left as legacy, never re-authored under a forged signature,
//      and the run refuses to report "complete" while any such row survives.
//
// The ledger is real because resume reads it; the repos are faked because the
// faults these tests inject — a delete that fails once, a postv2 that changed CID
// under the verify, an author with no credentials — are precisely the ones a real
// PDS will not produce on demand. The write PRIMITIVES the tool reuses
// (createAuthorRecord's converge-by-read, WriteAcceptance's skip) have their own
// real-PDS coverage; the outer real-stack proof is tests/e2e/rematerialize_
// contract_test.go.
//
// # THE TOOL DOES NOT RE-DECIDE (mirrors the scriptedDecider trick)
//
// The Rematerializer holds a CommunityRecordWriter — the DIRECT acceptance writer
// — and NOT an AcceptanceEngine or an AdmissionDecider. That is the whole reason
// the acceptance is written by WriteAcceptance here: routing through the engine
// would re-run admission, and a since-banned author's LIVE production post would
// be REJECTED, rewriting history (§11 step 4). service_writeforward_test.go pins
// the same "no re-decision" property on the fast path with a scriptedDecider that
// would refuse; here the property is structural — there is no decider to consult
// — and TestRematerialize_UsesDirectAcceptanceWriter_NeverReDecides makes it a
// behaviour.

// ---- fakes ---------------------------------------------------------------

func strPtr(s string) *string { return &s }

// deterministicCID is the CID a fake author repo assigns a postv2 at a given
// rkey: stable across calls, so a re-run's converge-by-read reads the SAME CID
// back rather than a fresh one. Real PDS CIDs are content-addressed and share
// this property for identical bytes.
func deterministicCID(rkey string) string { return "bafyreipostv2" + rkey }

// fakeAuthorRepo models the one PDS behaviour the tool's idempotence rests on:
// a create-only put (swapRecord "") of a record that already stands is refused
// with ErrSwapConflict, and the standing record is read back instead.
type fakeAuthorRepo struct {
	did string

	mu       sync.Mutex
	records  map[string]*pds.RecordResponse // rkey -> record
	putErr   error                          // one-shot injected put failure
	getCIDAt map[string]string              // rkey -> CID GetRecord should report (verify-window override)
}

func newFakeAuthorRepo(did string) *fakeAuthorRepo {
	return &fakeAuthorRepo{did: did, records: map[string]*pds.RecordResponse{}, getCIDAt: map[string]string{}}
}

func (r *fakeAuthorRepo) recordCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

func (r *fakeAuthorRepo) GetRecord(_ context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[rkey]
	if !ok {
		return nil, pds.ErrNotFound
	}
	if override, ok := r.getCIDAt[rkey]; ok {
		clone := *rec
		clone.CID = override
		return &clone, nil
	}
	return rec, nil
}

func (r *fakeAuthorRepo) PutRecordWithCommit(_ context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.putErr != nil {
		err := r.putErr
		r.putErr = nil
		return nil, err
	}

	if _, exists := r.records[rkey]; exists && swapRecord == "" {
		// The create-only guard: the record is already here. This is exactly what
		// a re-run of a re-materialized post meets, and createAuthorRecord answers
		// it by reading the standing record back rather than minting a second.
		return nil, pds.ErrSwapConflict
	}

	uri := "at://" + r.did + "/" + collection + "/" + rkey
	cid := deterministicCID(rkey)
	body, _ := record.(map[string]any)
	r.records[rkey] = &pds.RecordResponse{URI: uri, CID: cid, Value: body}
	return &pds.RecordCommit{URI: uri, CID: cid, CommitRev: "3krematputxxx"}, nil
}

func (r *fakeAuthorRepo) DeleteRecord(_ context.Context, collection, rkey string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, rkey)
	return nil
}

func (r *fakeAuthorRepo) UploadBlob(_ context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	return nil, fmt.Errorf("fakeAuthorRepo.UploadBlob: not used by re-materialization")
}

func (r *fakeAuthorRepo) DID() string { return r.did }

// seedStanding puts a postv2 record straight into the repo, so a resume test can
// stage "the first run already wrote this".
func (r *fakeAuthorRepo) seedStanding(collection, rkey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uri := "at://" + r.did + "/" + collection + "/" + rkey
	r.records[rkey] = &pds.RecordResponse{URI: uri, CID: deterministicCID(rkey), Value: map[string]any{}}
}

// fakeAuthorFactory hands out fake author repos by DID, and answers
// ErrNoAuthorCredentials for the DIDs marked as unrestorable — exactly what the
// production factory does for an aggregator whose stored session is gone.
type fakeAuthorFactory struct {
	repos   map[string]*fakeAuthorRepo
	noCreds map[string]bool
}

func newFakeAuthorFactory() *fakeAuthorFactory {
	return &fakeAuthorFactory{repos: map[string]*fakeAuthorRepo{}, noCreds: map[string]bool{}}
}

func (f *fakeAuthorFactory) repo(did string) *fakeAuthorRepo {
	if r, ok := f.repos[did]; ok {
		return r
	}
	r := newFakeAuthorRepo(did)
	f.repos[did] = r
	return r
}

func (f *fakeAuthorFactory) factory() posts.AuthorRepoFactory {
	return func(_ context.Context, authorDID string, _ *oauth.ClientSessionData) (posts.AuthorRepo, error) {
		if f.noCreds[authorDID] {
			return nil, fmt.Errorf("resuming the stored session of %s: %w", authorDID, posts.ErrNoAuthorCredentials)
		}
		r, ok := f.repos[authorDID]
		if !ok {
			return nil, fmt.Errorf("opening the repository of %s: %w", authorDID, posts.ErrNoAuthorCredentials)
		}
		return r, nil
	}
}

// spyAcceptanceWriter records every WriteAcceptance and returns a standing
// acceptance that pins whatever CID it was handed. A repeat call reports Skipped,
// the way the real writer does when the acceptance already pins the target CID —
// so a re-run mints no new CID.
type spyAcceptanceWriter struct {
	mu             sync.Mutex
	acceptanceCmds []posts.CommunityWriteCommand
	writeErr       error // one-shot injected failure
	otherCalled    []string
}

func (s *spyAcceptanceWriter) WriteAcceptance(_ context.Context, cmd posts.CommunityWriteCommand) (posts.CommunityWriteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.writeErr != nil {
		err := s.writeErr
		s.writeErr = nil
		return posts.CommunityWriteResult{}, err
	}

	repeat := false
	for _, prior := range s.acceptanceCmds {
		if prior.PostURI == cmd.PostURI && prior.PostCID == cmd.PostCID {
			repeat = true
		}
	}
	s.acceptanceCmds = append(s.acceptanceCmds, cmd)

	rkey := posts.SubjectRkey(cmd.PostURI)
	return posts.CommunityWriteResult{
		URI:     "at://" + cmd.CommunityDID + "/social.coves.community.acceptance/" + rkey,
		RKey:    rkey,
		CID:     "bafyreiacceptance" + rkey,
		Rev:     "3krematacceptxx",
		Skipped: repeat,
	}, nil
}

// The other four methods exist only to satisfy CommunityRecordWriter. The tool
// must never call them: a re-materialized post is accepted, not removed,
// restored, repinned, or withdrawn. Each records that it was reached so a test
// can prove it was not.
func (s *spyAcceptanceWriter) WriteRemoval(_ context.Context, _ posts.CommunityRemovalCommand) (posts.CommunityWriteResult, error) {
	s.note("WriteRemoval")
	return posts.CommunityWriteResult{}, nil
}
func (s *spyAcceptanceWriter) RestoreAcceptance(_ context.Context, _ posts.CommunityWriteCommand) (posts.CommunityWriteResult, error) {
	s.note("RestoreAcceptance")
	return posts.CommunityWriteResult{}, nil
}
func (s *spyAcceptanceWriter) RepinAcceptance(_ context.Context, _ posts.CommunityWriteCommand) (posts.CommunityWriteResult, error) {
	s.note("RepinAcceptance")
	return posts.CommunityWriteResult{}, nil
}
func (s *spyAcceptanceWriter) DeleteAcceptance(_ context.Context, _ posts.CommunityAcceptanceDeleteCommand) (posts.CommunityWriteResult, error) {
	s.note("DeleteAcceptance")
	return posts.CommunityWriteResult{}, nil
}
func (s *spyAcceptanceWriter) note(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.otherCalled = append(s.otherCalled, method)
}
func (s *spyAcceptanceWriter) calls() []posts.CommunityWriteCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]posts.CommunityWriteCommand(nil), s.acceptanceCmds...)
}

// fakeLegacySource yields staged legacy records and records deletes. It can fail
// the FIRST delete of a given URI once, so a crash-after-checkpoint is
// reproducible, and it treats a delete of an already-gone record as success —
// the idempotence the resumed delete leans on.
type fakeLegacySource struct {
	mu        sync.Mutex
	posts     []posts.LegacyPost
	deleted   map[string]int
	deleteErr map[string]error
}

func newFakeLegacySource(ps ...posts.LegacyPost) *fakeLegacySource {
	return &fakeLegacySource{posts: ps, deleted: map[string]int{}, deleteErr: map[string]error{}}
}

func (s *fakeLegacySource) ListLegacyPosts(_ context.Context) ([]posts.LegacyPost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]posts.LegacyPost(nil), s.posts...), nil
}

func (s *fakeLegacySource) DeleteLegacyPost(_ context.Context, legacy posts.LegacyPost) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted[legacy.URI]++
	if err, ok := s.deleteErr[legacy.URI]; ok {
		delete(s.deleteErr, legacy.URI)
		return err
	}
	return nil
}

func (s *fakeLegacySource) deleteCount(uri string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted[uri]
}

// legacyPost stages one deprecated community.post, keyed by a unique rkey so
// parallel tests never collide on the ledger's old_uri primary key.
func legacyPost(t *testing.T, communityDID, authorDID string) posts.LegacyPost {
	t.Helper()
	rkey := testkit.TID()
	oldURI := "at://" + communityDID + "/social.coves.community.post/" + rkey
	title := "legacy " + testkit.UniqueID(t)
	return posts.LegacyPost{
		URI:          oldURI,
		CID:          "bafyreilegacy" + rkey,
		CommunityDID: communityDID,
		AuthorDID:    authorDID,
		Record: posts.PostRecord{
			Type:      "social.coves.community.post",
			Community: communityDID,
			Author:    authorDID,
			Title:     strPtr(title),
			Content:   strPtr("words the author is accountable for"),
			CreatedAt: "2026-01-02T03:04:05Z",
		},
	}
}

const (
	rematCommunityDID = "did:plc:cccccccccccccccccccccccc"
	rematAuthorDID    = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
)

// ---- tests ---------------------------------------------------------------

func TestRematerialize_HappyPath_WalksToDoneVerifyBeforeDelete(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}
	legacy := legacyPost(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(legacy)

	tool := &posts.Rematerializer{Source: source, Ledger: ledger, AuthorRepos: authors.factory(), Acceptances: writer}

	state, err := tool.RematerializeOne(context.Background(), legacy)
	require.NoError(t, err)
	require.Equalf(t, posts.RematerializeDone, state, "a fresh legacy record must walk all the way to done")

	// The postv2 was written into the AUTHOR's repo at the deterministic rkey.
	authorRepo := authors.repo(rematAuthorDID)
	wantRkey := posts.RematerializeRkey(legacy.URI)
	newURI := "at://" + rematAuthorDID + "/social.coves.community.postv2/" + wantRkey
	assert.Equalf(t, 1, authorRepo.recordCount(), "exactly one postv2 must have been written")

	// The acceptance was written DIRECT, into the community's repo, pinning the
	// NEW postv2 CID — not the old community.post CID.
	calls := writer.calls()
	require.Lenf(t, calls, 1, "exactly one acceptance must have been written")
	assert.Equal(t, rematCommunityDID, calls[0].CommunityDID)
	assert.Equalf(t, newURI, calls[0].PostURI, "the acceptance must pin the NEW postv2 URI in the author's repo")
	assert.Equalf(t, deterministicCID(wantRkey), calls[0].PostCID,
		"the acceptance must pin the NEW postv2 CID; pinning the old community.post CID would attest to content that no longer exists")
	assert.Emptyf(t, writer.otherCalled, "a re-materialized post is accepted only — no removal/restore/repin/withdraw")

	// The old record was deleted — and the ledger proves the delete came AFTER the
	// migrated checkpoint (the row is done, which is only reachable through it).
	assert.Equalf(t, 1, source.deleteCount(legacy.URI), "the old community.post must be deleted exactly once")

	row, found, err := ledger.Get(context.Background(), legacy.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, posts.RematerializeDone, row.State)
	assert.Equalf(t, newURI, row.NewURI, "the ledger must record the postv2 URI it wrote")
	assert.Equalf(t, deterministicCID(wantRkey), row.NewCID, "the ledger must record the postv2 CID it pinned")
	assert.Equal(t, wantRkey, row.NewRkey)
}

func TestRematerialize_ReRun_IsAPureNoOp(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}
	legacy := legacyPost(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(legacy)
	tool := &posts.Rematerializer{Source: source, Ledger: ledger, AuthorRepos: authors.factory(), Acceptances: writer}

	first, err := tool.RematerializeOne(context.Background(), legacy)
	require.NoError(t, err)
	require.Equal(t, posts.RematerializeDone, first)

	// Run it again against a record already fully migrated. Nothing new may
	// happen: the same rkey converges (no second postv2), the acceptance skips
	// (no new CID), the old record is already gone.
	second, err := tool.RematerializeOne(context.Background(), legacy)
	require.NoError(t, err)
	assert.Equal(t, posts.RematerializeDone, second)

	assert.Equalf(t, 1, authors.repo(rematAuthorDID).recordCount(),
		"a re-run must not mint a second postv2 — the deterministic rkey converges on the first record")

	calls := writer.calls()
	require.GreaterOrEqual(t, len(calls), 1)
	assert.Truef(t, calls[len(calls)-1].PostCID == deterministicCID(posts.RematerializeRkey(legacy.URI)),
		"a re-run's acceptance must still pin the same CID; a fresh CID would dangle every reference to the acceptance")
	// The old record was already gone; a re-run's delete (if attempted) is a no-op
	// success, never an error, and never resurrects the record.
	row, _, err := ledger.Get(context.Background(), legacy.URI)
	require.NoError(t, err)
	assert.Equal(t, posts.RematerializeDone, row.State)
}

func TestRematerialize_ResumeAfterDeleteFailure_RetriesOnlyTheDelete(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}
	legacy := legacyPost(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(legacy)
	source.deleteErr[legacy.URI] = fmt.Errorf("transient: the community PDS returned 502 on delete")
	tool := &posts.Rematerializer{Source: source, Ledger: ledger, AuthorRepos: authors.factory(), Acceptances: writer}

	// First pass: everything succeeds up to the delete, which fails once. The row
	// must stop at migrated — the checkpoint BEFORE the delete — never done.
	_, err := tool.RematerializeOne(context.Background(), legacy)
	require.Errorf(t, err, "a failed delete must surface as an error, not be swallowed into a false 'done'")

	row, found, err := ledger.Get(context.Background(), legacy.URI)
	require.NoError(t, err)
	require.True(t, found)
	require.Equalf(t, posts.RematerializeMigrated, row.State,
		"a crash on the delete must leave the row at the migrated checkpoint: postv2 and acceptance verified, old record still present")
	require.Equalf(t, 1, source.deleteCount(legacy.URI), "the delete was attempted once and failed")

	postV2Before := authors.repo(rematAuthorDID).recordCount()
	acceptancesBefore := len(writer.calls())

	// Resume. Only the delete should do new work; the postv2 must not be rewritten
	// and no new acceptance CID may be minted.
	state, err := tool.RematerializeOne(context.Background(), legacy)
	require.NoError(t, err)
	assert.Equal(t, posts.RematerializeDone, state)

	assert.Equalf(t, postV2Before, authors.repo(rematAuthorDID).recordCount(),
		"resume rewrote the postv2; the migrated checkpoint exists so a crash retries ONLY the delete")
	for _, c := range writer.calls()[acceptancesBefore:] {
		assert.Equalf(t, deterministicCID(posts.RematerializeRkey(legacy.URI)), c.PostCID,
			"resume minted a new acceptance CID; a re-fire must converge on the same pinned CID")
	}
	assert.Equalf(t, 2, source.deleteCount(legacy.URI), "resume must retry the delete that failed")
}

func TestRematerialize_CIDMismatch_DoesNotCheckpointOrDelete(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authorRepo := authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}
	legacy := legacyPost(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(legacy)
	tool := &posts.Rematerializer{Source: source, Ledger: ledger, AuthorRepos: authors.factory(), Acceptances: writer}

	// The verify re-read of the postv2 comes back with a DIFFERENT CID than the
	// one the acceptance pinned — a concurrent edit landing in the write→verify
	// window. Verification must fail, and the tool must NOT checkpoint and must
	// NOT delete: deleting here would destroy the only copy of a post whose new
	// acceptance points at content that no longer stands.
	wantRkey := posts.RematerializeRkey(legacy.URI)
	authorRepo.getCIDAt[wantRkey] = "bafyreianeditlandedmidverify"

	_, err := tool.RematerializeOne(context.Background(), legacy)
	require.Errorf(t, err, "a CID mismatch at verify must be an error, not a silent success")

	assert.Equalf(t, 0, source.deleteCount(legacy.URI),
		"VERIFY BEFORE DELETE: the old record must not be deleted when the postv2 CID no longer matches the acceptance")

	row, found, err := ledger.Get(context.Background(), legacy.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEqualf(t, posts.RematerializeDone, row.State, "a mismatched record must never reach done")
	assert.NotEqualf(t, posts.RematerializeMigrated, row.State,
		"a mismatched record must never reach the migrated checkpoint — migrated asserts the delete is safe, and it is not")
}

func TestRematerialize_NoCredentials_LeavesLegacyNeverForges(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	writer := &spyAcceptanceWriter{}

	// A human author whose repo credentials cannot be restored non-interactively
	// (there is no stored session to resume — see the RED report's credential
	// finding). The tool must leave the post as legacy, write NOTHING, and delete
	// NOTHING — never an admin-forged postv2, which would reintroduce the §2
	// impersonation liability the whole flip exists to remove.
	humanDID := "did:plc:humanhumanhumanhumanhum"
	authors.noCreds[humanDID] = true
	legacy := legacyPost(t, rematCommunityDID, humanDID)
	source := newFakeLegacySource(legacy)
	tool := &posts.Rematerializer{Source: source, Ledger: ledger, AuthorRepos: authors.factory(), Acceptances: writer}

	state, err := tool.RematerializeOne(context.Background(), legacy)
	require.NoError(t, err, "a no-creds record is an expected terminal outcome, not a run-failing error")
	assert.Equalf(t, posts.RematerializeFallbackLeftLegacy, state,
		"an author with no restorable credentials must land in fallback_left_legacy")
	assert.Truef(t, posts.IsFallback(state), "the terminal state must be a fallback state the census can gate on")

	assert.Emptyf(t, writer.calls(), "no acceptance may be written for a post that was never re-authored")
	assert.Equalf(t, 0, source.deleteCount(legacy.URI),
		"the old community.post must SURVIVE: with no valid postv2 to replace it, deleting it destroys the post outright")

	row, found, err := ledger.Get(context.Background(), legacy.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, posts.RematerializeFallbackLeftLegacy, row.State)
	assert.Emptyf(t, row.NewURI, "a fallback row wrote no postv2, so it can name none")
}

func TestRematerialize_Run_CensusGatesCompletionWhileFallbackSurvives(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()

	// One migratable aggregator post and one no-creds human post in the same run.
	aggregatorDID := "did:plc:aggregatoraggregatoragg"
	humanDID := "did:plc:humantwohumantwohumantwo"
	authors.repo(aggregatorDID)
	authors.noCreds[humanDID] = true

	migratable := legacyPost(t, rematCommunityDID, aggregatorDID)
	stranded := legacyPost(t, rematCommunityDID, humanDID)
	source := newFakeLegacySource(migratable, stranded)
	writer := &spyAcceptanceWriter{}
	tool := &posts.Rematerializer{Source: source, Ledger: ledger, AuthorRepos: authors.factory(), Acceptances: writer}

	report, err := tool.Run(context.Background())
	require.NoError(t, err)

	// The migratable one completed; the stranded one is a surviving fallback, so
	// the run REFUSES to report complete — the gate on the manual legacy-removal
	// follow-up (§11 step 6).
	assert.Falsef(t, report.Complete,
		"the run must not report complete while a fallback row survives; the operator uses Complete to gate the irreversible legacy-removal step")
	assert.GreaterOrEqualf(t, report.Fallbacks, 1, "the census must count the surviving fallback")
	assert.GreaterOrEqualf(t, report.Done, 1, "the migratable record must still have reached done")

	// The stranded post's old record must be untouched.
	assert.Equalf(t, 0, source.deleteCount(stranded.URI), "a fallback post's old record must never be deleted by the run")
	assert.Equalf(t, 1, source.deleteCount(migratable.URI), "the migratable post's old record is deleted")
}

func TestRematerialize_UsesDirectAcceptanceWriter_NeverReDecides(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}
	legacy := legacyPost(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(legacy)

	// The Rematerializer's community seam is a CommunityRecordWriter — WriteAcceptance
	// direct. There is no AdmissionDecider or AcceptanceEngine field to route
	// through, so the tool CANNOT re-run admission. If it could, a since-banned
	// author's live post would be rejected here, silently deleting it from a
	// community it currently sits in. This mirrors service_writeforward_test.go's
	// scriptedDecider trick, made structural: the acceptance is written for the
	// post's content unconditionally.
	tool := &posts.Rematerializer{Source: source, Ledger: ledger, AuthorRepos: authors.factory(), Acceptances: writer}

	state, err := tool.RematerializeOne(context.Background(), legacy)
	require.NoError(t, err)
	require.Equal(t, posts.RematerializeDone, state)

	calls := writer.calls()
	require.Lenf(t, calls, 1, "the acceptance must be written exactly once, through the direct writer")
	assert.Emptyf(t, writer.otherCalled,
		"the tool called a moderation writer (%v); re-materialization only ever writes an acceptance, and never re-decides removal", writer.otherCalled)
}
