//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The holder commits a newer admission while the writer is blocked. Observing
// the backend lock wait proves that the writer actually overlaps that commit.
// The writer may wait on either its pre-read lock or the guarded upsert.
func withdrawalBlockedWriter(t *testing.T, db *sql.DB, hold func(*sql.Tx) error, write func(context.Context) (posts.AdmissionResult, error)) posts.AdmissionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	transaction, holderProcessID := notificationRaceTransaction(t, db, ctx)
	require.NoError(t, hold(transaction))

	type writerResult struct {
		admission posts.AdmissionResult
		err       error
	}
	results := make(chan writerResult, 1)
	go func() {
		admission, err := write(ctx)
		results <- writerResult{admission: admission, err: err}
	}()

	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var processID int
		err := db.QueryRowContext(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database()
				AND pid <> pg_backend_pid()
				AND $1 = ANY(pg_blocking_pids(pid))
				AND wait_event_type = 'Lock'
				AND query ILIKE '%community_post_admissions%'
			LIMIT 1`, holderProcessID).Scan(&processID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("admission writer blocked behind the holder transaction"))
	select {
	case result := <-results:
		t.Fatalf("admission writer returned before the holder committed: %v", result.err)
	default:
	}
	require.NoError(t, transaction.Commit())
	select {
	case result := <-results:
		require.NoError(t, result.err)
		return result.admission
	case <-ctx.Done():
		t.Fatalf("admission writer did not return after holder committed: %v", ctx.Err())
		return posts.AdmissionResult{}
	}
}

func TestAdmissionRepo_CommunityWithdrawal_ConcurrentExistingRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		initialAccepted  bool
		holderAccepted   bool
		acceptanceDelete bool
		markerRev        bool
	}{
		{"accepted becomes pending before removal", true, false, false, false},
		{"pending becomes accepted before removal", false, true, false, true},
		{"pending becomes accepted before acceptance delete", false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 4)
			subject, cid := newWithdrawalSubject(t, db)
			if tc.initialAccepted {
				acceptWithdrawalSubject(t, repo, subject, cid, revs[1])
				seedWithdrawalMarker(t, db, subject.PostURI, "communityWithdrawal", revs[0])
			} else {
				_, err := repo.UpsertPending(context.Background(), posts.UpsertPendingCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: cid,
				})
				require.NoError(t, err)
			}
			acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
			result := withdrawalBlockedWriter(t, db, func(tx *sql.Tx) error {
				if tc.holderAccepted {
					_, err := tx.ExecContext(context.Background(), `UPDATE community_post_admissions SET
						status = 'accepted', acceptance_uri = $3, acceptance_rkey = $4, accepted_cid = $5,
						last_community_rev = $6, last_community_op_rank = $7
						WHERE community_did = $1 AND post_uri = $2`,
						subject.CommunityDID, subject.PostURI, acceptanceURI, acceptanceRkey, cid,
						revs[2], int16(posts.CommunityOpPut))
					return err
				}
				_, err := tx.ExecContext(context.Background(), `UPDATE community_post_admissions SET
					status = 'pending', acceptance_uri = NULL, acceptance_rkey = NULL, accepted_cid = NULL,
					last_community_rev = $3, last_community_op_rank = $4
					WHERE community_did = $1 AND post_uri = $2`,
					subject.CommunityDID, subject.PostURI, revs[2], int16(posts.CommunityOpDelete))
				return err
			}, func(ctx context.Context) (posts.AdmissionResult, error) {
				if tc.acceptanceDelete {
					return repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
						CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
						Watermark: posts.CommunityWatermark{Rev: revs[3], OpRank: posts.CommunityOpDelete},
					})
				}
				return repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, DecisionCode: "rule_violation",
					Watermark: posts.CommunityWatermark{Rev: revs[3], OpRank: posts.CommunityOpPut},
				})
			})
			require.Equal(t, posts.AdmissionApplied, result.Outcome)
			status, rank := posts.AdmissionStatusRemoved, posts.CommunityOpPut
			if tc.acceptanceDelete {
				status, rank = posts.AdmissionStatusPending, posts.CommunityOpDelete
			}
			requireWithdrawalStatus(t, repo, subject, status, revs[3], rank)
			if tc.markerRev {
				requireCommunityWithdrawal(t, db, subject.PostURI, revs[3])
			} else {
				require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			}
		})
	}
}

func TestAdmissionRepo_CommunityWithdrawal_ConcurrentAbsentRow(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	revs := increasingRevs(t, 2)
	subject, cid := newWithdrawalSubject(t, db)
	acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
	result := withdrawalBlockedWriter(t, db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO community_post_admissions
			(community_did, post_uri, status, acceptance_uri, acceptance_rkey, accepted_cid, evaluated_cid,
			 last_community_rev, last_community_op_rank)
			VALUES ($1, $2, 'accepted', $3, $4, $5, $5, $6, $7)`,
			subject.CommunityDID, subject.PostURI, acceptanceURI, acceptanceRkey, cid,
			revs[0], int16(posts.CommunityOpPut))
		return err
	}, func(ctx context.Context) (posts.AdmissionResult, error) {
		return repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
			CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, DecisionCode: "rule_violation",
			Watermark: posts.CommunityWatermark{Rev: revs[1], OpRank: posts.CommunityOpPut},
		})
	})
	require.Equal(t, posts.AdmissionApplied, result.Outcome)
	requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[1], posts.CommunityOpPut)
	require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
}
