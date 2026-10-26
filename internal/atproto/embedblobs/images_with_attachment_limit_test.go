package embedblobs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImagesWithAttachmentNestedImageLimit(t *testing.T) {
	for _, test := range []struct {
		name          string
		galleryCount  int
		externalCount int
		count         int
		exceeds       bool
	}{
		{"gallery eight", 8, 0, 8, false},
		{"gallery nine", 9, 0, 9, true},
		{"external gallery eight", 0, 8, 8, false},
		{"external gallery nine", 0, 9, 9, true},
		{"combined galleries four plus four", 4, 4, 8, false},
		{"combined galleries five plus four", 5, 4, 9, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Raw entries count before CID extraction; malformed leaves cannot
			// evade the firehose/backfill cap across both galleries combined.
			entries := []interface{}{testImage(testBlob(firstCID)), nil, "not an image", map[string]interface{}{}, testImage("not a blob"), nil, nil, nil, testImage(testBlob("not-a-cid"))}
			embed := map[string]interface{}{
				"$type":  "social.coves.embed.imagesWithAttachment",
				"images": testImages(entries[:test.galleryCount]...),
				"attachment": map[string]interface{}{
					"$type": "social.coves.embed.post",
					"post":  map[string]interface{}{"uri": "at://did:plc:quoted/social.coves.community.postv2/3kquoted", "cid": secondCID},
				},
			}
			if test.externalCount > 0 {
				embed["attachment"] = map[string]interface{}{
					"$type":    TypeExternal,
					"external": map[string]interface{}{"images": entries[:test.externalCount]},
				}
			}
			assert.Equal(t, test.count, ImageCount(embed), "nested raw entries must count before malformed blobs are skipped")
			assert.Equal(t, test.exceeds, ExceedsImageLimit(embed), "shared firehose/backfill limit must cap both nested galleries combined")
		})
	}
}
