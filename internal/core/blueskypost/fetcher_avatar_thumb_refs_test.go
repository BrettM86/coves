package blueskypost

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchBlueskyPost_AvatarAndExternalThumbnailTrust(t *testing.T) {
	validAvatar := resolvedMediaCDN + "/img/avatar_thumbnail/plain/opaque-avatar"
	validThumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/opaque-card-thumb"
	destination := "https://outside.example/article?keep=this#destination"

	tests := []struct {
		name       string
		avatar     string
		thumb      string
		wantAvatar bool
		wantThumb  bool
	}{
		{name: "valid CDN media survives unchanged", avatar: validAvatar, thumb: validThumb, wantAvatar: true, wantThumb: true},
		{name: "foreign avatar is omitted only", avatar: "https://images.example.test/avatar", thumb: validThumb, wantThumb: true},
		{name: "foreign card thumb is omitted only", avatar: validAvatar, thumb: "https://images.example.test/card", wantAvatar: true},
		{name: "avatar query is omitted only", avatar: validAvatar + "?token=x", thumb: validThumb, wantThumb: true},
		{name: "card thumb port is omitted only", avatar: validAvatar, thumb: "https://cdn.bsky.app:443/img/feed_thumbnail/plain/x", wantAvatar: true}, // coves:allow-public-host: hostile fixture URL; never fetched.
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := `{"$type":"app.bsky.embed.external#view","external":{"uri":` + jsonString(t, destination) +
				`,"title":"Title","description":"Description","thumb":` + jsonString(t, test.thumb) + `}}`
			result, public := fetchPostFixture(t, getPostsPostJSON(t, test.avatar, "", view))

			require.NotNil(t, result.Author)
			assert.Equal(t, "did:plc:parent", result.Author.DID)
			assert.Equal(t, "parent.bsky.social", result.Author.Handle)
			assert.Equal(t, "Parent", result.Author.DisplayName)
			if test.wantAvatar {
				assert.Equal(t, test.avatar, result.Author.Avatar, "valid typed avatar survives unchanged")
			} else {
				assert.Empty(t, result.Author.Avatar, "invalid typed avatar is omitted at the fetch boundary")
			}

			author := publicObject(t, public["author"])
			assert.Equal(t, "did:plc:parent", author["did"])
			assert.Equal(t, "parent.bsky.social", author["handle"])
			if test.wantAvatar {
				assert.Equal(t, test.avatar, author["avatar"])
			} else {
				assert.NotContains(t, author, "avatar")
			}

			embed := publicObject(t, public["embed"])
			assert.Equal(t, destination, embed["uri"], "external destinations are not CDN-restricted")
			assert.Equal(t, "Title", embed["title"])
			assert.Equal(t, "Description", embed["description"])
			if test.wantThumb {
				assert.Equal(t, test.thumb, embed["thumb"])
			} else {
				assert.NotContains(t, embed, "thumb")
			}
		})
	}
}

func TestFetchBlueskyPost_QuotedAvatarAndCardThumbnail(t *testing.T) {
	quotedAvatar := resolvedMediaCDN + "/img/avatar/plain/opaque-quoted-avatar"
	quotedThumb := resolvedMediaCDN + "/img/feed_thumbnail/plain/opaque-quoted-card"
	quoted := `{
		"$type":"app.bsky.embed.record#view",
		"record":{"$type":"app.bsky.embed.record#viewRecord",
			"uri":"at://did:plc:quoted/app.bsky.feed.post/quoted","cid":"quoted-cid",
			"author":{"did":"did:plc:quoted","handle":"quoted.bsky.social","avatar":` + jsonString(t, quotedAvatar) + `},
			"value":{"$type":"app.bsky.feed.post","text":"quoted text","createdAt":"2025-12-20T08:00:00Z"},
			"embeds":[{"$type":"app.bsky.embed.external#view","external":{"uri":"https://outside.example/quoted","title":"Quoted title","description":"Quoted description","thumb":` + jsonString(t, quotedThumb) + `}}],
			"indexedAt":"2025-12-20T08:00:01Z"
		}
	}`

	result, public := fetchPostFixture(t, getPostsPostJSON(t, "", "", quoted))
	require.NotNil(t, result.QuotedPost)
	require.NotNil(t, result.QuotedPost.Author)
	assert.Equal(t, "did:plc:quoted", result.QuotedPost.Author.DID)
	assert.Equal(t, "quoted.bsky.social", result.QuotedPost.Author.Handle)
	assert.Equal(t, quotedAvatar, result.QuotedPost.Author.Avatar, "valid typed quoted avatar survives unchanged")
	quote := publicObject(t, public["quotedPost"])
	author := publicObject(t, quote["author"])
	assert.Equal(t, quotedAvatar, author["avatar"])
	embed := publicObject(t, quote["embed"])
	assert.Equal(t, "https://outside.example/quoted", embed["uri"])
	assert.Equal(t, quotedThumb, embed["thumb"])
}

func TestFetchBlueskyPost_InvalidQuotedMediaFieldsDoNotDropContent(t *testing.T) {
	quoted := `{
		"$type":"app.bsky.embed.record#view",
		"record":{"$type":"app.bsky.embed.record#viewRecord",
			"uri":"at://did:plc:quoted/app.bsky.feed.post/quoted","cid":"quoted-cid",
			"author":{"did":"did:plc:quoted","handle":"quoted.bsky.social","displayName":"Quoted","avatar":"https://images.example.test/avatar"},
			"value":{"$type":"app.bsky.feed.post","text":"quoted text","createdAt":"2025-12-20T08:00:00Z"},
			"embeds":[{"$type":"app.bsky.embed.external#view","external":{"uri":"https://outside.example/quoted","title":"Quoted title","description":"Quoted description","thumb":"javascript:alert(1)"}}],
			"likeCount":19,"indexedAt":"2025-12-20T08:00:01Z"
		}
	}`

	result, public := fetchPostFixture(t, getPostsPostJSON(t, "", "", quoted))
	require.NotNil(t, result.QuotedPost)
	require.NotNil(t, result.QuotedPost.Author)
	assert.Equal(t, "did:plc:quoted", result.QuotedPost.Author.DID)
	assert.Equal(t, "quoted.bsky.social", result.QuotedPost.Author.Handle)
	assert.Equal(t, "Quoted", result.QuotedPost.Author.DisplayName)
	assert.Empty(t, result.QuotedPost.Author.Avatar, "invalid typed quoted avatar is omitted at the fetch boundary")
	quote := publicObject(t, public["quotedPost"])
	assert.Equal(t, "quoted text", quote["text"])
	assert.EqualValues(t, 19, quote["likeCount"])
	author := publicObject(t, quote["author"])
	assert.Equal(t, "Quoted", author["displayName"])
	assert.NotContains(t, author, "avatar")
	embed := publicObject(t, quote["embed"])
	assert.Equal(t, "https://outside.example/quoted", embed["uri"])
	assert.Equal(t, "Quoted title", embed["title"])
	assert.Equal(t, "Quoted description", embed["description"])
	assert.NotContains(t, embed, "thumb")
	require.NotNil(t, quote)
}
