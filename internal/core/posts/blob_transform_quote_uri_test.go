package posts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"Coves/internal/core/blueskypost"
)

// recordingBlueskyResolver records every URI it is asked to resolve.
type recordingBlueskyResolver struct{ resolved []string }

func (r *recordingBlueskyResolver) ResolvePost(_ context.Context, uri string) (*blueskypost.BlueskyPostResult, error) {
	r.resolved = append(r.resolved, uri)
	return &blueskypost.BlueskyPostResult{URI: uri, Text: "resolved"}, nil
}

func (r *recordingBlueskyResolver) ParseBlueskyURL(context.Context, string) (string, error) {
	return "", nil
}

func (r *recordingBlueskyResolver) IsBlueskyURL(string) bool { return false }

// Only a URI whose collection segment is app.bsky.feed.post is sent to the
// Bluesky resolver; the substring elsewhere in a URI is not a Bluesky post.
func TestTransformPostEmbeds_ResolvesOnlyBlueskyCollectionURIs(t *testing.T) {
	for uri, wantResolved := range map[string]bool{
		"at://did:plc:quoted/app.bsky.feed.post/3kquoted":                                  true,
		"at://did:plc:quoted/social.coves.community.postv2/app.bsky.feed.post/3kquoted":    false,
		"at://did:plc:quoted/social.coves.community.postv2/3kquoted#/app.bsky.feed.post/1": false,
		"at://app.bsky.feed.post/social.coves.community.postv2/3kquoted":                   false,
	} {
		t.Run(uri, func(t *testing.T) {
			resolver := &recordingBlueskyResolver{}
			embed := map[string]interface{}{
				"$type": "social.coves.embed.post", "post": map[string]interface{}{"uri": uri, "cid": "bafyquoted"},
			}
			TransformPostEmbeds(t.Context(), &PostView{Embed: embed}, resolver)

			if wantResolved {
				assert.Equal(t, []string{uri}, resolver.resolved)
				assert.Equal(t, "social.coves.embed.post#view", embed["$type"])
				assert.Contains(t, embed, "resolved")
				return
			}
			assert.Empty(t, resolver.resolved, "a non-Bluesky collection must not reach the Bluesky resolver")
			assert.Equal(t, map[string]interface{}{
				"$type": "social.coves.embed.post", "post": map[string]interface{}{"uri": uri, "cid": "bafyquoted"},
			}, embed)
		})
	}
}
