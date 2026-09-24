package moderation_test

import (
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedIdempotencyComment(scenario *removeRulesScenario, recordKey string) moderation.RemoveContentRequest {
	request := scenario.request
	request.Subject.URI = "at://" + removeRulesAuthorDID + "/" + moderation.CommentCollection + "/" + recordKey
	scenario.store.state.indexedComments[request.Subject.URI] = moderation.IndexedComment{
		URI: request.Subject.URI, CID: request.Subject.CID, OwnerDID: removeRulesAuthorDID,
		CommunityDID: removeRulesCommunityDID,
	}
	return request
}

func assertIdempotencyNoMutation(t *testing.T, scenario removeRulesScenario, call func() (*moderation.MutationResult, error)) (*moderation.MutationResult, error) {
	t.Helper()
	before := scenario.store.state.copy()
	scenario.store.writeCalls = nil
	ownerPurges := append([]removeRulesOwnerPurge(nil), scenario.purger.ownerPurges...)
	blobPurges := append([]string(nil), scenario.purger.blobPurges...)
	result, err := call()
	assert.Equal(t, before, scenario.store.state, "replay or rejection must not commit a mutation")
	assert.Empty(t, scenario.store.writeCalls)
	assert.Equal(t, ownerPurges, scenario.purger.ownerPurges)
	assert.Equal(t, blobPurges, scenario.purger.blobPurges)
	return result, err
}

func TestModerationIdempotencyReplaysOriginalResultAfterSubjectChanges(t *testing.T) {
	t.Run("remove replay after restore", func(t *testing.T) {
		scenario := newRemoveRulesScenario()
		original, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
		require.NoError(t, err)
		require.NotNil(t, original)
		require.NotNil(t, original.Action)
		restored, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, moderation.RestoreContentRequest{
			ActionID: original.Action.ID, ReviewedSubject: &scenario.request.Subject,
			ExpectedVersion: original.State.Version, IdempotencyKey: "different-restore-key", Reason: removeRulesSpam,
		})
		require.NoError(t, err)
		require.NotNil(t, restored)
		require.NotEqual(t, original.State.Version, restored.State.Version)
		replayed, err := assertIdempotencyNoMutation(t, scenario, func() (*moderation.MutationResult, error) {
			return scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
		})
		require.NoError(t, err)
		assert.Equal(t, original, replayed, "replay must return the original action and old version verbatim")
		assert.Empty(t, scenario.store.state.activeRemovals)
		assert.Len(t, scenario.store.state.actions, 2)
	})

	t.Run("restore replay after a new removal", func(t *testing.T) {
		scenario := newRestoreRulesScenario(t)
		original, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, scenario.request)
		require.NoError(t, err)
		require.NotNil(t, original)
		require.NotNil(t, original.Action)
		next := scenario.removeRulesScenario.request
		next.ExpectedVersion = original.State.Version
		next.IdempotencyKey = "new-removal-key"
		newRemoval, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, next)
		require.NoError(t, err)
		require.NotNil(t, newRemoval)
		require.NotNil(t, newRemoval.Action)
		require.NotEqual(t, original.State.Version, newRemoval.State.Version)
		replayed, err := assertIdempotencyNoMutation(t, scenario.removeRulesScenario, func() (*moderation.MutationResult, error) {
			return scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, scenario.request)
		})
		require.NoError(t, err)
		assert.Equal(t, original, replayed)
		assert.Equal(t, newRemoval.Action.ID, scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, removeRulesURI}])
		assert.Len(t, scenario.store.state.actions, 3)
	})
}

func TestModerationIdempotencyRejectsDifferentFingerprintWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*moderation.RemoveContentRequest)
	}{
		{"different reason", func(request *moderation.RemoveContentRequest) { request.Reason = removeRulesIllegal }},
		{"different private note", func(request *moderation.RemoveContentRequest) { request.PrivateNote = "different" }},
		{"different subject CID", func(request *moderation.RemoveContentRequest) { request.Subject.CID = "bafyreidifferentcomment" }},
	} {
		t.Run("remove: "+test.name, func(t *testing.T) {
			scenario := newRemoveRulesScenario()
			comment := scenario.store.state.indexedComments[removeRulesURI]
			comment.AuthorDeleted = true
			scenario.store.state.indexedComments[removeRulesURI] = comment
			seed := scenario.request
			seed.IdempotencyKey = "seed-removal"
			removed, err := scenario.service.RemoveContent(t.Context(), restoreRulesAdminDID, seed)
			require.NoError(t, err)
			require.NotNil(t, removed)
			scenario.request.ExpectedVersion = removed.State.Version
			original, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
			require.NoError(t, err)
			require.NotNil(t, original)
			require.Equal(t, moderation.OutcomeUnchanged, original.Outcome)
			request := scenario.request
			test.change(&request)
			result, err := assertIdempotencyNoMutation(t, scenario, func() (*moderation.MutationResult, error) {
				return scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, request)
			})
			require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
			assert.Nil(t, result)
		})
	}

	t.Run("restore: different reviewed CID", func(t *testing.T) {
		scenario := newRestoreRulesScenario(t)
		original, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, scenario.request)
		require.NoError(t, err)
		require.NotNil(t, original)
		request := scenario.request
		request.ReviewedSubject = &moderation.StrongRef{URI: removeRulesURI, CID: "bafyreidifferentcomment"}
		result, err := assertIdempotencyNoMutation(t, scenario.removeRulesScenario, func() (*moderation.MutationResult, error) {
			return scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, request)
		})
		require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
		assert.Nil(t, result)
	})
}

func TestModerationIdempotencyKeyIsScopedToActor(t *testing.T) {
	scenario := newRemoveRulesScenario()
	first := scenario.request
	first.IdempotencyKey = "k1"
	second := seedIdempotencyComment(&scenario, "3ksecond")
	second.IdempotencyKey = "k1"
	firstResult, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, first)
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	secondResult, err := scenario.service.RemoveContent(t.Context(), restoreRulesAdminDID, second)
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.NotNil(t, firstResult.Action)
	require.NotNil(t, secondResult.Action)
	assert.Equal(t, moderation.OutcomeApplied, firstResult.Outcome)
	assert.Equal(t, moderation.OutcomeApplied, secondResult.Outcome)
	assert.NotEqual(t, firstResult.Action.ID, secondResult.Action.ID)
	assert.Len(t, scenario.store.state.actions, 2)
	assert.Len(t, scenario.store.state.idempotency, 2)
	assert.Contains(t, scenario.store.state.idempotency, inMemoryModerationIdempotencyKey{removeRulesAdminDID, removeRulesInstanceDID, "k1"})
	assert.Contains(t, scenario.store.state.idempotency, inMemoryModerationIdempotencyKey{restoreRulesAdminDID, removeRulesInstanceDID, "k1"})
}

func TestModerationIdempotencyLiveKeyCapRejectsOnlyNewKeysForActor(t *testing.T) {
	scenario := newRemoveRulesScenario()
	scenario.service = moderation.NewService(scenario.reader, scenario.store, moderation.Config{
		InstanceDID: removeRulesInstanceDID, IdempotencyRetention: 24 * time.Hour,
		MaxLiveIdempotencyKeys: 2, Now: func() time.Time { return scenario.store.now }, Purger: scenario.purger,
	})
	first := scenario.request
	first.IdempotencyKey = "k1"
	second := seedIdempotencyComment(&scenario, "3ksecond")
	second.IdempotencyKey = "k2"
	third := seedIdempotencyComment(&scenario, "3kthird")
	third.IdempotencyKey = "k3"
	firstResult, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, first)
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	secondResult, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, second)
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	result, err := assertIdempotencyNoMutation(t, scenario, func() (*moderation.MutationResult, error) {
		return scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, third)
	})
	require.ErrorIs(t, err, moderation.ErrInvalidRequest)
	assert.ErrorContains(t, err, "2")
	assert.Nil(t, result)
	assert.Len(t, scenario.store.state.idempotency, 2)
	replayed, err := assertIdempotencyNoMutation(t, scenario, func() (*moderation.MutationResult, error) {
		return scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, first)
	})
	require.NoError(t, err)
	assert.Equal(t, firstResult, replayed)
	otherActor, err := scenario.service.RemoveContent(t.Context(), restoreRulesAdminDID, third)
	require.NoError(t, err)
	require.NotNil(t, otherActor)
	assert.Equal(t, moderation.OutcomeApplied, otherActor.Outcome)
	assert.Len(t, scenario.store.state.idempotency, 3)
}

func TestModerationIdempotencyExpiredKeyIsReplaced(t *testing.T) {
	scenario := newRemoveRulesScenario()
	retention := 2 * time.Hour
	scenario.service = moderation.NewService(scenario.reader, scenario.store, moderation.Config{
		InstanceDID: removeRulesInstanceDID, IdempotencyRetention: retention,
		MaxLiveIdempotencyKeys: 2, Now: func() time.Time { return scenario.store.now }, Purger: scenario.purger,
	})
	first, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.Action)
	restored, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, moderation.RestoreContentRequest{
		ActionID: first.Action.ID, ReviewedSubject: &scenario.request.Subject,
		ExpectedVersion: first.State.Version, IdempotencyKey: "restore-before-expiry", Reason: removeRulesSpam,
	})
	require.NoError(t, err)
	require.NotNil(t, restored)
	key := inMemoryModerationIdempotencyKey{removeRulesAdminDID, removeRulesInstanceDID, scenario.request.IdempotencyKey}
	expired := scenario.store.state.idempotency[key]
	expired.ExpiresAt = scenario.store.now.Add(-time.Second)
	scenario.store.state.idempotency[key] = expired
	request := scenario.request
	request.ExpectedVersion = restored.State.Version
	request.Reason = removeRulesIllegal
	result, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Action)
	assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
	assert.NotEqual(t, first.Action.ID, result.Action.ID)
	assert.Equal(t, "v2", restored.State.Version)
	assert.Equal(t, "v3", result.State.Version)
	replacement := scenario.store.state.idempotency[key]
	assert.NotEqual(t, expired.Fingerprint, replacement.Fingerprint)
	assert.Equal(t, *result, replacement.Result)
	assert.Equal(t, scenario.store.now, replacement.CreatedAt)
	assert.Equal(t, scenario.store.now.Add(retention), replacement.ExpiresAt)
	assert.Len(t, scenario.store.state.idempotency, 2)
}

func TestModerationIdempotencySavesAppliedAndUnchangedButNotRejected(t *testing.T) {
	scenario := newRemoveRulesScenario()
	retention := 90 * time.Minute
	scenario.service = moderation.NewService(scenario.reader, scenario.store, moderation.Config{
		InstanceDID: removeRulesInstanceDID, IdempotencyRetention: retention,
		MaxLiveIdempotencyKeys: 2, Now: func() time.Time { return scenario.store.now }, Purger: scenario.purger,
	})
	applied, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
	require.NoError(t, err)
	require.NotNil(t, applied)
	require.Equal(t, moderation.OutcomeApplied, applied.Outcome)
	appliedKey := inMemoryModerationIdempotencyKey{removeRulesAdminDID, removeRulesInstanceDID, scenario.request.IdempotencyKey}
	require.Len(t, scenario.store.state.idempotency, 1)
	record := scenario.store.state.idempotency[appliedKey]
	assert.Equal(t, removeRulesAdminDID, record.ActorDID)
	assert.Equal(t, removeRulesInstanceDID, record.AuthorityDID)
	assert.Equal(t, scenario.request.IdempotencyKey, record.Key)
	assert.Equal(t, scenario.store.now, record.CreatedAt)
	assert.Equal(t, scenario.store.now.Add(retention), record.ExpiresAt)
	assert.Equal(t, *applied, record.Result)
	assert.NotEmpty(t, record.Fingerprint)

	unchangedRequest := scenario.request
	unchangedRequest.ExpectedVersion = applied.State.Version
	unchangedRequest.IdempotencyKey = "redundant-removal"
	unchanged, err := scenario.service.RemoveContent(t.Context(), restoreRulesAdminDID, unchangedRequest)
	require.NoError(t, err)
	require.NotNil(t, unchanged)
	assert.Equal(t, moderation.OutcomeUnchanged, unchanged.Outcome)
	assert.Nil(t, unchanged.Action)
	unchangedKey := inMemoryModerationIdempotencyKey{restoreRulesAdminDID, removeRulesInstanceDID, unchangedRequest.IdempotencyKey}
	require.Len(t, scenario.store.state.idempotency, 2)
	assert.Equal(t, *unchanged, scenario.store.state.idempotency[unchangedKey].Result)
	assert.Equal(t, scenario.store.now.Add(retention), scenario.store.state.idempotency[unchangedKey].ExpiresAt)

	rejectedRequest := scenario.request
	rejectedRequest.IdempotencyKey = "stale-removal"
	result, err := assertIdempotencyNoMutation(t, scenario, func() (*moderation.MutationResult, error) {
		return scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, rejectedRequest)
	})
	require.ErrorIs(t, err, moderation.ErrStateConflict)
	assert.Nil(t, result)
	assert.NotContains(t, scenario.store.state.idempotency, inMemoryModerationIdempotencyKey{removeRulesAdminDID, removeRulesInstanceDID, rejectedRequest.IdempotencyKey})
}
