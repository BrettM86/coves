package tests

import (
	"testing"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

func TestNotificationUpdateSeenLexicon_Contract(t *testing.T) {
	const nsid = "social.coves.notification.updateSeen"
	doc := readPreferencesEndpoint(t, nsid)
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory(lexiconDir))
	resolved, err := catalog.Resolve(nsid)
	require.NoError(t, err)
	procedure, ok := resolved.Def.(lexicon.SchemaProcedure)
	require.True(t, ok, "%s must be a procedure, got %T", nsid, resolved.Def)
	require.NotNil(t, procedure.Input)
	require.Equal(t, "application/json", procedure.Input.Encoding)
	require.Nil(t, procedure.Output, "updateSeen must declare no output")
	var errorNames []string
	for _, declared := range procedure.Errors {
		errorNames = append(errorNames, declared.Name)
	}
	require.Contains(t, errorNames, "AccountNotIndexed")

	require.NotNil(t, doc.Defs.Main.Input.Schema)
	input, ok := doc.Defs.Main.Input.Schema.Inner.(lexicon.SchemaObject)
	require.True(t, ok, "%s input must be an object, got %T", nsid, doc.Defs.Main.Input.Schema.Inner)
	const recordID = "test.coves.notification.updateSeenInput"
	addPreferencesRecord(t, catalog, recordID, input)
	for _, tc := range []struct {
		name  string
		data  map[string]any
		valid bool
	}{
		{"valid datetime", map[string]any{"seenAt": "2026-09-30T12:34:56.123456Z"}, true},
		{"missing seenAt", map[string]any{}, false},
		{"invalid datetime", map[string]any{"seenAt": "yesterday"}, false},
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
}
