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
		{
			name:     "blockquote",
			typeName: "social.coves.richtext.facet#blockquote",
			feature: map[string]interface{}{
				"$type": "social.coves.richtext.facet#blockquote",
				"level": 1,
			},
		},
		{
			name:     "heading",
			typeName: "social.coves.richtext.facet#heading",
			feature: map[string]interface{}{
				"$type": "social.coves.richtext.facet#heading",
				"level": 2,
			},
		},
		{
			name:     "code",
			typeName: "social.coves.richtext.facet#code",
			feature: map[string]interface{}{
				"$type": "social.coves.richtext.facet#code",
			},
		},
		{
			name:     "codeBlock",
			typeName: "social.coves.richtext.facet#codeBlock",
			feature: map[string]interface{}{
				"$type":    "social.coves.richtext.facet#codeBlock",
				"language": "go",
			},
		},
	}

	for _, ft := range featureTypes {
		t.Run(ft.name, func(t *testing.T) {
			// Verify the $type field is present and correct
			if typeVal, ok := ft.feature["$type"].(string); !ok || typeVal != ft.typeName {
				t.Errorf("Feature type mismatch: got %v, want %s", ft.feature["$type"], ft.typeName)
			}

			// Verify the level attribute survives a marshal round-trip
			// (blockquote/heading)
			if level, hasLevel := ft.feature["level"]; hasLevel {
				data, err := json.Marshal(ft.feature)
				if err != nil {
					t.Fatalf("Failed to marshal feature: %v", err)
				}
				var decoded map[string]interface{}
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatalf("Failed to unmarshal feature: %v", err)
				}
				if decodedLevel, ok := decoded["level"].(float64); !ok || int(decodedLevel) != level.(int) {
					t.Errorf("level attribute lost in round-trip: got %v, want %v", decoded["level"], level)
				}
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

// TestBlockFacetConventions documents the byte-range conventions for block-level
// features (blockquote, heading, codeBlock) on realistic bridged content:
//   - Block ranges span whole lines, excluding the trailing newline
//   - Nested quotes are disjoint ranges with increasing level, NOT nested ranges
//   - Source markers ('>', '#', code fences) are stripped by the writer;
//     the plaintext must stay readable when every facet is ignored
func TestBlockFacetConventions(t *testing.T) {
	// A bridged Lemmy post: heading, a two-level nested quote, and a code block.
	// Markdown source would have been:
	//   ## The Button\n> They said\n> > Do not press\nUse:\n```go\nfmt.Println("hi")\n```
	content := "The Button\nThey said\nDo not press\nUse:\nfmt.Println(\"hi\")"

	lines := map[string][2]int{
		"The Button":          {0, 10},  // heading, level 2
		"They said":           {11, 20}, // blockquote, level 1
		"Do not press":        {21, 33}, // blockquote, level 2 (disjoint range, deeper level)
		"fmt.Println(\"hi\")": {39, 56}, // codeBlock
	}
	// Verify the documented offsets actually slice the content correctly
	for text, span := range lines {
		if got := content[span[0]:span[1]]; got != text {
			t.Fatalf("documented span [%d:%d] slices to %q, want %q", span[0], span[1], got, text)
		}
	}

	facets := []map[string]interface{}{
		{
			"index": map[string]interface{}{"byteStart": 0, "byteEnd": 10},
			"features": []interface{}{
				map[string]interface{}{"$type": "social.coves.richtext.facet#heading", "level": 2},
			},
		},
		{
			"index": map[string]interface{}{"byteStart": 11, "byteEnd": 20},
			"features": []interface{}{
				map[string]interface{}{"$type": "social.coves.richtext.facet#blockquote", "level": 1},
			},
		},
		{
			"index": map[string]interface{}{"byteStart": 21, "byteEnd": 33},
			"features": []interface{}{
				map[string]interface{}{"$type": "social.coves.richtext.facet#blockquote", "level": 2},
			},
		},
		{
			"index": map[string]interface{}{"byteStart": 39, "byteEnd": 56},
			"features": []interface{}{
				map[string]interface{}{"$type": "social.coves.richtext.facet#codeBlock", "language": "go"},
			},
		},
	}

	for i, f := range facets {
		index := f["index"].(map[string]interface{})
		start, end := index["byteStart"].(int), index["byteEnd"].(int)

		// Block ranges never start or end mid-line: the byte before the range
		// (if any) and the byte after (if any) must be newlines.
		if start > 0 && content[start-1] != '\n' {
			t.Errorf("facet %d starts mid-line at byte %d", i, start)
		}
		if end < len(content) && content[end] != '\n' {
			t.Errorf("facet %d ends mid-line at byte %d", i, end)
		}
		// Ranges exclude the trailing newline
		if content[end-1] == '\n' {
			t.Errorf("facet %d range includes its trailing newline", i)
		}
	}

	// Nested quotes: the level-1 and level-2 ranges are disjoint, not nested
	q1End := facets[1]["index"].(map[string]interface{})["byteEnd"].(int)
	q2Start := facets[2]["index"].(map[string]interface{})["byteStart"].(int)
	if q2Start < q1End {
		t.Error("nested quote ranges must be disjoint (level on separate ranges), not containment")
	}
}
