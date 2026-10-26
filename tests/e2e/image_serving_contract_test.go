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

func TestImageServingRequiresIndexedOwnerReference(t *testing.T) {
	p := newPipeline(t)
	author := p.IndexedAccount(t, "isra")
	community := indexedCommunity(t, p, "isra", author.DID)
	image := testkit.TestPNG(64, 64)
	authorBlob := author.UploadBlob(t, image, "image/png")
	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)
	record := postV2Record(community.DID, "indexed image "+testkit.UniqueID(t), "image in accepted post")
	record["embed"] = map[string]any{
		"$type":  "social.coves.embed.images",
		"images": []any{map[string]any{"image": blobRefValue(authorBlob), "alt": "indexed image"}},
	}
	postCID := author.PutRecord(t, postV2Collection, rkey, record).CID
	awaitStatus(t, p, uri, community.DID, "pending", "the image post to reach admission")
	community.PutRecord(t, acceptanceCollection, subjectRkey(uri), acceptanceRecord(uri, postCID))
	awaitStatus(t, p, uri, community.DID, "accepted", "the image post to be accepted")

	var imageURL string
	p.Await(t, "the accepted post to advertise its full-size image", func() (bool, error) {
		var response struct {
			Posts []map[string]any `json:"posts"`
		}
		if err := p.AppView.Query(context.Background(), "social.coves.community.post.get", url.Values{"uris": {uri}}, &response); err != nil {
			return false, err
		}
		if len(response.Posts) != 1 || response.Posts[0]["uri"] != uri {
			return false, nil
		}
		embed, ok := response.Posts[0]["embed"].(map[string]any)
		if !ok {
			return false, nil
		}
		images, ok := embed["images"].([]any)
		if !ok || len(images) != 1 {
			return false, nil
		}
		served, ok := images[0].(map[string]any)
		if !ok {
			return false, nil
		}
		imageURL, ok = served["fullsize"].(string)
		return ok && imageURL != "", nil
	})
	requireServesImage(t, p, "accepted post image", imageURL)

	otherOwner := p.IndexedAccount(t, "isrb")
	otherBlob := otherOwner.UploadBlob(t, image, "image/png")
	require.Equal(t, authorBlob.CID(), otherBlob.CID())
	otherOwner.PutRecord(t, "app.bsky.actor.profile", "self", map[string]any{
		"$type":  "app.bsky.actor.profile",
		"avatar": blobRefValue(otherBlob),
	})
	query := url.Values{"did": {otherOwner.DID}, "cid": {otherBlob.CID()}}
	pdsBlob, err := testkit.NewXRPCClient(p.PDS.URL()).GetBinary(context.Background(), "/xrpc/com.atproto.sync.getBlob?"+query.Encode())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, pdsBlob.Status)
	require.Equal(t, image, pdsBlob.Body)

	path := fmt.Sprintf("/img/content_full/plain/%s/%s", otherOwner.DID, otherBlob.CID())
	_, err = p.AppView.GetBinary(context.Background(), path)
	require.Truef(t, testkit.IsStatus(err, http.StatusNotFound), "unindexed owner's PDS blob must return 404; got %v", err)
}
