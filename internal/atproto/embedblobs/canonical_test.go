package embedblobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	typeImage       = "social.coves.embed.images#image"
	typeAspectRatio = "social.coves.embed.images#aspectRatio"
	typeExternalObj = "social.coves.embed.external#external"
	typeSource      = "social.coves.embed.external#source"
	typeStrongRef   = "com.atproto.repo.strongRef"
	typePost        = "social.coves.embed.post"

	quotedURI = "at://did:plc:quoted/social.coves.community.postv2/3kquoted"
)

// deepCopy copies the JSON-shaped maps and slices of value so a test can
// prove its input was left untouched.
func deepCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, entry := range typed {
			copied[key] = deepCopy(entry)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for i, entry := range typed {
			copied[i] = deepCopy(entry)
		}
		return copied
	default:
		return value
	}
}

func untypedGallery(entries ...any) map[string]any {
	return map[string]any{"images": entries}
}

func quoteAttachment() map[string]any {
	return map[string]any{"$type": typePost, "post": map[string]any{"uri": quotedURI, "cid": firstCID}}
}

func externalAttachment() map[string]any {
	return map[string]any{
		"$type": TypeExternal,
		"external": map[string]any{
			"uri":    "https://example.com/article",
			"thumb":  testBlob(secondCID),
			"images": []any{testImage(testBlob(firstCID))},
			"sources": []any{map[string]any{
				"uri":        "https://example.com/source",
				"sourcePost": map[string]any{"uri": quotedURI, "cid": firstCID},
			}},
		},
	}
}

func composite(gallery, attachment any) map[string]any {
	return map[string]any{"$type": TypeImagesWithAttachment, "images": gallery, "attachment": attachment}
}

func TestCanonicalizeEmbedFillsOnlyTheGalleryType(t *testing.T) {
	typedComposite := composite(testImages(testImage(testBlob(firstCID))), externalAttachment())
	tests := []struct {
		name  string
		embed map[string]any
		want  map[string]any
	}{
		{
			name:  "untyped gallery with quote attachment gets the images type",
			embed: composite(untypedGallery(testImage(testBlob(firstCID))), quoteAttachment()),
			want:  composite(testImages(testImage(testBlob(firstCID))), quoteAttachment()),
		},
		{
			name:  "untyped gallery with external attachment gets the images type and nothing else",
			embed: composite(untypedGallery(testImage(testBlob(firstCID))), externalAttachment()),
			want:  composite(testImages(testImage(testBlob(firstCID))), externalAttachment()),
		},
		{
			name: "untyped union attachment is not filled",
			embed: composite(untypedGallery(testImage(testBlob(firstCID))),
				map[string]any{"post": map[string]any{"uri": quotedURI, "cid": firstCID}}),
			want: composite(testImages(testImage(testBlob(firstCID))),
				map[string]any{"post": map[string]any{"uri": quotedURI, "cid": firstCID}}),
		},
		{name: "typed composite is unchanged", embed: typedComposite, want: deepCopy(typedComposite).(map[string]any)},
		{
			name:  "fully typed nested refs are unchanged",
			embed: composite(testImages(map[string]any{"$type": typeImage, "image": testBlob(firstCID), "aspectRatio": map[string]any{"$type": typeAspectRatio, "width": 4.0, "height": 3.0}}), quoteAttachment()),
			want:  composite(testImages(map[string]any{"$type": typeImage, "image": testBlob(firstCID), "aspectRatio": map[string]any{"$type": typeAspectRatio, "width": 4.0, "height": 3.0}}), quoteAttachment()),
		},
		{
			name:  "untyped top-level embed is not filled",
			embed: map[string]any{"images": []any{testImage(testBlob(firstCID))}},
			want:  map[string]any{"images": []any{testImage(testBlob(firstCID))}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := deepCopy(tt.embed)
			got, err := CanonicalizeEmbed(tt.embed)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, before, tt.embed, "input must not be mutated")
		})
	}

	t.Run("nil input", func(t *testing.T) {
		got, err := CanonicalizeEmbed(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

func TestCanonicalizeEmbedReturnsAMapIndependentOfItsInput(t *testing.T) {
	embed := composite(untypedGallery(testImage(testBlob(firstCID))), quoteAttachment())
	before := deepCopy(embed)

	got, err := CanonicalizeEmbed(embed)
	require.NoError(t, err)
	gallery, ok := got["images"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, TypeImages, gallery["$type"])

	got["$type"] = "changed"
	gallery["changed"] = true
	got["attachment"].(map[string]any)["changed"] = true
	assert.Equal(t, before, embed, "writes to the result must not reach the input")
}

// wrongRefCase places value as the $type at one non-union ref position.
type wrongRefCase struct {
	name     string
	position RefPosition
	build    func(value any) map[string]any
}

func wrongRefCases() []wrongRefCase {
	galleryPosition := RefPosition{Lexicon: TypeImagesWithAttachment, Def: "main", Property: "images", Type: TypeImages}
	imagePosition := RefPosition{Lexicon: TypeImages, Def: "main", Property: "images", Array: true, Type: typeImage}
	aspectPosition := RefPosition{Lexicon: TypeImages, Def: "image", Property: "aspectRatio", Type: typeAspectRatio}
	externalPosition := RefPosition{Lexicon: TypeExternal, Def: "main", Property: "external", Type: typeExternalObj}
	externalImagePosition := RefPosition{Lexicon: TypeExternal, Def: "external", Property: "images", Array: true, Type: typeImage}
	sourcePosition := RefPosition{Lexicon: TypeExternal, Def: "external", Property: "sources", Array: true, Type: typeSource}
	sourcePostPosition := RefPosition{Lexicon: TypeExternal, Def: "source", Property: "sourcePost", Type: typeStrongRef}
	postPosition := RefPosition{Lexicon: typePost, Def: "main", Property: "post", Type: typeStrongRef}

	external := func(external map[string]any) map[string]any {
		external["uri"] = "https://example.com/article"
		return map[string]any{"$type": TypeExternal, "external": external}
	}
	withType := func(object map[string]any, value any) map[string]any {
		object["$type"] = value
		return object
	}
	return []wrongRefCase{
		{"composite gallery", galleryPosition, func(v any) map[string]any {
			return composite(withType(untypedGallery(testImage(testBlob(firstCID))), v), quoteAttachment())
		}},
		{"images entry", imagePosition, func(v any) map[string]any {
			return testImages(withType(testImage(testBlob(firstCID)), v))
		}},
		{"composite gallery image entry", imagePosition, func(v any) map[string]any {
			return composite(untypedGallery(withType(testImage(testBlob(firstCID)), v)), quoteAttachment())
		}},
		{"image aspectRatio", aspectPosition, func(v any) map[string]any {
			image := testImage(testBlob(firstCID))
			image["aspectRatio"] = withType(map[string]any{"width": 4.0, "height": 3.0}, v)
			return testImages(image)
		}},
		{"composite gallery image aspectRatio", aspectPosition, func(v any) map[string]any {
			image := testImage(testBlob(firstCID))
			image["aspectRatio"] = withType(map[string]any{"width": 4.0, "height": 3.0}, v)
			return composite(testImages(image), quoteAttachment())
		}},
		{"external.external", externalPosition, func(v any) map[string]any {
			return external(withType(map[string]any{}, v))
		}},
		{"composite external attachment external", externalPosition, func(v any) map[string]any {
			return composite(testImages(testImage(testBlob(firstCID))), external(withType(map[string]any{}, v)))
		}},
		{"external image", externalImagePosition, func(v any) map[string]any {
			return external(map[string]any{"images": []any{withType(testImage(testBlob(firstCID)), v)}})
		}},
		{"composite external attachment image", externalImagePosition, func(v any) map[string]any {
			return composite(testImages(testImage(testBlob(firstCID))),
				external(map[string]any{"images": []any{withType(testImage(testBlob(firstCID)), v)}}))
		}},
		{"external source", sourcePosition, func(v any) map[string]any {
			return external(map[string]any{"sources": []any{withType(map[string]any{"uri": "https://example.com/source"}, v)}})
		}},
		{"external source sourcePost", sourcePostPosition, func(v any) map[string]any {
			source := map[string]any{"uri": "https://example.com/source", "sourcePost": withType(map[string]any{"uri": quotedURI, "cid": firstCID}, v)}
			return external(map[string]any{"sources": []any{source}})
		}},
		{"composite external attachment sources[0].sourcePost", sourcePostPosition, func(v any) map[string]any {
			source := map[string]any{"uri": "https://example.com/source", "sourcePost": withType(map[string]any{"uri": quotedURI, "cid": firstCID}, v)}
			return composite(testImages(testImage(testBlob(firstCID))), external(map[string]any{"sources": []any{source}}))
		}},
		{"post.post", postPosition, func(v any) map[string]any {
			return map[string]any{"$type": typePost, "post": withType(map[string]any{"uri": quotedURI, "cid": firstCID}, v)}
		}},
		{"composite post attachment post", postPosition, func(v any) map[string]any {
			return composite(testImages(testImage(testBlob(firstCID))),
				map[string]any{"$type": typePost, "post": withType(map[string]any{"uri": quotedURI, "cid": firstCID}, v)})
		}},
	}
}

func TestCanonicalizeEmbedRejectsWrongRefType(t *testing.T) {
	wrongValues := []struct {
		name  string
		value any
	}{
		{"another NSID", TypeVideo},
		{"a number", 42.0},
		{"null", nil},
	}
	covered := make(map[RefPosition]bool)
	for _, tc := range wrongRefCases() {
		covered[tc.position] = true
		for _, wrong := range wrongValues {
			t.Run(tc.name+" "+wrong.name, func(t *testing.T) {
				embed := tc.build(wrong.value)
				before := deepCopy(embed)
				_, err := CanonicalizeEmbed(embed)
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrEmbedRefTypeMismatch), "got %v", err)
				assert.Equal(t, before, embed, "input must not be mutated")
			})
		}
	}

	t.Run("every ref position has a wrong-type case", func(t *testing.T) {
		for _, position := range RefPositions() {
			assert.True(t, covered[position], "no wrong-$type case for %+v", position)
		}
		for _, position := range lexiconRefPositions(t) {
			assert.True(t, covered[position], "no wrong-$type case for lexicon position %+v", position)
		}
	})
}

func TestMainGallery(t *testing.T) {
	images := testImages(testImage(testBlob(firstCID)))
	typedGallery := testImages(testImage(testBlob(firstCID)))
	bareGallery := untypedGallery(testImage(testBlob(firstCID)))
	withGalleryType := func(value any) map[string]any {
		gallery := untypedGallery(testImage(testBlob(firstCID)))
		gallery["$type"] = value
		return composite(gallery, quoteAttachment())
	}
	tests := []struct {
		name   string
		embed  map[string]any
		want   map[string]any
		wantOK bool
	}{
		{name: "images embed is its own gallery", embed: images, want: images, wantOK: true},
		{name: "composite typed gallery", embed: composite(typedGallery, quoteAttachment()), want: typedGallery, wantOK: true},
		{name: "composite untyped gallery", embed: composite(bareGallery, externalAttachment()), want: bareGallery, wantOK: true},
		{name: "composite gallery $type null", embed: withGalleryType(nil)},
		{name: "composite gallery $type number", embed: withGalleryType(42.0)},
		{name: "composite gallery $type another NSID", embed: withGalleryType(TypeVideo)},
		{name: "composite non-object images", embed: composite([]any{testImage(testBlob(firstCID))}, quoteAttachment())},
		{name: "external embed", embed: externalAttachment()},
		{name: "quote embed", embed: quoteAttachment()},
		{name: "nil embed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MainGallery(tt.embed)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// TestReadersUseOnlyTheMainGallery pins that an untyped composite gallery is
// read as the gallery, and a gallery whose $type is present but not the
// images type contributes nothing.
func TestReadersUseOnlyTheMainGallery(t *testing.T) {
	withGalleryType := func(value any, present bool) map[string]any {
		gallery := untypedGallery(testImage(testBlob(firstCID)), testImage(testBlob(secondCID)))
		if present {
			gallery["$type"] = value
		}
		return composite(gallery, externalAttachment())
	}
	attachmentOnly := []Reference{
		{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", KindExternalThumb},
		{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", KindPostImage},
	}
	tests := []struct {
		name           string
		embed          map[string]any
		wantPost       []Reference
		wantComment    []Reference
		wantImageCount int
	}{
		{
			name:  "untyped gallery",
			embed: withGalleryType(nil, false),
			wantPost: []Reference{
				{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", KindPostImage},
				{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", KindPostImage},
				{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", KindExternalThumb},
			},
			wantComment: []Reference{
				{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", KindCommentImage},
				{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", KindCommentImage},
			},
			wantImageCount: 3,
		},
		{name: "gallery $type null", embed: withGalleryType(nil, true), wantPost: attachmentOnly, wantImageCount: 1},
		{name: "gallery $type number", embed: withGalleryType(42.0, true), wantPost: attachmentOnly, wantImageCount: 1},
		{name: "gallery $type another NSID", embed: withGalleryType(TypeVideo, true), wantPost: attachmentOnly, wantImageCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantPost, PostReferences(tt.embed))
			assert.Equal(t, tt.wantComment, CommentReferences(tt.embed))
			assert.Equal(t, tt.wantImageCount, ImageCount(tt.embed))
		})
	}
}

// lexiconRefPositions walks the embed lexicons for every non-union ref
// position in their record (non-view) defs: an object property that is a ref,
// or an array whose items are a ref.
func lexiconRefPositions(t *testing.T) []RefPosition {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "lexicon", "social", "coves", "embed", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	type schema struct {
		Type  string  `json:"type"`
		Ref   string  `json:"ref"`
		Items *schema `json:"items"`
	}
	var positions []RefPosition
	for _, file := range files {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		var document struct {
			ID   string `json:"id"`
			Defs map[string]struct {
				Type       string            `json:"type"`
				Properties map[string]schema `json:"properties"`
			} `json:"defs"`
		}
		require.NoError(t, json.Unmarshal(raw, &document), file)
		resolve := func(ref string) string {
			if strings.HasPrefix(ref, "#") {
				ref = document.ID + ref
			}
			return strings.TrimSuffix(ref, "#main")
		}
		for defName, def := range document.Defs {
			if strings.HasPrefix(defName, "view") || def.Type != "object" {
				continue
			}
			for property, field := range def.Properties {
				switch {
				case field.Type == "ref":
					positions = append(positions, RefPosition{Lexicon: document.ID, Def: defName, Property: property, Type: resolve(field.Ref)})
				case field.Type == "array" && field.Items != nil && field.Items.Type == "ref":
					positions = append(positions, RefPosition{Lexicon: document.ID, Def: defName, Property: property, Array: true, Type: resolve(field.Items.Ref)})
				}
			}
		}
	}
	return positions
}

func sortedPositions(positions []RefPosition) []RefPosition {
	sorted := append([]RefPosition(nil), positions...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		return a.Lexicon+"\x00"+a.Def+"\x00"+a.Property < b.Lexicon+"\x00"+b.Def+"\x00"+b.Property
	})
	return sorted
}

// TestRefPositionsMatchTheEmbedLexicons is the drift guard: the code's table
// of non-union ref positions is exactly what the embed lexicons declare.
func TestRefPositionsMatchTheEmbedLexicons(t *testing.T) {
	assert.Equal(t, sortedPositions(lexiconRefPositions(t)), sortedPositions(RefPositions()))
}
