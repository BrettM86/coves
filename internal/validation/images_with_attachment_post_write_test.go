package validation

import (
	"testing"

	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const postWriteCompositeRef = "social.coves.embed.imagesWithAttachment"

func validationComposite(attachment string) map[string]any {
	branch := map[string]any{"$type": "social.coves.embed.post", "post": map[string]any{
		"uri": "at://did:plc:quotedauthor/social.coves.community.postv2/3m2quotedpostab", "cid": validTestCID,
	}}
	if attachment == "external" {
		branch = map[string]any{"$type": "social.coves.embed.external", "external": map[string]any{
			"uri": "https://news.example.com/story", "thumb": map[string]any{
				"$type": "blob", "ref": map[string]any{"$link": validTestCID}, "mimeType": "image/jpeg", "size": 1000,
			},
		}}
	}
	return map[string]any{
		"$type": postWriteCompositeRef,
		"images": map[string]any{"$type": "social.coves.embed.images", "images": []any{
			map[string]any{"image": map[string]any{"$type": "blob", "ref": map[string]any{"$link": validTestCID}, "mimeType": "image/png", "size": 2000}},
		}},
		"attachment": branch,
	}
}

// A synthetic record delegates to the actual procedure Input.Schema object;
// nested refs still resolve through the repository catalog. Procedure NSIDs
// themselves are not record schemas.
func checkCompositePostSchema(t *testing.T, surface string) {
	t.Helper()
	v, err := NewEmbeddedLexiconValidator(false)
	require.NoError(t, err)
	ref := "social.coves.community." + surface
	schema, err := v.catalog.Resolve(ref)
	require.NoError(t, err)
	var object lexicon.SchemaObject
	var validate func(map[string]any) error
	if surface == "postv2" {
		definition, ok := schema.Def.(lexicon.SchemaRecord)
		require.True(t, ok)
		object = definition.Record
		validate = func(data map[string]any) error { return v.ValidateRecord(data, ref) }
	} else {
		procedure, ok := schema.Def.(lexicon.SchemaProcedure)
		require.True(t, ok)
		require.NotNil(t, procedure.Input)
		require.NotNil(t, procedure.Input.Schema)
		object, ok = procedure.Input.Schema.Inner.(lexicon.SchemaObject)
		require.True(t, ok, "validate the procedure's actual input object")
		catalog := objectValidationCatalog{Catalog: v.catalog, record: lexicon.SchemaRecord{Type: "record", Key: "any", Record: object}}
		validate = func(data map[string]any) error {
			converted, err := toAtprotoData(data)
			if err != nil {
				return err
			}
			converted["$type"] = syntheticObjectRecordID
			return lexicon.ValidateRecord(catalog, converted, syntheticObjectRecordID, v.flags)
		}
	}
	union, ok := object.Properties["embed"].Inner.(lexicon.SchemaUnion)
	require.True(t, ok)
	assert.Contains(t, union.Refs, "social.coves.embed.imagesWithAttachment", "the embed union must explicitly declare the composite ref")
	for _, attachment := range []string{"quote", "external"} {
		t.Run(attachment, func(t *testing.T) {
			data := map[string]any{"title": "Harbour repairs", "embed": validationComposite(attachment)}
			switch surface {
			case "postv2":
				data["$type"], data["community"], data["createdAt"] = ref, "did:plc:community", "2026-10-01T09:00:00Z"
			case "post.create":
				data["community"] = "did:plc:community"
			case "post.update":
				data["uri"] = "at://did:plc:author/social.coves.community.postv2/3m2standingpost"
			}
			require.NoError(t, validate(data), "complete composites satisfy this schema surface")
			delete(data["embed"].(map[string]any), "attachment")
			assert.Error(t, validate(data), "a known composite missing its required attachment must fail")
		})
	}
}

func TestPostV2ImagesWithAttachmentSchema(t *testing.T) { checkCompositePostSchema(t, "postv2") }
func TestPostCreateImagesWithAttachmentInputSchema(t *testing.T) {
	checkCompositePostSchema(t, "post.create")
}

func TestPostUpdateImagesWithAttachmentInputSchema(t *testing.T) {
	checkCompositePostSchema(t, "post.update")
}
