package comments

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/blobs"
	"Coves/internal/core/users"
)

// A comment's served embed carries only the proxy URLs HydrateCommentView
// derives from blobs, the same CIDs CommentImageCIDs blocks. URL strings the
// author wrote into the record are not served in the view; the record keeps
// them verbatim.
func TestBuildCommentView_ServesOnlyBlobDerivedMedia(t *testing.T) {
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.coves.social"})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)

	const (
		commenterDID = "did:plc:commenter123"
		authorCID    = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
		quotedURI    = "at://did:plc:quoted/social.coves.community.postv2/3kquoted"
	)
	authorURL := "https://img.coves.social/img/content_preview/plain/" + commenterDID + "/" + authorCID
	postURI := "at://did:plc:post123/social.coves.community.postv2/test"

	for _, test := range []struct {
		name  string
		embed map[string]interface{}
		want  interface{}
	}{
		{
			name: "stored images#view is not served",
			embed: map[string]interface{}{"$type": "social.coves.embed.images#view", "images": []interface{}{
				map[string]interface{}{"thumb": authorURL, "fullsize": authorURL, "alt": "forged"},
			}},
		},
		{
			name: "images record entry with view URLs and no blob keeps only its alt",
			embed: map[string]interface{}{"$type": "social.coves.embed.images", "images": []interface{}{
				map[string]interface{}{"thumb": authorURL, "fullsize": authorURL, "alt": "forged"},
			}},
			want: map[string]interface{}{"$type": "social.coves.embed.images", "images": []interface{}{
				map[string]interface{}{"alt": "forged"},
			}},
		},
		{
			name: "quote view with a stored resolved keeps only its strongRef",
			embed: map[string]interface{}{
				"$type": "social.coves.embed.post#view", "post": map[string]interface{}{"uri": quotedURI, "cid": "bafyquoted"},
				"resolved": map[string]interface{}{"images": []interface{}{map[string]interface{}{"thumb": authorURL}}},
			},
			want: map[string]interface{}{
				"$type": "social.coves.embed.post", "post": map[string]interface{}{"uri": quotedURI, "cid": "bafyquoted"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.embed)
			require.NoError(t, err)
			embedJSON := string(encoded)
			comment := createTestComment("at://"+commenterDID+"/social.coves.community.comment/1", commenterDID, "commenter.test", postURI, postURI, 0)
			comment.Embed = &embedJSON
			service := NewCommentService(newMockCommentRepo(), newMockUserRepo(), newMockPostRepo(), newMockCommunityRepo(), nil, nil, nil).(*commentService)

			view := service.buildCommentView(comment, nil, nil, map[string]*users.User{
				commenterDID: {DID: commenterDID, Handle: "commenter.test", PDSURL: "https://pds.example.com"},
			})

			if test.want == nil {
				assert.Nil(t, view.Embed)
			} else {
				assert.Equal(t, test.want, view.Embed)
			}
			record, ok := view.Record.(*CommentRecord)
			require.True(t, ok)
			assert.Equal(t, test.embed, record.Embed, "the comment record keeps the stored embed verbatim")
		})
	}
}
