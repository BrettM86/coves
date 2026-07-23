package posts

import (
	"strings"
	"testing"
)

// TestValidateCreateRequest_Facets verifies facets are structurally validated
// against the post content at the API boundary, so malformed annotations are
// rejected before a broken record is persisted to the PDS.
func TestValidateCreateRequest_Facets(t *testing.T) {
	service := &postService{}
	content := "Hello world"

	validFacet := func(byteStart, byteEnd float64) interface{} {
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

	baseRequest := func() CreatePostRequest {
		c := content
		return CreatePostRequest{
			Community: "did:plc:community123",
			AuthorDID: "did:plc:author123",
			Content:   &c,
		}
	}

	t.Run("valid facets pass", func(t *testing.T) {
		req := baseRequest()
		req.Facets = []interface{}{validFacet(0, 5)}
		if err := service.validateCreateRequest(&req); err != nil {
			t.Errorf("expected valid facets to pass, got: %v", err)
		}
	})

	t.Run("no facets pass", func(t *testing.T) {
		req := baseRequest()
		if err := service.validateCreateRequest(&req); err != nil {
			t.Errorf("expected request without facets to pass, got: %v", err)
		}
	})

	t.Run("facet beyond content length rejected", func(t *testing.T) {
		req := baseRequest()
		req.Facets = []interface{}{validFacet(0, 999)}
		err := service.validateCreateRequest(&req)
		if err == nil {
			t.Fatal("expected validation error for out-of-range facet")
		}
		if !strings.Contains(err.Error(), "exceeds content length") {
			t.Errorf("expected 'exceeds content length' error, got: %v", err)
		}
	})

	t.Run("facets without content rejected", func(t *testing.T) {
		req := baseRequest()
		req.Content = nil
		req.Facets = []interface{}{validFacet(0, 5)}
		if err := service.validateCreateRequest(&req); err == nil {
			t.Fatal("expected validation error for facets on a post with no content")
		}
	})

	t.Run("inverted range rejected", func(t *testing.T) {
		req := baseRequest()
		req.Facets = []interface{}{validFacet(8, 3)}
		if err := service.validateCreateRequest(&req); err == nil {
			t.Fatal("expected validation error for inverted byte range")
		}
	})
}
