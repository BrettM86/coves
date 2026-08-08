package posts

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shape of the record that goes into the AUTHOR's repository.
//
// social.coves.community.postv2 is a different record from the deprecated
// social.coves.community.post, not a renamed one, and the difference that
// matters is a field that is GONE. A post used to live in the community's repo,
// so the only thing that could say who wrote it was an `author` field the
// COMMUNITY's credentials had signed — an assertion by one party about another,
// which no consumer could verify. In the author's own repo the repository IS the
// attribution, and a relay or a DID-resolved fetch can check it.
//
// So the assertion below is not a tidiness check. A PostV2Record that carried an
// author field would put a self-asserted claim beside a verifiable one, and every
// consumer would have two answers to "who wrote this" with no rule for which
// wins. The postv2 lexicon does not declare the field at all, so a record
// carrying it is also a record with an unknown key in it.

func TestPostV2Record_CarriesNoAuthorField(t *testing.T) {
	t.Parallel()

	title := "a post in its author's repo"
	content := "the repository is the attribution"

	encoded, err := json.Marshal(PostV2Record{
		Type:      PostV2Collection,
		Community: "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa",
		Title:     &title,
		Content:   &content,
		CreatedAt: "2026-08-08T12:00:00Z",
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	// Asserted over the DECODED KEYS rather than by a substring search, so that
	// a post whose CONTENT mentions the word "author" cannot make this pass or
	// fail by accident.
	for _, forbidden := range []string{"author", "authorDid", "author_did"} {
		assert.NotContainsf(t, decoded, forbidden,
			"the postv2 record carries %q; authorship is the repository the record lives in, and a "+
				"self-asserted author field beside it gives consumers two answers to one question "+
				"with no rule for which wins", forbidden)
	}
}

func TestPostV2Record_RequiredFieldsAreAlwaysPresent(t *testing.T) {
	t.Parallel()

	// The lexicon requires community and createdAt. Both are spelled without
	// omitempty for that reason: a record missing either is refused by any
	// consumer validating against the schema, and an empty-but-present field
	// fails loudly at the validator instead of silently at the reader.
	encoded, err := json.Marshal(PostV2Record{Type: PostV2Collection})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	for _, required := range []string{"$type", "community", "createdAt"} {
		assert.Containsf(t, decoded, required,
			"the postv2 lexicon requires %q, so it must survive marshalling even when unset — "+
				"an omitempty here turns a wiring bug into a record that silently fails validation "+
				"on every consumer but ours", required)
	}
}

func TestPostV2Record_OptionalFieldsAreOmittedWhenUnset(t *testing.T) {
	t.Parallel()

	// The mirror of the above. An empty optional field is not the same as an
	// absent one: `"embed": null` is a union member the lexicon has no ref for,
	// and `"tags": []` is a tag list a client will render as an empty row of
	// chips. Absent means absent.
	encoded, err := json.Marshal(PostV2Record{
		Type:      PostV2Collection,
		Community: "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: "2026-08-08T12:00:00Z",
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	for _, optional := range []string{
		"title", "content", "facets", "embed", "langs", "labels",
		"tags", "crosspostOf", "crosspostChain", "bridgedStats",
	} {
		assert.NotContainsf(t, decoded, optional,
			"the postv2 record emitted %q with nothing in it", optional)
	}
}

func TestPostV2Record_CarriesEverySurfaceTheLexiconDeclares(t *testing.T) {
	t.Parallel()

	// Every optional property of social.coves.community.postv2, populated. A
	// field the Go type does not have is a field the write path can never
	// produce and a field an EDIT silently drops on round-trip — UpdatePost
	// reads the standing record, re-marshals it, and puts it back, so anything
	// absent from this struct is erased from a post the first time its author
	// fixes a typo.
	title := "every surface"
	content := "populated"

	encoded, err := json.Marshal(PostV2Record{
		Type:      PostV2Collection,
		Community: "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: "2026-08-08T12:00:00Z",
		Title:     &title,
		Content:   &content,
		Facets:    []interface{}{map[string]any{"$type": "social.coves.richtext.facet"}},
		Embed:     map[string]interface{}{"$type": "social.coves.embed.external"},
		Langs:     []string{"en", "fr"},
		Labels:    &SelfLabels{Values: []SelfLabel{{Val: "nsfw"}}},
		Tags:      []string{"gardening"},
		CrosspostOf: &StrongRef{
			URI: "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/" + PostV2Collection + "/3lrc77gmww4nc",
			CID: "bafyoriginal",
		},
		CrosspostChain: []StrongRef{{
			URI: "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/" + PostV2Collection + "/3lrc77gmww4nc",
			CID: "bafyoriginal",
		}},
		BridgedStats: &BridgedStats{Upvotes: 12, Downvotes: 3, AsOf: "2026-08-08T11:00:00Z"},
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	for _, declared := range []string{
		"$type", "community", "createdAt", "title", "content", "facets", "embed",
		"langs", "labels", "tags", "crosspostOf", "crosspostChain", "bridgedStats",
	} {
		assert.Containsf(t, decoded, declared,
			"the postv2 lexicon declares %q and the Go record does not emit it", declared)
	}

	// The two ref-typed surfaces are spelled the way the lexicons they point at
	// spell them — com.atproto.repo.strongRef is {uri, cid}, and #bridgedStats
	// is {upvotes, downvotes, asOf}. Asserted because a struct whose field names
	// marshalled to anything else would still satisfy the presence checks above
	// while producing a record no consumer can read.
	assert.Equal(t, map[string]any{
		"uri": "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/" + PostV2Collection + "/3lrc77gmww4nc",
		"cid": "bafyoriginal",
	}, decoded["crosspostOf"])

	assert.Equal(t, map[string]any{
		"upvotes":   float64(12),
		"downvotes": float64(3),
		"asOf":      "2026-08-08T11:00:00Z",
	}, decoded["bridgedStats"])
}
