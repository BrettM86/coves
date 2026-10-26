package embedblobs

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImagesWithAttachmentPostReferences(t *testing.T) {
	embed := map[string]interface{}{
		"$type":  "social.coves.embed.imagesWithAttachment",
		"images": testImages(testImage(testBlob(testBase58Alias(t))), testImage(testBlob(secondCID))),
		"attachment": map[string]interface{}{
			"$type": TypeExternal,
			"external": map[string]interface{}{
				"thumb":  testBlob(firstCID),
				"images": []interface{}{testImage(testBlob(secondCID)), testImage(map[string]interface{}{"cid": thirdCID})},
			},
		},
	}
	require.Equal(t, []Reference{
		{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "post_image"},
		{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "post_image"},
		{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "external_thumb"},
		{"QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG", "post_image"},
	}, PostReferences(embed))
}

func TestImagesWithAttachmentCommentReferences(t *testing.T) {
	embed := map[string]interface{}{
		"$type":  "social.coves.embed.imagesWithAttachment",
		"images": testImages(testImage(testBlob(testBase58Alias(t))), testImage(map[string]interface{}{"cid": secondCID}), testImage(testBlob(firstCID))),
		"attachment": map[string]interface{}{
			"$type": "social.coves.embed.post",
			"post":  map[string]interface{}{"uri": "at://did:plc:quoted/social.coves.community.postv2/3kquoted", "cid": thirdCID},
		},
	}
	require.Equal(t, []Reference{
		{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "comment_image"},
		{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "comment_image"},
	}, CommentReferences(embed))
}
