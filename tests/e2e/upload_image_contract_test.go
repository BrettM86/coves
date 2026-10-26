//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"image/jpeg"
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestUploadImageStripsExifBeforeStoringOnTheCallersPDS is the outside-in
// contract for social.coves.media.uploadImage: a real account, signed in
// through the real OAuth web flow, uploads a JPEG carrying GPS EXIF, and the
// blob that lands in that account's repo on the local PDS is a JPEG of the same
// dimensions with no EXIF APP1 segment. The blob is read back from the PDS
// itself, without credentials, so what is asserted is what the PDS stores, not
// the AppView's account of it.
func TestUploadImageStripsExifBeforeStoringOnTheCallersPDS(t *testing.T) {
	p := newPipeline(t)

	// Given an indexed account signed in through OAuth.
	account := p.IndexedAccount(t, "upld")
	viewer := p.AppView.As(p.AppView.SignIn(t, account))

	// And a 48×32 JPEG whose EXIF carries a GPS position (orientation 1, so
	// the stored dimensions are the encoded ones).
	upload := testkit.JPEGWithSegments(testkit.TestJPEG(48, 32), testkit.JPEGExifGPSegment(1))
	require.True(t, bytes.Contains(upload, []byte("Exif\x00\x00")),
		"the upload fixture must carry an EXIF APP1 segment, or the stripping check below proves nothing")

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// When the account uploads it through the AppView.
	var out struct {
		Blob        testkit.BlobRef `json:"blob"`
		MimeType    string          `json:"mimeType"`
		Size        int             `json:"size"`
		AspectRatio struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"aspectRatio"`
	}
	require.NoError(t, viewer.Upload(ctx, "social.coves.media.uploadImage", "image/jpeg", upload, &out),
		"social.coves.media.uploadImage must accept a JPEG from %s's OAuth session", account.Handle)

	// Then the response describes a 48×32 JPEG blob.
	require.Equal(t, "image/jpeg", out.MimeType)
	require.Equal(t, 48, out.AspectRatio.Width)
	require.Equal(t, 32, out.AspectRatio.Height)
	require.NotEmpty(t, out.Blob.Ref.Link, "uploadImage's blob must carry a CID in ref.$link")

	// And once the account's repo references the returned blob (the PDS does
	// not serve a freshly uploaded, unreferenced blob from getBlob), the
	// account's own PDS serves that CID as a 48×32 JPEG with no EXIF.
	account.PutRecord(t, "app.bsky.actor.profile", "self", map[string]any{
		"$type":  "app.bsky.actor.profile",
		"avatar": blobRefValue(out.Blob),
	})
	query := url.Values{"did": {account.DID}, "cid": {out.Blob.Ref.Link}}
	stored, err := testkit.NewXRPCClient(p.PDS.URL()).GetBinary(ctx, "/xrpc/com.atproto.sync.getBlob?"+query.Encode())
	require.NoError(t, err, "the local PDS must hold blob %s in %s's repo", out.Blob.Ref.Link, account.Handle)
	require.Equal(t, http.StatusOK, stored.Status)
	require.Equal(t, len(stored.Body), out.Size, "uploadImage's size must be the length of the bytes the PDS stored")

	decoded, err := jpeg.DecodeConfig(bytes.NewReader(stored.Body))
	require.NoError(t, err, "the stored blob must decode as a JPEG")
	require.Equal(t, 48, decoded.Width)
	require.Equal(t, 32, decoded.Height)
	require.False(t, bytes.Contains(stored.Body, []byte("Exif\x00\x00")),
		"the stored blob still carries an EXIF APP1 segment: the GPS position reached the PDS")
}
