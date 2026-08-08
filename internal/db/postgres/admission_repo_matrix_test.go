//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The per-transition behaviour underneath the lifecycle contract.
//
// admission_repo_lifecycle_test.go walks ONE journey and proves the §5.2 rule
// makes each commit converge. This file is the breadth: every starting state
// each mutation can meet, including the ones a single journey never visits — an
// acceptance for a post nobody has indexed, a rejection reopened by an edit, a
// bridge repin that must not flap the status. Per docs/TEST_ARCHITECTURE.md
// §3.4 rule 3, behavioural breadth belongs here at T1 rather than in a pipeline
// contract.
//
// Two things are asserted throughout and are easy to lose sight of:
//
//   - "unchanged" means BYTE-identical, updated_at included. A mutation that
//     refuses an event but rewrites a timestamp has still corrupted the audit
//     trail, and an assertion narrowed to status would not see it.
//   - Author-repo events must never move the community watermark, and the
//     AppView's own rejection must not either. Both are checked by comparing
//     the watermark across the call rather than against a literal, so the
//     assertion holds whatever the watermark happened to be.

// admissionOrNil reads a subject's row, treating "never seen" as nil rather
// than as a failure — the absent-row case starts there.
func admissionOrNil(t *testing.T, repo posts.AdmissionRepository, subject admissionSubject) *posts.Admission {
	t.Helper()

	got, err := repo.Get(context.Background(), subject.CommunityDID, subject.PostURI)
	if errors.Is(err, posts.ErrNotFound) {
		return nil
	}
	require.NoError(t, err)
	return got
}

// rejectedSubject seeds a fresh subject, records content for it, and rejects it.
//
// The precondition is asserted rather than assumed: a matrix row that silently
// ran against a `pending` row because the rejection never landed would pass for
// entirely the wrong reason, and two rows below assert "nothing changed" —
// which a pending row satisfies trivially.
func rejectedSubject(t *testing.T, db *sql.DB, repo posts.AdmissionRepository, cid, decisionCode string, redrivable bool) admissionSubject {
	t.Helper()

	ctx := context.Background()
	subject := newAdmissionSubject(t, db)

	_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: cid,
	})
	require.NoError(t, err, "seeding the content to be judged")

	result, err := repo.RecordRejection(ctx, posts.RecordRejectionCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		DecisionCode: decisionCode,
		JudgedCID:    cid,
		Redrivable:   redrivable,
	})
	require.NoError(t, err, "seeding the rejection")
	require.NotNil(t, result.Admission, "seeding the rejection returned no row")
	require.Equal(t, posts.AdmissionStatusRejected, result.Admission.Status,
		"seeded subject is not rejected — RecordRejection has to work before this matrix's rejected rows mean anything")

	return subject
}

func TestAdmissionRepo_UpsertPendingMatrix(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	const decisionCode = "rule_violation"

	// arrange builds a subject in its starting state and returns it together
	// with the CID the incoming author-repo event carries. Each row owns its
	// subject, so the rows are order-independent and a failure names one
	// transition rather than a prefix of them.
	for _, testCase := range []struct {
		name    string
		arrange func(t *testing.T) (admissionSubject, string)

		wantOutcome posts.AdmissionOutcome
		wantStatus  posts.AdmissionStatus
		// wantUnchanged asserts the whole row survived byte-identical.
		wantUnchanged bool
		verify        func(t *testing.T, before, after *posts.Admission)
	}{
		{
			name: "a post nobody has seen becomes pending",
			arrange: func(t *testing.T) (admissionSubject, string) {
				return newAdmissionSubject(t, db), contentCID(t, "fresh")
			},
			wantOutcome: posts.AdmissionApplied,
			wantStatus:  posts.AdmissionStatusPending,
			verify: func(t *testing.T, before, after *posts.Admission) {
				assert.Nil(t, before, "the arrangement was supposed to leave no row")
				assert.Nil(t, after.LastCommunityEvent, "an author event must not create a community watermark")
				assert.Nil(t, after.AcceptedCID)
				assert.Nil(t, after.DecisionCode)
				assert.True(t, after.Redrivable, "a fresh admission has to be evaluable")
			},
		},
		{
			name: "an edit before any decision just refreshes what will be judged",
			arrange: func(t *testing.T) (admissionSubject, string) {
				subject := newAdmissionSubject(t, db)
				_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
					CommunityDID: subject.CommunityDID,
					PostURI:      subject.PostURI,
					EvaluatedCID: contentCID(t, "first"),
				})
				require.NoError(t, err)
				return subject, contentCID(t, "second")
			},
			wantOutcome: posts.AdmissionApplied,
			wantStatus:  posts.AdmissionStatusPending,
			verify: func(t *testing.T, before, after *posts.Admission) {
				assert.NotEqual(t, before.EvaluatedCID, after.EvaluatedCID)
			},
		},
		{
			name: "re-delivering the content an accepted row already holds writes nothing",
			arrange: func(t *testing.T) (admissionSubject, string) {
				// Not a hypothetical: the fast path and the firehose both carry
				// the author's own post, so the second copy arrives at a row
				// that already records exactly this CID.
				cid := contentCID(t, "stable")
				return acceptedSubject(t, db, repo, cid, testkit.TID()), cid
			},
			wantOutcome:   posts.AdmissionSkippedStale,
			wantStatus:    posts.AdmissionStatusAccepted,
			wantUnchanged: true,
		},
		{
			name: "an edit to an accepted post awaits re-acceptance",
			arrange: func(t *testing.T) (admissionSubject, string) {
				subject := acceptedSubject(t, db, repo, contentCID(t, "orig"), testkit.TID())
				return subject, contentCID(t, "edited")
			},
			wantOutcome: posts.AdmissionApplied,
			wantStatus:  posts.AdmissionStatusPendingReacceptance,
			verify: func(t *testing.T, before, after *posts.Admission) {
				assert.Equal(t, before.AcceptedCID, after.AcceptedCID,
					"the acceptance still pins the pre-edit content; only a new acceptance may change that")
				assert.Equal(t, before.LastCommunityEvent, after.LastCommunityEvent,
					"an author edit must not advance the community watermark")
			},
		},
		{
			name: "content catching up to an acceptance that arrived first promotes to accepted",
			arrange: func(t *testing.T) (admissionSubject, string) {
				// The §5.2 convergence case. A community's acceptance for
				// content the AppView has not indexed yet lands first — a relay
				// coverage gap, or the author's edit still in flight — so the
				// row records the acceptance while staying pending_reacceptance.
				// When the post event finally carries the pinned CID the row
				// must promote itself; nothing else will come along and do it,
				// and waiting for another acceptance would livelock.
				subject := newAdmissionSubject(t, db)
				indexedCID := contentCID(t, "indexed")
				pinnedCID := contentCID(t, "pinned")

				_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
					CommunityDID: subject.CommunityDID,
					PostURI:      subject.PostURI,
					EvaluatedCID: indexedCID,
				})
				require.NoError(t, err)

				acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
				result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
					CommunityDID:   subject.CommunityDID,
					PostURI:        subject.PostURI,
					AcceptanceURI:  acceptanceURI,
					AcceptanceRkey: acceptanceRkey,
					PinnedCID:      pinnedCID,
					Watermark:      posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
				})
				require.NoError(t, err)
				require.NotNil(t, result.Admission)
				require.Equal(t, posts.AdmissionStatusPendingReacceptance, result.Admission.Status,
					"an acceptance pinning content the row does not hold must not read as accepted")

				return subject, pinnedCID
			},
			wantOutcome: posts.AdmissionApplied,
			wantStatus:  posts.AdmissionStatusAccepted,
			verify: func(t *testing.T, before, after *posts.Admission) {
				assert.Equal(t, before.AcceptedCID, after.EvaluatedCID,
					"the promotion happens precisely because the indexed content now IS what the acceptance pins")
				assert.Equal(t, before.LastCommunityEvent, after.LastCommunityEvent,
					"the promotion is an author event; the watermark stands where the community's acceptance left it")
			},
		},
		{
			name: "an edit while removed records the content and nothing else",
			arrange: func(t *testing.T) (admissionSubject, string) {
				revs := increasingRevs(t, 2)
				subject := removedSubject(t, db, repo, contentCID(t, "removed"), revs[0], revs[1], decisionCode)
				return subject, contentCID(t, "afterremoval")
			},
			wantOutcome: posts.AdmissionSkippedTerminal,
			wantStatus:  posts.AdmissionStatusRemoved,
			verify: func(t *testing.T, before, after *posts.Admission) {
				// Everything the moderator decided has to survive verbatim: if
				// an edit could disturb any of it, editing would launder a
				// removed post back toward auto-acceptance.
				assert.Equal(t, before.DecisionCode, after.DecisionCode)
				assert.Equal(t, before.DecisionAt, after.DecisionAt)
				assert.Equal(t, before.AcceptanceURI, after.AcceptanceURI)
				assert.Equal(t, before.AcceptanceRkey, after.AcceptanceRkey)
				assert.Equal(t, before.AcceptedCID, after.AcceptedCID)
				assert.Equal(t, before.Redrivable, after.Redrivable)
				assert.Equal(t, before.LastCommunityEvent, after.LastCommunityEvent)

				assert.NotEqual(t, before.EvaluatedCID, after.EvaluatedCID,
					"the content IS recorded: a later moderator restore judges what the AppView holds now")
				assert.True(t, after.UpdatedAt.After(before.UpdatedAt),
					"updated_at must advance — the row was audited even though the decision stood")
			},
		},
		{
			name: "a retryable rejection ignores a re-delivery of the content it judged",
			arrange: func(t *testing.T) (admissionSubject, string) {
				cid := contentCID(t, "judged")
				return rejectedSubject(t, db, repo, cid, decisionCode, true), cid
			},
			wantOutcome:   posts.AdmissionSkippedStale,
			wantStatus:    posts.AdmissionStatusRejected,
			wantUnchanged: true,
		},
		{
			name: "a terminal rejection ignores a re-delivery of the content it judged",
			arrange: func(t *testing.T) (admissionSubject, string) {
				cid := contentCID(t, "judged")
				return rejectedSubject(t, db, repo, cid, decisionCode, false), cid
			},
			wantOutcome:   posts.AdmissionSkippedStale,
			wantStatus:    posts.AdmissionStatusRejected,
			wantUnchanged: true,
		},
		{
			name: "new content reopens a retryable rejection",
			arrange: func(t *testing.T) (admissionSubject, string) {
				subject := rejectedSubject(t, db, repo, contentCID(t, "judged"), decisionCode, true)
				return subject, contentCID(t, "rewritten")
			},
			wantOutcome: posts.AdmissionApplied,
			wantStatus:  posts.AdmissionStatusPending,
			verify:      assertRejectionReopened,
		},
		{
			name: "new content reopens even a terminal rejection",
			arrange: func(t *testing.T) (admissionSubject, string) {
				// redrivable = false is about the DEAD-LETTER pass, which
				// retries the SAME content. Edited content has never been judged
				// at all, so leaving the flag set would make the rejection
				// permanent for a post that no longer exists in the form that
				// earned it.
				subject := rejectedSubject(t, db, repo, contentCID(t, "judged"), decisionCode, false)
				return subject, contentCID(t, "rewritten")
			},
			wantOutcome: posts.AdmissionApplied,
			wantStatus:  posts.AdmissionStatusPending,
			verify:      assertRejectionReopened,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			subject, incomingCID := testCase.arrange(t)
			before := admissionOrNil(t, repo, subject)

			result, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
				CommunityDID: subject.CommunityDID,
				PostURI:      subject.PostURI,
				EvaluatedCID: incomingCID,
			})
			require.NoError(t, err, "an author-repo observation is never an error path")
			assert.Equal(t, testCase.wantOutcome, result.Outcome)

			require.NotNil(t, result.Admission, "a mutation must return the row it left behind")
			assert.Equal(t, testCase.wantStatus, result.Admission.Status)

			after, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
			require.NoError(t, err)
			assert.Equal(t, result.Admission, after, "the returned row must be the row on disk")

			if testCase.wantUnchanged {
				require.NotNil(t, before)
				assert.Equal(t, before, after,
					"a refused observation must leave the row byte-identical, updated_at included")
			}
			if testCase.verify != nil {
				testCase.verify(t, before, after)
			}
		})
	}
}

// assertRejectionReopened is the shared expectation for both reopening rows: the
// decision that judged the OLD content goes away entirely, redrivable included.
func assertRejectionReopened(t *testing.T, before, after *posts.Admission) {
	t.Helper()

	assert.Nil(t, after.DecisionCode, "the code judged content that no longer exists")
	assert.Nil(t, after.DecisionAt)
	assert.True(t, after.Redrivable,
		"a reopened row must be evaluable again whatever the previous decision's redrivability was — "+
			"otherwise the new content would never be judged at all")
	assert.Equal(t, before.LastCommunityEvent, after.LastCommunityEvent,
		"reopening is an author event; it must not touch the community watermark")
}

func TestAdmissionRepo_CommunityEventsOnAnUnseenPost(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// A community event about a post the AppView has never indexed is ordinary
	// rather than exceptional: relay coverage gaps and cross-feed skew routinely
	// deliver a community's decision before the author's record. The row is what
	// records that the event was seen at all — without it the same event
	// re-applies on every redrive.

	t.Run("an acceptance for an unindexed post inserts an accepted row", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		pinnedCID := contentCID(t, "pinnedfirst")
		rev := testkit.TID()
		acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)

		result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
			CommunityDID:   subject.CommunityDID,
			PostURI:        subject.PostURI,
			AcceptanceURI:  acceptanceURI,
			AcceptanceRkey: acceptanceRkey,
			PinnedCID:      pinnedCID,
			Watermark:      posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionApplied, result.Outcome)

		require.NotNil(t, result.Admission)
		assert.Equal(t, posts.AdmissionStatusAccepted, result.Admission.Status)
		assertNullableString(t, pinnedCID, result.Admission.AcceptedCID, "accepted_cid")
		assertNullableString(t, pinnedCID, result.Admission.EvaluatedCID,
			"evaluated_cid: the pinned CID is the only content identifier available, so it is what the row records as judged")
		assertWatermark(t, rev, posts.CommunityOpPut, result.Admission.LastCommunityEvent)
	})

	t.Run("a pre-emptive removal for an unindexed post inserts a removed row", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		rev := testkit.TID()

		result, err := repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			DecisionCode: "banned_author",
			Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionApplied, result.Outcome)

		require.NotNil(t, result.Admission)
		assert.Equal(t, posts.AdmissionStatusRemoved, result.Admission.Status,
			"a removal with no prior acceptance is valid — communities may remove pre-emptively")
		assertNullableString(t, "banned_author", result.Admission.DecisionCode, "decision_code")
		assert.NotNil(t, result.Admission.DecisionAt)
		assertWatermark(t, rev, posts.CommunityOpPut, result.Admission.LastCommunityEvent)
	})
}

func TestAdmissionRepo_AcceptanceWithMismatchedCIDKeepsItsFields(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// The acceptance fields are persisted on a MISMATCH too, and that is the
	// whole convergence mechanism: dropping them would leave nothing for the
	// post event to promote against, and the subject would sit in
	// pending_reacceptance until another acceptance happened to be written.
	subject := newAdmissionSubject(t, db)
	indexedCID := contentCID(t, "indexed")
	pinnedCID := contentCID(t, "pinned")

	_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: indexedCID,
	})
	require.NoError(t, err)

	rev := testkit.TID()
	acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
	result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   subject.CommunityDID,
		PostURI:        subject.PostURI,
		AcceptanceURI:  acceptanceURI,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      pinnedCID,
		Watermark:      posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionApplied, result.Outcome,
		"the event APPLIED — the mismatch decides the status, not whether the write happened")

	require.NotNil(t, result.Admission)
	assert.Equal(t, posts.AdmissionStatusPendingReacceptance, result.Admission.Status,
		"content the acceptance does not pin must never render as accepted")
	assertNullableString(t, acceptanceURI, result.Admission.AcceptanceURI, "acceptance_uri")
	assertNullableString(t, acceptanceRkey, result.Admission.AcceptanceRkey, "acceptance_rkey")
	assertNullableString(t, pinnedCID, result.Admission.AcceptedCID, "accepted_cid")
	assertNullableString(t, indexedCID, result.Admission.EvaluatedCID,
		"evaluated_cid: the community's pin does not overwrite what the AppView actually holds")
	assertWatermark(t, rev, posts.CommunityOpPut, result.Admission.LastCommunityEvent)
}

func TestAdmissionRepo_AcceptanceDeleteOnANeverAcceptedRow(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// Withdrawing an acceptance this row never had decides nothing, so the
	// status stands — but the watermark MUST advance anyway. It is the record
	// that the event was seen, and without it the acceptance-create this
	// deletion supersedes would apply the next time any feed replays it.
	subject := newAdmissionSubject(t, db)
	cid := contentCID(t, "pending")

	_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: cid,
	})
	require.NoError(t, err)

	rev := testkit.TID()
	result, err := repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpDelete},
	})
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionApplied, result.Outcome)

	require.NotNil(t, result.Admission)
	assert.Equal(t, posts.AdmissionStatusPending, result.Admission.Status,
		"deleting an acceptance withdraws it; it does not decide anything, so a pending row stays pending")
	assertNullableString(t, cid, result.Admission.EvaluatedCID, "evaluated_cid")
	assertWatermark(t, rev, posts.CommunityOpDelete, result.Admission.LastCommunityEvent)
}

func TestAdmissionRepo_RepinAcceptedCID(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// The §5.5 bridgedStats exception. A bridge refreshes its record to carry
	// the origin platform's new vote counts; the CID changes, nothing a
	// moderator would judge does. Routing that through the ordinary edit path
	// would drop the post out of every feed until a re-acceptance commit caught
	// up — repeatedly, for as long as the origin post keeps getting votes.
	revs := increasingRevs(t, 2)
	originalCID := contentCID(t, "bridged")
	subject := acceptedSubject(t, db, repo, originalCID, revs[0])

	before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
	require.NoError(t, err)

	refreshedCID := contentCID(t, "restats")
	result, err := repo.RepinAcceptedCID(ctx, posts.RepinAcceptanceCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		PinnedCID:    refreshedCID,
		Watermark:    posts.CommunityWatermark{Rev: revs[1], OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionApplied, result.Outcome)

	require.NotNil(t, result.Admission)
	assert.Equal(t, posts.AdmissionStatusAccepted, result.Admission.Status,
		"a repin must not flap the status: the post never leaves the feed")
	assertNullableString(t, refreshedCID, result.Admission.AcceptedCID, "accepted_cid")
	assertNullableString(t, refreshedCID, result.Admission.EvaluatedCID,
		"evaluated_cid must move with it, or the very next author event would read as an edit and demand re-acceptance")
	assertWatermark(t, revs[1], posts.CommunityOpPut, result.Admission.LastCommunityEvent)

	assert.Equal(t, before.AcceptanceURI, result.Admission.AcceptanceURI,
		"the acceptance record is updated in place, same rkey — a repin is not a new acceptance")
	assert.Equal(t, before.AcceptanceRkey, result.Admission.AcceptanceRkey)

	t.Run("a stale repin is refused like any other community event", func(t *testing.T) {
		stale, err := repo.RepinAcceptedCID(ctx, posts.RepinAcceptanceCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			PinnedCID:    contentCID(t, "stale"),
			Watermark:    posts.CommunityWatermark{Rev: revs[0], OpRank: posts.CommunityOpPut},
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionSkippedStale, stale.Outcome,
			"a repin carries a watermark, so a redriven copy of an older refresh must not regress the pin")
		require.NotNil(t, stale.Admission)
		assertNullableString(t, refreshedCID, stale.Admission.AcceptedCID, "accepted_cid after the stale repin")
	})
}

func TestAdmissionRepo_RecordRejection(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// A rejection is the AppView's OWN decision — there is no community-repo
	// record behind it. That is why it must not advance the community
	// watermark: a local decision that outranked a genuine community event
	// would suppress the very acceptance that overrules it, and the community
	// would have no way to publish its way past us.
	for _, redrivable := range []bool{true, false} {
		name := "a terminal policy rejection"
		if redrivable {
			name = "a retryable evaluation failure"
		}

		t.Run(name, func(t *testing.T) {
			revs := increasingRevs(t, 2)

			// A subject that HAS a watermark, so "does not touch it" is
			// observable rather than vacuous: accepted, then the acceptance is
			// withdrawn, and the engine re-runs admitPost on what is left.
			judgedCID := contentCID(t, "judged")
			subject := acceptedSubject(t, db, repo, judgedCID, revs[0])
			_, err := repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
				CommunityDID: subject.CommunityDID,
				PostURI:      subject.PostURI,
				Watermark:    posts.CommunityWatermark{Rev: revs[1], OpRank: posts.CommunityOpDelete},
			})
			require.NoError(t, err)

			before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
			require.NoError(t, err)
			require.NotNil(t, before.LastCommunityEvent, "the arrangement must leave a watermark to compare against")

			result, err := repo.RecordRejection(ctx, posts.RecordRejectionCommand{
				CommunityDID: subject.CommunityDID,
				PostURI:      subject.PostURI,
				DecisionCode: "banned_author",
				JudgedCID:    judgedCID,
				Redrivable:   redrivable,
			})
			require.NoError(t, err)
			assert.Equal(t, posts.AdmissionApplied, result.Outcome)

			require.NotNil(t, result.Admission)
			assert.Equal(t, posts.AdmissionStatusRejected, result.Admission.Status)
			assertNullableString(t, "banned_author", result.Admission.DecisionCode, "decision_code")
			assert.NotNil(t, result.Admission.DecisionAt, "a decision has to say when it was made")
			assert.Equal(t, redrivable, result.Admission.Redrivable,
				"redrivable is the caller's classification of why: a policy rejection must not be retried by the redrive pass, a transient failure must be")

			assert.Equal(t, before.LastCommunityEvent, result.Admission.LastCommunityEvent,
				"an AppView-local decision must never advance the community watermark")
			assert.Equal(t, before.EvaluatedCID, result.Admission.EvaluatedCID,
				"the rejection judges the content already recorded; it does not change what was judged")
		})
	}
}

// ---------------------------------------------------------------------------
// RecordRejection as a full CAS (review finding 1)
// ---------------------------------------------------------------------------

func TestAdmissionRepo_RecordRejectionCAS(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	const decisionCode = "rule_violation"

	t.Run("a rejection cannot land on an accepted row", func(t *testing.T) {
		// Rejection's ONLY legal source state is pending (§5.5: re-acceptance
		// failure is a removal, not a rejection). A rejection overwriting an
		// accepted row would suppress a live community acceptance with a local
		// decision — the exact inversion of authority RecordRejection's
		// no-watermark rule exists to prevent.
		cid := contentCID(t, "live")
		subject := acceptedSubject(t, db, repo, cid, testkit.TID())

		before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)

		result, err := repo.RecordRejection(ctx, posts.RecordRejectionCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			DecisionCode: decisionCode,
			JudgedCID:    cid,
			Redrivable:   false,
		})
		require.NoError(t, err, "a refused rejection is an outcome, not an error")
		assert.Equal(t, posts.AdmissionSkippedTerminal, result.Outcome,
			"an accepted row refuses a rejection by its state, not by ordering")

		after, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		assert.Equal(t, before, after,
			"a refused rejection must leave the accepted row byte-identical, updated_at included")
	})

	t.Run("a rejection cannot land on a row awaiting re-acceptance", func(t *testing.T) {
		// pending_reacceptance still carries a live acceptance record; §5.5
		// says re-acceptance failure is expressed as a REMOVAL. A rejection
		// here would strand the acceptance columns under a local decision.
		subject := acceptedSubject(t, db, repo, contentCID(t, "orig"), testkit.TID())
		editedCID := contentCID(t, "edited")
		_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			EvaluatedCID: editedCID,
		})
		require.NoError(t, err)

		before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		require.Equal(t, posts.AdmissionStatusPendingReacceptance, before.Status)

		result, err := repo.RecordRejection(ctx, posts.RecordRejectionCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			DecisionCode: decisionCode,
			JudgedCID:    editedCID,
			Redrivable:   false,
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionSkippedTerminal, result.Outcome)

		after, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		assert.Equal(t, before, after, "the refused rejection must not disturb the standing acceptance")
	})

	t.Run("a rejection judging a CID the row no longer holds is refused", func(t *testing.T) {
		// The engine read the row, judged its content, and wrote the verdict —
		// but the author edited in between. The verdict judged content that no
		// longer exists and must not land on the new content.
		subject := newAdmissionSubject(t, db)
		judgedCID := contentCID(t, "judged")
		editedCID := contentCID(t, "edited")

		_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			EvaluatedCID: editedCID,
		})
		require.NoError(t, err)

		before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)

		result, err := repo.RecordRejection(ctx, posts.RecordRejectionCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			DecisionCode: decisionCode,
			JudgedCID:    judgedCID,
			Redrivable:   false,
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionSkippedStale, result.Outcome,
			"a verdict for content the row does not hold is stale, and the new content awaits its own judgment")

		after, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionStatusPending, after.Status,
			"the row stays pending: the edited content has never been judged")
		assert.Equal(t, before, after, "the refused rejection must leave the row byte-identical")
	})

	t.Run("a re-delivered rejection is a byte-identical no-op", func(t *testing.T) {
		cid := contentCID(t, "judged")
		subject := rejectedSubject(t, db, repo, cid, decisionCode, false)

		before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)

		result, err := repo.RecordRejection(ctx, posts.RecordRejectionCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			DecisionCode: decisionCode,
			JudgedCID:    cid,
			Redrivable:   false,
		})
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionSkippedStale, result.Outcome,
			"an exact duplicate of the decision already recorded is a replay")

		after, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		assert.Equal(t, before, after,
			"the duplicate must not re-stamp decision_at or updated_at — deep-equal, not merely same status")
	})

	t.Run("a rejection and an author edit converge in either order", func(t *testing.T) {
		// Whichever of the two lands first, the end state must be the same:
		// pending on the edited content, never `rejected` carrying the edited
		// CID — the rejection judged the OLD content only.
		judgedCID := contentCID(t, "judged")
		editedCID := contentCID(t, "edited")

		assertConverged := func(t *testing.T, subject admissionSubject) {
			t.Helper()
			final, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
			require.NoError(t, err)
			assert.Equal(t, posts.AdmissionStatusPending, final.Status)
			assertNullableString(t, editedCID, final.EvaluatedCID, "evaluated_cid")
			assert.Nil(t, final.DecisionCode, "no decision may survive against content it never judged")
			assert.Nil(t, final.DecisionAt)
			assert.True(t, final.Redrivable, "the edited content must be evaluable")
		}

		t.Run("edit first, then the stale rejection", func(t *testing.T) {
			subject := newAdmissionSubject(t, db)
			_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: judgedCID,
			})
			require.NoError(t, err)
			_, err = repo.UpsertPending(ctx, posts.UpsertPendingCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: editedCID,
			})
			require.NoError(t, err)

			result, err := repo.RecordRejection(ctx, posts.RecordRejectionCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
				DecisionCode: decisionCode, JudgedCID: judgedCID, Redrivable: false,
			})
			require.NoError(t, err)
			assert.Equal(t, posts.AdmissionSkippedStale, result.Outcome)
			assertConverged(t, subject)
		})

		t.Run("rejection first, then the edit reopens it", func(t *testing.T) {
			subject := rejectedSubject(t, db, repo, judgedCID, decisionCode, false)

			result, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: editedCID,
			})
			require.NoError(t, err)
			assert.Equal(t, posts.AdmissionApplied, result.Outcome)
			assertConverged(t, subject)
		})
	})
}

func TestAdmissionRepo_RecordRejectionOnAnUnseenSubjectIsAnError(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)

	// The engine rejects rows it read from its own queue. A subject with no row
	// cannot have been read from anything, so its absence is a caller bug — a
	// genuine error for the dead-letter queue, not delivery skew to skip over.
	subject := newAdmissionSubject(t, db)

	_, err := repo.RecordRejection(context.Background(), posts.RecordRejectionCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		DecisionCode: "rule_violation",
		JudgedCID:    contentCID(t, "unseen"),
		Redrivable:   false,
	})
	require.Error(t, err, "rejecting a subject the AppView has never recorded must be an error")
	assert.ErrorIs(t, err, posts.ErrNotFound)

	_, getErr := repo.Get(context.Background(), subject.CommunityDID, subject.PostURI)
	assert.ErrorIs(t, getErr, posts.ErrNotFound, "the failed rejection must not have manufactured a row")
}

// ---------------------------------------------------------------------------
// OpRank is derived from the operation, never taken from the caller (finding 4)
// ---------------------------------------------------------------------------

func TestAdmissionRepo_OpRankIsDerivedFromTheOperation(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// Every command below carries the WRONG rank on purpose. The rank IS the
	// operation's kind — rank(delete)=0 < rank(put)=1 — so it is derived inside
	// each method; a caller that could assert otherwise could reorder a commit
	// with one mislabeled event. The row's stored watermark is the proof.

	t.Run("ApplyAcceptance stamps a put rank", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		rev := testkit.TID()
		acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
		result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
			CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
			AcceptanceURI: acceptanceURI, AcceptanceRkey: acceptanceRkey,
			PinnedCID: contentCID(t, "rank"),
			Watermark: posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpDelete},
		})
		require.NoError(t, err)
		require.Equal(t, posts.AdmissionApplied, result.Outcome)
		assertWatermark(t, rev, posts.CommunityOpPut, result.Admission.LastCommunityEvent)
	})

	t.Run("ApplyAcceptanceDelete stamps a delete rank", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		rev := testkit.TID()
		result, err := repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
			CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
			Watermark: posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
		})
		require.NoError(t, err)
		require.Equal(t, posts.AdmissionApplied, result.Outcome)
		assertWatermark(t, rev, posts.CommunityOpDelete, result.Admission.LastCommunityEvent)
	})

	t.Run("ApplyRemoval stamps a put rank", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		rev := testkit.TID()
		result, err := repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
			CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
			DecisionCode: "rule_violation",
			Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpDelete},
		})
		require.NoError(t, err)
		require.Equal(t, posts.AdmissionApplied, result.Outcome)
		assertWatermark(t, rev, posts.CommunityOpPut, result.Admission.LastCommunityEvent)
	})

	t.Run("ApplyRemovalDelete stamps a delete rank", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		rev := testkit.TID()
		result, err := repo.ApplyRemovalDelete(ctx, posts.CommunityDeleteCommand{
			CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
			Watermark: posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
		})
		require.NoError(t, err)
		require.Equal(t, posts.AdmissionApplied, result.Outcome)
		assertWatermark(t, rev, posts.CommunityOpDelete, result.Admission.LastCommunityEvent)
	})

	t.Run("RepinAcceptedCID stamps a put rank", func(t *testing.T) {
		revs := increasingRevs(t, 2)
		subject := acceptedSubject(t, db, repo, contentCID(t, "bridged"), revs[0])
		result, err := repo.RepinAcceptedCID(ctx, posts.RepinAcceptanceCommand{
			CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
			PinnedCID: contentCID(t, "restats"),
			Watermark: posts.CommunityWatermark{Rev: revs[1], OpRank: posts.CommunityOpDelete},
		})
		require.NoError(t, err)
		require.Equal(t, posts.AdmissionApplied, result.Outcome)
		assertWatermark(t, revs[1], posts.CommunityOpPut, result.Admission.LastCommunityEvent)
	})
}

// ---------------------------------------------------------------------------
// Restore-vs-absent convergence for ApplyAcceptance (finding 2)
// ---------------------------------------------------------------------------

func TestAdmissionRepo_AcceptanceOntoANullEvaluatedTombstoneLandsAccepted(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// A restore commit's removal-delete half arriving at an ABSENT subject
	// inserts a tombstone with NO evaluated content. The acceptance half must
	// still converge on accepted: the pinned CID is the only content identifier
	// anyone has, exactly as on the acceptance-first insert path — NULL
	// evaluated_cid means "nothing recorded yet", not "content that mismatches".
	subject := newAdmissionSubject(t, db)
	revs := increasingRevs(t, 2)

	tombstone, err := repo.ApplyRemovalDelete(ctx, posts.CommunityDeleteCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		Watermark:    posts.CommunityWatermark{Rev: revs[0], OpRank: posts.CommunityOpDelete},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, tombstone.Outcome)
	require.Nil(t, tombstone.Admission.EvaluatedCID, "the arrangement needs a NULL-evaluated tombstone")

	pinnedCID := contentCID(t, "restored")
	acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
	result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   subject.CommunityDID,
		PostURI:        subject.PostURI,
		AcceptanceURI:  acceptanceURI,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      pinnedCID,
		Watermark:      posts.CommunityWatermark{Rev: revs[1], OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionApplied, result.Outcome)

	require.NotNil(t, result.Admission)
	assert.Equal(t, posts.AdmissionStatusAccepted, result.Admission.Status,
		"an acceptance meeting a row with NO recorded content must land accepted, not pending_reacceptance")
	assertNullableString(t, pinnedCID, result.Admission.AcceptedCID, "accepted_cid")
	assertNullableString(t, pinnedCID, result.Admission.EvaluatedCID,
		"evaluated_cid: the pinned CID is the only content identifier available, same as the insert path records")
}

func TestAdmissionRepo_RestoreCommitConvergesOnAnAbsentRow(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// The restore commit {removal-delete@(R,0), acceptance@(R,1)} delivered to
	// a subject this AppView holds NO row for — a relay coverage gap swallowed
	// the whole earlier history. Both delivery orders must leave the same row.
	rev := testkit.TID()

	deliver := func(t *testing.T, subject admissionSubject, pinnedCID string, acceptanceFirst bool) *posts.Admission {
		t.Helper()
		acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
		acceptance := func() {
			result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
				AcceptanceURI: acceptanceURI, AcceptanceRkey: acceptanceRkey,
				PinnedCID: pinnedCID,
				Watermark: posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
			})
			require.NoError(t, err)
			require.NotNil(t, result.Admission)
		}
		removalDelete := func() {
			_, err := repo.ApplyRemovalDelete(ctx, posts.CommunityDeleteCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
				Watermark: posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpDelete},
			})
			require.NoError(t, err)
		}
		if acceptanceFirst {
			acceptance()
			removalDelete()
		} else {
			removalDelete()
			acceptance()
		}
		final, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
		require.NoError(t, err)
		return final
	}

	assertRestored := func(t *testing.T, got *posts.Admission, pinnedCID string) {
		t.Helper()
		assert.Equal(t, posts.AdmissionStatusAccepted, got.Status)
		assertNullableString(t, pinnedCID, got.AcceptedCID, "accepted_cid")
		assertNullableString(t, pinnedCID, got.EvaluatedCID, "evaluated_cid")
		assertWatermark(t, rev, posts.CommunityOpPut, got.LastCommunityEvent)
		assert.Nil(t, got.DecisionCode)
		assert.Nil(t, got.DecisionAt)
		assert.True(t, got.Redrivable)
	}

	t.Run("removal-delete first", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		pinnedCID := contentCID(t, "convA")
		assertRestored(t, deliver(t, subject, pinnedCID, false), pinnedCID)
	})

	t.Run("acceptance first", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		pinnedCID := contentCID(t, "convB")
		assertRestored(t, deliver(t, subject, pinnedCID, true), pinnedCID)
	})
}

// ---------------------------------------------------------------------------
// RepinAcceptedCID applies to accepted rows ONLY (finding 3)
// ---------------------------------------------------------------------------

func TestAdmissionRepo_RepinRefusalMatrix(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	const decisionCode = "rule_violation"

	repin := func(t *testing.T, subject admissionSubject, rev string) (posts.AdmissionResult, error) {
		t.Helper()
		return repo.RepinAcceptedCID(ctx, posts.RepinAcceptanceCommand{
			CommunityDID: subject.CommunityDID,
			PostURI:      subject.PostURI,
			PinnedCID:    contentCID(t, "repin"),
			Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
		})
	}

	t.Run("an accepted row applies", func(t *testing.T) {
		revs := increasingRevs(t, 2)
		subject := acceptedSubject(t, db, repo, contentCID(t, "bridged"), revs[0])
		result, err := repin(t, subject, revs[1])
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionApplied, result.Outcome)
		require.NotNil(t, result.Admission)
		assert.Equal(t, posts.AdmissionStatusAccepted, result.Admission.Status)
	})

	// Every other status refuses by STATE — a repin re-decides nothing, so it
	// has no business at a row that would need a decision — and the refusal
	// must leave the row byte-identical.
	for _, refusal := range []struct {
		name    string
		arrange func(t *testing.T) admissionSubject
	}{
		{
			name: "a pending row refuses",
			arrange: func(t *testing.T) admissionSubject {
				subject := newAdmissionSubject(t, db)
				_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
					EvaluatedCID: contentCID(t, "pending"),
				})
				require.NoError(t, err)
				return subject
			},
		},
		{
			name: "a row awaiting re-acceptance refuses",
			arrange: func(t *testing.T) admissionSubject {
				// The dangerous one: the row still CARRIES an acceptance URI, so
				// a guard on the acceptance columns alone would let the repin
				// through — writing a fresh accepted_cid under a status that
				// says the acceptance does not cover the current content.
				subject := acceptedSubject(t, db, repo, contentCID(t, "orig"), testkit.TID())
				_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
					EvaluatedCID: contentCID(t, "edited"),
				})
				require.NoError(t, err)
				return subject
			},
		},
		{
			name: "a rejected row refuses",
			arrange: func(t *testing.T) admissionSubject {
				return rejectedSubject(t, db, repo, contentCID(t, "judged"), decisionCode, false)
			},
		},
		{
			name: "a removed row refuses",
			arrange: func(t *testing.T) admissionSubject {
				revs := increasingRevs(t, 2)
				return removedSubject(t, db, repo, contentCID(t, "removed"), revs[0], revs[1], decisionCode)
			},
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			subject := refusal.arrange(t)
			before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
			require.NoError(t, err)

			result, err := repin(t, subject, testkit.TID())
			require.NoError(t, err, "a refused repin is an outcome, not an error")
			assert.Equal(t, posts.AdmissionSkippedTerminal, result.Outcome,
				"the row's state refuses the repin regardless of ordering")
			require.NotNil(t, result.Admission)

			after, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
			require.NoError(t, err)
			assert.Equal(t, before, after, "a refused repin must leave the row byte-identical")
			assert.Equal(t, after, result.Admission, "the refused caller must be shown the row that refused it")
		})
	}

	t.Run("an absent subject refuses with no row to describe", func(t *testing.T) {
		subject := newAdmissionSubject(t, db)
		result, err := repin(t, subject, testkit.TID())
		require.NoError(t, err)
		assert.Equal(t, posts.AdmissionSkippedTerminal, result.Outcome)
		assert.Nil(t, result.Admission,
			"the documented nil-Admission shape: a repin may never CREATE a row, and there is genuinely nothing to report")
	})
}

// ---------------------------------------------------------------------------
// ApplyRemovalDelete standalone, and acceptance-delete against removed (finding 5)
// ---------------------------------------------------------------------------

func TestAdmissionRepo_StandaloneRemovalDeleteReopensEvaluation(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// A removal withdrawn with NO accompanying acceptance: the subject returns
	// to pending with the decision gone — including redrivable, which a
	// pre-removal terminal rejection may have left false. A pending row that
	// the redrive pass refuses to evaluate is a post nobody will ever judge.
	cid := contentCID(t, "judged")
	subject := rejectedSubject(t, db, repo, cid, "spam", false)

	revs := increasingRevs(t, 2)
	removed, err := repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		DecisionCode: "rule_violation",
		Watermark:    posts.CommunityWatermark{Rev: revs[0], OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, removed.Outcome)
	require.False(t, removed.Admission.Redrivable,
		"the arrangement needs the terminal rejection's redrivable=false to survive into the removal")

	result, err := repo.ApplyRemovalDelete(ctx, posts.CommunityDeleteCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		Watermark:    posts.CommunityWatermark{Rev: revs[1], OpRank: posts.CommunityOpDelete},
	})
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionApplied, result.Outcome)

	require.NotNil(t, result.Admission)
	assert.Equal(t, posts.AdmissionStatusPending, result.Admission.Status,
		"an un-removed post with no accompanying acceptance returns to pending")
	assert.Nil(t, result.Admission.DecisionCode, "a row no longer removed must not keep the code a reader would render")
	assert.Nil(t, result.Admission.DecisionAt)
	assert.True(t, result.Admission.Redrivable,
		"reopened evaluation must be redrivable: the standing decision is gone, so nothing justifies refusing to evaluate")
	assertNullableString(t, cid, result.Admission.EvaluatedCID, "evaluated_cid: the recorded content survives the un-remove")
	assertWatermark(t, revs[1], posts.CommunityOpDelete, result.Admission.LastCommunityEvent)
}

func TestAdmissionRepo_AcceptanceDeleteAtARemovedRowAdvancesTheWatermarkOnly(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// A later community commit deletes an acceptance while the row is removed —
	// e.g. cleanup of a stale acceptance record after the removal. Deleting an
	// acceptance cannot un-remove a post, but the event WAS seen, and the
	// watermark must say so or a replay of the superseded acceptance would apply.
	revs := increasingRevs(t, 3)
	subject := removedSubject(t, db, repo, contentCID(t, "removed"), revs[0], revs[1], "rule_violation")

	before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
	require.NoError(t, err)

	result, err := repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		Watermark:    posts.CommunityWatermark{Rev: revs[2], OpRank: posts.CommunityOpDelete},
	})
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionApplied, result.Outcome)

	require.NotNil(t, result.Admission)
	assert.Equal(t, posts.AdmissionStatusRemoved, result.Admission.Status,
		"deleting an acceptance withdraws it; it cannot un-remove a post")
	assert.Equal(t, before.DecisionCode, result.Admission.DecisionCode, "the moderator's code must survive")
	assert.Equal(t, before.DecisionAt, result.Admission.DecisionAt)
	assertWatermark(t, revs[2], posts.CommunityOpDelete, result.Admission.LastCommunityEvent)
}

// ---------------------------------------------------------------------------
// Hardening (finding 7)
// ---------------------------------------------------------------------------

func TestAdmissionRepo_EmptyWatermarkRevIsAnError(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// Every Jetstream commit carries a rev, so an empty one is an upstream
	// decoding bug — a genuine error for the dead-letter queue, never a skip.
	// Stamping it would write a clock value that never existed onto the row.
	subject := newAdmissionSubject(t, db)
	empty := posts.CommunityWatermark{Rev: "", OpRank: posts.CommunityOpPut}

	acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
	for name, call := range map[string]func() (posts.AdmissionResult, error){
		"ApplyAcceptance": func() (posts.AdmissionResult, error) {
			return repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
				AcceptanceURI: acceptanceURI, AcceptanceRkey: acceptanceRkey,
				PinnedCID: contentCID(t, "empty"), Watermark: empty,
			})
		},
		"ApplyAcceptanceDelete": func() (posts.AdmissionResult, error) {
			return repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, Watermark: empty,
			})
		},
		"ApplyRemoval": func() (posts.AdmissionResult, error) {
			return repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
				DecisionCode: "rule_violation", Watermark: empty,
			})
		},
		"ApplyRemovalDelete": func() (posts.AdmissionResult, error) {
			return repo.ApplyRemovalDelete(ctx, posts.CommunityDeleteCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, Watermark: empty,
			})
		},
		"RepinAcceptedCID": func() (posts.AdmissionResult, error) {
			return repo.RepinAcceptedCID(ctx, posts.RepinAcceptanceCommand{
				CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
				PinnedCID: contentCID(t, "empty"), Watermark: empty,
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := call()
			require.Error(t, err, "an empty rev must be refused before it reaches the row")
			assert.ErrorIs(t, err, posts.ErrInvalidWatermark)
		})
	}

	_, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
	assert.ErrorIs(t, err, posts.ErrNotFound, "no refused call may have written a row")
}

func TestAdmissionRepo_UpsertPendingSameCIDOnARemovedRowIsAStaleNoOp(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// Re-delivery of the exact content a removed row already records: nothing
	// is written — not even audit columns — so the honest label is the
	// duplicate-delivery one, skipped_stale, exactly as for an accepted row
	// meeting its own content again. skipped_terminal is reserved for the case
	// where the observation DID record new content and only the removal kept
	// the decision standing.
	cid := contentCID(t, "removedsame")
	revs := increasingRevs(t, 2)
	subject := removedSubject(t, db, repo, cid, revs[0], revs[1], "rule_violation")

	before, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
	require.NoError(t, err)

	result, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: cid,
	})
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionSkippedStale, result.Outcome,
		"the row already holds exactly this content; the delivery is a duplicate, not a refused transition")

	after, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a duplicate must leave the removed row byte-identical, updated_at included")
}

func TestAdmissionRepo_AcceptanceUpdateFreezesCreatedAt(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// created_at is when the SUBJECT entered the system — the moderation
	// queue's keyset orders by it. An applied acceptance updating it would
	// silently reorder the queue under every moderator paging through it.
	subject := newAdmissionSubject(t, db)
	cid := contentCID(t, "frozen")

	seeded, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: cid,
	})
	require.NoError(t, err)

	acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
	result, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   subject.CommunityDID,
		PostURI:        subject.PostURI,
		AcceptanceURI:  acceptanceURI,
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      cid,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome, "the freeze only means anything on an APPLIED update")

	assert.Equal(t, seeded.Admission.CreatedAt, result.Admission.CreatedAt,
		"an applied acceptance must not move created_at: it is the row's queue position, not an audit column")
}
