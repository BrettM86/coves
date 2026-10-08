package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"Coves/internal/api/handlers/notification"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const unreadOutputRecordID = "test.coves.notification.getUnreadCountOutput"

// Indigo validates records but has no public object validator. Wrap the query's
// actual output object in a temporary record definition in the loaded catalog.
func unreadOutputCatalog(t *testing.T, catalog *lexicon.BaseCatalog) {
	t.Helper()
	path := filepath.Join(lexiconDir, "social", "coves", "notification", "getUnreadCount.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading getUnreadCount lexicon")
	var doc struct {
		Defs struct {
			Main struct {
				Output struct {
					Schema lexicon.SchemaObject `json:"schema"`
				} `json:"output"`
			} `json:"main"`
		} `json:"defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, "object", doc.Defs.Main.Output.Schema.Type, "getUnreadCount must declare an output object")
	properties := make(map[string]any, len(doc.Defs.Main.Output.Schema.Properties))
	for key, value := range doc.Defs.Main.Output.Schema.Properties {
		properties[key] = value
	}
	assertResponseShapeMatches(t, properties, notification.UnreadCountResponse{})
	require.NoError(t, catalog.AddSchemaFile(lexicon.SchemaFile{
		Lexicon: 1,
		ID:      unreadOutputRecordID,
		Defs: map[string]lexicon.SchemaDef{
			"main": {Inner: lexicon.SchemaRecord{
				Type: "record", Key: "any", Record: doc.Defs.Main.Output.Schema,
			}},
		},
	}))
}

func TestNotificationLexicon_Contract(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory(lexiconDir))

	t.Run("query", func(t *testing.T) {
		query, err := catalog.Resolve("social.coves.notification.getUnreadCount")
		require.NoError(t, err)
		_, ok := query.Def.(lexicon.SchemaQuery)
		require.True(t, ok, "getUnreadCount must be a query, got %T", query.Def)
	})

	t.Run("open reason vocabulary", func(t *testing.T) {
		reason, err := catalog.Resolve("social.coves.notification.defs#reason")
		require.NoError(t, err)
		stringReason, ok := reason.Def.(lexicon.SchemaString)
		require.True(t, ok, "reason must be a string, got %T", reason.Def)
		assert.ElementsMatch(t, []string{"postReply", "commentReply", "mention", "upvote"}, stringReason.KnownValues)
		assert.Empty(t, stringReason.Enum, "reason vocabulary must remain open")
	})

	t.Run("output accepts only bounded required integer count", func(t *testing.T) {
		unreadOutputCatalog(t, catalog)
		for _, tc := range []struct {
			name  string
			data  map[string]any
			valid bool
		}{
			{"zero", map[string]any{"count": int64(0)}, true},
			{"maximum", map[string]any{"count": int64(101)}, true},
			{"above maximum", map[string]any{"count": int64(102)}, false},
			{"negative", map[string]any{"count": int64(-1)}, false},
			{"string", map[string]any{"count": "3"}, false},
			{"missing", map[string]any{}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tc.data["$type"] = unreadOutputRecordID
				err := lexicon.ValidateRecord(catalog, tc.data, unreadOutputRecordID, 0)
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}

		raw, err := json.Marshal(notification.UnreadCountResponse{Count: 3})
		require.NoError(t, err)
		decoded, err := atdata.UnmarshalJSON(raw)
		require.NoError(t, err)
		decoded["$type"] = unreadOutputRecordID
		require.NoError(t, lexicon.ValidateRecord(catalog, decoded, unreadOutputRecordID, 0),
			"Go's emitted JSON must validate against the output lexicon")
	})
}
