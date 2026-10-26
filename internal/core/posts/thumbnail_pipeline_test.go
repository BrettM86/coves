package posts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	_ "image/jpeg" // registers the JPEG decoder for image.DecodeConfig
	"image/png"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/core/blobs"
	"Coves/internal/core/media"
	"Coves/internal/core/unfurl"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A link card's thumbnail is fetched from an origin the poster chose, so its
// bytes, not the origin's Content-Type, decide what is uploaded: they go
// through the media pipeline (sniff, budget, sanitize) and are uploaded with
// the sniffed MIME, or not at all.

// thumbnailUpload is one UploadBlob call the author's repo received.
type thumbnailUpload struct {
	data     []byte
	mimeType string
}

// thumbnailRecordingAuthorRepo records what reached the author's storage.
type thumbnailRecordingAuthorRepo struct {
	*memAuthorRepo
	uploads []thumbnailUpload
	ref     *blobs.BlobRef
}

func (r *thumbnailRecordingAuthorRepo) UploadBlob(_ context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	r.uploads = append(r.uploads, thumbnailUpload{data: append([]byte(nil), data...), mimeType: mimeType})
	r.ref = &blobs.BlobRef{
		Type: "blob", Ref: map[string]string{"$link": "bafkreithumbnailpipelinetest"},
		MimeType: mimeType, Size: len(data),
	}
	return r.ref, nil
}

// thumbnailUnfurl answers every URL with a thumbnail at the test origin.
type thumbnailUnfurl struct{ thumbnailURL string }

func (u *thumbnailUnfurl) IsSupported(string) bool { return true }

func (u *thumbnailUnfurl) UnfurlURL(context.Context, string) (*unfurl.UnfurlResult, error) {
	return &unfurl.UnfurlResult{Type: "article", Title: "A linked article", ThumbnailURL: u.thumbnailURL}, nil
}

// servedThumbnail is what the origin answers; an empty contentType sends no
// Content-Type header at all.
type servedThumbnail struct {
	body        []byte
	contentType string
}

func newThumbnailPipeline(t *testing.T) *media.Pipeline {
	t.Helper()
	pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: media.DefaultMaxLongEdge})
	require.NoError(t, err)
	return pipeline
}

// enhanceWithServedThumbnail runs a user's external embed through
// enhanceExternalEmbed against an origin serving the thumbnail, and returns the
// author repo and the embed's external map. A nil pipeline builds the service
// without WithMediaPipeline.
func enhanceWithServedThumbnail(t *testing.T, served servedThumbnail, pipeline *media.Pipeline) (*thumbnailRecordingAuthorRepo, map[string]interface{}) {
	t.Helper()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served.contentType == "" {
			// Without this the server would sniff and send a Content-Type.
			w.Header()["Content-Type"] = nil
		} else {
			w.Header().Set("Content-Type", served.contentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(served.body)
	}))
	t.Cleanup(origin.Close)

	options := []PostServiceOption{WithAdmissionPolicy(NewAllowAllAdmissionPolicyForTests())}
	if pipeline != nil {
		options = append(options, WithMediaPipeline(pipeline))
	}
	service := NewPostService(
		nil, nil, nil,
		// The origin is an httptest server on loopback, which the SSRF guard refuses.
		blobs.NewBlobService("https://pds.invalid", blobs.WithPrivateHostsAllowed()),
		&thumbnailUnfurl{thumbnailURL: origin.URL + "/thumbnail"},
		nil, "https://pds.invalid", options...,
	).(*postService)

	external := map[string]interface{}{"uri": "https://article.invalid/story"}
	record := &PostRecord{Embed: map[string]interface{}{
		"$type":    "social.coves.embed.external",
		"external": external,
	}}
	authorRepo := &thumbnailRecordingAuthorRepo{memAuthorRepo: newMemAuthorRepo("did:plc:thumbnailauthor")}

	service.enhanceExternalEmbed(context.Background(), record, CreatePostRequest{}, authorRepo, false)
	return authorRepo, external
}

// requireOneThumbnailUpload asserts exactly one upload with mimeType, set as the
// embed's thumb, and returns its bytes.
func requireOneThumbnailUpload(t *testing.T, authorRepo *thumbnailRecordingAuthorRepo, external map[string]interface{}, mimeType string) []byte {
	t.Helper()
	require.Equal(t, 1, len(authorRepo.uploads), "thumbnail uploads")
	assert.Equal(t, mimeType, authorRepo.uploads[0].mimeType)
	require.Contains(t, external, "thumb")
	assert.Same(t, authorRepo.ref, external["thumb"])
	return authorRepo.uploads[0].data
}

func assertNoThumbnail(t *testing.T, authorRepo *thumbnailRecordingAuthorRepo, external map[string]interface{}) {
	t.Helper()
	uploadedMIMETypes := make([]string, 0, len(authorRepo.uploads))
	for _, upload := range authorRepo.uploads {
		uploadedMIMETypes = append(uploadedMIMETypes, upload.mimeType)
	}
	assert.Empty(t, uploadedMIMETypes, "MIME types of thumbnail uploads")
	_, hasThumb := external["thumb"]
	assert.False(t, hasThumb, "the external embed has a thumb")
}

func TestEnhanceExternalEmbedUploadsThumbnailWithSniffedMIME(t *testing.T) {
	webp, err := base64.StdEncoding.DecodeString(testkit.TransparentQuadrantWebP300x200Base64)
	require.NoError(t, err)

	rows := []struct {
		name     string
		served   servedThumbnail
		mimeType string
	}{
		{"JPEG served as image/png", servedThumbnail{testkit.TestJPEG(48, 32), "image/png"}, "image/jpeg"},
		{"PNG served as application/octet-stream", servedThumbnail{testkit.TestPNG(48, 32), "application/octet-stream"}, "image/png"},
		{"WebP served with no Content-Type", servedThumbnail{webp, ""}, "image/webp"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			authorRepo, external := enhanceWithServedThumbnail(t, row.served, newThumbnailPipeline(t))
			requireOneThumbnailUpload(t, authorRepo, external, row.mimeType)
		})
	}
}

func TestEnhanceExternalEmbedStripsThumbnailEXIF(t *testing.T) {
	source := testkit.JPEGWithSegments(testkit.TestJPEG(48, 32), testkit.JPEGExifGPSegment(1))
	require.True(t, bytes.Contains(source, []byte("Exif\x00\x00")), "fixture carries EXIF")

	authorRepo, external := enhanceWithServedThumbnail(t, servedThumbnail{source, "image/jpeg"}, newThumbnailPipeline(t))
	uploaded := requireOneThumbnailUpload(t, authorRepo, external, "image/jpeg")

	_, format, err := image.DecodeConfig(bytes.NewReader(uploaded))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	segments, _, err := testkit.WalkJPEGSegments(uploaded)
	require.NoError(t, err)
	for _, segment := range segments {
		assert.NotEqual(t, byte(0xe1), segment.Marker, "uploaded thumbnail keeps an APP1 segment")
	}
	assert.False(t, bytes.Contains(uploaded, []byte("Exif\x00\x00")), "uploaded thumbnail keeps EXIF")
}

func TestEnhanceExternalEmbedRefusesThumbnailThePipelineRejects(t *testing.T) {
	rows := []struct {
		name         string
		served       servedThumbnail
		withPipeline bool
	}{
		{"SVG served as image/svg+xml", servedThumbnail{[]byte(`<svg viewBox="0 0 1 1"><rect width="1" height="1"/></svg>`), "image/svg+xml"}, true},
		{"HTML served as image/png", servedThumbnail{[]byte("<!doctype html><html><body>not an image</body></html>"), "image/png"}, true},
		{"corrupt JPEG served as image/jpeg", servedThumbnail{truncatedThumbnailJPEG(t), "image/jpeg"}, true},
		{"PNG header over 50 megapixels served as image/png", servedThumbnail{thumbnailPNGHeader(10000, 10000), "image/png"}, true},
		{"GIF served as image/gif", servedThumbnail{testkit.StaticGIF87a(), "image/gif"}, true},
		{"valid JPEG with no pipeline wired", servedThumbnail{testkit.TestJPEG(48, 32), "image/jpeg"}, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var pipeline *media.Pipeline
			if row.withPipeline {
				pipeline = newThumbnailPipeline(t)
			}
			authorRepo, external := enhanceWithServedThumbnail(t, row.served, pipeline)
			assertNoThumbnail(t, authorRepo, external)
		})
	}
}

func TestEnhanceExternalEmbedRefusesSanitizedThumbnailOverLexiconMaxSize(t *testing.T) {
	t.Run("incompressible PNG between the thumb maxSize and the fetch cap", func(t *testing.T) {
		source := incompressibleThumbnailPNG(t)
		require.Greater(t, len(source), 6000000, "fixture size")
		require.Less(t, len(source), 6291456, "fixture size")

		authorRepo, external := enhanceWithServedThumbnail(t, servedThumbnail{source, "image/png"}, newThumbnailPipeline(t))
		assertNoThumbnail(t, authorRepo, external)
	})

	t.Run("JPEG under the thumb maxSize whose oriented re-encode exceeds it", func(t *testing.T) {
		source := testkit.JPEGThatGrowsPastSixMillionBytesWhenRotated()
		require.Less(t, len(source), 6000000, "fixture size")

		authorRepo, external := enhanceWithServedThumbnail(t, servedThumbnail{source, "image/jpeg"}, newThumbnailPipeline(t))
		assertNoThumbnail(t, authorRepo, external)
	})
}

func TestEnhanceExternalEmbedKeepsThumbnailDimensions(t *testing.T) {
	authorRepo, external := enhanceWithServedThumbnail(t, servedThumbnail{testkit.TestPNG(5000, 8), "image/png"}, newThumbnailPipeline(t))
	uploaded := requireOneThumbnailUpload(t, authorRepo, external, "image/png")

	config, format, err := image.DecodeConfig(bytes.NewReader(uploaded))
	require.NoError(t, err)
	assert.Equal(t, "png", format)
	assert.Equal(t, 5000, config.Width)
	assert.Equal(t, 8, config.Height)
}

// truncatedThumbnailJPEG is a JPEG cut off halfway through its scan.
func truncatedThumbnailJPEG(t *testing.T) []byte {
	t.Helper()
	data := testkit.TestJPEG(48, 32)
	_, scan, err := testkit.WalkJPEGSegments(data)
	require.NoError(t, err)
	return data[:len(data)-len(scan)/2]
}

// thumbnailPNGHeader is a PNG signature and IHDR claiming width x height,
// followed by the start of an IDAT and nothing else.
func thumbnailPNGHeader(width, height uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	file := []byte("\x89PNG\r\n\x1a\n")
	file = append(file, thumbnailPNGChunk("IHDR", ihdr)...)
	return append(file, thumbnailPNGChunk("IDAT", []byte{0x78, 0x9c})...)
}

func thumbnailPNGChunk(kind string, payload []byte) []byte {
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	chunk = append(chunk, kind...)
	chunk = append(chunk, payload...)
	return binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
}

// incompressibleThumbnailPNG is 1240x1240 seeded RGBA noise, about 6.15 MB
// encoded. The seed is fixed so the bytes are the same on every run.
func incompressibleThumbnailPNG(t *testing.T) []byte {
	t.Helper()
	random := rand.New(rand.NewPCG(24, 6))
	noise := image.NewNRGBA(image.Rect(0, 0, 1240, 1240))
	for index := range noise.Pix {
		noise.Pix[index] = uint8(random.UintN(256))
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, noise))
	return encoded.Bytes()
}
