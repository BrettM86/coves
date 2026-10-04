package moderation_test

import (
	"errors"
	"strings"
	"testing"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLabelRulesScenario() (removeRulesScenario, moderation.LabelContentRequest) {
	scenario := newPostRulesScenario(postRulesURI, postRulesAuthorDID, nil)
	return scenario, moderation.LabelContentRequest{
		Subject: scenario.request.Subject, LabelValue: moderation.LabelNSFW,
		ExpectedVersion: "v0", IdempotencyKey: "first-label",
	}
}

func labelRulesNoWrites(t *testing.T, scenario removeRulesScenario, before inMemoryModerationState) {
	t.Helper()
	assert.Empty(t, scenario.store.writeCalls)
	assert.Equal(t, before, scenario.store.state, "rejection must not commit even a version or idempotency key")
	assert.Empty(t, scenario.purger.ownerPurges)
	assert.Empty(t, scenario.purger.blobPurges)
}

func labelRulesAction(id, uri string) moderation.Action {
	return moderation.Action{
		ID: id, Action: moderation.ActionLabel, LabelValue: moderation.LabelNSFW,
		AuthorityDID: removeRulesInstanceDID, ScopeKind: moderation.ScopeInstance,
		SubjectURI: uri, SubjectCollection: moderation.PostV2Collection,
		SubjectCommunityDID: postRulesCommunityDID, ObservedCID: postRulesCID,
		Origin: moderation.OriginLocal,
	}
}

func TestLabelContentRejectsInvalidFieldsWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*removeRulesScenario, *moderation.LabelContentRequest)
		want   error
	}{
		{"comment subject", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Subject.URI = removeRulesURI
			request.Subject.CID = removeRulesCID
		}, moderation.ErrInvalidSubject},
		{"non-content collection", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Subject.URI = "at://did:plc:postauthor/social.coves.actor.profile/self"
		}, moderation.ErrInvalidSubject},
		{"invalid CID", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Subject.CID = "not a cid"
		}, moderation.ErrInvalidRequest},
		{"empty value", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.LabelValue = ""
		}, moderation.ErrInvalidRequest},
		{"129 byte value", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.LabelValue = strings.Repeat("x", 129)
		}, moderation.ErrInvalidRequest},
		{"128 byte value is well formed but unsupported", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.LabelValue = strings.Repeat("x", 128)
		}, moderation.ErrUnsupportedLabel},
		{"gore", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.LabelValue = "gore"
		}, moderation.ErrUnsupportedLabel},
		{"takedown", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.LabelValue = "!takedown"
		}, moderation.ErrUnsupportedLabel},
		{"reason over 640 bytes", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Reason = strings.Repeat("r", 641)
		}, moderation.ErrInvalidRequest},
		{"invalid UTF-8 reason", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Reason = string([]byte{0xff})
		}, moderation.ErrInvalidRequest},
		{"unknown reason", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Reason = "social.coves.moderation.defs#reasonUnknown"
		}, moderation.ErrUnsupportedReason},
		{"doxing reason is only for removal", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Reason = "social.coves.moderation.defs#reasonDoxing"
		}, moderation.ErrUnsupportedReason},
		{"illegal-content reason is only for removal", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Reason = removeRulesIllegal
		}, moderation.ErrUnsupportedReason},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario, request := newLabelRulesScenario()
			test.change(&scenario, &request)
			before := scenario.store.state.copy()
			result, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
			require.ErrorIs(t, err, test.want)
			assert.Nil(t, result)
			labelRulesNoWrites(t, scenario, before)
		})
	}
}

func TestLabelContentAppliesToBothPostCollectionsAndAcceptsOptionalReasons(t *testing.T) {
	for _, reason := range []string{
		"", removeRulesSpam, "social.coves.moderation.defs#reasonHarassment",
		"social.coves.moderation.defs#reasonRuleViolation",
		"social.coves.moderation.defs#reasonModeratorDiscretion",
	} {
		for _, post := range []struct{ name, uri, owner string }{
			{"postv2", postRulesURI, postRulesAuthorDID},
			{"legacy post", legacyPostRulesURI, postRulesCommunityDID},
		} {
			t.Run(post.name+" reason "+reason, func(t *testing.T) {
				scenario := newPostRulesScenario(post.uri, post.owner, nil)
				request := moderation.LabelContentRequest{
					Subject: scenario.request.Subject, LabelValue: moderation.LabelNSFW,
					ExpectedVersion: "v0", IdempotencyKey: "apply-label", Reason: reason, PrivateNote: "operator note",
				}
				result, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
				require.NoError(t, err)
				require.NotNil(t, result, "a successful label must return its mutation result")
				require.Equal(t, moderation.OutcomeApplied, result.Outcome)
				require.NotNil(t, result.Action)
				action := result.Action
				require.NotEmpty(t, action.ID)
				assert.Equal(t, moderation.ActionLabel, action.Action)
				assert.Equal(t, moderation.LabelNSFW, action.LabelValue)
				assert.Equal(t, postRulesCID, action.ObservedCID)
				assert.Equal(t, reason, action.Reason)
				assert.Equal(t, request.PrivateNote, action.PrivateNote)
				assert.Equal(t, removeRulesAdminDID, action.ActorDID)
				assert.Equal(t, removeRulesInstanceDID, action.AuthorityDID)
				assert.Equal(t, moderation.ScopeInstance, action.ScopeKind)
				assert.Equal(t, moderation.OriginLocal, action.Origin)
				assert.Equal(t, post.uri, action.SubjectURI)
				assert.Equal(t, postRulesCommunityDID, action.SubjectCommunityDID)
				collection := moderation.PostV2Collection
				if post.uri == legacyPostRulesURI {
					collection = moderation.LegacyPostCollection
				}
				assert.Equal(t, collection, action.SubjectCollection)
				assert.Equal(t, "v1", result.State.Version)
				assert.Equal(t, moderation.RecordStatePresent, result.State.RecordState)
				assert.Equal(t, moderation.ModerationStateClear, result.State.Moderation.State)
				assert.Equal(t, []moderation.LocalLabel{{Value: moderation.LabelNSFW,
					Action: moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: action.ID}}}, result.State.LocalLabels)
				assert.Equal(t, []moderation.ContentLabel{{Value: moderation.LabelNSFW,
					Sources: []moderation.DecisionSource{{AuthorityDID: removeRulesInstanceDID, ScopeKind: moderation.ScopeInstance}}}},
					result.State.Moderation.ContentLabels)
				assert.Equal(t, map[string]moderation.Action{action.ID: *action}, scenario.store.state.actions)
				assert.Equal(t, inMemoryModerationLabelDecision{actionID: action.ID, active: true},
					scenario.store.state.labelDecisions[inMemoryModerationLabelKey{removeRulesInstanceDID, post.uri, moderation.LabelNSFW}])
				assert.Equal(t, map[string]int64{post.uri: 1}, scenario.store.state.versions)
				assert.Equal(t, []string{"InsertAction", "SetLabelDecision", "SetSubjectVersion", "SaveIdempotencyRecord"}, scenario.store.writeCalls)
				assert.Empty(t, scenario.store.state.mediaBlocks)
				assert.Empty(t, scenario.purger.ownerPurges)
				assert.Empty(t, scenario.purger.blobPurges)
			})
		}
	}
}

func TestLabelContentRedundantApplyKeepsActionAndVersion(t *testing.T) {
	scenario, request := newLabelRulesScenario()
	prior := labelRulesAction("previous-label", postRulesURI)
	scenario.store.seedActiveLabel(prior)
	scenario.store.state.versions[postRulesURI] = 1
	request.ExpectedVersion = "v1"
	request.IdempotencyKey = "different-key"
	result, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, moderation.OutcomeUnchanged, result.Outcome)
	assert.Nil(t, result.Action)
	assert.Equal(t, "v1", result.State.Version)
	assert.Equal(t, []moderation.LocalLabel{{Value: moderation.LabelNSFW,
		Action: moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: prior.ID}}}, result.State.LocalLabels)
	assert.Equal(t, []moderation.ContentLabel{{Value: moderation.LabelNSFW,
		Sources: []moderation.DecisionSource{{AuthorityDID: removeRulesInstanceDID, ScopeKind: moderation.ScopeInstance}}}},
		result.State.Moderation.ContentLabels)
	assert.Equal(t, map[string]moderation.Action{prior.ID: prior}, scenario.store.state.actions)
	assert.Equal(t, map[string]int64{postRulesURI: 1}, scenario.store.state.versions)
	assert.Equal(t, []string{"SaveIdempotencyRecord"}, scenario.store.writeCalls)
}

func TestLabelContentIdempotencyReplayAndChangedValue(t *testing.T) {
	scenario, request := newLabelRulesScenario()
	original, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
	require.NoError(t, err)
	require.NotNil(t, original)
	require.NotNil(t, original.Action)
	replayed, err := assertIdempotencyNoMutation(t, scenario, func() (*moderation.MutationResult, error) {
		return scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
	})
	require.NoError(t, err)
	assert.Equal(t, original, replayed)
	changed := request
	changed.LabelValue = "gore"
	result, err := assertIdempotencyNoMutation(t, scenario, func() (*moderation.MutationResult, error) {
		return scenario.service.LabelContent(t.Context(), removeRulesAdminDID, changed)
	})
	require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
	assert.Nil(t, result)
	assert.Len(t, scenario.store.state.actions, 1)
}

func TestLabelContentRejectsStaleMissingAndDeletedPostsWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*removeRulesScenario, *moderation.LabelContentRequest)
		want   error
	}{
		{"stale version", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.ExpectedVersion = "v2"
		}, moderation.ErrStateConflict},
		{"changed CID", func(_ *removeRulesScenario, request *moderation.LabelContentRequest) {
			request.Subject.CID = "bafyreidifferentpostcid"
		}, moderation.ErrContentChanged},
		{"never indexed", func(scenario *removeRulesScenario, _ *moderation.LabelContentRequest) {
			delete(scenario.store.state.indexedPosts, postRulesURI)
			scenario.reader.record = nil
			scenario.reader.err = moderation.ErrSubjectNotIndexed
		}, moderation.ErrSubjectNotFound},
		{"author deleted", func(scenario *removeRulesScenario, _ *moderation.LabelContentRequest) {
			post := scenario.store.state.indexedPosts[postRulesURI]
			post.AuthorDeleted = true
			scenario.store.state.indexedPosts[postRulesURI] = post
			scenario.reader.record.Deleted = true
		}, moderation.ErrSubjectNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario, request := newLabelRulesScenario()
			test.change(&scenario, &request)
			before := scenario.store.state.copy()
			result, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
			require.ErrorIs(t, err, test.want)
			assert.Nil(t, result)
			labelRulesNoWrites(t, scenario, before)
		})
	}
}

func TestLabelContentCanLabelRemovedPresentPost(t *testing.T) {
	scenario, request := newLabelRulesScenario()
	removal := moderation.Action{
		ID: "existing-removal", Action: moderation.ActionRemove, AuthorityDID: removeRulesInstanceDID,
		ScopeKind: moderation.ScopeInstance, SubjectURI: postRulesURI, SubjectCollection: moderation.PostV2Collection,
	}
	scenario.store.state.actions[removal.ID] = removal
	scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, postRulesURI}] = removal.ID
	scenario.store.state.versions[postRulesURI] = 1
	request.ExpectedVersion = "v1"
	result, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	assert.Equal(t, "v2", result.State.Version)
	assert.Equal(t, moderation.ModerationStateRemoved, result.State.Moderation.State)
	assert.Equal(t, &moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: removal.ID}, result.State.LocalRemoval)
	assert.Equal(t, []moderation.LocalLabel{{Value: moderation.LabelNSFW,
		Action: moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: result.Action.ID}}}, result.State.LocalLabels)
	assert.Equal(t, []moderation.ContentLabel{{Value: moderation.LabelNSFW,
		Sources: []moderation.DecisionSource{{AuthorityDID: removeRulesInstanceDID, ScopeKind: moderation.ScopeInstance}}}},
		result.State.Moderation.ContentLabels)
	assert.Equal(t, removal.ID, scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, postRulesURI}])
}

func TestLabelContentReturnsActiveLabelsOrderedByValue(t *testing.T) {
	scenario, request := newLabelRulesScenario()
	spoiler := labelRulesAction("existing-spoiler-label", postRulesURI)
	spoiler.LabelValue = "spoiler"
	scenario.store.seedActiveLabel(spoiler)
	scenario.store.state.versions[postRulesURI] = 1
	request.ExpectedVersion = "v1"
	result, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Action)
	assert.Equal(t, []moderation.LocalLabel{
		{Value: moderation.LabelNSFW, Action: moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: result.Action.ID}},
		{Value: "spoiler", Action: moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: spoiler.ID}},
	}, result.State.LocalLabels, "labels must be ordered by value, as getSubjectState orders them")
}

func TestLabelContentStoreFailureRollsBackEverything(t *testing.T) {
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
			scenario, request := newLabelRulesScenario()
			storageError := errors.New("injected store failure")
			test.inject(scenario.store, storageError)
			before := scenario.store.state.copy()
			result, err := scenario.service.LabelContent(t.Context(), removeRulesAdminDID, request)
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			assert.ErrorIs(t, err, storageError)
			assert.Nil(t, result)
			assert.Equal(t, test.wantCalls, scenario.store.writeCalls)
			assert.Equal(t, before, scenario.store.state, "a failed label must persist no action, decision, version or idempotency key")
		})
	}
}
