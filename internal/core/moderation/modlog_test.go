package moderation_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modlogReasonSpam       = "social.coves.moderation.defs#reasonSpam"
	modlogReasonHarassment = "social.coves.moderation.defs#reasonHarassment"
	modlogReasonDoxing     = "social.coves.moderation.defs#reasonDoxing"
	modlogReasonIllegal    = "social.coves.moderation.defs#reasonIllegalContent"
	modlogReasonRule       = "social.coves.moderation.defs#reasonRuleViolation"
	modlogReasonDiscretion = "social.coves.moderation.defs#reasonModeratorDiscretion"
)

func modlogTestAction() moderation.Action {
	return moderation.Action{
		ID:                  "3mwmytestaction",
		ActorDID:            "did:plc:operator",
		AuthorityDID:        "did:web:instance.example",
		ScopeKind:           moderation.ScopeInstance,
		SubjectURI:          "at://did:plc:author/social.coves.community.comment/3mwmycomment",
		SubjectCollection:   moderation.CommentCollection,
		SubjectCommunityDID: "did:plc:community",
		ObservedCID:         "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
		Action:              moderation.ActionRemove,
		Reason:              modlogReasonSpam,
		PrivateNote:         "SECRET-NOTE-operator-only 🔒\nsecond line",
		Origin:              moderation.OriginLocal,
		CreatedAt:           time.Date(2026, 9, 28, 15, 4, 5, 123456789, time.FixedZone("UTC+3", 3*60*60)),
		SubjectAccess:       moderation.SubjectAccessPublic,
	}
}

func modlogJSON(t *testing.T, value any) (map[string]any, []byte) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(raw, &object))
	return object, raw
}

func modlogObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	require.Truef(t, ok, "expected JSON object, got %#v", value)
	return object
}

func TestActionViewProjectsOnlyPublicFields(t *testing.T) {
	action := modlogTestAction()
	view := moderation.NewActionView(action)
	require.Equal(t, moderation.ActionRefView{ServiceDID: action.AuthorityDID, ActionID: action.ID}, view.Ref)
	assert.Equal(t, action.Action, view.Action)
	assert.Equal(t, action.AuthorityDID, view.AuthorityDID)
	assert.Equal(t, moderation.ScopeView{Kind: moderation.ScopeInstance}, view.Scope)
	assert.Equal(t, "2026-09-28T12:04:05.123456789Z", view.CreatedAt)
	assert.Equal(t, action.Origin, view.Origin)
	assert.Equal(t, &moderation.SubjectRefView{URI: action.SubjectURI, CID: action.ObservedCID}, view.Subject)
	assert.Equal(t, action.Reason, view.Reason)
	assert.Equal(t, &moderation.ActorRefView{DID: action.ActorDID}, view.Actor)
	assert.Nil(t, view.Reverses)

	object, raw := modlogJSON(t, view)
	assert.NotContains(t, object, "reverses")
	assert.NotContains(t, object, "actorDid")
	assert.NotContains(t, object, "privateNote")
	assert.NotContains(t, string(raw), action.PrivateNote)
	assert.NotContains(t, string(raw), "SECRET-NOTE-")
	assert.NotContains(t, string(raw), action.SubjectCommunityDID)
}

func TestActionViewOptionalFieldsAndReverses(t *testing.T) {
	action := modlogTestAction()
	action.ActorDID = ""
	action.ObservedCID = ""
	action.ScopeCommunityDID = "did:plc:scopedcommunity"
	action.LabelValue = "nsfw"
	action.Action = moderation.ActionRestore
	action.ReversesActionID = "3mwmyremoval"
	view := moderation.NewActionView(action)
	assert.Nil(t, view.Actor)
	assert.Equal(t, "nsfw", view.LabelValue)
	assert.Equal(t, "did:plc:scopedcommunity", view.Scope.CommunityDID)
	assert.Equal(t, &moderation.ActionRefView{ServiceDID: action.AuthorityDID, ActionID: action.ReversesActionID}, view.Reverses)
	require.NotNil(t, view.Subject)
	assert.Equal(t, action.SubjectURI, view.Subject.URI)
	assert.Empty(t, view.Subject.CID)

	object, _ := modlogJSON(t, view)
	assert.NotContains(t, object, "actor")
	assert.NotContains(t, modlogObject(t, object["subject"]), "cid")
	assert.Equal(t, action.ScopeCommunityDID, modlogObject(t, object["scope"])["communityDid"])
	assert.Equal(t, action.ReversesActionID, modlogObject(t, object["reverses"])["actionId"])

	action.ScopeCommunityDID = ""
	action.LabelValue = ""
	object, _ = modlogJSON(t, moderation.NewActionView(action))
	assert.NotContains(t, modlogObject(t, object["scope"]), "communityDid")
	assert.NotContains(t, object, "labelValue")
}

func TestModlogHiddenSubjectAndAdminProjection(t *testing.T) {
	for _, test := range []struct {
		name           string
		reason         string
		reversedReason string
		restore        bool
		hidden         bool
	}{
		{"illegal content removal", modlogReasonIllegal, "", false, true},
		{"doxing removal", modlogReasonDoxing, "", false, true},
		{"restore of illegal content", modlogReasonDiscretion, modlogReasonIllegal, true, true},
		{"restore of doxing", modlogReasonDiscretion, modlogReasonDoxing, true, true},
		{"spam removal", modlogReasonSpam, "", false, false},
		{"harassment removal", modlogReasonHarassment, "", false, false},
		{"rule violation removal", modlogReasonRule, "", false, false},
		{"discretion removal", modlogReasonDiscretion, "", false, false},
		{"public restore of spam", modlogReasonDiscretion, modlogReasonSpam, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := modlogTestAction()
			action.Reason = test.reason
			if test.restore {
				action.Action = moderation.ActionRestore
				action.ReversesActionID = "3mwmyoriginalremoval"
				action.ReversedActionReason = test.reversedReason
			}
			public := moderation.NewActionView(action)
			publicObject, publicJSON := modlogJSON(t, public)
			assert.Equal(t, test.reason, public.Reason)
			if test.hidden {
				assert.Nil(t, public.Subject)
				assert.NotContains(t, publicObject, "subject", "hidden subject must be omitted, not null")
				assert.NotContains(t, string(publicJSON), action.SubjectURI)
			} else {
				assert.Equal(t, &moderation.SubjectRefView{URI: action.SubjectURI, CID: action.ObservedCID}, public.Subject)
			}
			if test.restore {
				assert.Equal(t, &moderation.ActionRefView{ServiceDID: action.AuthorityDID, ActionID: action.ReversesActionID}, public.Reverses)
			} else {
				assert.Nil(t, public.Reverses)
			}

			admin := moderation.NewAdminActionView(action)
			assert.Equal(t, public, admin.Action)
			assert.Equal(t, action.ActorDID, admin.ActorDID)
			assert.Equal(t, action.PrivateNote, admin.PrivateNote)
			adminObject, _ := modlogJSON(t, admin)
			assert.Equal(t, publicObject, modlogObject(t, adminObject["action"]))
			assert.Equal(t, action.ActorDID, adminObject["actorDid"])
			assert.Equal(t, action.PrivateNote, adminObject["privateNote"])
			if test.hidden {
				require.Equal(t, &moderation.SubjectRefView{URI: action.SubjectURI, CID: action.ObservedCID}, admin.PrivateSubject)
				assert.Equal(t, action.SubjectURI, modlogObject(t, adminObject["privateSubject"])["uri"])
			} else {
				assert.Nil(t, admin.PrivateSubject)
				assert.NotContains(t, adminObject, "privateSubject")
			}
		})
	}
	blank := modlogTestAction()
	blank.Reason = modlogReasonIllegal
	blank.PrivateNote = ""
	blank.ObservedCID = ""
	adminObject, _ := modlogJSON(t, moderation.NewAdminActionView(blank))
	assert.NotContains(t, adminObject, "privateNote")
	privateSubject := modlogObject(t, adminObject["privateSubject"])
	assert.Equal(t, blank.SubjectURI, privateSubject["uri"])
	assert.NotContains(t, privateSubject, "cid")
}

func TestModlogPublicProjectionDoesNotLeakStoredFields(t *testing.T) {
	allowed := map[string]bool{}
	for _, path := range []string{
		"ref", "ref.serviceDid", "ref.actionId", "action", "authorityDid", "scope", "scope.kind",
		"scope.communityDid", "createdAt", "origin", "subject", "subject.uri", "subject.cid",
		"reason", "labelValue", "actor", "actor.did", "reverses", "reverses.serviceDid", "reverses.actionId",
	} {
		allowed[path] = true
	}
	forbidden := map[string]bool{
		"privateNote": true, "actorDid": true, "privateSubject": true,
		"private_classification": true, "stored_result": true,
	}
	for _, test := range []struct {
		name, reason, reversedReason string
		hidden                       bool
	}{
		{"spam", modlogReasonSpam, modlogReasonSpam, false},
		{"harassment", modlogReasonHarassment, modlogReasonSpam, false},
		{"rule violation", modlogReasonRule, modlogReasonSpam, false},
		{"discretion", modlogReasonDiscretion, modlogReasonSpam, false},
		{"illegal content", modlogReasonIllegal, modlogReasonSpam, true},
		{"doxing", modlogReasonDoxing, modlogReasonSpam, true},
		{"restore reversing illegal content", modlogReasonDiscretion, modlogReasonIllegal, true},
		{"restore reversing doxing", modlogReasonDiscretion, modlogReasonDoxing, true},
	} {
		for _, kind := range []string{moderation.ActionRemove, moderation.ActionRestore} {
			t.Run(test.name+" "+kind, func(t *testing.T) {
				action := modlogTestAction()
				action.Action = kind
				action.Reason = test.reason
				action.ReversedActionReason = test.reversedReason
				action.ReversesActionID = "3mwmyearlier"
				action.ScopeCommunityDID = "did:plc:scope"
				action.LabelValue = "nsfw"
				view := moderation.NewActionView(action)
				object, raw := modlogJSON(t, view)
				require.Equal(t, action.Reason, object["reason"], "the leak check must inspect a populated projection")
				require.NotEmpty(t, object["ref"], "the public projection must include an action reference")
				var paths []string
				var walk func(string, any)
				walk = func(prefix string, value any) {
					switch value := value.(type) {
					case map[string]any:
						for key, child := range value {
							path := key
							if prefix != "" {
								path = prefix + "." + key
							}
							paths = append(paths, path)
							assert.False(t, forbidden[key], "private key at %s", path)
							walk(path, child)
						}
					case []any:
						for _, child := range value {
							walk(prefix, child)
						}
					}
				}
				walk("", object)
				for _, path := range paths {
					assert.True(t, allowed[path], "unexpected public JSON key path %s", path)
				}
				assert.False(t, strings.Contains(string(raw), action.PrivateNote), "public JSON leaked the private note")
				assert.NotContains(t, string(raw), "SECRET-NOTE-", "public JSON leaked a serialized private note")
				if test.hidden {
					assert.NotContains(t, object, "subject")
					assert.NotContains(t, string(raw), action.SubjectURI)
				} else {
					assert.Equal(t, action.SubjectURI, modlogObject(t, object["subject"])["uri"])
				}
			})
		}
	}
}
