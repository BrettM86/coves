package posts

import (
	"errors"
	"strings"
	"testing"

	"Coves/internal/validation"
)

func TestValidateEmbed(t *testing.T) {
	// Reusable valid blob (the JSON shape a client sends after uploading).
	blob := func() map[string]interface{} {
		return map[string]interface{}{
			"$type":    "blob",
			"ref":      map[string]interface{}{"$link": "bafyblob"},
			"mimeType": "image/png",
			"size":     float64(1234),
		}
	}

	tests := []struct {
		name      string
		embed     map[string]interface{}
		wantError bool
		// errContains, when set, asserts the validation message mentions it so
		// the client gets an actionable reason.
		errContains string
	}{
		// --- optional / happy paths -------------------------------------
		{
			name:      "nil embed is valid (embed is optional)",
			embed:     nil,
			wantError: false,
		},
		{
			name: "valid external embed",
			embed: map[string]interface{}{
				"$type":    embedTypeExternal,
				"external": map[string]interface{}{"uri": "https://example.com/article"},
			},
			wantError: false,
		},
		{
			name: "valid external with junk thumb still passes (thumb is validated later in the service, not here)",
			embed: map[string]interface{}{
				"$type": embedTypeExternal,
				"external": map[string]interface{}{
					"uri":   "https://example.com/article",
					"thumb": "https://example.com/not-a-blob.jpg",
				},
			},
			wantError: false,
		},
		{
			name: "valid images embed",
			embed: map[string]interface{}{
				"$type":  embedTypeImages,
				"images": []interface{}{map[string]interface{}{"image": blob(), "alt": "a cat"}},
			},
			wantError: false,
		},
		{
			name: "valid video embed",
			embed: map[string]interface{}{
				"$type": embedTypeVideo,
				"video": blob(),
			},
			wantError: false,
		},
		{
			name: "valid post embed (matches Bluesky-conversion output shape)",
			embed: map[string]interface{}{
				"$type": embedTypePost,
				"post": map[string]interface{}{
					"uri": "at://did:plc:abc/app.bsky.feed.post/xyz",
					"cid": "bafycid",
				},
			},
			wantError: false,
		},

		// --- the actual reported frontend bug ---------------------------
		{
			name:        "bare {uri} with no $type is rejected (the link-post bug)",
			embed:       map[string]interface{}{"uri": "https://example.com"},
			wantError:   true,
			errContains: "$type",
		},

		// --- $type discriminator problems -------------------------------
		{
			name:        "non-string $type is rejected",
			embed:       map[string]interface{}{"$type": 42},
			wantError:   true,
			errContains: "must be a string",
		},
		{
			name: "unknown $type is rejected",
			embed: map[string]interface{}{
				"$type":    "social.coves.embed.externl", // typo
				"external": map[string]interface{}{"uri": "https://example.com"},
			},
			wantError:   true,
			errContains: "unknown embed",
		},
		{
			name: "#view variant is rejected on input (output-only type)",
			embed: map[string]interface{}{
				"$type":    "social.coves.embed.external#view",
				"external": map[string]interface{}{"uri": "https://example.com"},
			},
			wantError:   true,
			errContains: "unknown embed",
		},

		// --- external failure modes -------------------------------------
		{
			name:        "external without 'external' wrapper is rejected",
			embed:       map[string]interface{}{"$type": embedTypeExternal, "uri": "https://example.com"},
			wantError:   true,
			errContains: "'external' object",
		},
		{
			name: "external with empty uri is rejected",
			embed: map[string]interface{}{
				"$type":    embedTypeExternal,
				"external": map[string]interface{}{"uri": ""},
			},
			wantError:   true,
			errContains: "external.uri",
		},
		{
			name: "external with non-string uri is rejected",
			embed: map[string]interface{}{
				"$type":    embedTypeExternal,
				"external": map[string]interface{}{"uri": 12345},
			},
			wantError:   true,
			errContains: "external.uri",
		},

		// --- images failure modes ---------------------------------------
		{
			name:        "images with empty array is rejected",
			embed:       map[string]interface{}{"$type": embedTypeImages, "images": []interface{}{}},
			wantError:   true,
			errContains: "non-empty 'images'",
		},
		{
			name:        "images with non-array is rejected",
			embed:       map[string]interface{}{"$type": embedTypeImages, "images": "not-an-array"},
			wantError:   true,
			errContains: "non-empty 'images'",
		},
		{
			name: "images item missing 'image' blob is rejected",
			embed: map[string]interface{}{
				"$type":  embedTypeImages,
				"images": []interface{}{map[string]interface{}{"alt": "no blob here"}},
			},
			wantError:   true,
			errContains: "requires an 'image' blob",
		},
		{
			name: "images item that is a bare string is rejected",
			embed: map[string]interface{}{
				"$type":  embedTypeImages,
				"images": []interface{}{"https://example.com/cat.png"},
			},
			wantError:   true,
			errContains: "must be an object",
		},
		{
			name: "images item with a bare-string image blob is rejected",
			embed: map[string]interface{}{
				"$type":  embedTypeImages,
				"images": []interface{}{map[string]interface{}{"image": "https://example.com/cat.png"}},
			},
			wantError:   true,
			errContains: "requires an 'image' blob object",
		},
		{
			name: "images with valid first item but malformed second is rejected (exercises loop past index 0)",
			embed: map[string]interface{}{
				"$type": embedTypeImages,
				"images": []interface{}{
					map[string]interface{}{"image": blob()},
					map[string]interface{}{"alt": "no blob here"},
				},
			},
			wantError:   true,
			errContains: "images[1]",
		},

		// --- video failure modes ----------------------------------------
		{
			name:        "video without 'video' blob is rejected",
			embed:       map[string]interface{}{"$type": embedTypeVideo},
			wantError:   true,
			errContains: "'video' blob",
		},
		{
			name:        "video as a bare URL string is rejected",
			embed:       map[string]interface{}{"$type": embedTypeVideo, "video": "https://example.com/v.mp4"},
			wantError:   true,
			errContains: "'video' blob",
		},

		// --- post failure modes -----------------------------------------
		{
			name:        "post without 'post' strongRef is rejected",
			embed:       map[string]interface{}{"$type": embedTypePost},
			wantError:   true,
			errContains: "'post' strong reference",
		},
		{
			name: "post missing uri is rejected",
			embed: map[string]interface{}{
				"$type": embedTypePost,
				"post":  map[string]interface{}{"cid": "bafycid"},
			},
			wantError:   true,
			errContains: "post.uri",
		},
		{
			name: "post missing cid is rejected",
			embed: map[string]interface{}{
				"$type": embedTypePost,
				"post":  map[string]interface{}{"uri": "at://did:plc:abc/app.bsky.feed.post/xyz"},
			},
			wantError:   true,
			errContains: "post.cid",
		},
		{
			name: "post with non-string uri is rejected",
			embed: map[string]interface{}{
				"$type": embedTypePost,
				"post":  map[string]interface{}{"uri": 123, "cid": "bafycid"},
			},
			wantError:   true,
			errContains: "post.uri",
		},
		{
			name: "post with empty uri is rejected",
			embed: map[string]interface{}{
				"$type": embedTypePost,
				"post":  map[string]interface{}{"uri": "", "cid": "bafycid"},
			},
			wantError:   true,
			errContains: "post.uri",
		},
		{
			name: "post with non-string cid is rejected",
			embed: map[string]interface{}{
				"$type": embedTypePost,
				"post":  map[string]interface{}{"uri": "at://did:plc:abc/app.bsky.feed.post/xyz", "cid": 99},
			},
			wantError:   true,
			errContains: "post.cid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEmbed(tt.embed)

			if tt.wantError {
				if err == nil {
					t.Fatalf("validateEmbed() = nil, want error")
				}
				if !IsValidationError(err) {
					t.Errorf("validateEmbed() error = %T, want *ValidationError (maps to 400 InvalidRequest)", err)
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("validateEmbed() error = %q, want it to contain %q", err.Error(), tt.errContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("validateEmbed() = %v, want nil", err)
			}
		})
	}
}

// TestNormalizeEmbedURIs covers the write-path repair of `format: uri` fields on
// external embeds. The RSS bridge shipped raw non-percent-encoded URLs into
// sources[].uri for months, producing records that fail schema validation for
// any third-party tool that resolves our lexicons; normalization here makes a
// conforming record independent of what the client sent.
func TestNormalizeEmbedURIs(t *testing.T) {
	external := func(fields map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"$type": embedTypeExternal, "external": fields}
	}

	t.Run("accented source uri is percent-encoded in place", func(t *testing.T) {
		embed := external(map[string]interface{}{
			"uri": "https://kagi.com/news/daily",
			"sources": []interface{}{
				map[string]interface{}{
					"uri":   "https://example.com/rudy_gobert_pokémon_lineup/",
					"title": "Rudy Gobert",
				},
			},
		})

		if err := normalizeEmbedURIs(embed); err != nil {
			t.Fatalf("normalizeEmbedURIs() = %v, want nil", err)
		}

		sources := embed["external"].(map[string]interface{})["sources"].([]interface{})
		got := sources[0].(map[string]interface{})["uri"].(string)
		want := "https://example.com/rudy_gobert_pok%C3%A9mon_lineup/"
		if got != want {
			t.Errorf("sources[0].uri = %q, want %q", got, want)
		}
	})

	t.Run("primary uri is normalized", func(t *testing.T) {
		embed := external(map[string]interface{}{"uri": "https://example.com/pokémon"})
		if err := normalizeEmbedURIs(embed); err != nil {
			t.Fatalf("normalizeEmbedURIs() = %v, want nil", err)
		}
		got := embed["external"].(map[string]interface{})["uri"].(string)
		if want := "https://example.com/pok%C3%A9mon"; got != want {
			t.Errorf("external.uri = %q, want %q", got, want)
		}
	})

	t.Run("every source is normalized, not just the first", func(t *testing.T) {
		embed := external(map[string]interface{}{
			"uri": "https://kagi.com/news/daily",
			"sources": []interface{}{
				map[string]interface{}{"uri": "https://example.com/ok"},
				map[string]interface{}{"uri": "https://example.com/café"},
				map[string]interface{}{"uri": "https://example.com/naïve"},
			},
		})
		if err := normalizeEmbedURIs(embed); err != nil {
			t.Fatalf("normalizeEmbedURIs() = %v, want nil", err)
		}
		sources := embed["external"].(map[string]interface{})["sources"].([]interface{})
		want := []string{
			"https://example.com/ok",
			"https://example.com/caf%C3%A9",
			"https://example.com/na%C3%AFve",
		}
		for i, w := range want {
			if got := sources[i].(map[string]interface{})["uri"].(string); got != w {
				t.Errorf("sources[%d].uri = %q, want %q", i, got, w)
			}
		}
	})

	t.Run("conforming uris are left byte-identical", func(t *testing.T) {
		const primary = "https://example.com/already%C3%A9"
		const source = "https://example.com/plain"
		embed := external(map[string]interface{}{
			"uri":     primary,
			"sources": []interface{}{map[string]interface{}{"uri": source}},
		})
		if err := normalizeEmbedURIs(embed); err != nil {
			t.Fatalf("normalizeEmbedURIs() = %v, want nil", err)
		}
		ext := embed["external"].(map[string]interface{})
		if got := ext["uri"].(string); got != primary {
			t.Errorf("external.uri = %q, want it unchanged as %q", got, primary)
		}
		got := ext["sources"].([]interface{})[0].(map[string]interface{})["uri"].(string)
		if got != source {
			t.Errorf("sources[0].uri = %q, want it unchanged as %q", got, source)
		}
	})

	t.Run("unrecoverable uri is rejected as a validation error", func(t *testing.T) {
		embed := external(map[string]interface{}{"uri": "not a url at all"})
		err := normalizeEmbedURIs(embed)
		if err == nil {
			t.Fatal("normalizeEmbedURIs() = nil, want error for a scheme-less uri")
		}
		if !IsValidationError(err) {
			t.Errorf("error = %T, want *ValidationError (maps to 400 InvalidRequest)", err)
		}
		if !strings.Contains(err.Error(), "embed.external.uri") {
			t.Errorf("error = %q, want it to name the offending field", err)
		}
	})

	t.Run("source missing its required uri is rejected with its index", func(t *testing.T) {
		embed := external(map[string]interface{}{
			"uri": "https://kagi.com/news/daily",
			"sources": []interface{}{
				map[string]interface{}{"uri": "https://example.com/ok"},
				map[string]interface{}{"title": "no uri here"},
			},
		})
		err := normalizeEmbedURIs(embed)
		if err == nil {
			t.Fatal("normalizeEmbedURIs() = nil, want error for a source with no uri")
		}
		if !IsValidationError(err) {
			t.Errorf("error = %T, want *ValidationError", err)
		}
		if !strings.Contains(err.Error(), "sources[1]") {
			t.Errorf("error = %q, want it to point at sources[1]", err)
		}
	})

	t.Run("non-external embeds and nil are no-ops", func(t *testing.T) {
		if err := normalizeEmbedURIs(nil); err != nil {
			t.Errorf("normalizeEmbedURIs(nil) = %v, want nil", err)
		}
		video := map[string]interface{}{
			"$type": embedTypeVideo,
			"video": map[string]interface{}{"$type": "blob"},
		}
		if err := normalizeEmbedURIs(video); err != nil {
			t.Errorf("normalizeEmbedURIs(video) = %v, want nil", err)
		}
	})
}

// TestNormalizeEmbedURIsBoundsSources covers the guards that keep a
// schema-invalid record from being signed. validateEmbed does not inspect
// sources at all, so these checks are the only thing standing between a
// malformed or oversized array and the PDS.
func TestNormalizeEmbedURIsBoundsSources(t *testing.T) {
	external := func(fields map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"$type": embedTypeExternal, "external": fields}
	}
	source := func(uri string) interface{} {
		return map[string]interface{}{"uri": uri}
	}

	t.Run("sources beyond the lexicon maximum are rejected", func(t *testing.T) {
		entries := make([]interface{}, maxEmbedSources+1)
		for i := range entries {
			entries[i] = source("https://example.com/ok")
		}
		embed := external(map[string]interface{}{
			"uri":     "https://kagi.com/news/daily",
			"sources": entries,
		})
		err := normalizeEmbedURIs(embed)
		if err == nil {
			t.Fatalf("normalizeEmbedURIs() = nil, want error for %d sources", len(entries))
		}
		if !IsValidationError(err) {
			t.Errorf("error = %T, want *ValidationError", err)
		}
		if !strings.Contains(err.Error(), "too many sources") {
			t.Errorf("error = %q, want it to explain the limit", err)
		}
	})

	t.Run("exactly the maximum is allowed", func(t *testing.T) {
		entries := make([]interface{}, maxEmbedSources)
		for i := range entries {
			entries[i] = source("https://example.com/café")
		}
		embed := external(map[string]interface{}{
			"uri":     "https://kagi.com/news/daily",
			"sources": entries,
		})
		if err := normalizeEmbedURIs(embed); err != nil {
			t.Fatalf("normalizeEmbedURIs() = %v, want nil at the boundary", err)
		}
	})

	t.Run("a non-array sources value is rejected, not silently skipped", func(t *testing.T) {
		for name, bad := range map[string]interface{}{
			"object": map[string]interface{}{"uri": "https://example.com/café"},
			"string": "https://example.com/café",
			"number": float64(3),
		} {
			embed := external(map[string]interface{}{
				"uri":     "https://kagi.com/news/daily",
				"sources": bad,
			})
			err := normalizeEmbedURIs(embed)
			if err == nil {
				t.Errorf("sources as %s: got nil, want error (would sign an unnormalized record)", name)
				continue
			}
			if !IsValidationError(err) {
				t.Errorf("sources as %s: error = %T, want *ValidationError", name, err)
			}
		}
	})

	t.Run("an absent sources key remains valid", func(t *testing.T) {
		embed := external(map[string]interface{}{"uri": "https://example.com/café"})
		if err := normalizeEmbedURIs(embed); err != nil {
			t.Errorf("normalizeEmbedURIs() = %v, want nil", err)
		}
	})
}

// TestNormalizeEmbedURIsPreservesErrorChain pins that a typed cause survives the
// hop through ValidationError. Without Unwrap the sentinels in
// internal/validation are unreachable from any caller and exist only for tests.
func TestNormalizeEmbedURIsPreservesErrorChain(t *testing.T) {
	tests := []struct {
		name  string
		uri   string
		want  error
		field string
	}{
		{"no scheme", "example.com/path", validation.ErrURINoScheme, "embed.external.uri"},
		{"forbidden scheme", "javascript:alert(1)", validation.ErrURISchemeNotAllowed, "embed.external.uri"},
		{"non-web scheme (allowlist)", "ftp://example.com/file.zip", validation.ErrURISchemeNotAllowed, "embed.external.uri"},
		{"blob scheme (allowlist)", "blob:https://example.com/abc", validation.ErrURISchemeNotAllowed, "embed.external.uri"},
		{"bad scheme", "s3://bucket/key", validation.ErrURIBadScheme, "embed.external.uri"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			embed := map[string]interface{}{
				"$type":    embedTypeExternal,
				"external": map[string]interface{}{"uri": tt.uri},
			}
			err := normalizeEmbedURIs(embed)
			if err == nil {
				t.Fatalf("normalizeEmbedURIs(%q) = nil, want %v", tt.uri, tt.want)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("errors.Is(err, %v) = false; chain severed (err = %v)", tt.want, err)
			}
			if !IsValidationError(err) {
				t.Errorf("error = %T, want it to still be a *ValidationError for the 400 mapping", err)
			}
		})
	}
}

// TestNormalizeEmbedURIsRejectsForbiddenSchemeInSources ensures the scheme guard
// applies to aggregated sources too, not just the primary link. ftp: is included
// because the old blocklist accepted it: it proves sources see the allowlist,
// not merely the executable-scheme refusal.
func TestNormalizeEmbedURIsRejectsForbiddenSchemeInSources(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "ftp://example.com/file.zip"} {
		t.Run(bad, func(t *testing.T) {
			embed := map[string]interface{}{
				"$type": embedTypeExternal,
				"external": map[string]interface{}{
					"uri": "https://kagi.com/news/daily",
					"sources": []interface{}{
						map[string]interface{}{"uri": "https://example.com/ok"},
						map[string]interface{}{"uri": bad},
					},
				},
			}
			err := normalizeEmbedURIs(embed)
			if err == nil {
				t.Fatal("normalizeEmbedURIs() = nil, want the forbidden scheme rejected")
			}
			if !errors.Is(err, validation.ErrURISchemeNotAllowed) {
				t.Errorf("error = %v, want ErrURISchemeNotAllowed", err)
			}
			if !strings.Contains(err.Error(), "sources[1]") {
				t.Errorf("error = %q, want it to point at sources[1]", err)
			}
		})
	}
}
