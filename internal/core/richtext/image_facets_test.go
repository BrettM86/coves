package richtext

import (
	"errors"
	"testing"
)

// imageFeature builds a #image facet feature placing embed image `index`.
func imageFeature(index interface{}) map[string]interface{} {
	return map[string]interface{}{"$type": "social.coves.richtext.facet#image", "index": index}
}

// imagesEmbed builds a social.coves.embed.images embed holding n image blobs.
func imagesEmbed(n int) map[string]interface{} {
	links := []string{
		"bafkreibme22gw2h7y2h7tg2fhqotaqjucnbc24deqo72b6mkl2egezxhvy",
		"bafkreih6kvpkpwzi6vlk7cx6xoauvterrn3fqp75lwvj6saow4dbotcm2q",
	}
	images := make([]interface{}, n)
	for i := range images {
		images[i] = map[string]interface{}{
			"image": map[string]interface{}{
				"$type":    "blob",
				"ref":      map[string]interface{}{"$link": links[i%len(links)]},
				"mimeType": "image/jpeg",
				"size":     float64(120000 + i),
			},
			"alt": "Rack photo",
		}
	}
	return map[string]interface{}{"$type": "social.coves.embed.images", "images": images}
}

// tableContentByteLen is the content length every TestCheckImageFacets row is
// checked against; the rows' ranges lie within it unless a row says otherwise.
const tableContentByteLen = 20

func TestCheckImageFacets(t *testing.T) {
	codeFeature := map[string]interface{}{"$type": "social.coves.richtext.facet#code"}
	codeBlockFeature := map[string]interface{}{"$type": "social.coves.richtext.facet#codeBlock"}
	boldFeature := map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}

	tests := []struct {
		name    string
		facets  []interface{}
		embed   map[string]interface{}
		wantErr bool
	}{
		{
			name:    "rejects #image with no embed",
			facets:  []interface{}{facet(0, 3, imageFeature(float64(0)))},
			embed:   nil,
			wantErr: true,
		},
		{
			name:   "rejects #image with an external embed",
			facets: []interface{}{facet(0, 3, imageFeature(float64(0)))},
			embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":   "https://example.com/article",
					"title": "An article",
				},
			},
			wantErr: true,
		},
		{
			name:   "rejects #image against a non-images embed carrying a top-level images array",
			facets: []interface{}{facet(0, 3, imageFeature(float64(0)))},
			embed: map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":   "https://example.com/article",
					"title": "An article",
				},
				"images": imagesEmbed(1)["images"],
			},
			wantErr: true,
		},
		{
			name:   "rejects #image with a video embed",
			facets: []interface{}{facet(0, 3, imageFeature(float64(0)))},
			embed: map[string]interface{}{
				"$type": "social.coves.embed.video",
				"video": map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": "bafkreibme22gw2h7y2h7tg2fhqotaqjucnbc24deqo72b6mkl2egezxhvy"},
					"mimeType": "video/mp4",
					"size":     float64(5000000),
				},
			},
			wantErr: true,
		},
		{
			name:   "rejects #image with a quoted-post embed",
			facets: []interface{}{facet(0, 3, imageFeature(float64(0)))},
			embed: map[string]interface{}{
				"$type": "social.coves.embed.post",
				"post": map[string]interface{}{
					"uri": "at://did:plc:test123/social.coves.community.post/3k7a3dmb5bk2c",
					"cid": "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si",
				},
			},
			wantErr: true,
		},
		{
			name:    "rejects index equal to the image count",
			facets:  []interface{}{facet(0, 3, imageFeature(float64(3)))},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name:    "rejects negative index",
			facets:  []interface{}{facet(0, 3, imageFeature(float64(-1)))},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name: "rejects #image without an index",
			facets: []interface{}{facet(0, 3, map[string]interface{}{
				"$type": "social.coves.richtext.facet#image",
			})},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name:    "rejects non-integer index",
			facets:  []interface{}{facet(0, 3, imageFeature(1.5))},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name:    "rejects #image sharing its facet with another feature",
			facets:  []interface{}{facet(0, 3, imageFeature(float64(0)), boldFeature)},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name: "rejects the same index placed twice",
			facets: []interface{}{
				facet(0, 3, imageFeature(float64(1))),
				facet(5, 8, imageFeature(float64(1))),
			},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name: "rejects #image inside a #code range listed after it",
			facets: []interface{}{
				facet(4, 6, imageFeature(float64(0))),
				facet(2, 10, codeFeature),
			},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name: "rejects #image inside a #codeBlock range",
			facets: []interface{}{
				facet(0, 20, codeBlockFeature),
				facet(4, 6, imageFeature(float64(0))),
			},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name: "rejects #image range equal to an enclosing #code range",
			facets: []interface{}{
				facet(2, 10, codeFeature),
				facet(2, 10, imageFeature(float64(0))),
			},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name:    "rejects #image range running past the content",
			facets:  []interface{}{facet(15, 21, imageFeature(float64(0)))},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name: "accepts #code range running past the content beside a valid #image",
			facets: []interface{}{
				facet(15, 25, codeFeature),
				facet(0, 3, imageFeature(float64(0))),
			},
			embed:   imagesEmbed(3),
			wantErr: false,
		},
		{
			name:    "rejects #image sharing its facet with an earlier feature",
			facets:  []interface{}{facet(0, 3, boldFeature, imageFeature(float64(0)))},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name: "accepts #image partially overlapping a #code range",
			facets: []interface{}{
				facet(2, 10, codeFeature),
				facet(8, 14, imageFeature(float64(0))),
			},
			embed:   imagesEmbed(3),
			wantErr: false,
		},
		{
			name: "accepts #image enclosing a #code range",
			facets: []interface{}{
				facet(5, 8, codeFeature),
				facet(0, 20, imageFeature(float64(0))),
			},
			embed:   imagesEmbed(3),
			wantErr: false,
		},
		{
			name:    "rejects zero-length #image range",
			facets:  []interface{}{facet(3, 3, imageFeature(float64(0)))},
			embed:   imagesEmbed(3),
			wantErr: true,
		},
		{
			name: "accepts first and last indices placed on a 3-image embed",
			facets: []interface{}{
				facet(0, 3, imageFeature(float64(0))),
				facet(5, 8, imageFeature(float64(2))),
			},
			embed:   imagesEmbed(3),
			wantErr: false,
		},
		{
			name: "accepts #image touching the end of a #code range",
			facets: []interface{}{
				facet(2, 10, codeFeature),
				facet(10, 11, imageFeature(float64(0))),
			},
			embed:   imagesEmbed(3),
			wantErr: false,
		},
		{
			name:    "accepts a lone #code facet with no embed",
			facets:  []interface{}{facet(2, 10, codeFeature)},
			embed:   nil,
			wantErr: false,
		},
		{
			name:    "accepts nil facets with no embed",
			facets:  nil,
			embed:   nil,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckImageFacets(tt.facets, tt.embed, tableContentByteLen)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidImageFacet) {
					t.Fatalf("expected ErrInvalidImageFacet, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// Passes today by design: the #image write rules must stay out of the firehose structural check.
func TestSanitizeFacetsKeepsRuleBreakingImageFacets(t *testing.T) {
	facets := []interface{}{
		facet(0, 3, imageFeature(float64(0)), map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}),
		facet(5, 8, imageFeature(float64(8))),
	}
	kept, dropped := SanitizeFacets(facets, 10)
	if len(kept) != 2 || dropped != 0 {
		t.Fatalf("expected 2 kept and 0 dropped, got %d kept and %d dropped", len(kept), dropped)
	}
}

// Composite placements resolve only against the nested main gallery. The
// existing TestCheckImageFacets matrix owns the other placement rules.
func TestCheckImageFacets_ImagesWithAttachmentUsesMainGallery(t *testing.T) {
	embed := map[string]interface{}{
		"$type":  "social.coves.embed.imagesWithAttachment",
		"images": imagesEmbed(2),
		"attachment": map[string]interface{}{
			"$type": "social.coves.embed.external",
			"external": map[string]interface{}{
				"uri":    "https://example.com/harbour",
				"images": imagesEmbed(3)["images"],
			},
		},
	}
	t.Run("index 1 resolves in the main gallery", func(t *testing.T) {
		err := CheckImageFacets([]interface{}{facet(0, 3, imageFeature(float64(1)))}, embed, tableContentByteLen)
		if err != nil {
			t.Fatalf("main image index 1 must resolve: %v", err)
		}
	})
	t.Run("index 2 cannot resolve in the attachment gallery", func(t *testing.T) {
		err := CheckImageFacets([]interface{}{facet(0, 3, imageFeature(float64(2)))}, embed, tableContentByteLen)
		if !errors.Is(err, ErrInvalidImageFacet) {
			t.Fatalf("main image index 2 must return ErrInvalidImageFacet despite three attachment images; got %v", err)
		}
	})
}
