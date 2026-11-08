package tests

import (
	"testing"

	"Coves/internal/validation"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

// subscribeInputRecordID names the test-local record that wraps the inline
// input object of social.coves.community.subscribe. Indigo validates records,
// not endpoint bodies, so the procedure's input is wrapped the same way
// lexicon_notification_preferences_test.go wraps putPreferences' input.
const subscribeInputRecordID = "test.coves.community.subscribeInput"

// validateSubscribeInput validates data against the published input object of
// social.coves.community.subscribe.
func validateSubscribeInput(t *testing.T, catalog *lexicon.BaseCatalog, data map[string]any) error {
	t.Helper()
	if _, err := catalog.Resolve(subscribeInputRecordID); err != nil {
		schema, err := catalog.Resolve("social.coves.community.subscribe")
		require.NoError(t, err, "social.coves.community.subscribe must resolve")
		procedure, ok := schema.Def.(lexicon.SchemaProcedure)
		require.True(t, ok, "social.coves.community.subscribe must be a procedure, got %T", schema.Def)
		require.NotNil(t, procedure.Input, "subscribe declares no input")
		require.NotNil(t, procedure.Input.Schema, "subscribe input has no schema")
		input, ok := procedure.Input.Schema.Inner.(lexicon.SchemaObject)
		require.True(t, ok, "subscribe input must be an inline object, got %T", procedure.Input.Schema.Inner)
		require.NoError(t, catalog.AddSchemaFile(lexicon.SchemaFile{
			Lexicon: 1,
			ID:      subscribeInputRecordID,
			Defs: map[string]lexicon.SchemaDef{
				"main": {Inner: lexicon.SchemaRecord{Type: "record", Key: "any", Record: input}},
			},
		}), "wrapping subscribe input for record validation")
	}
	record := make(map[string]any, len(data)+1)
	for key, value := range data {
		record[key] = value
	}
	record["$type"] = subscribeInputRecordID
	return lexicon.ValidateRecord(catalog, record, subscribeInputRecordID, 0)
}

// TestLexiconSubscriptionContentVisibilityBounds pins contentVisibility to
// 1..5 (optional) on every surface that carries it: the subscribe input, the
// detailed viewer state, and the list view's inline viewer object.
func TestLexiconSubscriptionContentVisibilityBounds(t *testing.T) {
	catalog := loadFixtureCatalog(t)

	surfaces := []struct {
		name     string
		validate func(t *testing.T, contentVisibility any) error
	}{
		{
			name: "social.coves.community.subscribe input",
			validate: func(t *testing.T, contentVisibility any) error {
				data := map[string]any{"community": "did:plc:community123"}
				if contentVisibility != nil {
					data["contentVisibility"] = contentVisibility
				}
				return validateSubscribeInput(t, catalog, data)
			},
		},
		{
			name: "social.coves.community.defs#viewerState",
			validate: func(t *testing.T, contentVisibility any) error {
				data := map[string]any{"subscribed": true}
				if contentVisibility != nil {
					data["contentVisibility"] = contentVisibility
				}
				return validation.ValidateData(catalog, data, "social.coves.community.defs#viewerState", 0)
			},
		},
		{
			name: "social.coves.community.defs#communityView viewer",
			validate: func(t *testing.T, contentVisibility any) error {
				viewer := map[string]any{"subscribed": true}
				if contentVisibility != nil {
					viewer["contentVisibility"] = contentVisibility
				}
				data := map[string]any{
					"did":    "did:plc:community123",
					"name":   "gaming",
					"viewer": viewer,
				}
				return validation.ValidateData(catalog, data, "social.coves.community.defs#communityView", 0)
			},
		},
	}

	rows := []struct {
		name              string
		contentVisibility any // nil = omitted
		valid             bool
	}{
		{name: "omitted", contentVisibility: nil, valid: true},
		{name: "1", contentVisibility: int64(1), valid: true},
		{name: "5", contentVisibility: int64(5), valid: true},
		{name: "0", contentVisibility: int64(0), valid: false},
		{name: "6", contentVisibility: int64(6), valid: false},
	}

	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					err := surface.validate(t, row.contentVisibility)
					if row.valid {
						require.NoError(t, err, "contentVisibility %v must be valid", row.contentVisibility)
					} else {
						require.Error(t, err, "contentVisibility %v must be rejected", row.contentVisibility)
					}
				})
			}
		})
	}
}
