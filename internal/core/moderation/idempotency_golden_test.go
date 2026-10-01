package moderation_test

import (
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Idempotency records outlive deploys: a fingerprint or stored result written
// by one build is read back by the next within the retention window. These
// pins fail when the fingerprint tuple or the stored result's JSON field names
// change. Remove and restore fingerprints equal what the code before label
// support produced for the same requests.

const (
	goldenRemoveFingerprint       = "6eed62114f94a7e5f8be6fdac815f87172d5bfe0d6e63bb86167692fdac715a0"
	goldenRestoreFingerprint      = "e349f3bd469aa767505477b493fa9cb01406cac8e57b21fdd82d9d95829856ef"
	goldenLabelFingerprint        = "e6482a4e5c96496e95308e900a948e0f7b92b5397fb47a39d5bbab82a510da4d"
	goldenRetractLabelFingerprint = "8eb887b44b9c3b17b0760156a007127a88eb8b88c8381ebe89c27c487330a378"
)

func goldenLabelledMutationResult() moderation.MutationResult {
	return moderation.MutationResult{
		Outcome: moderation.OutcomeApplied,
		State: moderation.SubjectState{
			Subject: postRulesURI, Version: "v7",
			Moderation: moderation.ModerationView{
				State: moderation.ModerationStateRemoved,
				ContentLabels: []moderation.ContentLabel{{
					Value:   moderation.LabelNSFW,
					Sources: []moderation.DecisionSource{{AuthorityDID: removeRulesInstanceDID, ScopeKind: moderation.ScopeInstance}},
				}},
			},
			RecordState:    moderation.RecordStatePresent,
			CurrentSubject: &moderation.StrongRef{URI: postRulesURI, CID: postRulesCID},
			LocalRemoval:   &moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: "golden-removal"},
			LocalLabels: []moderation.LocalLabel{{
				Value: moderation.LabelNSFW, Action: moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: "golden-label"},
			}},
		},
		Action: &moderation.Action{
			ID: "golden-label", ActorDID: removeRulesAdminDID, AuthorityDID: removeRulesInstanceDID,
			ScopeKind: moderation.ScopeInstance, ScopeCommunityDID: "did:plc:scopecommunity",
			SubjectURI: postRulesURI, SubjectCollection: moderation.PostV2Collection,
			SubjectCommunityDID: postRulesCommunityDID, ObservedCID: postRulesCID,
			Action: moderation.ActionLabel, LabelValue: moderation.LabelNSFW, Reason: removeRulesSpam,
			PrivateNote: "golden note", ReversesActionID: "golden-reversed",
			ReversedActionReason: "social.coves.moderation.defs#reasonHarassment",
			Origin:               moderation.OriginLocal, CreatedAt: time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC),
		},
	}
}

const goldenLabelledMutationResultJSON = `{
	"Outcome": "applied",
	"State": {
		"Subject": "at://did:plc:postauthor/social.coves.community.postv2/3kabc",
		"Version": "v7",
		"Moderation": {
			"State": "removed",
			"ContentLabels": [{"Value": "nsfw", "Sources": [{"AuthorityDID": "did:web:moderation.test", "ScopeKind": "instance"}]}]
		},
		"RecordState": "present",
		"CurrentSubject": {"URI": "at://did:plc:postauthor/social.coves.community.postv2/3kabc", "CID": "bafyreipostversion"},
		"LocalRemoval": {"ServiceDID": "did:web:moderation.test", "ActionID": "golden-removal"},
		"LocalLabels": [{"Value": "nsfw", "Action": {"ServiceDID": "did:web:moderation.test", "ActionID": "golden-label"}}]
	},
	"Action": {
		"ID": "golden-label",
		"ActorDID": "did:plc:firstadmin",
		"AuthorityDID": "did:web:moderation.test",
		"ScopeKind": "instance",
		"ScopeCommunityDID": "did:plc:scopecommunity",
		"SubjectURI": "at://did:plc:postauthor/social.coves.community.postv2/3kabc",
		"SubjectCollection": "social.coves.community.postv2",
		"SubjectCommunityDID": "did:plc:postcommunity",
		"ObservedCID": "bafyreipostversion",
		"Action": "label",
		"LabelValue": "nsfw",
		"Reason": "social.coves.moderation.defs#reasonSpam",
		"PrivateNote": "golden note",
		"ReversesActionID": "golden-reversed",
		"ReversedActionReason": "social.coves.moderation.defs#reasonHarassment",
		"Origin": "local",
		"CreatedAt": "2026-09-24T12:00:00Z"
	}
}`

func TestModerationIdempotencyStoredResultJSONIsStable(t *testing.T) {
	encoded, err := json.Marshal(goldenLabelledMutationResult())
	require.NoError(t, err)
	assert.JSONEq(t, goldenLabelledMutationResultJSON, string(encoded))
	var decoded moderation.MutationResult
	require.NoError(t, json.Unmarshal([]byte(goldenLabelledMutationResultJSON), &decoded))
	assert.Equal(t, goldenLabelledMutationResult(), decoded, "a stored result written before a deploy must replay unchanged")
}

// seedGoldenIdempotencyRecord stores a record under the golden fingerprint with
// a result no mutation could have produced, so a replay is unmistakable.
func seedGoldenIdempotencyRecord(scenario removeRulesScenario, key, fingerprint string) moderation.MutationResult {
	stored := goldenLabelledMutationResult()
	scenario.store.state.idempotency[inMemoryModerationIdempotencyKey{removeRulesAdminDID, removeRulesInstanceDID, key}] = moderation.IdempotencyRecord{
		ActorDID: removeRulesAdminDID, AuthorityDID: removeRulesInstanceDID, Key: key,
		Fingerprint: fingerprint, Result: stored,
		CreatedAt: scenario.store.now, ExpiresAt: scenario.store.now.Add(time.Hour),
	}
	return stored
}

func TestModerationIdempotencyFingerprintsAreStable(t *testing.T) {
	const goldenKey = "golden-key"
	for _, test := range []struct {
		name        string
		fingerprint string
		scenario    func() removeRulesScenario
		call        func(removeRulesScenario) (*moderation.MutationResult, error)
	}{
		{"remove", goldenRemoveFingerprint, newRemoveRulesScenario, func(scenario removeRulesScenario) (*moderation.MutationResult, error) {
			return scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, moderation.RemoveContentRequest{
				Subject:         moderation.StrongRef{URI: removeRulesURI, CID: removeRulesCID},
				ExpectedVersion: "v0", IdempotencyKey: goldenKey, Reason: removeRulesSpam, PrivateNote: "golden removal note",
			})
		}},
		{"restore", goldenRestoreFingerprint, newRemoveRulesScenario, func(scenario removeRulesScenario) (*moderation.MutationResult, error) {
			return scenario.service.RestoreContent(t.Context(), removeRulesAdminDID, moderation.RestoreContentRequest{
				ActionID: "golden-removal", ReviewedSubject: &moderation.StrongRef{URI: removeRulesURI, CID: removeRulesCID},
				ExpectedVersion: "v1", IdempotencyKey: goldenKey, Reason: removeRulesSpam, PrivateNote: "golden restore note",
			})
		}},
		{"label", goldenLabelFingerprint, func() removeRulesScenario {
			scenario, _ := newLabelRulesScenario()
			return scenario
		}, func(scenario removeRulesScenario) (*moderation.MutationResult, error) {
			return scenario.service.LabelContent(t.Context(), removeRulesAdminDID, moderation.LabelContentRequest{
				Subject: moderation.StrongRef{URI: postRulesURI, CID: postRulesCID}, LabelValue: moderation.LabelNSFW,
				ExpectedVersion: "v0", IdempotencyKey: goldenKey, Reason: removeRulesSpam, PrivateNote: "golden label note",
			})
		}},
		{"retract-label", goldenRetractLabelFingerprint, func() removeRulesScenario {
			scenario, _ := newLabelRulesScenario()
			return scenario
		}, func(scenario removeRulesScenario) (*moderation.MutationResult, error) {
			return scenario.service.RetractContentLabel(t.Context(), removeRulesAdminDID, moderation.RetractContentLabelRequest{
				ActionID: "golden-label", ReviewedSubject: &moderation.StrongRef{URI: postRulesURI, CID: postRulesCID},
				ExpectedVersion: "v1", IdempotencyKey: goldenKey,
				Reason: "social.coves.moderation.defs#reasonHarassment", PrivateNote: "golden retraction note",
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := test.scenario()
			stored := seedGoldenIdempotencyRecord(scenario, goldenKey, test.fingerprint)
			result, err := assertIdempotencyNoMutation(t, scenario, func() (*moderation.MutationResult, error) {
				return test.call(scenario)
			})
			require.NoError(t, err, "the request must still match the fingerprint stored by an earlier build")
			assert.Equal(t, &stored, result)
		})
	}
}
