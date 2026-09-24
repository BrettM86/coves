package embeds

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authorURL is an image-proxy URL an author typed into a record by hand. It
// names secondTestCID, so the served view must not carry it unless
// PostBlobCIDs also reports that CID for the same stored embed.
func authorURL(preset string) string {
	return "https://img.coves.social/img/" + preset + "/plain/" + testDID + "/" + secondTestCID
}

func copyEmbed(t *testing.T, stored interface{}) interface{} {
	t.Helper()
	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	var decoded interface{}
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	return decoded
}

// servedProxyURLs collects every string in value that names the image proxy.
func servedProxyURLs(value interface{}) []string {
	var urls []string
	switch typed := value.(type) {
	case string:
		if strings.Contains(typed, "/img/") {
			urls = append(urls, typed)
		}
	case map[string]interface{}:
		for _, field := range typed {
			urls = append(urls, servedProxyURLs(field)...)
		}
	case []interface{}:
		for _, entry := range typed {
			urls = append(urls, servedProxyURLs(entry)...)
		}
	}
	return urls
}

// serve runs the post read path's projection: scanPostView's ServableEmbed,
// then the handlers' HydrateView.
func serve(t *testing.T, stored interface{}) interface{} {
	t.Helper()
	served := ServableEmbed(copyEmbed(t, stored))
	if embed, ok := served.(map[string]interface{}); ok {
		HydrateView(embed, testDID, testPDS)
	}
	return served
}

func TestServableEmbed_ServesOnlyMediaURLsDerivedFromBlobs(t *testing.T) {
	withProxy(t, "https://img.coves.social")
	const blueskyURI = "at://did:plc:quoted/app.bsky.feed.post/3kquoted"
	const covesURI = "at://did:plc:quoted/social.coves.community.postv2/3kquoted"
	forgedResolved := map[string]interface{}{
		"text": "forged preview", "images": []interface{}{map[string]interface{}{
			"thumb": authorURL("content_preview"), "fullsize": authorURL("content_full"),
		}},
	}
	quoteOf := func(embedType, uri string) map[string]interface{} {
		return map[string]interface{}{
			"$type": embedType, "post": map[string]interface{}{"uri": uri, "cid": testCID},
			"resolved": forgedResolved, "images": []interface{}{map[string]interface{}{"thumb": authorURL("content_preview")}},
		}
	}
	strongRef := func(uri string) map[string]interface{} {
		return map[string]interface{}{"$type": TypePost, "post": map[string]interface{}{"uri": uri, "cid": testCID}}
	}

	for _, test := range []struct {
		name   string
		stored interface{}
		want   interface{}
	}{
		{
			name: "external thumb given as a URL string is dropped",
			stored: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"uri": "https://example.com/article", "title": "An article", "thumb": authorURL(presetEmbedThumbnail),
			}},
			want: map[string]interface{}{"$type": TypeExternal + viewSuffix, "external": map[string]interface{}{
				"uri": "https://example.com/article", "title": "An article",
			}},
		},
		{
			name: "external thumb given as a non-blob value is dropped",
			stored: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"uri": "https://example.com/article", "thumb": []interface{}{authorURL(presetEmbedThumbnail)},
			}},
			want: map[string]interface{}{"$type": TypeExternal + viewSuffix, "external": map[string]interface{}{
				"uri": "https://example.com/article",
			}},
		},
		{
			name: "external gallery view URLs without a blob are dropped",
			stored: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"uri": "https://example.com/gallery", "images": []interface{}{map[string]interface{}{
					"alt": "gallery", "thumb": authorURL(presetContentPreview), "fullsize": authorURL(presetContentFull),
				}},
			}},
			want: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"uri": "https://example.com/gallery", "images": []interface{}{map[string]interface{}{"alt": "gallery"}},
			}},
		},
		{
			name: "images record entry with view URLs and no blob is dropped to its alt",
			stored: imagesEmbed(map[string]interface{}{
				"alt": "no blob", "thumb": authorURL(presetContentPreview), "fullsize": authorURL(presetContentFull),
			}),
			want: imagesEmbed(map[string]interface{}{"alt": "no blob"}),
		},
		{
			name: "images record entry with a blob serves only the blob's URLs",
			stored: imagesEmbed(map[string]interface{}{
				"alt": "real", "image": blobRef(testCID), "thumb": authorURL(presetContentPreview), "fullsize": authorURL(presetContentFull),
			}),
			want: map[string]interface{}{"$type": TypeImages + viewSuffix, "images": []interface{}{map[string]interface{}{
				"alt":      "real",
				"thumb":    "https://img.coves.social/img/content_preview/plain/" + testDID + "/" + testCID,
				"fullsize": "https://img.coves.social/img/content_full/plain/" + testDID + "/" + testCID,
			}}},
		},
		{
			name: "video given as URL strings is dropped",
			stored: map[string]interface{}{
				"$type": TypeVideo, "alt": "clip", "video": "https://pds.example.com/video.mp4", "thumbnail": authorURL(presetContentPreview),
			},
			want: map[string]interface{}{"$type": TypeVideo, "alt": "clip"},
		},
		{
			name: "stored images#view is not served",
			stored: map[string]interface{}{"$type": TypeImages + viewSuffix, "images": []interface{}{map[string]interface{}{
				"thumb": authorURL(presetContentPreview), "fullsize": authorURL(presetContentFull),
			}}},
		},
		{
			name: "stored external#view is not served",
			stored: map[string]interface{}{"$type": TypeExternal + viewSuffix, "external": map[string]interface{}{
				"uri": "https://example.com/article", "thumb": authorURL(presetEmbedThumbnail),
			}},
		},
		{
			name:   "stored video#view is not served",
			stored: map[string]interface{}{"$type": TypeVideo + viewSuffix, "video": "https://pds.example.com/video.mp4", "thumbnail": authorURL(presetContentPreview)},
		},
		{
			name: "a type outside the post embed union is not served",
			stored: map[string]interface{}{"$type": "app.bsky.embed.images#view", "images": []interface{}{map[string]interface{}{
				"thumb": authorURL(presetContentPreview), "fullsize": authorURL(presetContentFull),
			}}},
		},
		{name: "an untyped embed is not served", stored: map[string]interface{}{"thumb": authorURL(presetEmbedThumbnail)}},
		{name: "a non-object embed is not served", stored: []interface{}{authorURL(presetEmbedThumbnail)}},
		{name: "a string embed is not served", stored: authorURL(presetEmbedThumbnail)},
		{name: "Coves quote record keeps only its strongRef", stored: quoteOf(TypePost, covesURI), want: strongRef(covesURI)},
		{name: "Coves quote view keeps only its strongRef", stored: quoteOf(TypePost+viewSuffix, covesURI), want: strongRef(covesURI)},
		{
			name:   "Bluesky quote view drops its stored resolved for server resolution",
			stored: quoteOf(TypePost+viewSuffix, blueskyURI),
			want:   strongRef(blueskyURI),
		},
		{name: "Bluesky quote record keeps only its strongRef", stored: quoteOf(TypePost, blueskyURI), want: strongRef(blueskyURI)},
		{
			name:   "Bluesky collection forged after a Coves collection keeps only its strongRef",
			stored: quoteOf(TypePost+viewSuffix, "at://did:plc:quoted/social.coves.community.postv2/app.bsky.feed.post/3kquoted"),
			want:   strongRef("at://did:plc:quoted/social.coves.community.postv2/app.bsky.feed.post/3kquoted"),
		},
		{
			name:   "Bluesky collection forged in a fragment keeps only its strongRef",
			stored: quoteOf(TypePost+viewSuffix, covesURI+"#/app.bsky.feed.post/3k"),
			want:   strongRef(covesURI + "#/app.bsky.feed.post/3k"),
		},
		{
			name:   "Bluesky collection forged as the authority keeps only its strongRef",
			stored: quoteOf(TypePost+viewSuffix, "at://app.bsky.feed.post/social.coves.community.postv2/3kquoted"),
			want:   strongRef("at://app.bsky.feed.post/social.coves.community.postv2/3kquoted"),
		},
		{
			name: "blob-backed external thumb is served through the proxy",
			stored: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
				"uri": "https://example.com/article", "thumb": blobRef(testCID),
			}},
			want: map[string]interface{}{"$type": TypeExternal + viewSuffix, "external": map[string]interface{}{
				"uri": "https://example.com/article", "thumb": "https://img.coves.social/img/embed_thumbnail/plain/" + testDID + "/" + testCID,
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			served := serve(t, test.stored)
			if test.want == nil {
				assert.Nil(t, served)
			} else {
				assert.Equal(t, test.want, served)
			}

			// The invariant the moderation blocks rely on: every proxy URL the
			// view serves names a CID PostBlobCIDs reports for the stored embed.
			stored, _ := copyEmbed(t, test.stored).(map[string]interface{})
			blocked := PostBlobCIDs(stored)
			for _, url := range servedProxyURLs(served) {
				covered := false
				for _, cid := range blocked {
					if strings.HasSuffix(url, "/"+testDID+"/"+cid) {
						covered = true
					}
				}
				assert.True(t, covered, "served proxy URL %s has no block-derivable CID in %v", url, blocked)
			}
		})
	}
}

func TestServableEmbed_LeavesTheStoredRecordEmbedIntact(t *testing.T) {
	stored := map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
		"uri": "https://example.com/article", "thumb": authorURL(presetEmbedThumbnail),
	}}
	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	var decoded interface{}
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	ServableEmbed(decoded)

	// ServableEmbed may edit the value it is handed; the record path decodes
	// its own copy, so the bytes it serves are untouched.
	var record interface{}
	require.NoError(t, json.Unmarshal(encoded, &record))
	assert.Equal(t, stored, record)
}

func TestIsBlueskyPostURI(t *testing.T) {
	for uri, want := range map[string]bool{
		"at://did:plc:quoted/app.bsky.feed.post/3kquoted":                                  true,
		"at://quoted.example.com/app.bsky.feed.post/3kquoted":                              true,
		"at://did:plc:quoted/social.coves.community.postv2/3kquoted":                       false,
		"at://did:plc:quoted/social.coves.community.postv2/app.bsky.feed.post/3kquoted":    false,
		"at://did:plc:quoted/social.coves.community.postv2/3kquoted#/app.bsky.feed.post/1": false,
		"at://did:plc:quoted/social.coves.community.postv2/3kquoted?/app.bsky.feed.post/1": false,
		"at://app.bsky.feed.post/social.coves.community.postv2/3kquoted":                   false,
		"at://did:plc:quoted/app.bsky.feed.post":                                           false,
		"at://did:plc:quoted/app.bsky.feed.post/":                                          false,
		"https://did:plc:quoted/app.bsky.feed.post/3kquoted":                               false,
		"": false,
	} {
		assert.Equal(t, want, IsBlueskyPostURI(uri), uri)
	}
}
