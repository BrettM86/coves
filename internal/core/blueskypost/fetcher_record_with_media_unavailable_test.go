package blueskypost

import (
	"encoding/json"
	"testing"

	bsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchBlueskyPost_UnavailableQuoteUnionNesting(t *testing.T) {
	const unavailableURI = "at://did:plc:unavailable/app.bsky.feed.post/unavailable"
	externalMedia := `{"$type":"app.bsky.embed.external#view","external":{"uri":"https://outside.example/card","title":"Card","description":"Description"}}`
	tests := []struct {
		name            string
		union           string
		message         string
		wantAuthor      bool
		variant         string
		recordWithMedia bool
	}{
		{
			name:       "plain blocked",
			union:      `{"$type":"app.bsky.embed.record#viewBlocked","uri":"` + unavailableURI + `","blocked":true,"author":{"did":"did:plc:blocked","viewer":{"blockedBy":true}}}`,
			message:    "This post is from a blocked account",
			wantAuthor: true,
			variant:    "blocked",
		},
		{
			name:    "plain not found",
			union:   `{"$type":"app.bsky.embed.record#viewNotFound","uri":"` + unavailableURI + `","notFound":true}`,
			message: "This post has been deleted",
			variant: "notFound",
		},
		{
			name:    "plain detached",
			union:   `{"$type":"app.bsky.embed.record#viewDetached","uri":"` + unavailableURI + `","detached":true}`,
			message: "This post is unavailable",
			variant: "detached",
		},
		{
			name:            "recordWithMedia blocked",
			union:           `{"$type":"app.bsky.embed.record#viewBlocked","uri":"` + unavailableURI + `","blocked":true,"author":{"did":"did:plc:blocked","viewer":{"blockedBy":true}}}`,
			message:         "This post is from a blocked account",
			wantAuthor:      true,
			variant:         "blocked",
			recordWithMedia: true,
		},
		{
			name:            "recordWithMedia not found",
			union:           `{"$type":"app.bsky.embed.record#viewNotFound","uri":"` + unavailableURI + `","notFound":true}`,
			message:         "This post has been deleted",
			variant:         "notFound",
			recordWithMedia: true,
		},
		{
			name:            "recordWithMedia detached",
			union:           `{"$type":"app.bsky.embed.record#viewDetached","uri":"` + unavailableURI + `","detached":true}`,
			message:         "This post is unavailable",
			variant:         "detached",
			recordWithMedia: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := plainQuoteViewJSON(test.union)
			if test.recordWithMedia {
				view = recordWithMediaViewJSON(externalMedia, test.union)
			}
			postJSON := getPostsPostJSON(t, "", "", view)

			var upstream bsky.FeedGetPosts_Output
			require.NoError(t, json.Unmarshal([]byte(`{"posts":[`+postJSON+`]}`), &upstream))
			require.Len(t, upstream.Posts, 1)
			require.NotNil(t, upstream.Posts[0].Embed)
			var union *bsky.EmbedRecord_View_Record
			if test.recordWithMedia {
				recordWithMedia := upstream.Posts[0].Embed.EmbedRecordWithMedia_View
				require.NotNil(t, recordWithMedia)
				require.NotNil(t, recordWithMedia.Record)
				union = recordWithMedia.Record.Record
			} else {
				record := upstream.Posts[0].Embed.EmbedRecord_View
				require.NotNil(t, record)
				union = record.Record
			}
			require.NotNil(t, union)
			switch test.variant {
			case "blocked":
				require.NotNil(t, union.EmbedRecord_ViewBlocked)
			case "notFound":
				require.NotNil(t, union.EmbedRecord_ViewNotFound)
			case "detached":
				require.NotNil(t, union.EmbedRecord_ViewDetached)
			default:
				require.FailNow(t, "unknown fixture variant", test.variant)
			}

			_, public := fetchPostFixture(t, postJSON)
			quoted := publicObject(t, public["quotedPost"])
			assert.Equal(t, unavailableURI, quoted["uri"])
			assert.Equal(t, true, quoted["unavailable"])
			assert.Equal(t, test.message, quoted["message"])
			assert.NotContains(t, quoted, "images")
			if test.wantAuthor {
				author := publicObject(t, quoted["author"])
				assert.Equal(t, "did:plc:blocked", author["did"])
			} else {
				assert.NotContains(t, quoted, "author")
			}
			require.NotNil(t, quoted)
		})
	}
}
