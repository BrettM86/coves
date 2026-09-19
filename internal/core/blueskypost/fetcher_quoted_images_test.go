package blueskypost

import (
	"encoding/json"
	"testing"

	bsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const quotedPostURI = "at://did:plc:quoted/app.bsky.feed.post/quoted"

func quotedViewRecordJSON(t *testing.T, embeds string) string {
	t.Helper()
	return `{"$type":"app.bsky.embed.record#viewRecord",
		"uri":"` + quotedPostURI + `","cid":"quoted-cid",
		"author":{"did":"did:plc:quoted","handle":"quoted.bsky.social","displayName":"Quoted"},
		"value":{"$type":"app.bsky.feed.post","text":"quoted value text","createdAt":"2025-12-20T08:00:00.987654Z"},
		"replyCount":11,"repostCount":13,"likeCount":17,"embeds":[` + embeds + `],
		"indexedAt":"2025-12-20T08:00:01Z"}`
}

func plainQuoteViewJSON(record string) string {
	return `{"$type":"app.bsky.embed.record#view","record":` + record + `}`
}

func TestFetchBlueskyPost_QuotedResolvedImagesAndOneLevelCap(t *testing.T) {
	thumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/quoted-thumb"
	fullsize := resolvedMediaCDN + "/img/feed_fullsize/plain/quoted-fullsize"
	nestedQuote := plainQuoteViewJSON(`{"$type":"app.bsky.embed.record#viewRecord",
		"uri":"at://did:plc:nested/app.bsky.feed.post/nested","cid":"nested-cid",
		"author":{"did":"did:plc:nested","handle":"nested.bsky.social"},
		"value":{"$type":"app.bsky.feed.post","text":"must be dropped","createdAt":"2025-12-19T08:00:00Z"},
		"indexedAt":"2025-12-19T08:00:01Z"}`)
	images := imageViewJSON(t, thumb, fullsize, "quoted alt", `{"width":3,"height":2}`)
	quote := plainQuoteViewJSON(quotedViewRecordJSON(t, images+`,`+nestedQuote))
	postJSON := getPostsPostJSON(t, "", "", quote)

	var upstream bsky.FeedGetPosts_Output
	require.NoError(t, json.Unmarshal([]byte(`{"posts":[`+postJSON+`]}`), &upstream))
	require.NotNil(t, upstream.Posts[0].Embed.EmbedRecord_View)
	require.NotNil(t, upstream.Posts[0].Embed.EmbedRecord_View.Record.EmbedRecord_ViewRecord)
	upstreamQuoted := upstream.Posts[0].Embed.EmbedRecord_View.Record.EmbedRecord_ViewRecord
	require.Len(t, upstreamQuoted.Embeds, 2)
	require.NotNil(t, upstreamQuoted.Embeds[0].EmbedImages_View,
		"quoted media is the viewRecord embeds[] image view, not value.embed raw blobs")
	require.NotNil(t, upstreamQuoted.Embeds[1].EmbedRecord_View)

	_, public := fetchPostFixture(t, postJSON)
	quoted := publicObject(t, public["quotedPost"])
	assert.Equal(t, quotedPostURI, quoted["uri"])
	assert.Equal(t, "quoted-cid", quoted["cid"])
	assert.Equal(t, "quoted value text", quoted["text"])
	assert.Equal(t, "2025-12-20T08:00:00.987654Z", quoted["createdAt"])
	assert.EqualValues(t, 11, quoted["replyCount"])
	assert.EqualValues(t, 13, quoted["repostCount"])
	assert.EqualValues(t, 17, quoted["likeCount"])
	quotedImages := publicImages(t, quoted)
	require.Len(t, quotedImages, 1)
	image := publicObject(t, quotedImages[0])
	assert.Equal(t, thumb, image["thumb"])
	assert.Equal(t, fullsize, image["fullsize"])
	assert.Equal(t, "quoted alt", image["alt"])
	assert.EqualValues(t, 1, quoted["mediaCount"])
	assert.Equal(t, true, quoted["hasMedia"])
	assert.NotContains(t, quoted, "quotedPost", "quotes are capped at one level")
}

func TestFetchBlueskyPost_QuotedOwnRecordWithMediaUsesMediaView(t *testing.T) {
	thumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/quoted-rwm-thumb"
	fullsize := resolvedMediaCDN + "/img/feed_fullsize/plain/quoted-rwm-fullsize"
	quotedOwnEmbed := `{"$type":"app.bsky.embed.recordWithMedia#view",
		"media":` + imageViewJSON(t, thumb, fullsize, "quoted own image", "") + `,
		"record":` + plainQuoteViewJSON(`{"$type":"app.bsky.embed.record#viewRecord",
			"uri":"at://did:plc:nested/app.bsky.feed.post/nested","cid":"nested-cid",
			"author":{"did":"did:plc:nested","handle":"nested.bsky.social"},
			"value":{"$type":"app.bsky.feed.post","text":"nested quote","createdAt":"2025-12-19T08:00:00Z"},
			"indexedAt":"2025-12-19T08:00:01Z"}`) + `}`
	quote := plainQuoteViewJSON(quotedViewRecordJSON(t, quotedOwnEmbed))

	_, public := fetchPostFixture(t, getPostsPostJSON(t, "", "", quote))
	quoted := publicObject(t, public["quotedPost"])
	images := publicImages(t, quoted)
	require.Len(t, images, 1)
	assert.Equal(t, thumb, publicObject(t, images[0])["thumb"])
	assert.EqualValues(t, 1, quoted["mediaCount"])
	assert.Equal(t, true, quoted["hasMedia"])
	assert.NotContains(t, quoted, "quotedPost", "the record half is a quote-of-quote and is dropped")
}

func TestFetchBlueskyPost_QuotedOwnRecordWithMediaExternalCard(t *testing.T) {
	thumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/quoted-rwm-card"
	external := `{"$type":"app.bsky.embed.external#view","external":{"uri":"https://outside.example/quoted-rwm","title":"RWM card","description":"card description","thumb":` + jsonString(t, thumb) + `}}`
	quotedOwnEmbed := `{"$type":"app.bsky.embed.recordWithMedia#view",
		"media":` + external + `,
		"record":` + plainQuoteViewJSON(`{"$type":"app.bsky.embed.record#viewNotFound","uri":"at://did:plc:nested/app.bsky.feed.post/nested","notFound":true}`) + `}`
	quote := plainQuoteViewJSON(quotedViewRecordJSON(t, quotedOwnEmbed))

	_, public := fetchPostFixture(t, getPostsPostJSON(t, "", "", quote))
	quoted := publicObject(t, public["quotedPost"])
	embed := publicObject(t, quoted["embed"])
	assert.Equal(t, "https://outside.example/quoted-rwm", embed["uri"])
	assert.Equal(t, "RWM card", embed["title"])
	assert.Equal(t, "card description", embed["description"])
	assert.Equal(t, thumb, embed["thumb"])
	assert.NotContains(t, quoted, "quotedPost")
}

func TestFetchBlueskyPost_UnsupportedQuoteUnionDoesNotCreateEmptyQuote(t *testing.T) {
	unknownRecord := `{"$type":"app.example.unsupported#view","uri":"at://did:plc:other/app.example.record/x"}`
	quote := plainQuoteViewJSON(unknownRecord)
	postJSON := getPostsPostJSON(t, "", "", quote)

	var upstream bsky.FeedGetPosts_Output
	require.NoError(t, json.Unmarshal([]byte(`{"posts":[`+postJSON+`]}`), &upstream))
	require.NotNil(t, upstream.Posts[0].Embed.EmbedRecord_View)
	recordUnion := upstream.Posts[0].Embed.EmbedRecord_View.Record
	require.NotNil(t, recordUnion)
	assert.Nil(t, recordUnion.EmbedRecord_ViewRecord)
	assert.Nil(t, recordUnion.EmbedRecord_ViewBlocked)
	assert.Nil(t, recordUnion.EmbedRecord_ViewNotFound)
	assert.Nil(t, recordUnion.EmbedRecord_ViewDetached)

	_, public := fetchPostFixture(t, postJSON)
	assert.NotContains(t, public, "quotedPost", "an unknown union member is not an empty quote")
}
