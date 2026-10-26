package richtext

import (
	"errors"
	"fmt"
	"maps"
)

// ErrInvalidImageFacet reports an #image facet that breaks a write rule.
var ErrInvalidImageFacet = errors.New("invalid image facet")

const (
	featureTypeImage = "social.coves.richtext.facet#image"
	featureTypeCode  = "social.coves.richtext.facet#code"

	embedTypeImages               = "social.coves.embed.images"
	embedTypeImagesWithAttachment = "social.coves.embed.imagesWithAttachment"
)

// CheckImageFacets checks every #image facet against the record's embed and
// the record's other facets, returning an error matching ErrInvalidImageFacet
// for the first facet that breaks a rule. contentByteLen is the UTF-8 byte
// length of the text the facets annotate; an #image range must end within it.
// Facets without an #image feature are not inspected beyond reading #code and
// #codeBlock ranges; their structure, including their own bounds, is
// ValidateFacets' job. With no #image facet the result is nil whatever the
// embed is.
//
// These rules are deliberately NOT part of checkFacet: SanitizeFacets shares
// that check at firehose ingest and must keep #image facets whose byte ranges
// are valid, whatever their placement.
func CheckImageFacets(facets []interface{}, embed map[string]interface{}, contentByteLen int) error {
	rules := newImageFacetRules(facets, embed)
	rules.contentByteLen = contentByteLen
	for i, entry := range facets {
		facet, features, ok := imageFacet(entry)
		if !ok {
			continue
		}
		if err := rules.check(facet, features); err != nil {
			return fmt.Errorf("facet %d: %w: %s", i, ErrInvalidImageFacet, err)
		}
	}
	return nil
}

// FilterImageFacets removes #image placements that break the write rules,
// using only the main gallery's served image count. Other features and covered
// text are preserved, and the input facets are never modified.
func FilterImageFacets(facets []interface{}, imageCount, contentByteLen int) []interface{} {
	if len(facets) == 0 {
		return facets
	}
	rules := newImageFacetRules(facets, nil)
	rules.imageCount = imageCount
	rules.hasImages = imageCount > 0
	rules.contentByteLen = contentByteLen
	var filtered []interface{}
	for _, entry := range facets {
		facet, features, image := imageFacet(entry)
		if !image || rules.check(facet, features) == nil {
			filtered = append(filtered, entry)
			continue
		}
		var remaining []interface{}
		for _, feature := range features {
			if !hasFeature([]interface{}{feature}, featureTypeImage) {
				remaining = append(remaining, feature)
			}
		}
		if len(remaining) > 0 {
			copy := maps.Clone(facet)
			copy["features"] = remaining
			filtered = append(filtered, copy)
		}
	}
	return filtered
}

// imageFacetRules holds what the #image rules need from the whole record: the
// embed's image count, the content length, the #code and #codeBlock ranges,
// and the indices already placed.
type imageFacetRules struct {
	contentByteLen int
	imageCount     int  // number of images in the main gallery
	hasImages      bool // the main gallery is an images embed with an images array
	codeRanges     []byteRange
	placed         map[int]bool
}

type byteRange struct{ start, end int }

func newImageFacetRules(facets []interface{}, embed map[string]interface{}) *imageFacetRules {
	rules := &imageFacetRules{placed: make(map[int]bool)}
	if embedType, _ := embed["$type"].(string); embedType == embedTypeImagesWithAttachment {
		embed, _ = embed["images"].(map[string]interface{})
	}
	if embedType, _ := embed["$type"].(string); embedType == embedTypeImages {
		if images, ok := embed["images"].([]interface{}); ok {
			rules.hasImages = true
			rules.imageCount = len(images)
		}
	}
	for _, entry := range facets {
		facet, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		features, _ := facet["features"].([]interface{})
		if !hasFeature(features, featureTypeCode) && !hasFeature(features, featureTypeCodeBlock) {
			continue
		}
		if r, err := facetRange(facet); err == nil {
			rules.codeRanges = append(rules.codeRanges, r)
		}
	}
	return rules
}

// check applies every #image rule to one facet carrying an #image feature and,
// when it passes, records its index as placed.
func (r *imageFacetRules) check(facet map[string]interface{}, features []interface{}) error {
	if len(features) != 1 {
		return errors.New("#image must be the only feature in its facet")
	}
	span, err := facetRange(facet)
	if err != nil {
		return err
	}
	if span.end > r.contentByteLen {
		return fmt.Errorf("range [%d, %d) runs past the content (%d bytes)", span.start, span.end, r.contentByteLen)
	}
	for _, code := range r.codeRanges {
		if code.start <= span.start && span.end <= code.end {
			return errors.New("range lies inside a #code or #codeBlock range")
		}
	}

	feature, _ := features[0].(map[string]interface{})
	index, err := jsonInt(feature["index"])
	if err != nil {
		return fmt.Errorf("index %w", err)
	}
	if !r.hasImages {
		return fmt.Errorf("index %d needs a %s embed", index, embedTypeImages)
	}
	if index < 0 || index >= r.imageCount {
		return fmt.Errorf("index %d does not name an image of the embed (%d images)", index, r.imageCount)
	}
	if r.placed[index] {
		return fmt.Errorf("index %d is placed more than once", index)
	}
	r.placed[index] = true
	return nil
}

// imageFacet returns the facet and its features when entry is a facet object
// carrying an #image feature.
func imageFacet(entry interface{}) (map[string]interface{}, []interface{}, bool) {
	facet, ok := entry.(map[string]interface{})
	if !ok {
		return nil, nil, false
	}
	features, _ := facet["features"].([]interface{})
	if !hasFeature(features, featureTypeImage) {
		return nil, nil, false
	}
	return facet, features, true
}

func hasFeature(features []interface{}, featureType string) bool {
	for _, raw := range features {
		feature, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := feature["$type"].(string); t == featureType {
			return true
		}
	}
	return false
}

// facetRange reads a facet's byte range, requiring a well-formed, non-empty
// range. It does not check the range against the content length; check does
// that for #image facets only.
func facetRange(facet map[string]interface{}) (byteRange, error) {
	index, ok := facet["index"].(map[string]interface{})
	if !ok {
		return byteRange{}, errors.New("missing index object")
	}
	start, err := jsonInt(index["byteStart"])
	if err != nil {
		return byteRange{}, fmt.Errorf("index.byteStart %w", err)
	}
	end, err := jsonInt(index["byteEnd"])
	if err != nil {
		return byteRange{}, fmt.Errorf("index.byteEnd %w", err)
	}
	if start < 0 || end <= start {
		return byteRange{}, fmt.Errorf("range [%d, %d) is empty or negative", start, end)
	}
	return byteRange{start: start, end: end}, nil
}
