package blueskypost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const resolvedMediaCDN = "https://cdn.bsky.app" // coves:allow-public-host: inert URLs in canned getPosts responses served only by httptest.

const (
	testPostURI = "at://did:plc:parent/app.bsky.feed.post/parent"
	testPostCID = "bafyreij6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqe"
	testRawCID  = "bafyreih6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqa"
)

func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func getPostsPostJSON(t *testing.T, avatar, recordEmbed, viewEmbed string) string {
	t.Helper()
	avatarField := ""
	if avatar != "" {
		avatarField = `,"avatar":` + jsonString(t, avatar)
	}
	recordEmbedField := ""
	if recordEmbed != "" {
		recordEmbedField = `,"embed":` + recordEmbed
	}
	viewEmbedField := ""
	if viewEmbed != "" {
		viewEmbedField = `,"embed":` + viewEmbed
	}
	return `{
		"uri":"` + testPostURI + `","cid":"` + testPostCID + `",
		"author":{"did":"did:plc:parent","handle":"parent.bsky.social","displayName":"Parent"` + avatarField + `},
		"record":{"$type":"app.bsky.feed.post","text":"parent text","createdAt":"2025-12-21T10:30:00.123456Z"` + recordEmbedField + `},
		"replyCount":3,"repostCount":5,"likeCount":7,"indexedAt":"2025-12-21T10:30:01Z"` + viewEmbedField + `
	}`
}

func fetchPostFixture(t *testing.T, postJSON string) (*BlueskyPostResult, map[string]interface{}) {
	t.Helper()
	responseJSON := `{"posts":[` + postJSON + `]}`
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		assert.Equal(t, http.MethodGet, request.Method)
		assert.Equal(t, "/xrpc/app.bsky.feed.getPosts", request.URL.Path)
		assert.Equal(t, []string{testPostURI}, request.URL.Query()["uris"])
		assert.Len(t, request.URL.Query(), 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseJSON))
	}))
	t.Cleanup(server.Close)

	result, err := fetchBlueskyPost(context.Background(), testPostURI, time.Second, blueskyAPI{
		baseURL:          server.URL,
		allowPrivateHost: true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, requests, "the only request is getPosts metadata; media URLs are never fetched")

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	var public map[string]interface{}
	require.NoError(t, json.Unmarshal(encoded, &public))
	return result, public
}

func publicImages(t *testing.T, post map[string]interface{}) []interface{} {
	t.Helper()
	images, ok := post["images"].([]interface{})
	require.True(t, ok, "serialized images must be an array: %#v", post["images"])
	return images
}

func publicObject(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	object, ok := value.(map[string]interface{})
	require.True(t, ok, "serialized value must be an object: %#v", value)
	return object
}

func rawMisleadingImageRecord() string {
	return `{"$type":"app.bsky.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"` + testRawCID + `"},"mimeType":"image/jpeg","size":123},"alt":"raw blob must not win"}]}`
}

func imageViewJSON(t *testing.T, thumb, fullsize, alt, aspectRatio string) string {
	t.Helper()
	aspect := ""
	if aspectRatio != "" {
		aspect = `,"aspectRatio":` + aspectRatio
	}
	return `{"$type":"app.bsky.embed.images#view","images":[{"thumb":` + jsonString(t, thumb) +
		`,"fullsize":` + jsonString(t, fullsize) + `,"alt":` + jsonString(t, alt) + aspect + `}]}`
}

func TestFetchBlueskyPost_UsesResolvedImageViews(t *testing.T) {
	thumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/opaque-thumb@jpeg"
	fullsize := resolvedMediaCDN + "/img/feed_fullsize/plain/opaque-fullsize@webp"
	postJSON := getPostsPostJSON(t, "", rawMisleadingImageRecord(),
		imageViewJSON(t, thumb, fullsize, "", `{"width":16,"height":9}`))

	var upstream bsky.FeedGetPosts_Output
	require.NoError(t, json.Unmarshal([]byte(`{"posts":[`+postJSON+`]}`), &upstream))
	require.Len(t, upstream.Posts, 1)
	require.NotNil(t, upstream.Posts[0].Embed)
	require.NotNil(t, upstream.Posts[0].Embed.EmbedImages_View,
		"pinned Indigo must recognize the fixture as app.bsky.embed.images#view")
	require.Len(t, upstream.Posts[0].Embed.EmbedImages_View.Images, 1)
	assert.Equal(t, thumb, upstream.Posts[0].Embed.EmbedImages_View.Images[0].Thumb)

	_, public := fetchPostFixture(t, postJSON)

	assert.Equal(t, "2025-12-21T10:30:00.123456Z", public["createdAt"])
	assert.EqualValues(t, 1, public["mediaCount"])
	assert.Equal(t, true, public["hasMedia"])
	images := publicImages(t, public)
	require.Len(t, images, 1)
	image := publicObject(t, images[0])
	assert.Equal(t, thumb, image["thumb"])
	assert.Equal(t, fullsize, image["fullsize"])
	assert.Contains(t, image, "alt", "the required alt field survives even when empty")
	assert.Equal(t, "", image["alt"])
	assert.Equal(t, map[string]interface{}{"width": float64(16), "height": float64(9)}, image["aspectRatio"])
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), testRawCID, "raw record blob refs are never a media fallback")
}

func TestFetchBlueskyPost_MissingResolvedImagesIgnoresRawBlobs(t *testing.T) {
	_, public := fetchPostFixture(t, getPostsPostJSON(t, "", rawMisleadingImageRecord(), ""))

	assert.NotContains(t, public, "images")
	assert.EqualValues(t, 0, public["mediaCount"])
	assert.Equal(t, false, public["hasMedia"])
}

func TestFetchBlueskyPost_ImageURLTrustBoundary(t *testing.T) {
	validThumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/opaque-thumb"
	validFullsize := resolvedMediaCDN + "/img/feed_fullsize/plain/opaque/fullsize@jpeg"
	productionThumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/did:plc:ewvi7nxzyoun6zhxrhs64oiz/bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku@jpeg"
	productionFullsize := resolvedMediaCDN + "/img/feed_fullsize/plain/did:plc:ewvi7nxzyoun6zhxrhs64oiz/bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku@jpeg"
	tests := []struct {
		name     string
		thumb    string
		fullsize string
		want     bool
	}{
		{name: "valid opaque identifiers", thumb: validThumb, fullsize: validFullsize, want: true},
		{name: "http scheme", thumb: "http://cdn.bsky.app/img/feed_thumbnail/plain/x", fullsize: validFullsize},            // coves:allow-public-host: hostile fixture URL; never fetched.
		{name: "host suffix", thumb: "https://cdn.bsky.app.evil.test/img/feed_thumbnail/plain/x", fullsize: validFullsize}, // coves:allow-public-host: hostile fixture URL; never fetched.
		{name: "userinfo", thumb: "https://user@cdn.bsky.app/img/feed_thumbnail/plain/x", fullsize: validFullsize},         // coves:allow-public-host: hostile fixture URL; never fetched.
		{name: "explicit port", thumb: "https://cdn.bsky.app:443/img/feed_thumbnail/plain/x", fullsize: validFullsize},     // coves:allow-public-host: hostile fixture URL; never fetched.
		{name: "relative", thumb: "/img/feed_thumbnail/plain/x", fullsize: validFullsize},
		{name: "missing preset", thumb: resolvedMediaCDN + "/img//opaque", fullsize: validFullsize},
		{name: "missing rest", thumb: resolvedMediaCDN + "/img/feed_thumbnail/", fullsize: validFullsize},
		{name: "query", thumb: validThumb + "?token=x", fullsize: validFullsize},
		{name: "fragment", thumb: validThumb + "#x", fullsize: validFullsize},
		{name: "backslash", thumb: resolvedMediaCDN + `/img/feed_thumbnail/plain\evil`, fullsize: validFullsize},
		{name: "control character", thumb: resolvedMediaCDN + "/img/feed_thumbnail/plain/x\nheader", fullsize: validFullsize},
		{name: "encoded slash", thumb: resolvedMediaCDN + "/img/feed_thumbnail/plain%2Fevil", fullsize: validFullsize},
		{name: "encoded backslash", thumb: resolvedMediaCDN + "/img/feed_thumbnail/plain%5Cevil", fullsize: validFullsize},
		{name: "encoded dot traversal", thumb: resolvedMediaCDN + "/img/feed_thumbnail/%2e%2e/evil", fullsize: validFullsize},
		{name: "dot segment", thumb: resolvedMediaCDN + "/img/feed_thumbnail/../evil", fullsize: validFullsize},
		{name: "unsafe fullsize drops complete pair", thumb: validThumb, fullsize: "https://images.example.test/full"},
		{name: "production shaped identifiers", thumb: productionThumb, fullsize: productionFullsize, want: true},
		{name: "double encoded slash", thumb: resolvedMediaCDN + "/img/feed_thumbnail/plain%252Fevil", fullsize: validFullsize},
		{name: "double encoded dot traversal", thumb: resolvedMediaCDN + "/img/feed_thumbnail/%252e%252e/evil", fullsize: validFullsize},
		{name: "uppercase authority", thumb: "https://CDN.BSKY.APP/img/feed_thumbnail/plain/x", fullsize: validFullsize}, // coves:allow-public-host: hostile fixture URL; never fetched.
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, public := fetchPostFixture(t, getPostsPostJSON(t, "", "",
				imageViewJSON(t, test.thumb, test.fullsize, "alt", "")))
			if test.want {
				images := publicImages(t, public)
				require.Len(t, images, 1)
				image := publicObject(t, images[0])
				assert.Equal(t, test.thumb, image["thumb"], "accepted strings are unchanged")
				assert.Equal(t, test.fullsize, image["fullsize"], "accepted strings are unchanged")
				assert.EqualValues(t, 1, public["mediaCount"])
				assert.Equal(t, true, public["hasMedia"])
				return
			}
			assert.NotContains(t, public, "images", "an unsafe half drops the complete image entry")
			assert.EqualValues(t, 0, public["mediaCount"])
			assert.Equal(t, false, public["hasMedia"])
		})
	}
}

func TestFetchBlueskyPost_InvalidAspectRatioIsOmitted(t *testing.T) {
	thumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/aspect-thumb"
	fullsize := resolvedMediaCDN + "/img/feed_fullsize/plain/aspect-fullsize"
	tests := []struct {
		name   string
		aspect string
		want   bool
	}{
		{name: "positive", aspect: `{"width":4,"height":3}`, want: true},
		{name: "zero width", aspect: `{"width":0,"height":3}`},
		{name: "negative height", aspect: `{"width":4,"height":-1}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, public := fetchPostFixture(t, getPostsPostJSON(t, "", "", imageViewJSON(t, thumb, fullsize, "alt", test.aspect)))
			image := publicObject(t, publicImages(t, public)[0])
			if test.want {
				assert.Contains(t, image, "aspectRatio")
			} else {
				assert.NotContains(t, image, "aspectRatio")
			}
		})
	}
}
