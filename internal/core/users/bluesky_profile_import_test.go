package users

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	importAvatarCID = "bafkreidhqb6afzkiias6nwij3ckl6cv2zdbyes7hhljww56uuusre4jaiq"
	importBannerCID = "bafkreigrgaoxwbprispsenknucpnisupzbbtyzb6vmhi7lqbvw73k6v2ki"

	// familyEmoji is one grapheme cluster of 25 bytes (four 4-byte emoji
	// joined by three 3-byte ZWJs).
	familyEmoji = "\U0001F468\u200D\U0001F469\u200D\U0001F467\u200D\U0001F466"
	// stackedA is one grapheme cluster of 21 bytes: "a" plus ten 2-byte
	// combining marks.
	stackedA = "à́̂̃̄̅̆̇̈̉"
)

// blobRecord builds a blob ref as it arrives in a JSON-decoded record value.
func blobRecord(link, mimeType string, size interface{}) map[string]interface{} {
	return map[string]interface{}{
		"$type":    "blob",
		"ref":      map[string]interface{}{"$link": link},
		"mimeType": mimeType,
		"size":     size,
	}
}

func TestParseBlueskyProfileRecord(t *testing.T) {
	validAvatar := func() map[string]interface{} {
		return blobRecord(importAvatarCID, "image/png", float64(12345))
	}

	tests := []struct {
		name   string
		record map[string]interface{}
		want   ImportableBlueskyProfile
	}{
		{
			name: "valid fields pass through and blob maps keep exactly the four ref keys",
			record: map[string]interface{}{
				"$type":       "app.bsky.actor.profile",
				"displayName": "Alice Example",
				"description": "Hello from Bluesky",
				"avatar": map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importAvatarCID},
					"mimeType": "image/jpeg",
					"size":     float64(12345),
					"extra":    "dropped",
				},
				"banner": blobRecord(importBannerCID, "image/png", float64(54321)),
			},
			want: ImportableBlueskyProfile{
				DisplayName: "Alice Example",
				Description: "Hello from Bluesky",
				Avatar: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importAvatarCID},
					"mimeType": "image/jpeg",
					"size":     float64(12345),
				},
				Banner: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importBannerCID},
					"mimeType": "image/png",
					"size":     float64(54321),
				},
			},
		},
		{
			name: "non-string displayName and description are absent; other fields unaffected",
			record: map[string]interface{}{
				"displayName": float64(42),
				"description": []interface{}{"not", "a", "string"},
				"avatar":      validAvatar(),
			},
			want: ImportableBlueskyProfile{
				Avatar: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importAvatarCID},
					"mimeType": "image/png",
					"size":     float64(12345),
				},
			},
		},
		{
			name: "whitespace-only displayName and description are absent",
			record: map[string]interface{}{
				"displayName": "   \t ",
				"description": "\n\n  \t",
				"banner":      blobRecord(importBannerCID, "image/webp", float64(100)),
			},
			want: ImportableBlueskyProfile{
				Banner: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importBannerCID},
					"mimeType": "image/webp",
					"size":     float64(100),
				},
			},
		},
		{
			name: "displayName over 64 graphemes under 640 bytes keeps the first 64 graphemes",
			// 70 ASCII graphemes, 70 bytes.
			record: map[string]interface{}{"displayName": strings.Repeat("abcdefghij", 7)},
			want: ImportableBlueskyProfile{
				DisplayName: "abcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcd",
			},
		},
		{
			name: "displayName within 64 graphemes over 640 bytes keeps whole clusters within 640 bytes",
			// 26 clusters, 650 bytes: 25 clusters are 625 bytes; the 26th would
			// end at 650, so the 640-byte cut falls inside it.
			record: map[string]interface{}{"displayName": strings.Repeat(familyEmoji, 26)},
			want:   ImportableBlueskyProfile{DisplayName: strings.Repeat(familyEmoji, 25)},
		},
		{
			name: "description over 256 graphemes under 2560 bytes keeps the first 256 graphemes",
			// 300 ASCII graphemes, 300 bytes.
			record: map[string]interface{}{"description": strings.Repeat("abcdefghij", 30)},
			want: ImportableBlueskyProfile{
				Description: strings.Repeat("abcdefghij", 25) + "abcdef",
			},
		},
		{
			name: "description within 256 graphemes over 2560 bytes keeps whole clusters within 2560 bytes",
			// 130 clusters, 2730 bytes: 121 clusters are 2541 bytes; the 122nd
			// would end at 2562, so the 2560-byte cut falls inside it.
			record: map[string]interface{}{"description": strings.Repeat(stackedA, 130)},
			want:   ImportableBlueskyProfile{Description: strings.Repeat(stackedA, 121)},
		},
		{
			name: "blob whose $type is not blob is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar": map[string]interface{}{
					"$type":    "image",
					"ref":      map[string]interface{}{"$link": importAvatarCID},
					"mimeType": "image/png",
					"size":     float64(10),
				},
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "blob missing ref is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar": map[string]interface{}{
					"$type":    "blob",
					"mimeType": "image/png",
					"size":     float64(10),
				},
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "blob missing ref.$link is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar": map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"cid": importAvatarCID},
					"mimeType": "image/png",
					"size":     float64(10),
				},
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "blob $link of 257 characters is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar":      blobRecord(strings.Repeat("b", 257), "image/png", float64(10)),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "blob $link of plausible characters that is not a CID is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar":      blobRecord(strings.Repeat("b", 256), "image/png", float64(10)),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "blob $link in another encoding is stored in canonical form",
			record: map[string]interface{}{
				"avatar": blobRecord(strings.ToUpper(importAvatarCID), "image/png", float64(10)),
			},
			want: ImportableBlueskyProfile{
				Avatar: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importAvatarCID},
					"mimeType": "image/png",
					"size":     float64(10),
				},
			},
		},
		{
			name: "gif avatar is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar":      blobRecord(importAvatarCID, "image/gif", float64(10)),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "webp avatar is accepted",
			record: map[string]interface{}{
				"avatar": blobRecord(importAvatarCID, "image/webp", float64(10)),
			},
			want: ImportableBlueskyProfile{
				Avatar: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importAvatarCID},
					"mimeType": "image/webp",
					"size":     float64(10),
				},
			},
		},
		{
			name: "avatar of 1_000_001 bytes is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar":      blobRecord(importAvatarCID, "image/png", float64(1_000_001)),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "avatar of 1_000_000 bytes is accepted",
			record: map[string]interface{}{
				"avatar": blobRecord(importAvatarCID, "image/png", float64(1_000_000)),
			},
			want: ImportableBlueskyProfile{
				Avatar: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importAvatarCID},
					"mimeType": "image/png",
					"size":     float64(1_000_000),
				},
			},
		},
		{
			name: "banner of 2_000_001 bytes is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"banner":      blobRecord(importBannerCID, "image/jpeg", float64(2_000_001)),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "banner of 2_000_000 bytes is accepted",
			record: map[string]interface{}{
				"banner": blobRecord(importBannerCID, "image/jpeg", float64(2_000_000)),
			},
			want: ImportableBlueskyProfile{
				Banner: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importBannerCID},
					"mimeType": "image/jpeg",
					"size":     float64(2_000_000),
				},
			},
		},
		{
			name: "non-numeric blob size is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar":      blobRecord(importAvatarCID, "image/png", "12345"),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "blob size of zero is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar":      blobRecord(importAvatarCID, "image/png", float64(0)),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "negative blob size is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar":      blobRecord(importAvatarCID, "image/png", float64(-1)),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "fractional blob size is absent",
			record: map[string]interface{}{
				"displayName": "Alice",
				"avatar":      blobRecord(importAvatarCID, "image/png", 1.5),
			},
			want: ImportableBlueskyProfile{DisplayName: "Alice"},
		},
		{
			name: "text containing a NUL character is absent",
			record: map[string]interface{}{
				"displayName": "Ali\x00ce",
				"description": "A bio\x00",
				"avatar":      validAvatar(),
			},
			want: ImportableBlueskyProfile{
				Avatar: map[string]interface{}{
					"$type":    "blob",
					"ref":      map[string]interface{}{"$link": importAvatarCID},
					"mimeType": "image/png",
					"size":     float64(12345),
				},
			},
		},
		{
			name: "a NUL character past the truncation point still makes the text absent",
			record: map[string]interface{}{
				"displayName": strings.Repeat("a", 70) + "\x00",
			},
			want: ImportableBlueskyProfile{},
		},
		{
			name: "text that is blank within the cap is absent, whatever follows the cut",
			record: map[string]interface{}{
				"displayName": strings.Repeat(" ", 64) + "Alice",
			},
			want: ImportableBlueskyProfile{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseBlueskyProfileRecord(tt.record))
		})
	}
}
