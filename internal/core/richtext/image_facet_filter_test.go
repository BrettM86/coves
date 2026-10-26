package richtext

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestFilterImageFacetsDropsUnresolvedIndices(t *testing.T) {
	tests := []struct {
		name       string
		index      interface{}
		imageCount int
	}{
		{"at image count", float64(3), 3},
		{"beyond image count", float64(5), 3},
		{"no served images", float64(0), 0},
		{"negative index", float64(-1), 3},
		{"fractional index", 1.5, 3},
		{"string index", "0", 3},
		{"missing index", nil, 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			feature := imageFeature(test.index)
			if test.index == nil {
				delete(feature, "index")
			}
			facets := []interface{}{
				facet(0, 3, feature),
				facet(4, 8, map[string]interface{}{"$type": "social.coves.richtext.facet#link", "uri": "https://example.com/kept"}),
			}
			want := []interface{}{
				facet(4, 8, map[string]interface{}{"$type": "social.coves.richtext.facet#link", "uri": "https://example.com/kept"}),
			}
			if got := FilterImageFacets(facets, test.imageCount, 20); !reflect.DeepEqual(got, want) {
				t.Fatalf("unresolved placement must be removed while link stays: got %#v, want %#v", got, want)
			}
		})
	}
}

func TestFilterImageFacetsKeepsFirstValidDuplicate(t *testing.T) {
	tests := []struct {
		name   string
		facets []interface{}
		want   []interface{}
	}{
		{
			name: "first in array order wins",
			facets: []interface{}{
				facet(10, 13, imageFeature(float64(1))),
				facet(0, 3, imageFeature(float64(1))),
				facet(15, 18, imageFeature(float64(0))),
			},
			want: []interface{}{
				facet(10, 13, imageFeature(float64(1))),
				facet(15, 18, imageFeature(float64(0))),
			},
		},
		{
			name: "invalid earlier range does not reserve index",
			facets: []interface{}{
				facet(3, 3, imageFeature(float64(1))),
				facet(5, 8, imageFeature(float64(1))),
				facet(10, 13, imageFeature(float64(1))),
			},
			want: []interface{}{facet(5, 8, imageFeature(float64(1)))},
		},
		{
			name: "shared earlier feature does not reserve index",
			facets: []interface{}{
				facet(0, 3, imageFeature(float64(1)), map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}),
				facet(5, 8, imageFeature(float64(1))),
			},
			want: []interface{}{
				facet(0, 3, map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}),
				facet(5, 8, imageFeature(float64(1))),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := FilterImageFacets(test.facets, 2, 20); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("only the first valid placement may reserve an index: got %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestFilterImageFacetsPreservesSharedFeaturesAndInput(t *testing.T) {
	facets := []interface{}{
		facet(0, 3, map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}),
		facet(4, 8,
			map[string]interface{}{"$type": "social.coves.richtext.facet#link", "uri": "https://example.com/shared"},
			imageFeature(float64(0)),
			map[string]interface{}{"$type": "social.coves.richtext.facet#mention", "did": "did:plc:mentioned"},
			imageFeature(float64(1))),
		facet(10, 13, imageFeature(float64(0))),
	}
	before, err := json.Marshal(facets)
	if err != nil {
		t.Fatal(err)
	}
	want := []interface{}{
		facet(0, 3, map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}),
		facet(4, 8,
			map[string]interface{}{"$type": "social.coves.richtext.facet#link", "uri": "https://example.com/shared"},
			map[string]interface{}{"$type": "social.coves.richtext.facet#mention", "did": "did:plc:mentioned"}),
		facet(10, 13, imageFeature(float64(0))),
	}
	if got := FilterImageFacets(facets, 2, 20); !reflect.DeepEqual(got, want) {
		t.Fatalf("shared facet must retain other features and order: got %#v, want %#v", got, want)
	}
	after, err := json.Marshal(facets)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("filter mutated input facets: got %s, want %s", after, before)
	}
}

func TestFilterImageFacetsDropsCodeContainedPlacements(t *testing.T) {
	tests := []struct {
		name       string
		codeType   string
		imageStart float64
		imageEnd   float64
		keepImage  bool
	}{
		{"equal code range", "social.coves.richtext.facet#code", 2, 10, false},
		{"inside code block", "social.coves.richtext.facet#codeBlock", 4, 6, false},
		{"partial overlap", "social.coves.richtext.facet#code", 8, 14, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facets := []interface{}{
				facet(test.imageStart, test.imageEnd, imageFeature(float64(0))),
				facet(2, 10, map[string]interface{}{"$type": test.codeType}),
			}
			want := []interface{}{facet(2, 10, map[string]interface{}{"$type": test.codeType})}
			if test.keepImage {
				want = []interface{}{
					facet(8, 14, imageFeature(float64(0))),
					facet(2, 10, map[string]interface{}{"$type": "social.coves.richtext.facet#code"}),
				}
			}
			if got := FilterImageFacets(facets, 1, 20); !reflect.DeepEqual(got, want) {
				t.Fatalf("containment must remove images, overlap must retain them: got %#v, want %#v", got, want)
			}
		})
	}
}

func TestFilterImageFacetsDropsMalformedRanges(t *testing.T) {
	tests := []struct {
		name  string
		index interface{}
	}{
		{"empty", map[string]interface{}{"byteStart": float64(3), "byteEnd": float64(3)}},
		{"reversed", map[string]interface{}{"byteStart": float64(5), "byteEnd": float64(3)}},
		{"negative start", map[string]interface{}{"byteStart": float64(-1), "byteEnd": float64(3)}},
		{"missing range", nil},
		{"nonobject range", "invalid"},
		{"fractional bound", map[string]interface{}{"byteStart": 0.5, "byteEnd": float64(3)}},
		{"missing bound", map[string]interface{}{"byteStart": float64(0)}},
		{"past content", map[string]interface{}{"byteStart": float64(18), "byteEnd": float64(21)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facets := []interface{}{
				map[string]interface{}{"index": test.index, "features": []interface{}{imageFeature(float64(0))}},
				facet(5, 8, imageFeature(float64(1))),
			}
			want := []interface{}{facet(5, 8, imageFeature(float64(1)))}
			if got := FilterImageFacets(facets, 2, 20); !reflect.DeepEqual(got, want) {
				t.Fatalf("invalid image range must be removed: got %#v, want %#v", got, want)
			}
		})
	}
}

func TestFilterImageFacetsPreservesValidAndOtherFacets(t *testing.T) {
	facets := []interface{}{
		facet(0, 3, imageFeature(float64(0))),
		facet(4, 8, map[string]interface{}{"$type": "social.coves.richtext.facet#link", "uri": "https://example.com/link"}),
		facet(9, 12, map[string]interface{}{"$type": "social.coves.richtext.facet#mention", "did": "did:plc:mentioned"}),
		facet(13, 16, map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}),
		facet(17, 20, imageFeature(float64(2))),
		map[string]interface{}{"index": "opaque", "features": []interface{}{map[string]interface{}{"$type": "example.test.facet#future", "value": "kept"}}},
		"opaque non-image facet",
	}
	want := []interface{}{
		facet(0, 3, imageFeature(float64(0))),
		facet(4, 8, map[string]interface{}{"$type": "social.coves.richtext.facet#link", "uri": "https://example.com/link"}),
		facet(9, 12, map[string]interface{}{"$type": "social.coves.richtext.facet#mention", "did": "did:plc:mentioned"}),
		facet(13, 16, map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}),
		facet(17, 20, imageFeature(float64(2))),
		map[string]interface{}{"index": "opaque", "features": []interface{}{map[string]interface{}{"$type": "example.test.facet#future", "value": "kept"}}},
		"opaque non-image facet",
	}
	if got := FilterImageFacets(facets, 3, 20); !reflect.DeepEqual(got, want) {
		t.Fatalf("valid images and all other facets must pass through unchanged: got %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(facets, want) {
		t.Fatalf("filter mutated unchanged input facets: got %#v, want %#v", facets, want)
	}
}
