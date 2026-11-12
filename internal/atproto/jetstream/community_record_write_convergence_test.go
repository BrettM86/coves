//go:build integration

package jetstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B8: an instance removal and restore of a post its community never accepted
// leaves it where it was before the removal: pending, in the acceptance
// engine's backlog. The real community record write queue stamps the
// admission row with the revs of its own commits; the community's echoes of
// those commits then arrive through the real post consumer in either order and
// must not move it.

// convergenceRepo is an in-memory community repo whose commits carry the given
// revs in order.
type convergenceRepo struct {
	mu       sync.Mutex
	revs     []string
	removals map[string]posts.StandingRemoval
}

func (repo *convergenceRepo) nextRev() string {
	rev := repo.revs[0]
	repo.revs = repo.revs[1:]
	return rev
}

func (repo *convergenceRepo) StandingAcceptance(context.Context, string, string) (*posts.StandingRecordRef, error) {
	return nil, nil
}

func (repo *convergenceRepo) StandingRemoval(_ context.Context, _, subjectURI string) (*posts.StandingRemoval, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if removal, ok := repo.removals[subjectURI]; ok {
		return &removal, nil
	}
	return nil, nil
}

func (repo *convergenceRepo) WriteInstanceRemoval(_ context.Context, cmd posts.InstanceRemovalCommand) (posts.InstanceRemovalResult, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	removal := posts.StandingRemoval{CID: "bafyreiremovalrecord", SubjectURI: cmd.SubjectURI, SubjectCID: cmd.SubjectCID, Code: string(cmd.Code), CreatedAt: cmd.CreatedAt}
	repo.removals[cmd.SubjectURI] = removal
	return posts.InstanceRemovalResult{CommunityWriteResult: posts.CommunityWriteResult{CID: removal.CID, Rev: repo.nextRev()}}, nil
}

func (repo *convergenceRepo) UndoInstanceRemoval(_ context.Context, cmd posts.InstanceRemovalUndoCommand) (posts.CommunityWriteResult, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	delete(repo.removals, cmd.SubjectURI)
	return posts.CommunityWriteResult{Rev: repo.nextRev()}, nil
}

func TestCommunityRecordWriteQueue_NeverAcceptedPostReturnsToTheBacklogAfterRestore(t *testing.T) {
	for _, start := range []string{"pending", "rejected"} {
		for _, order := range []string{"put echo first", "delete echo first"} {
			t.Run(start+", "+order, func(t *testing.T) {
				t.Parallel()
				db := testkit.DB(t)
				ctx := context.Background()
				f := newAccFixture(t, db)
				_, err := db.ExecContext(ctx, `UPDATE communities SET pds_refresh_token_encrypted = $2 WHERE did = $1`,
					accCommunity, []byte("a stored refresh token"))
				require.NoError(t, err, "fixture: this AppView hosts the community")
				base := time.Now().UnixMicro()

				const cid = "bafyreineveraccepted"
				uri := f.indexPV2(t, "neveraccepted", cid, base)
				if start == "rejected" {
					_, err := f.admissions.RecordRejection(ctx, posts.RecordRejectionCommand{
						CommunityDID: accCommunity, PostURI: uri, DecisionCode: string(posts.DecisionRateLimitExceeded),
						JudgedCID: cid, Redrivable: false,
					})
					require.NoError(t, err, "fixture: the engine rejected the post")
				}

				moderationRepository := postgres.NewModerationRepository(db)
				removedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
				var actionID string
				require.NoError(t, moderationRepository.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
					action, err := tx.InsertAction(ctx, moderation.Action{
						ActorDID: "did:plc:accadmin", AuthorityDID: "did:web:coves.test", ScopeKind: moderation.ScopeInstance,
						SubjectURI: uri, SubjectCollection: moderation.PostV2Collection, SubjectCommunityDID: accCommunity,
						ObservedCID: cid, Action: moderation.ActionRemove,
						Reason: "social.coves.moderation.defs#reasonSpam", Origin: moderation.OriginLocal, CreatedAt: removedAt,
					})
					if err != nil {
						return err
					}
					actionID = action.ID
					return tx.RequireCommunityRecordRemoval(ctx, moderation.CommunityRecordWrite{
						CommunityDID: accCommunity, SubjectURI: uri, SubjectCollection: moderation.PostV2Collection,
						SubjectCID: cid, RemovalActionID: action.ID, Code: posts.DecisionSpam, RemovalCreatedAt: removedAt,
					})
				}))

				revs := increasingTIDs(t, 2)
				repo := &convergenceRepo{revs: append([]string(nil), revs...), removals: map[string]posts.StandingRemoval{}}
				// The queue's clock runs a minute ahead so rows the database
				// stamped due now are due on a database clock a little ahead of
				// the test host.
				queue := moderation.NewCommunityRecordWriteQueue(moderationRepository, repo, f.admissions,
					moderation.CommunityRecordWriteQueueConfig{Now: func() time.Time { return time.Now().Add(time.Minute) }})
				subject := moderation.CommunityRecordSubject{CommunityDID: accCommunity, SubjectURI: uri}

				queue.WriteAfterCommit(ctx, subject)
				require.NoError(t, queue.Wait(ctx))
				row, err := f.admissions.Get(ctx, accCommunity, uri)
				require.NoError(t, err)
				require.Equal(t, posts.AdmissionStatusRemoved, row.Status, "fixture: the queue stamped its removal")

				var restoreRequested bool
				require.NoError(t, moderationRepository.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
					var err error
					restoreRequested, err = tx.RequestCommunityRecordRestore(ctx, accCommunity, uri, actionID, cid)
					return err
				}))
				require.True(t, restoreRequested, "fixture: the removal action owns the row")
				queue.WriteAfterCommit(ctx, subject)
				require.NoError(t, queue.Wait(ctx))
				_, removalStands := repo.removals[uri]
				require.False(t, removalStands, "fixture: the queue undid its removal")

				put := removalEvent(accCommunity, uri, cid, string(posts.DecisionSpam), revs[0], base+1_000_000)
				deletion := removalDeleteEvent(accCommunity, uri, revs[1], base+2_000_000)
				echoes := []*JetstreamEvent{put, deletion}
				if order == "delete echo first" {
					echoes = []*JetstreamEvent{deletion, put}
				}
				for _, echo := range echoes {
					require.NoError(t, f.consumer.HandleEvent(ctx, echo))
				}

				row, err = f.admissions.Get(ctx, accCommunity, uri)
				require.NoError(t, err)
				assert.Equal(t, posts.AdmissionStatusPending, row.Status,
					"the removal and its undo are both older than or equal to the queue's own stamp: the post is back where it was before the removal")
				assert.Nil(t, row.DecisionCode)
				backlog, err := f.admissions.ListPendingSubjects(ctx, 100)
				require.NoError(t, err)
				var listed bool
				for _, pending := range backlog {
					listed = listed || (pending.CommunityDID == accCommunity && pending.PostURI == uri)
				}
				assert.True(t, listed, "the acceptance engine decides the post afresh")
			})
		}
	}
}
