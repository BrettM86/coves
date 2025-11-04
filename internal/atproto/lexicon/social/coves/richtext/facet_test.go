package richtext

import (
	"encoding/json"
	"testing"
)

// TestFacetStructure tests the basic structure of facets
func TestFacetStructure(t *testing.T) {
	tests := []struct {
		name    string
		facet   string
		wantErr bool
	}{
		{
			name: "valid mention facet",
			facet: `{
				"index": {
					"byteStart": 5,
					"byteEnd": 18
				},
				"features": [{
					"$type": "social.coves.richtext.facet#mention",
					"did": "did:plc:example123"
				}]
			}`,
			wantErr: false,
		},
		{
			name: "valid link facet",
			facet: `{
				"index": {
					"byteStart": 10,
					"byteEnd": 35
				},
				"features": [{
					"$type": "social.coves.richtext.facet#link",
					"uri": "https://example.com"
				}]
			}`,
			wantErr: false,
		},
		{
			name: "valid formatting facet",
			facet: `{
				"index": {
					"byteStart": 0,
					"byteEnd": 5
				},
				"features": [{
					"$type": "social.coves.richtext.facet#bold"
				}]
			}`,
			wantErr: false,
		},
		{
			name: "multiple features on same range",
			facet: `{
				"index": {
					"byteStart": 0,
					"byteEnd": 10
				},
				"features": [
					{"$type": "social.coves.richtext.facet#bold"},
					{"$type": "social.coves.richtext.facet#italic"}
				]
			}`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var facet map[string]interface{}
			err := json.Unmarshal([]byte(tt.facet), &facet)
			if err != nil {
				if !tt.wantErr {
					t.Errorf("json.Unmarshal() unexpected error = %v", err)
				}
				return
			}

			// Basic validation
			if _, hasIndex := facet["index"]; !hasIndex && !tt.wantErr {
				t.Error("facet missing required 'index' field")
			}
			if _, hasFeatures := facet["features"]; !hasFeatures && !tt.wantErr {
				t.Error("facet missing required 'features' field")
			}
		})
	}
}

// TestUTF8ByteCounting tests proper UTF-8 byte counting for facets
func TestUTF8ByteCounting(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		substring string
		wantStart int
		wantEnd   int
	}{
		{
			name:      "ASCII text",
			text:      "Hello @alice!",
			substring: "@alice",
			wantStart: 6,
			wantEnd:   12,
		},
		{
			name:      "Emoji in text",
			text:      "Hi 👋 @alice!",
			substring: "@alice",
			wantStart: 8,  // "Hi " (3) + "👋" (4) + " " (1) = 8
			wantEnd:   14, // 8 + 6 = 14
		},
		{
			name:      "Complex emoji (family)",
			text:      "Family: 👨‍👩‍👧‍👧 @alice",
			substring: "@alice",
			wantStart: 34, // "Family: " (8) + complex emoji (25) + " " (1) = 34
			wantEnd:   40, // 34 + 6 = 40
		},
		{
			name:      "Multibyte characters",
			text:      "Привет @alice!",
			substring: "@alice",
			wantStart: 13, // Cyrillic "Привет " = 12 bytes + 1 space = 13
			wantEnd:   19, // 13 + 6 = 19
		},
		{
			name:      "Mixed content",
			text:      "Test 测试 @alice done",
			substring: "@alice",
			wantStart: 12, // "Test " (5) + "测试" (6) + " " (1) = 12
			wantEnd:   18, // 12 + 6 = 18
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Find byte positions using strings.Index (which works on bytes)
			idx := -1
			for i := 0; i < len(tt.text); i++ {
				if i+len(tt.substring) <= len(tt.text) && tt.text[i:i+len(tt.substring)] == tt.substring {
					idx = i
					break
				}
			}

			if idx == -1 {
				t.Fatalf("substring %q not found in text %q", tt.substring, tt.text)
			}

			// Calculate byte positions
			startByte := len([]byte(tt.text[:idx]))
			endByte := startByte + len([]byte(tt.substring))

			if startByte != tt.wantStart {
				t.Errorf("ByteStart = %d, want %d", startByte, tt.wantStart)
			}
			if endByte != tt.wantEnd {
				t.Errorf("ByteEnd = %d, want %d", endByte, tt.wantEnd)
			}
		})
	}
}

// TestOverlappingFacets tests validation of overlapping facet ranges
func TestOverlappingFacets(t *testing.T) {
	tests := []struct {
		name        string
		description string
		facets      []map[string]interface{}
		expectError bool
	}{
		{
			name: "non-overlapping facets",
			facets: []map[string]interface{}{
				{
					"index": map[string]int{
						"byteStart": 0,
						"byteEnd":   5,
					},
				},
				{
					"index": map[string]int{
						"byteStart": 10,
						"byteEnd":   15,
					},
				},
			},
			expectError: false,
			description: "Facets with non-overlapping ranges should be valid",
		},
		{
			name: "exact same range",
			facets: []map[string]interface{}{
				{
					"index": map[string]int{
						"byteStart": 5,
						"byteEnd":   10,
					},
				},
				{
					"index": map[string]int{
						"byteStart": 5,
						"byteEnd":   10,
					},
				},
			},
			expectError: false,
			description: "Multiple facets on the same range are allowed (e.g., bold + italic)",
		},
		{
			name: "nested ranges",
			facets: []map[string]interface{}{
				{
					"index": map[string]int{
						"byteStart": 0,
						"byteEnd":   20,
					},
				},
				{
					"index": map[string]int{
						"byteStart": 5,
						"byteEnd":   15,
					},
				},
			},
			expectError: false,
			description: "Nested facet ranges are allowed",
		},
		{
			name: "partial overlap",
			facets: []map[string]interface{}{
				{
					"index": map[string]int{
						"byteStart": 0,
						"byteEnd":   10,
					},
				},
				{
					"index": map[string]int{
						"byteStart": 5,
						"byteEnd":   15,
					},
				},
			},
			expectError: false,
			description: "Partially overlapping facets are allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// For now, we're not implementing overlap validation
			// as it's allowed in AT Protocol
			// This test documents the expected behavior
			if tt.expectError {
				t.Skip("Overlap validation not implemented - all overlaps are currently allowed")
			}
		})
	}
}

// TestFacetFeatureTypes tests all supported facet feature types
func TestFacetFeatureTypes(t *testing.T) {
	featureTypes := []struct {
		feature  map[string]interface{}
		name     string
		typeName string
	}{
		{
			name:     "mention",
			typeName: "social.coves.richtext.facet#mention",
			feature: map[string]interface{}{
				"$type": "social.coves.richtext.facet#mention",
				"did":   "did:plc:example123",
			},
		},
		{
			name:     "link",
			typeName: "social.coves.richtext.facet#link",
			feature: map[string]interface{}{
				"$type": "social.coves.richtext.facet#link",
				"uri":   "https://example.com",
			},
		},
		{
			name:     "bold",
			typeName: "social.coves.richtext.facet#bold",
			feature: map[string]interface{}{
				"$type": "social.coves.richtext.facet#bold",
			},
		},
		{
			name:     "italic",
			typeName: "social.coves.richtext.facet#italic",
			feature: map[string]interface{}{
				"$type": "social.coves.richtext.facet#italic",
			},
		},
		{
			name:     "strikethrough",
			typeName: "social.coves.richtext.facet#strikethrough",
			feature: map[string]interface{}{
				"$type": "social.coves.richtext.facet#strikethrough",
			},
		},
		{
			name:     "spoiler",
			typeName: "social.coves.richtext.facet#spoiler",
			feature: map[string]interface{}{
				"$type":  "social.coves.richtext.facet#spoiler",
				"reason": "Plot spoiler",
			},
		},
	}

	for _, ft := range featureTypes {
		t.Run(ft.name, func(t *testing.T) {
			// Verify the $type field is present and correct
			if typeVal, ok := ft.feature["$type"].(string); !ok || typeVal != ft.typeName {
				t.Errorf("Feature type mismatch: got %v, want %s", ft.feature["$type"], ft.typeName)
			}

			// Create a complete facet with this feature
			facet := map[string]interface{}{
				"index": map[string]interface{}{
					"byteStart": 0,
					"byteEnd":   10,
				},
				"features": []interface{}{ft.feature},
			}

			// Verify it can be marshaled/unmarshaled
			data, err := json.Marshal(facet)
			if err != nil {
				t.Errorf("Failed to marshal facet: %v", err)
			}

			var decoded map[string]interface{}
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Errorf("Failed to unmarshal facet: %v", err)
			}
		})
	}
}
