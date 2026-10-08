package richtext

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMentionedDIDs(t *testing.T) {
	const (
		mentionedB  = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
		mentionedD  = "did:plc:dddddddddddddddddddddddd"
		mentionType = "social.coves.richtext.facet#mention"
	)
	mention := func(did interface{}) map[string]interface{} {
		return map[string]interface{}{"$type": mentionType, "did": did}
	}
	facetWithFeatures := func(features ...interface{}) map[string]interface{} {
		return map[string]interface{}{
			"index":    map[string]interface{}{"byteStart": 0, "byteEnd": 1},
			"features": features,
		}
	}
	storedJSON := func(facets interface{}) string {
		t.Helper()
		data, err := json.Marshal(facets)
		require.NoError(t, err, "encode the stored facet fixture")
		return string(data)
	}

	tests := []struct {
		name       string
		facetsJSON string
		want       []string
	}{
		{
			name: "duplicate B across facets keeps first occurrence before D",
			facetsJSON: storedJSON([]interface{}{
				facetWithFeatures(mention(mentionedB)),
				facetWithFeatures(mention(mentionedD)),
				facetWithFeatures(mention(mentionedB)),
			}),
			want: []string{mentionedB, mentionedD},
		},
		{
			name:       "two mentions in one facet retain feature order",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(mention(mentionedB), mention(mentionedD))}),
			want:       []string{mentionedB, mentionedD},
		},
		{
			name: "link tag and unknown feature types do not contribute DIDs",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(
				map[string]interface{}{"$type": "social.coves.richtext.facet#link", "did": mentionedB},
				map[string]interface{}{"$type": "social.coves.richtext.facet#tag", "did": mentionedB},
				map[string]interface{}{"$type": "social.coves.richtext.facet#future", "did": mentionedB},
				mention(mentionedD),
			)}),
			want: []string{mentionedD},
		},
		{
			name:       "non-object facet is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{"not a facet", facetWithFeatures(mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "missing features is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{map[string]interface{}{"index": map[string]interface{}{"byteStart": 0, "byteEnd": 1}}, facetWithFeatures(mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "non-array features is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{map[string]interface{}{"features": mention(mentionedB)}, facetWithFeatures(mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "non-object feature is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures("not a feature", mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "mention missing did is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(map[string]interface{}{"$type": mentionType}, mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "numeric did is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(mention(42), mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "null did is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(mention(nil), mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "object did is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(mention(map[string]interface{}{"did": mentionedB}), mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "non-DID string is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(mention("not-a-did"), mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "empty DID is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(mention("did:"), mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "DID with no identifier is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(mention("did:plc:"), mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "missing feature type is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(map[string]interface{}{"did": mentionedB}, mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{
			name:       "non-string feature type is skipped beside a valid mention",
			facetsJSON: storedJSON([]interface{}{facetWithFeatures(map[string]interface{}{"$type": 42, "did": mentionedB}, mention(mentionedD))}),
			want:       []string{mentionedD},
		},
		{name: "empty string has no mentions", facetsJSON: ""},
		{name: "null has no mentions", facetsJSON: "null"},
		{name: "empty array has no mentions", facetsJSON: "[]"},
		{name: "invalid JSON has no mentions", facetsJSON: `[{`},
		{name: "object instead of array has no mentions", facetsJSON: storedJSON(facetWithFeatures(mention(mentionedB)))},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, dropped := MentionedDIDs(test.facetsJSON)
			require.Zero(t, dropped, "no case under the cap drops a mention")
			if len(test.want) == 0 {
				require.Empty(t, got)
			} else {
				require.Equal(t, test.want, got)
			}
		})
	}
}

// Stored facets are capped at MaxFacets, but each may carry MaxFeaturesPerFacet
// mentions over overlapping ranges. MentionedDIDs keeps the first MaxFacets
// distinct DIDs and counts only further distinct DIDs as dropped.
func TestMentionedDIDs_CapsDistinctDIDsAtMaxFacets(t *testing.T) {
	const distinctCount = 15 * MaxFeaturesPerFacet
	require.Greater(t, distinctCount, MaxFacets, "fixture: more distinct mentions than the cap")
	distinct := make([]string, 0, distinctCount)
	for index := 0; index < distinctCount; index++ {
		distinct = append(distinct, fmt.Sprintf("did:plc:cappedmention%03d", index))
	}
	// Every DID appears once in facet order; the last facets then repeat DIDs
	// from both sides of the cap, which must count neither as kept nor dropped.
	ordered := append(append([]string{}, distinct...), distinct[0], distinct[MaxFacets-1], distinct[MaxFacets], distinct[distinctCount-1])
	var facets []interface{}
	for start := 0; start < len(ordered); start += MaxFeaturesPerFacet {
		end := min(start+MaxFeaturesPerFacet, len(ordered))
		features := make([]interface{}, 0, end-start)
		for _, did := range ordered[start:end] {
			features = append(features, map[string]interface{}{"$type": "social.coves.richtext.facet#mention", "did": did})
		}
		facets = append(facets, map[string]interface{}{
			"index":    map[string]interface{}{"byteStart": 0, "byteEnd": 1},
			"features": features,
		})
	}
	data, err := json.Marshal(facets)
	require.NoError(t, err, "encode the stored facet fixture")

	got, dropped := MentionedDIDs(string(data))
	require.Equal(t, distinct[:MaxFacets], got, "the first MaxFacets distinct DIDs, in facet and feature order")
	require.Equal(t, distinctCount-MaxFacets, dropped, "only distinct DIDs past the cap count as dropped")
}
