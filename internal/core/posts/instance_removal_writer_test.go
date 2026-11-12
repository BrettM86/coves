package posts

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/pds"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The instance-removal writer at T0: an instance admin's removal mirrored into
// a hosted community's repo, and its reversal. Everything here is proved
// against fakeCommunityRepo, which refuses a write guarded by a head the repo
// has moved past, as the PDS does.

const (
	instanceRemovalCreatedAt   = "2026-11-11T12:00:00Z"
	instanceRemovalCommentURI  = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.community.comment/3kjzl5kcb2s2w"
	instanceRemovalCommentCID  = "bafyreicommentaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	instanceRemovalStandingCID = "bafyreiacceptanceaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	instanceRemovalOwnedCID    = "bafyreiownedremovalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	instanceRemovalForeignCID  = "bafyreiforeignremovalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	instanceRemovalRestoredCID = "bafyreirestoredversionaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func newInstanceRemovalWriter(repo *fakeCommunityRepo) InstanceRemovalWriter {
	return NewInstanceRemovalWriter(fixedFactory(repo), writerClock,
		WithSwapRetrySleeper(func(context.Context, time.Duration) error { return nil }))
}

func seedAcceptance(repo *fakeCommunityRepo, subjectURI, subjectCID string) {
	repo.setRecord(AcceptanceCollection, SubjectRkey(subjectURI), instanceRemovalStandingCID, map[string]any{
		"$type":     AcceptanceCollection,
		"subject":   map[string]any{"uri": subjectURI, "cid": subjectCID},
		"createdAt": "2026-06-30T09:00:00Z",
	})
}

func seedRemoval(repo *fakeCommunityRepo, subjectURI, recordCID string, value map[string]any) {
	repo.setRecord(RemovalCollection, SubjectRkey(subjectURI), recordCID, value)
}

// foreignRemovalBody is a removal a community moderator wrote: a different
// code, a reason, and its own createdAt.
func foreignRemovalBody(subjectURI string) map[string]any {
	return map[string]any{
		"$type":     RemovalCollection,
		"subject":   map[string]any{"uri": subjectURI, "cid": "bafyreiolderversionaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"code":      "rule-violation",
		"reason":    "off-topic thread",
		"createdAt": "2026-10-01T08:00:00Z",
	}
}

func instanceRemovalBody(subjectURI, subjectCID, code string) map[string]any {
	return map[string]any{
		"$type":     RemovalCollection,
		"subject":   map[string]any{"uri": subjectURI, "cid": subjectCID},
		"code":      code,
		"createdAt": instanceRemovalCreatedAt,
	}
}

type writeShape struct {
	op         pds.WriteOp
	collection string
}

func shapeOf(batch fakeBatch) []writeShape {
	shapes := make([]writeShape, len(batch.writes))
	for i, write := range batch.writes {
		shapes[i] = writeShape{op: write.Op, collection: write.Collection}
	}
	return shapes
}

func recordBody(t *testing.T, repo *fakeCommunityRepo, collection, subjectURI string) map[string]any {
	t.Helper()
	record, ok := repo.records[repo.key(collection, SubjectRkey(subjectURI))]
	require.Truef(t, ok, "no %s record stands for %s", collection, subjectURI)
	return record.Value
}

func assertNoRecord(t *testing.T, repo *fakeCommunityRepo, collection, subjectURI string) {
	t.Helper()
	_, ok := repo.records[repo.key(collection, SubjectRkey(subjectURI))]
	assert.Falsef(t, ok, "a %s record still stands for %s", collection, subjectURI)
}

// ---------------------------------------------------------------------------
// B1: WriteInstanceRemoval
// ---------------------------------------------------------------------------

func TestInstanceRemovalWriter_WritesTheRemovalAndWithdrawsAStandingAcceptanceInOneCommit(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name             string
		subjectURI       string
		subjectCID       string
		acceptanceStands bool
		wantShape        []writeShape
		wantWithdrew     bool
	}{
		{
			name: "post with a standing acceptance", subjectURI: writerPostURI, subjectCID: writerPostCID,
			acceptanceStands: true,
			wantShape: []writeShape{
				{op: pds.WriteOpDelete, collection: AcceptanceCollection},
				{op: pds.WriteOpCreate, collection: RemovalCollection},
			},
			wantWithdrew: true,
		},
		{
			name: "post never accepted", subjectURI: writerPostURI, subjectCID: writerPostCID,
			wantShape:    []writeShape{{op: pds.WriteOpCreate, collection: RemovalCollection}},
			wantWithdrew: false,
		},
		{
			name: "comment", subjectURI: instanceRemovalCommentURI, subjectCID: instanceRemovalCommentCID,
			wantShape:    []writeShape{{op: pds.WriteOpCreate, collection: RemovalCollection}},
			wantWithdrew: false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeCommunityRepo(writerCommunityDID)
			if row.acceptanceStands {
				seedAcceptance(repo, row.subjectURI, row.subjectCID)
			}

			result, err := newInstanceRemovalWriter(repo).WriteInstanceRemoval(context.Background(), InstanceRemovalCommand{
				CommunityDID: writerCommunityDID,
				SubjectURI:   row.subjectURI,
				SubjectCID:   row.subjectCID,
				Code:         DecisionCode("spam"),
				CreatedAt:    instanceRemovalCreatedAt,
			})
			require.NoError(t, err)

			require.Lenf(t, repo.batches, 1, "the removal must be exactly one commit")
			assert.ElementsMatch(t, row.wantShape, shapeOf(repo.batches[0]))
			assert.Equal(t, repo.head.CID, repo.batches[0].swapCommit, "the commit must be guarded by the head read before the pre-reads")
			assert.Empty(t, repo.puts)

			assert.Equalf(t, instanceRemovalBody(row.subjectURI, row.subjectCID, "spam"),
				recordBody(t, repo, RemovalCollection, row.subjectURI),
				"the record carries the reviewed strongRef, the code and the action's createdAt, and never a reason")
			assertNoRecord(t, repo, AcceptanceCollection, row.subjectURI)

			assert.Nil(t, result.Standing)
			assert.Equal(t, row.wantWithdrew, result.WithdrewAcceptance)
			assert.False(t, result.Skipped)
			assert.Equal(t, "3kjzl5batchaa", result.Rev)
			assert.Equal(t, "bafyreibatchaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", result.CID)
		})
	}
}

func TestInstanceRemovalWriter_AStandingRemovalIsReturnedAndNothingIsWritten(t *testing.T) {
	t.Parallel()

	// Any removal standing at the rkey, whoever wrote it, is the answer: the
	// writer reports what stands and the caller decides whether it is its own.
	repo := newFakeCommunityRepo(writerCommunityDID)
	seedRemoval(repo, writerPostURI, instanceRemovalForeignCID, foreignRemovalBody(writerPostURI))

	result, err := newInstanceRemovalWriter(repo).WriteInstanceRemoval(context.Background(), InstanceRemovalCommand{
		CommunityDID: writerCommunityDID,
		SubjectURI:   writerPostURI,
		SubjectCID:   writerPostCID,
		Code:         DecisionCode("spam"),
		CreatedAt:    instanceRemovalCreatedAt,
	})
	require.NoError(t, err)

	assert.Equal(t, &StandingRemoval{
		CID:        instanceRemovalForeignCID,
		SubjectURI: writerPostURI,
		SubjectCID: "bafyreiolderversionaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Code:       "rule-violation",
		Reason:     "off-topic thread",
		CreatedAt:  "2026-10-01T08:00:00Z",
	}, result.Standing)
	assert.True(t, result.Skipped)
	assert.Equal(t, "3kjzl5headaaa", result.Rev, "a skip reports the head read before the pre-reads")
	assert.False(t, result.WithdrewAcceptance)
	assert.Empty(t, repo.batches)
	assert.Empty(t, repo.puts)
	assert.Equal(t, foreignRemovalBody(writerPostURI), recordBody(t, repo, RemovalCollection, writerPostURI))
}

func TestInstanceRemovalWriter_ReshapesWhenACommitLandsBetweenThePreReadAndTheWrite(t *testing.T) {
	t.Parallel()

	t.Run("an acceptance lands: the retried commit withdraws it", func(t *testing.T) {
		t.Parallel()
		repo := newFakeCommunityRepo(writerCommunityDID)
		repo.beforeFirstWrite = func(r *fakeCommunityRepo) {
			r.landCommit(func() { seedAcceptance(r, writerPostURI, writerPostCID) })
		}

		result, err := newInstanceRemovalWriter(repo).WriteInstanceRemoval(context.Background(), InstanceRemovalCommand{
			CommunityDID: writerCommunityDID,
			SubjectURI:   writerPostURI,
			SubjectCID:   writerPostCID,
			Code:         DecisionCode("moderator-discretion"),
			CreatedAt:    instanceRemovalCreatedAt,
		})
		require.NoError(t, err)

		assert.Equal(t, instanceRemovalBody(writerPostURI, writerPostCID, "moderator-discretion"),
			recordBody(t, repo, RemovalCollection, writerPostURI))
		assertNoRecord(t, repo, AcceptanceCollection, writerPostURI)
		assert.True(t, result.WithdrewAcceptance, "the commit that landed deleted the acceptance the re-read found")
		require.NotEmpty(t, repo.batches)
		assert.ElementsMatch(t, []writeShape{
			{op: pds.WriteOpDelete, collection: AcceptanceCollection},
			{op: pds.WriteOpCreate, collection: RemovalCollection},
		}, shapeOf(repo.batches[len(repo.batches)-1]))
	})

	t.Run("a removal lands: it is returned and nothing is written over it", func(t *testing.T) {
		t.Parallel()
		repo := newFakeCommunityRepo(writerCommunityDID)
		repo.beforeFirstWrite = func(r *fakeCommunityRepo) {
			r.landCommit(func() { seedRemoval(r, writerPostURI, instanceRemovalForeignCID, foreignRemovalBody(writerPostURI)) })
		}

		result, err := newInstanceRemovalWriter(repo).WriteInstanceRemoval(context.Background(), InstanceRemovalCommand{
			CommunityDID: writerCommunityDID,
			SubjectURI:   writerPostURI,
			SubjectCID:   writerPostCID,
			Code:         DecisionCode("spam"),
			CreatedAt:    instanceRemovalCreatedAt,
		})
		require.NoError(t, err)

		require.NotNil(t, result.Standing)
		assert.Equal(t, instanceRemovalForeignCID, result.Standing.CID)
		assert.Equal(t, foreignRemovalBody(writerPostURI), recordBody(t, repo, RemovalCollection, writerPostURI))
	})
}

// ---------------------------------------------------------------------------
// StandingAcceptance and StandingRemoval
// ---------------------------------------------------------------------------

func TestInstanceRemovalWriter_ReadsTheStandingRecords(t *testing.T) {
	t.Parallel()

	repo := newFakeCommunityRepo(writerCommunityDID)
	seedAcceptance(repo, writerPostURI, writerPostCID)
	seedRemoval(repo, instanceRemovalCommentURI, instanceRemovalOwnedCID,
		instanceRemovalBody(instanceRemovalCommentURI, instanceRemovalCommentCID, "spam"))
	writer := newInstanceRemovalWriter(repo)
	ctx := context.Background()

	acceptance, err := writer.StandingAcceptance(ctx, writerCommunityDID, writerPostURI)
	require.NoError(t, err)
	assert.Equal(t, &StandingRecordRef{CID: instanceRemovalStandingCID, SubjectCID: writerPostCID}, acceptance)

	removal, err := writer.StandingRemoval(ctx, writerCommunityDID, instanceRemovalCommentURI)
	require.NoError(t, err)
	assert.Equal(t, &StandingRemoval{
		CID:        instanceRemovalOwnedCID,
		SubjectURI: instanceRemovalCommentURI,
		SubjectCID: instanceRemovalCommentCID,
		Code:       "spam",
		CreatedAt:  instanceRemovalCreatedAt,
	}, removal)

	acceptance, err = writer.StandingAcceptance(ctx, writerCommunityDID, instanceRemovalCommentURI)
	require.NoError(t, err)
	assert.Nil(t, acceptance, "no acceptance stands for the comment")

	removal, err = writer.StandingRemoval(ctx, writerCommunityDID, writerPostURI)
	require.NoError(t, err)
	assert.Nil(t, removal, "no removal stands for the post")
}

// ---------------------------------------------------------------------------
// B2: UndoInstanceRemoval
// ---------------------------------------------------------------------------

func TestInstanceRemovalWriter_UndoDeletesItsOwnRemovalAndRestoresTheAcceptanceWhenAsked(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name           string
		acceptanceCID  string
		wantShape      []writeShape
		wantAcceptance map[string]any
	}{
		{
			name:          "with an acceptance",
			acceptanceCID: instanceRemovalRestoredCID,
			wantShape: []writeShape{
				{op: pds.WriteOpDelete, collection: RemovalCollection},
				{op: pds.WriteOpCreate, collection: AcceptanceCollection},
			},
			wantAcceptance: map[string]any{
				"$type":     AcceptanceCollection,
				"subject":   map[string]any{"uri": writerPostURI, "cid": instanceRemovalRestoredCID},
				"createdAt": "2026-07-01T12:00:00Z",
			},
		},
		{
			name:      "delete only",
			wantShape: []writeShape{{op: pds.WriteOpDelete, collection: RemovalCollection}},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeCommunityRepo(writerCommunityDID)
			seedRemoval(repo, writerPostURI, instanceRemovalOwnedCID, instanceRemovalBody(writerPostURI, writerPostCID, "spam"))

			result, err := newInstanceRemovalWriter(repo).UndoInstanceRemoval(context.Background(), InstanceRemovalUndoCommand{
				CommunityDID:  writerCommunityDID,
				SubjectURI:    writerPostURI,
				RemovalCID:    instanceRemovalOwnedCID,
				AcceptanceCID: row.acceptanceCID,
			})
			require.NoError(t, err)

			require.Lenf(t, repo.batches, 1, "the undo must be exactly one commit")
			assert.ElementsMatch(t, row.wantShape, shapeOf(repo.batches[0]))
			assert.Equal(t, repo.head.CID, repo.batches[0].swapCommit)
			assert.False(t, result.Skipped)
			assert.Equal(t, "3kjzl5batchaa", result.Rev)

			assertNoRecord(t, repo, RemovalCollection, writerPostURI)
			if row.wantAcceptance == nil {
				assertNoRecord(t, repo, AcceptanceCollection, writerPostURI)
			} else {
				assert.Equal(t, row.wantAcceptance, recordBody(t, repo, AcceptanceCollection, writerPostURI))
			}
		})
	}
}

func TestInstanceRemovalWriter_UndoSkipsAnyRemovalThatIsNotTheOneNamed(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		seed       func(repo *fakeCommunityRepo)
		race       func(repo *fakeCommunityRepo)
		wantRemain bool
	}{
		{
			name: "a different removal stands",
			seed: func(repo *fakeCommunityRepo) {
				seedRemoval(repo, writerPostURI, instanceRemovalForeignCID, foreignRemovalBody(writerPostURI))
			},
			wantRemain: true,
		},
		{
			name: "no removal stands",
			seed: func(*fakeCommunityRepo) {},
		},
		{
			name: "the removal is replaced between the pre-read and the commit",
			seed: func(repo *fakeCommunityRepo) {
				seedRemoval(repo, writerPostURI, instanceRemovalOwnedCID, instanceRemovalBody(writerPostURI, writerPostCID, "spam"))
			},
			race: func(repo *fakeCommunityRepo) {
				repo.landCommit(func() {
					seedRemoval(repo, writerPostURI, instanceRemovalForeignCID, foreignRemovalBody(writerPostURI))
				})
			},
			wantRemain: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeCommunityRepo(writerCommunityDID)
			row.seed(repo)
			repo.beforeFirstWrite = row.race

			result, err := newInstanceRemovalWriter(repo).UndoInstanceRemoval(context.Background(), InstanceRemovalUndoCommand{
				CommunityDID:  writerCommunityDID,
				SubjectURI:    writerPostURI,
				RemovalCID:    instanceRemovalOwnedCID,
				AcceptanceCID: instanceRemovalRestoredCID,
			})
			require.NoError(t, err)

			assert.True(t, result.Skipped, "an undo whose removal does not stand writes nothing")
			assertNoRecord(t, repo, AcceptanceCollection, writerPostURI)
			if row.wantRemain {
				assert.Equal(t, foreignRemovalBody(writerPostURI), recordBody(t, repo, RemovalCollection, writerPostURI),
					"a removal someone else wrote is left exactly as it stands")
			} else {
				assertNoRecord(t, repo, RemovalCollection, writerPostURI)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// B3: the acceptance writer against a removal committed after its pre-read
// ---------------------------------------------------------------------------

func TestWriter_AcceptanceRefusesARemovalCommittedAfterThePreRead(t *testing.T) {
	t.Parallel()

	// A removal committed between the acceptance writer's pre-reads and its
	// put touches only the removal rkey, so a put guarded by the acceptance
	// record alone would still land and leave both records standing. The put
	// is guarded by the head read before the pre-reads, so the commit is a
	// conflict and the re-read meets the removal.
	repo := newFakeCommunityRepo(writerCommunityDID)
	repo.beforeFirstWrite = func(r *fakeCommunityRepo) {
		r.landCommit(func() {
			seedRemoval(r, writerPostURI, instanceRemovalForeignCID, foreignRemovalBody(writerPostURI))
		})
	}
	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock,
		WithSwapRetrySleeper(func(context.Context, time.Duration) error { return nil }))

	_, err := writer.WriteAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	})
	require.ErrorIs(t, err, ErrRemovalStands)
	assertNoRecord(t, repo, AcceptanceCollection, writerPostURI)
}

// ---------------------------------------------------------------------------
// I2: unrelated commits between the acceptance pre-read and its put
// ---------------------------------------------------------------------------

// unrelatedCommitURI is another subject's rkey in the same community: a
// commit there moves the head without touching this subject's acceptance or
// removal.
const unrelatedCommitURI = "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.postv2/3kjzl5unrelated"

func landUnrelatedCommit(r *fakeCommunityRepo) {
	r.landCommit(func() {
		seedAcceptance(r, unrelatedCommitURI, "bafyreiunrelatedaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	})
}

func TestWriter_AcceptanceSurvivesABurstOfUnrelatedCommits(t *testing.T) {
	t.Parallel()

	// The put is guarded by the head, so any commit in the community — an
	// aggregator run accepting other posts — is a conflict. A conflict whose
	// re-read shows no removal and an unchanged acceptance is not a lost race
	// for THIS record and must not spend the swap-retry budget: three such
	// passes in a row (one more than swapRetryLimit) still end in the write.
	repo := newFakeCommunityRepo(writerCommunityDID)
	repo.beforeEachWrite = func(r *fakeCommunityRepo, write int) {
		if write < 3 {
			landUnrelatedCommit(r)
		}
	}
	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock,
		WithSwapRetrySleeper(func(context.Context, time.Duration) error { return nil }))

	result, err := writer.WriteAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	})
	require.NoError(t, err, "head-only conflicts from unrelated commits must not exhaust the retry budget")
	assert.False(t, result.Skipped)
	assert.Len(t, repo.puts, 4, "three conflicted puts, then the one that lands")
	subject, _ := recordBody(t, repo, AcceptanceCollection, writerPostURI)["subject"].(map[string]any)
	assert.Equal(t, writerPostCID, subject["cid"])
}

func TestWriter_AcceptanceUnderEndlessUnrelatedCommitsGivesUp(t *testing.T) {
	t.Parallel()

	// The head-only allowance is bounded: a community whose repo never stops
	// moving defers the subject rather than spinning a worker forever.
	repo := newFakeCommunityRepo(writerCommunityDID)
	repo.beforeEachWrite = func(r *fakeCommunityRepo, _ int) { landUnrelatedCommit(r) }
	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock,
		WithSwapRetrySleeper(func(context.Context, time.Duration) error { return nil }))

	_, err := writer.WriteAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	})
	require.ErrorIs(t, err, pds.ErrSwapConflict)
	assert.Less(t, len(repo.puts), 100, "the head-only allowance must have a cap")
	assertNoRecord(t, repo, AcceptanceCollection, writerPostURI)
}

func TestWriter_AcceptanceRefusesARemovalLandingAfterUnrelatedCommits(t *testing.T) {
	t.Parallel()

	// The head-only allowance must not skip the removal re-read: unrelated
	// commits on the first passes, then a removal, still refuse.
	repo := newFakeCommunityRepo(writerCommunityDID)
	repo.beforeEachWrite = func(r *fakeCommunityRepo, write int) {
		switch {
		case write < 2:
			landUnrelatedCommit(r)
		case write == 2:
			r.landCommit(func() {
				seedRemoval(r, writerPostURI, instanceRemovalForeignCID, foreignRemovalBody(writerPostURI))
			})
		}
	}
	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock,
		WithSwapRetrySleeper(func(context.Context, time.Duration) error { return nil }))

	_, err := writer.WriteAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	})
	require.ErrorIs(t, err, ErrRemovalStands)
	assertNoRecord(t, repo, AcceptanceCollection, writerPostURI)
}

// ---------------------------------------------------------------------------
// C5: the before-commit callback
// ---------------------------------------------------------------------------

func TestInstanceRemovalWriter_BeforeCommitReportsWhetherEachBatchDeletesAnAcceptance(t *testing.T) {
	t.Parallel()

	// The caller persists "an acceptance was seen" before a batch that
	// deletes one is sent: if that batch's response is lost, the evidence
	// already stands. An acceptance landing after the first pass forces a
	// reshape, and the reshaped batch is reported as deleting it.
	repo := newFakeCommunityRepo(writerCommunityDID)
	repo.beforeFirstWrite = func(r *fakeCommunityRepo) {
		r.landCommit(func() { seedAcceptance(r, writerPostURI, writerPostCID) })
	}

	var reported []bool
	result, err := newInstanceRemovalWriter(repo).WriteInstanceRemoval(context.Background(), InstanceRemovalCommand{
		CommunityDID: writerCommunityDID,
		SubjectURI:   writerPostURI,
		SubjectCID:   writerPostCID,
		Code:         DecisionCode("spam"),
		CreatedAt:    instanceRemovalCreatedAt,
		BeforeCommit: func(_ context.Context, deletesAcceptance bool) error {
			reported = append(reported, deletesAcceptance)
			repo.calls = append(repo.calls, "BeforeCommit")
			return nil
		},
	})
	require.NoError(t, err)
	assert.True(t, result.WithdrewAcceptance)

	assert.Equal(t, []bool{false, true}, reported,
		"one call per batch: the first shaped with no acceptance, the reshaped one deleting it")

	var sequence []string
	for _, call := range repo.calls {
		if call == "BeforeCommit" || call == "ApplyWrites" {
			sequence = append(sequence, call)
		}
	}
	assert.Equal(t, []string{"BeforeCommit", "ApplyWrites", "BeforeCommit", "ApplyWrites"}, sequence,
		"the callback runs before each batch is sent")
}

func TestInstanceRemovalWriter_BeforeCommitErrorAbortsWithNothingWritten(t *testing.T) {
	t.Parallel()

	errEvidence := errors.New("evidence save failed")

	for name, row := range map[string]struct {
		failOnCall  int
		wantBatches int
	}{
		"first pass":    {failOnCall: 1, wantBatches: 0},
		"reshaped pass": {failOnCall: 2, wantBatches: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeCommunityRepo(writerCommunityDID)
			seedAcceptance(repo, writerPostURI, writerPostCID)
			repo.beforeFirstWrite = func(r *fakeCommunityRepo) { landUnrelatedCommit(r) }

			calls := 0
			_, err := newInstanceRemovalWriter(repo).WriteInstanceRemoval(context.Background(), InstanceRemovalCommand{
				CommunityDID: writerCommunityDID,
				SubjectURI:   writerPostURI,
				SubjectCID:   writerPostCID,
				Code:         DecisionCode("spam"),
				CreatedAt:    instanceRemovalCreatedAt,
				BeforeCommit: func(context.Context, bool) error {
					calls++
					if calls == row.failOnCall {
						return errEvidence
					}
					return nil
				},
			})
			require.ErrorIs(t, err, errEvidence)
			assert.Len(t, repo.batches, row.wantBatches, "no batch may be sent after the callback fails")
			assertNoRecord(t, repo, RemovalCollection, writerPostURI)
			assert.Equal(t, instanceRemovalStandingCID,
				repo.records[repo.key(AcceptanceCollection, SubjectRkey(writerPostURI))].CID,
				"the standing acceptance is untouched")
		})
	}
}
