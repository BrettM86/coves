package posts

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/pds"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The acceptance engine's routing matrix (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6).
//
// ProcessAdmission is a two-input decision: the row's STATUS and the policy's
// VERDICT together choose what gets written, and neither alone is enough. The
// same "refused" answer means an AppView-local rejection on a pending row and a
// removal commit on a pending_reacceptance row, because §5.5 is explicit that a
// failed re-acceptance is a removal — a rejection there would suppress an
// acceptance that is currently standing and that the community published.
//
// WHAT THESE TESTS ASSERT IS THE CALL SEQUENCE, NOT JUST THE OUTCOME. Every
// interesting failure of an engine like this is a write that happened when it
// should not have: a rejection recorded for a post whose credentials expired, a
// removal committed for a row that was already removed, a repo stamped before
// the PDS write it claims to describe actually committed. An outcome value
// cannot see any of those. The recorded sequence can, so the fakes below share
// one recorder and the assertions name the exact calls in the exact order.
//
// The outer contract — that this really writes records into a real repo — is
// engine_contract_test.go against a live PDS.

const (
	engineCommunityDID = "did:plc:cccccccccccccccccccccccc"
	enginePostURI      = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.community.postv2/3kjzl5kcb2s2v"

	engineIndexedCID  = "bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	engineAcceptedCID = "bafyreibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	engineCommitRev  = "3kjzl5kcb2s2v"
	engineRecordCID  = "bafyreiddddddddddddddddddddddddddddddddddddddddddddddddd"
	engineRemovalCID = "bafyreieeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// engineRecorder is the shared call log. One log across all four collaborators
// is what makes ORDER assertable — "the row was stamped after the commit
// landed" is a claim about two different fakes.
type engineRecorder struct{ calls []string }

func (r *engineRecorder) record(name string) { r.calls = append(r.calls, name) }

// mutatingCalls are every call that changes state somewhere. A deferred pass
// must make none of them.
var mutatingCalls = []string{
	"WriteAcceptance", "WriteRemoval", "RestoreAcceptance", "RepinAcceptance",
	"ApplyAcceptance", "ApplyRemoval", "RecordRejection", "UpsertPending",
	"ApplyAcceptanceDelete", "ApplyRemovalDelete", "RepinAcceptedCID",
}

// assertWroteNothing fails if the pass touched anything at all.
func assertWroteNothing(t *testing.T, rec *engineRecorder) {
	t.Helper()
	for _, call := range rec.calls {
		for _, mutation := range mutatingCalls {
			assert.NotEqualf(t, mutation, call,
				"this pass must write nothing anywhere; it called %s (whole sequence: %v)",
				call, rec.calls)
		}
	}
}

// fakeDecider is the admission policy. It answers with whatever it was handed:
// an admission, a refusal code, or the undecided pair admitPost returns when a
// lookup failed.
type fakeDecider struct {
	rec *engineRecorder

	code DecisionCode
	err  error

	// cause is the VALUE-shaped undecided answer: Cause set, Code empty, and
	// a NIL error. It is a separate field from err so the fake can produce
	// each of the two undecided shapes independently — a policy bug (or a
	// future refusal path) can hand back exactly this, and the engine must
	// treat it as undecided rather than as a verdict.
	cause error

	lastCommunityDID string
	lastPostURI      string
}

func (d *fakeDecider) DecideAdmission(_ context.Context, communityDID, postURI string) (AdmissionDecision, error) {
	d.rec.record("DecideAdmission")
	d.lastCommunityDID = communityDID
	d.lastPostURI = postURI
	if d.err != nil {
		// Exactly admitPost's undecided shape: the error is returned AND
		// carried on the decision, so a caller inspecting only the value still
		// sees "not admitted".
		return AdmissionDecision{Cause: d.err}, d.err
	}
	if d.cause != nil {
		return AdmissionDecision{Cause: d.cause}, nil
	}
	return AdmissionDecision{Code: d.code}, nil
}

// fakeWriter is the community repo. Its results are canned; what matters is
// which method the engine reached for and with what.
type fakeWriter struct {
	rec *engineRecorder

	acceptanceResult CommunityWriteResult
	removalResult    CommunityWriteResult

	// acceptanceErrs is consumed one per WriteAcceptance call, so a test can
	// fail the first attempt and let the retry through.
	acceptanceErrs []error
	removalErr     error

	acceptanceCmds []CommunityWriteCommand
	removalCmds    []CommunityRemovalCommand
}

func (w *fakeWriter) WriteAcceptance(_ context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	w.rec.record("WriteAcceptance")
	w.acceptanceCmds = append(w.acceptanceCmds, cmd)
	if len(w.acceptanceErrs) > 0 {
		err := w.acceptanceErrs[0]
		w.acceptanceErrs = w.acceptanceErrs[1:]
		if err != nil {
			return CommunityWriteResult{}, err
		}
	}
	return w.acceptanceResult, nil
}

func (w *fakeWriter) WriteRemoval(_ context.Context, cmd CommunityRemovalCommand) (CommunityWriteResult, error) {
	w.rec.record("WriteRemoval")
	w.removalCmds = append(w.removalCmds, cmd)
	if w.removalErr != nil {
		return CommunityWriteResult{}, w.removalErr
	}
	return w.removalResult, nil
}

func (w *fakeWriter) RestoreAcceptance(_ context.Context, _ CommunityWriteCommand) (CommunityWriteResult, error) {
	w.rec.record("RestoreAcceptance")
	return CommunityWriteResult{}, nil
}

func (w *fakeWriter) RepinAcceptance(_ context.Context, _ CommunityWriteCommand) (CommunityWriteResult, error) {
	w.rec.record("RepinAcceptance")
	return CommunityWriteResult{}, nil
}

// DeleteAcceptance is the author-deletion sweep, which the ENGINE never
// performs — an author withdrawing their own post is not a verdict. It records
// its call for the same reason every other method here does: the recorder is
// how this file asserts which repo write each verdict produced, so an engine
// that started withdrawing acceptances would show up as an unexpected entry
// rather than as a silent behaviour change.
func (w *fakeWriter) DeleteAcceptance(_ context.Context, _ CommunityAcceptanceDeleteCommand) (CommunityWriteResult, error) {
	w.rec.record("DeleteAcceptance")
	return CommunityWriteResult{}, nil
}

// fakeRefresher counts forced credential renewals.
type fakeRefresher struct {
	rec *engineRecorder
	err error
}

func (r *fakeRefresher) RefreshCommunityCredentials(_ context.Context, _ string) error {
	r.rec.record("RefreshCommunityCredentials")
	return r.err
}

// fakeAdmissions is the admission row store. It answers Get with one row and
// records every mutation with its command, so the assertions can check what the
// engine believed it was recording.
type fakeAdmissions struct {
	rec *engineRecorder

	row    *Admission
	getErr error

	acceptanceResult AdmissionResult
	removalResult    AdmissionResult
	rejectionResult  AdmissionResult

	acceptanceErr error
	removalErr    error
	rejectionErr  error

	acceptanceCmds []ApplyAcceptanceCommand
	removalCmds    []ApplyRemovalCommand
	rejectionCmds  []RecordRejectionCommand
}

func (a *fakeAdmissions) Get(_ context.Context, _, _ string) (*Admission, error) {
	a.rec.record("Get")
	if a.getErr != nil {
		return nil, a.getErr
	}
	return a.row, nil
}

func (a *fakeAdmissions) ApplyAcceptance(_ context.Context, cmd ApplyAcceptanceCommand) (AdmissionResult, error) {
	a.rec.record("ApplyAcceptance")
	a.acceptanceCmds = append(a.acceptanceCmds, cmd)
	return a.acceptanceResult, a.acceptanceErr
}

func (a *fakeAdmissions) ApplyRemoval(_ context.Context, cmd ApplyRemovalCommand) (AdmissionResult, error) {
	a.rec.record("ApplyRemoval")
	a.removalCmds = append(a.removalCmds, cmd)
	return a.removalResult, a.removalErr
}

func (a *fakeAdmissions) RecordRejection(_ context.Context, cmd RecordRejectionCommand) (AdmissionResult, error) {
	a.rec.record("RecordRejection")
	a.rejectionCmds = append(a.rejectionCmds, cmd)
	return a.rejectionResult, a.rejectionErr
}

func (a *fakeAdmissions) UpsertPending(_ context.Context, _ UpsertPendingCommand) (AdmissionResult, error) {
	a.rec.record("UpsertPending")
	return AdmissionResult{}, nil
}

func (a *fakeAdmissions) ApplyAcceptanceDelete(_ context.Context, _ CommunityDeleteCommand) (AdmissionResult, error) {
	a.rec.record("ApplyAcceptanceDelete")
	return AdmissionResult{}, nil
}

func (a *fakeAdmissions) ApplyRemovalDelete(_ context.Context, _ CommunityDeleteCommand) (AdmissionResult, error) {
	a.rec.record("ApplyRemovalDelete")
	return AdmissionResult{}, nil
}

func (a *fakeAdmissions) RepinAcceptedCID(_ context.Context, _ RepinAcceptanceCommand) (AdmissionResult, error) {
	a.rec.record("RepinAcceptedCID")
	return AdmissionResult{}, nil
}

func (a *fakeAdmissions) GetByPostURIs(_ context.Context, _ []string) (map[string][]*Admission, error) {
	a.rec.record("GetByPostURIs")
	return nil, nil
}

func (a *fakeAdmissions) ListByStatusForCommunity(_ context.Context, _ string, _ AdmissionStatus, _ int, _ *string) ([]*Admission, *string, error) {
	a.rec.record("ListByStatusForCommunity")
	return nil, nil, nil
}

// ListPendingSubjects is the queue driver's backlog query. The engine never
// calls it — the driver does, and drives the engine with what it returns — so
// this exists to satisfy the interface and records the call, which is itself an
// assertion: an engine that started listing its own work would show up here.
func (a *fakeAdmissions) ListPendingSubjects(_ context.Context, _ int) ([]PendingSubject, error) {
	a.rec.record("ListPendingSubjects")
	return nil, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// engineHarness is the engine plus every collaborator, all sharing one call log.
type engineHarness struct {
	engine     *AcceptanceEngine
	rec        *engineRecorder
	admissions *fakeAdmissions
	decider    *fakeDecider
	writer     *fakeWriter
	refresher  *fakeRefresher
}

// newEngineHarness builds an engine over a row in the given status holding the
// given indexed CID. A nil evaluatedCID means the AppView has not yet decoded
// the post's content.
func newEngineHarness(status AdmissionStatus, evaluatedCID *string) *engineHarness {
	rec := &engineRecorder{}

	row := &Admission{
		CommunityDID: engineCommunityDID,
		PostURI:      enginePostURI,
		Status:       status,
		EvaluatedCID: evaluatedCID,
		Redrivable:   true,
		CreatedAt:    time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
	if status == AdmissionStatusPendingReacceptance || status == AdmissionStatusAccepted {
		acceptanceRkey := SubjectRkey(enginePostURI)
		acceptanceURI := "at://" + engineCommunityDID + "/" + AcceptanceCollection + "/" + acceptanceRkey
		acceptedCID := engineAcceptedCID
		row.AcceptanceURI = &acceptanceURI
		row.AcceptanceRkey = &acceptanceRkey
		row.AcceptedCID = &acceptedCID
	}

	admissions := &fakeAdmissions{
		rec:              rec,
		row:              row,
		acceptanceResult: AdmissionResult{Outcome: AdmissionApplied, Admission: row},
		removalResult:    AdmissionResult{Outcome: AdmissionApplied, Admission: row},
		rejectionResult:  AdmissionResult{Outcome: AdmissionApplied, Admission: row},
	}

	acceptanceRkey := SubjectRkey(enginePostURI)
	writer := &fakeWriter{
		rec: rec,
		acceptanceResult: CommunityWriteResult{
			URI:  "at://" + engineCommunityDID + "/" + AcceptanceCollection + "/" + acceptanceRkey,
			RKey: acceptanceRkey,
			CID:  engineRecordCID,
			Rev:  engineCommitRev,
		},
		removalResult: CommunityWriteResult{
			URI:  "at://" + engineCommunityDID + "/" + RemovalCollection + "/" + acceptanceRkey,
			RKey: acceptanceRkey,
			CID:  engineRemovalCID,
			Rev:  engineCommitRev,
		},
	}

	decider := &fakeDecider{rec: rec}
	refresher := &fakeRefresher{rec: rec}

	return &engineHarness{
		engine:     NewAcceptanceEngine(admissions, decider, writer, refresher),
		rec:        rec,
		admissions: admissions,
		decider:    decider,
		writer:     writer,
		refresher:  refresher,
	}
}

func (h *engineHarness) process(t *testing.T) (EngineOutcome, error) {
	t.Helper()
	return h.engine.ProcessAdmission(context.Background(), engineCommunityDID, enginePostURI)
}

func cidPtr(cid string) *string { return &cid }

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

func TestEngine_PendingAdmittedCreatesTheAcceptance(t *testing.T) {
	t.Parallel()

	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))

	outcome, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, EngineAccepted, outcome)

	// The whole sequence, in order. Reading the row FIRST is what makes the
	// status half of the routing decision come from the database rather than
	// from whoever queued the subject; stamping the row LAST is what keeps the
	// AppView from claiming an acceptance the PDS never committed.
	assert.Equal(t, []string{"Get", "DecideAdmission", "WriteAcceptance", "ApplyAcceptance"}, h.rec.calls)

	require.Len(t, h.writer.acceptanceCmds, 1)
	assert.Equal(t, CommunityWriteCommand{
		CommunityDID: engineCommunityDID,
		PostURI:      enginePostURI,
		PostCID:      engineIndexedCID,
	}, h.writer.acceptanceCmds[0],
		"the acceptance must pin the CID the AppView has INDEXED; pinning anything else is an "+
			"acceptance of content nobody evaluated")

	require.Len(t, h.admissions.acceptanceCmds, 1)
	assert.Equal(t, ApplyAcceptanceCommand{
		CommunityDID:   engineCommunityDID,
		PostURI:        enginePostURI,
		AcceptanceURI:  "at://" + engineCommunityDID + "/" + AcceptanceCollection + "/" + SubjectRkey(enginePostURI),
		AcceptanceRkey: SubjectRkey(enginePostURI),
		PinnedCID:      engineIndexedCID,
		Watermark:      CommunityWatermark{Rev: engineCommitRev},
	}, h.admissions.acceptanceCmds[0],
		"the row is stamped with the rev the write actually COMMITTED in — that is the §5.2 "+
			"watermark, and inventing one would let this optimistic update outrank the firehose "+
			"copy of a later event")
}

func TestEngine_PendingRefusedRecordsALocalRejectionAndWritesNoRecord(t *testing.T) {
	t.Parallel()

	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
	h.decider.code = DecisionSpam

	outcome, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, EngineRejected, outcome)

	// NO PDS WRITE. §3.3 is explicit: a submission refused before it was ever
	// accepted writes no record, because spam must not bloat the community's
	// repository. This is the assertion that keeps that promise.
	assert.Equal(t, []string{"Get", "DecideAdmission", "RecordRejection"}, h.rec.calls)
	assert.Emptyf(t, h.writer.acceptanceCmds, "a refused submission must not reach the community's repo")
	assert.Empty(t, h.writer.removalCmds)

	require.Len(t, h.admissions.rejectionCmds, 1)
	assert.Equal(t, RecordRejectionCommand{
		CommunityDID: engineCommunityDID,
		PostURI:      enginePostURI,
		DecisionCode: string(DecisionSpam),
		// The CID the verdict JUDGED. The repository lands the rejection only
		// on a pending row still holding it, so an author who edited between
		// the read and the write gets fresh content judged fresh rather than
		// condemned by a verdict about something else.
		JudgedCID: engineIndexedCID,
		// A policy refusal is terminal. Leaving this true would have the
		// dead-letter redrive pass retry a decision that will never change.
		Redrivable: false,
	}, h.admissions.rejectionCmds[0])
}

func TestEngine_PendingReacceptanceAdmittedUpdatesTheAcceptanceInPlace(t *testing.T) {
	t.Parallel()

	h := newEngineHarness(AdmissionStatusPendingReacceptance, cidPtr(engineIndexedCID))

	outcome, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, EngineAccepted, outcome)

	assert.Equal(t, []string{"Get", "DecideAdmission", "WriteAcceptance", "ApplyAcceptance"}, h.rec.calls)

	// SAME RKEY, NEW CID. The record key is derived from the subject, so
	// re-acceptance is an update of the record that already exists rather than
	// a second acceptance — every reference to the acceptance URI stays valid
	// and the community's repo does not accumulate one record per edit.
	require.Len(t, h.writer.acceptanceCmds, 1)
	assert.Equal(t, engineIndexedCID, h.writer.acceptanceCmds[0].PostCID,
		"re-acceptance pins the NEW content; pinning the old CID would leave the post pending forever")

	require.Len(t, h.admissions.acceptanceCmds, 1)
	assert.Equal(t, SubjectRkey(enginePostURI), h.admissions.acceptanceCmds[0].AcceptanceRkey)
	assert.Equal(t, engineIndexedCID, h.admissions.acceptanceCmds[0].PinnedCID)
}

func TestEngine_PendingReacceptanceRefusedRemovesRatherThanRejects(t *testing.T) {
	t.Parallel()

	h := newEngineHarness(AdmissionStatusPendingReacceptance, cidPtr(engineIndexedCID))
	h.decider.code = DecisionRuleViolation

	outcome, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, EngineRemoved, outcome)

	// §5.5: a failed RE-acceptance is a removal, not a rejection. An acceptance
	// is currently standing in the community's repo and was published to the
	// firehose; recording an AppView-local rejection would leave that record in
	// place, so peers would keep rendering the post while this AppView hid it.
	assert.Equal(t, []string{"Get", "DecideAdmission", "WriteRemoval", "ApplyRemoval"}, h.rec.calls)
	assert.Emptyf(t, h.admissions.rejectionCmds,
		"a standing acceptance must be withdrawn with a removal record, never with a local rejection")

	require.Len(t, h.writer.removalCmds, 1)
	assert.Equal(t, CommunityRemovalCommand{
		CommunityDID: engineCommunityDID,
		PostURI:      enginePostURI,
		PostCID:      engineIndexedCID,
		Code:         DecisionRuleViolation,
	}, h.writer.removalCmds[0])

	require.Len(t, h.admissions.removalCmds, 1)
	assert.Equal(t, ApplyRemovalCommand{
		CommunityDID: engineCommunityDID,
		PostURI:      enginePostURI,
		DecisionCode: string(DecisionRuleViolation),
		Watermark:    CommunityWatermark{Rev: engineCommitRev},
	}, h.admissions.removalCmds[0])
}

func TestEngine_UndecidedWritesNothingAnywhere(t *testing.T) {
	t.Parallel()

	// An undecided answer is an infrastructure failure — a community lookup or
	// a ban lookup that could not be reached — dressed as neither an admission
	// nor a refusal. Recording ANY verdict from it would turn a Postgres blip
	// into a permanent decision about someone's post.
	for _, status := range []AdmissionStatus{
		AdmissionStatusPending,
		AdmissionStatusPendingReacceptance,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()

			lookupFailed := errors.New("failed to look up community membership: connection refused")
			h := newEngineHarness(status, cidPtr(engineIndexedCID))
			h.decider.err = lookupFailed

			outcome, err := h.process(t)
			assert.Equal(t, EngineDeferred, outcome)
			require.Error(t, err, "a decision that could not be made is a genuine failure and must be visible")
			assert.ErrorIs(t, err, lookupFailed)

			assertWroteNothing(t, h.rec)
		})
	}
}

func TestEngine_ValueShapedUndecidedWritesNothingAnywhere(t *testing.T) {
	t.Parallel()

	// The undecided answer arriving as a VALUE: Cause set, Code empty, error
	// nil. Admitted() is false for a refusal and for an undecided answer
	// alike, and the empty code is the only thing telling them apart — an
	// answer with neither a code nor a clean bill is one nothing may be
	// written from. A decider bug that produced this shape must cost a
	// deferral, never a verdict.
	lookupFailed := errors.New("aggregator authorization: connection refused")
	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
	h.decider.cause = lookupFailed

	outcome, err := h.process(t)
	assert.Equal(t, EngineDeferred, outcome)
	require.Error(t, err, "a policy that returned no verdict is a genuine failure and must be visible")
	assert.ErrorIs(t, err, lookupFailed)

	assertWroteNothing(t, h.rec)
}

func TestEngine_AdmittedWithNoIndexedCIDDefers(t *testing.T) {
	t.Parallel()

	// An acceptance's subject is a strongRef, and a strongRef without a CID
	// pins nothing — which is precisely the guarantee the acceptance exists to
	// make. A row with no evaluated_cid is one whose content the AppView has
	// not decoded yet, so there is nothing to accept and the answer is "later".
	for _, status := range []AdmissionStatus{
		AdmissionStatusPending,
		AdmissionStatusPendingReacceptance,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()

			h := newEngineHarness(status, nil)

			outcome, err := h.process(t)
			require.NoError(t, err, "an un-indexed subject is the engine working, not a failure")
			assert.Equal(t, EngineDeferred, outcome)

			assertWroteNothing(t, h.rec)
		})
	}
}

func TestEngine_SettledRowsAreSkippedWithoutBeingReDecided(t *testing.T) {
	t.Parallel()

	// The engine's queue can hand it the same subject twice — a redrive, a
	// duplicate from an overlapping feed, a notify racing the firehose. A row
	// that is already settled must not be re-decided, and the strongest reason
	// is `removed`: §5.5 makes removal terminal against everything except a
	// moderator restore at a strictly greater watermark, so an engine that
	// re-ran policy on a removed row would launder it straight back into the
	// feeds it was removed from.
	for _, status := range []AdmissionStatus{
		AdmissionStatusAccepted,
		AdmissionStatusRejected,
		AdmissionStatusRemoved,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()

			h := newEngineHarness(status, cidPtr(engineIndexedCID))

			outcome, err := h.process(t)
			require.NoError(t, err)
			assert.Equal(t, EngineDeferred, outcome)

			assert.Equal(t, []string{"Get"}, h.rec.calls,
				"a settled row costs one read and nothing else — the policy must not even be consulted")
		})
	}
}

func TestEngine_MissingRowDefersWithoutDeciding(t *testing.T) {
	t.Parallel()

	// A subject the engine was handed but that has no row. Whether that is
	// reported as an error is the engine's business; what is NOT negotiable is
	// that it decides nothing, because there is no evaluated CID to judge and
	// no status to route on.
	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
	h.admissions.row = nil
	h.admissions.getErr = ErrNotFound

	outcome, _ := h.process(t)
	assert.Equal(t, EngineDeferred, outcome)
	assertWroteNothing(t, h.rec)
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func TestEngine_RetriesOnceWithFreshCredentials(t *testing.T) {
	t.Parallel()

	// A community's PDS access token expires on a schedule that has nothing to
	// do with moderation. One forced renewal and one retry is the difference
	// between a decision that lands and a decision that has to be redriven.
	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
	h.writer.acceptanceErrs = []error{fmt.Errorf("putRecord: %w: token expired", pds.ErrUnauthorized)}

	outcome, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, EngineAccepted, outcome)

	assert.Equal(t, []string{
		"Get", "DecideAdmission",
		"WriteAcceptance", "RefreshCommunityCredentials", "WriteAcceptance",
		"ApplyAcceptance",
	}, h.rec.calls)
}

func TestEngine_CredentialFailureDefersAndNeverRejects(t *testing.T) {
	t.Parallel()

	// THE MOST DANGEROUS CONFUSION IN THE WHOLE COMPONENT. "I could not write
	// the acceptance" and "this post is not acceptable" are opposite facts, and
	// an engine that recorded the second when it meant the first would answer
	// the author with a permanent verdict — and set redrivable=false on it, so
	// nothing would ever retry.
	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
	expired := fmt.Errorf("putRecord: %w: token expired", pds.ErrUnauthorized)
	h.writer.acceptanceErrs = []error{expired, expired}

	outcome, err := h.process(t)
	assert.Equal(t, EngineDeferred, outcome)
	require.Error(t, err, "credentials that stay dead after a forced renewal are an operator problem and must surface")
	assert.ErrorIs(t, err, pds.ErrUnauthorized)

	assert.Equal(t, []string{
		"Get", "DecideAdmission",
		"WriteAcceptance", "RefreshCommunityCredentials", "WriteAcceptance",
	}, h.rec.calls)
	assert.Emptyf(t, h.admissions.rejectionCmds,
		"an authentication failure must NEVER be recorded as a rejection of the post")
	assert.Empty(t, h.admissions.acceptanceCmds,
		"nothing committed, so nothing may be stamped on the row")
}

func TestEngine_RetriesTheCredentialFailureAtMostOnce(t *testing.T) {
	t.Parallel()

	// Bounded, not persistent. A community whose credentials are genuinely gone
	// would otherwise have every pass in the queue spin against its PDS.
	h := newEngineHarness(AdmissionStatusPendingReacceptance, cidPtr(engineIndexedCID))
	h.decider.code = DecisionSpam
	h.writer.removalErr = fmt.Errorf("applyWrites: %w", pds.ErrUnauthorized)

	outcome, _ := h.process(t)
	assert.Equal(t, EngineDeferred, outcome)

	refreshes := 0
	for _, call := range h.rec.calls {
		if call == "RefreshCommunityCredentials" {
			refreshes++
		}
	}
	assert.Equalf(t, 1, refreshes, "exactly one forced renewal per pass; got %d (sequence: %v)",
		refreshes, h.rec.calls)
	assert.Empty(t, h.admissions.removalCmds, "nothing committed, so nothing may be stamped on the row")
}

// ---------------------------------------------------------------------------
// The optimistic repository update
// ---------------------------------------------------------------------------

func TestEngine_TreatsRepositorySkipsAsSuccess(t *testing.T) {
	t.Parallel()

	// The engine stamps the row itself rather than waiting for the firehose
	// copy of its own commit. When the firehose gets there first the repository
	// answers skipped_stale; when the row has moved on it answers
	// skipped_terminal. Neither is a failure — the record IS in the community's
	// repo, the firehose is the authority on what the repo says, and reporting
	// an error here would dead-letter a write that succeeded.
	for _, outcome := range []AdmissionOutcome{AdmissionSkippedStale, AdmissionSkippedTerminal} {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()

			h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
			h.admissions.acceptanceResult = AdmissionResult{Outcome: outcome, Admission: h.admissions.row}

			got, err := h.process(t)
			require.NoErrorf(t, err, "a skipped stamp means the firehose won a race, not that the write failed")
			assert.Equal(t, EngineAccepted, got,
				"the acceptance record is in the community's repo; that is what the outcome reports")
		})
	}
}

func TestEngine_SkippedWriteWithNoRevStampsNothing(t *testing.T) {
	t.Parallel()

	// THE DEFENSIVE HALF of the skip contract. The writer reports the repo's
	// head rev on every skip (see TestEngine_SkippedWriteStampsTheCatchUpWatermark),
	// but if a skip ever arrives WITHOUT one, the engine must not stamp:
	// ApplyAcceptance refuses an empty rev with ErrInvalidWatermark (correctly
	// — an empty rev is a fabricated clock value), and inventing one to get
	// past that would write a watermark no commit ever had.
	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
	h.writer.acceptanceResult = CommunityWriteResult{
		URI:     "at://" + engineCommunityDID + "/" + AcceptanceCollection + "/" + SubjectRkey(enginePostURI),
		RKey:    SubjectRkey(enginePostURI),
		CID:     engineRecordCID,
		Skipped: true,
	}

	outcome, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, EngineAccepted, outcome,
		"the acceptance stands — it was already there — so the pass succeeded")

	assert.Equal(t, []string{"Get", "DecideAdmission", "WriteAcceptance"}, h.rec.calls)
	assert.Empty(t, h.admissions.acceptanceCmds,
		"a skipped write has no commit rev, and the repository refuses an empty one as a fabricated watermark")
}

func TestEngine_SkippedWriteStampsTheCatchUpWatermark(t *testing.T) {
	t.Parallel()

	// THE RE-FIRE AFTER A LOST STAMP. The first pass wrote the acceptance and
	// then failed ApplyAcceptance — a database blip after a successful PDS
	// commit. The record stands, the row is still pending, and the next pass's
	// write is a skip. If a skip stamps nothing, that row is stranded until a
	// human notices: nothing else re-drives it before task 5 exists.
	//
	// So a skip carries the repo's HEAD rev and the engine stamps it. That is
	// safe because a standing acceptance pinning our CID proves no
	// subject-scoped community event lies between the acceptance's commit and
	// the head: a removal would have deleted the record, and a repin would pin
	// a different CID.
	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
	h.writer.acceptanceResult = CommunityWriteResult{
		URI:  "at://" + engineCommunityDID + "/" + AcceptanceCollection + "/" + SubjectRkey(enginePostURI),
		RKey: SubjectRkey(enginePostURI),
		CID:  engineRecordCID,
		// The head rev the writer read around its pre-read — the catch-up
		// watermark.
		Rev:     engineCommitRev,
		Skipped: true,
	}

	outcome, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, EngineAccepted, outcome)

	assert.Equal(t, []string{"Get", "DecideAdmission", "WriteAcceptance", "ApplyAcceptance"}, h.rec.calls,
		"a skipped write that reports a head rev must still stamp the row — that is what un-strands "+
			"a row whose first stamp failed after the PDS commit landed")
	require.Len(t, h.admissions.acceptanceCmds, 1)
	assert.Equal(t, engineCommitRev, h.admissions.acceptanceCmds[0].Watermark.Rev)
	assert.Equal(t, engineIndexedCID, h.admissions.acceptanceCmds[0].PinnedCID)
}

func TestEngine_SkippedRemovalStampsTheCatchUpWatermark(t *testing.T) {
	t.Parallel()

	// The removal twin of the acceptance catch-up: a removal commit landed, the
	// stamp failed, and the re-fire's write is a skip carrying the head rev.
	h := newEngineHarness(AdmissionStatusPendingReacceptance, cidPtr(engineIndexedCID))
	h.decider.code = DecisionSpam
	h.writer.removalResult = CommunityWriteResult{
		URI:     "at://" + engineCommunityDID + "/" + RemovalCollection + "/" + SubjectRkey(enginePostURI),
		RKey:    SubjectRkey(enginePostURI),
		CID:     engineRemovalCID,
		Rev:     engineCommitRev,
		Skipped: true,
	}

	outcome, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, EngineRemoved, outcome)

	assert.Equal(t, []string{"Get", "DecideAdmission", "WriteRemoval", "ApplyRemoval"}, h.rec.calls)
	require.Len(t, h.admissions.removalCmds, 1)
	assert.Equal(t, engineCommitRev, h.admissions.removalCmds[0].Watermark.Rev)
}

func TestEngine_ARejectionThatDidNotLandIsDeferredNotRejected(t *testing.T) {
	t.Parallel()

	// RecordRejection is a pending-only CAS carrying the judged CID, and both
	// of its skip outcomes mean THE REJECTION DID NOT LAND: skipped_stale, the
	// author edited between the read and the write, so the verdict judged
	// content the row no longer holds; skipped_terminal, another writer settled
	// the row first. Reporting EngineRejected for either would claim a refusal
	// that was never recorded — and the caller would tell the author their post
	// was rejected while the row says otherwise. The honest outcome is a
	// deferral: nothing landed, and the edit (or the settled state) is what
	// drives the subject next.
	for _, skip := range []AdmissionOutcome{AdmissionSkippedStale, AdmissionSkippedTerminal} {
		t.Run(string(skip), func(t *testing.T) {
			t.Parallel()

			h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
			h.decider.code = DecisionSpam
			h.admissions.rejectionResult = AdmissionResult{Outcome: skip, Admission: h.admissions.row}

			outcome, err := h.process(t)
			require.NoError(t, err, "a rejection refused by the CAS is the guard working, not a failure")
			assert.Equalf(t, EngineDeferred, outcome,
				"the rejection did not land, so the pass must not report EngineRejected")
		})
	}
}

func TestEngine_ReportsAFailedStamp(t *testing.T) {
	t.Parallel()

	// The opposite of a skip. If the repository genuinely errors, the AppView's
	// view of the row now disagrees with the community's repo, and that has to
	// be visible: the firehose will reconcile it, but a silent divergence is
	// how a post stays invisible for hours with nothing to search for.
	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))
	dbDown := errors.New("apply acceptance: connection refused")
	h.admissions.acceptanceErr = dbDown

	_, err := h.process(t)
	require.Error(t, err)
	assert.ErrorIs(t, err, dbDown)
}

func TestEngine_PassesTheSubjectToThePolicyUnchanged(t *testing.T) {
	t.Parallel()

	// Small, but it is the join between the queue and the decision: a policy
	// asked about the wrong subject answers confidently about someone else's
	// post.
	h := newEngineHarness(AdmissionStatusPending, cidPtr(engineIndexedCID))

	_, err := h.process(t)
	require.NoError(t, err)
	assert.Equal(t, engineCommunityDID, h.decider.lastCommunityDID)
	assert.Equal(t, enginePostURI, h.decider.lastPostURI)
}
