package richtext

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"Coves/internal/validation"
)

// facet builds a well-formed facet map the way encoding/json would decode it
// (numbers as float64).
func facet(byteStart, byteEnd float64, features ...interface{}) map[string]interface{} {
	if len(features) == 0 {
		features = []interface{}{
			map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
		}
	}
	return map[string]interface{}{
		"index": map[string]interface{}{
			"byteStart": byteStart,
			"byteEnd":   byteEnd,
		},
		"features": features,
	}
}

func TestValidateFacets(t *testing.T) {
	tests := []struct {
		name           string
		facets         []interface{}
		contentByteLen int
		wantError      bool
		// errContains, when set, asserts the message mentions it so the client
		// gets an actionable reason.
		errContains string
	}{
		// --- happy paths ------------------------------------------------
		{
			name:           "nil facets are valid (facets are optional)",
			facets:         nil,
			contentByteLen: 0,
			wantError:      false,
		},
		{
			name:           "empty facets are valid",
			facets:         []interface{}{},
			contentByteLen: 10,
			wantError:      false,
		},
		{
			name:           "valid single facet",
			facets:         []interface{}{facet(0, 5)},
			contentByteLen: 10,
			wantError:      false,
		},
		{
			name: "valid facet ending exactly at content length",
			facets: []interface{}{
				facet(3, 10),
			},
			contentByteLen: 10,
			wantError:      false,
		},
		{
			name: "valid block features with attributes",
			facets: []interface{}{
				facet(0, 20, map[string]interface{}{
					"$type": "social.coves.richtext.facet#heading",
					"level": float64(2),
				}),
				facet(21, 40, map[string]interface{}{
					"$type": "social.coves.richtext.facet#blockquote",
					"level": float64(1),
				}),
				facet(41, 60, map[string]interface{}{
					"$type":    "social.coves.richtext.facet#codeBlock",
					"language": "go",
				}),
			},
			contentByteLen: 60,
			wantError:      false,
		},
		{
			name: "unknown feature $type is allowed (open union, forward compat)",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type": "social.coves.richtext.facet#futureFeature",
					"attr":  "whatever",
				}),
			},
			contentByteLen: 10,
			wantError:      false,
		},
		{
			name: "integer-typed byte offsets are accepted (hand-built maps)",
			facets: []interface{}{
				map[string]interface{}{
					"index": map[string]interface{}{
						"byteStart": 0,
						"byteEnd":   int64(5),
					},
					"features": []interface{}{
						map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
					},
				},
			},
			contentByteLen: 10,
			wantError:      false,
		},
		{
			name: "json.Number byte offsets are accepted (UseNumber decoders)",
			facets: []interface{}{
				map[string]interface{}{
					"index": map[string]interface{}{
						"byteStart": json.Number("0"),
						"byteEnd":   json.Number("5"),
					},
					"features": []interface{}{
						map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
					},
				},
			},
			contentByteLen: 10,
			wantError:      false,
		},

		// --- range failures ---------------------------------------------
		{
			name:           "byteEnd beyond content length",
			facets:         []interface{}{facet(0, 11)},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "exceeds content length",
		},
		{
			name:           "any facet is invalid when there is no content",
			facets:         []interface{}{facet(0, 1)},
			contentByteLen: 0,
			wantError:      true,
			errContains:    "exceeds content length",
		},
		{
			name:           "byteEnd equal to byteStart (empty range)",
			facets:         []interface{}{facet(5, 5)},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "must be greater than",
		},
		{
			name:           "byteEnd before byteStart (inverted range)",
			facets:         []interface{}{facet(8, 3)},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "must be greater than",
		},
		{
			name:           "negative byteStart",
			facets:         []interface{}{facet(-1, 5)},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "must not be negative",
		},
		{
			name:           "non-integer byte offset",
			facets:         []interface{}{facet(1.5, 5)},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "must be an integer",
		},
		{
			name: "string byte offset",
			facets: []interface{}{
				map[string]interface{}{
					"index": map[string]interface{}{
						"byteStart": "0",
						"byteEnd":   float64(5),
					},
					"features": []interface{}{
						map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
					},
				},
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "must be an integer",
		},

		// --- structural failures ----------------------------------------
		{
			name:           "facet entry is not an object",
			facets:         []interface{}{"not a facet"},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "must be an object",
		},
		{
			name: "missing index",
			facets: []interface{}{
				map[string]interface{}{
					"features": []interface{}{
						map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
					},
				},
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "missing required field 'index'",
		},
		{
			name: "missing byteEnd",
			facets: []interface{}{
				map[string]interface{}{
					"index": map[string]interface{}{"byteStart": float64(0)},
					"features": []interface{}{
						map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
					},
				},
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "byteEnd is required",
		},
		{
			name: "missing features",
			facets: []interface{}{
				map[string]interface{}{
					"index": map[string]interface{}{
						"byteStart": float64(0),
						"byteEnd":   float64(5),
					},
				},
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "missing required field 'features'",
		},
		{
			name: "feature missing $type",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{"did": "did:plc:abc"}),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "missing required '$type'",
		},
		{
			name: "second facet invalid reports its position",
			facets: []interface{}{
				facet(0, 5),
				facet(0, 99),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "facet 1:",
		},

		// --- known-feature attribute constraints -------------------------
		{
			name: "heading missing required level",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type": "social.coves.richtext.facet#heading",
				}),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "level is required",
		},
		{
			name: "heading level out of range",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type": "social.coves.richtext.facet#heading",
					"level": float64(7),
				}),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "level must be between 1 and 6",
		},
		{
			name: "heading level zero",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type": "social.coves.richtext.facet#heading",
					"level": float64(0),
				}),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "level must be between 1 and 6",
		},
		{
			name: "blockquote without level is valid (absent means 1)",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type": "social.coves.richtext.facet#blockquote",
				}),
			},
			contentByteLen: 10,
			wantError:      false,
		},
		{
			name: "blockquote level out of range",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type": "social.coves.richtext.facet#blockquote",
					"level": float64(9),
				}),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "level must be between 1 and 6",
		},
		{
			name: "codeBlock language too long",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type":    "social.coves.richtext.facet#codeBlock",
					"language": strings.Repeat("x", 41),
				}),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "language too long",
		},
		{
			name: "codeBlock language non-string",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type":    "social.coves.richtext.facet#codeBlock",
					"language": float64(42),
				}),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "language must be a string",
		},
		{
			name: "spoiler reason too long",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type":  "social.coves.richtext.facet#spoiler",
					"reason": strings.Repeat("x", 129),
				}),
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "reason too long",
		},
		{
			name: "unknown feature type with junk attributes still passes (open union)",
			facets: []interface{}{
				facet(0, 5, map[string]interface{}{
					"$type": "social.coves.richtext.facet#futureFeature",
					"level": float64(999),
				}),
			},
			contentByteLen: 10,
			wantError:      false,
		},
		{
			name: "over MaxFeaturesPerFacet rejected",
			facets: func() []interface{} {
				features := make([]interface{}, MaxFeaturesPerFacet+1)
				for i := range features {
					features[i] = map[string]interface{}{"$type": "social.coves.richtext.facet#bold"}
				}
				return []interface{}{facet(0, 5, features...)}
			}(),
			contentByteLen: 10,
			wantError:      true,
			errContains:    "too many features",
		},
		{
			name: "int64 byte offset beyond int32 range rejected",
			facets: []interface{}{
				map[string]interface{}{
					"index": map[string]interface{}{
						"byteStart": int64(0),
						"byteEnd":   int64(1) << 40,
					},
					"features": []interface{}{
						map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
					},
				},
			},
			contentByteLen: 10,
			wantError:      true,
			errContains:    "must be an integer",
		},

		// --- count cap ---------------------------------------------------
		{
			name: "over MaxFacets rejected",
			facets: func() []interface{} {
				fs := make([]interface{}, MaxFacets+1)
				for i := range fs {
					fs[i] = facet(0, 5)
				}
				return fs
			}(),
			contentByteLen: 10,
			wantError:      true,
			errContains:    "too many facets",
		},
		{
			name: "exactly MaxFacets accepted",
			facets: func() []interface{} {
				fs := make([]interface{}, MaxFacets)
				for i := range fs {
					fs[i] = facet(0, 5)
				}
				return fs
			}(),
			contentByteLen: 10,
			wantError:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateFacets(tt.facets, tt.contentByteLen)
			if tt.wantError {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error to contain %q, got: %v", tt.errContains, err)
				}
			} else if err != nil {
				t.Errorf("expected facets to validate, got: %v", err)
			}
		})
	}
}

// TestValidateFacets_EmptyFeatures covers the empty-features case directly since
// the facet() helper substitutes a default feature for empty variadics.
func TestValidateFacets_EmptyFeatures(t *testing.T) {
	facets := []interface{}{
		map[string]interface{}{
			"index": map[string]interface{}{
				"byteStart": float64(0),
				"byteEnd":   float64(5),
			},
			"features": []interface{}{},
		},
	}
	err := ValidateFacets(facets, 10)
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("expected 'must not be empty' error, got: %v", err)
	}
}

func TestSanitizeFacets(t *testing.T) {
	t.Run("nil in, nil out", func(t *testing.T) {
		kept, dropped := SanitizeFacets(nil, 10)
		if kept != nil || dropped != 0 {
			t.Errorf("expected (nil, 0), got (%v, %d)", kept, dropped)
		}
	})

	t.Run("valid facets survive untouched", func(t *testing.T) {
		facets := []interface{}{facet(0, 5), facet(5, 10)}
		kept, dropped := SanitizeFacets(facets, 10)
		if len(kept) != 2 || dropped != 0 {
			t.Errorf("expected 2 kept and 0 dropped, got %d kept and %d dropped", len(kept), dropped)
		}
	})

	t.Run("invalid facets dropped, valid neighbours kept", func(t *testing.T) {
		facets := []interface{}{
			facet(0, 5),   // valid
			facet(0, 99),  // out of range
			facet(7, 3),   // inverted
			facet(6, 10),  // valid
			"not a facet", // wrong type
		}
		kept, dropped := SanitizeFacets(facets, 10)
		if len(kept) != 2 || dropped != 3 {
			t.Fatalf("expected 2 kept and 3 dropped, got %d kept and %d dropped", len(kept), dropped)
		}
		// Order of survivors is preserved
		first := kept[0].(map[string]interface{})["index"].(map[string]interface{})["byteEnd"].(float64)
		second := kept[1].(map[string]interface{})["index"].(map[string]interface{})["byteEnd"].(float64)
		if first != 5 || second != 10 {
			t.Errorf("expected survivors (byteEnd 5, byteEnd 10) in order, got (%v, %v)", first, second)
		}
	})

	t.Run("no content drops every facet and returns nil", func(t *testing.T) {
		kept, dropped := SanitizeFacets([]interface{}{facet(0, 5)}, 0)
		if kept != nil || dropped != 1 {
			t.Errorf("expected (nil, 1), got (%v, %d)", kept, dropped)
		}
	})

	t.Run("truncates to MaxFacets", func(t *testing.T) {
		facets := make([]interface{}, MaxFacets+25)
		for i := range facets {
			facets[i] = facet(0, 5)
		}
		kept, dropped := SanitizeFacets(facets, 10)
		if len(kept) != MaxFacets || dropped != 25 {
			t.Errorf("expected %d kept and 25 dropped, got %d kept and %d dropped", MaxFacets, len(kept), dropped)
		}
	})
}

// TestSanitizeFacets_RoundTrip verifies sanitized facets built from a real JSON
// decode (the jetstream path) survive re-serialization unchanged.
func TestSanitizeFacets_RoundTrip(t *testing.T) {
	content := "Quoted line\nfmt.Println(\"hi\")"
	raw := `[
		{"index": {"byteStart": 0, "byteEnd": 11}, "features": [{"$type": "social.coves.richtext.facet#blockquote", "level": 1}]},
		{"index": {"byteStart": 12, "byteEnd": 29}, "features": [{"$type": "social.coves.richtext.facet#codeBlock", "language": "go"}]},
		{"index": {"byteStart": 500, "byteEnd": 600}, "features": [{"$type": "social.coves.richtext.facet#bold"}]}
	]`
	var facets []interface{}
	if err := json.Unmarshal([]byte(raw), &facets); err != nil {
		t.Fatalf("failed to decode fixture: %v", err)
	}

	kept, dropped := SanitizeFacets(facets, len(content))
	if len(kept) != 2 || dropped != 1 {
		t.Fatalf("expected 2 kept and 1 dropped, got %d kept and %d dropped", len(kept), dropped)
	}

	out, err := json.Marshal(kept)
	if err != nil {
		t.Fatalf("failed to re-serialize sanitized facets: %v", err)
	}
	for _, want := range []string{"#blockquote", "#codeBlock", `"language":"go"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("re-serialized facets missing %q: %s", want, out)
		}
	}
	if strings.Contains(string(out), "#bold") {
		t.Errorf("out-of-range facet survived sanitization: %s", out)
	}
}

// TestNormalizeLinkURIs covers the write-path repair of #link feature targets.
// facet#link.uri carries the same `format: uri` as the external embed fields, so
// a body link containing a raw accented character invalidates the record just as
// a bad embed URI does.
func TestNormalizeLinkURIs(t *testing.T) {
	linkFacet := func(uri string) map[string]interface{} {
		return map[string]interface{}{
			"index": map[string]interface{}{"byteStart": float64(0), "byteEnd": float64(4)},
			"features": []interface{}{
				map[string]interface{}{"$type": featureTypeLink, "uri": uri},
			},
		}
	}

	t.Run("accented link target is percent-encoded in place", func(t *testing.T) {
		facets := []interface{}{linkFacet("https://example.com/rudy_gobert_pokémon_lineup/")}
		if err := NormalizeLinkURIs(facets); err != nil {
			t.Fatalf("NormalizeLinkURIs() = %v, want nil", err)
		}
		features := facets[0].(map[string]interface{})["features"].([]interface{})
		got := features[0].(map[string]interface{})["uri"].(string)
		want := "https://example.com/rudy_gobert_pok%C3%A9mon_lineup/"
		if got != want {
			t.Errorf("link uri = %q, want %q", got, want)
		}
	})

	t.Run("conforming link target is left byte-identical", func(t *testing.T) {
		const uri = "https://example.com/pok%C3%A9mon"
		facets := []interface{}{linkFacet(uri)}
		if err := NormalizeLinkURIs(facets); err != nil {
			t.Fatalf("NormalizeLinkURIs() = %v, want nil", err)
		}
		features := facets[0].(map[string]interface{})["features"].([]interface{})
		if got := features[0].(map[string]interface{})["uri"].(string); got != uri {
			t.Errorf("link uri = %q, want it unchanged as %q", got, uri)
		}
	})

	t.Run("non-link features are untouched", func(t *testing.T) {
		facets := []interface{}{
			map[string]interface{}{
				"index": map[string]interface{}{"byteStart": float64(0), "byteEnd": float64(4)},
				"features": []interface{}{
					map[string]interface{}{"$type": featureTypeHeading, "level": float64(2)},
					map[string]interface{}{"$type": "social.coves.richtext.facet#bold"},
				},
			},
		}
		if err := NormalizeLinkURIs(facets); err != nil {
			t.Fatalf("NormalizeLinkURIs() = %v, want nil", err)
		}
		features := facets[0].(map[string]interface{})["features"].([]interface{})
		if _, present := features[0].(map[string]interface{})["uri"]; present {
			t.Error("normalization invented a uri on a heading feature")
		}
	})

	t.Run("every link across every facet is normalized", func(t *testing.T) {
		facets := []interface{}{
			linkFacet("https://example.com/café"),
			linkFacet("https://example.com/plain"),
			linkFacet("https://exämple.com/x"),
		}
		if err := NormalizeLinkURIs(facets); err != nil {
			t.Fatalf("NormalizeLinkURIs() = %v, want nil", err)
		}
		want := []string{
			"https://example.com/caf%C3%A9",
			"https://example.com/plain",
			"https://xn--exmple-cua.com/x",
		}
		for i, w := range want {
			features := facets[i].(map[string]interface{})["features"].([]interface{})
			if got := features[0].(map[string]interface{})["uri"].(string); got != w {
				t.Errorf("facet %d link uri = %q, want %q", i, got, w)
			}
		}
	})

	t.Run("unrecoverable link target is reported with its position", func(t *testing.T) {
		facets := []interface{}{
			linkFacet("https://example.com/ok"),
			linkFacet("not a url at all"),
		}
		err := NormalizeLinkURIs(facets)
		if err == nil {
			t.Fatal("NormalizeLinkURIs() = nil, want error for a scheme-less link target")
		}
		if !strings.Contains(err.Error(), "facet 1") {
			t.Errorf("error = %q, want it to identify facet 1", err)
		}
	})

	t.Run("link feature with no uri is rejected", func(t *testing.T) {
		facets := []interface{}{
			map[string]interface{}{
				"index":    map[string]interface{}{"byteStart": float64(0), "byteEnd": float64(4)},
				"features": []interface{}{map[string]interface{}{"$type": featureTypeLink}},
			},
		}
		if err := NormalizeLinkURIs(facets); err == nil {
			t.Fatal("NormalizeLinkURIs() = nil, want error for a link feature with no uri")
		}
	})

	t.Run("empty and structurally malformed input is a no-op", func(t *testing.T) {
		if err := NormalizeLinkURIs(nil); err != nil {
			t.Errorf("NormalizeLinkURIs(nil) = %v, want nil", err)
		}
		// ValidateFacets is what reports these; normalization must not panic.
		junk := []interface{}{"not an object", map[string]interface{}{"features": "not an array"}}
		if err := NormalizeLinkURIs(junk); err != nil {
			t.Errorf("NormalizeLinkURIs(junk) = %v, want nil", err)
		}
	})
}

// TestSanitizeFacetsKeepsUnencodedLinks pins the deliberate asymmetry: the
// firehose path must NOT drop a link facet whose uri is unencoded, or reindexing
// an already-federated post would silently strip its links.
func TestSanitizeFacetsKeepsUnencodedLinks(t *testing.T) {
	facets := []interface{}{
		map[string]interface{}{
			"index": map[string]interface{}{"byteStart": float64(0), "byteEnd": float64(4)},
			"features": []interface{}{
				map[string]interface{}{
					"$type": featureTypeLink,
					"uri":   "https://example.com/rudy_gobert_pokémon_lineup/",
				},
			},
		},
	}
	kept, dropped := SanitizeFacets(facets, 10)
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0: ingest must not strip links over an encoding nit", dropped)
	}
	if len(kept) != 1 {
		t.Fatalf("kept %d facets, want 1", len(kept))
	}
	features := kept[0].(map[string]interface{})["features"].([]interface{})
	if got := features[0].(map[string]interface{})["uri"].(string); got != "https://example.com/rudy_gobert_pokémon_lineup/" {
		t.Errorf("ingest rewrote a federated link uri to %q; it must pass through verbatim", got)
	}
}

// TestNormalizeLinkURIsRejectsForbiddenSchemes covers the stored-XSS vector.
// A #link feature is rendered as an href by every client, so a javascript: or
// data: target must be refused outright rather than percent-encoded into a
// schema-valid record and signed into the firehose.
func TestNormalizeLinkURIsRejectsForbiddenSchemes(t *testing.T) {
	linkFacet := func(uri string) map[string]interface{} {
		return map[string]interface{}{
			"index": map[string]interface{}{"byteStart": float64(0), "byteEnd": float64(4)},
			"features": []interface{}{
				map[string]interface{}{"$type": featureTypeLink, "uri": uri},
			},
		}
	}

	for _, uri := range []string{
		"javascript:alert(document.cookie)",
		"javascript:alert('é')",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"mailto:someone@example.com",
		// Allowlist, not blocklist: anything that is not http/https is refused.
		"ftp://example.com/file.zip",
		"blob:https://example.com/9d1b3b2a",
		"intent://scan/#Intent;scheme=zxing;end",
		"at://did:plc:abc/social.coves.community.post/xyz",
	} {
		t.Run(uri, func(t *testing.T) {
			facets := []interface{}{linkFacet(uri)}
			err := NormalizeLinkURIs(facets)
			if err == nil {
				t.Fatalf("NormalizeLinkURIs(%q) = nil, want the scheme refused", uri)
			}
			if !errors.Is(err, validation.ErrURISchemeNotAllowed) {
				t.Errorf("error = %v, want ErrURISchemeNotAllowed", err)
			}
			// The facet must be left alone rather than half-rewritten.
			features := facets[0].(map[string]interface{})["features"].([]interface{})
			if got := features[0].(map[string]interface{})["uri"].(string); got != uri {
				t.Errorf("uri was rewritten to %q despite the error", got)
			}
		})
	}
}

// TestNormalizeLinkURIsPreservesReservedEscapes guards the same regression as
// the validation package's test, one layer up: a link target naming one
// resource must never come back naming another.
func TestNormalizeLinkURIsPreservesReservedEscapes(t *testing.T) {
	facets := []interface{}{
		map[string]interface{}{
			"index": map[string]interface{}{"byteStart": float64(0), "byteEnd": float64(4)},
			"features": []interface{}{
				map[string]interface{}{
					"$type": featureTypeLink,
					"uri":   "https://web.archive.org/web/2020/https%3A%2F%2Fexample.com/café",
				},
			},
		},
	}
	if err := NormalizeLinkURIs(facets); err != nil {
		t.Fatalf("NormalizeLinkURIs() = %v, want nil", err)
	}
	features := facets[0].(map[string]interface{})["features"].([]interface{})
	got := features[0].(map[string]interface{})["uri"].(string)
	want := "https://web.archive.org/web/2020/https%3A%2F%2Fexample.com/caf%C3%A9"
	if got != want {
		t.Errorf("link uri\n got %q\nwant %q", got, want)
	}
}
