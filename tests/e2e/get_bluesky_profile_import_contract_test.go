//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type blueskyImportOfferField struct {
	Field           string `json:"field"`
	Bluesky         string `json:"bluesky"`
	BlueskyImageURL string `json:"blueskyImageUrl"`
	CovesIsEmpty    bool   `json:"covesIsEmpty"`
}

type blueskyImportOffer struct {
	Status string                    `json:"status"`
	Fields []blueskyImportOfferField `json:"fields"`
}

// requirePreviewServesBlob fetches a preview URL and requires it to serve
// exactly the uploaded blob bytes. It refuses to contact the Bluesky CDN: the
// stack must be configured with the PDS preview form.
func requirePreviewServesBlob(ctx context.Context, t *testing.T, field, previewURL string, want []byte) {
	t.Helper()
	parsed, err := url.Parse(previewURL)
	require.NoError(t, err, "the %s preview URL %q must parse", field, previewURL)
	require.NotEqual(t, "cdn.bsky.app", parsed.Hostname(),
		"the %s preview URL %q points at the Bluesky CDN; the e2e stack must set BLUESKY_PROFILE_IMAGE_PREVIEW_SOURCE=pds",
		field, previewURL)
	require.NotEmpty(t, parsed.Host, "the %s preview URL %q must be absolute", field, previewURL)

	path := parsed.EscapedPath()
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	served, err := testkit.NewXRPCClient(parsed.Scheme+"://"+parsed.Host).GetBinary(ctx, path)
	require.NoError(t, err, "the %s preview URL %q must be served", field, previewURL)
	require.Equal(t, http.StatusOK, served.Status)
	require.Equal(t, len(want), len(served.Body), "the %s preview must serve the uploaded blob's size", field)
	require.True(t, bytes.Equal(want, served.Body), "the %s preview must serve the uploaded blob's bytes", field)
}

func offerField(t *testing.T, offer blueskyImportOffer, name string) blueskyImportOfferField {
	t.Helper()
	for _, entry := range offer.Fields {
		if entry.Field == name {
			return entry
		}
	}
	require.Failf(t, "field missing from the offer", "no %q entry in %#v", name, offer.Fields)
	return blueskyImportOfferField{}
}

// TestGetBlueskyProfileImportContract reads both profile records off a real
// PDS through the user's session:
//
//	offer     → an account with a Bluesky profile and a Coves profile holding
//	            only a display name is offered the import, avatar empty on
//	            Coves with a preview URL naming the Bluesky avatar CID; the
//	            avatar and banner preview URLs serve the uploaded bytes (the
//	            stack runs BLUESKY_PROFILE_IMAGE_PREVIEW_SOURCE=pds, so they are
//	            the local PDS's getBlob URLs)
//	no record → a fresh account with no Bluesky profile gets noBlueskyProfile
//	dismissed → after importBlueskyProfile the first account gets dismissed
func TestGetBlueskyProfileImportContract(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "bskyoffer")
	client := p.AppView.As(p.AppView.SignIn(t, account))

	avatarBytes := testkit.TestPNG(64, 64)
	bannerBytes := testkit.TestJPEG(96, 32)
	avatar := account.UploadBlob(t, avatarBytes, "image/png")
	banner := account.UploadBlob(t, bannerBytes, "image/jpeg")

	account.PutRecord(t, blueskyProfileCollection, "self", map[string]any{
		"$type":       blueskyProfileCollection,
		"displayName": "Bluesky " + testkit.UniqueID(t),
		"description": "bio written on Bluesky",
		"avatar":      blobRefValue(avatar),
		"banner":      blobRefValue(banner),
	})
	account.PutRecord(t, profileCollection, "self", map[string]any{
		"$type":       profileCollection,
		"displayName": "Coves " + testkit.UniqueID(t),
	})

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// ---- offer ----------------------------------------------------------------
	var offer blueskyImportOffer
	require.NoError(t, client.Query(ctx, "social.coves.actor.getBlueskyProfileImport", nil, &offer))
	require.Equal(t, "offer", offer.Status, "fields: %#v", offer.Fields)

	avatarEntry := offerField(t, offer, "avatar")
	require.True(t, avatarEntry.CovesIsEmpty, "the Coves profile has no avatar")
	require.NotEmpty(t, avatarEntry.BlueskyImageURL, "an image field must carry a preview URL")
	require.True(t, strings.Contains(avatarEntry.BlueskyImageURL, avatar.CID()),
		"the avatar preview URL %q must name the Bluesky avatar CID %s", avatarEntry.BlueskyImageURL, avatar.CID())
	requirePreviewServesBlob(ctx, t, "avatar", avatarEntry.BlueskyImageURL, avatarBytes)

	bannerEntry := offerField(t, offer, "banner")
	require.True(t, bannerEntry.CovesIsEmpty, "the Coves profile has no banner")
	requirePreviewServesBlob(ctx, t, "banner", bannerEntry.BlueskyImageURL, bannerBytes)

	displayNameEntry := offerField(t, offer, "displayName")
	require.False(t, displayNameEntry.CovesIsEmpty, "the Coves profile already has a display name")

	// ---- no record ------------------------------------------------------------
	other := p.IndexedAccount(t, "bskynone")
	otherClient := p.AppView.As(p.AppView.SignIn(t, other))
	var none blueskyImportOffer
	require.NoError(t, otherClient.Query(ctx, "social.coves.actor.getBlueskyProfileImport", nil, &none))
	require.Equal(t, "noBlueskyProfile", none.Status)
	require.Empty(t, none.Fields)

	// ---- dismissed ------------------------------------------------------------
	require.NoError(t, client.Procedure(ctx, "social.coves.actor.importBlueskyProfile",
		map[string]any{"fields": []string{"avatar"}}, nil))

	var afterImport blueskyImportOffer
	require.NoError(t, client.Query(ctx, "social.coves.actor.getBlueskyProfileImport", nil, &afterImport))
	require.Equal(t, "dismissed", afterImport.Status, "an import records the dismissal")
}
