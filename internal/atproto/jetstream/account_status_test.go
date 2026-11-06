package jetstream

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

// accountStatusTargetDID is the account whose #account events these tests
// deliver.
const accountStatusTargetDID = "did:plc:accountstatustargetaaaaa"

// fakeAccountStatusStore is an in-memory accountstatus.Store whose Reconcile
// follows the contract: read the restriction and recheck rows, call decide,
// apply both changes, and change nothing when decide fails.
type fakeAccountStatusStore struct {
	mutex    sync.Mutex
	rows     map[string]accountstatus.Status
	rechecks map[string]accountstatus.PendingRecheck
}

func newFakeAccountStatusStore() *fakeAccountStatusStore {
	return &fakeAccountStatusStore{
		rows:     make(map[string]accountstatus.Status),
		rechecks: make(map[string]accountstatus.PendingRecheck),
	}
}

func (s *fakeAccountStatusStore) Restriction(_ context.Context, did string) (accountstatus.Status, bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	status, found := s.rows[did]
	return status, found, nil
}

func (s *fakeAccountStatusStore) Reconcile(ctx context.Context, did string, decide func(ctx context.Context, current accountstatus.State) (accountstatus.Change, error)) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	status, found := s.rows[did]
	// Every DID here is a known Coves account; the unknown-DID drop is
	// pinned at the reconciler and by the integration test.
	current := accountstatus.State{Status: status, Found: found, Present: true}
	if pending, ok := s.rechecks[did]; ok {
		copied := pending
		current.Recheck = &copied
	}
	change, err := decide(ctx, current)
	if err != nil {
		return err
	}
	switch change.Kind {
	case accountstatus.ChangeSet, accountstatus.ChangeClear, accountstatus.ChangeNone:
	default:
		return fmt.Errorf("fake store: unknown change kind %d", change.Kind)
	}
	switch change.Recheck.Action {
	case accountstatus.RecheckKeep, accountstatus.RecheckSchedule, accountstatus.RecheckClear:
	default:
		return fmt.Errorf("fake store: unknown recheck action %d", change.Recheck.Action)
	}
	switch change.Kind {
	case accountstatus.ChangeSet:
		s.rows[did] = change.Status
	case accountstatus.ChangeClear:
		delete(s.rows, did)
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

func (s *fakeAccountStatusStore) DueRechecks(_ context.Context, now time.Time, limit int) ([]string, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
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
	return due, nil
}

// fakeRepoStatusConfirmer answers every DID with one scripted PDS answer.
type fakeRepoStatusConfirmer struct {
	answer accountstatus.RepoStatus
	err    error
}

func (c *fakeRepoStatusConfirmer) RepoStatus(_ context.Context, _ string) (accountstatus.RepoStatus, error) {
	if c.err != nil {
		return accountstatus.RepoStatus{}, c.err
	}
	return c.answer, nil
}

func accountStatusEvent(active bool, status string) *JetstreamEvent {
	return &JetstreamEvent{
		Did:    accountStatusTargetDID,
		Kind:   "account",
		TimeUS: 1760000000000000,
		Account: &AccountEvent{
			Did:    accountStatusTargetDID,
			Time:   "2026-10-22T12:00:00.000Z",
			Seq:    4242,
			Active: active,
			Status: status,
		},
	}
}

var (
	pdsAnswersTakendown   = accountstatus.RepoStatus{Active: false, Status: "takendown"}
	pdsAnswersSuspended   = accountstatus.RepoStatus{Active: false, Status: "suspended"}
	pdsAnswersActive      = accountstatus.RepoStatus{Active: true}
	pdsAnswersDeactivated = accountstatus.RepoStatus{Active: false, Status: "deactivated"}
)

// TestUserConsumer_AccountStatusEvents pins which #account events change the
// stored status restriction, and that the stored state follows the PDS's
// confirmed answer rather than the event.
func TestUserConsumer_AccountStatusEvents(t *testing.T) {
	takendown := accountstatus.Takendown
	suspended := accountstatus.Suspended

	tests := []struct {
		name string
		// withoutAccountStatus builds the consumer with no WithUserAccountStatus.
		withoutAccountStatus bool
		// knownUser indexes the DID in the users fake; otherwise GetUserByDID
		// answers ErrUserNotFound.
		knownUser   bool
		existingRow *accountstatus.Status
		eventActive bool
		eventStatus string
		pdsAnswer   accountstatus.RepoStatus
		pdsError    error
		wantRow     *accountstatus.Status
	}{
		{
			name: "takendown event confirmed takendown sets takendown", knownUser: true,
			eventStatus: "takendown", pdsAnswer: pdsAnswersTakendown, wantRow: &takendown,
		},
		{
			name: "suspended event confirmed suspended sets suspended", knownUser: true,
			eventStatus: "suspended", pdsAnswer: pdsAnswersSuspended, wantRow: &suspended,
		},
		{
			name: "takendown event while PDS reports active sets nothing", knownUser: true,
			eventStatus: "takendown", pdsAnswer: pdsAnswersActive, wantRow: nil,
		},
		{
			name: "takendown event over a suspended row confirmed takendown stores takendown", knownUser: true,
			existingRow: &suspended, eventStatus: "takendown", pdsAnswer: pdsAnswersTakendown, wantRow: &takendown,
		},
		{
			name: "active event over a row confirmed active clears it", knownUser: true,
			existingRow: &takendown, eventActive: true, pdsAnswer: pdsAnswersActive, wantRow: nil,
		},
		{
			name: "active event over a row while PDS still reports takendown keeps it", knownUser: true,
			existingRow: &takendown, eventActive: true, pdsAnswer: pdsAnswersTakendown, wantRow: &takendown,
		},
		{
			name: "active event with no row sets nothing even when PDS would answer takendown", knownUser: true,
			eventActive: true, pdsAnswer: pdsAnswersTakendown, wantRow: nil,
		},
		{
			name: "deactivated event with no row is ignored", knownUser: true,
			eventStatus: "deactivated", pdsAnswer: pdsAnswersTakendown, wantRow: nil,
		},
		{
			name: "deactivated event over a row is ignored", knownUser: true,
			existingRow: &suspended, eventStatus: "deactivated", pdsAnswer: pdsAnswersTakendown, wantRow: &suspended,
		},
		{
			name: "deleted event with no row is ignored", knownUser: true,
			eventStatus: "deleted", pdsAnswer: pdsAnswersTakendown, wantRow: nil,
		},
		{
			name: "deleted event over a row is ignored", knownUser: true,
			existingRow: &suspended, eventStatus: "deleted", pdsAnswer: pdsAnswersTakendown, wantRow: &suspended,
		},
		{
			name: "throttled event with no row is ignored", knownUser: true,
			eventStatus: "throttled", pdsAnswer: pdsAnswersTakendown, wantRow: nil,
		},
		{
			name: "throttled event over a row is ignored", knownUser: true,
			existingRow: &suspended, eventStatus: "throttled", pdsAnswer: pdsAnswersTakendown, wantRow: &suspended,
		},
		{
			name: "desynchronized event with no row is ignored", knownUser: true,
			eventStatus: "desynchronized", pdsAnswer: pdsAnswersTakendown, wantRow: nil,
		},
		{
			name: "desynchronized event over a row is ignored", knownUser: true,
			existingRow: &suspended, eventStatus: "desynchronized", pdsAnswer: pdsAnswersTakendown, wantRow: &suspended,
		},
		{
			name: "inactive event with empty status and no row is ignored", knownUser: true,
			eventStatus: "", pdsAnswer: pdsAnswersTakendown, wantRow: nil,
		},
		{
			name: "inactive event with empty status over a row is ignored", knownUser: true,
			existingRow: &suspended, eventStatus: "", pdsAnswer: pdsAnswersTakendown, wantRow: &suspended,
		},
		{
			name: "takendown event while PDS reports deactivated sets nothing", knownUser: true,
			eventStatus: "takendown", pdsAnswer: pdsAnswersDeactivated, wantRow: nil,
		},
		{
			name: "takendown event when PDS has no such repo sets nothing", knownUser: true,
			eventStatus: "takendown", pdsError: fmt.Errorf("getRepoStatus: %w", accountstatus.ErrRepoNotFound), wantRow: nil,
		},
		{
			name: "takendown event for a DID with no users row is confirmed and set", knownUser: false,
			eventStatus: "takendown", pdsAnswer: pdsAnswersTakendown, wantRow: &takendown,
		},
		{
			// Guard: today's behavior without the option; green on the base.
			name: "consumer without account status handling accepts a takendown event", withoutAccountStatus: true,
			knownUser: true, eventStatus: "takendown", pdsAnswer: pdsAnswersTakendown, wantRow: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userService := newMockUserService()
			if tt.knownUser {
				require.NoError(t, userService.IndexUser(context.Background(), accountStatusTargetDID, "target.coves.test", "https://pds.coves.test"))
			}
			store := newFakeAccountStatusStore()
			if tt.existingRow != nil {
				store.rows[accountStatusTargetDID] = *tt.existingRow
			}
			confirmer := &fakeRepoStatusConfirmer{answer: tt.pdsAnswer, err: tt.pdsError}

			var options []ConsumerOption
			if !tt.withoutAccountStatus {
				options = append(options, WithUserAccountStatus(store, confirmer))
			}
			consumer := NewUserEventConsumer(userService, &mockIdentityResolverForUser{}, options...)

			err := consumer.HandleEvent(context.Background(), accountStatusEvent(tt.eventActive, tt.eventStatus))
			require.NoError(t, err)

			status, found, readErr := store.Restriction(context.Background(), accountStatusTargetDID)
			require.NoError(t, readErr)
			if tt.wantRow == nil {
				assert.False(t, found, "expected no status row, found %q", status)
				return
			}
			require.True(t, found, "expected status row %q, found none", *tt.wantRow)
			assert.Equal(t, *tt.wantRow, status)
		})
	}
}

// TestUserConsumer_AccountStatusConfirmationFailure pins confirmation-failure
// handling at the consumer: a failed PDS confirmation leaves the stored restriction alone,
// is not an error (the event is consumed), and leaves a recheck scheduled with
// the failure's code so the confirmation is retried.
func TestUserConsumer_AccountStatusConfirmationFailure(t *testing.T) {
	takendown := accountstatus.Takendown

	tests := []struct {
		name         string
		existingRow  *accountstatus.Status
		eventActive  bool
		eventStatus  string
		confirmError error
		wantRow      *accountstatus.Status
		wantCode     accountstatus.ConfirmationFailure
	}{
		{
			name:         "resolution failure on a takendown event with no row",
			eventStatus:  "takendown",
			confirmError: &accountstatus.ConfirmationError{Failure: accountstatus.FailureIdentityUnresolved, Err: errors.New("plc directory unreachable")},
			wantRow:      nil,
			wantCode:     accountstatus.FailureIdentityUnresolved,
		},
		{
			name:         "PDS 503 on a takendown event with no row",
			eventStatus:  "takendown",
			confirmError: &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSErrorStatus, Err: errors.New("getRepoStatus answered status 503")},
			wantRow:      nil,
			wantCode:     accountstatus.FailurePDSErrorStatus,
		},
		{
			name:         "PDS 503 on an active event over a row",
			existingRow:  &takendown,
			eventActive:  true,
			confirmError: &accountstatus.ConfirmationError{Failure: accountstatus.FailurePDSErrorStatus, Err: errors.New("getRepoStatus answered status 503")},
			wantRow:      &takendown,
			wantCode:     accountstatus.FailurePDSErrorStatus,
		},
		{
			name:         "unclassified failure on an active event over a row",
			existingRow:  &takendown,
			eventActive:  true,
			confirmError: errors.New("resolve did document: plc directory unreachable"),
			wantRow:      &takendown,
			wantCode:     accountstatus.FailureUnclassified,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userService := newMockUserService()
			require.NoError(t, userService.IndexUser(context.Background(), accountStatusTargetDID, "target.coves.test", "https://pds.coves.test"))
			store := newFakeAccountStatusStore()
			if tt.existingRow != nil {
				store.rows[accountStatusTargetDID] = *tt.existingRow
			}
			confirmer := &fakeRepoStatusConfirmer{err: tt.confirmError}
			consumer := NewUserEventConsumer(userService, &mockIdentityResolverForUser{},
				WithUserAccountStatus(store, confirmer))

			err := consumer.HandleEvent(context.Background(), accountStatusEvent(tt.eventActive, tt.eventStatus))
			require.NoError(t, err, "a confirmation failure is consumed and rechecked, not returned")

			status, found, readErr := store.Restriction(context.Background(), accountStatusTargetDID)
			require.NoError(t, readErr)
			if tt.wantRow == nil {
				assert.False(t, found, "expected no status row, found %q", status)
			} else if assert.True(t, found, "expected status row %q, found none", *tt.wantRow) {
				assert.Equal(t, *tt.wantRow, status)
			}

			store.mutex.Lock()
			recheck, scheduled := store.rechecks[accountStatusTargetDID]
			store.mutex.Unlock()
			require.True(t, scheduled, "expected a scheduled recheck, found none")
			assert.Equal(t, 1, recheck.Attempts)
			assert.Equal(t, tt.wantCode, recheck.LastErrorCode)
		})
	}
}

// failingAccountStatusStore is an accountstatus.Store whose Reconcile fails
// before deciding, standing in for a Postgres failure.
type failingAccountStatusStore struct {
	fakeAccountStatusStore
	err error
}

func (s *failingAccountStatusStore) Reconcile(_ context.Context, _ string, _ func(ctx context.Context, current accountstatus.State) (accountstatus.Change, error)) error {
	return s.err
}

// TestUserConsumer_AccountStatusStoreFailureIsTransient pins that a local
// store failure stays TRANSIENT: it is not the account's doing, so it keeps
// the in-line retries and the full redrive budget.
func TestUserConsumer_AccountStatusStoreFailureIsTransient(t *testing.T) {
	store := &failingAccountStatusStore{err: errors.New("postgres: connection reset")}
	consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
		WithUserAccountStatus(store, &fakeRepoStatusConfirmer{answer: pdsAnswersTakendown}))

	err := consumer.HandleEvent(context.Background(), accountStatusEvent(false, "takendown"))
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrPermanentEvent), "store failure must not be permanent: %v", err)
	assert.False(t, errors.Is(err, ErrUnresolvedReference), "store failure must not be unresolved-reference: %v", err)
}

// countingRepoStatusConfirmer answers like fakeRepoStatusConfirmer and counts
// the calls it receives.
type countingRepoStatusConfirmer struct {
	fakeRepoStatusConfirmer
	calls int
}

func (c *countingRepoStatusConfirmer) RepoStatus(ctx context.Context, did string) (accountstatus.RepoStatus, error) {
	c.calls++
	return c.fakeRepoStatusConfirmer.RepoStatus(ctx, did)
}

// TestUserConsumer_AccountStatusNonIndexableDID pins that a takendown,
// suspended or active #account event for a DID that is not did:plc or did:web
// is a permanent failure decided without asking any PDS, while a status the
// handler ignores stays a no-op whatever the DID.
func TestUserConsumer_AccountStatusNonIndexableDID(t *testing.T) {
	const nonIndexableDID = "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"
	tests := []struct {
		name          string
		active        bool
		status        string
		wantPermanent bool
	}{
		{name: "takendown", status: "takendown", wantPermanent: true},
		{name: "suspended", status: "suspended", wantPermanent: true},
		{name: "active", active: true, wantPermanent: true},
		{name: "deactivated is ignored", status: "deactivated"},
		{name: "deleted is ignored", status: "deleted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeAccountStatusStore()
			store.rows[nonIndexableDID] = accountstatus.Takendown
			confirmer := &countingRepoStatusConfirmer{fakeRepoStatusConfirmer: fakeRepoStatusConfirmer{answer: pdsAnswersActive}}
			consumer := NewUserEventConsumer(newMockUserService(), &mockIdentityResolverForUser{},
				WithUserAccountStatus(store, confirmer))

			event := accountStatusEvent(tt.active, tt.status)
			event.Did = nonIndexableDID
			event.Account.Did = nonIndexableDID
			err := consumer.HandleEvent(context.Background(), event)
			if tt.wantPermanent {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrPermanentEvent), "non-indexable DID must be permanent: %v", err)
			} else {
				require.NoError(t, err)
			}
			assert.Zero(t, confirmer.calls, "no PDS may be asked about a non-indexable DID")
		})
	}
}
