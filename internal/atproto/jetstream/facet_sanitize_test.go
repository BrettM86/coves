package jetstream

import (
	"strings"
	"testing"
)

// facetFixture builds a facet map the way encoding/json decodes it (float64 numbers).
func facetFixture(byteStart, byteEnd float64) interface{} {
	return map[string]interface{}{
		"index": map[string]interface{}{
			"byteStart": byteStart,
			"byteEnd":   byteEnd,
		},
		"features": []interface{}{
			map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
		},
	}
}

func TestSanitizedPostFacets(t *testing.T) {
	content := "Hello world"

	t.Run("nil facets pass through as nil", func(t *testing.T) {
		if got := sanitizeFacets(nil, &content, "at://did:plc:x/social.coves.community.post/1"); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("nil content drops every facet and returns nil", func(t *testing.T) {
		if got := sanitizeFacets(
			[]interface{}{facetFixture(0, 5)}, nil,
			"at://did:plc:x/social.coves.community.post/1",
		); got != nil {
			t.Errorf("expected nil for facets on a post with no content, got %v", got)
		}
	})

	t.Run("invalid facet dropped, valid kept", func(t *testing.T) {
		got := sanitizeFacets(
			[]interface{}{
				facetFixture(0, 5),
				facetFixture(0, 999), // out of range
			},
			&content, "at://did:plc:x/social.coves.community.post/1",
		)
		if len(got) != 1 {
			t.Fatalf("expected 1 surviving facet, got %d", len(got))
		}
	})
}

func TestSerializeOptionalFields_SanitizesFacets(t *testing.T) {
	record := &CommentRecordFromJetstream{
		Content: "Hello world",
		Facets: []interface{}{
			facetFixture(0, 5),
			facetFixture(0, 999), // out of range
		},
	}

	facetsJSON, _, _, err := serializeOptionalFields(record, "at://did:plc:x/social.coves.community.comment/1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facetsJSON == nil {
		t.Fatal("expected surviving facet to be serialized")
	}
	if strings.Contains(*facetsJSON, "999") {
		t.Errorf("out-of-range facet survived sanitization: %s", *facetsJSON)
	}
	if !strings.Contains(*facetsJSON, `"byteEnd":5`) {
		t.Errorf("valid facet missing from serialized output: %s", *facetsJSON)
	}
	// The helper must not mutate the caller's record
	if len(record.Facets) != 2 {
		t.Errorf("serializeOptionalFields mutated the input record: %d facets left", len(record.Facets))
	}
}

func TestSerializeOptionalFields_AllFacetsInvalid(t *testing.T) {
	record := &CommentRecordFromJetstream{
		Content: "Hi",
		Facets:  []interface{}{facetFixture(0, 999)},
	}
	facetsJSON, _, _, err := serializeOptionalFields(record, "at://did:plc:x/social.coves.community.comment/1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facetsJSON != nil {
		t.Errorf("expected nil facetsJSON when nothing survives, got %s", *facetsJSON)
	}
}

func TestSanitizedDescriptionFacets(t *testing.T) {
	profile := &CommunityProfile{
		Description: "A community",
		DescriptionFacets: []interface{}{
			facetFixture(0, 11),
			facetFixture(5, 500), // out of range
		},
	}
	got := sanitizedDescriptionFacets(profile, "did:plc:community")
	if len(got) != 1 {
		t.Fatalf("expected 1 surviving description facet, got %d", len(got))
	}

	t.Run("nil facets stay nil", func(t *testing.T) {
		if got := sanitizedDescriptionFacets(&CommunityProfile{Description: "x"}, "did:plc:community"); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
}
