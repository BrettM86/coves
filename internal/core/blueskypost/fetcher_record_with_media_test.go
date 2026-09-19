package blueskypost

import (
	"encoding/json"
	"testing"

	bsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordWithMediaViewJSON(media, recordUnion string) string {
	return `{"$type":"app.bsky.embed.recordWithMedia#view","media":` + media +
		`,"record":` + plainQuoteViewJSON(recordUnion) + `}`
}

func TestFetchBlueskyPost_RecordWithMediaUsesResolvedParentImages(t *testing.T) {
	thumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/parent-rwm-thumb"
	fullsize := resolvedMediaCDN + "/img/feed_fullsize/plain/parent-rwm-fullsize"
	media := imageViewJSON(t, thumb, fullsize, "parent image", "")
	quoteRecord := quotedViewRecordJSON(t, "")
	view := recordWithMediaViewJSON(media, quoteRecord)
	postJSON := getPostsPostJSON(t, "", `{"$type":"app.bsky.embed.recordWithMedia","record":{"uri":"`+
		quotedPostURI+`","cid":"quoted-cid"},"media":`+rawMisleadingImageRecord()+`}`, view)

	var upstream bsky.FeedGetPosts_Output
	require.NoError(t, json.Unmarshal([]byte(`{"posts":[`+postJSON+`]}`), &upstream))
	require.NotNil(t, upstream.Posts[0].Embed.EmbedRecordWithMedia_View)
	require.NotNil(t, upstream.Posts[0].Embed.EmbedRecordWithMedia_View.Media.EmbedImages_View)
	require.NotNil(t, upstream.Posts[0].Embed.EmbedRecordWithMedia_View.Record.Record.EmbedRecord_ViewRecord,
		"recordWithMedia quote union is embed.record.record")

	_, public := fetchPostFixture(t, postJSON)
	images := publicImages(t, public)
	require.Len(t, images, 1)
	image := publicObject(t, images[0])
	assert.Equal(t, thumb, image["thumb"])
	assert.Equal(t, fullsize, image["fullsize"])
	assert.Equal(t, "parent image", image["alt"])
	assert.EqualValues(t, 1, public["mediaCount"])
	assert.Equal(t, true, public["hasMedia"])
	assert.Equal(t, "quoted value text", publicObject(t, public["quotedPost"])["text"])
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), testRawCID)
}

func TestFetchBlueskyPost_RecordWithMediaUsesResolvedParentExternal(t *testing.T) {
	thumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/parent-rwm-card"
	media := `{"$type":"app.bsky.embed.external#view","external":{"uri":"https://outside.example/parent-rwm","title":"Parent card","description":"Parent description","thumb":` + jsonString(t, thumb) + `}}`
	view := recordWithMediaViewJSON(media, quotedViewRecordJSON(t, ""))

	_, public := fetchPostFixture(t, getPostsPostJSON(t, "", "", view))
	embed := publicObject(t, public["embed"])
	assert.Equal(t, "https://outside.example/parent-rwm", embed["uri"])
	assert.Equal(t, "Parent card", embed["title"])
	assert.Equal(t, "Parent description", embed["description"])
	assert.Equal(t, thumb, embed["thumb"])
	assert.Equal(t, "quoted value text", publicObject(t, public["quotedPost"])["text"])
}

func TestFetchBlueskyPost_PreservesVideoIndicatorsWithoutImages(t *testing.T) {
	tests := []struct {
		name string
		view string
	}{
		{
			name: "direct video view",
			view: `{"$type":"app.bsky.embed.video#view","cid":"video-cid","playlist":"https://video.example.test/playlist.m3u8","thumbnail":"https://video.example.test/thumb.jpg"}`,
		},
		{
			name: "recordWithMedia video view",
			view: recordWithMediaViewJSON(
				`{"$type":"app.bsky.embed.video#view","cid":"video-cid","playlist":"https://video.example.test/playlist.m3u8","thumbnail":"https://video.example.test/thumb.jpg"}`,
				quotedViewRecordJSON(t, "")),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, public := fetchPostFixture(t, getPostsPostJSON(t, "", "", test.view))
			assert.NotContains(t, public, "images")
			assert.Equal(t, true, public["hasMedia"])
			assert.EqualValues(t, 1, public["mediaCount"])
		})
	}
}

func TestFetchBlueskyPost_PreservesRawVideoIndicatorWithoutResolvedEmbed(t *testing.T) {
	rawVideo := `{"$type":"app.bsky.embed.video","video":{"$type":"blob","ref":{"$link":"` + testRawCID + `"},"mimeType":"video/mp4","size":123}}`

	result, public := fetchPostFixture(t, getPostsPostJSON(t, "", rawVideo, ""))

	assert.Empty(t, result.Images)
	assert.True(t, result.HasMedia)
	assert.Equal(t, 1, result.MediaCount)
	assert.NotContains(t, public, "images")
	assert.Equal(t, true, public["hasMedia"])
	assert.EqualValues(t, 1, public["mediaCount"])
}
