package posts

import (
	"bytes"
	"context"
	"image"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"Coves/internal/core/blobs"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/unfurl"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nestedThumbnailCID = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"

// Keep the JSON-persisting author repo, but return a lexicon-valid upload blob.
// The older task24 recorder's synthetic CID cannot pass create's final validator.
type nestedThumbnailAuthorRepo struct {
	*recordStoringAuthorRepo
	uploads []thumbnailUpload
}

func (r *nestedThumbnailAuthorRepo) UploadBlob(_ context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	r.uploads = append(r.uploads, thumbnailUpload{data: bytes.Clone(data), mimeType: mimeType})
	return &blobs.BlobRef{
		Type: "blob", Ref: map[string]string{"$link": nestedThumbnailCID},
		MimeType: mimeType, Size: len(data),
	}, nil
}

func nestedUnfurlHarness(t *testing.T, result unfurl.UnfurlResult, served *servedThumbnail) (*lexiconHarness, *nestedThumbnailAuthorRepo, *atomic.Int64) {
	t.Helper()
	fetches := &atomic.Int64{}
	if served != nil {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fetches.Add(1)
			w.Header().Set("Content-Type", served.contentType)
			_, _ = w.Write(served.body)
		}))
		t.Cleanup(origin.Close)
		result.ThumbnailURL = origin.URL + "/thumbnail"
	}
	h := newLexiconHarnessWithBlobs(t, &fixedUnfurl{result: result},
		blobs.NewBlobService("https://pds.invalid", blobs.WithPrivateHostsAllowed()))
	author := &nestedThumbnailAuthorRepo{recordStoringAuthorRepo: h.repo}
	WithAuthorRepoFactory(func(context.Context, string, *oauth.ClientSessionData) (AuthorRepo, error) {
		return author, nil
	})(h.service.(*postService))
	return h, author, fetches
}

func nestedUnfurlExternal(embed map[string]any) map[string]any {
	return embed["attachment"].(map[string]any)["external"].(map[string]any)
}

// The expected galleries are decoded independently of the request: in-place
// enrichment must not change the expected image blobs, order, alt or ratios.
func createNestedUnfurlPost(t *testing.T, h *lexiconHarness, embed map[string]any) map[string]any {
	t.Helper()
	expected := jsonObject(t, postWriteExternalComposite)
	expectedURI := nestedUnfurlExternal(embed)["uri"]
	_, err := h.create(CreatePostRequest{
		Title: lexiconStringPtr("Harbour repairs"), Content: lexiconStringPtr("The pier is open."), Embed: embed,
	})
	require.NoError(t, err, "thumbnail enhancement must not prevent post creation")
	stored, ok := h.repo.onlyRecord(t).Value["embed"].(map[string]any)
	require.True(t, ok, "the author record must retain its embed")
	assert.Equal(t, "social.coves.embed.imagesWithAttachment", stored["$type"])
	assert.Equal(t, expected["images"], stored["images"], "complete main gallery is unchanged")
	attachment, ok := stored["attachment"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "social.coves.embed.external", attachment["$type"])
	external, ok := attachment["external"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, nestedUnfurlExternal(expected)["images"], external["images"], "complete external gallery is unchanged")
	assert.Equal(t, expectedURI, external["uri"])
	return external
}

// B1 owns create wiring and the real task24 fetch/sniff/sanitize/upload path.
func TestCreatePost_ImagesWithAttachmentUnfurlSanitizesThumbnail(t *testing.T) {
	source := testkit.JPEGWithSegments(testkit.TestJPEG(48, 32), testkit.JPEGExifGPSegment(1))
	require.True(t, bytes.Contains(source, []byte("Exif\x00\x00")), "fixture contains removable EXIF")
	nestedBlueskyURI := blueskyPostURL("test.bsky.social", "xyz789")
	for _, test := range []struct{ name, uri string }{
		{"news link", "https://news.example.com/story"},
		{"Bluesky link retains galleries", nestedBlueskyURI},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, author, fetches := nestedUnfurlHarness(t, unfurl.UnfurlResult{Type: "article"},
				&servedThumbnail{body: source, contentType: "image/png"})
			if test.uri == nestedBlueskyURI {
				h.service.(*postService).blueskyService = &mockBlueskyService{
					isBlueskyURLResult: true,
					parseURLResult:     "at://did:plc:abcdef/app.bsky.feed.post/xyz789",
					resolvePostResult: &blueskypost.BlueskyPostResult{
						URI: "at://did:plc:abcdef/app.bsky.feed.post/xyz789",
						CID: "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
					},
				}
			}
			embed := jsonObject(t, postWriteExternalComposite)
			nestedUnfurlExternal(embed)["uri"] = test.uri
			delete(nestedUnfurlExternal(embed), "thumb")

			external := createNestedUnfurlPost(t, h, embed)
			require.Contains(t, external, "thumb", "unfurl must fill the nested external thumbnail")
			require.Len(t, author.uploads, 1, "one thumbnail reaches the author's repo")
			assert.Positive(t, fetches.Load(), "the real blob service fetched the thumbnail")
			uploaded := author.uploads[0]
			assert.Equal(t, "image/jpeg", uploaded.mimeType, "sniff bytes rather than trusting the origin's PNG header")
			assert.False(t, bytes.Contains(uploaded.data, []byte("Exif\x00\x00")), "raw EXIF must not reach author storage")
			segments, _, err := testkit.WalkJPEGSegments(uploaded.data)
			require.NoError(t, err)
			for _, segment := range segments {
				assert.NotEqual(t, byte(0xe1), segment.Marker, "no APP1 metadata remains")
			}
			config, format, err := image.DecodeConfig(bytes.NewReader(uploaded.data))
			require.NoError(t, err)
			assert.Equal(t, "jpeg", format)
			assert.Equal(t, 48, config.Width)
			assert.Equal(t, 32, config.Height)
			assert.Equal(t, map[string]any{
				"$type": "blob", "ref": map[string]any{"$link": nestedThumbnailCID},
				"mimeType": "image/jpeg", "size": float64(len(uploaded.data)),
			}, external["thumb"], "persist the blob returned by the author's sanitized upload")
		})
	}
}

// B2 permits metadata unfurl, but a supplied thumb must never be fetched again.
func TestCreatePost_ImagesWithAttachmentUnfurlPreservesSuppliedThumbnail(t *testing.T) {
	h, author, fetches := nestedUnfurlHarness(t, unfurl.UnfurlResult{Type: "article"},
		&servedThumbnail{body: testkit.TestPNG(48, 32), contentType: "image/png"})
	embed := jsonObject(t, postWriteExternalComposite)
	expectedThumb := nestedUnfurlExternal(jsonObject(t, postWriteExternalComposite))["thumb"]

	external := createNestedUnfurlPost(t, h, embed)
	assert.Equal(t, expectedThumb, external["thumb"], "keep the complete supplied blob")
	assert.Zero(t, fetches.Load(), "a supplied thumb prevents a thumbnail-origin fetch")
	assert.Empty(t, author.uploads, "a supplied thumb prevents a thumbnail upload")
}

// B3 owns nonfatal rejection, including GIF which the general upload pipeline
// accepts but the link-card thumbnail path refuses.
func TestCreatePost_ImagesWithAttachmentUnfurlRejectsInvalidThumbnailNonfatally(t *testing.T) {
	for _, test := range []struct {
		name   string
		served servedThumbnail
	}{
		{"HTML claiming PNG", servedThumbnail{body: []byte("<!doctype html><html>not an image</html>"), contentType: "image/png"}},
		{"GIF", servedThumbnail{body: testkit.StaticGIF87a(), contentType: "image/gif"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, author, _ := nestedUnfurlHarness(t, unfurl.UnfurlResult{Type: "article"}, &test.served)
			embed := jsonObject(t, postWriteExternalComposite)
			delete(nestedUnfurlExternal(embed), "thumb")

			external := createNestedUnfurlPost(t, h, embed)
			assert.NotContains(t, external, "thumb", "rejected bytes must not create a nested thumbnail")
			assert.Empty(t, author.uploads, "rejected bytes must not reach author storage")
		})
	}
}

// B4 pins standalone metadata semantics with literal expected values.
func TestCreatePost_ImagesWithAttachmentUnfurlMetadata(t *testing.T) {
	for _, test := range []struct {
		name                                                   string
		provided                                               bool
		title, description, expectedTitle, expectedDescription string
	}{
		{name: "absent text is filled", expectedTitle: "Linked headline", expectedDescription: "Linked summary"},
		{name: "provided text is kept", provided: true, title: "Poster headline", description: "Poster summary", expectedTitle: "Poster headline", expectedDescription: "Poster summary"},
		{name: "explicit empty text is kept", provided: true, expectedTitle: "", expectedDescription: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, author, _ := nestedUnfurlHarness(t, unfurl.UnfurlResult{
				Title: "Linked headline", Description: "Linked summary", Provider: "Harbour News", Domain: "news.example.com", Type: "article",
			}, nil)
			embed := jsonObject(t, postWriteExternalComposite)
			external := nestedUnfurlExternal(embed)
			delete(external, "thumb")
			delete(external, "title")
			delete(external, "description")
			if test.provided {
				external["title"], external["description"] = test.title, test.description
			}
			external["provider"], external["domain"], external["embedType"] = "Old provider", "old.example.com", "website"

			stored := createNestedUnfurlPost(t, h, embed)
			assert.Equal(t, test.expectedTitle, stored["title"])
			assert.Equal(t, test.expectedDescription, stored["description"])
			assert.Equal(t, "Harbour News", stored["provider"])
			assert.Equal(t, "news.example.com", stored["domain"])
			assert.Equal(t, "article", stored["embedType"])
			assert.NotContains(t, stored, "thumb", "metadata without a thumbnail URL creates no thumb")
			assert.Empty(t, author.uploads)
		})
	}
}
