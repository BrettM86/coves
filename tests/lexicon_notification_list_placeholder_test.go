package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

func placeholderListLexicon(t *testing.T) (*lexicon.BaseCatalog, string, lexicon.SchemaObject) {
	t.Helper()
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory(lexiconDir))
	raw, err := os.ReadFile(filepath.Join(lexiconDir, "social", "coves", "notification", "listNotifications.json"))
	require.NoError(t, err)
	var document struct {
		Defs struct {
			Main struct {
				Output struct {
					Schema json.RawMessage `json:"schema"`
				} `json:"output"`
			} `json:"main"`
		} `json:"defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &document))
	var output lexicon.SchemaObject
	require.NoError(t, json.Unmarshal(document.Defs.Main.Output.Schema, &output))
	const recordID = "test.coves.notification.placeholderListOutput"
	addPreferencesRecord(t, catalog, recordID, output)
	return catalog, recordID, output
}

func TestNotificationListLexicon_PlaceholderRepliesValidate(t *testing.T) {
	catalog, recordID, output := placeholderListLexicon(t)
	// Both reply reasons appear as live and as all-three-reference placeholders.
	const fixture = `{"notifications":[
		{"reason":"postReply","sortAt":"2026-09-20T09:04:00Z","isRead":false,"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot1","title":"Root"},"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot1","preview":"Root"},"record":{"uri":"at://did:plc:actor/social.coves.community.comment/livepost","cid":"bafyreply1","createdAt":"2026-09-20T09:04:00Z","excerpt":"Reply"}},
		{"reason":"commentReply","sortAt":"2026-09-20T09:03:00Z","isRead":false,"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot1","title":"Root"},"subject":{"uri":"at://did:plc:owner/social.coves.community.comment/parent","cid":"bafyparent1","preview":"Parent"},"record":{"uri":"at://did:plc:actor/social.coves.community.comment/livecomment","cid":"bafyreply2","createdAt":"2026-09-20T09:03:00Z","excerpt":"Reply"}},
		{"reason":"postReply","sortAt":"2026-09-20T09:02:00Z","isRead":true,"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot2","status":"deleted"},"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot2","status":"deleted"},"record":{"uri":"at://did:plc:actor/social.coves.community.comment/deleted","cid":"bafyreply3","createdAt":"2026-09-20T09:02:00Z","status":"deleted"}},
		{"reason":"commentReply","sortAt":"2026-09-20T09:01:00Z","isRead":true,"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/removed","cid":"bafyroot3","status":"removedByModerator"},"subject":{"uri":"at://did:plc:owner/social.coves.community.comment/deletedparent","cid":"bafyparent2","status":"deleted"},"record":{"uri":"at://did:plc:actor/social.coves.community.comment/removed","cid":"bafyreply4","createdAt":"2026-09-20T09:01:00Z","status":"removedByModerator"}}
	]}`
	data, err := atdata.UnmarshalJSON([]byte(fixture))
	require.NoError(t, err)
	data["$type"] = recordID
	require.NoError(t, lexicon.ValidateRecord(catalog, data, recordID, 0), "live and placeholder reply views must validate")
	// Indigo permits unknown properties; acceptance alone cannot establish that
	// the three placeholder statuses are part of the published output contract.
	items := output.Properties["notifications"].Inner.(lexicon.SchemaArray)
	viewDef, err := catalog.Resolve(items.Items.Inner.(lexicon.SchemaRef).Ref)
	require.NoError(t, err)
	view := viewDef.Def.(lexicon.SchemaObject)
	for _, field := range []string{"rootPost", "subject", "record"} {
		object := view.Properties[field].Inner.(lexicon.SchemaObject)
		require.Contains(t, object.Properties, "status", "%s.status must be declared so clients can consume placeholder replies", field)
	}
}

func TestNotificationListLexicon_PlaceholderStatusRejectsBoolean(t *testing.T) {
	catalog, recordID, _ := placeholderListLexicon(t)
	for _, field := range []string{"rootPost", "subject", "record"} {
		t.Run(field, func(t *testing.T) {
			const fixture = `{"notifications":[{"reason":"postReply","sortAt":"2026-09-20T09:00:00Z","isRead":false,"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot1","status":"deleted"},"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot1","status":"deleted"},"record":{"uri":"at://did:plc:actor/social.coves.community.comment/reply","cid":"bafyreply1","createdAt":"2026-09-20T09:00:00Z","status":"deleted"}}]}`
			data, err := atdata.UnmarshalJSON([]byte(fixture))
			require.NoError(t, err)
			data["$type"] = recordID
			row := data["notifications"].([]any)[0].(map[string]any)
			row[field].(map[string]any)["status"] = true
			require.Error(t, lexicon.ValidateRecord(catalog, data, recordID, 0), "%s.status must be a string", field)
		})
	}
}

func TestNotificationListLexicon_PlaceholderStatusKnownValuesNotEnum(t *testing.T) {
	catalog, _, output := placeholderListLexicon(t)
	items, ok := output.Properties["notifications"].Inner.(lexicon.SchemaArray)
	require.True(t, ok)
	itemRef, ok := items.Items.Inner.(lexicon.SchemaRef)
	require.True(t, ok)
	resolved, err := catalog.Resolve(itemRef.Ref)
	require.NoError(t, err)
	view, ok := resolved.Def.(lexicon.SchemaObject)
	require.True(t, ok)
	for _, field := range []string{"rootPost", "subject", "record"} {
		t.Run(field, func(t *testing.T) {
			object, ok := view.Properties[field].Inner.(lexicon.SchemaObject)
			require.True(t, ok)
			// Inspect the authored schema to distinguish extensible knownValues from enum.
			encoded, err := json.Marshal(object.Properties["status"])
			require.NoError(t, err)
			var status struct {
				Type        string   `json:"type"`
				KnownValues []string `json:"knownValues"`
				Enum        []string `json:"enum"`
			}
			require.NoError(t, json.Unmarshal(encoded, &status))
			require.Equal(t, "string", status.Type)
			require.ElementsMatch(t, []string{"deleted", "removedByModerator", "removedByServerAdmin"}, status.KnownValues)
			require.Empty(t, status.Enum, "status must allow future values")
		})
	}
}
