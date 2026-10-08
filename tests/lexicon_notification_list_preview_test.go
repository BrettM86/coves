package tests

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

func TestNotificationListLexicon_PreviewContract(t *testing.T) {
	catalog, recordID, output := placeholderListLexicon(t)
	items, ok := output.Properties["notifications"].Inner.(lexicon.SchemaArray)
	require.True(t, ok)
	viewDef, err := catalog.Resolve(items.Items.Inner.(lexicon.SchemaRef).Ref)
	require.NoError(t, err)
	view := viewDef.Def.(lexicon.SchemaObject)
	for _, reference := range []string{"rootPost", "subject", "record"} {
		t.Run(reference+" declarations", func(t *testing.T) {
			object := view.Properties[reference].Inner.(lexicon.SchemaObject)
			for _, field := range []struct{ name, declaration string }{
				{"labels", `{"type":"ref","ref":"com.atproto.label.defs#selfLabels"}`},
				{"thumbnail", `{"type":"string","format":"uri"}`},
				{"thumbnailAlt", `{"type":"string","maxLength":10000,"maxGraphemes":1000}`},
			} {
				require.NotContains(t, object.Required, field.name, "%s.%s must be optional", reference, field.name)
				property, exists := object.Properties[field.name]
				require.True(t, exists, "%s.%s must be declared", reference, field.name)
				encoded, err := json.Marshal(property)
				require.NoError(t, err)
				var declaration map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(encoded, &declaration))
				delete(declaration, "description")
				actual, err := json.Marshal(declaration)
				require.NoError(t, err)
				require.JSONEq(t, field.declaration, string(actual), reference+"."+field.name)
			}
		})
	}
	// These remain separate output rows: a post reply carries the post subject,
	// while a post mention carries its post in record, and placeholders carry none.
	fixture := `{"notifications":[` +
		`{"reason":"postReply","sortAt":"2026-09-20T09:02:00Z","isRead":false,` +
		`"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot",` +
		`"labels":{"values":[{"val":"unknown-value"},{"val":"` + strings.Repeat("é", 64) + `"},{"val":"nsfw","neg":true}]},` +
		`"thumbnail":"https://img.example.test/root","thumbnailAlt":"` + strings.Repeat("👨‍👩‍👧‍👦", 400) + `"},` +
		`"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/subject","cid":"bafysubject",` +
		`"labels":{"values":[{"val":"unknown-value"},{"val":"` + strings.Repeat("é", 64) + `"},{"val":"nsfw","neg":true}]},` +
		`"thumbnail":"https://img.example.test/subject","thumbnailAlt":"` + strings.Repeat("👨‍👩‍👧‍👦", 400) + `"},` +
		`"record":{"uri":"at://did:plc:actor/social.coves.community.comment/reply","cid":"bafyreply","createdAt":"2026-09-20T09:02:00Z"}},` +
		`{"reason":"mention","sortAt":"2026-09-20T09:01:00Z","isRead":false,` +
		`"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot","status":"deleted"},` +
		`"record":{"uri":"at://did:plc:actor/social.coves.community.postv2/mention","cid":"bafymention","createdAt":"2026-09-20T09:01:00Z",` +
		`"labels":{"values":[{"val":"unknown-value"},{"val":"` + strings.Repeat("é", 64) + `"},{"val":"nsfw","neg":true}]},` +
		`"thumbnail":"https://img.example.test/mention","thumbnailAlt":"` + strings.Repeat("👨‍👩‍👧‍👦", 400) + `"}},` +
		`{"reason":"postReply","sortAt":"2026-09-20T09:00:00Z","isRead":true,` +
		`"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot","status":"removedByModerator"},` +
		`"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/subject","cid":"bafysubject","status":"deleted"},` +
		`"record":{"uri":"at://did:plc:actor/social.coves.community.comment/reply","cid":"bafyreply","createdAt":"2026-09-20T09:00:00Z","status":"deleted"}}]}`
	readFixture := func(t *testing.T) map[string]any {
		t.Helper()
		data, err := atdata.UnmarshalJSON([]byte(fixture))
		require.NoError(t, err)
		data["$type"] = recordID
		return data
	}
	t.Run("schema-valid previews and empty placeholders", func(t *testing.T) {
		require.NoError(t, lexicon.ValidateRecord(catalog, readFixture(t), recordID, 0))
	})
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"eleven self-labels", "labels", map[string]any{"values": []any{
			map[string]any{"val": "one"}, map[string]any{"val": "two"}, map[string]any{"val": "three"}, map[string]any{"val": "four"}, map[string]any{"val": "five"}, map[string]any{"val": "six"}, map[string]any{"val": "seven"}, map[string]any{"val": "eight"}, map[string]any{"val": "nine"}, map[string]any{"val": "ten"}, map[string]any{"val": "eleven"}}}},
		{"129-byte label", "labels", map[string]any{"values": []any{map[string]any{"val": strings.Repeat("é", 64) + "a"}}}},
		{"invalid thumbnail URI", "thumbnail", "not a URI"},
		{"1001 ASCII alt", "thumbnailAlt", strings.Repeat("a", 1001)},
		{"401 family-emoji clusters and 10025 bytes", "thumbnailAlt", strings.Repeat("👨‍👩‍👧‍👦", 401)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := readFixture(t)
			row := data["notifications"].([]any)[0].(map[string]any)
			row["rootPost"].(map[string]any)[tc.field] = tc.value
			require.Error(t, lexicon.ValidateRecord(catalog, data, recordID, 0), "rootPost.%s must reject %s", tc.field, tc.name)
		})
	}
}
