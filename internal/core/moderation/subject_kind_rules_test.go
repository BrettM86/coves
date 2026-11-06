package moderation_test

import (
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subjectKindSeededActionID is the active instance decision seeded on an
// at://<did> subject by seedSubjectKindDecision.
const subjectKindSeededActionID = "action-seeded"

// seedSubjectKindDecision seeds an active instance removal decision of kind on
// at://<did> at version 1, as a store that records the subject kind returns it.
func seedSubjectKindDecision(store *inMemoryModerationStore, did string, kind moderation.SubjectKind, expiresAt *time.Time) {
	subjectURI := "at://" + did
	store.state.actions[subjectKindSeededActionID] = moderation.Action{
		ID: subjectKindSeededActionID, ActorDID: accountBanAdminDID, AuthorityDID: accountBanInstanceDID,
		ScopeKind: moderation.ScopeInstance, SubjectURI: subjectURI, SubjectKind: kind,
		Action: moderation.ActionRemove, Reason: accountBanSpam, Origin: moderation.OriginLocal,
		CreatedAt: time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC), ExpiresAt: expiresAt,
	}
	store.state.activeRemovals[inMemoryModerationDecisionKey{accountBanInstanceDID, subjectURI}] = subjectKindSeededActionID
	store.state.versions[subjectURI] = 1
}

type subjectKindOperation int

const (
	subjectKindBan subjectKindOperation = iota
	subjectKindUnban
	subjectKindRemoveCommunity
	subjectKindRestoreCommunity
)

func runSubjectKindOperation(t *testing.T, store *inMemoryModerationStore, operation subjectKindOperation, did, expectedVersion string) (*moderation.MutationResult, error) {
	t.Helper()
	service := newAccountBanService(store)
	switch operation {
	case subjectKindBan:
		return service.BanAccount(t.Context(), accountBanAdminDID, moderation.BanAccountRequest{
			Subject: did, ExpectedVersion: expectedVersion, IdempotencyKey: "subject-kind-ban",
			Reason: accountBanSpam, PrivateNote: "n",
		})
	case subjectKindUnban:
		return service.UnbanAccount(t.Context(), accountBanAdminDID, moderation.UnbanAccountRequest{
			Subject: did, ExpectedVersion: expectedVersion, IdempotencyKey: "subject-kind-unban",
			Reason: accountBanSpam, PrivateNote: "n",
		})
	case subjectKindRemoveCommunity:
		return service.RemoveCommunity(t.Context(), accountBanAdminDID, moderation.RemoveCommunityRequest{
			Subject: did, ExpectedVersion: expectedVersion, IdempotencyKey: "subject-kind-remove-community",
			Reason: accountBanSpam, PrivateNote: "n",
		})
	case subjectKindRestoreCommunity:
		return service.RestoreCommunity(t.Context(), accountBanAdminDID, moderation.RestoreCommunityRequest{
			Subject: did, ExpectedVersion: expectedVersion, IdempotencyKey: "subject-kind-restore-community",
			Reason: accountBanSpam, PrivateNote: "n",
		})
	}
	t.Fatalf("unknown operation %d", operation)
	return nil, nil
}

func TestAccountAndCommunityMutationsRecordSubjectKind(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation subjectKindOperation
		subject   string
		// seedKind, when set, seeds an active permanent decision of that kind.
		seedKind        moderation.SubjectKind
		expectedVersion string
		want            moderation.SubjectKind
	}{
		{name: "banAccount", operation: subjectKindBan, subject: accountBanAccountDID, expectedVersion: "v0", want: "account"},
		{
			name: "unbanAccount", operation: subjectKindUnban, subject: accountBanAccountDID,
			seedKind: moderation.SubjectKindAccount, expectedVersion: "v1", want: "account",
		},
		{name: "removeCommunity", operation: subjectKindRemoveCommunity, subject: accountBanCommunityDID, expectedVersion: "v0", want: "community"},
		{
			name: "restoreCommunity", operation: subjectKindRestoreCommunity, subject: accountBanCommunityDID,
			seedKind: moderation.SubjectKindCommunity, expectedVersion: "v1", want: "community",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			if test.seedKind != "" {
				seedSubjectKindDecision(store, test.subject, test.seedKind, nil)
			}

			result, err := runSubjectKindOperation(t, store, test.operation, test.subject, test.expectedVersion)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, test.want, result.Action.SubjectKind, "the returned action carries its subject kind")
			stored, exists := store.state.actions[result.Action.ID]
			require.True(t, exists, "the applied action is stored")
			assert.Equal(t, test.want, stored.SubjectKind, "the stored action records its subject kind")
		})
	}
}

// TestSubjectKindDecidesAccountAndCommunityMutations covers mutations whose
// outcome depends on the recorded kind of the active decision rather than on
// whether the DID is an indexed community or whether the decision has an expiry.
func TestSubjectKindDecidesAccountAndCommunityMutations(t *testing.T) {
	inForceExpiry := time.Date(2026, time.October, 23, 12, 0, 0, 0, time.UTC)
	nonIndexedDID := "did:plc:deletedcommunity"
	for _, test := range []struct {
		name      string
		operation subjectKindOperation
		subject   string
		seedKind  moderation.SubjectKind
		expiresAt *time.Time
		wantError error
	}{
		{
			name:      "restoreCommunity never reverses a permanent account ban on an indexed community",
			operation: subjectKindRestoreCommunity, subject: accountBanCommunityDID,
			seedKind: moderation.SubjectKindAccount, wantError: moderation.ErrStateConflict,
		},
		{
			name:      "restoreCommunity lifts a community removal whose community row was deleted",
			operation: subjectKindRestoreCommunity, subject: nonIndexedDID,
			seedKind: moderation.SubjectKindCommunity,
		},
		{
			name:      "unbanAccount lifts an account ban on a DID since indexed as a community",
			operation: subjectKindUnban, subject: accountBanCommunityDID,
			seedKind: moderation.SubjectKindAccount,
		},
		{
			name:      "unbanAccount never reverses a community removal whose community row was deleted",
			operation: subjectKindUnban, subject: nonIndexedDID,
			seedKind: moderation.SubjectKindCommunity, wantError: moderation.ErrStateConflict,
		},
		{
			name:      "banAccount never replaces a community removal whose community row was deleted",
			operation: subjectKindBan, subject: nonIndexedDID,
			seedKind: moderation.SubjectKindCommunity, wantError: moderation.ErrStateConflict,
		},
		{
			name:      "removeCommunity refuses an indexed community under a permanent account ban",
			operation: subjectKindRemoveCommunity, subject: accountBanCommunityDID,
			seedKind: moderation.SubjectKindAccount, wantError: moderation.ErrStateConflict,
		},
		{
			name:      "removeCommunity refuses an indexed community under an in-force timed account ban",
			operation: subjectKindRemoveCommunity, subject: accountBanCommunityDID,
			seedKind: moderation.SubjectKindAccount, expiresAt: &inForceExpiry, wantError: moderation.ErrStateConflict,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			seedSubjectKindDecision(store, test.subject, test.seedKind, test.expiresAt)
			subjectURI := "at://" + test.subject
			before := store.state.copy()

			result, err := runSubjectKindOperation(t, store, test.operation, test.subject, "v1")
			if test.wantError != nil {
				require.ErrorIs(t, err, test.wantError)
				assert.Nil(t, result)
				assert.Equal(t, before, store.state, "a refused mutation leaves the active decision, version and idempotency records unchanged")
				assert.Equal(t, subjectKindSeededActionID,
					store.state.activeRemovals[inMemoryModerationDecisionKey{accountBanInstanceDID, subjectURI}],
					"the seeded decision stays active")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, moderation.ActionRestore, result.Action.Action)
			assert.Equal(t, subjectKindSeededActionID, result.Action.ReversesActionID)
			assert.Equal(t, test.seedKind, result.Action.SubjectKind, "the restore carries the kind of the decision it lifts")
			assert.Empty(t, store.state.activeRemovals, "the lifted decision is no longer active")
			assert.Equal(t, "v2", result.State.Version)
		})
	}
}

func TestRemoveCommunitySupersedesLapsedAccountBan(t *testing.T) {
	for _, test := range []struct {
		name      string
		expiresAt time.Time
	}{
		{name: "expiry equal to the server clock", expiresAt: time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)},
		{name: "expiry before the server clock", expiresAt: time.Date(2026, time.October, 22, 11, 59, 59, 999000000, time.UTC)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			expiresAt := test.expiresAt
			seedSubjectKindDecision(store, accountBanCommunityDID, moderation.SubjectKindAccount, &expiresAt)

			result, err := runSubjectKindOperation(t, store, subjectKindRemoveCommunity, accountBanCommunityDID, "v1")
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, moderation.ActionRemove, result.Action.Action)
			assert.Nil(t, result.Action.ExpiresAt, "a community removal is permanent")
			assert.Equal(t, moderation.SubjectKind("community"), result.Action.SubjectKind)
			assert.Equal(t, result.Action.ID,
				store.state.activeRemovals[inMemoryModerationDecisionKey{accountBanInstanceDID, "at://" + accountBanCommunityDID}],
				"the community removal supersedes the lapsed ban as the active decision")
			assert.Equal(t, "v2", result.State.Version)
		})
	}
}

func TestGetSubjectStateReadsAccountBanOnIndexedCommunity(t *testing.T) {
	for _, test := range []struct {
		name          string
		expiresAt     time.Time
		wantState     string
		wantExpiresAt string
	}{
		{
			name: "ban in force", expiresAt: time.Date(2026, time.October, 23, 12, 0, 0, 0, time.UTC),
			wantState: "removed", wantExpiresAt: "2026-10-23T12:00:00Z",
		},
		{
			name: "ban lapsed at the server clock", expiresAt: time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC),
			wantState: "clear", wantExpiresAt: "2026-10-22T12:00:00Z",
		},
		{
			name: "ban lapsed before the server clock", expiresAt: time.Date(2026, time.October, 21, 18, 0, 0, 0, time.UTC),
			wantState: "clear", wantExpiresAt: "2026-10-21T18:00:00Z",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			expiresAt := test.expiresAt
			seedSubjectKindDecision(store, accountBanCommunityDID, moderation.SubjectKindAccount, &expiresAt)

			state, err := newAccountBanService(store).GetSubjectState(t.Context(), "at://"+accountBanCommunityDID)
			require.NoError(t, err)
			require.NotNil(t, state)
			assert.Equal(t, test.wantState, state.Moderation.State)
			require.NotNil(t, state.LocalRemoval, "the active account ban shows as the local removal")
			assert.Equal(t, subjectKindSeededActionID, state.LocalRemoval.ActionID)
			require.NotNil(t, state.LocalRemoval.ExpiresAt, "an account ban's state carries its expiry")
			assert.Equal(t, test.wantExpiresAt, state.LocalRemoval.ExpiresAt.UTC().Format(time.RFC3339Nano))
		})
	}
}

// TestBanAccountReplayAfterSubjectIndexedAsCommunity covers a DID banned as an
// account that later indexes itself as a community: the stored result answers
// a replay of the same key, while a new key is refused.
func TestBanAccountReplayAfterSubjectIndexedAsCommunity(t *testing.T) {
	const laterCommunityDID = "did:plc:latercommunity"
	for _, test := range []struct {
		name      string
		replayKey string
		wantError error
	}{
		{name: "the same key returns the stored result", replayKey: "ban-before-indexing"},
		{name: "a new key is refused", replayKey: "ban-after-indexing", wantError: moderation.ErrInvalidSubject},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			indexedCommunities := map[string]bool{}
			service := moderation.NewService(&fakeSubjectReader{}, store, moderation.Config{
				InstanceDID: accountBanInstanceDID, IdempotencyRetention: 24 * time.Hour,
				MaxLiveIdempotencyKeys: 1000, Now: accountBanClock,
				Communities: accountBanCommunities{dids: indexedCommunities},
			})
			request := moderation.BanAccountRequest{
				Subject: laterCommunityDID, ExpectedVersion: "v0", IdempotencyKey: "ban-before-indexing",
				Reason: accountBanSpam, PrivateNote: "n",
			}
			first, err := service.BanAccount(t.Context(), accountBanAdminDID, request)
			require.NoError(t, err)
			require.NotNil(t, first)
			require.Equal(t, moderation.OutcomeApplied, first.Outcome)
			require.NotNil(t, first.Action)

			indexedCommunities[laterCommunityDID] = true
			store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{laterCommunityDID: {IsCommunity: true}}
			request.IdempotencyKey = test.replayKey
			replay, err := service.BanAccount(t.Context(), accountBanAdminDID, request)
			if test.wantError != nil {
				require.ErrorIs(t, err, test.wantError)
				assert.Nil(t, replay)
				return
			}
			require.NoError(t, err, "a replay of a stored ban is answered from its idempotency record")
			require.NotNil(t, replay)
			assert.Equal(t, moderation.OutcomeApplied, replay.Outcome)
			require.NotNil(t, replay.Action)
			assert.Equal(t, first.Action.ID, replay.Action.ID, "the replay returns the original action")
		})
	}
}
