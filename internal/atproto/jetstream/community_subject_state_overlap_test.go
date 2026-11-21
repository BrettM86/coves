//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B6: the local owner-record index call and the firehose event for the same
// record at the same rev run at once. A holder transaction keeps the record's
// rev gate row locked until both are waiting on it, then lets them race. Both
// finish without a deadlock or error, the role is indexed once, and the
// roster version of a fresh community advances exactly once.
func TestCommunitySubjectState_LocalRoleIndexOverlapsFirehoseAtSameRev(t *testing.T) {
	t.Parallel()

	const (
		gateURI   = "at://did:plc:rostercommunitya/social.coves.community.moderator/lk4ulvb25z3owhmahunggqgfc4aiv7a3cbmrrcuwlfdva7x5uiwa"
		recordCID = "bafyreicsoverlapowner"
		rounds    = 3
	)

	tests := []struct {
		name string
		// hold starts the holder transaction, which locks the gate row for
		// gateURI; olderRev is a rev before the record's.
		hold func(t *testing.T, ctx context.Context, db *sql.DB, olderRev string) *sql.Tx
	}{
		{
			name: "no gate row yet: both wait on an uncommitted gate insert",
			hold: func(t *testing.T, ctx context.Context, db *sql.DB, olderRev string) *sql.Tx {
				holder, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				_, err = holder.ExecContext(ctx,
					`INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)`, gateURI, olderRev)
				require.NoError(t, err)
				return holder
			},
		},
		{
			name: "a gate row at an older rev: both wait on its row lock",
			hold: func(t *testing.T, ctx context.Context, db *sql.DB, olderRev string) *sql.Tx {
				_, err := db.ExecContext(ctx,
					`INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)`, gateURI, olderRev)
				require.NoError(t, err)
				holder, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				var locked string
				require.NoError(t, holder.QueryRowContext(ctx,
					`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1 FOR UPDATE`, gateURI).Scan(&locked))
				return holder
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for round := 0; round < rounds; round++ {
				t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
					db := testkit.DB(t)
					// Holder, local call, firehose claim and firehose apply each
					// hold a connection at once.
					db.SetMaxOpenConns(6)
					consumer, roles := newRosterConsumer(t, db)
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					revs := increasingTIDs(t, 2)

					holder := tt.hold(t, ctx, db, revs[0])
					defer func() { _ = holder.Rollback() }()
					var holderPID int
					require.NoError(t, holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID))

					localDone := make(chan error, 1)
					firehoseDone := make(chan error, 1)
					go func() {
						_, err := roles.UpsertCommunityRoleAtRev(ctx, communities.CommunityRole{
							CommunityDID: rosterCommunityA, SubjectDID: rosterSubject2, Role: communities.RoleOwner,
							RecordKey: rosterSubject2Key, RecordCID: recordCID, RecordCreatedAt: rosterT1,
						}, revs[1])
						localDone <- err
					}()
					go func() {
						firehoseDone <- consumer.HandleEvent(ctx, rosterCommitEvent(rosterStep{
							repo: rosterCommunityA, op: "create", subject: rosterSubject2, rkey: rosterSubject2Key,
							role: "owner", createdAt: rosterTime1, rev: revs[1], cid: recordCID,
						}, time.Now().UnixMicro()))
					}()

					testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
						// pg_stat_activity's snapshot persists inside the holder's
						// transaction otherwise.
						if _, err := holder.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
							return false, err
						}
						// On a held row the second waiter queues behind the first
						// waiter's tuple lock, so it is not counted by blocker.
						var waiting int
						err := holder.QueryRowContext(ctx, `
							SELECT count(*) FROM pg_stat_activity
							WHERE datname = current_database() AND pid <> $1
							  AND wait_event_type = 'Lock'
							  AND query ILIKE '%jetstream_record_revs%'`, holderPID).Scan(&waiting)
						return waiting == 2, err
					}, testkit.WithDescription("the local call and the firehose event both wait on the held gate row"))

					require.NoError(t, holder.Rollback())
					overlapResult(t, ctx, localDone, "the local role index call")
					overlapResult(t, ctx, firehoseDone, "the firehose role event")

					indexed, err := roles.ListCommunityRoles(ctx, rosterCommunityA)
					require.NoError(t, err)
					require.Len(t, indexed, 1)
					assert.Equal(t, recordCID, indexed[0].RecordCID, "the indexed owner record")
					assert.Equal(t, "did:plc:rostersubject2", indexed[0].SubjectDID)

					var gateRev string
					require.NoError(t, db.QueryRowContext(ctx,
						`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, gateURI).Scan(&gateRev))
					assert.Equal(t, revs[1], gateRev, "the owner record's rev gate")

					state := readSubjectState(t, db, rosterCommunityA, rosterCommunityA, "roster")
					assert.EqualValues(t, 1, state.Version, "the roster version advances once")
				})
			}
		})
	}
}

// overlapResult waits for one overlapping caller and fails on a deadlock, an
// error or the context deadline.
func overlapResult(t *testing.T, ctx context.Context, done <-chan error, caller string) {
	t.Helper()
	select {
	case err := <-done:
		var databaseError *pq.Error
		require.False(t, errors.As(err, &databaseError) && databaseError.Code == "40P01", "%s deadlocked: %v", caller, err)
		require.NoError(t, err, "%s failed", caller)
	case <-ctx.Done():
		require.FailNowf(t, caller+" did not finish", "context deadline: %v", ctx.Err())
	}
}
