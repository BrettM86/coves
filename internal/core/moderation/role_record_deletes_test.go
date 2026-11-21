package moderation_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B2: the role record delete queue's attempt policy, through a fake store and
// a fake guarded deleter on an injected clock.

const (
	roleDeleteCommunityDID = "did:plc:hostedcommunity"
	// roleDeleteRecordKey is communities.RoleRecordKey("did:plc:troublemaker456").
	roleDeleteRecordKey = "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa"
	roleDeleteRecordCID = "bafyreiroleindexed"
)

type fakeGuardedDelete struct {
	collection, rkey, swapRecord string
}

// fakeRoleRecordDeleter is a pds.GuardedDeleter that records every delete it
// receives and answers with the next queued error (nil once they run out).
type fakeRoleRecordDeleter struct {
	mu      sync.Mutex
	deletes []fakeGuardedDelete
	errs    []error
}

func (deleter *fakeRoleRecordDeleter) DeleteRecordWithSwap(_ context.Context, collection, rkey, swapRecord string) error {
	deleter.mu.Lock()
	defer deleter.mu.Unlock()
	deleter.deletes = append(deleter.deletes, fakeGuardedDelete{collection, rkey, swapRecord})
	if len(deleter.errs) == 0 {
		return nil
	}
	err := deleter.errs[0]
	deleter.errs = deleter.errs[1:]
	return err
}

func (deleter *fakeRoleRecordDeleter) received() []fakeGuardedDelete {
	deleter.mu.Lock()
	defer deleter.mu.Unlock()
	return append([]fakeGuardedDelete(nil), deleter.deletes...)
}

// fakeRoleRecordDeleteStore is an in-memory moderation.RoleRecordDeleteStore.
// It claims due rows in ID order, up to claim.Limit, and records each claim's
// limit.
type fakeRoleRecordDeleteStore struct {
	mu     sync.Mutex
	rows   map[int64]moderation.RoleRecordDelete
	limits []int
}

func (store *fakeRoleRecordDeleteStore) ClaimDueRoleRecordDeletes(_ context.Context, claim moderation.RoleRecordDeleteClaim) ([]moderation.RoleRecordDelete, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.limits = append(store.limits, claim.Limit)
	ids := make([]int64, 0, len(store.rows))
	for id := range store.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var due []moderation.RoleRecordDelete
	for _, id := range ids {
		row := store.rows[id]
		if len(due) >= claim.Limit || row.NextAttemptAt.After(claim.DueAt) {
			continue
		}
		row.Claim++
		store.rows[id] = row
		due = append(due, row)
	}
	return due, nil
}

func (store *fakeRoleRecordDeleteStore) claimLimits() []int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]int(nil), store.limits...)
}

func (store *fakeRoleRecordDeleteStore) CompleteRoleRecordDelete(_ context.Context, d moderation.RoleRecordDelete) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	row, ok := store.rows[d.ID]
	if !ok || row.Generation != d.Generation || row.Claim != d.Claim {
		return moderation.ErrRoleRecordDeleteClaimLost
	}
	delete(store.rows, d.ID)
	return nil
}

func (store *fakeRoleRecordDeleteStore) RescheduleRoleRecordDelete(_ context.Context, d moderation.RoleRecordDelete, nextAttemptAt time.Time, failureCode string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	row, ok := store.rows[d.ID]
	if !ok || row.Generation != d.Generation || row.Claim != d.Claim {
		return moderation.ErrRoleRecordDeleteClaimLost
	}
	row.Attempts++
	row.NextAttemptAt = nextAttemptAt
	row.LastFailureCode = failureCode
	store.rows[d.ID] = row
	return nil
}

func (store *fakeRoleRecordDeleteStore) row(id int64) (moderation.RoleRecordDelete, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	row, ok := store.rows[id]
	return row, ok
}

// roleDeleteStep is one Sweep at a clock time, with what the factory and the
// PDS answer, and the queue row the sweep leaves.
type roleDeleteStep struct {
	at         time.Time
	factoryErr error
	deleteErr  error
	// wantDeletes are the guarded deletes the PDS received in this step.
	wantDeletes []fakeGuardedDelete
	// wantRemoved means the row is gone after the step; otherwise it is kept
	// with wantAttempts and wantNextAttemptAt and a non-empty failure code.
	wantRemoved       bool
	wantAttempts      int
	wantNextAttemptAt time.Time
}

func TestRoleRecordDeleteQueue_AttemptPolicy(t *testing.T) {
	sent := []fakeGuardedDelete{{"social.coves.community.moderator", roleDeleteRecordKey, roleDeleteRecordCID}}
	pdsDown := errors.New("pds returned 502")

	tests := []struct {
		name  string
		steps []roleDeleteStep
	}{
		{
			name:  "deleted record completes the row",
			steps: []roleDeleteStep{{at: queueStart, wantDeletes: sent, wantRemoved: true}},
		},
		{
			name:  "record already gone completes the row",
			steps: []roleDeleteStep{{at: queueStart, deleteErr: fmt.Errorf("deleting: %w", pds.ErrNotFound), wantDeletes: sent, wantRemoved: true}},
		},
		{
			name:  "swap conflict completes the row",
			steps: []roleDeleteStep{{at: queueStart, deleteErr: fmt.Errorf("deleting: %w", pds.ErrSwapConflict), wantDeletes: sent, wantRemoved: true}},
		},
		{
			name:  "community no longer hosted completes the row without a delete",
			steps: []roleDeleteStep{{at: queueStart, factoryErr: fmt.Errorf("opening: %w", posts.ErrCommunityNotHosted), wantRemoved: true}},
		},
		{
			name: "credentials failure reschedules the row",
			steps: []roleDeleteStep{{
				at: queueStart, factoryErr: errors.New("refreshing community session failed"),
				wantAttempts: 1, wantNextAttemptAt: time.Date(2026, time.November, 11, 12, 1, 0, 0, time.UTC),
			}},
		},
		{
			name: "PDS failures back off and a later healthy sweep completes",
			steps: []roleDeleteStep{
				{
					at: queueStart, deleteErr: pdsDown, wantDeletes: sent,
					wantAttempts: 1, wantNextAttemptAt: time.Date(2026, time.November, 11, 12, 1, 0, 0, time.UTC),
				},
				{
					at: time.Date(2026, time.November, 11, 12, 1, 0, 0, time.UTC), deleteErr: pdsDown, wantDeletes: sent,
					wantAttempts: 2, wantNextAttemptAt: time.Date(2026, time.November, 11, 12, 3, 0, 0, time.UTC),
				},
				{
					at: time.Date(2026, time.November, 11, 12, 3, 30, 0, time.UTC), wantDeletes: sent, wantRemoved: true,
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clock := &queueClock{now: queueStart}
			store := &fakeRoleRecordDeleteStore{rows: map[int64]moderation.RoleRecordDelete{
				1: {
					ID: 1, CommunityDID: roleDeleteCommunityDID, RecordKey: roleDeleteRecordKey, RecordCID: roleDeleteRecordCID,
					NextAttemptAt: queueStart, Generation: 1,
				},
			}}

			for i, step := range tc.steps {
				clock.Set(step.at)
				deleter := &fakeRoleRecordDeleter{}
				if step.deleteErr != nil {
					deleter.errs = []error{step.deleteErr}
				}
				var opened []string
				factory := func(_ context.Context, communityDID string) (pds.GuardedDeleter, error) {
					opened = append(opened, communityDID)
					if step.factoryErr != nil {
						return nil, step.factoryErr
					}
					return deleter, nil
				}
				queue := moderation.NewRoleRecordDeleteQueue(store, factory, moderation.RoleRecordDeleteQueueConfig{Now: clock.Now})

				require.NoError(t, queue.Sweep(ctx), "step %d sweep", i)

				assert.Equal(t, step.wantDeletes, deleter.received(), "step %d: guarded deletes the PDS received", i)
				row, kept := store.row(1)
				if step.wantRemoved {
					require.NotEmpty(t, opened, "step %d: the sweep opened the community's deleter", i)
					assert.False(t, kept, "step %d: the finished row is removed", i)
					continue
				}
				require.True(t, kept, "step %d: the failed row is kept for a retry", i)
				assert.Equal(t, step.wantAttempts, row.Attempts, "step %d attempts", i)
				assert.True(t, step.wantNextAttemptAt.Equal(row.NextAttemptAt), "step %d next attempt: want %s, got %s", i, step.wantNextAttemptAt, row.NextAttemptAt)
				assert.NotEmpty(t, row.LastFailureCode, "step %d failure code", i)
			}
		})
	}
}

// cancellingRoleRecordDeleter cancels the sweep's context on its first delete.
type cancellingRoleRecordDeleter struct {
	fakeRoleRecordDeleter
	cancel context.CancelFunc
}

func (deleter *cancellingRoleRecordDeleter) DeleteRecordWithSwap(ctx context.Context, collection, rkey, swapRecord string) error {
	deleter.cancel()
	return deleter.fakeRoleRecordDeleter.DeleteRecordWithSwap(ctx, collection, rkey, swapRecord)
}

func dueRoleRecordDeletes(ids ...int64) map[int64]moderation.RoleRecordDelete {
	rows := make(map[int64]moderation.RoleRecordDelete, len(ids))
	for _, id := range ids {
		rows[id] = moderation.RoleRecordDelete{
			ID: id, CommunityDID: roleDeleteCommunityDID, RecordKey: fmt.Sprintf("%s%d", roleDeleteRecordKey, id),
			RecordCID: roleDeleteRecordCID, NextAttemptAt: queueStart, Generation: 1,
		}
	}
	return rows
}

// Each row is claimed just before its attempt, so every attempt starts on a
// full lease instead of sharing one lease across a batch.
func TestRoleRecordDeleteQueue_SweepClaimsOneRowPerAttempt(t *testing.T) {
	store := &fakeRoleRecordDeleteStore{rows: dueRoleRecordDeletes(1, 2, 3)}
	deleter := &fakeRoleRecordDeleter{}
	factory := func(context.Context, string) (pds.GuardedDeleter, error) { return deleter, nil }
	queue := moderation.NewRoleRecordDeleteQueue(store, factory, moderation.RoleRecordDeleteQueueConfig{Now: (&queueClock{now: queueStart}).Now})

	require.NoError(t, queue.Sweep(context.Background()))

	assert.Len(t, deleter.received(), 3, "every due row is attempted")
	limits := store.claimLimits()
	require.NotEmpty(t, limits)
	for i, limit := range limits {
		assert.Equal(t, 1, limit, "claim %d leases one row", i)
	}
}

// Once the sweep's context is done, no further row is attempted.
func TestRoleRecordDeleteQueue_SweepStopsAttemptingOnceItsContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeRoleRecordDeleteStore{rows: dueRoleRecordDeletes(1, 2, 3)}
	deleter := &cancellingRoleRecordDeleter{cancel: cancel}
	factory := func(context.Context, string) (pds.GuardedDeleter, error) { return deleter, nil }
	queue := moderation.NewRoleRecordDeleteQueue(store, factory, moderation.RoleRecordDeleteQueueConfig{Now: (&queueClock{now: queueStart}).Now})

	err := queue.Sweep(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, deleter.received(), 1, "the delete in flight finishes and no other row is attempted")
	_, kept := store.row(1)
	assert.False(t, kept, "the attempt in flight still writes its outcome")
	for _, id := range []int64{2, 3} {
		row, kept := store.row(id)
		require.True(t, kept, "row %d stays pending", id)
		assert.Zero(t, row.Attempts, "row %d counts no attempt", id)
	}
}

// An attempt timeout that, with the outcome write, reaches the lease could let
// another worker claim the row mid-attempt.
func TestNewRoleRecordDeleteQueue_RejectsAnAttemptTimeoutThatReachesTheLease(t *testing.T) {
	factory := func(context.Context, string) (pds.GuardedDeleter, error) { return &fakeRoleRecordDeleter{}, nil }
	assert.Panics(t, func() {
		moderation.NewRoleRecordDeleteQueue(&fakeRoleRecordDeleteStore{}, factory, moderation.RoleRecordDeleteQueueConfig{AttemptTimeout: 115 * time.Second})
	})
	assert.NotPanics(t, func() {
		moderation.NewRoleRecordDeleteQueue(&fakeRoleRecordDeleteStore{}, factory, moderation.RoleRecordDeleteQueueConfig{AttemptTimeout: 114 * time.Second})
	})
}
