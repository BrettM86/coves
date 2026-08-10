//go:build integration

package posts_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE GUARDS ON THE IRREVERSIBLE STEP.
//
// rematerialize_test.go proves the state machine walks. This file proves the
// thing that actually matters: that the delete DOES NOT HAPPEN unless a
// replacement is provably standing, RIGHT NOW, on every path into it — including
// the resumed paths, which are the ones a real 2am run will take.
//
// Every test here is written against a specific way the tool could destroy a
// post, and each names that way in its failure message. If one of them ever goes
// red, the correct response is to stop, not to adjust the assertion.

// guardHarness is one legacy post wired to a real ledger and faked repos, with
// every seam a test might need to break exposed.
type guardHarness struct {
	t       *testing.T
	db      *sql.DB
	ledger  posts.RematerializeLedger
	authors *fakeAuthorFactory
	writer  *spyAcceptanceWriter
	source  *fakeLegacySource
	tool    *posts.Rematerializer
	legacy  posts.LegacyPost
	rkey    string
	newURI  string
	newCID  string
}

func newGuardHarness(t *testing.T, authorDID string) *guardHarness {
	t.Helper()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(authorDID)
	writer := &spyAcceptanceWriter{}
	legacy := legacyPost(t, rematCommunityDID, authorDID)
	source := newFakeLegacySource(legacy)

	h := &guardHarness{
		t: t, db: db, ledger: ledger, authors: authors, writer: writer, source: source, legacy: legacy,
	}
	h.rkey = posts.RematerializeRkey(legacy.URI)
	h.newURI = "at://" + authorDID + "/" + posts.PostV2Collection + "/" + h.rkey
	h.newCID = deterministicCID(h.rkey)
	h.tool = &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
	}
	return h
}

func (h *guardHarness) state() posts.RematerializeState {
	h.t.Helper()
	row, found, err := h.ledger.Get(context.Background(), h.legacy.URI)
	require.NoError(h.t, err)
	require.True(h.t, found)
	return row.State
}

// dropLedgerRow removes the row entirely, forcing a genuine re-entry from
// `discovered` on the next pass. It is the only way to make the converge-by-read
// path actually execute against a repo that already holds the record.
func (h *guardHarness) dropLedgerRow() {
	h.t.Helper()
	_, err := h.db.ExecContext(context.Background(),
		`DELETE FROM post_rematerialization_ledger WHERE old_uri = $1`, h.legacy.URI)
	require.NoError(h.t, err)
}

// ---- BLOCKER 2: the acceptance is READ BACK, never inferred -----------------

// The acceptance write's own result is computed from the inputs it was handed,
// so comparing it to those inputs is a tautology that cannot fail. Only a read
// of the COMMUNITY's repo can say whether the acceptance stands.
func TestRematerialize_AcceptanceThatDoesNotStand_IsNotAcceptedOnTheWritersWord(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)

	// The writer reports a perfectly well-formed success — right rkey, right CID —
	// and the community repo holds nothing. This is what a lost commit, a proxy
	// that swallowed the write, or a bug in the writer looks like from here.
	h.writer.resultOverride = &posts.CommunityWriteResult{
		URI:  "at://" + rematCommunityDID + "/" + posts.AcceptanceCollection + "/" + posts.SubjectRkey(h.newURI),
		RKey: posts.SubjectRkey(h.newURI),
		CID:  "bafyreiacceptancethatneverlanded",
	}
	h.writer.suppressStanding = true

	_, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.Errorf(t, err,
		"the tool accepted the acceptance on the WRITER'S WORD. The writer computes its result from the arguments it was given, so checking that result "+
			"against those same arguments is SubjectRkey(X) != SubjectRkey(X) — a comparison that cannot fail. The acceptance must be read back out of the "+
			"community's repo, or the legacy record is deleted on the strength of a record that may not exist and the post drops out of its community")
	assert.Equalf(t, 0, h.source.deleteCount(h.legacy.URI),
		"the legacy record was deleted although no acceptance stands in the community repo")
	assert.Equalf(t, posts.RematerializePostV2Written, h.state(),
		"the row must stay at postv2_written: `verified` asserts the acceptance stands, and it does not")
}

// A record standing at the deterministic acceptance key is not enough — it has
// to pin OUR postv2. The rkey is a digest of the subject URI, so a record at the
// right key naming the wrong subject means someone else's write landed there.
func TestRematerialize_AcceptanceNamingADifferentSubject_RefusesToDelete(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	h.writer.afterWrite = func(w *spyAcceptanceWriter, cmd posts.CommunityWriteCommand) {
		w.repinStanding(cmd.PostURI, "at://did:plc:someoneelse2222222222222/social.coves.community.postv2/other", "bafyreisomethingelse")
	}

	_, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.Errorf(t, err,
		"an acceptance naming a DIFFERENT subject was accepted. Finding a record at the deterministic key proves only that a record is there; the subject "+
			"strongRef is what says the community accepted THIS post")
	assert.Equal(t, 0, h.source.deleteCount(h.legacy.URI))
	assert.Equal(t, posts.RematerializePostV2Written, h.state())
}

// The acceptance pinning a STALE CID is the same failure with a subtler shape:
// the community attests to a version of the post that is no longer there.
func TestRematerialize_AcceptancePinningADifferentCID_RefusesToDelete(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	h.writer.afterWrite = func(w *spyAcceptanceWriter, cmd posts.CommunityWriteCommand) {
		w.repinStanding(cmd.PostURI, cmd.PostURI, "bafyreiacidthepostnolongercarries")
	}

	_, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.Errorf(t, err,
		"an acceptance pinning a CID the postv2 does not carry was accepted; the community would be attesting to content that does not stand")
	assert.Equal(t, 0, h.source.deleteCount(h.legacy.URI))
}

// TEST GAP 3: the read-back's ERROR branch. A transport failure reading the
// acceptance is not permission to proceed.
func TestRematerialize_AcceptanceReadFails_RefusesToDelete(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	h.writer.readErr = errors.New("the community PDS returned 503 on getRecord")

	_, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.Errorf(t, err,
		"a FAILED read of the acceptance was treated as a passing verification. 'I could not ask' is not 'it is there', and the difference is a deleted post")
	assert.Equalf(t, 0, h.source.deleteCount(h.legacy.URI),
		"the legacy record was deleted after the verification read failed")
	assert.Equal(t, posts.RematerializePostV2Written, h.state())
}

// ---- TEST GAP 3: the postv2 read-back's error branch ------------------------

func TestRematerialize_PostV2ReadFails_RefusesToDelete(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	h.authors.repo(rematAuthorDID).getErrAt = map[string]error{
		h.rkey: errors.New("the author's PDS returned 503 on getRecord"),
	}

	_, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.Errorf(t, err,
		"a FAILED read of the postv2 was treated as a passing verification; substituting the ledger's remembered CID for a read that did not happen "+
			"is exactly the mutation this test exists to catch")
	assert.Equal(t, 0, h.source.deleteCount(h.legacy.URI))
}

// The postv2 DELETED between the write and the verify — an author withdrawing
// the post mid-migration, or a bug elsewhere.
func TestRematerialize_PostV2DeletedBetweenWriteAndVerify_RefusesToDelete(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	h.authors.repo(rematAuthorDID).deleteOnGet = map[string]bool{h.rkey: true}

	_, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.Errorf(t, err,
		"the postv2 was gone at verify time and the legacy record was deleted anyway; that destroys the only surviving copy of the post")
	assert.Equal(t, 0, h.source.deleteCount(h.legacy.URI))
	assert.Equal(t, posts.RematerializePostV2Written, h.state())
}

// ---- TEST GAP 5: the acceptance-WRITE failure paths -------------------------

func TestRematerialize_AcceptanceWriteFails_NoCheckpointNoDelete(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	h.writer.writeErr = errors.New("the community PDS returned 502 on putRecord")

	_, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.Errorf(t, err, "a failed acceptance write must surface as an error, not be swallowed")
	assert.Equalf(t, 0, h.source.deleteCount(h.legacy.URI),
		"the legacy record was deleted although the acceptance write failed")
	assert.Equalf(t, posts.RematerializePostV2Written, h.state(),
		"the row must stop at postv2_written — the postv2 exists, the acceptance does not")
}

func TestRematerialize_AcceptanceWriteReportsNothing_NoDelete(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	// An empty result: no URI, no rkey, no CID. Nothing was written.
	h.writer.resultOverride = &posts.CommunityWriteResult{}
	h.writer.suppressStanding = true

	_, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.Errorf(t, err, "an acceptance write reporting nothing at all must never license a delete")
	assert.Equal(t, 0, h.source.deleteCount(h.legacy.URI))
}

// ---- BLOCKER 4: the legacy record must not have changed ---------------------

// The listing snapshots every body at t0; the delete happens minutes to hours
// later. An edit landing in that gap is content that was never re-materialized.
func TestRematerialize_LegacyRecordEditedAfterConversion_RefusesToDelete(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	ctx := context.Background()

	// Drive to the migrated checkpoint by failing the first delete, so the row is
	// staged exactly where a crash-before-delete leaves it.
	h.source.deleteErr[h.legacy.URI] = errors.New("transient: 502 on delete")
	_, err := h.tool.RematerializeOne(ctx, h.legacy)
	require.Error(t, err)
	require.Equal(t, posts.RematerializeMigrated, h.state())
	deletesBefore := h.source.deleteCount(h.legacy.URI)

	// NOW an edit lands on the legacy record: same URI, new CID.
	h.source.setCurrentCID(h.legacy.URI, "bafyreianeditthatlandedaftert0")

	_, err = h.tool.RematerializeOne(ctx, h.legacy)
	require.Errorf(t, err,
		"the tool deleted a legacy record whose CID had changed since the postv2 was built from it. The maintenance window's 'stop writers' is never "+
			"perfect — an aggregator cron or a cached mobile session lands an edit — and deleting here destroys the newer content with no trace while the "+
			"run reports clean. Re-read the record and compare its CID before every delete")
	assert.Equalf(t, deletesBefore, h.source.deleteCount(h.legacy.URI),
		"no further delete may be attempted once the legacy record is known to have changed")
	assert.Equalf(t, posts.RematerializeMigrated, h.state(),
		"the row stays at the migrated checkpoint so a later pass can re-verify once the writer is stopped")
}

// The delete must ALSO carry the source CID as the PDS's own swap guard: the
// tool's check and its delete are two moments, and only the PDS can make them
// one.
func TestRematerialize_Delete_IsGuardedBySourceCID(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)

	state, err := h.tool.RematerializeOne(context.Background(), h.legacy)
	require.NoError(t, err)
	require.Equal(t, posts.RematerializeDone, state)

	guards := h.source.swapGuards()
	require.Lenf(t, guards, 1, "exactly one delete must have been issued")
	assert.Equalf(t, h.legacy.CID, guards[0],
		"the delete was sent with swap guard %q, not the legacy record's own CID %q. The guard is what makes the PDS refuse a delete of a version the "+
			"tool never saw", guards[0], h.legacy.CID)
}

// ---- BLOCKER 6: re-verification on EVERY resumed path -----------------------

// A row at `verified` records a check that passed at a moment now in the past.
// If the acceptance was withdrawn in the gap, the resumed run must notice.
func TestRematerialize_ResumeAtVerified_ReVerifiesTheAcceptance(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	ctx := context.Background()

	// Stage the row exactly at `verified`, with the postv2 standing and the
	// acceptance standing — the state a crash right after MarkVerified leaves.
	_, err := h.ledger.Discover(ctx, h.legacy.URI, rematCommunityDID, rematAuthorDID)
	require.NoError(t, err)
	require.NoError(t, h.ledger.RecordPostV2Written(ctx, h.legacy.URI, h.legacy.CID, h.newURI, h.newCID, h.rkey))
	require.NoError(t, h.ledger.MarkVerified(ctx, h.legacy.URI))
	h.authors.repo(rematAuthorDID).seedStanding(posts.PostV2Collection, h.rkey)
	h.writer.seedStandingAcceptance(rematCommunityDID, h.newURI, h.newCID)

	// ...and THEN the acceptance is withdrawn, as a moderator action or a bug
	// might do while the operator was asleep.
	h.writer.withdrawStanding(h.newURI)

	_, err = h.tool.RematerializeOne(ctx, h.legacy)
	require.Errorf(t, err,
		"a row resumed at `verified` was deleted with NO new reads. `verified` is a memory of a check, not a licence to destroy: the acceptance can be "+
			"withdrawn, the postv2 edited, in the gap the crash opened. Verification must be a fresh read on every path")
	assert.Equalf(t, 0, h.source.deleteCount(h.legacy.URI),
		"the legacy record was deleted although the acceptance no longer stands")
}

// The same for `migrated`, which is one step closer to the delete and therefore
// the more dangerous of the two.
func TestRematerialize_ResumeAtMigrated_ReVerifiesThePostV2(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	ctx := context.Background()

	_, err := h.ledger.Discover(ctx, h.legacy.URI, rematCommunityDID, rematAuthorDID)
	require.NoError(t, err)
	require.NoError(t, h.ledger.RecordPostV2Written(ctx, h.legacy.URI, h.legacy.CID, h.newURI, h.newCID, h.rkey))
	require.NoError(t, h.ledger.MarkVerified(ctx, h.legacy.URI))
	require.NoError(t, h.ledger.MarkMigrated(ctx, h.legacy.URI))
	h.authors.repo(rematAuthorDID).seedStanding(posts.PostV2Collection, h.rkey)
	h.writer.seedStandingAcceptance(rematCommunityDID, h.newURI, h.newCID)

	// The postv2 was edited in the gap: it now carries a CID the acceptance does
	// not pin.
	h.authors.repo(rematAuthorDID).getCIDAt[h.rkey] = "bafyreitheposthasbeenedited"

	_, err = h.tool.RematerializeOne(ctx, h.legacy)
	require.Errorf(t, err,
		"a row resumed at `migrated` deleted the legacy record without re-reading the postv2. The checkpoint means 'the delete was safe when we wrote "+
			"this', and the whole reason the checkpoint exists is that the process then died")
	assert.Equal(t, 0, h.source.deleteCount(h.legacy.URI))
}

// TEST GAP 8: every crash boundary, with the repo state the crash would have
// left, driven to its correct terminal outcome.
func TestRematerialize_ResumesCorrectlyFromEveryStepBoundary(t *testing.T) {
	t.Parallel()

	boundaries := []struct {
		name string
		// seed stages the ledger and the repos as a crash at this boundary would
		// have left them.
		seed      func(t *testing.T, h *guardHarness)
		wantState posts.RematerializeState
		// wantDeletes is how many delete attempts the resumed run should make.
		wantDeletes int
		// wantAcceptanceWrites is how many NEW acceptance writes it should make.
		wantAcceptanceWrites int
	}{
		{
			name:                 "crashed before anything was written (discovered)",
			seed:                 func(*testing.T, *guardHarness) {},
			wantState:            posts.RematerializeDone,
			wantDeletes:          1,
			wantAcceptanceWrites: 1,
		},
		{
			name: "crashed after the postv2 write, before the acceptance (postv2_written)",
			seed: func(t *testing.T, h *guardHarness) {
				ctx := context.Background()
				_, err := h.ledger.Discover(ctx, h.legacy.URI, rematCommunityDID, rematAuthorDID)
				require.NoError(t, err)
				require.NoError(t, h.ledger.RecordPostV2Written(ctx, h.legacy.URI, h.legacy.CID, h.newURI, h.newCID, h.rkey))
				h.authors.repo(rematAuthorDID).seedStanding(posts.PostV2Collection, h.rkey)
			},
			wantState:            posts.RematerializeDone,
			wantDeletes:          1,
			wantAcceptanceWrites: 1,
		},
		{
			name: "crashed after the acceptance, before the verify checkpoint (postv2_written, acceptance standing)",
			seed: func(t *testing.T, h *guardHarness) {
				ctx := context.Background()
				_, err := h.ledger.Discover(ctx, h.legacy.URI, rematCommunityDID, rematAuthorDID)
				require.NoError(t, err)
				require.NoError(t, h.ledger.RecordPostV2Written(ctx, h.legacy.URI, h.legacy.CID, h.newURI, h.newCID, h.rkey))
				h.authors.repo(rematAuthorDID).seedStanding(posts.PostV2Collection, h.rkey)
				h.writer.seedStandingAcceptance(rematCommunityDID, h.newURI, h.newCID)
			},
			wantState:   posts.RematerializeDone,
			wantDeletes: 1,
			// The acceptance already stands, but the tool re-fires the write; the
			// real writer SKIPS in that case and mints no new CID. What must not
			// happen is a second acceptance RECORD.
			wantAcceptanceWrites: 1,
		},
		{
			name: "crashed after the verify checkpoint, before migrated (verified)",
			seed: func(t *testing.T, h *guardHarness) {
				ctx := context.Background()
				_, err := h.ledger.Discover(ctx, h.legacy.URI, rematCommunityDID, rematAuthorDID)
				require.NoError(t, err)
				require.NoError(t, h.ledger.RecordPostV2Written(ctx, h.legacy.URI, h.legacy.CID, h.newURI, h.newCID, h.rkey))
				require.NoError(t, h.ledger.MarkVerified(ctx, h.legacy.URI))
				h.authors.repo(rematAuthorDID).seedStanding(posts.PostV2Collection, h.rkey)
				h.writer.seedStandingAcceptance(rematCommunityDID, h.newURI, h.newCID)
			},
			wantState:            posts.RematerializeDone,
			wantDeletes:          1,
			wantAcceptanceWrites: 0,
		},
		{
			name: "crashed after the migrated checkpoint, before the delete (migrated)",
			seed: func(t *testing.T, h *guardHarness) {
				ctx := context.Background()
				_, err := h.ledger.Discover(ctx, h.legacy.URI, rematCommunityDID, rematAuthorDID)
				require.NoError(t, err)
				require.NoError(t, h.ledger.RecordPostV2Written(ctx, h.legacy.URI, h.legacy.CID, h.newURI, h.newCID, h.rkey))
				require.NoError(t, h.ledger.MarkVerified(ctx, h.legacy.URI))
				require.NoError(t, h.ledger.MarkMigrated(ctx, h.legacy.URI))
				h.authors.repo(rematAuthorDID).seedStanding(posts.PostV2Collection, h.rkey)
				h.writer.seedStandingAcceptance(rematCommunityDID, h.newURI, h.newCID)
			},
			wantState:            posts.RematerializeDone,
			wantDeletes:          1,
			wantAcceptanceWrites: 0,
		},
		{
			name: "crashed after the delete, before MarkDone (migrated, record already gone)",
			seed: func(t *testing.T, h *guardHarness) {
				ctx := context.Background()
				_, err := h.ledger.Discover(ctx, h.legacy.URI, rematCommunityDID, rematAuthorDID)
				require.NoError(t, err)
				require.NoError(t, h.ledger.RecordPostV2Written(ctx, h.legacy.URI, h.legacy.CID, h.newURI, h.newCID, h.rkey))
				require.NoError(t, h.ledger.MarkVerified(ctx, h.legacy.URI))
				require.NoError(t, h.ledger.MarkMigrated(ctx, h.legacy.URI))
				h.authors.repo(rematAuthorDID).seedStanding(posts.PostV2Collection, h.rkey)
				h.writer.seedStandingAcceptance(rematCommunityDID, h.newURI, h.newCID)
				h.source.markGone(h.legacy.URI)
			},
			wantState: posts.RematerializeDone,
			// The record is ALREADY gone. A resumed run must not issue a delete it
			// does not owe — it must simply finish the ledger.
			wantDeletes:          0,
			wantAcceptanceWrites: 0,
		},
	}

	for _, tc := range boundaries {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newGuardHarness(t, rematAuthorDID)
			tc.seed(t, h)

			state, err := h.tool.RematerializeOne(context.Background(), h.legacy)
			require.NoErrorf(t, err, "a run resumed at this boundary must finish the record, not fail")

			assert.Equalf(t, tc.wantState, state, "the resumed run reached the wrong terminal state")
			assert.Equalf(t, tc.wantDeletes, h.source.deleteCount(h.legacy.URI),
				"the resumed run issued %d delete(s), want %d — a resume must re-do exactly the steps its predecessor owed and no others",
				h.source.deleteCount(h.legacy.URI), tc.wantDeletes)
			assert.Equalf(t, tc.wantAcceptanceWrites, len(h.writer.calls()),
				"the resumed run made %d acceptance write(s), want %d", len(h.writer.calls()), tc.wantAcceptanceWrites)
			assert.Equalf(t, 1, h.authors.repo(rematAuthorDID).recordCount(),
				"exactly one postv2 must stand; a resume that mints a second dangles every strongRef built from the first")
			assert.Equalf(t, 1, h.writer.acceptanceCount(),
				"exactly one acceptance must stand")
		})
	}
}

// ---- TEST GAP 1: genuine re-entry, so converge-by-read actually runs --------

// The "re-run is a no-op" tests return early on a `done` ledger row, making ZERO
// PDS calls — so createAuthorRecord's converge-by-read and sameRecordBody are
// never executed on the success path at any tier. Dropping the ledger row is
// what forces the tool back through step 1 against a repo that already holds the
// record, which is the only way that branch runs.
func TestRematerialize_ReEntryWithNoLedgerRow_ConvergesOnTheStandingPostV2(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	ctx := context.Background()

	// Pass 1 stops after the postv2 is written, so the legacy record still stands
	// and the repo already holds a postv2 at the deterministic rkey.
	h.writer.writeErr = errors.New("the community PDS returned 502 on the acceptance")
	_, err := h.tool.RematerializeOne(ctx, h.legacy)
	require.Error(t, err)
	require.Equal(t, posts.RematerializePostV2Written, h.state())

	firstCID := h.authors.repo(rematAuthorDID).standingCID(h.rkey)
	require.NotEmpty(t, firstCID)

	// Now lose the ledger entirely. The next pass re-enters at `discovered` and
	// must meet its own first attempt rather than minting a second post.
	h.dropLedgerRow()

	state, err := h.tool.RematerializeOne(ctx, h.legacy)
	require.NoErrorf(t, err,
		"a re-entry with no ledger row failed. This is the converge-by-read path: the create-only put meets ErrSwapConflict, the standing record is read "+
			"back, and its body is compared to the intended conversion. If sameRecordBody stopped comparing bodies faithfully, this is where it shows")
	assert.Equal(t, posts.RematerializeDone, state)

	assert.Equalf(t, 1, h.authors.repo(rematAuthorDID).recordCount(),
		"a re-entry minted a SECOND postv2. The deterministic rkey exists precisely so that a re-run converges on the record its first attempt wrote")
	assert.Equalf(t, firstCID, h.authors.repo(rematAuthorDID).standingCID(h.rkey),
		"the postv2's CID changed across a re-entry; every strongRef built from the first record now dangles")
	assert.Equalf(t, 1, h.writer.acceptanceCount(),
		"exactly one acceptance record must stand after a re-entry")
}

// TEST GAP 9: a re-run over a completed record must make EXACTLY no new calls —
// "at least one acceptance write" is satisfied by a second one.
func TestRematerialize_ReRunOverADoneRow_MakesExactlyNoNewCalls(t *testing.T) {
	t.Parallel()
	h := newGuardHarness(t, rematAuthorDID)
	ctx := context.Background()

	_, err := h.tool.RematerializeOne(ctx, h.legacy)
	require.NoError(t, err)

	acceptancesAfterFirst := len(h.writer.calls())
	deletesAfterFirst := h.source.deleteCount(h.legacy.URI)
	acceptanceCIDAfterFirst := h.writer.standingCID(h.newURI)
	require.Equal(t, 1, acceptancesAfterFirst)
	require.Equal(t, 1, deletesAfterFirst)

	state, err := h.tool.RematerializeOne(ctx, h.legacy)
	require.NoError(t, err)
	assert.Equal(t, posts.RematerializeDone, state)

	assert.Equalf(t, acceptancesAfterFirst, len(h.writer.calls()),
		"a re-run over a done row wrote another acceptance (%d → %d). A second write mints a fresh record CID and invalidates every reference to the one "+
			"it replaced", acceptancesAfterFirst, len(h.writer.calls()))
	assert.Equalf(t, deletesAfterFirst, h.source.deleteCount(h.legacy.URI),
		"a re-run over a done row attempted another delete (%d → %d)", deletesAfterFirst, h.source.deleteCount(h.legacy.URI))
	assert.Equalf(t, acceptanceCIDAfterFirst, h.writer.standingCID(h.newURI),
		"the standing acceptance's CID changed across a re-run")
	assert.Equalf(t, 1, h.authors.repo(rematAuthorDID).recordCount(), "exactly one postv2 must stand")
}

// ---- BLOCKER 5: the -community scope is enforced on the destructive path ----

// The reconcile pass drives rows the discovery pass never listed. Unscoped, a
// staged run for community A resumes — and deletes — community B's records.
func TestRematerialize_ScopedRun_RefusesARecordFromAnotherCommunity(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}

	otherCommunity := "did:plc:othercommunityotherco1"
	foreign := legacyPost(t, otherCommunity, rematAuthorDID)
	source := newFakeLegacySource(foreign)

	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
		CommunityScope: rematCommunityDID,
	}

	_, err := tool.RematerializeOne(context.Background(), foreign)
	require.Errorf(t, err,
		"a run scoped to %s processed a record belonging to %s. The staged rollout exists so that one community's posts can be migrated and DELETED while "+
			"every other community is untouched; a scope applied only at discovery does not do that, because the ledger reconcile pass reaches rows "+
			"discovery never listed", rematCommunityDID, otherCommunity)
	assert.Equalf(t, 0, source.deleteCount(foreign.URI),
		"a record outside the run's scope was deleted")
}

// A staged run finishing its own scope is a SUCCESS, reported separately from
// "the whole migration is done". Collapsing them makes every staged run exit
// non-zero and trains the operator to ignore the only machine-checkable gate on
// the irreversible §11 step 6.
func TestRematerialize_ScopedRun_ReportsScopeAndWholeMigrationSeparately(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	ctx := context.Background()

	// Another community's row is already on the ledger and NOT done: the whole
	// migration is unfinished.
	otherCommunity := "did:plc:othercommunityotherco2"
	otherURI := "at://" + otherCommunity + "/social.coves.community.post/" + testkit.TID()
	_, err := ledger.Discover(ctx, otherURI, otherCommunity, rematAuthorDID)
	require.NoError(t, err)

	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}
	mine := legacyPost(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(mine)

	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
		CommunityScope: rematCommunityDID,
	}

	report, err := tool.Run(ctx)
	require.NoError(t, err)

	assert.Truef(t, report.ScopeComplete,
		"the run finished every post in its scope but did not report ScopeComplete. A staged run that always reports failure is a staged run whose exit "+
			"code the operator learns to ignore — and that exit code is the gate on the irreversible legacy-removal step")
	assert.Equalf(t, rematCommunityDID, report.CommunityScope, "the report must name the scope it describes")
	assert.Equalf(t, 1, report.Discovered, "the scoped census must count only this community's rows, got %d", report.Discovered)

	assert.Falsef(t, report.Complete,
		"the run reported the WHOLE MIGRATION complete while another community still has an unfinished row. Complete is what gates the irreversible "+
			"legacy-removal follow-up, and a scoped census cannot see outside its scope")
	assert.GreaterOrEqualf(t, report.GlobalDiscovered, 2,
		"the global census must count rows outside the run's scope, got %d", report.GlobalDiscovered)
}

// ---- COMPLETION IS GATED ON A FINAL SOURCE RE-SCAN --------------------------

// A completion signal computed only from rows the run already discovered is
// circular: it cannot see a record written after the discovery pass.
func TestRematerialize_Complete_IsGatedOnARescanOfTheSource(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}

	first := legacyPost(t, rematCommunityDID, rematAuthorDID)
	late := legacyPost(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(first)
	// A record that appears only AFTER the discovery pass — a writer the
	// maintenance window did not stop.
	source.appendOnNextList(late)

	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
	}

	report, err := tool.Run(context.Background())
	require.NoError(t, err)

	assert.Equalf(t, 1, report.RemainingLegacy,
		"the final re-scan did not see the legacy record that appeared during the run, so 'complete' was computed from the tool's own memory of what it "+
			"had discovered — which can never contradict itself")
	assert.Falsef(t, report.Complete,
		"the run reported the migration complete while a legacy record still stands in the source. Complete gates the IRREVERSIBLE removal of the legacy "+
			"read/ingest surfaces; a post still living only as a community.post would silently vanish")
}

// ---- TEST GAP 6: the never-forge property, proven against WRITABLE others ---

// The original never-forge test held a factory with NO repos at all, so "did not
// re-author under another identity" was proven in a world where nothing was
// writable. Here the community's and the instance's repos ARE writable, and the
// assertion is positive: nothing but the author's repo received a write.
func TestRematerialize_NoCredentials_WritesNothingEvenWhenOtherIdentitiesAreWritable(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	writer := &spyAcceptanceWriter{}

	humanDID := "did:plc:humanhumanhumanhumanhum"
	instanceDID := "did:plc:instanceinstanceinstan"

	// A factory that WOULD hand out a perfectly writable repo for the community
	// and for the instance — the two identities a forging implementation would
	// reach for — and refuses only the author's own.
	authors := newFakeAuthorFactory()
	authors.repo(rematCommunityDID)
	authors.repo(instanceDID)
	authors.noCreds[humanDID] = true

	legacy := legacyPost(t, rematCommunityDID, humanDID)
	source := newFakeLegacySource(legacy)
	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
	}

	state, err := tool.RematerializeOne(context.Background(), legacy)
	require.NoError(t, err, "a no-creds record is an expected terminal outcome, not a run-failing error")
	require.Equal(t, posts.RematerializeFallbackLeftLegacy, state)

	for did, repo := range authors.repos {
		assert.Equalf(t, 0, repo.recordCount(),
			"a record was written into the repo of %s for a post whose own author could not be restored. Re-authoring under ANY other identity — the "+
				"community, the instance, an admin — is the §2 impersonation the whole author-owned flip exists to remove", did)
	}
	assert.Emptyf(t, writer.calls(), "no acceptance may be written for a post that was never re-authored")
	assert.Equalf(t, 0, source.deleteCount(legacy.URI),
		"the old community.post must SURVIVE: with no valid postv2 to replace it, deleting it destroys the post outright")
}

// ---- BLOCKER 3: a retryable credential failure fails the run ----------------

// A transient failure resolving credentials must NOT be written to the ledger as
// a terminal fallback. One aggregator authors most of production; a single blip
// recorded as a verdict sentences the whole corpus with no in-tool way back.
func TestRematerialize_RetryableCredentialFailure_FailsTheRunAndSentencesNothing(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	writer := &spyAcceptanceWriter{}
	aggregatorDID := "did:plc:aggregatoraggregatoragg"

	authors := newFakeAuthorFactory()
	authors.retryable[aggregatorDID] = true

	one := legacyPost(t, rematCommunityDID, aggregatorDID)
	two := legacyPost(t, rematCommunityDID, aggregatorDID)
	source := newFakeLegacySource(one, two)
	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
	}

	_, err := tool.Run(context.Background())
	require.Errorf(t, err,
		"a RETRYABLE credential failure was swallowed. It must fail the run loudly: the alternative is recording a terminal verdict on the strength of a "+
			"network blip, over every post the aggregator ever wrote")

	// The run stops at the first record, so the second may have no row at all.
	// What must be true of EVERY row that does exist is that none was sentenced.
	for _, legacy := range []posts.LegacyPost{one, two} {
		row, found, err := ledger.Get(context.Background(), legacy.URI)
		require.NoError(t, err)
		if !found {
			continue
		}
		assert.Equalf(t, posts.RematerializeDiscovered, row.State,
			"a transient credential failure left %s at %s. fallback_left_legacy is TERMINAL — ListResumable excludes it and MarkFallback will not "+
				"re-open it — so a blip written there is a permanent no-op over that post, forever", legacy.URI, row.State)
		assert.Falsef(t, posts.IsFallback(row.State),
			"a network blip was written as a terminal verdict on %s", legacy.URI)
	}
}

// The census can be made to STOP before mutating anything, which is the
// operator's defence against a run that "succeeded" having migrated almost
// nothing.
func TestRematerialize_AbortOnFallback_StopsBeforeAnyRepoIsMutated(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	writer := &spyAcceptanceWriter{}
	authors := newFakeAuthorFactory()

	goodDID := "did:plc:hascredshascredshascreds"
	strandedDID := "did:plc:nocredsnocredsnocredsnoc"
	authors.repo(goodDID)
	authors.noCreds[strandedDID] = true

	good := legacyPost(t, rematCommunityDID, goodDID)
	stranded := legacyPost(t, rematCommunityDID, strandedDID)
	source := newFakeLegacySource(good, stranded)

	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
		AbortOnFallback: true,
	}

	_, err := tool.Run(context.Background())
	require.Errorf(t, err, "with AbortOnFallback set, a census that stranded a post must stop the run")
	assert.Containsf(t, err.Error(), strandedDID,
		"the abort message must name the stranded author(s); the operator's next action is to re-authorize them")

	assert.Equalf(t, 0, source.deleteCount(good.URI),
		"the run mutated a repo after the census had already found a stranded post; the whole point of aborting is that nothing has happened yet")
	assert.Equalf(t, 0, authors.repo(goodDID).recordCount(), "no postv2 may be written once the run has decided to abort")
	assert.Emptyf(t, writer.calls(), "no acceptance may be written once the run has decided to abort")
}

// ---- the fallback recovery path --------------------------------------------

// A fallback row is terminal, deliberately. It must not be IRREVERSIBLE: the
// operator re-authorizes the author and needs a supported way to retry.
func TestRematerialize_ReopenFallback_LetsAReAuthorizedAuthorBeRetried(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	ctx := context.Background()
	authors := newFakeAuthorFactory()
	writer := &spyAcceptanceWriter{}

	authorDID := "did:plc:reauthorizedreauthoriz1"
	authors.noCreds[authorDID] = true
	legacy := legacyPost(t, rematCommunityDID, authorDID)
	source := newFakeLegacySource(legacy)
	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
	}

	_, err := tool.Run(ctx)
	require.NoError(t, err)
	row, _, err := ledger.Get(ctx, legacy.URI)
	require.NoError(t, err)
	require.Equal(t, posts.RematerializeFallbackLeftLegacy, row.State)

	// The operator re-authorizes the author, then reopens.
	delete(authors.noCreds, authorDID)
	authors.repo(authorDID)

	moved, err := ledger.ReopenFallback(ctx, rematCommunityDID)
	require.NoError(t, err)
	assert.Equalf(t, 1, moved, "ReopenFallback must report how many rows it moved, so the operator can tell it did anything at all")

	// The documented workflow is `-reopen-fallbacks` and then RE-RUN THE BINARY, so
	// the retry is a fresh process: a terminal credential verdict is cached for the
	// life of one run on purpose, and re-resolving it mid-run would mean thousands
	// of extra token rotations for an author who has none.
	retry := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(),
	}
	state, err := retry.RematerializeOne(ctx, legacy)
	require.NoErrorf(t, err,
		"a reopened row could not be retried. Without a way back out of fallback_left_legacy, a single expired aggregator grant makes every subsequent "+
			"run a permanent no-op and the only remedy is hand-written SQL against a production table")
	assert.Equal(t, posts.RematerializeDone, state)
}

func TestRematerialize_ReopenFallback_NeverResurrectsADoneRow(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	h := newGuardHarness(t, rematAuthorDID)
	_ = ledger
	ctx := context.Background()

	_, err := h.tool.RematerializeOne(ctx, h.legacy)
	require.NoError(t, err)
	require.Equal(t, posts.RematerializeDone, h.state())

	moved, err := h.ledger.ReopenFallback(ctx, rematCommunityDID)
	require.NoError(t, err)
	assert.Equalf(t, 0, moved, "ReopenFallback moved a row that was not a fallback")
	assert.Equalf(t, posts.RematerializeDone, h.state(),
		"a done row was moved back out of done. `done` means the legacy record has been DELETED; re-running against it cannot bring the record back, so "+
			"nothing may ever move a row out of it")
}

// ---- blob handling ---------------------------------------------------------

// The blob presence probe's error branch: "I could not ask" is not "it is not
// there", and neither is permission to delete the community's only copy.
func TestRematerialize_BlobProbeFails_RefusesToDelete(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}

	legacy := legacyPostWithBlob(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(legacy)
	blobClient := &fakeBlobClient{
		bytes:      map[string][]byte{embeddedBlobCID(): embeddedBlobBytes},
		presentErr: errors.New("the author's PDS returned 503 on getBlob"),
	}

	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(), Blobs: blobClient,
	}

	_, err := tool.RematerializeOne(context.Background(), legacy)
	require.Errorf(t, err,
		"a FAILED blob presence probe was treated as either answer. Reported as absence it refuses a healthy record; reported as presence it licenses "+
			"deleting the last record that keeps the community's only copy of the bytes alive")
	assert.Equal(t, 0, source.deleteCount(legacy.URI))
}

// The blob bytes come from the COMMUNITY's PDS — the repo that actually holds
// them — not from the author's host, which is the same machine only for as long
// as every account on the instance shares one PDS.
func TestRematerialize_FetchesCommunityBlobsFromTheCommunitysHost(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID).host = "http://author-pds.invalid"
	writer := &spyAcceptanceWriter{communityHost: "http://community-pds.invalid"}

	legacy := legacyPostWithBlob(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(legacy)
	blobClient := &fakeBlobClient{bytes: map[string][]byte{embeddedBlobCID(): embeddedBlobBytes}}

	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(), Blobs: blobClient,
	}

	_, err := tool.RematerializeOne(context.Background(), legacy)
	require.NoError(t, err)

	require.NotEmpty(t, blobClient.fetches, "no blob was fetched at all")
	assert.Equalf(t, "http://community-pds.invalid", blobClient.fetches[0].host,
		"the community's blob was fetched from %q — the AUTHOR's PDS. A blob lives in the repo that holds it, and fetching the community's bytes from "+
			"the author's host works only while both accounts happen to share one PDS", blobClient.fetches[0].host)
	assert.Equalf(t, rematCommunityDID, blobClient.fetches[0].did, "the blob must be fetched from the community's repo")
}

// The blob presence check must run on a RESUMED path too. Running it only in the
// first-pass branch meant a resume re-entering at postv2_written deleted the
// legacy record — and the last reference keeping the community's blobs alive —
// having checked nothing about the media.
func TestRematerialize_ResumeAtPostV2Written_StillVerifiesTheBlobs(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ledger := postgres.NewRematerializeLedger(db)
	ctx := context.Background()
	authors := newFakeAuthorFactory()
	authors.repo(rematAuthorDID)
	writer := &spyAcceptanceWriter{}

	legacy := legacyPostWithBlob(t, rematCommunityDID, rematAuthorDID)
	source := newFakeLegacySource(legacy)
	rkey := posts.RematerializeRkey(legacy.URI)
	newURI := "at://" + rematAuthorDID + "/" + posts.PostV2Collection + "/" + rkey
	newCID := deterministicCID(rkey)

	_, err := ledger.Discover(ctx, legacy.URI, rematCommunityDID, rematAuthorDID)
	require.NoError(t, err)
	require.NoError(t, ledger.RecordPostV2Written(ctx, legacy.URI, legacy.CID, newURI, newCID, rkey))
	authors.repo(rematAuthorDID).seedStanding(posts.PostV2Collection, rkey)

	// The blob is NOT in the author's repo — the first pass died before the
	// upload landed.
	blobClient := &fakeBlobClient{bytes: map[string][]byte{embeddedBlobCID(): embeddedBlobBytes}, absent: true}

	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authors.factory(),
		Acceptances: writer, CommunityRepos: writer.repos(), Blobs: blobClient,
	}

	_, err = tool.RematerializeOne(ctx, legacy)
	require.Errorf(t, err,
		"a resume re-entering at postv2_written deleted the legacy record without checking that its media had been copied. The blob check ran only in "+
			"the first-pass branch, so exactly the run that crashed mid-upload was the one that skipped it — and the community's copy becomes "+
			"garbage-collectable the moment the legacy record goes")
	assert.Equal(t, 0, source.deleteCount(legacy.URI))
}

// ---- fixtures --------------------------------------------------------------

// legacyPostWithBlob stages a legacy record whose embed references a blob living
// in the community's blob store.
// The blob's CID is the one a CONTENT-ADDRESSED store mints for these exact
// bytes, which is the property the whole "carry the ref through unchanged"
// design rests on: re-uploading identical bytes yields the identical CID, so the
// postv2 may keep the community's reference verbatim.
var embeddedBlobBytes = []byte("PNGDATA-embedded")

func embeddedBlobCID() string { return blobCIDFor(embeddedBlobBytes) }

func legacyPostWithBlob(t *testing.T, communityDID, authorDID string) posts.LegacyPost {
	t.Helper()
	legacy := legacyPost(t, communityDID, authorDID)
	legacy.RawRecord["embed"] = map[string]any{
		"$type": "social.coves.embed.images",
		"images": []any{map[string]any{
			"alt": "a picture",
			"image": map[string]any{
				"$type":    "blob",
				"ref":      map[string]any{"$link": embeddedBlobCID()},
				"mimeType": "image/png",
				"size":     float64(len(embeddedBlobBytes)),
			},
		}},
	}
	return legacy
}

// fakeBlobClient records what was fetched from where, and can fail or answer
// absent on demand.
type fakeBlobClient struct {
	bytes      map[string][]byte
	fetches    []blobFetch
	presentErr error
	absent     bool
}

type blobFetch struct {
	host string
	did  string
	cid  string
}

func (c *fakeBlobClient) Fetch(_ context.Context, host, did, cid string) ([]byte, error) {
	c.fetches = append(c.fetches, blobFetch{host: host, did: did, cid: cid})
	data, ok := c.bytes[cid]
	if !ok {
		return nil, fmt.Errorf("no such blob %s", cid)
	}
	return data, nil
}

func (c *fakeBlobClient) Present(_ context.Context, _, _, _ string) (bool, error) {
	if c.presentErr != nil {
		return false, c.presentErr
	}
	return !c.absent, nil
}

// unusedOAuthSession keeps the indigo oauth import honest for the factory
// signature the fakes satisfy.
var _ = func(_ *oauth.ClientSessionData) {}

// unusedPDSTypes keeps the pds import honest across build-tag permutations.
var _ = pds.ErrNotFound
