package accountstatus_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	recheckDID       = "did:plc:recheckaccountaaaaaaaaa"
	recheckOtherDID  = "did:plc:recheckaccountbbbbbbbbb"
	recheckThirdDID  = "did:plc:recheckaccountccccccccc"
	recheckFourthDID = "did:plc:recheckaccountddddddddd"
)

// reconcilerNow is the fixed clock every Reconciler test starts from.
var reconcilerNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func at(hour, minute int) time.Time {
	return time.Date(2026, 11, 5, hour, minute, 0, 0, time.UTC)
}

var (
	answerTakendown = accountstatus.RepoStatus{Active: false, Status: "takendown"}
	answerSuspended = accountstatus.RepoStatus{Active: false, Status: "suspended"}
	answerActive    = accountstatus.RepoStatus{Active: true}
)

// fakeStore is an in-memory accountstatus.Store following the contract:
// Reconcile reads both rows under one lock, calls decide, and applies the
// restriction change and the recheck change together, or nothing when
// decide fails.
type fakeStore struct {
	mutex        sync.Mutex
	restrictions map[string]accountstatus.Status
	rechecks     map[string]accountstatus.PendingRecheck
	// present holds the DIDs with Coves presence (a users or communities
	// row, or an authored post or comment); Reconcile reports it as
	// State.Present.
	present map[string]bool

	// afterDueRechecks, when set, runs after DueRechecks has chosen its DIDs
	// and before it returns, standing in for a write that lands between the
	// listing and the per-DID Reconcile.
	afterDueRechecks func(s *fakeStore)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		restrictions: make(map[string]accountstatus.Status),
		rechecks:     make(map[string]accountstatus.PendingRecheck),
		present:      make(map[string]bool),
	}
}

func (s *fakeStore) Restriction(_ context.Context, did string) (accountstatus.Status, bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	status, found := s.restrictions[did]
	return status, found, nil
}

func (s *fakeStore) Reconcile(ctx context.Context, did string, decide func(ctx context.Context, current accountstatus.State) (accountstatus.Change, error)) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	status, found := s.restrictions[did]
	current := accountstatus.State{Status: status, Found: found, Present: s.present[did]}
	if pending, ok := s.rechecks[did]; ok {
		copied := pending
		current.Recheck = &copied
	}
	change, err := decide(ctx, current)
	if err != nil {
		return err
	}

	switch change.Kind {
	case accountstatus.ChangeNone:
	case accountstatus.ChangeSet:
		if change.Status != accountstatus.Takendown && change.Status != accountstatus.Suspended {
			return fmt.Errorf("fake store: unknown status %q", change.Status)
		}
	case accountstatus.ChangeClear:
	default:
		return fmt.Errorf("fake store: unknown change kind %d", change.Kind)
	}
	switch change.Recheck.Action {
	case accountstatus.RecheckKeep, accountstatus.RecheckClear:
	case accountstatus.RecheckSchedule:
		if change.Recheck.Attempts < 1 {
			return fmt.Errorf("fake store: recheck attempts %d < 1", change.Recheck.Attempts)
		}
	default:
		return fmt.Errorf("fake store: unknown recheck action %d", change.Recheck.Action)
	}

	switch change.Kind {
	case accountstatus.ChangeSet:
		s.restrictions[did] = change.Status
	case accountstatus.ChangeClear:
		delete(s.restrictions, did)
	}
	switch change.Recheck.Action {
	case accountstatus.RecheckSchedule:
		s.rechecks[did] = accountstatus.PendingRecheck{
			Attempts:      change.Recheck.Attempts,
			NextAttemptAt: change.Recheck.NextAttemptAt,
			LastErrorCode: change.Recheck.LastErrorCode,
		}
	case accountstatus.RecheckClear:
		delete(s.rechecks, did)
	}
	return nil
}

func (s *fakeStore) DueRechecks(_ context.Context, now time.Time, limit int) ([]string, error) {
	s.mutex.Lock()
	var due []string
	for did, pending := range s.rechecks {
		if !pending.NextAttemptAt.After(now) {
			due = append(due, did)
		}
	}
	// Fewest attempts first, then oldest next attempt, then DID, matching the
	// accountstatus.Store contract.
	sort.Slice(due, func(i, j int) bool {
		left, right := s.rechecks[due[i]], s.rechecks[due[j]]
		if left.Attempts != right.Attempts {
			return left.Attempts < right.Attempts
		}
		if !left.NextAttemptAt.Equal(right.NextAttemptAt) {
			return left.NextAttemptAt.Before(right.NextAttemptAt)
		}
		return due[i] < due[j]
	})
	if len(due) > limit {
		due = due[:limit]
	}
	s.mutex.Unlock()

	if s.afterDueRechecks != nil {
		s.afterDueRechecks(s)
	}
	return due, nil
}

// scriptedConfirmer answers each DID with its scripted function and records
// which DIDs it was asked about. A DID with no script fails the test.
type scriptedConfirmer struct {
	t       *testing.T
	answers map[string]func(ctx context.Context) (accountstatus.RepoStatus, error)
	asked   []string
}

func newScriptedConfirmer(t *testing.T) *scriptedConfirmer {
	return &scriptedConfirmer{t: t, answers: make(map[string]func(ctx context.Context) (accountstatus.RepoStatus, error))}
}

func (c *scriptedConfirmer) answer(did string, status accountstatus.RepoStatus, err error) {
	c.answers[did] = func(context.Context) (accountstatus.RepoStatus, error) { return status, err }
}

func (c *scriptedConfirmer) RepoStatus(ctx context.Context, did string) (accountstatus.RepoStatus, error) {
	c.asked = append(c.asked, did)
	script, ok := c.answers[did]
	if !ok {
		c.t.Errorf("the PDS of %s was asked but has no scripted answer", did)
		return accountstatus.RepoStatus{}, errors.New("no scripted answer")
	}
	return script(ctx)
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func pending(attempts int, next time.Time, code accountstatus.ConfirmationFailure) *accountstatus.PendingRecheck {
	return &accountstatus.PendingRecheck{Attempts: attempts, NextAttemptAt: next, LastErrorCode: code}
}

func statusPointer(status accountstatus.Status) *accountstatus.Status { return &status }

func seed(store *fakeStore, did string, restriction *accountstatus.Status, recheck *accountstatus.PendingRecheck) {
	if restriction != nil {
		store.restrictions[did] = *restriction
	}
	if recheck != nil {
		store.rechecks[did] = *recheck
	}
}

// markPresent gives each DID Coves presence in store.
func markPresent(store *fakeStore, dids ...string) {
	for _, did := range dids {
		store.present[did] = true
	}
}

func requireStored(t *testing.T, store *fakeStore, did string, wantRestriction *accountstatus.Status, wantRecheck *accountstatus.PendingRecheck) {
	t.Helper()
	store.mutex.Lock()
	defer store.mutex.Unlock()
	status, found := store.restrictions[did]
	if wantRestriction == nil {
		assert.False(t, found, "%s: expected no restriction, found %q", did, status)
	} else if assert.True(t, found, "%s: expected restriction %q, found none", did, *wantRestriction) {
		assert.Equal(t, *wantRestriction, status, "%s: restriction", did)
	}
	recheck, found := store.rechecks[did]
	if wantRecheck == nil {
		assert.False(t, found, "%s: expected no recheck row, found %+v", did, recheck)
	} else if assert.True(t, found, "%s: expected recheck row %+v, found none", did, *wantRecheck) {
		assert.Equal(t, *wantRecheck, recheck, "%s: recheck row", did)
	}
}

// TestRecheckDelay pins the recheck backoff: one minute doubling per attempt,
// capped at one hour, with attempts below one treated as one.
func TestRecheckDelay(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: -3, want: time.Minute},
		{attempts: 0, want: time.Minute},
		{attempts: 1, want: time.Minute},
		{attempts: 2, want: 2 * time.Minute},
		{attempts: 3, want: 4 * time.Minute},
		{attempts: 4, want: 8 * time.Minute},
		{attempts: 5, want: 16 * time.Minute},
		{attempts: 6, want: 32 * time.Minute},
		{attempts: 7, want: time.Hour},
		{attempts: 8, want: time.Hour},
		{attempts: 64, want: time.Hour},
		{attempts: 1 << 30, want: time.Hour},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempts %d", tt.attempts), func(t *testing.T) {
			assert.Equal(t, tt.want, accountstatus.RecheckDelay(tt.attempts))
		})
	}
}

// TestReconcilerHandleEvent_ConfirmationFailureSchedulesRecheck pins that a
// failed confirmation keeps the current restriction, returns nil, and
// schedules the first recheck one minute out with the failure's code.
func TestReconcilerHandleEvent_ConfirmationFailureSchedulesRecheck(t *testing.T) {
	tests := []struct {
		name            string
		restriction     *accountstatus.Status
		present         bool
		active          bool
		confirmError    error
		wantRestriction *accountstatus.Status
		wantRecheck     *accountstatus.PendingRecheck
	}{
		{
			name:         "takendown event with no restriction, PDS error status",
			present:      true,
			confirmError: &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSErrorStatus, Err: errors.New("getRepoStatus answered status 503")},
			wantRecheck:  pending(1, at(12, 1), accountstatus.FailurePDSErrorStatus),
		},
		{
			name:            "active event over a stored takedown, identity unresolved",
			restriction:     statusPointer(accountstatus.Takendown),
			active:          true,
			confirmError:    &accountstatus.ConfirmationError{Failure: accountstatus.FailureIdentityUnresolved, Err: errors.New("plc directory unreachable")},
			wantRestriction: statusPointer(accountstatus.Takendown),
			wantRecheck:     pending(1, at(12, 1), accountstatus.FailureIdentityUnresolved),
		},
		{
			name:            "active event over a stored suspension, wrapped invalid response",
			restriction:     statusPointer(accountstatus.Suspended),
			active:          true,
			confirmError:    fmt.Errorf("confirming: %w", &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSInvalidResponse, Err: errors.New("answered for a different DID")}),
			wantRestriction: statusPointer(accountstatus.Suspended),
			wantRecheck:     pending(1, at(12, 1), accountstatus.FailurePDSInvalidResponse),
		},
		{
			name:         "takendown event with no restriction, error without a code",
			present:      true,
			confirmError: errors.New("confirmer exploded"),
			wantRecheck:  pending(1, at(12, 1), accountstatus.FailureUnclassified),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			seed(store, recheckDID, tt.restriction, nil)
			if tt.present {
				markPresent(store, recheckDID)
			}
			confirmer := newScriptedConfirmer(t)
			confirmer.answer(recheckDID, accountstatus.RepoStatus{}, tt.confirmError)
			clock := &testClock{now: reconcilerNow}
			reconciler := accountstatus.NewReconciler(store, confirmer, clock.Now)

			err := reconciler.HandleEvent(context.Background(), recheckDID, tt.active)
			require.NoError(t, err)
			requireStored(t, store, recheckDID, tt.wantRestriction, tt.wantRecheck)
		})
	}
}

// TestReconcilerHandleEvent_SupersedesPendingRecheck pins that a newer event
// supersedes a pending recheck: a confirmed answer clears it, RepoNotFound
// clears it when no restriction is stored, and a newer failure (RepoNotFound
// over a stored restriction included) restarts the backoff at attempt 1.
func TestReconcilerHandleEvent_SupersedesPendingRecheck(t *testing.T) {
	stale := pending(4, at(12, 8), accountstatus.FailurePDSUnreachable)
	tests := []struct {
		name            string
		restriction     *accountstatus.Status
		present         bool
		recheck         *accountstatus.PendingRecheck
		active          bool
		pdsAnswer       accountstatus.RepoStatus
		pdsError        error
		wantRestriction *accountstatus.Status
		wantRecheck     *accountstatus.PendingRecheck
		wantNoPDSCall   bool
	}{
		{
			name:            "takendown event confirmed takendown sets the restriction and clears the recheck",
			present:         true,
			recheck:         stale,
			pdsAnswer:       answerTakendown,
			wantRestriction: statusPointer(accountstatus.Takendown),
		},
		{
			name:            "suspended event confirmed suspended sets the restriction and clears the recheck",
			present:         true,
			recheck:         stale,
			pdsAnswer:       answerSuspended,
			wantRestriction: statusPointer(accountstatus.Suspended),
		},
		{
			name:        "active event over a takedown confirmed active lifts it and clears the recheck",
			restriction: statusPointer(accountstatus.Takendown),
			recheck:     stale,
			active:      true,
			pdsAnswer:   answerActive,
		},
		{
			name:      "takendown event while the PDS reports active clears the recheck only",
			present:   true,
			recheck:   stale,
			pdsAnswer: answerActive,
		},
		{
			name:            "active event over a takedown, repo not found keeps the restriction and restarts the backoff",
			restriction:     statusPointer(accountstatus.Takendown),
			recheck:         stale,
			active:          true,
			pdsError:        fmt.Errorf("getRepoStatus: %w", accountstatus.ErrRepoNotFound),
			wantRestriction: statusPointer(accountstatus.Takendown),
			wantRecheck:     pending(1, at(12, 1), accountstatus.FailureRepoNotFound),
		},
		{
			name:            "takendown event over a takedown, repo not found keeps the restriction and restarts the backoff",
			restriction:     statusPointer(accountstatus.Takendown),
			recheck:         stale,
			pdsError:        fmt.Errorf("getRepoStatus: %w", accountstatus.ErrRepoNotFound),
			wantRestriction: statusPointer(accountstatus.Takendown),
			wantRecheck:     pending(1, at(12, 1), accountstatus.FailureRepoNotFound),
		},
		{
			// Guard: with no restriction stored there is nothing to keep, so
			// RepoNotFound clears the recheck and stores nothing.
			name:     "takendown event for a present account with no restriction, repo not found clears the recheck",
			present:  true,
			recheck:  stale,
			pdsError: fmt.Errorf("getRepoStatus: %w", accountstatus.ErrRepoNotFound),
		},
		{
			name:        "a newer failure restarts the backoff at attempt 1",
			present:     true,
			recheck:     stale,
			pdsError:    &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSErrorStatus, Err: errors.New("status 502")},
			wantRecheck: pending(1, at(12, 1), accountstatus.FailurePDSErrorStatus),
		},
		{
			// Guard: no restriction to lift, so no PDS is asked and the pending
			// recheck stays for the worker.
			name:          "active event with no restriction keeps the pending recheck and asks no PDS",
			present:       true,
			recheck:       stale,
			active:        true,
			pdsAnswer:     answerTakendown,
			wantRecheck:   stale,
			wantNoPDSCall: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			seed(store, recheckDID, tt.restriction, tt.recheck)
			if tt.present {
				markPresent(store, recheckDID)
			}
			confirmer := newScriptedConfirmer(t)
			confirmer.answer(recheckDID, tt.pdsAnswer, tt.pdsError)
			clock := &testClock{now: reconcilerNow}
			reconciler := accountstatus.NewReconciler(store, confirmer, clock.Now)

			err := reconciler.HandleEvent(context.Background(), recheckDID, tt.active)
			require.NoError(t, err)
			requireStored(t, store, recheckDID, tt.wantRestriction, tt.wantRecheck)
			if tt.wantNoPDSCall {
				assert.Empty(t, confirmer.asked, "no PDS may be asked")
			}
		})
	}
}

// TestReconcilerHandleEvent_ContextEnd pins that a cancelled or expired
// parent context during confirmation returns an error and changes nothing,
// while a confirmer timeout under a live parent is an ordinary failure that
// schedules a recheck.
func TestReconcilerHandleEvent_ContextEnd(t *testing.T) {
	existing := pending(2, at(11, 58), accountstatus.FailurePDSUnreachable)

	t.Run("parent cancelled during confirmation changes nothing", func(t *testing.T) {
		store := newFakeStore()
		seed(store, recheckDID, statusPointer(accountstatus.Takendown), existing)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		confirmer := newScriptedConfirmer(t)
		confirmer.answers[recheckDID] = func(ctx context.Context) (accountstatus.RepoStatus, error) {
			cancel()
			return accountstatus.RepoStatus{}, &accountstatus.ConfirmationError{
				Failure: accountstatus.FailurePDSUnreachable,
				Err:     fmt.Errorf("asking the PDS: %w", ctx.Err()),
			}
		}
		reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

		err := reconciler.HandleEvent(ctx, recheckDID, true)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		requireStored(t, store, recheckDID, statusPointer(accountstatus.Takendown), existing)
	})

	t.Run("parent deadline passed changes nothing", func(t *testing.T) {
		store := newFakeStore()
		seed(store, recheckDID, nil, existing)
		markPresent(store, recheckDID)
		ctx, cancel := context.WithDeadline(context.Background(), at(11, 0))
		defer cancel()
		confirmer := newScriptedConfirmer(t)
		confirmer.answers[recheckDID] = func(ctx context.Context) (accountstatus.RepoStatus, error) {
			return accountstatus.RepoStatus{}, &accountstatus.ConfirmationError{
				Failure: accountstatus.FailurePDSUnreachable,
				Err:     fmt.Errorf("asking the PDS: %w", ctx.Err()),
			}
		}
		reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

		err := reconciler.HandleEvent(ctx, recheckDID, false)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		requireStored(t, store, recheckDID, nil, existing)
	})

	t.Run("confirmer timeout under a live parent schedules a recheck", func(t *testing.T) {
		store := newFakeStore()
		seed(store, recheckDID, nil, nil)
		markPresent(store, recheckDID)
		confirmer := newScriptedConfirmer(t)
		confirmer.answer(recheckDID, accountstatus.RepoStatus{}, &accountstatus.ConfirmationError{
			Failure: accountstatus.FailurePDSUnreachable,
			Err:     fmt.Errorf("asking the PDS: %w", context.DeadlineExceeded),
		})
		reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

		err := reconciler.HandleEvent(context.Background(), recheckDID, false)
		require.NoError(t, err)
		requireStored(t, store, recheckDID, nil, pending(1, at(12, 1), accountstatus.FailurePDSUnreachable))
	})
}

// TestReconcilerRecheckDue_SingleAccount pins one due recheck's outcomes: a
// confirmed answer is applied and clears the row, a failure (RepoNotFound
// over a stored restriction included) advances the backoff, and a row
// deleted or rescheduled after listing is left alone without asking any PDS.
// Rows with a stored restriction and no presence guard that a stored
// restriction alone counts as presence.
func TestReconcilerRecheckDue_SingleAccount(t *testing.T) {
	tests := []struct {
		name             string
		restriction      *accountstatus.Status
		present          bool
		recheck          *accountstatus.PendingRecheck
		afterDueRechecks func(s *fakeStore)
		pdsAnswer        accountstatus.RepoStatus
		pdsError         error
		wantRestriction  *accountstatus.Status
		wantRecheck      *accountstatus.PendingRecheck
		wantReport       accountstatus.RecheckReport
		wantNoPDSCall    bool
	}{
		{
			name:            "due recheck confirmed takendown sets the restriction and clears the row",
			present:         true,
			recheck:         pending(3, at(11, 59), accountstatus.FailurePDSUnreachable),
			pdsAnswer:       answerTakendown,
			wantRestriction: statusPointer(accountstatus.Takendown),
			wantReport:      accountstatus.RecheckReport{Due: 1, Confirmed: 1},
		},
		{
			name:            "recheck due exactly now is processed",
			present:         true,
			recheck:         pending(1, at(12, 0), accountstatus.FailurePDSErrorStatus),
			pdsAnswer:       answerSuspended,
			wantRestriction: statusPointer(accountstatus.Suspended),
			wantReport:      accountstatus.RecheckReport{Due: 1, Confirmed: 1},
		},
		{
			name:        "due recheck over a takedown confirmed active lifts it and clears the row",
			restriction: statusPointer(accountstatus.Takendown),
			recheck:     pending(2, at(11, 30), accountstatus.FailureIdentityUnresolved),
			pdsAnswer:   answerActive,
			wantReport:  accountstatus.RecheckReport{Due: 1, Confirmed: 1},
		},
		{
			name:            "due recheck over a takedown answered deactivated keeps it and clears the row",
			restriction:     statusPointer(accountstatus.Takendown),
			recheck:         pending(2, at(11, 30), accountstatus.FailurePDSErrorStatus),
			pdsAnswer:       accountstatus.RepoStatus{Active: false, Status: "deactivated"},
			wantRestriction: statusPointer(accountstatus.Takendown),
			wantReport:      accountstatus.RecheckReport{Due: 1, Confirmed: 1},
		},
		{
			name:            "due recheck over a takedown still takendown keeps it and clears the row",
			restriction:     statusPointer(accountstatus.Takendown),
			recheck:         pending(2, at(11, 30), accountstatus.FailureIdentityUnresolved),
			pdsAnswer:       answerTakendown,
			wantRestriction: statusPointer(accountstatus.Takendown),
			wantReport:      accountstatus.RecheckReport{Due: 1, Confirmed: 1},
		},
		{
			name:        "failure advances attempts and backoff",
			present:     true,
			recheck:     pending(3, at(11, 59), accountstatus.FailurePDSUnreachable),
			pdsError:    &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSErrorStatus, Err: errors.New("status 503")},
			wantRecheck: pending(4, at(12, 8), accountstatus.FailurePDSErrorStatus),
			wantReport:  accountstatus.RecheckReport{Due: 1, Failed: 1},
		},
		{
			name:            "failure over a takedown keeps it and advances the backoff",
			restriction:     statusPointer(accountstatus.Takendown),
			recheck:         pending(1, at(11, 59), accountstatus.FailurePDSErrorStatus),
			pdsError:        errors.New("confirmer exploded"),
			wantRestriction: statusPointer(accountstatus.Takendown),
			wantRecheck:     pending(2, at(12, 2), accountstatus.FailureUnclassified),
			wantReport:      accountstatus.RecheckReport{Due: 1, Failed: 1},
		},
		{
			name:        "failure at attempts 7 caps the backoff at one hour",
			present:     true,
			recheck:     pending(7, at(11, 0), accountstatus.FailurePDSUnreachable),
			pdsError:    &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSUnreachable, Err: errors.New("dial tcp: connection refused")},
			wantRecheck: pending(8, at(13, 0), accountstatus.FailurePDSUnreachable),
			wantReport:  accountstatus.RecheckReport{Due: 1, Failed: 1},
		},
		{
			name:            "repo not found over a takedown keeps it and advances the backoff",
			restriction:     statusPointer(accountstatus.Takendown),
			recheck:         pending(2, at(11, 50), accountstatus.FailurePDSErrorStatus),
			pdsError:        fmt.Errorf("getRepoStatus: %w", accountstatus.ErrRepoNotFound),
			wantRestriction: statusPointer(accountstatus.Takendown),
			wantRecheck:     pending(3, at(12, 4), accountstatus.FailureRepoNotFound),
			wantReport:      accountstatus.RecheckReport{Due: 1, Failed: 1},
		},
		{
			name:        "row rescheduled after listing is left alone",
			present:     true,
			recheck:     pending(3, at(11, 59), accountstatus.FailurePDSUnreachable),
			pdsAnswer:   answerTakendown,
			wantRecheck: pending(1, at(12, 5), accountstatus.FailurePDSErrorStatus),
			afterDueRechecks: func(s *fakeStore) {
				s.mutex.Lock()
				defer s.mutex.Unlock()
				s.rechecks[recheckDID] = *pending(1, at(12, 5), accountstatus.FailurePDSErrorStatus)
			},
			wantReport:    accountstatus.RecheckReport{Due: 1},
			wantNoPDSCall: true,
		},
		{
			name:            "row deleted after listing is left alone",
			restriction:     statusPointer(accountstatus.Suspended),
			recheck:         pending(3, at(11, 59), accountstatus.FailurePDSUnreachable),
			pdsAnswer:       answerActive,
			wantRestriction: statusPointer(accountstatus.Suspended),
			afterDueRechecks: func(s *fakeStore) {
				s.mutex.Lock()
				defer s.mutex.Unlock()
				delete(s.rechecks, recheckDID)
			},
			wantReport:    accountstatus.RecheckReport{Due: 1},
			wantNoPDSCall: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			seed(store, recheckDID, tt.restriction, tt.recheck)
			if tt.present {
				markPresent(store, recheckDID)
			}
			store.afterDueRechecks = tt.afterDueRechecks
			confirmer := newScriptedConfirmer(t)
			confirmer.answer(recheckDID, tt.pdsAnswer, tt.pdsError)
			reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

			report, err := reconciler.RecheckDue(context.Background(), 50)
			require.NoError(t, err)
			assert.Equal(t, tt.wantReport, report)
			requireStored(t, store, recheckDID, tt.wantRestriction, tt.wantRecheck)
			if tt.wantNoPDSCall {
				assert.Empty(t, confirmer.asked, "no PDS may be asked")
			}
		})
	}
}

// TestReconcilerRecheckDue_Batch pins a pass over several accounts: only due
// rows are processed, the limit is honored fewest attempts first (even ahead of
// an older next attempt), and the report counts confirmations and failures.
func TestReconcilerRecheckDue_Batch(t *testing.T) {
	store := newFakeStore()
	seed(store, recheckDID, nil, pending(1, at(11, 20), accountstatus.FailurePDSUnreachable))
	seed(store, recheckOtherDID, nil, pending(2, at(11, 30), accountstatus.FailurePDSErrorStatus))
	seed(store, recheckThirdDID, nil, pending(5, at(11, 10), accountstatus.FailureIdentityUnresolved))
	seed(store, recheckFourthDID, nil, pending(1, at(12, 1), accountstatus.FailurePDSErrorStatus))
	markPresent(store, recheckDID, recheckOtherDID)
	confirmer := newScriptedConfirmer(t)
	confirmer.answer(recheckDID, answerTakendown, nil)
	confirmer.answer(recheckOtherDID, accountstatus.RepoStatus{}, &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSInvalidResponse, Err: errors.New("bad JSON")})
	confirmer.answer(recheckThirdDID, answerTakendown, nil)
	confirmer.answer(recheckFourthDID, answerTakendown, nil)
	reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

	report, err := reconciler.RecheckDue(context.Background(), 2)
	require.NoError(t, err)
	assert.Equal(t, accountstatus.RecheckReport{Due: 2, Confirmed: 1, Failed: 1}, report)
	requireStored(t, store, recheckDID, statusPointer(accountstatus.Takendown), nil)
	requireStored(t, store, recheckOtherDID, nil, pending(3, at(12, 4), accountstatus.FailurePDSInvalidResponse))
	// Due, and the oldest, but past the limit because it has the most attempts.
	requireStored(t, store, recheckThirdDID, nil, pending(5, at(11, 10), accountstatus.FailureIdentityUnresolved))
	// Not yet due.
	requireStored(t, store, recheckFourthDID, nil, pending(1, at(12, 1), accountstatus.FailurePDSErrorStatus))
}

// TestReconcilerRecheckDue_StopsOnCancellation pins that a pass cancelled
// during a confirmation records no attempt for that account, processes no
// later account, and returns the cancellation.
func TestReconcilerRecheckDue_StopsOnCancellation(t *testing.T) {
	first := pending(1, at(11, 55), accountstatus.FailurePDSUnreachable)
	second := pending(2, at(11, 50), accountstatus.FailurePDSErrorStatus)
	store := newFakeStore()
	seed(store, recheckDID, nil, first)
	seed(store, recheckOtherDID, nil, second)
	markPresent(store, recheckDID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	confirmer := newScriptedConfirmer(t)
	confirmer.answers[recheckDID] = func(ctx context.Context) (accountstatus.RepoStatus, error) {
		cancel()
		return accountstatus.RepoStatus{}, &accountstatus.ConfirmationError{
			Failure: accountstatus.FailurePDSUnreachable,
			Err:     fmt.Errorf("asking the PDS: %w", ctx.Err()),
		}
	}
	confirmer.answer(recheckOtherDID, answerTakendown, nil)
	reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

	_, err := reconciler.RecheckDue(ctx, 50)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	requireStored(t, store, recheckDID, nil, first)
	requireStored(t, store, recheckOtherDID, nil, second)
}

// TestReconcilerRecheckDue_CappedFailureThenRecovery pins the long-outage
// path: attempt 7 fails to attempt 8 an hour out, and when the PDS answers at
// that hour the confirmed takedown is applied and the row cleared.
func TestReconcilerRecheckDue_CappedFailureThenRecovery(t *testing.T) {
	store := newFakeStore()
	seed(store, recheckDID, nil, pending(7, at(11, 0), accountstatus.FailurePDSUnreachable))
	markPresent(store, recheckDID)
	confirmer := newScriptedConfirmer(t)
	confirmer.answer(recheckDID, accountstatus.RepoStatus{}, &accountstatus.ConfirmationError{
		Failure: accountstatus.FailurePDSUnreachable, Err: errors.New("dial tcp: connection refused"),
	})
	clock := &testClock{now: reconcilerNow}
	reconciler := accountstatus.NewReconciler(store, confirmer, clock.Now)

	report, err := reconciler.RecheckDue(context.Background(), 50)
	require.NoError(t, err)
	assert.Equal(t, accountstatus.RecheckReport{Due: 1, Failed: 1}, report)
	requireStored(t, store, recheckDID, nil, pending(8, at(13, 0), accountstatus.FailurePDSUnreachable))

	clock.now = at(13, 0)
	confirmer.answer(recheckDID, answerTakendown, nil)
	report, err = reconciler.RecheckDue(context.Background(), 50)
	require.NoError(t, err)
	assert.Equal(t, accountstatus.RecheckReport{Due: 1, Confirmed: 1}, report)
	requireStored(t, store, recheckDID, statusPointer(accountstatus.Takendown), nil)
}

// TestReconcilerHandleEvent_UnknownDID pins the events for a DID with no
// Coves presence and no stored restriction. A takendown or suspended event is
// still confirmed with the PDS and a confirmed restriction is stored, since
// content the account wrote can be indexed after the event. A failed
// confirmation stores nothing and leaves no recheck row: the recheck worker
// would drop it unasked. An activation asks no PDS (guard: unchanged).
// Replaces TestReconcilerHandleEvent_UnknownDIDIsIgnored, which pinned
// ignoring such takedowns without asking the PDS.
func TestReconcilerHandleEvent_UnknownDID(t *testing.T) {
	unreachable := &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSUnreachable, Err: errors.New("connection refused")}
	tests := []struct {
		name            string
		recheck         *accountstatus.PendingRecheck
		active          bool
		pdsAnswer       accountstatus.RepoStatus
		pdsError        error
		wantAsked       []string
		wantRestriction *accountstatus.Status
	}{
		{
			name:            "takendown event confirmed takendown stores it",
			pdsAnswer:       answerTakendown,
			wantAsked:       []string{recheckDID},
			wantRestriction: statusPointer(accountstatus.Takendown),
		},
		{
			name:            "takendown event confirmed suspended stores it and clears a pending recheck",
			recheck:         pending(3, at(12, 4), accountstatus.FailurePDSUnreachable),
			pdsAnswer:       answerSuspended,
			wantAsked:       []string{recheckDID},
			wantRestriction: statusPointer(accountstatus.Suspended),
		},
		{
			name:      "takendown event whose confirmation fails stores nothing and creates no recheck row",
			pdsError:  unreachable,
			wantAsked: []string{recheckDID},
		},
		{
			name:      "takendown event whose confirmation fails clears a pending recheck",
			recheck:   pending(3, at(12, 4), accountstatus.FailurePDSUnreachable),
			pdsError:  unreachable,
			wantAsked: []string{recheckDID},
		},
		{
			// Guard: unchanged behavior.
			name:   "activation asks no PDS and stores nothing",
			active: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			seed(store, recheckDID, nil, tt.recheck)
			confirmer := newScriptedConfirmer(t)
			if tt.wantAsked != nil {
				confirmer.answer(recheckDID, tt.pdsAnswer, tt.pdsError)
			}
			reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

			err := reconciler.HandleEvent(context.Background(), recheckDID, tt.active)
			require.NoError(t, err)
			assert.Equal(t, tt.wantAsked, confirmer.asked, "PDS confirmations asked")
			requireStored(t, store, recheckDID, tt.wantRestriction, nil)
		})
	}
}

// TestReconcilerRecheckDue_UnknownDIDIsDropped pins that a due recheck for a
// DID with no Coves presence and no stored restriction asks no PDS, clears
// the row and counts as Dropped.
func TestReconcilerRecheckDue_UnknownDIDIsDropped(t *testing.T) {
	store := newFakeStore()
	seed(store, recheckDID, nil, pending(2, at(11, 50), accountstatus.FailurePDSUnreachable))
	// No scripted answer: asking the PDS fails the test.
	confirmer := newScriptedConfirmer(t)
	reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

	report, err := reconciler.RecheckDue(context.Background(), 50)
	require.NoError(t, err)
	assert.Equal(t, accountstatus.RecheckReport{Due: 1, Dropped: 1}, report)
	assert.Empty(t, confirmer.asked, "no PDS may be asked for an unknown DID")
	requireStored(t, store, recheckDID, nil, nil)
}

// TestReconcilerRecheckDue_PartialPassKeepsCounts pins that a pass whose
// context ends during a later confirmation returns the error together with
// the counts of the accounts it finished.
func TestReconcilerRecheckDue_PartialPassKeepsCounts(t *testing.T) {
	store := newFakeStore()
	seed(store, recheckDID, nil, pending(1, at(11, 50), accountstatus.FailurePDSUnreachable))
	seed(store, recheckOtherDID, nil, pending(1, at(11, 55), accountstatus.FailurePDSErrorStatus))
	markPresent(store, recheckDID, recheckOtherDID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	confirmer := newScriptedConfirmer(t)
	confirmer.answer(recheckDID, answerTakendown, nil)
	confirmer.answers[recheckOtherDID] = func(ctx context.Context) (accountstatus.RepoStatus, error) {
		cancel()
		<-ctx.Done()
		return accountstatus.RepoStatus{}, &accountstatus.ConfirmationError{
			Failure: accountstatus.FailurePDSUnreachable,
			Err:     fmt.Errorf("asking the PDS: %w", ctx.Err()),
		}
	}
	reconciler := accountstatus.NewReconciler(store, confirmer, (&testClock{now: reconcilerNow}).Now)

	report, err := reconciler.RecheckDue(ctx, 50)
	require.Error(t, err)
	assert.Equal(t, accountstatus.RecheckReport{Due: 2, Confirmed: 1}, report)
	requireStored(t, store, recheckDID, statusPointer(accountstatus.Takendown), nil)
	requireStored(t, store, recheckOtherDID, nil, pending(1, at(11, 55), accountstatus.FailurePDSErrorStatus))
}
