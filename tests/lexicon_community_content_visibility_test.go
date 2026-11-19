package tests

import (
	"testing"

	"Coves/internal/validation"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

// subscribeInputRecordID and subscribeOutputRecordID name the test-local
// records that wrap the inline input and output objects of
// social.coves.community.subscribe. Indigo validates records, not endpoint
// bodies, so each body is wrapped the same way
// lexicon_notification_preferences_test.go wraps putPreferences' input.
const (
	subscribeInputRecordID  = "test.coves.community.subscribeInput"
	subscribeOutputRecordID = "test.coves.community.subscribeOutput"
)

// validateSubscribeInput validates data against the published input object of
// social.coves.community.subscribe.
func validateSubscribeInput(t *testing.T, catalog *lexicon.BaseCatalog, data map[string]any) error {
	t.Helper()
	return validateSubscribeBody(t, catalog, subscribeInputRecordID, "input",
		func(procedure lexicon.SchemaProcedure) *lexicon.SchemaBody { return procedure.Input }, data)
}

// validateSubscribeOutput validates data against the published output object
// of social.coves.community.subscribe.
func validateSubscribeOutput(t *testing.T, catalog *lexicon.BaseCatalog, data map[string]any) error {
	t.Helper()
	return validateSubscribeBody(t, catalog, subscribeOutputRecordID, "output",
		func(procedure lexicon.SchemaProcedure) *lexicon.SchemaBody { return procedure.Output }, data)
}

func validateSubscribeBody(t *testing.T, catalog *lexicon.BaseCatalog, recordID, bodyName string, body func(lexicon.SchemaProcedure) *lexicon.SchemaBody, data map[string]any) error {
	t.Helper()
	if _, err := catalog.Resolve(recordID); err != nil {
		schema, err := catalog.Resolve("social.coves.community.subscribe")
		require.NoError(t, err, "social.coves.community.subscribe must resolve")
		procedure, ok := schema.Def.(lexicon.SchemaProcedure)
		require.True(t, ok, "social.coves.community.subscribe must be a procedure, got %T", schema.Def)
		declared := body(procedure)
		require.NotNil(t, declared, "subscribe declares no %s", bodyName)
		require.NotNil(t, declared.Schema, "subscribe %s has no schema", bodyName)
		object, ok := declared.Schema.Inner.(lexicon.SchemaObject)
		require.True(t, ok, "subscribe %s must be an inline object, got %T", bodyName, declared.Schema.Inner)
		require.NoError(t, catalog.AddSchemaFile(lexicon.SchemaFile{
			Lexicon: 1,
			ID:      recordID,
			Defs: map[string]lexicon.SchemaDef{
				"main": {Inner: lexicon.SchemaRecord{Type: "record", Key: "any", Record: object}},
			},
		}), "wrapping subscribe %s for record validation", bodyName)
	}
	record := make(map[string]any, len(data)+1)
	for key, value := range data {
		record[key] = value
	}
	record["$type"] = recordID
	return lexicon.ValidateRecord(catalog, record, recordID, 0)
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

// TestLexiconSubscriptionFavorite declares favorite as an optional boolean on
// every surface that carries it: the subscribe input, the detailed viewer
// state, and the list view's inline viewer object. The validator ignores
// undeclared properties, so only the rejected non-boolean row proves each
// surface declares the field.
func TestLexiconSubscriptionFavorite(t *testing.T) {
	catalog := loadFixtureCatalog(t)

	surfaces := []struct {
		name     string
		validate func(t *testing.T, favorite any) error
	}{
		{
			name: "social.coves.community.subscribe input",
			validate: func(t *testing.T, favorite any) error {
				data := map[string]any{"community": "did:plc:community123"}
				if favorite != nil {
					data["favorite"] = favorite
				}
				return validateSubscribeInput(t, catalog, data)
			},
		},
		{
			name: "social.coves.community.defs#viewerState",
			validate: func(t *testing.T, favorite any) error {
				data := map[string]any{"subscribed": true}
				if favorite != nil {
					data["favorite"] = favorite
				}
				return validation.ValidateData(catalog, data, "social.coves.community.defs#viewerState", 0)
			},
		},
		{
			name: "social.coves.community.defs#communityView viewer",
			validate: func(t *testing.T, favorite any) error {
				viewer := map[string]any{"subscribed": true}
				if favorite != nil {
					viewer["favorite"] = favorite
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
		name     string
		favorite any // nil = omitted
		valid    bool
	}{
		{name: "omitted", favorite: nil, valid: true},
		{name: "true", favorite: true, valid: true},
		{name: "false", favorite: false, valid: true},
		{name: "string", favorite: "yes", valid: false},
	}

	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					err := surface.validate(t, row.favorite)
					if row.valid {
						require.NoError(t, err, "favorite %v must be valid", row.favorite)
					} else {
						require.Error(t, err, "favorite %v must be rejected", row.favorite)
					}
				})
			}
		})
	}
}

// TestLexiconSubscribeOutputSettings requires favorite (boolean) and
// contentVisibility (integer 1..5) in the subscribe output: the response
// reports the settings the subscription holds after the call.
func TestLexiconSubscribeOutputSettings(t *testing.T) {
	catalog := loadFixtureCatalog(t)

	valid := func() map[string]any {
		return map[string]any{
			"uri":               "at://did:plc:testuser/social.coves.community.subscription/3kaaaaaaaaaa2",
			"cid":               "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
			"existing":          true,
			"favorite":          true,
			"contentVisibility": int64(4),
		}
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{name: "complete output", mutate: func(map[string]any) {}, valid: true},
		{name: "favorite missing", mutate: func(d map[string]any) { delete(d, "favorite") }},
		{name: "contentVisibility missing", mutate: func(d map[string]any) { delete(d, "contentVisibility") }},
		{name: "favorite not a boolean", mutate: func(d map[string]any) { d["favorite"] = "yes" }},
		{name: "contentVisibility 0", mutate: func(d map[string]any) { d["contentVisibility"] = int64(0) }},
		{name: "contentVisibility 6", mutate: func(d map[string]any) { d["contentVisibility"] = int64(6) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := valid()
			tt.mutate(data)
			err := validateSubscribeOutput(t, catalog, data)
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "the subscribe output schema must reject this body")
			}
		})
	}
}
