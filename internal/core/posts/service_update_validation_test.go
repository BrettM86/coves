//go:build integration

package posts_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What an EDIT is allowed to put into a signed record.
//
// # THE APP LAYER IS THE ONLY GATE THERE IS
//
// Every write on this path goes out as putRecord with `validate: false`, because
// the Coves lexicons are ones no PDS has been taught. That is a deliberate and
// correct choice, and it has a consequence people forget: the PDS will sign and
// publish literally any JSON the AppView hands it. There is no second opinion
// downstream. Whatever validateCreateRequest refuses on the way in is the entire
// definition of a well-formed Coves post — and an edit path that skipped those
// checks would be a hole straight through all of them.
//
// The hole is not theoretical. CreatePost refuses a thumb sent as a URL string
// (it is a blob reference or it is nothing), a title past the lexicon's cap,
// facets whose byte ranges fall outside the content they annotate, a label
// outside the allowlist, and an embed that matches no union member. An UpdatePost
// with none of those checks lets a client create a clean post and then edit it
// into every one of those shapes — signed by the author's own key, published to
// the firehose, and handed to every consumer in the network as a valid record.
//
// So the checks have to be SHARED, not merely duplicated. Two copies drift, and
// the copy that drifts is the one nobody is looking at. The pins below assert
// the property that makes sharing observable from outside: create and update
// refuse the SAME input for the SAME reason.
//
// # WHY THE FACET CHECK IS THE SUBTLE ONE
//
// Facets carry byte offsets into the content. An edit that changes the content
// changes what those offsets mean, so the check has to run against the MERGED
// record — the content the edit will actually produce — not against the content
// the request happens to carry. An update that sends new facets and no content
// must be validated against the STANDING content; one that sends new content and
// no facets must re-validate the STANDING facets against it. Either omission
// leaves a record whose annotations slice outside its own text, which is a
// renderer crash on somebody else's client.

// editValidationFixture is the write path with an already-standing post to edit.
type editValidationFixture struct {
	*postFixture

	post *posts.CreatePostResponse
}

func newEditValidationFixture(t *testing.T) *editValidationFixture {
	t.Helper()

	f := newPostFixture(t)
	return &editValidationFixture{
		postFixture: f,
		post:        f.createPost(t, f.author.DID, "a valid post", "a body with some text in it"),
	}
}

// edit applies req to the standing post as its author.
func (f *editValidationFixture) edit(t *testing.T, req posts.UpdatePostRequest) (*posts.UpdatePostResponse, error) {
	t.Helper()

	req.URI = f.post.URI
	return f.service.UpdatePost(context.Background(), sessionFor(t, f.author, f.pds.URL()), req)
}

// standingRecord reads back what the author's repo actually holds.
func (f *editValidationFixture) standingRecord(t *testing.T) map[string]any {
	t.Helper()
	return f.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, f.post.URI)).Value
}

func TestService_UpdateRefusesContentCreateWouldHaveRefused(t *testing.T) {
	t.Parallel()

	// Each case is an input CreatePost already refuses. None may reach a signed
	// record through the edit path — and each must come back as a VALIDATION
	// error, so the boundary answers 400 naming the field rather than a 500 that
	// tells the client nothing about what to fix.
	for _, tc := range []struct {
		req  posts.UpdatePostRequest
		name string
	}{
		{
			// The one the security review found first, and the one with teeth: a
			// thumb sent as a URL string. CreatePost refuses it because a thumb
			// is a blob reference — the image proxy resolves blobs, and a record
			// carrying a raw URL routes media around the proxy and therefore
			// around CSAM scanning. An edit that accepted it would put exactly
			// that record on the firehose.
			name: "a thumbnail sent as a URL string instead of a blob",
			req: posts.UpdatePostRequest{Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":   "https://example.com/an-article",
					"thumb": "https://example.com/thumb.png",
				},
			}},
		},
		{
			name: "a thumbnail blob missing its ref",
			req: posts.UpdatePostRequest{Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":   "https://example.com/an-article",
					"thumb": map[string]interface{}{"$type": "blob", "mimeType": "image/png"},
				},
			}},
		},
		{
			name: "a title past the lexicon's byte cap",
			req:  posts.UpdatePostRequest{Title: ptr(strings.Repeat("a", 3001))},
		},
		{
			name: "content past the lexicon's cap",
			req:  posts.UpdatePostRequest{Content: ptr(strings.Repeat("b", 100001))},
		},
		{
			name: "a label outside the allowlist",
			req: posts.UpdatePostRequest{
				Labels: &posts.SelfLabels{Values: []posts.SelfLabel{{Val: "not-a-real-label"}}},
			},
		},
		{
			name: "an embed matching no union member",
			req:  posts.UpdatePostRequest{Embed: map[string]interface{}{"$type": "social.coves.embed.nonsense"}},
		},
		{
			// The URI half of the parity, and the one that hid the longest:
			// validateEmbed only asks that external.uri be a non-empty string —
			// it never parses it. The scheme rules live in the NORMALIZATION
			// step, which the create path ran and the edit path did not, so an
			// author could post a clean link and then edit it into a
			// javascript: URI signed by their own key.
			name: "a javascript: external embed URI",
			req: posts.UpdatePostRequest{Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "javascript:alert(document.cookie)",
				},
			}},
		},
		{
			name: "a schemeless external embed URI",
			req: posts.UpdatePostRequest{Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "example.com/an-article",
				},
			}},
		},
		{
			// checkKnownFeature deliberately has no #link arm — the scheme rules
			// are NormalizeLinkURIs' job — so a structurally perfect facet
			// carrying a javascript: link passes every check the edit path used
			// to run.
			name: "a javascript: facet link URI",
			req: posts.UpdatePostRequest{Facets: []interface{}{map[string]interface{}{
				"index": map[string]interface{}{"byteStart": 0, "byteEnd": 6},
				"features": []interface{}{map[string]interface{}{
					"$type": "social.coves.richtext.facet#link",
					"uri":   "javascript:alert(document.cookie)",
				}},
			}}},
		},
		{
			// The sources cap is enforced by the normalization step and by
			// nothing else: validateEmbed does not inspect sources at all.
			name: "more embed sources than the lexicon allows",
			req: posts.UpdatePostRequest{Embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":     "https://example.com/an-article",
					"sources": tooManySources(51),
				},
			}},
		},
		{
			name: "more langs than the lexicon allows",
			req:  posts.UpdatePostRequest{Langs: []string{"en", "fr", "de", "es"}},
		},
		{
			name: "a lang that is not a language tag",
			req:  posts.UpdatePostRequest{Langs: []string{"definitely not a language"}},
		},
		{
			name: "more tags than the lexicon allows",
			req:  posts.UpdatePostRequest{Tags: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		},
		{
			name: "a tag past the lexicon's byte cap",
			req:  posts.UpdatePostRequest{Tags: []string{strings.Repeat("t", 641)}},
		},
		{
			name: "an empty tag",
			req:  posts.UpdatePostRequest{Tags: []string{"fine", ""}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newEditValidationFixture(t)
			before := f.standingRecord(t)

			_, err := f.edit(t, tc.req)
			require.Errorf(t, err, "the edit path accepted content the create path refuses, so a client "+
				"can post cleanly and then edit into a record no validation would ever have allowed")
			assert.Truef(t, posts.IsValidationError(err),
				"expected a validation error the boundary turns into a 400 naming the field, got: %v", err)

			// REFUSED MEANS NOTHING WAS SIGNED. Asserted against the standing
			// record rather than trusting the error, because a validation that
			// ran AFTER the put would return exactly this error over a record
			// that is already on the firehose.
			assert.Equal(t, before, f.standingRecord(t),
				"the refused edit was written anyway — validation must run before the put, not after it")
		})
	}
}

func TestService_UpdateValidatesFacetsAgainstTheMergedContent(t *testing.T) {
	t.Parallel()

	// THE MERGED RECORD, NOT THE REQUEST. A facet's byte range is meaningful
	// only against the content it annotates, and an edit produces content from
	// two sources: what the request carries and what the standing record already
	// holds. Validating the request alone gets both of these wrong.

	t.Run("new facets are checked against the STANDING content", func(t *testing.T) {
		t.Parallel()

		f := newEditValidationFixture(t)

		// The standing content is 27 bytes. A facet ending at 5000 slices far
		// past it. The request carries no content at all, so a validator looking
		// only at the request sees a zero-length string — or skips the check
		// entirely — and signs a record whose annotation points into nothing.
		_, err := f.edit(t, posts.UpdatePostRequest{
			Facets: []interface{}{map[string]interface{}{
				"index":    map[string]interface{}{"byteStart": 0, "byteEnd": 5000},
				"features": []interface{}{map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}},
			}},
		})
		require.Error(t, err)
		assert.Truef(t, posts.IsValidationError(err),
			"a facet slicing past the standing content must be refused; got: %v", err)
	})

	t.Run("standing facets are re-checked against NEW content", func(t *testing.T) {
		t.Parallel()

		f := newPostFixture(t)

		// A post whose facets are valid for its own content: 11 bytes annotated
		// out of 27.
		content := "a body with some text in it"
		title := "a post with rich text"
		created, err := f.service.CreatePost(
			testAuthCtx(f.author.DID),
			sessionFor(t, f.author, f.pds.URL()),
			posts.CreatePostRequest{
				Community: f.community.DID,
				Title:     &title,
				Content:   &content,
				AuthorDID: f.author.DID,
				Facets: []interface{}{map[string]interface{}{
					"index":    map[string]interface{}{"byteStart": 0, "byteEnd": 11},
					"features": []interface{}{map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}},
				}},
			})
		require.NoError(t, err)

		// The edit SHORTENS the content to 4 bytes and sends no facets. The
		// standing facet now slices past the end of the record it is part of.
		// The edit must be refused rather than signing an inconsistent record —
		// truncating or dropping the facets silently would also be wrong, since
		// the author never asked for their formatting to be discarded.
		shorter := "tiny"
		_, err = f.service.UpdatePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
			posts.UpdatePostRequest{URI: created.URI, Content: &shorter})
		require.Errorf(t, err, "shortening the content past its own facets must be refused: the record "+
			"that would be signed carries an annotation slicing outside its own text")
		assert.Truef(t, posts.IsValidationError(err), "got: %v", err)
	})
}

func TestService_UpdateStillAcceptsAValidEdit(t *testing.T) {
	t.Parallel()

	// The half that keeps the pins above honest. Every one of them would also
	// pass against an UpdatePost that refused everything, and a validator copied
	// from the create path carries a field the edit path does not populate —
	// community, authorDid — which is exactly how a shared validator ends up
	// refusing every legitimate edit.
	f := newEditValidationFixture(t)

	edited := "a longer body, still perfectly well formed"
	updated, err := f.edit(t, posts.UpdatePostRequest{
		Content: &edited,
		Labels:  &posts.SelfLabels{Values: []posts.SelfLabel{{Val: "nsfw"}}},
		Facets: []interface{}{map[string]interface{}{
			"index":    map[string]interface{}{"byteStart": 0, "byteEnd": 8},
			"features": []interface{}{map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}},
		}},
	})
	require.NoErrorf(t, err, "a valid edit was refused — the shared validator must not demand the "+
		"create-only fields (community, authorDid) an edit never carries")

	record := f.standingRecord(t)
	assert.Equal(t, edited, record["content"])
	assert.Equal(t, updated.CID, f.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, f.post.URI)).CID)
}

func TestService_CreateAndUpdateRefuseTheSameThumbForTheSameReason(t *testing.T) {
	t.Parallel()

	// THE SHARING ITSELF, asserted from outside. The pins above prove the edit
	// path refuses these inputs; this proves it refuses them because it runs the
	// SAME check, by putting one input through both entry points and requiring
	// the same answer. Two copies of a validator drift, and the copy that drifts
	// is the one nobody is looking at.
	f := newEditValidationFixture(t)

	badEmbed := map[string]interface{}{
		"$type": "social.coves.embed.external",
		"external": map[string]interface{}{
			"uri":   "https://example.com/an-article",
			"thumb": "https://example.com/thumb.png",
		},
	}

	title, content := "a post with a bad thumb", "body"
	_, createErr := f.service.CreatePost(
		testAuthCtx(f.author.DID),
		sessionFor(t, f.author, f.pds.URL()),
		posts.CreatePostRequest{
			Community: f.community.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: f.author.DID,
			Embed:     badEmbed,
		})
	require.Error(t, createErr)

	_, updateErr := f.edit(t, posts.UpdatePostRequest{Embed: badEmbed})
	require.Error(t, updateErr)

	assert.Equal(t, createErr.Error(), updateErr.Error(),
		"create and update must refuse this thumb with the identical message, because they must be "+
			"running the identical check — a different wording here means a second copy that will drift")
}

func TestService_UpdateNormalizesTheURIsItSigns(t *testing.T) {
	t.Parallel()

	// REFUSING THE DANGEROUS ONES IS ONLY HALF OF PARITY. The create path also
	// REPAIRS a recoverable URI — an unencoded character in a URL is a client
	// bug, not user intent — so that the record satisfies the lexicon's
	// `format: uri`. An edit path that only refused would still sign records the
	// create path would never have produced, and any third-party tool that
	// resolves our lexicons and validates the firehose judges the bytes we wrote.
	f := newEditValidationFixture(t)

	_, err := f.edit(t, posts.UpdatePostRequest{
		Embed: map[string]interface{}{
			"$type": "social.coves.embed.external",
			"external": map[string]interface{}{
				"uri": "https://example.com/a path/with spaces",
			},
		},
		Facets: []interface{}{map[string]interface{}{
			"index": map[string]interface{}{"byteStart": 0, "byteEnd": 6},
			"features": []interface{}{map[string]interface{}{
				"$type": "social.coves.richtext.facet#link",
				"uri":   "https://example.com/another path",
			}},
		}},
	})
	require.NoError(t, err, "a repairable URI is normalized, not refused")

	record := f.standingRecord(t)
	external, ok := record["embed"].(map[string]any)["external"].(map[string]any)
	require.True(t, ok, "the edited record kept its external embed")
	assert.Equal(t, "https://example.com/a%20path/with%20spaces", external["uri"],
		"the signed record must satisfy the lexicon's format: uri, exactly as the create path guarantees")

	facet := record["facets"].([]any)[0].(map[string]any)
	feature := facet["features"].([]any)[0].(map[string]any)
	assert.Equal(t, "https://example.com/another%20path", feature["uri"],
		"a facet link URI is normalized on the edit path too")
}

func TestService_UpdateAcceptsTagsAndLangsWithinTheLexiconCaps(t *testing.T) {
	t.Parallel()

	// The half that keeps the tags/langs pins honest: the caps must refuse what
	// the lexicon refuses and nothing more, or an author loses fields the schema
	// allows them.
	f := newEditValidationFixture(t)

	_, err := f.edit(t, posts.UpdatePostRequest{
		Langs: []string{"en", "fr", "de"},
		Tags:  []string{"one", "two", "three", "four", "five", "six", "seven", "eight"},
	})
	require.NoError(t, err, "the lexicon allows 3 langs and 8 tags; refusing them would drop fields the schema permits")

	record := f.standingRecord(t)
	assert.Equal(t, []any{"en", "fr", "de"}, record["langs"])
	assert.Len(t, record["tags"], 8)
}

// tooManySources builds an external embed sources array of n well-formed
// entries, for the cases that must be refused on length alone.
func tooManySources(n int) []interface{} {
	sources := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		sources = append(sources, map[string]interface{}{
			"uri": fmt.Sprintf("https://example.com/source-%d", i),
		})
	}
	return sources
}

// ptr is the one-liner every table above needs for an optional string field.
func ptr(s string) *string { return &s }

// testAuthCtx is the authenticated context CreatePost cross-checks the request
// DID against.
func testAuthCtx(authorDID string) context.Context {
	return middleware.SetTestUserDID(context.Background(), authorDID)
}
