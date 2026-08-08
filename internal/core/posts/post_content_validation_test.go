package posts

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shared content gate, exercised through the CREATE entry point.
//
// Its edit-path twin is service_update_validation_test.go, which proves the
// same inputs are refused on the way through UpdatePost. These two files
// together are the parity claim: what is refused here is refused there, because
// it is the same function.
//
// # WHY THE URI RULES BELONG IN THE SHARED GATE
//
// They used to live in CreatePost itself, one step after validation, so an edit
// never ran them. validateEmbed only asks that external.uri be a NON-EMPTY
// STRING — it never parses it, and it does not look at external.sources at all —
// and richtext's structural check deliberately has no #link arm. So without the
// normalization step nothing anywhere refuses a javascript: URI, a schemeless
// one, or a sources array past the lexicon's cap.
//
// This is defence in depth and schema conformance, not the system's only XSS
// defence: an author can write any record they like straight into their own PDS
// repo, and the firehose ingest path does not scheme-check what it indexes. What
// it does guarantee is that a record THIS AppView signs conforms to the lexicon
// it claims, on both the paths that produce one.

func embedExternal(uri string, extra map[string]interface{}) map[string]interface{} {
	external := map[string]interface{}{"uri": uri}
	for k, v := range extra {
		external[k] = v
	}
	return map[string]interface{}{
		"$type":    embedTypeExternal,
		"external": external,
	}
}

func linkFacet(uri string) []interface{} {
	return []interface{}{map[string]interface{}{
		"index": map[string]interface{}{"byteStart": 0, "byteEnd": 5},
		"features": []interface{}{map[string]interface{}{
			"$type": "social.coves.richtext.facet#link",
			"uri":   uri,
		}},
	}}
}

func sourcesOfLength(n int) []interface{} {
	sources := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		sources = append(sources, map[string]interface{}{
			"uri": fmt.Sprintf("https://example.com/source-%d", i),
		})
	}
	return sources
}

// createRequestWith is a minimally valid create request with content long
// enough for the facets these cases attach to it.
func createRequestWith(mutate func(*CreatePostRequest)) CreatePostRequest {
	content := "hello world"
	req := CreatePostRequest{
		Community: "did:plc:community1234567890",
		AuthorDID: "did:plc:author1234567890abc",
		Content:   &content,
	}
	mutate(&req)
	return req
}

func TestValidateCreateRequest_RefusesURIsTheRecordMayNotCarry(t *testing.T) {
	t.Parallel()

	service := &postService{}

	for _, tc := range []struct {
		mutate func(*CreatePostRequest)
		name   string
		field  string
	}{
		{
			name:   "a javascript: external embed URI",
			field:  "embed.external.uri",
			mutate: func(r *CreatePostRequest) { r.Embed = embedExternal("javascript:alert(1)", nil) },
		},
		{
			name:   "a data: external embed URI",
			field:  "embed.external.uri",
			mutate: func(r *CreatePostRequest) { r.Embed = embedExternal("data:text/html,<script>", nil) },
		},
		{
			name:   "a schemeless external embed URI",
			field:  "embed.external.uri",
			mutate: func(r *CreatePostRequest) { r.Embed = embedExternal("example.com/article", nil) },
		},
		{
			name:  "more embed sources than the lexicon allows",
			field: "embed.external.sources",
			mutate: func(r *CreatePostRequest) {
				r.Embed = embedExternal("https://example.com/a", map[string]interface{}{
					"sources": sourcesOfLength(maxEmbedSources + 1),
				})
			},
		},
		{
			name:  "a javascript: URI on an embed source",
			field: "embed.external.sources[0].uri",
			mutate: func(r *CreatePostRequest) {
				r.Embed = embedExternal("https://example.com/a", map[string]interface{}{
					"sources": []interface{}{map[string]interface{}{"uri": "javascript:alert(1)"}},
				})
			},
		},
		{
			name:   "a javascript: facet link URI",
			field:  "facets",
			mutate: func(r *CreatePostRequest) { r.Facets = linkFacet("javascript:alert(1)") },
		},
		{
			name:   "a vbscript: facet link URI",
			field:  "facets",
			mutate: func(r *CreatePostRequest) { r.Facets = linkFacet("vbscript:msgbox(1)") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := createRequestWith(tc.mutate)
			err := service.validateCreateRequest(&req)
			require.Errorf(t, err, "%s must never reach a record this AppView signs", tc.name)
			require.Truef(t, IsValidationError(err),
				"the boundary answers 400 naming the field; got: %v", err)
			assert.Containsf(t, err.Error(), tc.field,
				"the message must name the field the client has to fix; got: %v", err)
		})
	}
}

func TestValidateCreateRequest_NormalizesURIsInPlace(t *testing.T) {
	t.Parallel()

	// A repairable URI is REPAIRED, not refused: an unencoded character in a URL
	// is a client bug rather than user intent, and the record has to satisfy the
	// lexicon's `format: uri` for every third-party validator on the firehose.
	// The repair happens in place, because the record is built from these same
	// values afterwards.
	service := &postService{}

	req := createRequestWith(func(r *CreatePostRequest) {
		r.Embed = embedExternal("https://example.com/a path", map[string]interface{}{
			"sources": []interface{}{map[string]interface{}{"uri": "https://example.com/another path"}},
		})
		r.Facets = linkFacet("https://example.com/a third path")
	})

	require.NoError(t, service.validateCreateRequest(&req))

	external := req.Embed["external"].(map[string]interface{})
	assert.Equal(t, "https://example.com/a%20path", external["uri"])
	source := external["sources"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "https://example.com/another%20path", source["uri"])

	feature := req.Facets[0].(map[string]interface{})["features"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "https://example.com/a%20third%20path", feature["uri"],
		"the normalization must mutate the request the record is built from, not a copy")
}

// The lexicon's caps on langs and tags, which nothing in Go enforced: neither
// field was on postContent, so the shared validator never saw them, and the only
// bound either had was the handler's 1MB body cap. Both the postv2 record
// lexicon and the create/update procedure lexicons declare the same limits.
func TestValidatePostContent_LangsAndTags(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		field   string
		content postContent
	}{
		{
			name:    "more langs than the lexicon allows",
			field:   "langs",
			content: postContent{Langs: []string{"en", "fr", "de", "es"}},
		},
		{
			name:    "a lang that is not a language tag",
			field:   "langs",
			content: postContent{Langs: []string{"definitely not a language"}},
		},
		{
			name:    "an empty lang",
			field:   "langs",
			content: postContent{Langs: []string{""}},
		},
		{
			name:    "more tags than the lexicon allows",
			field:   "tags",
			content: postContent{Tags: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		},
		{
			name:    "a tag past the lexicon's byte cap",
			field:   "tags",
			content: postContent{Tags: []string{strings.Repeat("t", maxTagLength+1)}},
		},
		{
			name:    "an empty tag",
			field:   "tags",
			content: postContent{Tags: []string{"fine", ""}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := normalizeAndValidatePostContent(tc.content)
			require.Errorf(t, err, "%s must be refused before the record is signed", tc.name)
			require.Truef(t, IsValidationError(err), "got: %v", err)
			assert.Contains(t, err.Error(), tc.field)
		})
	}

	t.Run("the caps themselves are accepted", func(t *testing.T) {
		t.Parallel()

		// The half that keeps the pins above honest: a validator that refused
		// everything would pass every one of them, and would silently cost
		// authors fields the lexicon allows.
		require.NoError(t, normalizeAndValidatePostContent(postContent{
			Langs: []string{"en", "fr", "de"},
			Tags:  []string{"a", "b", "c", "d", "e", "f", "g", strings.Repeat("t", maxTagLength)},
		}))
	})

	t.Run("no langs and no tags are accepted", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, normalizeAndValidatePostContent(postContent{}))
	})
}

// Both lexicons declare langs and tags on their input, and the postv2 record
// declares them too — so a create that dropped them while an update honoured
// them would have an author unable to set on submission a field they can set one
// second later by editing.
func TestPostRecordFor_CarriesLangsAndTags(t *testing.T) {
	t.Parallel()

	req := CreatePostRequest{
		Community: "gaming.coves.social",
		AuthorDID: "did:plc:author1234567890abc",
		Langs:     []string{"en", "fr"},
		Tags:      []string{"golang", "atproto"},
	}

	record := postRecordFor(req, "did:plc:community1234567890", "2026-08-01T12:00:00Z")
	assert.Equal(t, []string{"en", "fr"}, record.Langs)
	assert.Equal(t, []string{"golang", "atproto"}, record.Tags)

	written := postV2From(record)
	assert.Equal(t, []string{"en", "fr"}, written.Langs,
		"langs must survive the write boundary, or the create path silently discards them")
	assert.Equal(t, []string{"golang", "atproto"}, written.Tags,
		"tags must survive the write boundary, or the create path silently discards them")
}
