package embeds

import (
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/embedblobs"
)

func referenceCIDs(references []embedblobs.Reference) []string {
	var cids []string
	seen := make(map[string]bool)
	for _, reference := range references {
		if !seen[reference.CID] {
			seen[reference.CID] = true
			cids = append(cids, reference.CID)
		}
	}
	return cids
}

func TestExtractorsAgreeWithLeaf(t *testing.T) {
	parsed, err := cid.Decode(testCID)
	require.NoError(t, err)
	alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	legacy := map[string]interface{}{"cid": secondTestCID, "mimeType": "image/png"}
	quote := map[string]interface{}{"$type": TypePost, "post": map[string]interface{}{"embed": imagesEmbed(imageEntry(blobRef(testCID)))}}

	for _, test := range []struct {
		name    string
		comment bool
		embed   map[string]interface{}
		want    []string
	}{
		{name: "post images", embed: imagesEmbed(imageEntry(blobRef(testCID)), imageEntry(blobRef(secondTestCID))), want: []string{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}},
		{name: "post external thumb and gallery", embed: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
			"thumb": blobRef(testCID), "images": []interface{}{imageEntry(blobRef(secondTestCID))},
		}}, want: []string{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}},
		{name: "post external thumb is also gallery CID", embed: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{
			"thumb": blobRef(testCID), "images": []interface{}{imageEntry(blobRef(testCID))},
		}}, want: []string{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}},
		{name: "post video thumbnail", embed: map[string]interface{}{"$type": TypeVideo, "video": blobRef(testCID), "thumbnail": blobRef(secondTestCID)}, want: []string{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}},
		{name: "post legacy CID", embed: imagesEmbed(imageEntry(legacy)), want: []string{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}},
		{name: "post base58 alias", embed: imagesEmbed(imageEntry(blobRef(alias))), want: []string{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}},
		{name: "post duplicates", embed: imagesEmbed(imageEntry(blobRef(testCID)), imageEntry(map[string]interface{}{"cid": alias})), want: []string{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}},
		{name: "post malformed sibling", embed: imagesEmbed(imageEntry(blobRef(testCID)), "bad entry", imageEntry(blobRef("bafynotacid")), imageEntry(blobRef(secondTestCID))), want: []string{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}},
		{name: "post quote", embed: quote},
		{name: "comment images", comment: true, embed: imagesEmbed(imageEntry(blobRef(testCID)), imageEntry(blobRef(secondTestCID))), want: []string{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}},
		{name: "comment legacy CID", comment: true, embed: imagesEmbed(imageEntry(legacy)), want: []string{"bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"}},
		{name: "comment base58 alias", comment: true, embed: imagesEmbed(imageEntry(blobRef(alias))), want: []string{"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"}},
		{name: "comment quote", comment: true, embed: quote},
	} {
		t.Run(test.name, func(t *testing.T) {
			var existing []string
			var references []embedblobs.Reference
			if test.comment {
				existing = CommentImageCIDs(test.embed)
				references = embedblobs.CommentReferences(test.embed)
			} else {
				existing = PostBlobCIDs(test.embed)
				references = embedblobs.PostReferences(test.embed)
			}
			require.Equal(t, test.want, existing)
			require.Equal(t, existing, referenceCIDs(references))
		})
	}
}
