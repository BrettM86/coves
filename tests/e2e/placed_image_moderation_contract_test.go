//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestPlacedImageUploadAndModerationContract(t *testing.T) {
	p := newPipeline(t)
	creator := p.IndexedAccount(t, "pih")
	community := indexedCommunity(t, p, "pim", creator.DID)
	root := indexedPost(t, p, community, creator, "placed image comment root")
	admin := testkit.ModerationAdmin(t, 1)
	var previousImageCID string
	for _, subject := range []struct {
		name   string
		prefix string
		width  int
	}{
		{name: "post", prefix: "pip", width: 32},
		{name: "comment", prefix: "pic", width: 48},
	} {
		t.Run(subject.name, func(t *testing.T) {
			author := p.IndexedAccount(t, subject.prefix)
			require.NotEqual(t, creator.DID, author.DID)
			require.NotEqual(t, admin.DID, author.DID)
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
			require.NoError(t, viewer.Upload(ctx, "social.coves.media.uploadImage", "image/png",
				testkit.TestPNG(subject.width, 24), &uploaded))
			require.NotEmpty(t, uploaded.Blob.CID())
			require.NotEqual(t, previousImageCID, uploaded.Blob.CID(), "each subject must use a distinct image CID")
			previousImageCID = uploaded.Blob.CID()
			input := map[string]any{
				"content": "Before [image] after",
				"facets": []any{map[string]any{
					"$type":    "social.coves.richtext.facet",
					"index":    map[string]any{"byteStart": 7, "byteEnd": 14},
					"features": []any{map[string]any{"$type": "social.coves.richtext.facet#image", "index": 0}},
				}},
				"embed": map[string]any{
					"$type": "social.coves.embed.images",
					"images": []any{map[string]any{
						"image": blobRefValue(uploaded.Blob), "alt": "placed image",
						"aspectRatio": map[string]any{"width": uploaded.AspectRatio.Width, "height": uploaded.AspectRatio.Height},
					}},
				},
			}
			method := "social.coves.community.post.create"
			if subject.name == "post" {
				input["community"] = community.DID
				input["title"] = "placed image upload"
			} else {
				method = "social.coves.community.comment.create"
				input["reply"] = map[string]any{
					"root":   map[string]any{"uri": root.URI, "cid": root.CID},
					"parent": map[string]any{"uri": root.URI, "cid": root.CID},
				}
			}
			var created strongRef
			require.NoError(t, viewer.Procedure(ctx, method, input, &created))
			require.NotEmpty(t, created.URI)
			require.NotEmpty(t, created.CID)
			if subject.name == "post" {
				p.Await(t, "the XRPC-created post to reach the admission index", func() (bool, error) {
					_, err := p.PostStatus(ctx, created.URI, community.DID)
					return testkit.PendingIfNotFound(err)
				})
				community.PutRecord(t, acceptanceCollection, subjectRkey(created.URI), acceptanceRecord(created.URI, created.CID))
				awaitStatus(t, p, created.URI, community.DID, "accepted", "the community to accept the placed-image post")
			}

			var record, embed map[string]any
			p.Await(t, "the created placed-image record to appear in its public view", func() (bool, error) {
				if subject.name == "comment" {
					thread, err := p.Thread(ctx, root.URI, nil)
					if err != nil {
						return false, err
					}
					node, found := thread.find(created.URI)
					if !found || node.Comment.CID != created.CID {
						return false, nil
					}
					record, embed = node.Comment.Record, node.Comment.Embed
					return true, nil
				}
				var response struct {
					Posts []struct {
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
				if post.NotFound || post.CID != created.CID {
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
			}}, record["facets"], "the indexed record must retain the literal image placement")
			require.Equal(t, "social.coves.embed.images#view", embed["$type"])
			images, ok := embed["images"].([]any)
			require.True(t, ok)
			require.Len(t, images, 1, "the public view must emit the uploaded image")
			image, ok := images[0].(map[string]any)
			require.True(t, ok)
			imageURL, ok := image["fullsize"].(string)
			require.True(t, ok)
			require.Contains(t, imageURL, uploaded.Blob.CID())
			requireServesImage(t, p, subject.name+" placed image before removal", imageURL)

			var inspected moderationSubjectStateResponse
			require.NoError(t, p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)).Query(
				ctx, subjectStateMethod, url.Values{"subject": {created.URI}}, &inspected))
			require.Equal(t, created.CID, inspected.State.CurrentSubject.CID)
			var removal struct {
				Outcome string `json:"outcome"`
			}
			require.NoError(t, p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, removeContentMethod)).Procedure(
				ctx, removeContentMethod, map[string]any{
					"subject":         map[string]any{"uri": created.URI, "cid": inspected.State.CurrentSubject.CID},
					"expectedVersion": inspected.State.Version,
					"idempotencyKey":  testkit.UniqueID(t),
					"reason":          "social.coves.moderation.defs#reasonSpam",
				}, &removal))
			require.Equal(t, "applied", removal.Outcome)
			parsed, err := url.Parse(imageURL)
			require.NoError(t, err)
			_, err = p.AppView.GetBinary(ctx, parsed.RequestURI())
			var blocked *testkit.StatusError
			require.ErrorAs(t, err, &blocked, "the same image URL must be blocked after admin removal")
			require.Equal(t, http.StatusNotFound, blocked.StatusCode)
			require.Equal(t, "no-store", blocked.Header.Get("Cache-Control"), "the removal's 404 must never be cached")
		})
	}
}
