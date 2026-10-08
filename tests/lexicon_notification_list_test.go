package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

func TestNotificationListLexicon_Contract(t *testing.T) {
	const nsid = "social.coves.notification.listNotifications"
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory(lexiconDir))
	raw, err := os.ReadFile(filepath.Join(lexiconDir, "social", "coves", "notification", "listNotifications.json"))
	require.NoError(t, err, "listNotifications query lexicon must exist")
	var doc struct {
		ID   string `json:"id"`
		Defs struct {
			Main struct {
				Parameters struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"parameters"`
				Output struct {
					Schema json.RawMessage `json:"schema"`
				} `json:"output"`
				Errors []struct {
					Name string `json:"name"`
				} `json:"errors"`
			} `json:"main"`
		} `json:"defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, nsid, doc.ID)
	resolved, err := catalog.Resolve(nsid)
	require.NoError(t, err)
	_, ok := resolved.Def.(lexicon.SchemaQuery)
	require.True(t, ok, "listNotifications must be a query, got %T", resolved.Def)
	var limit struct {
		Type    string `json:"type"`
		Minimum int    `json:"minimum"`
		Maximum int    `json:"maximum"`
		Default int    `json:"default"`
	}
	require.NoError(t, json.Unmarshal(doc.Defs.Main.Parameters.Properties["limit"], &limit))
	require.Equal(t, "integer", limit.Type)
	require.Equal(t, 1, limit.Minimum)
	require.Equal(t, 100, limit.Maximum)
	require.Equal(t, 50, limit.Default)
	var cursor struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(doc.Defs.Main.Parameters.Properties["cursor"], &cursor))
	require.Equal(t, "string", cursor.Type)
	var errorNames []string
	for _, declared := range doc.Defs.Main.Errors {
		errorNames = append(errorNames, declared.Name)
	}
	require.Contains(t, errorNames, "InvalidCursor")

	var output lexicon.SchemaObject
	require.NoError(t, json.Unmarshal(doc.Defs.Main.Output.Schema, &output))
	require.Equal(t, "object", output.Type)
	properties := make(map[string]any, len(output.Properties))
	for name, value := range output.Properties {
		properties[name] = value
	}
	assertResponseShapeMatches(t, properties, notifications.ListNotificationsOutput{})
	item, ok := output.Properties["notifications"].Inner.(lexicon.SchemaArray)
	require.True(t, ok, "notifications must be an array")
	itemRef, ok := item.Items.Inner.(lexicon.SchemaRef)
	require.True(t, ok, "notification items must reference notificationView")
	require.Equal(t, "social.coves.notification.defs#notificationView", itemRef.Ref)
	viewDef, err := catalog.Resolve(itemRef.Ref)
	require.NoError(t, err)
	viewObject, ok := viewDef.Def.(lexicon.SchemaObject)
	require.True(t, ok)
	viewProperties := make(map[string]any, len(viewObject.Properties))
	for name, value := range viewObject.Properties {
		viewProperties[name] = value
	}
	assertResponseShapeMatches(t, viewProperties, notifications.NotificationView{})
	authorRef, ok := viewObject.Properties["author"].Inner.(lexicon.SchemaRef)
	require.True(t, ok, "author must reference the shared profile view")
	require.Equal(t, "social.coves.actor.defs#profileView", authorRef.Ref)
	root, ok := viewObject.Properties["rootPost"].Inner.(lexicon.SchemaObject)
	require.True(t, ok, "rootPost must be an object")
	communityRef, ok := root.Properties["community"].Inner.(lexicon.SchemaRef)
	require.True(t, ok, "rootPost.community must reference the shared community view")
	require.Equal(t, "social.coves.community.post.defs#communityRef", communityRef.Ref)
	for _, nested := range []struct {
		field    string
		response any
	}{
		{"rootPost", notifications.RootPostView{}},
		{"subject", notifications.SubjectView{}},
		{"record", notifications.RecordView{}},
	} {
		object, ok := viewObject.Properties[nested.field].Inner.(lexicon.SchemaObject)
		require.True(t, ok, "%s must be an object", nested.field)
		nestedProperties := make(map[string]any, len(object.Properties))
		for name, value := range object.Properties {
			nestedProperties[name] = value
		}
		assertResponseShapeMatches(t, nestedProperties, nested.response)
	}
	profileDef, err := catalog.Resolve(authorRef.Ref)
	require.NoError(t, err)
	profileObject, ok := profileDef.Def.(lexicon.SchemaObject)
	require.True(t, ok, "profileView must be an object")
	profileProperties := make(map[string]any, len(profileObject.Properties))
	for name, value := range profileObject.Properties {
		profileProperties[name] = value
	}
	assertResponseShapeMatches(t, profileProperties, notifications.ProfileView{})
	addPreferencesRecord(t, catalog, "test.coves.notification.listOutput", output)

	name := "Reply Author"
	valid := notifications.ListNotificationsOutput{Notifications: []notifications.NotificationView{
		{
			Reason: notifications.ReasonPostReply, SortAt: "2026-09-20T09:03:00Z", IsRead: false,
			RootPost: &notifications.RootPostView{URI: "at://did:plc:owner/social.coves.community.postv2/root", CID: "bafyroot", Title: "Root", Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.coves.social", Name: "Community"}},
			Subject:  &notifications.SubjectView{URI: "at://did:plc:owner/social.coves.community.postv2/root", CID: "bafyroot", Preview: "Root"},
			Record:   &notifications.RecordView{URI: "at://did:plc:actor/social.coves.community.comment/one", CID: "bafyreplyone", Excerpt: "First reply", CreatedAt: "2026-09-20T09:02:00Z"},
			Author:   &notifications.ProfileView{DID: "did:plc:actor", Handle: "actor.test", DisplayName: &name},
		},
		{
			Reason: notifications.ReasonCommentReply, SortAt: "2026-09-20T09:01:00Z", IsRead: true,
			RootPost: &notifications.RootPostView{URI: "at://did:plc:owner/social.coves.community.postv2/root", CID: "bafyroot", Title: "Root", Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.coves.social", Name: "Community"}},
			Subject:  &notifications.SubjectView{URI: "at://did:plc:owner/social.coves.community.comment/parent", CID: "bafyparent", Preview: "Parent"},
			Record:   &notifications.RecordView{URI: "at://did:plc:actor/social.coves.community.comment/two", CID: "bafyreplytwo", Excerpt: "Second reply", CreatedAt: "2026-09-20T09:00:00Z"},
			Author:   &notifications.ProfileView{DID: "did:plc:unindexed"},
		},
	}, Cursor: "next-page", SeenAt: "2026-09-20T09:00:00Z"}
	encoded, err := json.Marshal(valid)
	require.NoError(t, err)
	data, err := atdata.UnmarshalJSON(encoded)
	require.NoError(t, err)
	data["$type"] = "test.coves.notification.listOutput"
	require.NoError(t, lexicon.ValidateRecord(catalog, data, "test.coves.notification.listOutput", 0))

	for _, tc := range []struct{ name, field, container string }{
		{"missing reason", "reason", "view"},
		{"missing sortAt", "sortAt", "view"},
		{"missing isRead", "isRead", "view"},
		{"missing rootPost", "rootPost", "view"},
		{"missing record cid", "cid", "record"},
		{"missing subject cid", "cid", "subject"},
		{"missing rootPost cid", "cid", "rootPost"},
		{"missing author did", "did", "author"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fixture map[string]any
			require.NoError(t, json.Unmarshal(encoded, &fixture))
			fixture["$type"] = "test.coves.notification.listOutput"
			view := fixture["notifications"].([]any)[0].(map[string]any)
			if tc.container == "view" {
				delete(view, tc.field)
			} else {
				delete(view[tc.container].(map[string]any), tc.field)
			}
			require.Error(t, lexicon.ValidateRecord(catalog, fixture, "test.coves.notification.listOutput", 0), "missing %s.%s must be rejected", tc.container, tc.field)
		})
	}
}
