package moderation_test

import (
	"testing"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertSubjectStateLocalLabel(t *testing.T, state moderation.SubjectState, actionID, removalState string) {
	t.Helper()
	assert.Equal(t, removalState, state.Moderation.State)
	assert.Equal(t, []moderation.LocalLabel{{
		Value: moderation.LabelNSFW, Action: moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: actionID},
	}}, state.LocalLabels)
	assert.Equal(t, []moderation.ContentLabel{{
		Value:   moderation.LabelNSFW,
		Sources: []moderation.DecisionSource{{AuthorityDID: removeRulesInstanceDID, ScopeKind: moderation.ScopeInstance}},
	}}, state.Moderation.ContentLabels)
}

func TestSubjectStateListsActiveLabelsAndClearsThemAfterRetract(t *testing.T) {
	scenario := newRetractLabelRulesScenario(t)
	stored, err := scenario.store.SubjectModeration(t.Context(), removeRulesInstanceDID, postRulesURI)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, []moderation.Action{scenario.label}, stored.ActiveLabels)
	state, err := scenario.service.GetSubjectState(t.Context(), postRulesURI)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, "v1", state.Version)
	assertSubjectStateLocalLabel(t, *state, scenario.label.ID, moderation.ModerationStateClear)
	retracted, err := scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, scenario.request)
	require.NoError(t, err)
	require.NotNil(t, retracted)
	state, err = scenario.service.GetSubjectState(t.Context(), postRulesURI)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, "v2", state.Version)
	assert.Empty(t, state.LocalLabels)
	assert.Equal(t, moderation.ModerationView{State: moderation.ModerationStateClear}, state.Moderation)
}

func TestSubjectStateRemovalAndRestorePreserveActiveLabel(t *testing.T) {
	scenario := newRetractLabelRulesScenario(t)
	request := scenario.removeRulesScenario.request
	request.ExpectedVersion = "v1"
	request.IdempotencyKey = "remove-labelled-post"
	removed, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, request)
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.NotNil(t, removed.Action)
	assertSubjectStateLocalLabel(t, removed.State, scenario.label.ID, moderation.ModerationStateRemoved)
	assert.Equal(t, &moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: removed.Action.ID}, removed.State.LocalRemoval)

	redundant := request
	redundant.ExpectedVersion = removed.State.Version
	redundant.IdempotencyKey = "redundant-labelled-removal"
	unchanged, err := scenario.service.RemoveContent(t.Context(), restoreRulesAdminDID, redundant)
	require.NoError(t, err)
	require.NotNil(t, unchanged)
	assert.Equal(t, moderation.OutcomeUnchanged, unchanged.Outcome)
	assert.Nil(t, unchanged.Action)
	assert.Equal(t, removed.State.Version, unchanged.State.Version)
	assertSubjectStateLocalLabel(t, unchanged.State, scenario.label.ID, moderation.ModerationStateRemoved)
	assert.Equal(t, removed.State.LocalRemoval, unchanged.State.LocalRemoval)

	restored, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &moderation.StrongRef{URI: postRulesURI, CID: postRulesCID},
		ExpectedVersion: removed.State.Version, IdempotencyKey: "restore-labelled-post", Reason: removeRulesSpam,
	})
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	assert.Equal(t, "v3", restored.State.Version)
	assert.Nil(t, restored.State.LocalRemoval)
	assertSubjectStateLocalLabel(t, restored.State, scenario.label.ID, moderation.ModerationStateClear)
}
