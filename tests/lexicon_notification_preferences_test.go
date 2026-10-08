package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Coves/internal/api/handlers/notification"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

const preferencesDefinition = "social.coves.notification.defs#preferences"

type preferencesEndpointDocument struct {
	ID   string `json:"id"`
	Defs struct {
		Main struct {
			Input  lexicon.SchemaBody `json:"input"`
			Output lexicon.SchemaBody `json:"output"`
		} `json:"main"`
	} `json:"defs"`
}

func readPreferencesEndpoint(t *testing.T, nsid string) preferencesEndpointDocument {
	t.Helper()
	path := filepath.Join(lexiconDir, "social", "coves", "notification", strings.TrimPrefix(nsid, "social.coves.notification.")+".json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s lexicon at %s", nsid, path)
	var doc preferencesEndpointDocument
	require.NoError(t, json.Unmarshal(raw, &doc), "parsing %s lexicon", nsid)
	require.Equal(t, nsid, doc.ID, "%s lexicon ID", nsid)
	return doc
}

// Indigo validates records, not endpoint bodies. Wrap the endpoint's resolved
// output object in a temporary record, as unreadOutputCatalog does.
func preferencesOutputCatalog(t *testing.T, catalog *lexicon.BaseCatalog, doc preferencesEndpointDocument) (lexicon.SchemaObject, string) {
	t.Helper()
	require.NotNil(t, doc.Defs.Main.Output.Schema, "%s output schema", doc.ID)
	ref, ok := doc.Defs.Main.Output.Schema.Inner.(lexicon.SchemaRef)
	require.True(t, ok, "%s output must reference %s", doc.ID, preferencesDefinition)
	resolvedRef := ref.Ref
	if strings.HasPrefix(resolvedRef, "#") {
		resolvedRef = doc.ID + resolvedRef
	} else {
		require.Equal(t, preferencesDefinition, resolvedRef, "%s output reference", doc.ID)
	}
	resolved, err := catalog.Resolve(resolvedRef)
	require.NoError(t, err, "resolving %s output reference %s", doc.ID, resolvedRef)
	canonical, err := catalog.Resolve(preferencesDefinition)
	require.NoError(t, err, "resolving %s", preferencesDefinition)
	require.Equal(t, canonical.Def, resolved.Def, "%s output must resolve to the preferences definition", doc.ID)
	object, ok := resolved.Def.(lexicon.SchemaObject)
	require.True(t, ok, "%s output reference must resolve to an object, got %T", doc.ID, resolved.Def)
	recordID := "test.coves.notification." + strings.TrimPrefix(doc.ID, "social.coves.notification.") + "Output"
	addPreferencesRecord(t, catalog, recordID, object)
	return object, recordID
}

func addPreferencesRecord(t *testing.T, catalog *lexicon.BaseCatalog, id string, object lexicon.SchemaObject) {
	t.Helper()
	require.NoError(t, catalog.AddSchemaFile(lexicon.SchemaFile{
		Lexicon: 1,
		ID:      id,
		Defs: map[string]lexicon.SchemaDef{
			"main": {Inner: lexicon.SchemaRecord{Type: "record", Key: "any", Record: object}},
		},
	}), "wrapping %s for record validation", id)
}

func TestNotificationPreferencesLexicon_Contract(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory(lexiconDir))

	for _, endpoint := range []struct {
		nsid  string
		query bool
	}{
		{"social.coves.notification.getPreferences", true},
		{"social.coves.notification.putPreferences", false},
	} {
		t.Run(endpoint.nsid, func(t *testing.T) {
			method, err := catalog.Resolve(endpoint.nsid)
			require.NoError(t, err, "%s lexicon must resolve", endpoint.nsid)
			if endpoint.query {
				_, ok := method.Def.(lexicon.SchemaQuery)
				require.True(t, ok, "%s must be a query, got %T", endpoint.nsid, method.Def)
			} else {
				_, ok := method.Def.(lexicon.SchemaProcedure)
				require.True(t, ok, "%s must be a procedure, got %T", endpoint.nsid, method.Def)
			}

			doc := readPreferencesEndpoint(t, endpoint.nsid)
			output, recordID := preferencesOutputCatalog(t, catalog, doc)
			properties := make(map[string]any, len(output.Properties))
			for key, property := range output.Properties {
				properties[key] = property
			}
			assertResponseShapeMatches(t, properties, notification.PreferencesResponse{})

			for _, tc := range []struct {
				name  string
				data  map[string]any
				valid bool
			}{
				{"all booleans", map[string]any{"postReply": true, "commentReply": false, "mention": true, "upvote": false}, true},
				{"missing upvote", map[string]any{"postReply": true, "commentReply": false, "mention": true}, false},
				{"string mention", map[string]any{"postReply": true, "commentReply": false, "mention": "true", "upvote": false}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tc.data["$type"] = recordID
					err := lexicon.ValidateRecord(catalog, tc.data, recordID, 0)
					if tc.valid {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}
				})
			}

			raw, err := json.Marshal(notification.PreferencesResponse{PostReply: true, CommentReply: false, Mention: true, Upvote: false})
			require.NoError(t, err)
			decoded, err := atdata.UnmarshalJSON(raw)
			require.NoError(t, err)
			decoded["$type"] = recordID
			require.NoError(t, lexicon.ValidateRecord(catalog, decoded, recordID, 0),
				"%s: emitted Go response must validate against the output lexicon", endpoint.nsid)

			if endpoint.query {
				return
			}
			require.NotNil(t, doc.Defs.Main.Input.Schema, "%s input schema", endpoint.nsid)
			input, ok := doc.Defs.Main.Input.Schema.Inner.(lexicon.SchemaObject)
			require.True(t, ok, "%s input must be an inline object, got %T", endpoint.nsid, doc.Defs.Main.Input.Schema.Inner)
			const inputRecordID = "test.coves.notification.putPreferencesInput"
			addPreferencesRecord(t, catalog, inputRecordID, input)
			for _, tc := range []struct {
				name  string
				data  map[string]any
				valid bool
			}{
				{"one preference", map[string]any{"mention": false}, true},
				{"empty update", map[string]any{}, true},
				{"string mention", map[string]any{"mention": "false"}, false},
			} {
				t.Run("input "+tc.name, func(t *testing.T) {
					tc.data["$type"] = inputRecordID
					err := lexicon.ValidateRecord(catalog, tc.data, inputRecordID, 0)
					if tc.valid {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}
				})
			}
		})
	}
}
