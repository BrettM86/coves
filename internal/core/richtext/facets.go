// Package richtext provides structural validation for social.coves.richtext.facet
// annotations. Facets are byte-range annotations over canonical plaintext; this
// package checks the parts a schema validator cannot: that ranges are sane and lie
// within the content they annotate.
//
// Feature $types are deliberately NOT checked against a whitelist. The facet
// feature union is open — clients may carry features this AppView doesn't know
// yet, and unknown features degrade gracefully to plaintext at render time.
// Rejecting them here would break forward compatibility.
package richtext

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"Coves/internal/validation"
)

// MaxFacets is the maximum number of facets accepted on a single record.
// Mirrors the maxLength constraint on the facets array in the
// social.coves.community.post and social.coves.community.comment lexicons.
const MaxFacets = 200

// MaxFeaturesPerFacet bounds the features array of a single facet. The lexicon
// leaves it open, but with only ten defined feature types a larger array is
// either redundant or padding — and firehose records from hostile repos would
// otherwise have an unbounded dimension that MaxFacets cannot cap.
const MaxFeaturesPerFacet = 20

// Known feature $types from the social.coves.richtext.facet lexicon. All but
// featureTypeLink carry attribute constraints enforced by checkKnownFeature;
// featureTypeLink is used only by NormalizeLinkURIs, which owns its uri rules.
// Only KNOWN types are checked — unknown $types pass untouched, keeping the
// union open for forward compatibility.
const (
	featureTypeBlockquote = "social.coves.richtext.facet#blockquote"
	featureTypeHeading    = "social.coves.richtext.facet#heading"
	featureTypeCodeBlock  = "social.coves.richtext.facet#codeBlock"
	featureTypeSpoiler    = "social.coves.richtext.facet#spoiler"
	featureTypeLink       = "social.coves.richtext.facet#link"
)

const (
	maxBlockLevel         = 6   // blockquote/heading level upper bound (lexicon: maximum 6)
	maxCodeBlockLanguage  = 40  // codeBlock.language byte cap (lexicon: maxLength 40)
	maxSpoilerReasonBytes = 128 // spoiler.reason byte cap (lexicon: maxLength 128)
)

// ValidateFacets checks facets against the byte length of the content they
// annotate, returning a descriptive error for the first invalid facet.
// Used at the API write path, where malformed input should be rejected
// before a broken record is persisted to the PDS.
//
// contentByteLen is the length in UTF-8 bytes of the canonical content string
// (pass 0 when the record has no content — any facet is then invalid).
func ValidateFacets(facets []interface{}, contentByteLen int) error {
	if len(facets) == 0 {
		return nil
	}
	if len(facets) > MaxFacets {
		return fmt.Errorf("too many facets: %d (max %d)", len(facets), MaxFacets)
	}
	for i, entry := range facets {
		if err := checkFacet(entry, contentByteLen); err != nil {
			return fmt.Errorf("facet %d: %w", i, err)
		}
	}
	return nil
}

// SanitizeFacets drops structurally invalid facets and truncates to MaxFacets,
// returning the surviving facets and the number dropped. Used at firehose
// ingest, where records from federated repos cannot be rejected back to their
// author — a bad facet must not poison the rest of the record's annotations,
// and clients must never receive ranges that slice outside the content.
// Returns nil when no facets survive so callers can keep their existing
// nil-means-absent handling.
func SanitizeFacets(facets []interface{}, contentByteLen int) (kept []interface{}, dropped int) {
	if len(facets) == 0 {
		return nil, 0
	}
	for _, entry := range facets {
		if checkFacet(entry, contentByteLen) == nil {
			kept = append(kept, entry)
		} else {
			dropped++
		}
	}
	if len(kept) > MaxFacets {
		dropped += len(kept) - MaxFacets
		kept = kept[:MaxFacets]
	}
	return kept, dropped
}

// NormalizeLinkURIs rewrites the uri of every #link feature in place into a form
// that satisfies the `format: uri` the facet lexicon declares for it, returning
// an error for the first uri that carries no recoverable URI at all.
//
// This is deliberately paired with ValidateFacets (the API write path) and NOT
// with SanitizeFacets (firehose ingest). On the write path the AppView still
// controls the bytes and can repair them before signing the record. At ingest
// the record is already signed by someone else, and an unencoded character in a
// link target is a schema violation rather than a rendering hazard — the byte
// ranges are what could slice content, and those are checked separately. Making
// the firehose drop these facets would strip links out of already-federated
// records on reindex, trading a validation nit for visible content loss.
//
// Structurally malformed facets and features are skipped rather than reported:
// callers run ValidateFacets first, which is what reports those. The one
// structural rule this function owns is a #link feature carrying no uri —
// checkKnownFeature has no #link arm, so nothing else rejects it.
func NormalizeLinkURIs(facets []interface{}) error {
	for i, entry := range facets {
		facet, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		features, ok := facet["features"].([]interface{})
		if !ok {
			continue
		}
		for j, featureRaw := range features {
			feature, ok := featureRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if featureType, _ := feature["$type"].(string); featureType != featureTypeLink {
				continue
			}
			raw, ok := feature["uri"].(string)
			if !ok || raw == "" {
				return fmt.Errorf("facet %d: features[%d] (%s): missing required 'uri' string",
					i, j, featureTypeLink)
			}
			normalized, err := validation.NormalizeURI(raw)
			if err != nil {
				return fmt.Errorf("facet %d: features[%d] (%s): %w", i, j, featureTypeLink, err)
			}
			feature["uri"] = normalized
		}
	}
	return nil
}

// checkFacet validates the structure of a single facet entry: an object with a
// byte-range index that lies within the content, and a non-empty features array
// whose members carry a $type discriminator.
func checkFacet(entry interface{}, contentByteLen int) error {
	facet, ok := entry.(map[string]interface{})
	if !ok {
		return errors.New("must be an object")
	}

	indexRaw, ok := facet["index"]
	if !ok {
		return errors.New("missing required field 'index'")
	}
	index, ok := indexRaw.(map[string]interface{})
	if !ok {
		return errors.New("'index' must be an object")
	}

	byteStart, err := jsonInt(index["byteStart"])
	if err != nil {
		return fmt.Errorf("index.byteStart %w", err)
	}
	byteEnd, err := jsonInt(index["byteEnd"])
	if err != nil {
		return fmt.Errorf("index.byteEnd %w", err)
	}
	if byteStart < 0 {
		return fmt.Errorf("index.byteStart must not be negative (got %d)", byteStart)
	}
	if byteEnd <= byteStart {
		return fmt.Errorf("index.byteEnd (%d) must be greater than index.byteStart (%d)", byteEnd, byteStart)
	}
	if byteEnd > contentByteLen {
		return fmt.Errorf("index.byteEnd (%d) exceeds content length (%d bytes)", byteEnd, contentByteLen)
	}

	featuresRaw, ok := facet["features"]
	if !ok {
		return errors.New("missing required field 'features'")
	}
	features, ok := featuresRaw.([]interface{})
	if !ok {
		return errors.New("'features' must be an array")
	}
	if len(features) == 0 {
		return errors.New("'features' must not be empty")
	}
	if len(features) > MaxFeaturesPerFacet {
		return fmt.Errorf("too many features: %d (max %d)", len(features), MaxFeaturesPerFacet)
	}
	for j, featureRaw := range features {
		feature, ok := featureRaw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("features[%d] must be an object", j)
		}
		featureType, ok := feature["$type"].(string)
		if !ok || featureType == "" {
			return fmt.Errorf("features[%d] missing required '$type' string", j)
		}
		if err := checkKnownFeature(featureType, feature); err != nil {
			return fmt.Errorf("features[%d] (%s): %w", j, featureType, err)
		}
	}
	return nil
}

// checkKnownFeature enforces the attribute constraints the lexicon declares for
// feature $types this AppView knows. The PDS cannot validate custom lexicons,
// so without this check the API would persist schema-violating records (e.g. a
// heading with no level) and the firehose sanitizer would index them. Unknown
// $types return nil — the union stays open for forward compatibility, and
// attributes of unknown features are deliberately not inspected.
func checkKnownFeature(featureType string, feature map[string]interface{}) error {
	switch featureType {
	case featureTypeHeading:
		level, err := jsonInt(feature["level"])
		if err != nil {
			return fmt.Errorf("level %w", err)
		}
		if level < 1 || level > maxBlockLevel {
			return fmt.Errorf("level must be between 1 and %d (got %d)", maxBlockLevel, level)
		}
	case featureTypeBlockquote:
		if raw, present := feature["level"]; present {
			level, err := jsonInt(raw)
			if err != nil {
				return fmt.Errorf("level %w", err)
			}
			if level < 1 || level > maxBlockLevel {
				return fmt.Errorf("level must be between 1 and %d (got %d)", maxBlockLevel, level)
			}
		}
	case featureTypeCodeBlock:
		if raw, present := feature["language"]; present {
			language, ok := raw.(string)
			if !ok {
				return errors.New("language must be a string")
			}
			if len(language) > maxCodeBlockLanguage {
				return fmt.Errorf("language too long: %d bytes (max %d)", len(language), maxCodeBlockLanguage)
			}
		}
	case featureTypeSpoiler:
		if raw, present := feature["reason"]; present {
			reason, ok := raw.(string)
			if !ok {
				return errors.New("reason must be a string")
			}
			if len(reason) > maxSpoilerReasonBytes {
				return fmt.Errorf("reason too long: %d bytes (max %d)", len(reason), maxSpoilerReasonBytes)
			}
		}
	}
	return nil
}

// jsonInt extracts an integer from a decoded JSON value. encoding/json decodes
// numbers to float64 by default; int variants and json.Number are accepted for
// callers that construct facet maps directly. All arms share the same int32
// bounds so no representation can smuggle in a value another would reject
// (content lengths are capped far below int32, so the bound costs nothing).
func jsonInt(v interface{}) (int, error) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) || n < math.MinInt32 || n > math.MaxInt32 {
			return 0, fmt.Errorf("must be an integer (got %v)", n)
		}
		return int(n), nil
	case int:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, fmt.Errorf("must be an integer (got %v)", n)
		}
		return n, nil
	case int64:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, fmt.Errorf("must be an integer (got %v)", n)
		}
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < math.MinInt32 || i > math.MaxInt32 {
			return 0, fmt.Errorf("must be an integer (got %v)", n)
		}
		return int(i), nil
	case nil:
		return 0, errors.New("is required")
	default:
		return 0, fmt.Errorf("must be an integer (got %T)", v)
	}
}
