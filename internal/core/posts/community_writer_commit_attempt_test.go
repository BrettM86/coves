package posts

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/pds"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The community moderator's remove and restore at T0: the command's createdAt
// stamps a newly created record, and BeforeCommit sees each batch's guard, the
// pre-read stand CID and the exact stand body before the batch is sent.

const (
	// commitAttemptCreatedAt is the intent's createdAt the command carries.
	// writerClock is a different time, so a record stamped by the clock fails.
	commitAttemptCreatedAt = "2026-11-14T09:30:00Z"

	// The fake repo's head, which every batch is guarded by.
	commitAttemptHeadCID = "bafyreiheadaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitAttemptHeadRev = "3kjzl5headaaa"

	commitAttemptSpamRemovalCID = "bafyreistandingspamremovalaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitAttemptOlderPostCID   = "bafyreiolderversionaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func newCommitAttemptWriter(repo *fakeCommunityRepo) CommunityRecordWriter {
	return NewCommunityRecordWriter(fixedFactory(repo), writerClock,
		WithSwapRetrySleeper(func(context.Context, time.Duration) error { return nil }))
}

// seedSpamRemoval stands a moderator's spam removal created on 2026-10-01.
func seedSpamRemoval(repo *fakeCommunityRepo) {
	seedRemoval(repo, writerPostURI, commitAttemptSpamRemovalCID, map[string]any{
		"$type":     RemovalCollection,
		"subject":   map[string]any{"uri": writerPostURI, "cid": writerPostCID},
		"code":      "spam",
		"createdAt": "2026-10-01T08:00:00Z",
	})
}

// observedAttempt is one BeforeCommit call and how many batches the repo had
// received when it ran.
type observedAttempt struct {
	attempt      CommunityCommitAttempt
	batchesSoFar int
}

func TestWriter_CommunityCommitUsesCommandCreatedAtAndReportsTheAttemptBeforeTheBatch(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		seed  func(repo *fakeCommunityRepo)
		write func(ctx context.Context, writer CommunityRecordWriter, hook func(context.Context, CommunityCommitAttempt) error) error
		// wantShape is the one batch sent; wantBody is the stand record in it.
		wantShape    []writeShape
		wantBody     string
		wantStandCID string
	}{
		{
			name: "remove creates the removal and withdraws the acceptance",
			seed: func(repo *fakeCommunityRepo) { seedAcceptance(repo, writerPostURI, writerPostCID) },
			write: func(ctx context.Context, writer CommunityRecordWriter, hook func(context.Context, CommunityCommitAttempt) error) error {
				_, err := writer.WriteRemoval(ctx, CommunityRemovalCommand{
					CommunityDID: writerCommunityDID, PostURI: writerPostURI, PostCID: writerPostCID,
					Code: DecisionCode("spam"), Reason: "selling followers",
					CreatedAt: commitAttemptCreatedAt, BeforeCommit: hook,
				})
				return err
			},
			wantShape: []writeShape{
				{op: pds.WriteOpDelete, collection: AcceptanceCollection},
				{op: pds.WriteOpCreate, collection: RemovalCollection},
			},
			wantBody: `{"$type":"social.coves.community.removal",
				"subject":{"uri":"at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.community.postv2/3kjzl5kcb2s2v","cid":"bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"code":"spam","reason":"selling followers","createdAt":"2026-11-14T09:30:00Z"}`,
			wantStandCID: "",
		},
		{
			name: "remove rewrites a standing spam removal to off-topic and keeps its createdAt",
			seed: seedSpamRemoval,
			write: func(ctx context.Context, writer CommunityRecordWriter, hook func(context.Context, CommunityCommitAttempt) error) error {
				_, err := writer.WriteRemoval(ctx, CommunityRemovalCommand{
					CommunityDID: writerCommunityDID, PostURI: writerPostURI, PostCID: writerPostCID,
					Code: DecisionCode("off-topic"), Reason: "wrong community",
					CreatedAt: commitAttemptCreatedAt, BeforeCommit: hook,
				})
				return err
			},
			wantShape: []writeShape{{op: pds.WriteOpUpdate, collection: RemovalCollection}},
			wantBody: `{"$type":"social.coves.community.removal",
				"subject":{"uri":"at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.community.postv2/3kjzl5kcb2s2v","cid":"bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"code":"off-topic","reason":"wrong community","createdAt":"2026-10-01T08:00:00Z"}`,
			wantStandCID: commitAttemptSpamRemovalCID,
		},
		{
			name: "restore deletes the removal and creates the acceptance when none stands",
			seed: seedSpamRemoval,
			write: func(ctx context.Context, writer CommunityRecordWriter, hook func(context.Context, CommunityCommitAttempt) error) error {
				_, err := writer.RestoreAcceptance(ctx, CommunityWriteCommand{
					CommunityDID: writerCommunityDID, PostURI: writerPostURI, PostCID: writerPostCID,
					CreatedAt: commitAttemptCreatedAt, BeforeCommit: hook,
				})
				return err
			},
			wantShape: []writeShape{
				{op: pds.WriteOpDelete, collection: RemovalCollection},
				{op: pds.WriteOpCreate, collection: AcceptanceCollection},
			},
			wantBody: `{"$type":"social.coves.community.acceptance",
				"subject":{"uri":"at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.community.postv2/3kjzl5kcb2s2v","cid":"bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"createdAt":"2026-11-14T09:30:00Z"}`,
			wantStandCID: "",
		},
		{
			name: "restore repins an acceptance standing on an older version and keeps its createdAt",
			seed: func(repo *fakeCommunityRepo) {
				seedSpamRemoval(repo)
				seedAcceptance(repo, writerPostURI, commitAttemptOlderPostCID)
			},
			write: func(ctx context.Context, writer CommunityRecordWriter, hook func(context.Context, CommunityCommitAttempt) error) error {
				_, err := writer.RestoreAcceptance(ctx, CommunityWriteCommand{
					CommunityDID: writerCommunityDID, PostURI: writerPostURI, PostCID: writerPostCID,
					CreatedAt: commitAttemptCreatedAt, BeforeCommit: hook,
				})
				return err
			},
			wantShape: []writeShape{
				{op: pds.WriteOpDelete, collection: RemovalCollection},
				{op: pds.WriteOpUpdate, collection: AcceptanceCollection},
			},
			wantBody: `{"$type":"social.coves.community.acceptance",
				"subject":{"uri":"at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.community.postv2/3kjzl5kcb2s2v","cid":"bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"createdAt":"2026-06-30T09:00:00Z"}`,
			wantStandCID: instanceRemovalStandingCID,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeCommunityRepo(writerCommunityDID)
			row.seed(repo)

			var observed []observedAttempt
			hook := func(_ context.Context, attempt CommunityCommitAttempt) error {
				observed = append(observed, observedAttempt{attempt: attempt, batchesSoFar: len(repo.batches)})
				return nil
			}
			require.NoError(t, row.write(context.Background(), newCommitAttemptWriter(repo), hook))

			require.Len(t, repo.batches, 1)
			batch := repo.batches[0]
			assert.Equal(t, row.wantShape, shapeOf(batch))
			assert.Equal(t, commitAttemptHeadCID, batch.swapCommit)
			sent, err := json.Marshal(batch.writes[len(batch.writes)-1].Record)
			require.NoError(t, err)
			assert.JSONEq(t, row.wantBody, string(sent), "the stand record sent in the batch")

			require.Len(t, observed, 1, "BeforeCommit runs once for the one batch")
			got := observed[0]
			assert.Zero(t, got.batchesSoFar, "BeforeCommit runs before the batch is sent")
			assert.Equal(t, commitAttemptHeadCID, got.attempt.GuardCommitCID, "the head used as swapCommit")
			assert.Equal(t, commitAttemptHeadRev, got.attempt.GuardRev)
			assert.Equal(t, row.wantStandCID, got.attempt.StandCID, "the pre-read CID of the record being made to stand")
			assert.JSONEq(t, row.wantBody, string(got.attempt.StandRecord), "the exact stand body about to be sent")
		})
	}
}

func TestWriter_CommunityCommitBeforeCommitErrorSendsNothing(t *testing.T) {
	t.Parallel()

	errAttempt := errors.New("attempt save failed")

	for _, row := range []struct {
		name  string
		seed  func(repo *fakeCommunityRepo)
		write func(ctx context.Context, writer CommunityRecordWriter, hook func(context.Context, CommunityCommitAttempt) error) error
	}{
		{
			name: "remove",
			seed: func(repo *fakeCommunityRepo) { seedAcceptance(repo, writerPostURI, writerPostCID) },
			write: func(ctx context.Context, writer CommunityRecordWriter, hook func(context.Context, CommunityCommitAttempt) error) error {
				_, err := writer.WriteRemoval(ctx, CommunityRemovalCommand{
					CommunityDID: writerCommunityDID, PostURI: writerPostURI, PostCID: writerPostCID,
					Code: DecisionCode("spam"), CreatedAt: commitAttemptCreatedAt, BeforeCommit: hook,
				})
				return err
			},
		},
		{
			name: "restore",
			seed: seedSpamRemoval,
			write: func(ctx context.Context, writer CommunityRecordWriter, hook func(context.Context, CommunityCommitAttempt) error) error {
				_, err := writer.RestoreAcceptance(ctx, CommunityWriteCommand{
					CommunityDID: writerCommunityDID, PostURI: writerPostURI, PostCID: writerPostCID,
					CreatedAt: commitAttemptCreatedAt, BeforeCommit: hook,
				})
				return err
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeCommunityRepo(writerCommunityDID)
			row.seed(repo)

			err := row.write(context.Background(), newCommitAttemptWriter(repo),
				func(context.Context, CommunityCommitAttempt) error { return errAttempt })

			require.ErrorIs(t, err, errAttempt)
			assert.Empty(t, repo.batches, "no batch may be sent after BeforeCommit fails")
		})
	}
}
