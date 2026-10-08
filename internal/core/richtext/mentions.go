package richtext

import (
	"encoding/json"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// MentionedDIDs returns the first MaxFacets valid, distinct mention DIDs in
// facet and feature order. dropped counts the further valid, distinct mention
// DIDs that were not returned; a repeat of a DID already seen is never counted.
// Invalid facet JSON or malformed entries are ignored.
//
// The cap is the bound on mentions per record. Stored facets are capped at
// MaxFacets, but each facet may carry MaxFeaturesPerFacet features over
// overlapping ranges, so without it one record could name
// MaxFacets*MaxFeaturesPerFacet distinct DIDs.
func MentionedDIDs(facetsJSON string) (dids []string, dropped int) {
	var facets []any
	if err := json.Unmarshal([]byte(facetsJSON), &facets); err != nil {
		return nil, 0
	}

	seen := make(map[string]struct{})
	for _, entry := range facets {
		facet, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		features, ok := facet["features"].([]any)
		if !ok {
			continue
		}
		for _, entry := range features {
			feature, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			featureType, ok := feature["$type"].(string)
			if !ok || featureType != featureTypeMention {
				continue
			}
			did, ok := feature["did"].(string)
			if !ok {
				continue
			}
			if _, err := syntax.ParseDID(did); err != nil {
				continue
			}
			if _, exists := seen[did]; exists {
				continue
			}
			seen[did] = struct{}{}
			if len(dids) == MaxFacets {
				dropped++
				continue
			}
			dids = append(dids, did)
		}
	}
	return dids, dropped
}
