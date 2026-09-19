package posts

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/blobs"
	"Coves/internal/core/blueskypost"
)

const blueskyMediaCDN = "https://cdn.bsky.app" // coves:allow-public-host: inert media URLs in a stubbed resolved post; no test fetches them.

func decodeBlueskyResult(t *testing.T, raw string) *blueskypost.BlueskyPostResult {
	t.Helper()
	var result blueskypost.BlueskyPostResult
	require.NoError(t, json.Unmarshal([]byte(raw), &result))
	return &result
}

func resolvedPostJSON(t *testing.T, postView *PostView) map[string]interface{} {
	t.Helper()
	embed, ok := postView.Embed.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "social.coves.embed.post#view", embed["$type"])
	encoded, err := json.Marshal(embed["resolved"])
	require.NoError(t, err)
	var resolved map[string]interface{}
	require.NoError(t, json.Unmarshal(encoded, &resolved))
	return resolved
}

func TestTransformPostEmbeds_ServesDirectBlueskyCDNMedia(t *testing.T) {
	parentThumb := blueskyMediaCDN + "/img/feed_thumbnail/plain/opaque-parent-thumb@jpeg"
	parentFullsize := blueskyMediaCDN + "/img/feed_fullsize/plain/opaque-parent-fullsize@webp"
	secondThumb := blueskyMediaCDN + "/img/feed_thumbnail/plain/opaque-second-thumb"
	secondFullsize := blueskyMediaCDN + "/img/feed_fullsize/plain/opaque-second-fullsize"
	avatar := blueskyMediaCDN + "/img/avatar_thumbnail/plain/opaque-avatar"
	cardThumb := blueskyMediaCDN + "/img/feed_thumbnail/plain/opaque-card-thumb"
	quotedThumb := blueskyMediaCDN + "/img/feed_thumbnail/plain/opaque-quoted-thumb"
	quotedFullsize := blueskyMediaCDN + "/img/feed_fullsize/plain/opaque-quoted-fullsize"
	quotedAvatar := blueskyMediaCDN + "/img/avatar/plain/opaque-quoted-avatar"
	quotedCardThumb := blueskyMediaCDN + "/img/feed_thumbnail/plain/opaque-quoted-card-thumb"

	rawResult := `{
		"createdAt":"2025-12-21T10:30:00.123456Z",
		"uri":"at://did:plc:parent/app.bsky.feed.post/parent",
		"cid":"parent-cid","text":"Two photos from the coast road",
		"replyCount":3,"repostCount":5,"likeCount":7,"mediaCount":2,"hasMedia":true,
		"author":{"did":"did:plc:parent","handle":"coastroad.bsky.social","displayName":"Coast Road","avatar":` + quoteJSON(t, avatar) + `},
		"images":[
			{"thumb":` + quoteJSON(t, parentThumb) + `,"fullsize":` + quoteJSON(t, parentFullsize) + `,"alt":"A wind farm at dusk","aspectRatio":{"width":16,"height":9}},
			{"thumb":` + quoteJSON(t, secondThumb) + `,"fullsize":` + quoteJSON(t, secondFullsize) + `,"alt":""}
		],
		"embed":{"uri":"https://example.test/harbor-report","title":"Weekly harbor report","description":"Slack tides","thumb":` + quoteJSON(t, cardThumb) + `},
		"quotedPost":{
			"createdAt":"2025-12-20T08:00:00.987654Z",
			"uri":"at://did:plc:quoted/app.bsky.feed.post/quoted","cid":"quoted-cid","text":"Tide chart",
			"replyCount":11,"repostCount":13,"likeCount":17,"mediaCount":1,"hasMedia":true,
			"author":{"did":"did:plc:quoted","handle":"tides.bsky.social","displayName":"Tides","avatar":` + quoteJSON(t, quotedAvatar) + `},
			"images":[{"thumb":` + quoteJSON(t, quotedThumb) + `,"fullsize":` + quoteJSON(t, quotedFullsize) + `,"alt":"Quoted chart"}],
			"embed":{"uri":"https://outside.example/article","title":"Outside article","description":"Destination is not CDN-restricted","thumb":` + quoteJSON(t, quotedCardThumb) + `}
		}
	}`

	tests := []struct {
		name   string
		config blobs.ImageURLConfig
	}{
		{name: "proxy disabled", config: blobs.ImageURLConfig{ProxyEnabled: false}},
		{name: "proxy enabled", config: blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://proxy.example.test"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			blobs.ResetImageURLConfigForTesting()
			blobs.SetImageURLConfig(test.config)
			t.Cleanup(blobs.ResetImageURLConfigForTesting)

			result := decodeBlueskyResult(t, rawResult)
			before, err := json.Marshal(result)
			require.NoError(t, err)
			postView := &PostView{Embed: map[string]interface{}{
				"$type": "social.coves.embed.post",
				"post":  map[string]interface{}{"uri": "at://did:plc:parent/app.bsky.feed.post/parent"},
			}}

			TransformPostEmbeds(context.Background(), postView, &mockBlueskyService{resolvePostResult: result})

			resolved := resolvedPostJSON(t, postView)
			assert.Equal(t, "at://did:plc:parent/app.bsky.feed.post/parent", resolved["uri"])
			assert.Equal(t, "parent-cid", resolved["cid"])
			assert.Equal(t, "Two photos from the coast road", resolved["text"])
			assert.Equal(t, "2025-12-21T10:30:00.123456Z", resolved["createdAt"])
			assert.EqualValues(t, 3, resolved["replyCount"])
			assert.EqualValues(t, 5, resolved["repostCount"])
			assert.EqualValues(t, 7, resolved["likeCount"])
			assert.EqualValues(t, 2, resolved["mediaCount"])
			assert.Equal(t, true, resolved["hasMedia"])

			author := requireJSONMap(t, resolved["author"])
			assert.Equal(t, avatar, author["avatar"])
			images := requireJSONArray(t, resolved["images"])
			require.Len(t, images, 2)
			first := requireJSONMap(t, images[0])
			assert.Equal(t, parentThumb, first["thumb"])
			assert.Equal(t, parentFullsize, first["fullsize"])
			assert.Equal(t, "A wind farm at dusk", first["alt"])
			assert.Equal(t, map[string]interface{}{"width": float64(16), "height": float64(9)}, first["aspectRatio"])
			second := requireJSONMap(t, images[1])
			assert.Equal(t, secondThumb, second["thumb"])
			assert.Equal(t, secondFullsize, second["fullsize"])
			assert.Contains(t, second, "alt", "empty alt is required and must not be omitted")
			assert.Equal(t, "", second["alt"])

			embed := requireJSONMap(t, resolved["embed"])
			assert.Equal(t, "https://example.test/harbor-report", embed["uri"])
			assert.Equal(t, cardThumb, embed["thumb"])
			quoted := requireJSONMap(t, resolved["quotedPost"])
			assert.Equal(t, "2025-12-20T08:00:00.987654Z", quoted["createdAt"])
			assert.EqualValues(t, 11, quoted["replyCount"])
			assert.EqualValues(t, 13, quoted["repostCount"])
			assert.EqualValues(t, 17, quoted["likeCount"])
			assert.Equal(t, quotedAvatar, requireJSONMap(t, quoted["author"])["avatar"])
			quotedImages := requireJSONArray(t, quoted["images"])
			require.Len(t, quotedImages, 1)
			assert.Equal(t, quotedThumb, requireJSONMap(t, quotedImages[0])["thumb"])
			quotedEmbed := requireJSONMap(t, quoted["embed"])
			assert.Equal(t, "https://outside.example/article", quotedEmbed["uri"])
			assert.Equal(t, quotedCardThumb, quotedEmbed["thumb"])

			after, err := json.Marshal(result)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after), "serving must not mutate the cached result")
		})
	}
}

func TestTransformPostEmbeds_FiltersUntrustedCachedMedia(t *testing.T) {
	validThumb := blueskyMediaCDN + "/img/feed_thumbnail/plain/cache-valid-thumb"
	validFullsize := blueskyMediaCDN + "/img/feed_fullsize/plain/cache-valid-fullsize"
	quotedThumb := blueskyMediaCDN + "/img/feed_thumbnail/plain/quote-valid-thumb"
	quotedFullsize := blueskyMediaCDN + "/img/feed_fullsize/plain/quote-valid-fullsize"
	rawResult := `{
		"createdAt":"2025-12-21T10:30:00Z","uri":"at://did:plc:parent/app.bsky.feed.post/parent","cid":"parent-cid","text":"parent",
		"replyCount":1,"repostCount":2,"likeCount":3,"mediaCount":99,"hasMedia":true,
		"author":{"did":"did:plc:parent","handle":"parent.bsky.social","avatar":` + quoteJSON(t, "https://cdn.bsky.app.evil.test/img/avatar/plain/x") + // coves:allow-public-host: hostile cached-media fixture; never fetched.
		`},
		"images":[
			{"thumb":` + quoteJSON(t, validThumb) + `,"fullsize":` + quoteJSON(t, validFullsize) + `,"alt":"kept"},
			{"thumb":` + quoteJSON(t, "http://cdn.bsky.app/img/feed_thumbnail/plain/x") + // coves:allow-public-host: hostile cached-media fixture; never fetched.
		`,"fullsize":` + quoteJSON(t, validFullsize) + `,"alt":"bad scheme"},
			{"thumb":` + quoteJSON(t, validThumb) + `,"fullsize":` + quoteJSON(t, validFullsize+"?token=x") + `,"alt":"partial pair"}
		],
		"embed":{"uri":"https://outside.example/parent","title":"Parent card","description":"keep text","thumb":` + quoteJSON(t, validThumb+"#fragment") + `},
		"quotedPost":{
			"createdAt":"2025-12-20T08:00:00Z","uri":"at://did:plc:quoted/app.bsky.feed.post/quoted","cid":"quoted-cid","text":"quoted",
			"mediaCount":2,"hasMedia":true,
			"author":{"did":"did:plc:quoted","handle":"quoted.bsky.social","avatar":` + quoteJSON(t, blueskyMediaCDN+"/img/avatar/plain/quote-avatar") + `},
			"images":[
				{"thumb":` + quoteJSON(t, quotedThumb) + `,"fullsize":` + quoteJSON(t, quotedFullsize) + `,"alt":"quoted kept"},
				{"thumb":` + quoteJSON(t, quotedThumb) + `,"fullsize":"/img/feed_fullsize/plain/relative","alt":"quoted dropped"}
			],
			"embed":{"uri":"https://outside.example/quoted","title":"Quoted card","description":"keep quoted text","thumb":"javascript:alert(1)"}
		}
	}`

	result := decodeBlueskyResult(t, rawResult)
	before, err := json.Marshal(result)
	require.NoError(t, err)
	postView := &PostView{Embed: map[string]interface{}{
		"$type": "social.coves.embed.post",
		"post":  map[string]interface{}{"uri": "at://did:plc:parent/app.bsky.feed.post/parent"},
	}}
	TransformPostEmbeds(context.Background(), postView, &mockBlueskyService{resolvePostResult: result})
	after, err := json.Marshal(result)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after), "filtering must not mutate the cached result")
	resolved := resolvedPostJSON(t, postView)

	author := requireJSONMap(t, resolved["author"])
	assert.NotContains(t, author, "avatar")
	images := requireJSONArray(t, resolved["images"])
	require.Len(t, images, 1, "invalid or partial cached pairs are dropped completely")
	assert.Equal(t, validThumb, requireJSONMap(t, images[0])["thumb"])
	assert.EqualValues(t, 1, resolved["mediaCount"], "fresh image indicators describe valid resolved pairs")
	assert.Equal(t, true, resolved["hasMedia"])
	embed := requireJSONMap(t, resolved["embed"])
	assert.Equal(t, "https://outside.example/parent", embed["uri"])
	assert.Equal(t, "Parent card", embed["title"])
	assert.NotContains(t, embed, "thumb")

	quoted := requireJSONMap(t, resolved["quotedPost"])
	assert.Equal(t, blueskyMediaCDN+"/img/avatar/plain/quote-avatar", requireJSONMap(t, quoted["author"])["avatar"])
	quotedImages := requireJSONArray(t, quoted["images"])
	require.Len(t, quotedImages, 1)
	assert.Equal(t, quotedThumb, requireJSONMap(t, quotedImages[0])["thumb"])
	assert.EqualValues(t, 1, quoted["mediaCount"])
	quotedEmbed := requireJSONMap(t, quoted["embed"])
	assert.Equal(t, "https://outside.example/quoted", quotedEmbed["uri"])
	assert.Equal(t, "Quoted card", quotedEmbed["title"])
	assert.NotContains(t, quotedEmbed, "thumb")
}

func TestTransformPostEmbeds_PreservesLegacyIndicatorsWithoutImages(t *testing.T) {
	legacy := decodeBlueskyResult(t, `{
		"createdAt":"2025-12-21T10:30:00Z","uri":"at://did:plc:legacy/app.bsky.feed.post/legacy","cid":"legacy-cid","text":"legacy cache row",
		"replyCount":4,"repostCount":5,"likeCount":6,"mediaCount":2,"hasMedia":true,
		"author":{"did":"did:plc:legacy","handle":"legacy.bsky.social"}
	}`)
	postView := &PostView{Embed: map[string]interface{}{
		"$type": "social.coves.embed.post",
		"post":  map[string]interface{}{"uri": legacy.URI},
	}}
	TransformPostEmbeds(context.Background(), postView, &mockBlueskyService{resolvePostResult: legacy})
	resolved := resolvedPostJSON(t, postView)

	assert.NotContains(t, resolved, "images")
	assert.EqualValues(t, 2, resolved["mediaCount"])
	assert.Equal(t, true, resolved["hasMedia"])
	assert.Equal(t, "legacy cache row", resolved["text"])
}

func quoteJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func requireJSONMap(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	result, ok := value.(map[string]interface{})
	require.True(t, ok, "value must be a JSON object: %#v", value)
	return result
}

func requireJSONArray(t *testing.T, value interface{}) []interface{} {
	t.Helper()
	result, ok := value.([]interface{})
	require.True(t, ok, "value must be a JSON array: %#v", value)
	return result
}
