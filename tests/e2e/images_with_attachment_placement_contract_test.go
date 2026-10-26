//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestImagesWithAttachmentPlacedUploadContract(t *testing.T) {
	p := newPipeline(t)
	quotedAuthor := p.IndexedAccount(t, "iwq")
	community := indexedCommunity(t, p, "iwc", quotedAuthor.DID)
	quoted := indexedPost(t, p, community, quotedAuthor, "quoted placement target")
	author := p.IndexedAccount(t, "iwa")
	require.NotEqual(t, quotedAuthor.DID, author.DID)
	viewer := p.AppView.As(p.AppView.SignIn(t, author))
	ctx, cancel := context.WithTimeout(t.Context(), contractBudget)
	defer cancel()

	var uploaded struct {
		Blob        testkit.BlobRef `json:"blob"`
		AspectRatio struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"aspectRatio"`
	}
	require.NoError(t, viewer.Upload(ctx, "social.coves.media.uploadImage", "image/png", testkit.TestPNG(40, 24), &uploaded))
	require.NotEmpty(t, uploaded.Blob.CID())
	var created strongRef
	require.NoError(t, viewer.Procedure(ctx, "social.coves.community.post.create", map[string]any{
		"community": community.DID,
		"title":     "placed image with a quote",
		"content":   "Before [image] after",
		"facets": []any{map[string]any{
			"$type":    "social.coves.richtext.facet",
			"index":    map[string]any{"byteStart": 7, "byteEnd": 14},
			"features": []any{map[string]any{"$type": "social.coves.richtext.facet#image", "index": 0}},
		}},
		"embed": map[string]any{
			"$type": "social.coves.embed.imagesWithAttachment",
			"images": map[string]any{
				"$type": "social.coves.embed.images",
				"images": []any{map[string]any{
					"image": blobRefValue(uploaded.Blob), "alt": "placed harbour image",
					"aspectRatio": map[string]any{"width": uploaded.AspectRatio.Width, "height": uploaded.AspectRatio.Height},
				}},
			},
			"attachment": map[string]any{
				"$type": "social.coves.embed.post",
				"post":  map[string]any{"uri": quoted.URI, "cid": quoted.CID},
			},
		},
	}, &created), "a quoted composite with a valid main-image placement must be accepted")
	require.NotEmpty(t, created.URI)
	require.NotEmpty(t, created.CID)
	p.Await(t, "the XRPC-created composite to reach the admission index", func() (bool, error) {
		_, err := p.PostStatus(ctx, created.URI, community.DID)
		return testkit.PendingIfNotFound(err)
	})
	community.PutRecord(t, acceptanceCollection, subjectRkey(created.URI), acceptanceRecord(created.URI, created.CID))
	awaitStatus(t, p, created.URI, community.DID, "accepted", "the community to accept the placed composite")

	var record, embed map[string]any
	p.Await(t, "the exact created composite CID to appear in its anonymous public view", func() (bool, error) {
		var response struct {
			Posts []struct {
				URI      string         `json:"uri"`
				CID      string         `json:"cid"`
				NotFound bool           `json:"notFound"`
				Record   map[string]any `json:"record"`
				Embed    map[string]any `json:"embed"`
			} `json:"posts"`
		}
		if err := p.AppView.Query(ctx, "social.coves.community.post.get", url.Values{"uris": {created.URI}}, &response); err != nil {
			return false, err
		}
		if len(response.Posts) != 1 {
			return false, fmt.Errorf("post.get returned %d members for one requested URI", len(response.Posts))
		}
		post := response.Posts[0]
		if post.NotFound || post.URI != created.URI || post.CID != created.CID {
			return false, nil
		}
		record, embed = post.Record, post.Embed
		return true, nil
	}, withReadCadence())
	require.Equal(t, "Before [image] after", record["content"])
	require.Equal(t, []any{map[string]any{
		"$type":    "social.coves.richtext.facet",
		"index":    map[string]any{"byteStart": float64(7), "byteEnd": float64(14)},
		"features": []any{map[string]any{"$type": "social.coves.richtext.facet#image", "index": float64(0)}},
	}}, record["facets"], "the indexed record must retain the literal placement")
	require.Equal(t, "social.coves.embed.imagesWithAttachment#view", embed["$type"])
	require.Equal(t, map[string]any{
		"$type": "social.coves.embed.post#view",
		"post":  map[string]any{"uri": quoted.URI, "cid": quoted.CID},
	}, embed["attachment"], "the quote attachment must contain only its strongRef")
	main, ok := embed["images"].(map[string]any)
	require.True(t, ok, "the view must project the nested main gallery")
	require.Equal(t, "social.coves.embed.images#view", main["$type"])
	images, ok := main["images"].([]any)
	require.True(t, ok)
	require.Len(t, images, 1)
	image, ok := images[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "placed harbour image", image["alt"])
	imageURL, ok := image["fullsize"].(string)
	require.True(t, ok)
	require.Contains(t, imageURL, uploaded.Blob.CID())
	require.Contains(t, imageURL, author.DID)
	requireServesImage(t, p, "placed composite main image", imageURL)
}
