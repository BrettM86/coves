//go:build integration

package jetstream

import (
	"context"
	"errors"
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

// The community's firehose echoes of removal records this AppView wrote itself
// (the instance removal outbox, migration 080, and the acceptance engine) log
// no community moderation action: the instance action, or the engine's
// decision, is that removal's audit.

// echoRemovedAt is the instance removal's time; the removal record the outbox
// writes carries it as its createdAt, the createdAt removalEvent's record has.
var echoRemovedAt = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// echoingConvergenceRepo is convergenceRepo with a hook run after a removal
// write commits and before the queue sees its result.
type echoingConvergenceRepo struct {
	*convergenceRepo
	afterRemovalWrite func()
}

func (repo *echoingConvergenceRepo) WriteInstanceRemoval(ctx context.Context, cmd posts.InstanceRemovalCommand) (posts.InstanceRemovalResult, error) {
	result, err := repo.convergenceRepo.WriteInstanceRemoval(ctx, cmd)
	if err == nil && repo.afterRemovalWrite != nil {
		repo.afterRemovalWrite()
	}
	return result, err
}

// restoreStampFailingAdmissions is the Postgres admission repository whose
// next failRestoreStamps restore stamps (ApplyRemovalDelete) fail.
type restoreStampFailingAdmissions struct {
	posts.AdmissionRepository
	mu                sync.Mutex
	failRestoreStamps int
}

func (a *restoreStampFailingAdmissions) ApplyRemovalDelete(ctx context.Context, cmd posts.CommunityDeleteCommand) (posts.AdmissionResult, error) {
	a.mu.Lock()
	fail := a.failRestoreStamps > 0
	if fail {
		a.failRestoreStamps--
	}
	a.mu.Unlock()
	if fail {
		return posts.AdmissionResult{}, errors.New("postgres: connection reset")
	}
	return a.AdmissionRepository.ApplyRemovalDelete(ctx, cmd)
}

func (a *restoreStampFailingAdmissions) ReleaseRemovalRecord(ctx context.Context, communityDID, postURI, rev string) error {
	return a.AdmissionRepository.(moderation.CommunityRemovalRecordRelease).ReleaseRemovalRecord(ctx, communityDID, postURI, rev)
}

// echoClock is the queue's clock: a minute ahead of the wall clock (rows the
// database stamped due now are due on it), plus whatever the test adds.
type echoClock struct {
	mu    sync.Mutex
	extra time.Duration
}

func (c *echoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(time.Minute + c.extra)
}

func (c *echoClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.extra += d
}

// subjectActionScopes is the scope of every moderation_actions row about
// subjectURI, oldest first.
func subjectActionScopes(t *testing.T, f accFixture, subjectURI string) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT scope_kind || ':' || action FROM moderation_actions WHERE subject_uri = $1 ORDER BY created_at, id`, subjectURI)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var scope string
		require.NoError(t, rows.Scan(&scope))
		out = append(out, scope)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestCommunityRecordWriteQueue_OutboxRemovalEchoesLogNoCommunityAction(t *testing.T) {
	tests := []struct {
		name string
		// echoDuringWrite delivers the removal create echo after the outbox's
		// write commits and before its stamp.
		echoDuringWrite bool
		// restore has the outbox undo its removal, then delivers the delete
		// echo; failRestoreStamp makes the restore stamp fail once, so the
		// echo arrives while the stamp is still owed.
		restore, failRestoreStamp bool
		wantStatus                posts.AdmissionStatus
	}{
		{name: "removal stamped, then its echo", wantStatus: posts.AdmissionStatusRemoved},
		{name: "removal echo before its stamp", echoDuringWrite: true, wantStatus: posts.AdmissionStatusRemoved},
		{name: "restore stamped, then its delete echo", restore: true, wantStatus: posts.AdmissionStatusPending},
		{name: "restore delete echo before its stamp", restore: true, failRestoreStamp: true, wantStatus: posts.AdmissionStatusPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f := newAccFixture(t, db)
			_, err := db.ExecContext(ctx, `UPDATE communities SET pds_refresh_token_encrypted = $2 WHERE did = $1`,
				accCommunity, []byte("a stored refresh token"))
			require.NoError(t, err, "fixture: this AppView hosts the community")
			base := time.Now().UnixMicro()
			const cid = "bafyreioutboxecho"
			uri := f.indexPV2(t, "outboxecho", cid, base)

			moderationRepository := postgres.NewModerationRepository(db)
			var actionID string
			require.NoError(t, moderationRepository.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
				action, err := tx.InsertAction(ctx, moderation.Action{
					ActorDID: "did:plc:accadmin", AuthorityDID: "did:web:coves.test", ScopeKind: moderation.ScopeInstance,
					SubjectURI: uri, SubjectCollection: moderation.PostV2Collection, SubjectCommunityDID: accCommunity,
					ObservedCID: cid, Action: moderation.ActionRemove,
					Reason: "social.coves.moderation.defs#reasonSpam", Origin: moderation.OriginLocal, CreatedAt: echoRemovedAt,
				})
				if err != nil {
					return err
				}
				actionID = action.ID
				return tx.RequireCommunityRecordRemoval(ctx, moderation.CommunityRecordWrite{
					CommunityDID: accCommunity, SubjectURI: uri, SubjectCollection: moderation.PostV2Collection,
					SubjectCID: cid, RemovalActionID: action.ID, Code: posts.DecisionSpam, RemovalCreatedAt: echoRemovedAt,
				})
			}))

			revs := increasingTIDs(t, 2)
			put := removalEvent(accCommunity, uri, cid, string(posts.DecisionSpam), revs[0], base+1_000_000)
			deletion := removalDeleteEvent(accCommunity, uri, revs[1], base+2_000_000)
			repo := &echoingConvergenceRepo{convergenceRepo: &convergenceRepo{
				revs: append([]string(nil), revs...), removals: map[string]posts.StandingRemoval{},
			}}
			if tt.echoDuringWrite {
				repo.afterRemovalWrite = func() {
					assert.NoError(t, f.consumer.HandleEvent(ctx, put), "the echo arrives before the stamp")
				}
			}
			admissions := &restoreStampFailingAdmissions{AdmissionRepository: f.admissions}
			if tt.failRestoreStamp {
				admissions.failRestoreStamps = 1
			}
			clock := &echoClock{}
			queue := moderation.NewCommunityRecordWriteQueue(moderationRepository, repo, admissions,
				moderation.CommunityRecordWriteQueueConfig{Now: clock.Now})
			t.Cleanup(queue.Close)
			subject := moderation.CommunityRecordSubject{CommunityDID: accCommunity, SubjectURI: uri}

			queue.WriteAfterCommit(ctx, subject)
			require.NoError(t, queue.Wait(ctx))
			row, err := f.admissions.Get(ctx, accCommunity, uri)
			require.NoError(t, err)
			require.Equal(t, posts.AdmissionStatusRemoved, row.Status, "fixture: the outbox's removal is stamped")
			if !tt.echoDuringWrite {
				require.NoError(t, f.consumer.HandleEvent(ctx, put), "the removal's echo arrives after the stamp")
			}

			if tt.restore {
				var restoreRequested bool
				require.NoError(t, moderationRepository.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
					var err error
					restoreRequested, err = tx.RequestCommunityRecordRestore(ctx, accCommunity, uri, actionID, cid)
					return err
				}))
				require.True(t, restoreRequested, "fixture: the removal action owns the row")
				queue.WriteAfterCommit(ctx, subject)
				require.NoError(t, queue.Wait(ctx))
				repo.mu.Lock()
				_, removalStands := repo.removals[uri]
				repo.mu.Unlock()
				require.False(t, removalStands, "fixture: the outbox undid its removal")
				if tt.failRestoreStamp {
					var owed string
					require.NoError(t, db.QueryRowContext(ctx, `
						SELECT COALESCE(restore_stamp_rev, '') FROM moderation_community_record_writes
						WHERE community_did = $1 AND subject_uri = $2
					`, accCommunity, uri).Scan(&owed))
					require.Equal(t, revs[1], owed, "fixture: the restore stamp is still owed")
				}

				require.NoError(t, f.consumer.HandleEvent(ctx, deletion), "the removal's delete echo arrives")

				if tt.failRestoreStamp {
					clock.advance(2 * time.Hour)
					require.NoError(t, queue.Sweep(ctx), "the retry applies the owed stamp")
					var state string
					require.NoError(t, db.QueryRowContext(ctx, `
						SELECT state FROM moderation_community_record_writes WHERE community_did = $1 AND subject_uri = $2
					`, accCommunity, uri).Scan(&state))
					require.Equal(t, "completed", state, "fixture: the restore completes")
				}
			}

			assert.Equal(t, []string{"instance:remove"}, subjectActionScopes(t, f, uri),
				"the instance's own removal is the only action; the echoes log no community action")
			row, err = f.admissions.Get(ctx, accCommunity, uri)
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, row.Status)
		})
	}
}

// engineEchoDecider refuses every post as spam.
type engineEchoDecider struct{}

func (engineEchoDecider) DecideAdmission(context.Context, string, string) (posts.AdmissionDecision, error) {
	return posts.AdmissionDecision{Code: posts.DecisionSpam}, nil
}

// engineEchoWriter commits every removal at rev; any other write is
// unexpected.
type engineEchoWriter struct{ rev string }

func (w engineEchoWriter) WriteRemoval(_ context.Context, cmd posts.CommunityRemovalCommand) (posts.CommunityWriteResult, error) {
	rkey := posts.SubjectRkey(cmd.PostURI)
	return posts.CommunityWriteResult{
		URI: "at://" + cmd.CommunityDID + "/" + posts.RemovalCollection + "/" + rkey, RKey: rkey,
		CID: "bafyreiremovalrecord", Rev: w.rev,
	}, nil
}

func (engineEchoWriter) WriteAcceptance(context.Context, posts.CommunityWriteCommand) (posts.CommunityWriteResult, error) {
	return posts.CommunityWriteResult{}, errors.New("unexpected acceptance write")
}

func (engineEchoWriter) RestoreAcceptance(context.Context, posts.CommunityWriteCommand) (posts.CommunityWriteResult, error) {
	return posts.CommunityWriteResult{}, errors.New("unexpected acceptance restore")
}

func (engineEchoWriter) RepinAcceptance(context.Context, posts.CommunityWriteCommand) (posts.CommunityWriteResult, error) {
	return posts.CommunityWriteResult{}, errors.New("unexpected acceptance repin")
}

func (engineEchoWriter) DeleteAcceptance(context.Context, posts.CommunityAcceptanceDeleteCommand) (posts.CommunityWriteResult, error) {
	return posts.CommunityWriteResult{}, errors.New("unexpected acceptance delete")
}

// The acceptance engine removes an accepted post whose edit fails
// re-acceptance and stamps the removal; the removal's echo then arrives and
// logs no community action.
func TestAcceptanceEngine_RemovalEchoAfterStampLogsNoCommunityAction(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	f := newAccFixture(t, db)
	base := time.Now().UnixMicro()
	const originalCID = "bafyreiengineechov1"
	const editedCID = "bafyreiengineechov2"
	uri := f.indexPV2(t, "engineecho", originalCID, base)
	revs := increasingTIDs(t, 2)
	rkey := posts.SubjectRkey(uri)
	accepted, err := f.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID: accCommunity, PostURI: uri,
		AcceptanceURI: "at://" + accCommunity + "/" + posts.AcceptanceCollection + "/" + rkey, AcceptanceRkey: rkey,
		PinnedCID: originalCID, Watermark: posts.CommunityWatermark{Rev: revs[0]},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, accepted.Outcome, "fixture: the community accepted the original")
	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		accAuthor, "update", "engineecho", testkit.TID(), editedCID, base+1_000_000,
		pv2Record(accCommunity, "after the edit", "words the community has not seen"),
	)))
	row, err := f.admissions.Get(ctx, accCommunity, uri)
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionStatusPendingReacceptance, row.Status, "fixture: the edit reopened the decision")

	engine := posts.NewAcceptanceEngine(f.admissions, engineEchoDecider{}, engineEchoWriter{rev: revs[1]}, nil)
	outcome, err := engine.ProcessAdmission(ctx, accCommunity, uri)
	require.NoError(t, err)
	require.Equal(t, posts.EngineRemoved, outcome, "fixture: the failed re-acceptance is a removal")

	require.NoError(t, f.consumer.HandleEvent(ctx, removalEvent(accCommunity, uri, editedCID, string(posts.DecisionSpam), revs[1], base+2_000_000)))

	assert.Empty(t, subjectActionScopes(t, f, uri), "the engine's removal echo logs no community action")
	row, err = f.admissions.Get(ctx, accCommunity, uri)
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionStatusRemoved, row.Status)
}
