package classification_test

import (
	"fmt"
	"strings"
	"testing"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
)

// A subject's score for each question is the highest any of its batches gave
// (R4).
func TestMaxProbabilities(t *testing.T) {
	got := classification.MaxProbabilities([]map[string]float64{
		{"sexual_explicit": 0.1, "spam_scam": 0.7, "violence": 0.2},
		{"sexual_explicit": 0.9, "spam_scam": 0.3},
		{"sexual_explicit": 0.4, "spam_scam": 0.5, "violence": 0.05},
	})

	assert.Equal(t, map[string]float64{"sexual_explicit": 0.9, "spam_scam": 0.7, "violence": 0.2}, got)
}

const (
	imageCIDA = "bafkreiam6rl6essht4bp2tjukqbyt5za6cah3t7zfj2weeebmwzgg7vif4"
	imageCIDB = "bafkreic2a4l4wzmwi2hkdx72qyar7gypjfzurwaeegbvwulzt6nowrkwii"
	imageCIDC = "bafkreigz6mj254wzpzmn54crd7oboujnkptlgdkxrbqk4bfvfcggui4qca"
)

// blobJSON is an atproto blob reference to cid.
func blobJSON(cid string) string {
	return fmt.Sprintf(`{"$type":"blob","ref":{"$link":%q},"mimeType":"image/jpeg","size":1024}`, cid)
}

// galleryJSON is a social.coves.embed.images embed of the cids.
func galleryJSON(cids ...string) string {
	entries := make([]string, 0, len(cids))
	for _, cid := range cids {
		entries = append(entries, fmt.Sprintf(`{"image":%s,"alt":"a photo"}`, blobJSON(cid)))
	}
	return `{"$type":"social.coves.embed.images","images":[` + strings.Join(entries, ",") + `]}`
}

// Post images are the gallery and a blob link thumbnail, owned by the post's
// blob owner; a link thumbnail that is not a blob, and a video, are skipped
// and counted, and nothing of a video is sent (R4).
func TestPostImages(t *testing.T) {
	const owner = "did:plc:author"
	for _, test := range []struct {
		name        string
		embed       string
		wantRefs    []classification.ImageRef
		wantSkipped int
	}{
		{
			name:  "gallery of blobs",
			embed: galleryJSON(imageCIDA, imageCIDB),
			wantRefs: []classification.ImageRef{
				{OwnerDID: owner, CID: imageCIDA},
				{OwnerDID: owner, CID: imageCIDB},
			},
		},
		{
			name: "gallery with an external attachment's blob thumbnail",
			embed: `{"$type":"social.coves.embed.imagesWithAttachment","images":` + galleryJSON(imageCIDA, imageCIDB) +
				`,"attachment":{"$type":"social.coves.embed.external","external":{"uri":"https://example.com/seals","title":"Seal survey","thumb":` + blobJSON(imageCIDC) + `}}}`,
			wantRefs: []classification.ImageRef{
				{OwnerDID: owner, CID: imageCIDA},
				{OwnerDID: owner, CID: imageCIDB},
				{OwnerDID: owner, CID: imageCIDC},
			},
		},
		{
			name:        "external link thumbnail that is a URL, not a blob",
			embed:       `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com/seals","thumb":"https://example.com/seals.jpg"}}`,
			wantSkipped: 1,
		},
		{
			name:        "video with a thumbnail sends nothing",
			embed:       `{"$type":"social.coves.embed.video","video":` + blobJSON(imageCIDA) + `,"thumbnail":` + blobJSON(imageCIDB) + `}`,
			wantSkipped: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			refs, skipped := classification.PostImages(owner, test.embed)
			if test.wantRefs == nil {
				assert.Empty(t, refs)
			} else {
				assert.Equal(t, test.wantRefs, refs)
			}
			assert.Equal(t, test.wantSkipped, skipped)
		})
	}
}

// Comment images are its image embed's gallery, owned by the commenter (R4).
func TestCommentImages(t *testing.T) {
	refs, skipped := classification.CommentImages("did:plc:commenter", galleryJSON(imageCIDA, imageCIDB))

	assert.Equal(t, []classification.ImageRef{
		{OwnerDID: "did:plc:commenter", CID: imageCIDA},
		{OwnerDID: "did:plc:commenter", CID: imageCIDB},
	}, refs)
	assert.Equal(t, 0, skipped)
}

// A community's images are its avatar then its banner, owned by the community
// (D16).
func TestCommunityImages(t *testing.T) {
	const community = "did:plc:community"
	for _, test := range []struct {
		name                 string
		avatarCID, bannerCID string
		want                 []classification.ImageRef
	}{
		{
			name: "avatar and banner", avatarCID: imageCIDA, bannerCID: imageCIDB,
			want: []classification.ImageRef{{OwnerDID: community, CID: imageCIDA}, {OwnerDID: community, CID: imageCIDB}},
		},
		{
			name: "no banner", avatarCID: imageCIDA,
			want: []classification.ImageRef{{OwnerDID: community, CID: imageCIDA}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, classification.CommunityImages(community, test.avatarCID, test.bannerCID))
		})
	}
}
