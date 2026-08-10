package posts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The blob copy leg, whose two default behaviours both destroyed data silently.

// A blob larger than the copy cap used to be uploaded TRUNCATED.
//
// io.ReadAll over an io.LimitReader cannot distinguish "the body ended" from
// "the limit was reached" — both come back as a successful read of exactly N
// bytes with a nil error. So an oversized blob was silently cut, uploaded under
// a DIFFERENT CID (blob CIDs are content-addressed), and the postv2 kept the
// original reference — pointing at bytes the author's repo does not hold. The
// legacy record, and with it the only intact copy, was then deleted.
func TestRematerializeBlobClient_Fetch_FailsRatherThanTruncating(t *testing.T) {
	const cap = 1024
	oversized := strings.Repeat("x", cap+64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oversized))
	}))
	defer server.Close()

	client := newRematerializeBlobClient(server.Client(), cap)
	_, err := client.Fetch(context.Background(), server.URL, "did:plc:community2222222222222222", "bafkreioversized")

	require.Errorf(t, err,
		"an oversized blob was read without complaint. A truncated read is uploaded under a different CID and the postv2 ends up referencing media the "+
			"author's repo does not serve — after the community's copy has been deleted. Read limit+1 and refuse the overrun")
	assert.Containsf(t, err.Error(), "truncated",
		"the error must say what the danger is: a truncated copy is DIFFERENT bytes, not fewer bytes")
}

func TestRematerializeBlobClient_Fetch_ReturnsTheWholeBodyUnderTheCap(t *testing.T) {
	payload := strings.Repeat("y", 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()

	data, err := newRematerializeBlobClient(server.Client(), 8192).
		Fetch(context.Background(), server.URL, "did:plc:community2222222222222222", "bafkreiok")
	require.NoError(t, err)
	assert.Equalf(t, payload, string(data), "a blob inside the cap must come back byte-for-byte")
}

// "I could not ask" is not "it is not there", and neither of them is "it is
// there". The probe used to collapse a request-construction error, a transport
// error and a 404 into a single false — so a network blip refused a healthy
// record, and any future change of polarity would have licensed deleting the
// last copy of a blob.
func TestRematerializeBlobClient_Present_DistinguishesAbsentFromUnaskable(t *testing.T) {
	t.Run("200 is present", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("bytes"))
		}))
		defer server.Close()

		present, err := DefaultRematerializeBlobClient().Present(context.Background(), server.URL, "did:plc:x2222222222222222222222", "bafkrei1")
		require.NoError(t, err)
		assert.True(t, present)
	})

	t.Run("404 is absent, and not an error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		present, err := DefaultRematerializeBlobClient().Present(context.Background(), server.URL, "did:plc:x2222222222222222222222", "bafkrei1")
		require.NoErrorf(t, err, "a definite 404 is an ANSWER — the blob is not there — and must not be reported as a failure to ask")
		assert.False(t, present)
	})

	t.Run("a 503 is neither, and must be an error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		_, err := DefaultRematerializeBlobClient().Present(context.Background(), server.URL, "did:plc:x2222222222222222222222", "bafkrei1")
		require.Errorf(t, err,
			"a 503 was collapsed into 'absent'. The caller uses this answer to decide whether it is safe to delete the record that keeps the community's "+
				"only copy of the bytes alive, and a server that could not answer has told it nothing")
	})

	t.Run("an unreachable host is an error, never a false", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := server.URL
		server.Close() // nothing is listening now

		_, err := DefaultRematerializeBlobClient().Present(context.Background(), url, "did:plc:x2222222222222222222222", "bafkrei1")
		require.Errorf(t, err, "a transport failure must surface; reporting it as 'absent' turns a blip into a refusal and a polarity slip into data loss")
	})

	t.Run("an unbuildable URL is an error, never a false", func(t *testing.T) {
		_, err := DefaultRematerializeBlobClient().Present(context.Background(), "", "did:plc:x2222222222222222222222", "bafkrei1")
		require.Errorf(t, err, "a URL that could not be built means the question was never asked")
	})
}

// THE BLOB REFERENCE IS CARRIED THROUGH UNCHANGED, ON PURPOSE.
//
// An external review claimed the conversion is broken because the postv2 keeps
// the community-repo blob CID rather than rewriting it to the CID of the
// re-upload. It is not broken: atProto blob CIDs are CONTENT-ADDRESSED, so
// re-uploading identical bytes yields the identical CID and the reference stays
// valid — which is also why the real-PDS contract's getBlob against the author's
// repo returns 200 for the community's original CID.
//
// That is a property of the PDS, though, not of this code. A host that
// re-encoded on upload would mint a different CID and the postv2 would reference
// bytes the repo does not serve — so uploadEmbedBlobs CHECKS the returned CID
// rather than assuming it, and verifyEmbedBlobsPresent independently proves the
// referenced CID is actually served before anything is deleted.
func TestPostV2Body_CarriesTheEmbedBlobReferenceThroughUnchanged(t *testing.T) {
	blobRef := map[string]any{
		"$type":    "blob",
		"ref":      map[string]any{"$link": "bafkreicommunityblobcid"},
		"mimeType": "image/png",
		"size":     float64(12),
	}
	legacy := LegacyPost{
		URI: "at://did:plc:community2222222222222222/social.coves.community.post/3kabc",
		RawRecord: map[string]any{
			"$type":  LegacyPostCollection,
			"author": "did:plc:author11111111111111111",
			"embed": map[string]any{
				"$type":  "social.coves.embed.images",
				"images": []any{map[string]any{"alt": "a picture", "image": blobRef}},
			},
		},
	}

	body, err := postV2Body(legacy)
	require.NoError(t, err)

	refs := extractBlobRefs(body)
	require.Lenf(t, refs, 1, "the conversion lost the embed's blob reference entirely")
	assert.Equalf(t, "bafkreicommunityblobcid", refs[0].cid,
		"the blob reference was REWRITTEN. It must be carried through verbatim: a blob CID is the hash of its bytes, so the re-upload of identical bytes "+
			"has the identical CID, and inventing a new one would point the postv2 at media that does not exist")
	assert.Equalf(t, "image/png", refs[0].mimeType,
		"the MIME type must survive: it is sent as the upload's Content-Type, and a PDS enforcing the granular blob:*/* scope rejects a wildcard")
}

// extractBlobRefs has to find a blob wherever the embed union puts it, because a
// blob it does not find is a blob that is never copied — and the record that
// referenced it is deleted anyway.
func TestExtractBlobRefs_FindsBlobsNestedAnywhereInTheRecord(t *testing.T) {
	record := map[string]any{
		"embed": map[string]any{
			"$type": "social.coves.embed.external",
			"external": map[string]any{
				"uri":   "https://example.com",
				"thumb": map[string]any{"$type": "blob", "ref": map[string]any{"$link": "bafkreithumb"}, "mimeType": "image/jpeg"},
			},
		},
		"images": []any{
			map[string]any{"image": map[string]any{"$type": "blob", "ref": map[string]any{"$link": "bafkreiimage1"}}},
			map[string]any{"image": map[string]any{"$type": "blob", "ref": "bafkreiimage2"}}, // the bare-string ref shape
		},
	}

	refs := extractBlobRefs(record)

	found := map[string]bool{}
	for _, ref := range refs {
		found[ref.cid] = true
	}
	for _, want := range []string{"bafkreithumb", "bafkreiimage1", "bafkreiimage2"} {
		assert.Truef(t, found[want],
			"the blob %s was not found. A blob this walk misses is one whose bytes are never copied into the author's repo — and the record that held "+
				"the only reference to them is deleted regardless, so the media is lost", want)
	}
}
