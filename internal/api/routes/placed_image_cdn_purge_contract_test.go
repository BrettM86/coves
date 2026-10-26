//go:build integration

package routes_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestPlacedImageModerationDurableCDNPurge(t *testing.T) {
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	purger, err := imageproxy.NewCloudflarePurger(imageproxy.CloudflarePurgerConfig{
		APIBase: endpoint.URL(), ZoneID: "placed-image-zone", APIToken: "placed-image-test-token",
		BaseURLs: []string{"https://img.example.test", "https://second-img.example.test"}, Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	h, _ := newModerationMediaHarness(t, false, func(options *moderationMediaHarnessConfig) {
		options.targets = func(db *sql.DB, now func() time.Time) moderation.CDNPurgeTargets {
			return moderation.NewCDNPurgeQueue(postgres.NewModerationRepository(db), purger,
				moderation.CDNPurgeQueueConfig{Now: now, WriteTimeout: 90 * time.Second, BatchSize: 100})
		}
	})
	postImage, commentImage := mediaImageCID("placed purge post image"), mediaImageCID("placed purge comment image")
	facets := `[{"index":{"byteStart":0,"byteEnd":7},"features":[{"$type":"social.coves.richtext.facet#image","index":0}]}]`
	postEmbed, err := json.Marshal(map[string]any{
		"$type": "social.coves.embed.images", "images": []any{map[string]any{
			"alt": "placed image", "image": map[string]any{"$type": "blob", "ref": map[string]any{"$link": postImage}, "mimeType": "image/png", "size": 10},
		}},
	})
	require.NoError(t, err)
	// The shared harness starts with a legacy post fixture. Convert this one
	// fixture to an author-owned post before any comment references its URI.
	postRkey := testkit.TID()
	postURI := "at://" + h.ownerA + "/social.coves.community.postv2/" + postRkey
	postCID := mediaImageCID("placed purge post record")
	_, err = h.db.ExecContext(t.Context(), `UPDATE posts SET uri = $1, cid = $2, rkey = $3, content = '[image]', content_facets = $4::jsonb, embed = $5::jsonb WHERE uri = $6`,
		postURI, postCID, postRkey, facets, string(postEmbed), h.postURI)
	require.NoError(t, err)
	h.postURI, h.postCID = postURI, postCID
	post := moderation.StrongRef{URI: postURI, CID: postCID}
	comment := h.comment(t, h.ownerB, commentImage)
	_, err = h.db.ExecContext(t.Context(), `UPDATE comments SET content = '[image]', content_facets = $1::jsonb WHERE uri = $2`, facets, comment.URI)
	require.NoError(t, err)

	h.remove(t, post, cdnPurgeSpam)
	h.remove(t, comment, cdnPurgeSpam)
	waitModerationCDN(t, h.queue)

	type targetPair struct{ owner, cid string }
	rows, err := h.db.QueryContext(t.Context(), `SELECT owner_did, blob_cid FROM moderation_media_purges`)
	require.NoError(t, err)
	defer rows.Close()
	var targets []targetPair
	for rows.Next() {
		var target targetPair
		require.NoError(t, rows.Scan(&target.owner, &target.cid))
		targets = append(targets, target)
	}
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, []targetPair{{h.ownerA, postImage}, {h.ownerB, commentImage}}, targets,
		"both placed images have durable purge targets for their actual owners")

	var recordedFiles []string
	for _, request := range endpoint.Requests() {
		require.NoError(t, request.DecodeError)
		recordedFiles = append(recordedFiles, request.Files...)
	}
	var wantedFiles []string
	for _, pair := range []targetPair{{h.ownerA, postImage}, {h.ownerB, commentImage}} {
		for _, base := range []string{"https://img.example.test", "https://second-img.example.test"} {
			for _, preset := range []string{"avatar", "avatar_small", "banner", "content_preview", "content_full", "embed_thumbnail"} {
				wantedFiles = append(wantedFiles, base+"/img/"+preset+"/plain/"+pair.owner+"/"+pair.cid)
			}
		}
	}
	require.ElementsMatch(t, wantedFiles, recordedFiles, "purge covers both configured bases and all six presets for each placed image")
}
