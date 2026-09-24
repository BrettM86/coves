package moderation_test

import (
	"strings"
	"testing"

	"Coves/internal/core/moderation"

	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const restoreRulesAdminDID = "did:plc:secondadmin"

type restoreRulesScenario struct {
	removeRulesScenario
	removal moderation.Action
	request moderation.RestoreContentRequest
}

func newRestoreRulesScenario(t *testing.T) restoreRulesScenario {
	t.Helper()
	scenario := newRemoveRulesScenario()
	removed, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.NotNil(t, removed.Action)
	scenario.store.writeCalls = nil
	return restoreRulesScenario{
		removeRulesScenario: scenario,
		removal:             *removed.Action,
		request: moderation.RestoreContentRequest{
			ActionID: removed.Action.ID, ReviewedSubject: &moderation.StrongRef{URI: removeRulesURI, CID: removeRulesCID},
			ExpectedVersion: removed.State.Version, IdempotencyKey: "first-restore",
			Reason: removeRulesSpam, PrivateNote: "reviewed",
		},
	}
}

func assertRestoreRulesRejected(t *testing.T, scenario restoreRulesScenario, want error) {
	t.Helper()
	before := scenario.store.state.copy()
	ownerPurges := append([]removeRulesOwnerPurge(nil), scenario.purger.ownerPurges...)
	blobPurges := append([]string(nil), scenario.purger.blobPurges...)
	result, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, scenario.request)
	require.ErrorIs(t, err, want)
	assert.Nil(t, result)
	assert.Equal(t, before, scenario.store.state, "rejected restore must not commit any change")
	assert.Equal(t, ownerPurges, scenario.purger.ownerPurges)
	assert.Equal(t, blobPurges, scenario.purger.blobPurges)
}

func TestRestoreContentValidatesRequestWithoutChangingActiveRemoval(t *testing.T) {
	tooManyGraphemes := strings.Repeat("👍🏽", 1001)
	tooManyBytes := strings.Repeat("👨‍👩‍👧‍👦", 400) + "x"
	require.Equal(t, 1001, uniseg.GraphemeClusterCount(tooManyGraphemes))
	require.Equal(t, 10001, len(tooManyBytes))
	for _, test := range []struct {
		name   string
		change func(*moderation.RestoreContentRequest)
		want   error
	}{
		{"missing action ID", func(r *moderation.RestoreContentRequest) { r.ActionID = "" }, moderation.ErrInvalidRequest},
		{"long action ID", func(r *moderation.RestoreContentRequest) { r.ActionID = strings.Repeat("a", 129) }, moderation.ErrInvalidRequest},
		{"missing key", func(r *moderation.RestoreContentRequest) { r.IdempotencyKey = "" }, moderation.ErrInvalidRequest},
		{"long key", func(r *moderation.RestoreContentRequest) { r.IdempotencyKey = strings.Repeat("k", 129) }, moderation.ErrInvalidRequest},
		{"missing version", func(r *moderation.RestoreContentRequest) { r.ExpectedVersion = "" }, moderation.ErrInvalidRequest},
		{"long version", func(r *moderation.RestoreContentRequest) { r.ExpectedVersion = strings.Repeat("v", 129) }, moderation.ErrInvalidRequest},
		{"missing reason", func(r *moderation.RestoreContentRequest) { r.Reason = "" }, moderation.ErrInvalidRequest},
		{"long reason", func(r *moderation.RestoreContentRequest) { r.Reason = strings.Repeat("r", 641) }, moderation.ErrInvalidRequest},
		{"unknown reason", func(r *moderation.RestoreContentRequest) { r.Reason = "social.coves.moderation.defs#reasonCsam" }, moderation.ErrUnsupportedReason},
		{"too many note graphemes", func(r *moderation.RestoreContentRequest) { r.PrivateNote = tooManyGraphemes }, moderation.ErrInvalidRequest},
		{"too many note bytes", func(r *moderation.RestoreContentRequest) { r.PrivateNote = tooManyBytes }, moderation.ErrInvalidRequest},
		{"malformed reviewed URI", func(r *moderation.RestoreContentRequest) { r.ReviewedSubject.URI = "not an at URI" }, moderation.ErrInvalidRequest},
		{"malformed reviewed CID", func(r *moderation.RestoreContentRequest) { r.ReviewedSubject.CID = "not a CID" }, moderation.ErrInvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRestoreRulesScenario(t)
			test.change(&scenario.request)
			assertRestoreRulesRejected(t, scenario, test.want)
		})
	}
}

func TestRestoreContentRejectsUnknownWrongAuthorityOrInactiveAction(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*restoreRulesScenario)
		want   error
	}{
		{"unknown action", func(s *restoreRulesScenario) { s.request.ActionID = "missing-action" }, moderation.ErrDecisionNotFound},
		{"label action from instance", func(s *restoreRulesScenario) {
			label := s.removal
			label.ID = "label-action"
			label.Action = "label"
			s.store.state.actions[label.ID] = label
			s.request.ActionID = label.ID
		}, moderation.ErrInvalidDecision},
		{"foreign authority removal", func(s *restoreRulesScenario) {
			foreign := s.removal
			foreign.ID = "foreign-removal"
			foreign.AuthorityDID = "did:web:other.example"
			s.store.state.actions[foreign.ID] = foreign
			s.store.state.activeRemovals[inMemoryModerationDecisionKey{foreign.AuthorityDID, removeRulesURI}] = foreign.ID
			s.request.ActionID = foreign.ID
		}, moderation.ErrInvalidDecision},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRestoreRulesScenario(t)
			test.change(&scenario)
			assertRestoreRulesRejected(t, scenario, test.want)
		})
	}

	t.Run("earlier removal cannot reverse a newer active removal", func(t *testing.T) {
		scenario := newRestoreRulesScenario(t)
		first := scenario.removal
		restored, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, scenario.request)
		require.NoError(t, err)
		require.NotNil(t, restored)
		scenario.store.writeCalls = nil
		next := scenario.removeRulesScenario.request
		next.ExpectedVersion = restored.State.Version
		next.IdempotencyKey = "second-removal"
		removedAgain, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, next)
		require.NoError(t, err)
		require.NotNil(t, removedAgain)
		require.NotNil(t, removedAgain.Action)
		scenario.store.writeCalls = nil
		scenario.request.ActionID = first.ID
		scenario.request.ExpectedVersion = removedAgain.State.Version
		scenario.request.IdempotencyKey = "restore-earlier-removal"
		assertRestoreRulesRejected(t, scenario, moderation.ErrInvalidDecision)
		assert.Equal(t, removedAgain.Action.ID, scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, removeRulesURI}])
	})
}

func TestRestoreContentRequiresReviewOfPresentIndexedComment(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*restoreRulesScenario)
		want   error
	}{
		{"missing reviewed subject", func(s *restoreRulesScenario) { s.request.ReviewedSubject = nil }, moderation.ErrInvalidRequest},
		{"reviewed URI differs from decision subject", func(s *restoreRulesScenario) {
			s.request.ReviewedSubject.URI = "at://did:plc:commentauthor/social.coves.community.comment/3kother"
		}, moderation.ErrInvalidRequest},
		{"indexed CID changed after review", func(s *restoreRulesScenario) {
			comment := s.store.state.indexedComments[removeRulesURI]
			comment.CID = "bafyreinewcommentversion"
			s.store.state.indexedComments[removeRulesURI] = comment
			s.reader.record.CID = comment.CID
		}, moderation.ErrContentChanged},
		{"stale version", func(s *restoreRulesScenario) { s.request.ExpectedVersion = "v0" }, moderation.ErrStateConflict},
		{"unparseable version", func(s *restoreRulesScenario) { s.request.ExpectedVersion = "banana" }, moderation.ErrStateConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRestoreRulesScenario(t)
			test.change(&scenario)
			assertRestoreRulesRejected(t, scenario, test.want)
		})
	}
}

func TestRestoreContentRejectsReviewedURIForAuthorDeletedComment(t *testing.T) {
	scenario := newRestoreRulesScenario(t)
	comment := scenario.store.state.indexedComments[removeRulesURI]
	comment.AuthorDeleted = true
	scenario.store.state.indexedComments[removeRulesURI] = comment
	scenario.reader.record.Deleted = true
	scenario.request.ReviewedSubject.URI = "at://did:plc:commentauthor/social.coves.community.comment/3kother"
	assertRestoreRulesRejected(t, scenario, moderation.ErrInvalidRequest)
}

func TestRestoreContentAllowsMissingReviewForDeletedOrPurgedComment(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*restoreRulesScenario)
		want   moderation.RecordState
	}{
		{"author-deleted", func(s *restoreRulesScenario) {
			comment := s.store.state.indexedComments[removeRulesURI]
			comment.AuthorDeleted = true
			s.store.state.indexedComments[removeRulesURI] = comment
			s.reader.record.Deleted = true
		}, moderation.RecordStateDeleted},
		{"purged indexed row", func(s *restoreRulesScenario) {
			delete(s.store.state.indexedComments, removeRulesURI)
			s.reader.record = nil
			s.reader.err = moderation.ErrSubjectNotIndexed
		}, moderation.RecordStateUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRestoreRulesScenario(t)
			test.change(&scenario)
			scenario.request.ReviewedSubject = nil
			result, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, scenario.request)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			assert.Equal(t, test.want, result.State.RecordState)
			assert.Nil(t, result.State.CurrentSubject)
			assert.Equal(t, moderation.ModerationStateClear, result.State.Moderation.State)
		})
	}
}

func TestRestoreContentAppliesInverseWithoutChangingOriginalAction(t *testing.T) {
	scenario := newRestoreRulesScenario(t)
	otherBlock := moderation.MediaBlock{OwnerDID: "did:plc:other", BlobCID: removeRulesFirstImage, ActionID: "unrelated-action"}
	scenario.store.state.mediaBlocks[otherBlock] = true
	priorVersion := scenario.store.state.versions[removeRulesURI]
	result, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, scenario.request)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	action := result.Action
	assert.NotEqual(t, scenario.removal.ID, action.ID)
	assert.Equal(t, moderation.ActionRestore, action.Action)
	assert.Equal(t, restoreRulesAdminDID, action.ActorDID)
	assert.Equal(t, removeRulesInstanceDID, action.AuthorityDID)
	assert.Equal(t, scenario.removal.ID, action.ReversesActionID)
	assert.Equal(t, removeRulesSpam, action.Reason)
	assert.Equal(t, "reviewed", action.PrivateNote)
	assert.Equal(t, moderation.OriginLocal, action.Origin)
	assert.Equal(t, moderation.ScopeInstance, action.ScopeKind)
	assert.Equal(t, removeRulesURI, action.SubjectURI)
	assert.Equal(t, moderation.CommentCollection, action.SubjectCollection)
	assert.Equal(t, scenario.removal, scenario.store.state.actions[scenario.removal.ID], "the removal log row is immutable")
	assert.Equal(t, *action, scenario.store.state.actions[action.ID])
	assert.Empty(t, scenario.store.state.activeRemovals)
	assert.Equal(t, priorVersion+1, scenario.store.state.versions[removeRulesURI])
	assert.Equal(t, "v2", result.State.Version)
	assert.Equal(t, moderation.ModerationStateClear, result.State.Moderation.State)
	assert.Nil(t, result.State.LocalRemoval)
	assert.Equal(t, []string{"InsertAction", "SetRemovalDecision", "SetSubjectVersion", "DeactivateMediaBlocks", "SaveIdempotencyRecord"}, scenario.store.writeCalls)
	require.Len(t, scenario.store.state.mediaBlocks, 3)
	for block, active := range scenario.store.state.mediaBlocks {
		assert.Equal(t, block.ActionID != scenario.removal.ID, active, "only the reversed action's blocks deactivate")
	}
	assert.True(t, scenario.store.state.mediaBlocks[otherBlock])
	following, err := scenario.service.GetSubjectState(t.Context(), removeRulesURI)
	require.NoError(t, err)
	require.NotNil(t, following)
	assert.Equal(t, result.State, *following)
}
