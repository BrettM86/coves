package users

import (
	"strings"

	"Coves/internal/atproto/embedblobs"
)

// Coves image caps for profile blobs imported from Bluesky, matching the
// social.coves.actor.profile lexicon's maxSize.
const (
	maxImportedAvatarBytes = 1_000_000
	maxImportedBannerBytes = 2_000_000
)

// importableImageMimeTypes are the image types a Coves profile accepts.
var importableImageMimeTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// ImportableBlueskyProfile holds the fields of an app.bsky.actor.profile record
// that may be copied onto a Coves profile. "" and nil mean absent.
type ImportableBlueskyProfile struct {
	DisplayName string
	Description string
	Avatar      map[string]interface{}
	Banner      map[string]interface{}
}

// ParseBlueskyProfileRecord extracts the importable fields from an
// app.bsky.actor.profile record value.
//
// SECURITY: the record comes from the user's PDS and is untrusted. Text fields
// must be strings and are truncated to the Coves caps; a value holding a NUL
// character, which Postgres text cannot store, or empty after trimming
// whitespace is absent. A blob counts only as a well-formed blob ref with a
// valid sha2-256 CID, of an accepted image type within the Coves size cap, and
// is copied with exactly the $type, ref, mimeType and size keys, its link in
// canonical form. A field that fails validation is absent; it never fails the
// parse or affects other fields.
func ParseBlueskyProfileRecord(record map[string]interface{}) ImportableBlueskyProfile {
	return ImportableBlueskyProfile{
		DisplayName: importableText(record["displayName"], maxDisplayNameGraphemes, maxDisplayNameBytes),
		Description: importableText(record["description"], maxDescriptionGraphemes, maxDescriptionBytes),
		Avatar:      importableBlob(record["avatar"], maxImportedAvatarBytes),
		Banner:      importableBlob(record["banner"], maxImportedBannerBytes),
	}
}

// importableText returns value truncated to the caps, or "" when value is not
// a string, contains a NUL character, or holds only whitespace within the caps.
func importableText(value interface{}, maxGraphemes, maxBytes int) string {
	text, ok := value.(string)
	if !ok || strings.ContainsRune(text, 0) {
		return ""
	}
	text = truncateGraphemes(text, maxGraphemes, maxBytes)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return text
}

// importableBlob returns a copy of a blob ref holding exactly $type, ref,
// mimeType and size, or nil when value is not an importable image blob.
func importableBlob(value interface{}, maxSize float64) map[string]interface{} {
	blob, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	rawLink, ok := extractProfileBlobCID(blob)
	if !ok {
		return nil
	}
	link, ok := embedblobs.CanonicalBlobCID(rawLink)
	if !ok {
		return nil
	}
	mimeType, ok := blob["mimeType"].(string)
	if !ok || !importableImageMimeTypes[mimeType] {
		return nil
	}
	size, ok := blob["size"].(float64)
	if !ok || size <= 0 || size > maxSize || size != float64(int64(size)) {
		return nil
	}
	return map[string]interface{}{
		"$type":    "blob",
		"ref":      map[string]interface{}{"$link": link},
		"mimeType": mimeType,
		"size":     size,
	}
}
