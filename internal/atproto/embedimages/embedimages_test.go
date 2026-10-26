package embedimages_test

import (
	"errors"
	"math"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"Coves/internal/atproto/embedimages"
)

const validCID = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"

// cidOf builds a CID over data with the given multihash code and digest length
// (-1 for the hash's default length).
func cidOf(t *testing.T, version, code uint64, length int) string {
	t.Helper()
	digest, err := multihash.Sum([]byte("embedimages test blob"), code, length)
	if err != nil {
		t.Fatalf("multihash.Sum(%d, %d): %v", code, length, err)
	}
	if version == 0 {
		return cid.NewCidV0(digest).String()
	}
	return cid.NewCidV1(cid.Raw, digest).String()
}

func blob(link, mimeType, size interface{}) map[string]interface{} {
	value := map[string]interface{}{
		"$type": "blob",
		"ref":   map[string]interface{}{"$link": link},
	}
	if mimeType != nil {
		value["mimeType"] = mimeType
	}
	if size != nil {
		value["size"] = size
	}
	return value
}

func embedOf(images ...map[string]interface{}) map[string]interface{} {
	entries := make([]interface{}, 0, len(images))
	for _, image := range images {
		entries = append(entries, map[string]interface{}{"image": image, "alt": "an image"})
	}
	return map[string]interface{}{"$type": "social.coves.embed.images", "images": entries}
}

func repeat(count int, image map[string]interface{}) []map[string]interface{} {
	images := make([]map[string]interface{}, count)
	for i := range images {
		images[i] = image
	}
	return images
}

func TestCheck(t *testing.T) {
	valid := blob(validCID, "image/png", float64(1000))
	malformed := blob("notacid12345", "image/png", float64(1000))

	tests := []struct {
		name    string
		embed   map[string]interface{}
		wantErr error
	}{
		{name: "valid sha2-256 CIDv1 png", embed: embedOf(valid)},
		{name: "size 1e3 float", embed: embedOf(blob(validCID, "image/jpeg", 1e3))},
		{name: "size at the limit", embed: embedOf(blob(validCID, "image/webp", float64(embedimages.MaxImageSize)))},
		{name: "size zero", embed: embedOf(blob(validCID, "image/gif", float64(0)))},
		{name: "size int from a Go caller", embed: embedOf(blob(validCID, "image/png", 1000))},
		{name: "size over the limit", embed: embedOf(blob(validCID, "image/png", float64(embedimages.MaxImageSize+1))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "size int over the limit", embed: embedOf(blob(validCID, "image/png", embedimages.MaxImageSize+1)), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "size negative", embed: embedOf(blob(validCID, "image/png", float64(-1))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "size int negative", embed: embedOf(blob(validCID, "image/png", -1)), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "size 2^63-1 decodes to float 2^63", embed: embedOf(blob(validCID, "image/png", float64(math.MaxInt64))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "size fractional", embed: embedOf(blob(validCID, "image/png", 1.5)), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "size string", embed: embedOf(blob(validCID, "image/png", "1000")), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "size missing", embed: embedOf(blob(validCID, "image/png", nil)), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "mimeType avif", embed: embedOf(blob(validCID, "image/avif", float64(1000))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "mimeType missing", embed: embedOf(blob(validCID, nil, float64(1000))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "link not a CID", embed: embedOf(blob("notacid12345", "image/png", float64(1000))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "link identity-hash CID", embed: embedOf(blob(cidOf(t, 1, multihash.IDENTITY, -1), "image/png", float64(1000))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "link sha2-512 CID", embed: embedOf(blob(cidOf(t, 1, multihash.SHA2_512, -1), "image/png", float64(1000))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "link truncated sha2-256 CID", embed: embedOf(blob(cidOf(t, 1, multihash.SHA2_256, 20), "image/png", float64(1000))), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "link sha2-256 CIDv0 accepted", embed: embedOf(blob(cidOf(t, 0, multihash.SHA2_256, -1), "image/png", float64(1000)))},
		{name: "eight valid images", embed: embedOf(repeat(embedimages.MaxImages, valid)...)},
		{name: "nine images with a malformed last one", embed: embedOf(append(repeat(embedimages.MaxImages, valid), malformed)...), wantErr: embedimages.ErrTooManyImages},
		{name: "malformed second image", embed: embedOf(valid, malformed), wantErr: embedimages.ErrInvalidImageBlob},
		{name: "other embed type is ignored", embed: map[string]interface{}{"$type": "social.coves.embed.external", "images": []interface{}{"x"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := embedimages.Check(test.embed)
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("Check() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Check() = %v, want an error matching %v", err, test.wantErr)
			}
		})
	}
}
