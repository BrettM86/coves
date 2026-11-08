package embeds_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"Coves/internal/atproto/embedblobs"
	"Coves/internal/atproto/embedimages"
	"Coves/internal/core/blobs"
	"Coves/internal/core/embeds"
	"Coves/internal/core/richtext"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guard proves that every reader tolerates an embed whose non-union refs
// carry no $type, as atproto allows and as rows stored before canonicalization
// were written. It never calls CanonicalizeEmbed: the expected value is what
// the same reader returns for the fully typed fixture.

const (
	guardFirstCID  = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	guardSecondCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	guardThirdCID  = "bafkreibme22gw2h7y2h7tg2fhqotaqjucnbc24deqo72b6mkl2egezxhvy"
	guardQuotedURI = "at://did:plc:quoted/social.coves.community.postv2/3kquoted"
	guardOwnerDID  = "did:plc:guardowner"
	guardOwnerPDS  = "https://pds.example"
)

func guardCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, entry := range typed {
			copied[key] = guardCopy(entry)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for i, entry := range typed {
			copied[i] = guardCopy(entry)
		}
		return copied
	default:
		return value
	}
}

func guardBlob(cid string) map[string]any {
	return map[string]any{"$type": "blob", "ref": map[string]any{"$link": cid}, "mimeType": "image/jpeg", "size": 52813.0}
}

func typedImage(cid string) map[string]any {
	return map[string]any{"$type": "social.coves.embed.images#image", "image": guardBlob(cid), "alt": "alt " + cid[len(cid)-4:]}
}

func typedGallery(entries ...any) map[string]any {
	return map[string]any{"$type": embedblobs.TypeImages, "images": entries}
}

func typedStrongRef() map[string]any {
	return map[string]any{"$type": "com.atproto.repo.strongRef", "uri": guardQuotedURI, "cid": guardSecondCID}
}

func typedExternal() map[string]any {
	return map[string]any{
		"$type": embedblobs.TypeExternal,
		"external": map[string]any{
			"$type":  "social.coves.embed.external#external",
			"uri":    "https://example.com/article",
			"title":  "An article",
			"thumb":  guardBlob(guardSecondCID),
			"images": []any{typedImage(guardThirdCID)},
			"sources": []any{map[string]any{
				"$type":      "social.coves.embed.external#source",
				"uri":        "https://example.com/source",
				"sourcePost": typedStrongRef(),
			}},
		},
	}
}

func typedQuote() map[string]any {
	return map[string]any{"$type": "social.coves.embed.post", "post": typedStrongRef()}
}

func typedGalleryWithAspectRatio() map[string]any {
	first := typedImage(guardFirstCID)
	first["aspectRatio"] = map[string]any{"$type": "social.coves.embed.images#aspectRatio", "width": 4.0, "height": 3.0}
	return typedGallery(first, typedImage(guardSecondCID))
}

func typedComposite(attachment map[string]any) map[string]any {
	return map[string]any{"$type": embedblobs.TypeImagesWithAttachment, "images": typedGalleryWithAspectRatio(), "attachment": attachment}
}

type guardFixture struct {
	name  string
	embed map[string]any
}

// lexiconEmbedFixtures returns the embed of every record in the shared
// lexicon test data that carries one.
func lexiconEmbedFixtures(t *testing.T) []guardFixture {
	t.Helper()
	root := filepath.Join("..", "..", "..", "tests", "lexicon-test-data")
	var fixtures []guardFixture
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("decode fixture %s: %w", path, err)
		}
		if embed, ok := record["embed"].(map[string]any); ok {
			name, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			fixtures = append(fixtures, guardFixture{name: name, embed: embed})
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, fixtures, "no embed fixtures found under %s", root)
	return fixtures
}

func guardFixtures(t *testing.T) []guardFixture {
	return append(lexiconEmbedFixtures(t),
		guardFixture{name: "typed images with aspect ratio", embed: typedGalleryWithAspectRatio()},
		guardFixture{name: "typed external with thumb, images and sources", embed: typedExternal()},
		guardFixture{name: "gallery with external attachment", embed: typedComposite(typedExternal())},
		guardFixture{name: "gallery with post attachment", embed: typedComposite(typedQuote())},
	)
}

// schemaType is the $type a def is known by: the bare NSID for main.
func schemaType(lexicon, def string) string {
	if def == "main" {
		return lexicon
	}
	return lexicon + "#" + def
}

// stripRefTypes deletes $type from every non-union ref position in embed,
// descending through the top-level union and the imagesWithAttachment
// attachment union by their own (kept) $type.
func stripRefTypes(t *testing.T, embed map[string]any) map[string]any {
	t.Helper()
	positions := embedblobs.RefPositions()
	require.NotEmpty(t, positions, "embedblobs.RefPositions is empty, so nothing would be stripped")
	byOwner := make(map[string][]embedblobs.RefPosition)
	for _, position := range positions {
		owner := schemaType(position.Lexicon, position.Def)
		byOwner[owner] = append(byOwner[owner], position)
	}

	var strip func(object map[string]any, ownerType string)
	strip = func(object map[string]any, ownerType string) {
		for _, position := range byOwner[ownerType] {
			var children []any
			if position.Array {
				children, _ = object[position.Property].([]any)
			} else {
				children = []any{object[position.Property]}
			}
			for _, child := range children {
				if childObject, ok := child.(map[string]any); ok {
					delete(childObject, "$type")
					strip(childObject, position.Type)
				}
			}
		}
		if ownerType == embedblobs.TypeImagesWithAttachment {
			if attachment, ok := object["attachment"].(map[string]any); ok {
				attachmentType, _ := attachment["$type"].(string)
				strip(attachment, attachmentType)
			}
		}
	}

	stripped := guardCopy(embed).(map[string]any)
	embedType, _ := stripped["$type"].(string)
	strip(stripped, embedType)
	return stripped
}

func checkErrorClass(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, embedimages.ErrTooManyImages):
		return "too many images"
	case errors.Is(err, embedimages.ErrInvalidImageBlob):
		return "invalid image blob"
	default:
		return "other: " + err.Error()
	}
}

func facetErrorClass(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, richtext.ErrInvalidImageFacet):
		return "invalid image facet"
	default:
		return "other: " + err.Error()
	}
}

// viewSummary is what a client reads from a hydrated or served embed: the
// view discriminators and every media URL, in a stable order.
type viewSummary struct {
	Type           any
	GalleryType    any
	AttachmentType any
	URLs           []string
}

func collectURLs(value any, urls *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if url, ok := typed[key].(string); ok && (key == "thumb" || key == "fullsize") {
				*urls = append(*urls, key+"="+url)
				continue
			}
			collectURLs(typed[key], urls)
		}
	case []any:
		for _, entry := range typed {
			collectURLs(entry, urls)
		}
	}
}

func summarize(value any) viewSummary {
	embed, _ := value.(map[string]any)
	if embed == nil {
		return viewSummary{}
	}
	summary := viewSummary{Type: embed["$type"]}
	if gallery, ok := embed["images"].(map[string]any); ok {
		summary.GalleryType = gallery["$type"]
	}
	if attachment, ok := embed["attachment"].(map[string]any); ok {
		summary.AttachmentType = attachment["$type"]
	}
	collectURLs(embed, &summary.URLs)
	return summary
}

// readerResults runs every embed reader on its own copy of embed.
type readerResults struct {
	PostReferences    []embedblobs.Reference
	CommentReferences []embedblobs.Reference
	ImageCount        int
	CheckError        string
	Servable          bool
	ServableSummary   viewSummary
	HydratedSummary   viewSummary
	HasImages         bool
	ImageFacetError   string
}

func runReaders(embed map[string]any) readerResults {
	fresh := func() map[string]any { return guardCopy(embed).(map[string]any) }
	served := embeds.ServableEmbed(fresh())
	hydrated := fresh()
	embeds.HydrateView(hydrated, guardOwnerDID, guardOwnerPDS)
	imageFacet := []any{map[string]any{
		"index":    map[string]any{"byteStart": 0.0, "byteEnd": 1.0},
		"features": []any{map[string]any{"$type": "social.coves.richtext.facet#image", "index": 0.0}},
	}}
	return readerResults{
		PostReferences:    embedblobs.PostReferences(fresh()),
		CommentReferences: embedblobs.CommentReferences(fresh()),
		ImageCount:        embedblobs.ImageCount(fresh()),
		CheckError:        checkErrorClass(embedimages.Check(fresh())),
		Servable:          served != nil,
		ServableSummary:   summarize(served),
		HydratedSummary:   summarize(hydrated),
		HasImages:         richtext.EmbedHasImages(fresh()),
		ImageFacetError:   facetErrorClass(richtext.CheckImageFacets(imageFacet, fresh(), 1)),
	}
}

func withGuardProxy(t *testing.T) {
	t.Helper()
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://media.example"})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
}

// TestReadersTolerateUntypedRefPositions must not run in parallel: the image
// URL configuration it sets is process-wide.
func TestReadersTolerateUntypedRefPositions(t *testing.T) {
	withGuardProxy(t)
	for _, fixture := range guardFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			typed := runReaders(fixture.embed)
			stripped := runReaders(stripRefTypes(t, fixture.embed))
			assert.Equal(t, typed.PostReferences, stripped.PostReferences, "PostReferences")
			assert.Equal(t, typed.CommentReferences, stripped.CommentReferences, "CommentReferences")
			assert.Equal(t, typed.ImageCount, stripped.ImageCount, "ImageCount")
			assert.Equal(t, typed.CheckError, stripped.CheckError, "embedimages.Check")
			assert.Equal(t, typed.Servable, stripped.Servable, "ServableEmbed served")
			assert.Equal(t, typed.ServableSummary.Type, stripped.ServableSummary.Type, "ServableEmbed $type")
			assert.Equal(t, typed.ServableSummary.GalleryType, stripped.ServableSummary.GalleryType, "ServableEmbed gallery $type")
			assert.Equal(t, typed.HydratedSummary, stripped.HydratedSummary, "HydrateView")
			assert.Equal(t, typed.HasImages, stripped.HasImages, "EmbedHasImages")
			assert.Equal(t, typed.ImageFacetError, stripped.ImageFacetError, "CheckImageFacets")
		})
	}
}

func TestUntypedGalleryCountsTowardTheImageCap(t *testing.T) {
	entries := make([]any, 9)
	for i := range entries {
		entries[i] = map[string]any{"image": guardBlob(guardFirstCID)}
	}
	embed := map[string]any{
		"$type":      embedblobs.TypeImagesWithAttachment,
		"images":     map[string]any{"images": entries},
		"attachment": map[string]any{"$type": "social.coves.embed.post", "post": map[string]any{"uri": guardQuotedURI, "cid": guardSecondCID}},
	}
	assert.Equal(t, 9, embedblobs.ImageCount(guardCopy(embed).(map[string]any)))
	err := embedimages.Check(guardCopy(embed).(map[string]any))
	assert.True(t, errors.Is(err, embedimages.ErrTooManyImages), "got %v", err)
}
