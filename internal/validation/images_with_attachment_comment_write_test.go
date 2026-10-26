package validation

import (
	"testing"

	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkCompositeCommentSchema(t *testing.T, surface string) {
	t.Helper()
	validator, err := NewEmbeddedLexiconValidator(false)
	require.NoError(t, err)
	reference := "social.coves.community." + surface
	schema, err := validator.catalog.Resolve(reference)
	require.NoError(t, err)
	var object lexicon.SchemaObject
	var validate func(map[string]any) error
	if surface == "comment" {
		record, ok := schema.Def.(lexicon.SchemaRecord)
		require.True(t, ok)
		object = record.Record
		validate = func(data map[string]any) error { return validator.ValidateRecord(data, reference) }
	} else {
		procedure, ok := schema.Def.(lexicon.SchemaProcedure)
		require.True(t, ok)
		require.NotNil(t, procedure.Input)
		require.NotNil(t, procedure.Input.Schema)
		object, ok = procedure.Input.Schema.Inner.(lexicon.SchemaObject)
		require.True(t, ok, "validate the actual procedure Input.Schema")
		catalog := objectValidationCatalog{Catalog: validator.catalog, record: lexicon.SchemaRecord{Type: "record", Key: "any", Record: object}}
		validate = func(data map[string]any) error {
			converted, err := toAtprotoData(data)
			if err != nil {
				return err
			}
			converted["$type"] = syntheticObjectRecordID
			return lexicon.ValidateRecord(catalog, converted, syntheticObjectRecordID, validator.flags)
		}
	}
	union, ok := object.Properties["embed"].Inner.(lexicon.SchemaUnion)
	require.True(t, ok)
	assert.Contains(t, union.Refs, "social.coves.embed.imagesWithAttachment", "the embed union must explicitly declare the composite ref")
	data := map[string]any{"content": "Harbour photos", "embed": validationComposite("quote")}
	if surface == "comment.update" {
		data["uri"] = "at://did:plc:author/social.coves.community.comment/3m2standingcomment"
	} else {
		strongReference := map[string]any{"uri": "at://did:plc:author/social.coves.community.postv2/3m2standingpost", "cid": validTestCID}
		data["reply"] = map[string]any{"root": strongReference, "parent": strongReference}
	}
	if surface == "comment" {
		data["$type"], data["createdAt"] = reference, "2026-10-01T09:00:00Z"
	}
	require.NoError(t, validate(data), "a quoted composite satisfies the schema")
	delete(data["embed"].(map[string]any), "attachment")
	assert.Error(t, validate(data), "a known composite missing its required attachment must fail")
}

func TestCommentImagesWithAttachmentRecordSchema(t *testing.T) {
	checkCompositeCommentSchema(t, "comment")
}

func TestCommentImagesWithAttachmentInputSchema(t *testing.T) {
	for _, surface := range []string{"comment.create", "comment.update"} {
		t.Run(surface, func(t *testing.T) { checkCompositeCommentSchema(t, surface) })
	}
}
