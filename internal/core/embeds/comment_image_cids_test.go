package embeds

import (
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const secondTestCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"

func imageEntry(image interface{}) map[string]interface{} {
	return map[string]interface{}{"alt": "an image", "image": image}
}

func imagesEmbed(entries ...interface{}) map[string]interface{} {
	return map[string]interface{}{"$type": TypeImages, "images": entries}
}

func TestCommentImageCIDs(t *testing.T) {
	parsed, err := cid.Decode(testCID)
	require.NoError(t, err)
	require.Equal(t, testCID, parsed.String(), "the fixture must already be canonical")
	base58Alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	const cidV0 = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"

	for _, test := range []struct {
		name  string
		embed map[string]interface{}
		want  []string
	}{
		{name: "nil embed", embed: nil},
		{name: "ref link blobs in order", embed: imagesEmbed(imageEntry(blobRef(testCID)), imageEntry(blobRef(secondTestCID))), want: []string{testCID, secondTestCID}},
		{name: "legacy top-level cid blob", embed: imagesEmbed(imageEntry(map[string]interface{}{"cid": testCID, "mimeType": "image/png"})), want: []string{testCID}},
		{name: "non-canonical encoding is canonicalized", embed: imagesEmbed(imageEntry(blobRef(base58Alias))), want: []string{testCID}},
		{name: "CIDv0 keeps its canonical form", embed: imagesEmbed(imageEntry(blobRef(cidV0))), want: []string{cidV0}},
		{name: "duplicates across encodings collapse", embed: imagesEmbed(imageEntry(blobRef(testCID)), imageEntry(map[string]interface{}{"cid": base58Alias}), imageEntry(blobRef(testCID))), want: []string{testCID}},
		{name: "view type is not a record", embed: map[string]interface{}{"$type": TypeImages + viewSuffix, "images": []interface{}{imageEntry(blobRef(testCID))}}},
		{name: "external embed is outside the comment union", embed: map[string]interface{}{"$type": TypeExternal, "external": map[string]interface{}{"thumb": blobRef(testCID)}}},
		{name: "video embed is outside the comment union", embed: map[string]interface{}{"$type": TypeVideo, "video": blobRef(testCID), "thumbnail": blobRef(secondTestCID)}},
		{name: "missing type", embed: map[string]interface{}{"images": []interface{}{imageEntry(blobRef(testCID))}}},
		{name: "non-string type", embed: map[string]interface{}{"$type": 5, "images": []interface{}{imageEntry(blobRef(testCID))}}},
		{name: "images is a string", embed: map[string]interface{}{"$type": TypeImages, "images": "x"}},
		{name: "images is an object", embed: map[string]interface{}{"$type": TypeImages, "images": map[string]interface{}{"image": blobRef(testCID)}}},
		{name: "empty images", embed: imagesEmbed()},
		{
			name: "malformed entries are skipped around a valid one",
			embed: imagesEmbed(
				"x", 7, nil,
				imageEntry("x"),
				imageEntry(map[string]interface{}{"ref": testCID}),
				imageEntry(map[string]interface{}{"ref": map[string]interface{}{"$link": 5}}),
				imageEntry(map[string]interface{}{"ref": map[string]interface{}{}, "cid": testCID}),
				imageEntry(blobRef("bafynotacid")),
				imageEntry(blobRef("")),
				imageEntry(blobRef(secondTestCID)),
			),
			want: []string{secondTestCID},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, CommentImageCIDs(test.embed))
		})
	}
}

// Every CID the enumerator returns is one HydrateCommentView turns into a
// proxy URL for the same embed, so blocking covers what serving emits.
func TestCommentImageCIDsMatchServedProxyURLs(t *testing.T) {
	withProxy(t, "https://img.coves.social")
	embed := imagesEmbed(imageEntry(blobRef(testCID)), imageEntry(map[string]interface{}{"cid": secondTestCID}))
	cids := CommentImageCIDs(embed)
	require.Equal(t, []string{testCID, secondTestCID}, cids)

	HydrateCommentView(embed, testDID, testPDS)
	images, ok := embed["images"].([]interface{})
	require.True(t, ok)
	require.Len(t, images, len(cids))
	for index, entry := range images {
		image, isObject := entry.(map[string]interface{})
		require.True(t, isObject)
		assert.Contains(t, image["thumb"], "/"+cids[index])
		assert.Contains(t, image["fullsize"], "/"+cids[index])
	}
}
