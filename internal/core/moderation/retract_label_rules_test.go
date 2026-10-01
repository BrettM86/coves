package moderation_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type retractLabelRulesScenario struct {
	removeRulesScenario
	label   moderation.Action
	request moderation.RetractContentLabelRequest
}

func newRetractLabelRulesScenario(t *testing.T) retractLabelRulesScenario {
	t.Helper()
	scenario, labelRequest := newLabelRulesScenario()
	labelRequest.Reason = removeRulesSpam
	labelled, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, labelRequest)
	require.NoError(t, err)
	require.NotNil(t, labelled)
	require.NotNil(t, labelled.Action)
	scenario.store.writeCalls = nil
	return retractLabelRulesScenario{
		removeRulesScenario: scenario,
		label:               *labelled.Action,
		request: moderation.RetractContentLabelRequest{
			ActionID: labelled.Action.ID, ReviewedSubject: &moderation.StrongRef{URI: postRulesURI, CID: postRulesCID},
			ExpectedVersion: labelled.State.Version, IdempotencyKey: "first-retraction",
			PrivateNote: "retraction note",
		},
	}
}

func assertRetractLabelRejected(t *testing.T, scenario retractLabelRulesScenario, want error) {
	t.Helper()
	before := scenario.store.state.copy()
	result, err := scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, scenario.request)
	require.ErrorIs(t, err, want)
	assert.Nil(t, result)
	labelRulesNoWrites(t, scenario.removeRulesScenario, before)
}

func TestRetractContentLabelValidatesRequestWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*moderation.RetractContentLabelRequest)
		want   error
	}{
		{"missing action ID", func(request *moderation.RetractContentLabelRequest) { request.ActionID = "" }, moderation.ErrInvalidRequest},
		{"long action ID", func(request *moderation.RetractContentLabelRequest) { request.ActionID = strings.Repeat("a", 129) }, moderation.ErrInvalidRequest},
		{"missing key", func(request *moderation.RetractContentLabelRequest) { request.IdempotencyKey = "" }, moderation.ErrInvalidRequest},
		{"long key", func(request *moderation.RetractContentLabelRequest) {
			request.IdempotencyKey = strings.Repeat("k", 129)
		}, moderation.ErrInvalidRequest},
		{"missing version", func(request *moderation.RetractContentLabelRequest) { request.ExpectedVersion = "" }, moderation.ErrInvalidRequest},
		{"long version", func(request *moderation.RetractContentLabelRequest) {
			request.ExpectedVersion = strings.Repeat("v", 129)
		}, moderation.ErrInvalidRequest},
		{"long reason", func(request *moderation.RetractContentLabelRequest) { request.Reason = strings.Repeat("r", 641) }, moderation.ErrInvalidRequest},
		{"invalid UTF-8 reason", func(request *moderation.RetractContentLabelRequest) { request.Reason = string([]byte{0xff}) }, moderation.ErrInvalidRequest},
		{"unknown reason", func(request *moderation.RetractContentLabelRequest) {
			request.Reason = "social.coves.moderation.defs#reasonUnknown"
		}, moderation.ErrUnsupportedReason},
		{"640 byte unknown reason", func(request *moderation.RetractContentLabelRequest) {
			request.Reason = strings.Repeat("r", 640)
		}, moderation.ErrUnsupportedReason},
		{"doxing reason is only for removal", func(request *moderation.RetractContentLabelRequest) {
			request.Reason = "social.coves.moderation.defs#reasonDoxing"
		}, moderation.ErrUnsupportedReason},
		{"illegal-content reason is only for removal", func(request *moderation.RetractContentLabelRequest) {
			request.Reason = removeRulesIllegal
		}, moderation.ErrUnsupportedReason},
		{"long private note", func(request *moderation.RetractContentLabelRequest) {
			request.PrivateNote = strings.Repeat("n", 10001)
		}, moderation.ErrInvalidRequest},
		{"malformed reviewed URI", func(request *moderation.RetractContentLabelRequest) {
			request.ReviewedSubject.URI = "not an at URI"
		}, moderation.ErrInvalidRequest},
		{"malformed reviewed CID", func(request *moderation.RetractContentLabelRequest) {
			request.ReviewedSubject.CID = "not a CID"
		}, moderation.ErrInvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRetractLabelRulesScenario(t)
			test.change(&scenario.request)
			assertRetractLabelRejected(t, scenario, test.want)
		})
	}
}

func TestRetractContentLabelRejectsUnknownAndInvalidDecisionWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*retractLabelRulesScenario)
		want   error
	}{
		{"unknown action", func(scenario *retractLabelRulesScenario) {
			scenario.request.ActionID = "missing-action"
		}, moderation.ErrDecisionNotFound},
		{"remove action", func(scenario *retractLabelRulesScenario) {
			action := scenario.label
			action.ID, action.Action = "removal-action", moderation.ActionRemove
			scenario.store.state.actions[action.ID] = action
			scenario.request.ActionID = action.ID
		}, moderation.ErrInvalidDecision},
		{"restore action", func(scenario *retractLabelRulesScenario) {
			action := scenario.label
			action.ID, action.Action = "restore-action", moderation.ActionRestore
			scenario.store.state.actions[action.ID] = action
			scenario.request.ActionID = action.ID
		}, moderation.ErrInvalidDecision},
		{"retract-label action", func(scenario *retractLabelRulesScenario) {
			action := scenario.label
			action.ID, action.Action = "other-retraction", moderation.ActionRetractLabel
			scenario.store.state.actions[action.ID] = action
			scenario.request.ActionID = action.ID
		}, moderation.ErrInvalidDecision},
		{"foreign authority label", func(scenario *retractLabelRulesScenario) {
			action := scenario.label
			action.ID, action.AuthorityDID = "foreign-label", "did:web:foreign.test"
			scenario.store.seedActiveLabel(action)
			scenario.request.ActionID = action.ID
		}, moderation.ErrInvalidDecision},
		{"inherited label", func(scenario *retractLabelRulesScenario) {
			action := scenario.label
			action.ID, action.Origin = "inherited-label", "inherited"
			scenario.store.seedActiveLabel(action)
			scenario.request.ActionID = action.ID
		}, moderation.ErrInvalidDecision},
		{"previously retracted label", func(scenario *retractLabelRulesScenario) {
			key := inMemoryModerationLabelKey{removeRulesInstanceDID, postRulesURI, moderation.LabelNSFW}
			scenario.store.state.labelDecisions[key] = inMemoryModerationLabelDecision{actionID: scenario.label.ID, active: false}
			scenario.store.state.versions[postRulesURI] = 2
			scenario.request.ExpectedVersion = "v2"
			scenario.store.state.actions["prior-retraction"] = moderation.Action{
				ID: "prior-retraction", Action: moderation.ActionRetractLabel, ReversesActionID: scenario.label.ID,
				AuthorityDID: removeRulesInstanceDID, SubjectURI: postRulesURI,
			}
		}, moderation.ErrInvalidDecision},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRetractLabelRulesScenario(t)
			test.change(&scenario)
			assertRetractLabelRejected(t, scenario, test.want)
		})
	}
}

func TestRetractContentLabelRejectsSupersededLabelWithoutWrites(t *testing.T) {
	scenario := newRetractLabelRulesScenario(t)
	first := scenario.label
	key := inMemoryModerationLabelKey{removeRulesInstanceDID, postRulesURI, moderation.LabelNSFW}
	scenario.store.state.labelDecisions[key] = inMemoryModerationLabelDecision{actionID: first.ID, active: false}
	scenario.store.state.actions["first-retraction"] = moderation.Action{
		ID: "first-retraction", Action: moderation.ActionRetractLabel, ReversesActionID: first.ID,
		AuthorityDID: removeRulesInstanceDID, SubjectURI: postRulesURI,
	}
	scenario.store.state.versions[postRulesURI] = 2
	secondRequest := moderation.LabelContentRequest{
		Subject: scenario.removeRulesScenario.request.Subject, LabelValue: moderation.LabelNSFW,
		ExpectedVersion: "v2", IdempotencyKey: "second-apply",
	}
	second, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, secondRequest)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.NotNil(t, second.Action)
	require.NotEqual(t, first.ID, second.Action.ID)
	scenario.store.writeCalls = nil
	scenario.request.ExpectedVersion = second.State.Version
	assertRetractLabelRejected(t, scenario, moderation.ErrInvalidDecision)
	assert.Equal(t, second.Action.ID, scenario.store.state.labelDecisions[key].actionID)
}

func TestRetractContentLabelRequiresCurrentReviewAndVersion(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*retractLabelRulesScenario)
		want   error
	}{
		{"present post requires reviewed subject", func(scenario *retractLabelRulesScenario) {
			scenario.request.ReviewedSubject = nil
		}, moderation.ErrInvalidRequest},
		{"reviewed URI differs", func(scenario *retractLabelRulesScenario) {
			scenario.request.ReviewedSubject.URI = "at://did:plc:postauthor/social.coves.community.postv2/3kother"
		}, moderation.ErrInvalidRequest},
		{"indexed CID differs", func(scenario *retractLabelRulesScenario) {
			post := scenario.store.state.indexedPosts[postRulesURI]
			post.CID = "bafyreinewpostversion"
			scenario.store.state.indexedPosts[postRulesURI] = post
			scenario.reader.record.CID = post.CID
		}, moderation.ErrContentChanged},
		{"stale version", func(scenario *retractLabelRulesScenario) {
			scenario.request.ExpectedVersion = "v0"
		}, moderation.ErrStateConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRetractLabelRulesScenario(t)
			test.change(&scenario)
			assertRetractLabelRejected(t, scenario, test.want)
		})
	}
}

func TestRetractContentLabelAppliesWithOptionalReason(t *testing.T) {
	for _, reason := range []string{
		"", removeRulesSpam, "social.coves.moderation.defs#reasonHarassment",
		"social.coves.moderation.defs#reasonRuleViolation",
		"social.coves.moderation.defs#reasonModeratorDiscretion",
	} {
		t.Run("reason "+reason, func(t *testing.T) {
			scenario := newRetractLabelRulesScenario(t)
			scenario.request.Reason = reason
			result, err := scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, scenario.request)
			require.NoError(t, err)
			require.NotNil(t, result, "retraction must return a mutation result")
			require.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			action := result.Action
			require.NotEmpty(t, action.ID)
			assert.NotEqual(t, scenario.label.ID, action.ID)
			assert.Equal(t, moderation.ActionRetractLabel, action.Action)
			assert.Equal(t, moderation.LabelNSFW, action.LabelValue)
			assert.Equal(t, scenario.label.ID, action.ReversesActionID)
			assert.Equal(t, scenario.label.Reason, action.ReversedActionReason)
			assert.Equal(t, reason, action.Reason)
			assert.Equal(t, scenario.request.PrivateNote, action.PrivateNote)
			assert.Equal(t, postRulesCID, action.ObservedCID)
			assert.Equal(t, restoreRulesAdminDID, action.ActorDID)
			assert.Equal(t, removeRulesInstanceDID, action.AuthorityDID)
			assert.Equal(t, moderation.ScopeInstance, action.ScopeKind)
			assert.Equal(t, moderation.OriginLocal, action.Origin)
			assert.Equal(t, postRulesURI, action.SubjectURI)
			assert.Equal(t, moderation.PostV2Collection, action.SubjectCollection)
			assert.Equal(t, postRulesCommunityDID, action.SubjectCommunityDID)
			assert.Equal(t, "v2", result.State.Version)
			assert.Equal(t, moderation.RecordStatePresent, result.State.RecordState)
			assert.Empty(t, result.State.LocalLabels)
			assert.Equal(t, moderation.ModerationView{State: moderation.ModerationStateClear}, result.State.Moderation)
			require.Len(t, scenario.store.state.actions, 2)
			assert.Equal(t, scenario.label, scenario.store.state.actions[scenario.label.ID])
			assert.Equal(t, *action, scenario.store.state.actions[action.ID])
			key := inMemoryModerationLabelKey{removeRulesInstanceDID, postRulesURI, moderation.LabelNSFW}
			assert.Equal(t, inMemoryModerationLabelDecision{actionID: scenario.label.ID, active: false}, scenario.store.state.labelDecisions[key])
			require.NoError(t, scenario.store.InTransaction(t.Context(), func(ctx context.Context, transaction moderation.Transaction) error {
				labels, err := transaction.ActiveLabels(ctx, removeRulesInstanceDID, postRulesURI)
				assert.Empty(t, labels)
				return err
			}))
			assert.Equal(t, int64(2), scenario.store.state.versions[postRulesURI])
			assert.Equal(t, []string{"InsertAction", "SetLabelDecision", "SetSubjectVersion", "SaveIdempotencyRecord"}, scenario.store.writeCalls)
			assert.Empty(t, scenario.store.state.mediaBlocks)
			assert.Empty(t, scenario.purger.ownerPurges)
			assert.Empty(t, scenario.purger.blobPurges)
		})
	}
}

func TestRetractContentLabelAppliesWithoutReviewForDeletedOrUnavailablePost(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*retractLabelRulesScenario)
		want   moderation.RecordState
	}{
		{"author deleted", func(scenario *retractLabelRulesScenario) {
			post := scenario.store.state.indexedPosts[postRulesURI]
			post.AuthorDeleted = true
			scenario.store.state.indexedPosts[postRulesURI] = post
			scenario.reader.record.Deleted = true
		}, moderation.RecordStateDeleted},
		{"never indexed with stored label", func(scenario *retractLabelRulesScenario) {
			delete(scenario.store.state.indexedPosts, postRulesURI)
			scenario.reader.record = nil
			scenario.reader.err = moderation.ErrSubjectNotIndexed
		}, moderation.RecordStateUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			var scenario retractLabelRulesScenario
			if test.want == moderation.RecordStateUnavailable {
				base, _ := newLabelRulesScenario()
				scenario.removeRulesScenario = base
				scenario.label = labelRulesAction("stored-label", postRulesURI)
				scenario.label.Reason = removeRulesSpam
				scenario.store.seedActiveLabel(scenario.label)
				scenario.store.state.versions[postRulesURI] = 1
				scenario.request = moderation.RetractContentLabelRequest{
					ActionID: scenario.label.ID, ExpectedVersion: "v1", IdempotencyKey: "retract-unindexed",
				}
			} else {
				scenario = newRetractLabelRulesScenario(t)
			}
			test.change(&scenario)
			scenario.request.ReviewedSubject = nil
			scenario.store.writeCalls = nil
			result, err := scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, scenario.request)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, test.want, result.State.RecordState)
			assert.Nil(t, result.State.CurrentSubject)
			assert.Empty(t, result.State.LocalLabels)
			assert.Equal(t, moderation.ModerationStateClear, result.State.Moderation.State)
			assert.Equal(t, "v2", result.State.Version)
			assert.False(t, scenario.store.state.labelDecisions[inMemoryModerationLabelKey{removeRulesInstanceDID, postRulesURI, moderation.LabelNSFW}].active)
		})
	}
}

func TestRetractContentLabelPreservesRemoval(t *testing.T) {
	scenario := newRetractLabelRulesScenario(t)
	removeRequest := scenario.removeRulesScenario.request
	removeRequest.ExpectedVersion = "v1"
	removeRequest.IdempotencyKey = "remove-after-label"
	removed, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, removeRequest)
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.NotNil(t, removed.Action)
	scenario.store.writeCalls = nil
	scenario.request.ExpectedVersion = removed.State.Version
	result, err := scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, scenario.request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	assert.Equal(t, "v3", result.State.Version)
	assert.Equal(t, moderation.ModerationStateRemoved, result.State.Moderation.State)
	assert.Equal(t, &moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: removed.Action.ID}, result.State.LocalRemoval)
	assert.Empty(t, result.State.LocalLabels)
	assert.Empty(t, result.State.Moderation.ContentLabels)
	assert.Equal(t, removed.Action.ID, scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, postRulesURI}])
}

func TestRetractContentLabelIdempotencyReplayAndConflicts(t *testing.T) {
	scenario := newRetractLabelRulesScenario(t)
	original, err := scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, scenario.request)
	require.NoError(t, err)
	require.NotNil(t, original)
	require.NotNil(t, original.Action)
	replayed, err := assertIdempotencyNoMutation(t, scenario.removeRulesScenario, func() (*moderation.MutationResult, error) {
		return scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, scenario.request)
	})
	require.NoError(t, err)
	assert.Equal(t, original, replayed)
	for _, test := range []struct {
		name   string
		change func(*moderation.RetractContentLabelRequest)
	}{
		{"different action ID", func(request *moderation.RetractContentLabelRequest) { request.ActionID = "other-action" }},
		{"different reason", func(request *moderation.RetractContentLabelRequest) {
			request.Reason = "social.coves.moderation.defs#reasonHarassment"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := scenario.request
			test.change(&changed)
			result, err := assertIdempotencyNoMutation(t, scenario.removeRulesScenario, func() (*moderation.MutationResult, error) {
				return scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, changed)
			})
			require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
			assert.Nil(t, result)
		})
	}
	assert.Len(t, scenario.store.state.actions, 2)
}

func TestRetractContentLabelStoreFailureRollsBackEverything(t *testing.T) {
	for _, test := range []struct {
		name      string
		inject    func(*inMemoryModerationStore, error)
		wantCalls []string
	}{
		{"SetLabelDecision failure rolls back the inserted action", func(store *inMemoryModerationStore, err error) {
			store.failSetLabelDecision = err
		}, []string{"InsertAction", "SetLabelDecision"}},
		{"label read failure rolls back the action, decision and version", func(store *inMemoryModerationStore, err error) {
			store.failActiveLabelsAfterLabelDecision = err
		}, []string{"InsertAction", "SetLabelDecision", "SetSubjectVersion"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRetractLabelRulesScenario(t)
			storageError := errors.New("injected store failure")
			test.inject(scenario.store, storageError)
			before := scenario.store.state.copy()
			result, err := scenario.service.RetractContentLabel(t.Context(), restoreRulesAdminDID, scenario.request)
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			assert.ErrorIs(t, err, storageError)
			assert.Nil(t, result)
			assert.Equal(t, test.wantCalls, scenario.store.writeCalls)
			assert.Equal(t, before, scenario.store.state, "a failed retraction must persist no action, decision change, version or idempotency key")
			assert.True(t, scenario.store.state.labelDecisions[inMemoryModerationLabelKey{removeRulesInstanceDID, postRulesURI, moderation.LabelNSFW}].active)
		})
	}
}

// The retraction target check rejects any label action the instance did not
// establish locally at instance scope, even if the stored instance decision
// points at it.
func TestRetractContentLabelRejectsNonLocalInstanceLabelBehindInstanceDecision(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*moderation.Action)
	}{
		{"foreign authority", func(action *moderation.Action) { action.AuthorityDID = "did:web:foreign.test" }},
		{"community scope", func(action *moderation.Action) {
			action.ScopeKind, action.ScopeCommunityDID = "community", postRulesCommunityDID
		}},
		{"inherited origin", func(action *moderation.Action) { action.Origin = "inherited" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRetractLabelRulesScenario(t)
			action := scenario.label
			action.ID = "non-local-label"
			test.change(&action)
			scenario.store.state.actions[action.ID] = action
			key := inMemoryModerationLabelKey{removeRulesInstanceDID, postRulesURI, moderation.LabelNSFW}
			scenario.store.state.labelDecisions[key] = inMemoryModerationLabelDecision{actionID: action.ID, active: true}
			scenario.request.ActionID = action.ID
			assertRetractLabelRejected(t, scenario, moderation.ErrInvalidDecision)
		})
	}
}
