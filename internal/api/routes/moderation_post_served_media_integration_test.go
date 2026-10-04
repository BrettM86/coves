//go:build integration

package routes_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"Coves/internal/core/blobs"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// servedProxyBlobs returns the (owner DID, CID) pair of every image-proxy URL
// under proxyBaseURL anywhere in value.
func servedProxyBlobs(t *testing.T, proxyBaseURL string, value any) []mediaBlobKey {
	t.Helper()
	var found []mediaBlobKey
	switch typed := value.(type) {
	case string:
		if !strings.HasPrefix(typed, proxyBaseURL+"/img/") {
			return nil
		}
		parsed, err := url.Parse(typed)
		require.NoError(t, err)
		// /img/{preset}/plain/{did}/{cid}
		segments := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
		require.Len(t, segments, 5, "unexpected proxy URL shape: %s", typed)
		found = append(found, mediaBlobKey{segments[3], segments[4]})
	case map[string]any:
		for _, field := range typed {
			found = append(found, servedProxyBlobs(t, proxyBaseURL, field)...)
		}
	case []any:
		for _, entry := range typed {
			found = append(found, servedProxyBlobs(t, proxyBaseURL, entry)...)
		}
	}
	return found
}

// PRD §9 Media: a removal blocks the blobs PostBlobCIDs derives, so the served
// view must carry no proxy URL outside them. An author who hand-writes a proxy
// URL for their own blob into the record gets no image in the view; the same
// image referenced as a blob is served and is blocked by an illegal-content
// removal.
func TestModerationPostServedMediaIsBlockedByRemoval(t *testing.T) {
	h, _ := newModerationMediaHarness(t, false)
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: h.proxy.URL})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
	var communityDID string
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT community_did FROM posts WHERE uri = $1`, h.postURI).Scan(&communityDID))
	repository := postgres.NewPostRepository(h.db)

	blobRef := func(imageCID string) map[string]any {
		return map[string]any{"$type": "blob", "ref": map[string]any{"$link": imageCID}, "mimeType": "image/png", "size": 10}
	}
	authorURL := func(preset, imageCID string) string {
		return h.proxy.URL + "/img/" + preset + "/plain/" + h.ownerA + "/" + imageCID
	}
	for _, test := range []struct {
		name       string
		embed      func(imageCID string) map[string]any
		wantServed bool
	}{
		{name: "external thumb as a URL string", embed: func(imageCID string) map[string]any {
			return map[string]any{"$type": "social.coves.embed.external", "external": map[string]any{
				"uri": "https://example.com/article", "thumb": authorURL("embed_thumbnail", imageCID),
			}}
		}},
		{name: "stored images#view", embed: func(imageCID string) map[string]any {
			return map[string]any{"$type": "social.coves.embed.images#view", "images": []any{map[string]any{
				"thumb": authorURL(postMediaPreset, imageCID), "fullsize": authorURL("content_full", imageCID), "alt": "forged",
			}}}
		}},
		{name: "stored external#view", embed: func(imageCID string) map[string]any {
			return map[string]any{"$type": "social.coves.embed.external#view", "external": map[string]any{
				"uri": "https://example.com/article", "thumb": authorURL("embed_thumbnail", imageCID),
			}}
		}},
		{name: "external thumb as a blob", wantServed: true, embed: func(imageCID string) map[string]any {
			return map[string]any{"$type": "social.coves.embed.external", "external": map[string]any{
				"uri": "https://example.com/article", "thumb": blobRef(imageCID),
			}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			imageCID := mediaImageCID("served media " + test.name)
			h.pds.mu.Lock()
			h.pds.known[mediaBlobKey{h.ownerA, imageCID}] = true
			h.pds.mu.Unlock()
			rkey := testkit.TID()
			subject := moderation.StrongRef{
				URI: "at://" + h.ownerA + "/" + moderation.PostV2Collection + "/" + rkey,
				CID: mediaImageCID("served media record " + rkey),
			}
			embed, err := json.Marshal(test.embed(imageCID))
			require.NoError(t, err)
			_, err = h.db.ExecContext(t.Context(), `
				INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, embed, created_at)
				VALUES ($1, $2, $3, $4, $5, 'served media post', 'body', $6::jsonb, NOW())
			`, subject.URI, subject.CID, rkey, h.ownerA, communityDID, string(embed))
			require.NoError(t, err)
			_, err = h.db.ExecContext(t.Context(), `
				INSERT INTO community_post_admissions
					(community_did, post_uri, status, accepted_cid, evaluated_cid, created_at, updated_at)
				VALUES ($1, $2, 'accepted', $3, $3, NOW(), NOW())
			`, communityDID, subject.URI, subject.CID)
			require.NoError(t, err)

			views, err := repository.GetViewsByURIs(t.Context(), []string{subject.URI}, "")
			require.NoError(t, err)
			view := views[subject.URI]
			require.NotNil(t, view, "the post must be served before removal")
			posts.TransformBlobRefsToURLs(view)
			encoded, err := json.Marshal(view.Embed)
			require.NoError(t, err)
			var servedEmbed any
			require.NoError(t, json.Unmarshal(encoded, &servedEmbed))
			served := servedProxyBlobs(t, h.proxy.URL, servedEmbed)
			if test.wantServed {
				require.Equal(t, []mediaBlobKey{{h.ownerA, imageCID}}, served)
			} else {
				require.Empty(t, served, "a proxy URL the author wrote as a string must not be served: %s", encoded)
			}

			h.remove(t, subject, postMediaIllegalReason)
			for _, blob := range served {
				blocked, err := h.proxyService.IsBlobBlocked(t.Context(), blob.did, blob.cid)
				require.NoError(t, err)
				assert.True(t, blocked, "served blob %s/%s must be blocked after removal", blob.did, blob.cid)
				assert.Equal(t, http.StatusNotFound, h.request(t, postMediaPreset, blob.did, blob.cid))
			}
		})
	}
}
