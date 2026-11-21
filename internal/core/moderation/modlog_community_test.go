package moderation_test

import (
	"testing"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modlogCommunityAction is a community-scope removal by an ordinary moderator
// of did:plc:community, as the community post-action recorder stores it.
func modlogCommunityAction() moderation.Action {
	action := modlogTestAction()
	action.ActorDID = "did:plc:moderator"
	action.AuthorityDID = "did:plc:community"
	action.ScopeKind = moderation.ScopeCommunity
	action.ScopeCommunityDID = "did:plc:community"
	action.SubjectURI = "at://did:plc:author/social.coves.community.post/3mwmypost"
	action.SubjectCollection = "social.coves.community.post"
	action.SubjectCommunityDID = "did:plc:community"
	return action
}

// modlogRosterAction is a community roster action about did:plc:member, as
// insertCommunityRoleAction stores it.
func modlogRosterAction(kind, role string) moderation.Action {
	action := modlogCommunityAction()
	action.ActorDID = "did:plc:owner"
	action.Action = kind
	action.Role = role
	action.Reason = ""
	action.SubjectURI = "at://did:plc:member"
	action.SubjectCollection = ""
	action.SubjectKind = moderation.SubjectKindAccount
	action.ObservedCID = ""
	return action
}

func TestModlogCommunityAdminActingProjection(t *testing.T) {
	for _, test := range []struct {
		name                  string
		action                func() moderation.Action
		wantAdminActing       bool
		wantActorDID          string
		wantScopeKind         string
		wantScopeCommunityDID string
	}{
		{
			name: "admin acting in a community",
			action: func() moderation.Action {
				action := modlogCommunityAction()
				action.ActorDID = "did:plc:admin"
				action.AdminActing = true
				return action
			},
			wantAdminActing:       true,
			wantActorDID:          "did:plc:admin",
			wantScopeKind:         "community",
			wantScopeCommunityDID: "did:plc:community",
		},
		{
			name:                  "ordinary community moderator",
			action:                modlogCommunityAction,
			wantActorDID:          "did:plc:moderator",
			wantScopeKind:         "community",
			wantScopeCommunityDID: "did:plc:community",
		},
		{
			name:          "instance admin",
			action:        modlogTestAction,
			wantActorDID:  "did:plc:operator",
			wantScopeKind: "instance",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := test.action()
			public, _ := modlogJSON(t, moderation.NewActionView(action))
			admin, _ := modlogJSON(t, moderation.NewAdminActionView(action))
			for viewName, object := range map[string]map[string]any{"public": public, "admin action": modlogObject(t, admin["action"])} {
				if test.wantAdminActing {
					assert.Equal(t, true, object["adminActing"], "%s view", viewName)
				} else {
					assert.NotContains(t, object, "adminActing", "%s view", viewName)
				}
				assert.Equal(t, map[string]any{"did": test.wantActorDID}, object["actor"], "%s view", viewName)
				scope := modlogObject(t, object["scope"])
				assert.Equal(t, test.wantScopeKind, scope["kind"], "%s view", viewName)
				if test.wantScopeCommunityDID == "" {
					assert.NotContains(t, scope, "communityDid", "%s view", viewName)
				} else {
					assert.Equal(t, test.wantScopeCommunityDID, scope["communityDid"], "%s view", viewName)
				}
			}
		})
	}
}

func TestModlogCommunityRosterProjection(t *testing.T) {
	for _, test := range []struct {
		name         string
		action       func() moderation.Action
		wantSubject  string
		wantRole     string
		wantActorDID string
	}{
		{
			name:         "grant-role",
			action:       func() moderation.Action { return modlogRosterAction(moderation.ActionGrantRole, "moderator") },
			wantSubject:  "at://did:plc:member",
			wantRole:     "moderator",
			wantActorDID: "did:plc:owner",
		},
		{
			name: "actorless remove-role",
			action: func() moderation.Action {
				action := modlogRosterAction(moderation.ActionRemoveRole, "moderator")
				action.ActorDID = ""
				return action
			},
			wantSubject: "at://did:plc:member",
			wantRole:    "moderator",
		},
		{
			name:         "transfer-ownership",
			action:       func() moderation.Action { return modlogRosterAction(moderation.ActionTransferOwnership, "owner") },
			wantSubject:  "at://did:plc:member",
			wantRole:     "owner",
			wantActorDID: "did:plc:owner",
		},
		{
			name:         "post removal is not a roster action",
			action:       modlogCommunityAction,
			wantSubject:  "at://did:plc:author/social.coves.community.post/3mwmypost",
			wantActorDID: "did:plc:moderator",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			object, _ := modlogJSON(t, moderation.NewActionView(test.action()))
			assert.Equal(t, test.wantSubject, modlogObject(t, object["subject"])["uri"])
			if test.wantRole == "" {
				assert.NotContains(t, object, "role")
			} else {
				assert.Equal(t, test.wantRole, object["role"])
			}
			if test.wantActorDID == "" {
				assert.NotContains(t, object, "actor")
			} else {
				assert.Equal(t, map[string]any{"did": test.wantActorDID}, object["actor"])
			}
		})
	}
}

func TestModlogHiddenCommunityActionWithholdsCommunity(t *testing.T) {
	for _, test := range []struct {
		name   string
		action func() moderation.Action
	}{
		{
			name: "doxing removal",
			action: func() moderation.Action {
				action := modlogCommunityAction()
				action.Reason = modlogReasonDoxing
				return action
			},
		},
		{
			name: "restore of a doxing removal",
			action: func() moderation.Action {
				action := modlogCommunityAction()
				action.Action = moderation.ActionRestore
				action.Reason = modlogReasonDiscretion
				action.ReversesActionID = "3mwmycommunityremoval"
				action.ReversedActionReason = modlogReasonDoxing
				return action
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := test.action()
			public, _ := modlogJSON(t, moderation.NewActionView(action))
			assert.NotContains(t, public, "subject")
			scope := modlogObject(t, public["scope"])
			assert.Equal(t, "community", scope["kind"])
			assert.NotContains(t, scope, "communityDid")

			admin, _ := modlogJSON(t, moderation.NewAdminActionView(action))
			require.Contains(t, admin, "privateSubject")
			assert.Equal(t, "at://did:plc:author/social.coves.community.post/3mwmypost", modlogObject(t, admin["privateSubject"])["uri"])
		})
	}
}
