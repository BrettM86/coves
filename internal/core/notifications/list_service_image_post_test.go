package notifications_test

import (
	"encoding/json"
	"strings"
	"testing"

	"Coves/internal/core/blobs"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_ImagePostsAndAltText(t *testing.T) {
	const imageURL = "https://img.example.test/img/content_preview/plain/did:plc:owner/bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	const linkURL = "https://img.example.test/img/embed_thumbnail/plain/did:plc:owner/bafkreie5737gdxlw5i64vzichcalba3z2v5n6icifvx5xytvske7mr3hpm"
	const legacyImageURL = "https://img.example.test/img/content_preview/plain/did:plc:community/bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	buildImages := func(firstAlt interface{}) map[string]interface{} {
		first := map[string]interface{}{"image": previewBlob(previewCID)}
		if firstAlt != nil {
			first["alt"] = firstAlt
		}
		return map[string]interface{}{"$type": "social.coves.embed.images", "images": []interface{}{
			first,
			map[string]interface{}{"image": previewBlob(thirdPreviewCID), "alt": "second"},
		}}
	}
	buildLink := func() map[string]interface{} {
		embed := previewLink(previewBlob(secondPreviewCID))
		external := embed["external"].(map[string]interface{})
		external["title"] = "Linked page title"
		external["description"] = "Linked page description"
		external["images"] = []interface{}{map[string]interface{}{"image": previewBlob(thirdPreviewCID), "alt": "gallery alt"}}
		return embed
	}
	for _, tc := range []struct {
		name, title, body, wantText, wantThumbnail, wantAlt string
		build                                               func() map[string]interface{}
		unprojectable                                       bool
	}{
		{"untitled image has no body fallback", "", "Secret image body", "", imageURL, "A red kite", func() map[string]interface{} { return buildImages("A red kite") }, false},
		{"blank-titled image has no body fallback", "  ", "Secret image body", "", imageURL, "A red kite", func() map[string]interface{} { return buildImages("A red kite") }, false},
		{"titled image shows title and thumbnail", "Sunset", "Body", "Sunset", imageURL, "A red kite", func() map[string]interface{} { return buildImages("A red kite") }, false},
		{"untitled link uses body and link thumb", "", "Linked article body", "Linked article body", linkURL, "", buildLink, false},
		{"first-image alt retained exactly across positions", "Sunset", "Body", "Sunset", imageURL, "  A red kite over the ridge ", func() map[string]interface{} { return buildImages("  A red kite over the ridge ") }, false},
		{"absent alt does not use second image", "Sunset", "Body", "Sunset", imageURL, "", func() map[string]interface{} { return buildImages(nil) }, false},
		{"empty alt does not use second image", "Sunset", "Body", "Sunset", imageURL, "", func() map[string]interface{} { return buildImages("") }, false},
		{"ASCII-whitespace alt omitted", "Sunset", "Body", "Sunset", imageURL, "", func() map[string]interface{} { return buildImages(" \t\n") }, false},
		{"ideographic-space alt omitted", "Sunset", "Body", "Sunset", imageURL, "", func() map[string]interface{} { return buildImages("　") }, false},
		{"numeric alt omitted", "Sunset", "Body", "Sunset", imageURL, "", func() map[string]interface{} { return buildImages(123) }, false},
		{"exactly 1000 ASCII alt characters", "Sunset", "Body", "Sunset", imageURL, strings.Repeat("a", 1000), func() map[string]interface{} { return buildImages(strings.Repeat("a", 1000)) }, false},
		{"1001 ASCII alt characters cut to 1000", "Sunset", "Body", "Sunset", imageURL, strings.Repeat("a", 1000), func() map[string]interface{} { return buildImages(strings.Repeat("a", 1001)) }, false},
		{"family emoji cut at 10000 bytes on cluster boundary", "Sunset", "Body", "Sunset", imageURL, strings.Repeat("👨‍👩‍👧‍👦", 400), func() map[string]interface{} { return buildImages(strings.Repeat("👨‍👩‍👧‍👦", 500)) }, false},
		{"prefix changes remaining byte budget without splitting cluster", "Sunset", "Body", "Sunset", imageURL, "a" + strings.Repeat("👨‍👩‍👧‍👦", 399), func() map[string]interface{} {
			return buildImages("a" + strings.Repeat("👨‍👩‍👧‍👦", 500))
		}, false},
		{"bounded whitespace-only alt omitted", "Sunset", "Body", "Sunset", imageURL, "", func() map[string]interface{} { return buildImages(strings.Repeat(" ", 1000) + "x") }, false},
		{"untitled image alt is not text fallback", "", "Secret image body", "", imageURL, "A red kite", func() map[string]interface{} { return buildImages("A red kite") }, false},
		{"link card title description and gallery alt are not thumb alt", "", "Linked article body", "Linked article body", linkURL, "", buildLink, false},
		{"unprojectable untitled image has neither text nor alt", "", "Secret image body", "", "", "", func() map[string]interface{} { return previewImages("https://evil.example/x.jpg", "leaked alt") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setPreviewImageConfig(t, blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test"})
			post := previewPostView("image", false, tc.build(), tc.title, tc.body)
			var rows []map[string]json.RawMessage
			var encoded string
			if tc.unprojectable {
				rows, encoded = previewPage(t, post, previewPostView("sibling", false, buildImages("Sibling alt"), "Sunset", "Body"))
				assertPreviewMedia(t, rows, 3, imageURL, "Sibling alt")
				require.NotContains(t, encoded, "leaked alt")
				require.NotContains(t, encoded, "https://evil.example/x.jpg")
			} else {
				rows, encoded = previewPage(t, post)
			}
			assertPreviewMedia(t, rows, 0, tc.wantThumbnail, tc.wantAlt)
			for _, position := range []struct {
				index          int
				field, textKey string
			}{
				{0, "rootPost", "title"}, {0, "subject", "preview"}, {1, "subject", "preview"}, {2, "record", "excerpt"},
			} {
				reference := previewReference(t, rows[position.index], position.field)
				wantText := tc.wantText
				if position.field == "rootPost" {
					wantText = tc.title
					if strings.TrimSpace(wantText) == "" {
						wantText = ""
					}
				}
				assertPreviewField(t, reference, position.textKey, wantText)
			}
			if tc.title == "" && strings.HasPrefix(tc.name, "untitled image") {
				require.NotContains(t, encoded, tc.body, "body must never become image-post text")
			}
		})
	}
	t.Run("legacy first-image blob cid resolves under community owner", func(t *testing.T) {
		setPreviewImageConfig(t, blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test"})
		legacyBlob := map[string]interface{}{"cid": thirdPreviewCID, "mimeType": "image/jpeg"}
		rows, _ := previewPage(t, previewPostView("legacy", true, previewImages(legacyBlob, "Legacy alt"), "", "Body"))
		assertPreviewMedia(t, rows, 0, legacyImageURL, "Legacy alt")
		for _, position := range []struct {
			index          int
			field, textKey string
		}{
			{0, "rootPost", "title"}, {0, "subject", "preview"}, {1, "subject", "preview"}, {2, "record", "excerpt"},
		} {
			assertPreviewField(t, previewReference(t, rows[position.index], position.field), position.textKey, "")
		}
	})
}
