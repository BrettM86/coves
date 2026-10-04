package embeds

import (
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostBlobCIDs(t *testing.T) {
	parsed, err := cid.Decode(testCID)
	require.NoError(t, err)
	base58Alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	legacyBlob := map[string]interface{}{"cid": secondTestCID, "mimeType": "image/png"}

	for _, test := range []struct {
		name  string
		embed map[string]interface{}
		want  []string
	}{
		{name: "nil embed", embed: nil},
		{name: "empty embed", embed: map[string]interface{}{}},
		{name: "images retain first-seen order", embed: imagesEmbed(imageEntry(blobRef(testCID)), imageEntry(blobRef(secondTestCID))), want: []string{testCID, secondTestCID}},
		{name: "video thumbnail is proxied but video is not", embed: map[string]interface{}{
			"$type": TypeVideo, "video": blobRef(testCID), "thumbnail": blobRef(secondTestCID),
		}, want: []string{secondTestCID}},
		{name: "video without thumbnail has no proxy blob", embed: map[string]interface{}{"$type": TypeVideo, "video": blobRef(testCID)}},
		{name: "external thumbnail and gallery", embed: map[string]interface{}{
			"$type": TypeExternal, "external": map[string]interface{}{
				"thumb": blobRef(testCID), "images": []interface{}{imageEntry(legacyBlob)},
			},
		}, want: []string{testCID, secondTestCID}},
		{name: "legacy top-level cid", embed: imagesEmbed(imageEntry(legacyBlob)), want: []string{secondTestCID}},
		{name: "noncanonical CID becomes canonical", embed: imagesEmbed(imageEntry(blobRef(base58Alias))), want: []string{testCID}},
		{name: "duplicate encodings keep first occurrence", embed: imagesEmbed(
			imageEntry(blobRef(secondTestCID)), imageEntry(blobRef(testCID)),
			imageEntry(blobRef(base58Alias)), imageEntry(legacyBlob),
		), want: []string{secondTestCID, testCID}},
		{name: "malformed image siblings are skipped", embed: imagesEmbed(
			"not an image", imageEntry("not a blob"), imageEntry(map[string]interface{}{"ref": map[string]interface{}{"$link": 5}}),
			imageEntry(blobRef("bafynotacid")), imageEntry(blobRef(testCID)),
		), want: []string{testCID}},
		{name: "malformed external siblings are skipped", embed: map[string]interface{}{
			"$type": TypeExternal, "external": map[string]interface{}{
				"thumb":  map[string]interface{}{"ref": map[string]interface{}{}},
				"images": []interface{}{imageEntry(blobRef(testCID)), nil, imageEntry(blobRef("bad cid")), imageEntry(legacyBlob)},
			},
		}, want: []string{testCID, secondTestCID}},
		{name: "quote embeds do not expose blobs", embed: map[string]interface{}{
			"$type": TypePost, "post": map[string]interface{}{"embed": imagesEmbed(imageEntry(blobRef(testCID)))},
		}},
		{name: "projected images are not indexed blobs", embed: map[string]interface{}{
			"$type": TypeImages + viewSuffix, "images": []interface{}{imageEntry(blobRef(testCID))},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, PostBlobCIDs(test.embed))
		})
	}
}
