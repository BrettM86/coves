package posts

import (
	"strings"
	"testing"
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
